package pot

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"time"

	hclog "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/drivers/shared/eventer"
	"github.com/hashicorp/nomad/helper/pluginutils/hclutils"
	"github.com/hashicorp/nomad/plugins/base"
	"github.com/hashicorp/nomad/plugins/drivers"
	"github.com/hashicorp/nomad/plugins/shared/hclspec"
	pstructs "github.com/hashicorp/nomad/plugins/shared/structs"
)

const (
	// pluginName is the name of the plugin
	pluginName = "pot"

	// fingerprintPeriod is the interval at which the driver will send fingerprint responses
	fingerprintPeriod = 30 * time.Second

	// taskHandleVersion is the version of task handle which this driver sets
	// and understands how to decode driver state
	taskHandleVersion = 1

	// potBIN is the singularity binary path.
	potBIN = "/usr/local/bin/pot"
)

var (
	// pluginInfo is the response returned for the PluginInfo RPC
	pluginInfo = &base.PluginInfoResponse{
		Type:              base.PluginTypeDriver,
		PluginApiVersions: []string{"0.1.0"},
		PluginVersion:     "0.0.1",
		Name:              pluginName,
	}

	// configSpec is the hcl specification returned by the ConfigSchema RPC
	configSpec = hclspec.NewObject(map[string]*hclspec.Spec{
		"enabled": hclspec.NewDefault(
			hclspec.NewAttr("enabled", "bool", false),
			hclspec.NewLiteral("true"),
		),
	})

	// taskConfigSpec is the hcl specification for the driver config section of
	// a taskConfig within a job. It is returned in the TaskConfigSchema RPC
	taskConfigSpec = hclspec.NewObject(map[string]*hclspec.Spec{
		"image":           hclspec.NewAttr("image", "string", true),
		"pot":             hclspec.NewAttr("pot", "string", true),
		"tag":             hclspec.NewAttr("tag", "string", true),
		"command":         hclspec.NewAttr("command", "string", false),
		"args":            hclspec.NewAttr("args", "list(string)", false),
		"port_map":        hclspec.NewAttr("port_map", "list(map(string))", false),
		"network_mode":    hclspec.NewAttr("network_mode", "string", false),
		"ip":              hclspec.NewAttr("ip", "string", false),
		"mount":           hclspec.NewAttr("mount", "list(string)", false),
		"copy":            hclspec.NewAttr("copy", "list(string)", false),
		"mount_read_only": hclspec.NewAttr("mount_read_only", "list(string)", false),
		"extra_hosts":     hclspec.NewAttr("extra_hosts", "list(string)", false),
	})

	// capabilities is returned by the Capabilities RPC and indicates what
	// optional features this driver supports
	capabilities = &drivers.Capabilities{
		SendSignals: true,
		Exec:        true,
	}
)

// Driver is a driver for running Pot containers
// https://github.com/pizzamig/pot
type Driver struct {
	// eventer is used to handle multiplexing of TaskEvents calls such that an
	// event can be broadcast to all callers
	eventer *eventer.Eventer

	// config is the driver configuration set by the SetConfig RPC
	config *Config

	// nomadConfig is the client config from nomad
	nomadConfig *base.ClientDriverConfig

	// tasks is the in memory datastore mapping taskIDs to rawExecDriverHandles
	tasks *taskStore

	// ctx is the context for the driver. It is passed to other subsystems to
	// coordinate shutdown
	ctx context.Context

	// signalShutdown is called when the driver is shutting down and cancels the
	// ctx passed to any subsystems
	signalShutdown context.CancelFunc

	// logger will log to the Nomad agent
	logger hclog.Logger
}

// Config is the driver configuration set by the SetConfig RPC call
type Config struct {
	// Enabled is set to true to enable the Pot driver
	Enabled bool `codec:"enabled"`
}

