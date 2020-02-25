package pot

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	hclog "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/plugins/drivers"
)

const (
	// defaultFailedCode for singularity runtime
	defaultFailedCode = 255
)

type syexec struct {
	argvCreate        []string
	argvCopy          []string
	argvMount         []string
	argvMountReadOnly []string
	argvMem           string
	argvEnv           string
	argvExtraHosts    string
	argvStart         []string
	argvStop          []string
	argvStats         []string
	argvDestroy       []string
	cmd               *exec.Cmd
	cachedir          string
	taskConfig        TaskConfig
	cfg               *drivers.TaskConfig
	stdout            io.WriteCloser
	stderr            io.WriteCloser
	env               []string
	TaskDir           string
	state             *psState
	containerPid      int
	exitCode          int
	ExitError         error
	logger            hclog.Logger
}

type psState struct {
	Pid      int
	ExitCode int
	Signal   int
	Time     time.Time
}

type potStats struct {
	ResourceUsage struct {
		MemoryStats struct {
			RSS int `json:"RSS"`
		} `json:"MemoryStats"`
		CPUStats struct {
			TotalTicks    float64 `json:"TotalTicks"`
			Percent       int     `json:"Percent"`
			OldTotalTicks int
		} `json:"CpuStats"`
	} `json:"ResourceUsage"`
}

var potStatistics map[string]potStats

func init() {
	potStatistics = make(map[string]potStats)
}

func (s *syexec) startContainer(commandCfg *drivers.TaskConfig) error {
	s.logger.Debug("launching StartContainer command", strings.Join(s.argvStart, " "))

	cmd := exec.Command(potBIN, s.argvStart...)

	// set the writers for stdout and stderr
	stdout, err := s.Stdout()
	if err != nil {
		return err
	}
	stderr, err := s.Stderr()
	if err != nil {
		return err
	}

	cmd.Stdout = stdout
	cmd.Stderr = stderr

	// set the task dir as the working directory for the command
	cmd.Dir = commandCfg.TaskDir().Dir
	cmd.Path = potBIN
	cmd.Args = append([]string{cmd.Path}, s.argvStart...)

	//cmdFull := strings.Join(cmd.Args, " ")

	// Start the process
	cmd.Start()

	s.cmd = cmd

	s.state = &psState{Pid: s.cmd.Process.Pid, ExitCode: s.exitCode, Time: time.Now()}
	s.logger.Debug("Starting container", "psState", hclog.Fmt("%+v", s.state))

	if s.exitCode != 0 {
		ExitCodeString := strconv.Itoa(s.exitCode)
		m := "Failed to start container with exitcode: " + ExitCodeString
		err = errors.New(m)
		return err
	}

	return nil
}

func (s *syexec) stopContainer(commandCfg *drivers.TaskConfig) error {
	s.logger.Debug("launching StopContainer command", strings.Join(s.argvStop, " "))

	cmd := exec.Command(potBIN, s.argvStop...)

	// set the task dir as the working directory for the command
	cmd.Dir = commandCfg.TaskDir().Dir
	cmd.Path = potBIN
	cmd.Args = append([]string{cmd.Path}, s.argvStop...)

	// Start the process
	if err := cmd.Run(); err != nil {
		// try to get the exit code
		if exitError, ok := err.(*exec.ExitError); ok {
			ws := exitError.Sys().(syscall.WaitStatus)
			s.exitCode = ws.ExitStatus()
		} else {
			s.logger.Error("Could not get exit code for stopping container ", "pot", s.argvStop)
			s.exitCode = defaultFailedCode
		}
	} else {
		// success, exitCode should be 0 if go is ok
		ws := cmd.ProcessState.Sys().(syscall.WaitStatus)
		s.exitCode = ws.ExitStatus()
	}

	s.cmd = cmd

	s.state = &psState{Pid: s.cmd.Process.Pid, ExitCode: s.exitCode, Time: time.Now()}
	return nil
}

func (s *syexec) destroyContainer(commandCfg *drivers.TaskConfig) error {
	s.argvDestroy = append(s.argvDestroy, "-F")
	s.logger.Debug("launching DestroyContainer command", strings.Join(s.argvDestroy, " "))

	cmd := exec.Command(potBIN, s.argvDestroy...)

	// set the task dir as the working directory for the command
	cmd.Dir = commandCfg.TaskDir().Dir
	cmd.Path = potBIN
	cmd.Args = append([]string{cmd.Path}, s.argvDestroy...)

	// Start the process
	if err := cmd.Run(); err != nil {
		// try to get the exit code
		if exitError, ok := err.(*exec.ExitError); ok {
			ws := exitError.Sys().(syscall.WaitStatus)
			s.exitCode = ws.ExitStatus()
		} else {
			s.logger.Error("Could not get exit code for destroying container ", "pot", s.argvDestroy)
			s.exitCode = defaultFailedCode
		}
	} else {
		// success, exitCode should be 0 if go is ok
		ws := cmd.ProcessState.Sys().(syscall.WaitStatus)
		s.exitCode = ws.ExitStatus()
	}

	s.cmd = cmd

	s.state = &psState{Pid: s.cmd.Process.Pid, ExitCode: s.exitCode, Time: time.Now()}
	return nil
}

