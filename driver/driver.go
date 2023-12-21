package pot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/hashicorp/consul-template/signals"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/drivers/shared/eventer"
	"github.com/hashicorp/nomad/helper/pluginutils/hclutils"
	"github.com/hashicorp/nomad/plugins/base"
	"github.com/hashicorp/nomad/plugins/drivers"
	"github.com/hashicorp/nomad/plugins/shared/hclspec"
	"github.com/hashicorp/nomad/plugins/shared/structs"
)

const (
	// pluginName is the name of the plugin
	pluginName = "pot"

	// pluginVersion allows the client to identify and use newer versions of
	// an installed plugin
	pluginVersion = "v0.10.0"

	// fingerprintPeriod is the interval at which the driver will send fingerprint responses
	fingerprintPeriod = 30 * time.Second

	// taskHandleVersion is the version of task handle which this driver sets
	// and understands how to decode driver state
	taskHandleVersion = 2

	// potBIN is the singularity binary path.
	potBIN = "/usr/local/bin/pot"
)

var (
	// pluginInfo is the response returned for the PluginInfo RPC
	pluginInfo = &base.PluginInfoResponse{
		Type:              base.PluginTypeDriver,
		PluginApiVersions: []string{drivers.ApiVersion010},
		PluginVersion:     pluginVersion,
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
		"mount":           hclspec.NewAttr("mount", "list(string)", false),
		"copy":            hclspec.NewAttr("copy", "list(string)", false),
		"mount_read_only": hclspec.NewAttr("mount_read_only", "list(string)", false),
		"extra_hosts":     hclspec.NewAttr("extra_hosts", "list(string)", false),
		"attributes":      hclspec.NewAttr("attributes", "list(string)", false),
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

	// Enable debug-verbose global options
	Debug   bool `codec:"debug"`
	Verbose bool `codec:"verbose"`

	Mount         []string `codec:"mount"`           // Host-Volumes to mount in, syntax: /path/to/host/directory:/destination/path/in/container
	MountReadOnly []string `codec:"mount_read_only"` // Host-Volumes to mount in, syntax: /path/to/host/directory:/destination/path/in/container
	Copy          []string `codec:"copy"`            // Files in host to copy in, syntax: /path/to/host/file.ext:/destination/path/in/container/file.ext
	ExtraHosts    []string `codec:"extra_hosts"`     // ExtraHosts a list of hosts, given as host:IP, to be added to /etc/hosts
	Attributes    []string `codec:"attributes"`     // Pot attributes, syntax: Attribute:Value
}

// TaskState is the state which is encoded in the handle returned in
// StartTask. This information is needed to rebuild the task state and handler
// during recovery.
type TaskState struct {
	ReattachConfig *structs.ReattachConfig
	TaskConfig     *drivers.TaskConfig
	StartedAt      time.Time
	ContainerName  string
	PID            int
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
	fp := &drivers.Fingerprint{
		Attributes:        map[string]*structs.Attribute{},
		Health:            drivers.HealthStateHealthy,
		HealthDescription: drivers.DriverHealthy,
	}

	if d.config.Enabled && pluginVersion != "" {
		fp.Health = drivers.HealthStateHealthy
		fp.HealthDescription = "healthy"
		fp.Attributes["driver.pot"] = structs.NewBoolAttribute(true)
		fp.Attributes["driver.pot.version"] = structs.NewStringAttribute(pluginVersion)
	} else {
		fp.Health = drivers.HealthStateUndetected
		fp.HealthDescription = "disabled"
	}

	return fp
}

// RecoverTask try to recover a failed task, if not return error
func (d *Driver) RecoverTask(handle *drivers.TaskHandle) error {
	d.logger.Trace("###########################################################################################################################################")
	d.logger.Trace("########################################################RECOVER-TASK#######################################################################")
	d.logger.Trace("###########################################################################################################################################")
	d.logger.Trace("RECOVER TASK", "ID", handle.Config.ID)
	if handle == nil {
		return errors.New("error: handle cannot be nil")
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
		parts := strings.Split(handle.Config.ID, "/")
		completeName := parts[1] + "_" + parts[2] + "_" + parts[0]
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
		se.destroyContainer(handle.Config, &h.calledDestroy)
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

	d.logger.Info("starting task", "driver_cfg", hclog.Fmt("%+v", driverConfig))
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
				err := se.destroyContainer(cfg, nil)
				if err != nil {
					d.logger.Error("Error destroying container with err: ", err)
				}
				return nil, nil, fmt.Errorf("unable to create container: %v", err)
			}
			d.logger.Trace("StartTask", "Created container, se:", se)

			if err := se.startContainer(cfg); err != nil {
				err := se.destroyContainer(cfg, nil)
				if err != nil {
					d.logger.Error("Error destroying container with err: ", err)
				}
				return nil, nil, fmt.Errorf("unable to start container: %v", err)
			}
			d.logger.Trace("StartTask", "Started task, se", se)
		} else {
			d.logger.Trace("StartTask", "Container existed, se", se)
			if err := se.startContainer(cfg); err != nil {
				err := se.destroyContainer(cfg, nil)
				if err != nil {
					d.logger.Error("Error destroying container with err: ", err)
				}
				return nil, nil, fmt.Errorf("unable to start container: %v", err)
			}
		}
	} else {
		se.containerPid = alive
		parts := strings.Split(cfg.ID, "/")
		completeName := parts[1] + "_" + parts[2] + "_" + parts[0]

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

	d.logger.Trace("START TASK", "taskState", driverState)

	if err := handle.SetDriverState(&driverState); err != nil {
		d.logger.Error("failed to start task, error setting driver state", "error", err)
		//Destroy container if err on setting driver state
		se.destroyContainer(cfg, &h.calledDestroy)
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
	return handle, nil, nil
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

	last_run, err := se.getContainerLastRunStats(handle.taskConfig)
	if err != nil {
		d.logger.Error("Error getting container last-run-stats with err: ", err)
		handle.exitResult.ExitCode = defaultFailedCode
	} else {
		handle.exitResult.ExitCode = last_run.ExitCode
	}

	err = se.destroyContainer(handle.taskConfig, &handle.calledDestroy)
	if err != nil {
		d.logger.Error("Error destroying container with err: ", err)
	}
}

func (d *Driver) potWait(taskID string, se syexec) {
	handle, _ := d.tasks.Get(taskID)
	err := se.cmd.Wait()
	handle.procState = drivers.TaskStateExited
	if err != nil {
		d.logger.Error("Error exiting se.cmd.Wait in potWait", "Err", err)
		handle.exitResult.ExitCode = defaultFailedCode
		if exitError, ok := err.(*exec.ExitError); ok {
			ws := exitError.Sys().(syscall.WaitStatus)
			if ws.ExitStatus() == 125 { // enclosed process exited with error
				last_run_stats, err := se.getContainerLastRunStats(handle.taskConfig)
				if err != nil {
					d.logger.Error("Error getting container last-run-stats with err: ", err)
				} else {
					handle.exitResult.ExitCode = last_run_stats.ExitCode
				}
			}
		}
	}

	err = se.destroyContainer(handle.taskConfig, &handle.calledDestroy)
	if err != nil {
		d.logger.Error("Error destroying container with err: ", err)
	}
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

	se := prepareDestroy(handle.taskConfig, driverConfig)

	se.logger = d.logger

	if err := se.destroyContainer(handle.taskConfig, &handle.calledDestroy); err != nil {
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
		return errors.New("cannot destroy running task")
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
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return drivers.ErrTaskNotFound
	}

	sig, err := signals.Parse(signal)
	if err != nil {
		return fmt.Errorf("failed to parse signal: %v", err)
	}

	var driverConfig TaskConfig

	if err := handle.taskConfig.DecodeDriverConfig(&driverConfig); err != nil {
		return fmt.Errorf("failed to decode driver config in SignalTask: %v", err)
	}

	se, err := prepareSignal(handle.taskConfig, driverConfig, sig)
	if err != nil {
		return fmt.Errorf("unable to run PrepareSignal: %v", err)
	}
	se.logger = d.logger

	if err := se.signalContainer(handle.taskConfig); err != nil {
		return fmt.Errorf("unable to run SignalContainer: %v", err)
	}

	if se.state.ExitCode != 0 {
		return fmt.Errorf("SignalContainer returned error code %v", se.state.ExitCode)
	}

	return nil
}


// ExecTask calls a exec cmd over a running task
func (d *Driver) ExecTask(taskID string, cmd []string, timeout time.Duration) (*drivers.ExecTaskResult, error) {
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return nil, drivers.ErrTaskNotFound
	}

	if len(cmd) == 0 {
		return nil, fmt.Errorf("cmd is required, but was empty")
	}

	se, err := prepareExec(handle.taskConfig, cmd)
	if err != nil {
		return nil, fmt.Errorf("unable to run PrepareCommand: %v", err)
	}
	se.logger = d.logger

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	return se.execInContainer(ctx, handle.taskConfig)
}


var _ drivers.ExecTaskStreamingRawDriver = (*Driver)(nil)

func (d *Driver) ExecTaskStreamingRaw(ctx context.Context,
	taskID string,
	command []string,
	tty bool,
	stream drivers.ExecTaskStream) error {

	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return drivers.ErrTaskNotFound
	}

	if len(command) == 0 {
		return fmt.Errorf("command is required")
	}

	se, err := prepareExec(handle.taskConfig, command)
	if err != nil {
		return fmt.Errorf("unable to run PrepareCommand: %v", err)
	}
	se.logger = d.logger

	return se.execStreaming(ctx, handle.taskConfig, tty, stream)
}