// TaskConfig is the driver configuration of a task within a job
type TaskConfig struct {
	Image string `codec:"image"`
	Pot   string `codec:"pot"`
	Tag   string `codec:"tag"`
	Alloc string `codec:"alloc"`

	// Command can be run or exec , shell is not supported via plugin
	Command string   `codec:"command"`
	Args    []string `codec:"args"`

	//Port    []string          `codec:"port"`
	PortMap hclutils.MapStrStr `codec:"port_map"`
	Name    string             `codec:"name"`

	//Network Mode
	NetworkMode string `codec:"network_mode"`
	IP          string `codec:"ip"`

	// Enable debug-verbose global options
	Debug   bool `codec:"debug"`
	Verbose bool `codec:"verbose"`

	Mount         []string `codec:"mount"`           // Host-Volumes to mount in, syntax: /path/to/host/directory:/destination/path/in/container
	MountReadOnly []string `codec:"mount_read_only"` // Host-Volumes to mount in, syntax: /path/to/host/directory:/destination/path/in/container
	Copy          []string `codec:"copy"`            // Files in host to copy in, syntax: /path/to/host/file.ext:/destination/path/in/container/file.ext
	ExtraHosts    []string `codec:"extra_hosts"`     // ExtraHosts a list of hosts, given as host:IP, to be added to /etc/hosts
}

// TaskState is the state which is encoded in the handle returned in
// StartTask. This information is needed to rebuild the task state and handler
// during recovery.
type TaskState struct {
	TaskConfig    *drivers.TaskConfig
	ContainerName string
	StartedAt     time.Time
	PID           int
}

// NewPotDriver returns a new DriverPlugin implementation
func NewPotDriver(logger hclog.Logger) drivers.DriverPlugin {
	ctx, cancel := context.WithCancel(context.Background())
	logger = logger.Named(pluginName)

	return &Driver{
		eventer:        eventer.NewEventer(ctx, logger),
		config:         &Config{},
		tasks:          newTaskStore(),
		ctx:            ctx,
		signalShutdown: cancel,
		logger:         logger,
	}
}

// PluginInfo return a base.PluginInfoResponse struct
func (d *Driver) PluginInfo() (*base.PluginInfoResponse, error) {
	return pluginInfo, nil
}

// ConfigSchema return a hclspec.Spec struct
func (d *Driver) ConfigSchema() (*hclspec.Spec, error) {
	return configSpec, nil
}

// SetConfig set the nomad agent config based on base.Config
func (d *Driver) SetConfig(cfg *base.Config) error {
	var config Config
	if len(cfg.PluginConfig) != 0 {
		if err := base.MsgPackDecode(cfg.PluginConfig, &config); err != nil {
			return err
		}
	}

	d.config = &config
	if cfg.AgentConfig != nil {
		d.nomadConfig = cfg.AgentConfig.Driver
	}

	return nil
}

// Shutdown the plugin
func (d *Driver) Shutdown(ctx context.Context) error {
	d.signalShutdown()
	return nil
}

// TaskConfigSchema returns a hclspec.Spec struct
func (d *Driver) TaskConfigSchema() (*hclspec.Spec, error) {
	return taskConfigSpec, nil
}

// Capabilities a drivers.Capabilities struct
func (d *Driver) Capabilities() (*drivers.Capabilities, error) {
	return capabilities, nil
}

// Fingerprint return the plugin fingerprint
func (d *Driver) Fingerprint(ctx context.Context) (<-chan *drivers.Fingerprint, error) {
	ch := make(chan *drivers.Fingerprint)
	go d.handleFingerprint(ctx, ch)
	return ch, nil
}

func (d *Driver) handleFingerprint(ctx context.Context, ch chan<- *drivers.Fingerprint) {
	defer close(ch)
	ticker := time.NewTimer(0)
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			ticker.Reset(fingerprintPeriod)
			ch <- d.buildFingerprint()
		}
	}
}

func (d *Driver) buildFingerprint() *drivers.Fingerprint {
	var health drivers.HealthState
	var desc string
	attrs := map[string]*pstructs.Attribute{}

	potVersion := "pot0.1.0"

	if d.config.Enabled && potVersion != "" {
		health = drivers.HealthStateHealthy
		desc = "healthy"
		attrs["driver.pot"] = pstructs.NewBoolAttribute(true)
		attrs["driver.pot.version"] = pstructs.NewStringAttribute(potVersion)
	} else {
		health = drivers.HealthStateUndetected
		desc = "disabled"
	}

	return &drivers.Fingerprint{
		Attributes:        attrs,
		Health:            health,
		HealthDescription: desc,
	}
}