func (s *syexec) createContainer(commandCfg *drivers.TaskConfig) error {
	s.logger.Debug("launching createContainer command", "log", strings.Join(s.argvCreate, " "))

	cmd := exec.Command(potBIN, s.argvCreate...)

	// set the task dir as the working directory for the command
	cmd.Dir = commandCfg.TaskDir().Dir
	cmd.Path = potBIN
	cmd.Args = append([]string{cmd.Path}, s.argvCreate...)

	var outb, errb bytes.Buffer
	cmd.Stdout = &outb
	cmd.Stderr = &errb

	// Start the process
	var err error
	if err = cmd.Run(); err != nil {
		// try to get the exit code
		if exitError, ok := err.(*exec.ExitError); ok {
			ws := exitError.Sys().(syscall.WaitStatus)
			s.exitCode = ws.ExitStatus()
		} else {
			s.logger.Error("Could not get exit code for creating container: ", "pot", s.argvCreate)
			s.exitCode = defaultFailedCode
		}
	} else {
		// success, exitCode should be 0 if go is ok
		ws := cmd.ProcessState.Sys().(syscall.WaitStatus)
		s.exitCode = ws.ExitStatus()
	}

	if s.exitCode != 0 {
		s.logger.Error("Error creating container", "err", err)
		return err
	}

	s.cmd = cmd

	s.state = &psState{Pid: s.cmd.Process.Pid, ExitCode: s.exitCode, Time: time.Now()}

	//Copy
	if len(s.argvCopy) > 0 {
		for _, command := range s.argvCopy {
			message := potBIN + " " + command
			s.logger.Debug("Copying files on jail: ", message)

			cmdFiles := potBIN + " " + command
			output, err := exec.Command("sh", "-c", cmdFiles).Output()
			if err != nil {
				if exitError, ok := err.(*exec.ExitError); ok {
					ws := exitError.Sys().(syscall.WaitStatus)
					s.logger.Error("ExitError copying files", "exitStatus ", ws.ExitStatus())
					return errors.New(string(output))
				}
				s.logger.Error("Could not get exit code for copy command ", "pot", command)

			}
		}
	}

	//Mount
	if len(s.argvMount) > 0 {
		for _, command := range s.argvMount {
			message := potBIN + " " + command
			s.logger.Debug("Mounting files on jail: ", message)

			cmdVolumes := potBIN + " " + command
			output, err := exec.Command("sh", "-c", cmdVolumes).Output()
			if err != nil {
				if exitError, ok := err.(*exec.ExitError); ok {
					ws := exitError.Sys().(syscall.WaitStatus)
					s.logger.Error("ExitError Mounting Files", "exitStatus", ws.ExitStatus())
					return errors.New(string(output))
				}
				s.logger.Error("Could not get exit code for mount command ", "pot", command)
			}
		}
	}

	//Mount Read Only
	if len(s.argvMountReadOnly) > 0 {
		for _, command := range s.argvMountReadOnly {
			message := potBIN + " " + command
			s.logger.Debug("Mounting READ only files on jail: ", message)

			cmdVolumesRO := potBIN + " " + command
			output, err := exec.Command("sh", "-c", cmdVolumesRO).Output()
			if err != nil {
				if exitError, ok := err.(*exec.ExitError); ok {
					ws := exitError.Sys().(syscall.WaitStatus)
					s.logger.Error("ExitError Mounting r-only files", "exitStatus", ws.ExitStatus())
					return errors.New(string(output))
				}
				s.logger.Error("Could not get exit code for mounting read only container ", "pot", command)

			}
		}
	}

	// Set env variable inside the pot
	envMessage := "Setting env variables inside the pot: " + s.argvEnv
	s.logger.Debug(envMessage)

	_, err = exec.Command("sh", "-c", s.argvEnv).Output()
	if err != nil {
		message := "Error setting env variables for pot with err: " + err.Error()
		return errors.New(string(message))
	}

	// Set hosts file inside the pot
	hostsMessage := "Setting env variables inside the pot: " + s.argvExtraHosts
	s.logger.Debug(hostsMessage)

	_, err = exec.Command("sh", "-c", s.argvExtraHosts).Output()
	if err != nil {
		message := "Error setting hosts file for pot with err: " + err.Error()
		return errors.New(string(message))
	}

	//Set memory limit for pot
	message := "Setting memory soft limit on jail: " + s.argvMem
	s.logger.Debug(message)

	_, err = exec.Command("sh", "-c", s.argvMem).Output()
	if err != nil {
		message := "Error setting memory limit for pot with err: " + err.Error()
		return errors.New(string(message))
	}

	return nil
}