// RecoverTask try to recover a failed task, if not return error
func (d *Driver) RecoverTask(handle *drivers.TaskHandle) error {
	d.logger.Trace("###########################################################################################################################################")
	d.logger.Trace("########################################################RECOVER-TASK#######################################################################")
	d.logger.Trace("###########################################################################################################################################")
	d.logger.Trace("RECOVER TASK", "ID", handle.Config.ID)
	if handle == nil {
		return fmt.Errorf("error: handle cannot be nil")
	}

	if taskhandle, ok := d.tasks.Get(handle.Config.ID); ok {
		d.logger.Debug("Getting task failed", "tasks ", taskhandle)
		return nil
	}

	var taskState TaskState
	if err := handle.GetDriverState(&taskState); err != nil {
		return fmt.Errorf("failed to decode task state from handle: %v", err)
	}

	if err := handle.SetDriverState(&taskState); err != nil {
		d.logger.Trace("failed to recover task, error setting driver state", "error", err)
	}

	d.logger.Trace("RECOVER TASK", "taskState", taskState)

	var driverConfig TaskConfig
	d.logger.Trace("TASKCONFIG RECOVER", "TASKCONFIG RECOVER", taskState.TaskConfig)
	if err := taskState.TaskConfig.DecodeDriverConfig(&driverConfig); err != nil {
		d.logger.Trace("failed to recover driverConfig, error setting driver state", "error", err)
	}

	se, err := prepareContainer(handle.Config, driverConfig)
	if err != nil {
		return err
	}
	se.logger = d.logger

	alive := se.checkContainerAlive(handle.Config)
	if alive == 0 {
		d.tasks.Delete(handle.Config.ID)
		return fmt.Errorf("unable to recover a container that is not running")
	} else {
		se.containerPid = alive
		completeName := handle.Config.JobName + handle.Config.Name + "_" + handle.Config.AllocID
		Sout, err := se.Stdout()
		if err != nil {
			d.logger.Error("Error setting stdout with", "err", err)
		}
		Serr, err := se.Stderr()
		if err != nil {
			d.logger.Error("Error setting stderr with", "err", err)
		}
		directory := handle.Config.TaskDir().SharedTaskDir
		se.cmd = &exec.Cmd{
			Args: []string{"/usr/local/bin/pot", "start", completeName},
			Dir:  directory,
			Path: potBIN,
			Process: &os.Process{
				Pid: alive,
			},
			Stdout: Sout,
			Stderr: Serr,
		}
	}

	h := &taskHandle{
		syexec:     se,
		pid:        se.containerPid,
		taskConfig: taskState.TaskConfig,
		procState:  drivers.TaskStateRunning,
		startedAt:  time.Now().Round(time.Millisecond),
		logger:     d.logger,
		exitResult: &drivers.ExitResult{
			ExitCode:  0,
			Err:       nil,
			OOMKilled: false,
			Signal:    0,
		},
	}

	driverState := TaskState{
		ContainerName: driverConfig.Image,
		PID:           se.containerPid,
		TaskConfig:    handle.Config,
		StartedAt:     h.startedAt,
	}

	d.logger.Trace("RECOVER TASK", "taskState before", driverState)

	if err := handle.SetDriverState(&driverState); err != nil {
		d.logger.Error("failed to start task, error setting driver state", "error", err)
		//Destroy container if err on setting driver state
		se.destroyContainer(handle.Config)
		return fmt.Errorf("failed to set driver state: %v", err)
	}

	d.logger.Trace("RECOVER TASK", "taskState after", driverState)

	d.tasks.Set(taskState.TaskConfig.ID, h)

	d.logger.Trace("RECOVER TASK", "h", h)
	if alive == 0 {
		go h.run()
	} else {
		h.procState = drivers.TaskStateRunning
		h.exitResult.ExitCode = h.syexec.exitCode
		h.exitResult.Signal = 0
		h.completedAt = time.Now()
	}

	go d.recoverWait(handle.Config.ID, se)

	d.logger.Trace("###########################################################################################################################################")
	d.logger.Trace("########################################################/RECOVER-TASK######################################################################")
	d.logger.Trace("###########################################################################################################################################")
	return nil
}

// StartTask setup the task exec and calls the container excecutor
func (d *Driver) StartTask(cfg *drivers.TaskConfig) (*drivers.TaskHandle, *drivers.DriverNetwork, error) {
	d.logger.Trace("###########################################################################################################################################")
	d.logger.Trace("########################################################STARTTASK##########################################################################")
	d.logger.Trace("###########################################################################################################################################")

	if _, ok := d.tasks.Get(cfg.ID); ok {
		return nil, nil, fmt.Errorf("task with ID %q already started", cfg.ID)
	}

	var driverConfig TaskConfig
	if err := cfg.DecodeDriverConfig(&driverConfig); err != nil {
		return nil, nil, fmt.Errorf("failed to decode driver config in STARTTASK: %v", err)
	}

	d.logger.Trace("StartTask", "driverConfig", driverConfig)

	handle := drivers.NewTaskHandle(taskHandleVersion)
	handle.Config = cfg

	se, err := prepareContainer(cfg, driverConfig)
	if err != nil {
		return nil, nil, err
	}

	se.logger = d.logger
	//se.logger.Info("Checking container alive from StartTask")
	alive := se.checkContainerAlive(cfg)
	if alive == 0 {
		d.logger.Trace("StartTask", "Container not alive", alive)
		exists := se.checkContainerExists(cfg)
		if exists == 0 {
			if err := se.createContainer(cfg); err != nil {
				//Destroy container if err on creation
				err := se.destroyContainer(cfg)
				if err != nil {
					d.logger.Error("Error destroying container with err: ", err)
				}
				return nil, nil, fmt.Errorf("unable to create container: %v", err)
			}
			d.logger.Trace("StartTask", "Created container, se:", se)

			if err := se.startContainer(cfg); err != nil {
				err := se.destroyContainer(cfg)
				if err != nil {
					d.logger.Error("Error destroying container with err: ", err)
				}
				return nil, nil, fmt.Errorf("unable to start container: %v", err)
			}
			d.logger.Trace("StartTask", "Started task, se", se)
		} else {
			d.logger.Trace("StartTask", "Container existed, se", se)
			if err := se.startContainer(cfg); err != nil {
				err := se.destroyContainer(cfg)
				if err != nil {
					d.logger.Error("Error destroying container with err: ", err)
				}
				return nil, nil, fmt.Errorf("unable to start container: %v", err)
			}
		}
	} else {
		se.containerPid = alive
		completeName := cfg.JobName + cfg.Name + "_" + cfg.AllocID

		se.cmd = &exec.Cmd{
			Args: []string{"/usr/local/bin/pot", "start", completeName},
			Dir:  cfg.AllocDir,
			Path: potBIN,
			Process: &os.Process{
				Pid: alive,
			},
		}
		d.logger.Trace("StartTask", "RECOVER TASK cmd", se.cmd)
	}
	h := &taskHandle{
		syexec:     se,
		pid:        se.containerPid,
		taskConfig: cfg,
		procState:  drivers.TaskStateRunning,
		startedAt:  time.Now().Round(time.Millisecond),
		logger:     d.logger,
	}

	driverState := TaskState{
		ContainerName: driverConfig.Image,
		PID:           se.containerPid,
		TaskConfig:    cfg,
		StartedAt:     h.startedAt,
	}

	var networkConfig drivers.DriverNetwork
	if driverConfig.IP != "" {
		networkConfig.IP = driverConfig.IP
		networkConfig.AutoAdvertise = true

		ports := make(map[string]int)
		for name, port := range driverConfig.PortMap {
			portString, err := strconv.Atoi(port)
			if err != nil {
				fmt.Println("Error converting port string to int")
				return nil, nil, err
			}
			ports[name] = portString
		}

		networkConfig.PortMap = ports
	}

	d.logger.Trace("START TASK", "taskState", driverState)

	if err := handle.SetDriverState(&driverState); err != nil {
		d.logger.Error("failed to start task, error setting driver state", "error", err)
		//Destroy container if err on setting driver state
		se.destroyContainer(cfg)
		return nil, nil, fmt.Errorf("failed to set driver state: %v", err)
	}

	d.logger.Trace("START TASK", "taskState", driverState)

	d.tasks.Set(cfg.ID, h)
	if alive == 0 {
		go h.run()
	}

	go d.potWait(cfg.ID, se)
	d.logger.Trace("###########################################################################################################################################")
	d.logger.Trace("########################################################/STARTTASK#########################################################################")
	d.logger.Trace("###########################################################################################################################################")
	return handle, &networkConfig, nil
}