func (s *syexec) containerStats(commandCfg *drivers.TaskConfig) (stats potStats, err error) {

	cmd := exec.Command(potBIN, s.argvStats...)

	// set the task dir as the working directory for the command
	cmd.Dir = commandCfg.TaskDir().Dir
	cmd.Path = potBIN
	cmd.Args = append([]string{cmd.Path}, s.argvStats...)

	var outb, errb bytes.Buffer
	cmd.Stdout = &outb
	cmd.Stderr = &errb

	// Start the process
	if err := cmd.Run(); err != nil {
		// try to get the exit code
		if exitError, ok := err.(*exec.ExitError); ok {
			ws := exitError.Sys().(syscall.WaitStatus)
			s.exitCode = ws.ExitStatus()
		} else {
			s.logger.Error("Could not get exit code for container stats: ", "pot", s.argvStats)
			s.exitCode = defaultFailedCode
		}
	} else {
		// success, exitCode should be 0 if go is ok
		ws := cmd.ProcessState.Sys().(syscall.WaitStatus)
		s.exitCode = ws.ExitStatus()
	}

	s.cmd = cmd

	s.state = &psState{Pid: s.cmd.Process.Pid, ExitCode: s.exitCode, Time: time.Now()}

	var potStats potStats

	if s.exitCode != 0 {
		err = errors.New("Pot exit code different than 0")
		return potStats, err
	}

	err = json.Unmarshal([]byte(outb.String()), &potStats)
	if err != nil {
		s.logger.Error("Error unmarshaling lucas json with err: ", err)
		return potStats, err
	}

	return potStats, nil
}

func (s *syexec) checkContainerAlive(commandCfg *drivers.TaskConfig) int {
	s.logger.Trace("Checking if pot is alive", "Checking")
	completeName := commandCfg.JobName + commandCfg.Name
	potName := completeName + "_" + commandCfg.AllocID
	s.logger.Trace("Allocation name beeing check for liveness", "alive", potName)

	psCommand := "/bin/sh /usr/local/bin/pot start " + potName
	pidCommand := "/bin/pgrep -f '" + psCommand + "'"
	s.logger.Trace("Command to execute", "alive", pidCommand)
	output, err := exec.Command("sh", "-c", pidCommand).Output()
	s.logger.Trace("Got output", "output:", string(output), "err: ", err)
	if err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			ws := exitError.Sys().(syscall.WaitStatus)
			s.logger.Error("ExitError checkContainerAlive", "exitStatus", ws.ExitStatus())
			return 0
		}
		s.logger.Error("Could not get exit code for ps command ", "pot", err)
	}
	pidString := string(output)
	pidString = strings.TrimSpace(pidString)
	s.logger.Trace("Command output:", "alive", pidString)
	pid, err := strconv.Atoi(pidString)
	if err != nil {
		s.logger.Error("Error converting PID into int", "alive", "0")
		return 0
	}
	s.logger.Trace("Got PID", "alive", pid)
	return pid
}

func (s *syexec) checkContainerExists(commandCfg *drivers.TaskConfig) int {
	s.logger.Debug("Checking if pot is alive")
	completeName := commandCfg.JobName + commandCfg.Name
	potName := completeName + "_" + commandCfg.AllocID
	s.logger.Trace("Allocation name beeing check for liveness", "alive", potName)

	pidCommand := "/usr/local/bin/pot ls -q | grep " + potName
	s.logger.Trace("Command to execute", "exists", pidCommand)

	output, err := exec.Command("sh", "-c", pidCommand).Output()
	if err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			ws := exitError.Sys().(syscall.WaitStatus)
			s.logger.Error("ExitError CheckContainerExists", "exitError", ws.ExitStatus())
			return 0
		}
		s.logger.Error("Could not get exit code for ps command ", "pot", err)
	}
	result := string(output)
	result = strings.TrimSpace(result)
	s.logger.Trace("EXIST", "Result", result)
	if result == potName {
		return 1
	}

	return 0
}