func (d *Driver) recoverWait(id string, se syexec) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

OuterLoop:
	for {
		select {
		case <-d.ctx.Done():
			break OuterLoop
		case <-ticker.C:
			//d.logger.Info("Checking containerAlive from Ticker recoverWait")
			code := se.checkContainerAlive(se.cfg)
			if code == 0 {
				d.logger.Error("Container", "RecoverWait Break", se.cfg.JobName)
				break OuterLoop
			}
		}
	}

	handle, _ := d.tasks.Get(id)
	handle.procState = drivers.TaskStateExited
}

func (d *Driver) potWait(taskID string, se syexec) {
	handle, _ := d.tasks.Get(taskID)
	err := se.cmd.Wait()
	if err != nil {
		d.logger.Error("Error exiting se.cmd.Wait in potWait", "Err", err)
	}
	handle.procState = drivers.TaskStateExited

}

// WaitTask waits for task completion
func (d *Driver) WaitTask(ctx context.Context, taskID string) (<-chan *drivers.ExitResult, error) {
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		d.logger.Error("WaitTask", "handle", "!ok")
		return nil, drivers.ErrTaskNotFound
	}

	ch := make(chan *drivers.ExitResult)
	go d.handleWait(ctx, handle, ch)

	return ch, nil
}

func (d *Driver) handleWait(ctx context.Context, handle *taskHandle, ch chan *drivers.ExitResult) {
	defer close(ch)

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			s := handle.TaskStatus()
			//d.logger.Error("handleWait", "handle", s)
			if s.State == drivers.TaskStateExited {
				ch <- handle.exitResult
			}
		}
	}
}

// StopTask shutdown a tasked based on its taskID
func (d *Driver) StopTask(taskID string, timeout time.Duration, signal string) error {
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return drivers.ErrTaskNotFound
	}

	var driverConfig TaskConfig

	if err := handle.taskConfig.DecodeDriverConfig(&driverConfig); err != nil {
		//return fmt.Errorf("failed to decode driver config in STOPTASK: %v", err)
		d.logger.Error("unable to decode driver in STOPTASK:", err)
	}

	se := prepareStop(handle.taskConfig, driverConfig)

	se.logger = d.logger

	if err := se.stopContainer(handle.taskConfig); err != nil {
		se.logger.Error("unable to run stopContainer: %v", err)
	}

	se = prepareDestroy(handle.taskConfig, driverConfig)

	se.logger = d.logger

	if err := se.destroyContainer(handle.taskConfig); err != nil {
		return fmt.Errorf("unable to run destroyContainer: %v", err)
	}

	return nil
}

// DestroyTask delete task
func (d *Driver) DestroyTask(taskID string, force bool) error {
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return drivers.ErrTaskNotFound
	}

	if handle.IsRunning() && !force {
		return fmt.Errorf("cannot destroy running task")
	}

	d.tasks.Delete(taskID)
	return nil
}

// InspectTask retrieves task info
func (d *Driver) InspectTask(taskID string) (*drivers.TaskStatus, error) {
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return nil, drivers.ErrTaskNotFound
	}

	return handle.TaskStatus(), nil
}

// TaskStats get task stats
func (d *Driver) TaskStats(ctx context.Context, taskID string, interval time.Duration) (<-chan *drivers.TaskResourceUsage, error) {
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return nil, drivers.ErrTaskNotFound
	}

	return handle.stats(ctx, interval)
}

// TaskEvents return a chan *drivers.TaskEvent
func (d *Driver) TaskEvents(ctx context.Context) (<-chan *drivers.TaskEvent, error) {
	return d.eventer.TaskEvents(ctx)
}

// SignalTask send a specific signal to a taskID
func (d *Driver) SignalTask(taskID string, signal string) error {
	return fmt.Errorf("Pot driver does not support signals")
}

// ExecTask calls a exec cmd over a running task
func (d *Driver) ExecTask(taskID string, cmd []string, timeout time.Duration) (*drivers.ExecTaskResult, error) {
	return nil, fmt.Errorf("POT driver does not support exec") //TODO
}
