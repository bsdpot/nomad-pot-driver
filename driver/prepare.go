package pot

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"strconv"
	"strings"

	"github.com/hashicorp/nomad/client/lib/fifo"
	"github.com/hashicorp/nomad/plugins/drivers"
)

// prepareContainer preloads the taskcnf into args to be passed to a execCmd
func prepareContainer(cfg *drivers.TaskConfig, taskCfg TaskConfig) (syexec, error) {
	argv := make([]string, 0, 50)
	var se syexec
	se.taskConfig = taskCfg
	se.cfg = cfg
	se.env = cfg.EnvList()

	// action can be run/exec

	argv = append(argv, "prepare", "-U", taskCfg.Image, "-p", taskCfg.Pot, "-t", taskCfg.Tag)

	if cfg.AllocID != "" {
		argv = append(argv, "-a", cfg.AllocID)
	}

	if len(taskCfg.Args) > 0 {
		if taskCfg.Command == "" {
			err := errors.New("command can not be empty if arguments are provided")
			fmt.Println("command can not be empty if arguments are provided")
			return se, err
		}

		for _, arg := range taskCfg.Args {
			taskCfg.Command = taskCfg.Command + " " + arg
		}
	}

	if taskCfg.Command != "" {
		taskCfg.Command = "\"" + taskCfg.Command + "\""
		argv = append(argv, "-c", taskCfg.Command)
	}
	if taskCfg.NetworkMode != "" {
		argv = append(argv, "-N", taskCfg.NetworkMode)
	} else if len(taskCfg.PortMap) > 0 {
		argv = append(argv, "-N", "public-bridge", "-i", "auto")
	}

	if taskCfg.NetworkMode != "host" {
		for name, port := range taskCfg.PortMap {
			envname := "NOMAD_HOST_PORT_" + name
			sPort := cfg.Env[envname]
			completePort := port + ":" + sPort
			argv = append(argv, "-e", completePort)
		}
	}

	completeName := cfg.JobName + cfg.Name
	argv = append(argv, "-n", completeName, "-v")

	se.argvCreate = argv

	potName := completeName + "_" + cfg.AllocID

	//Mount local
	commandLocal := "mount-in -p " + potName + " -d " + cfg.TaskDir().LocalDir + " -m /local"
	se.argvMount = append(se.argvMount, commandLocal)

	//Mount secrets
	commandSecret := "mount-in -p " + potName + " -d " + cfg.TaskDir().SecretsDir + " -m /secrets"
	se.argvMount = append(se.argvMount, commandSecret)

	if len(taskCfg.Copy) > 0 {
		argvCopy := make([]string, 0, 50)
		for _, file := range taskCfg.Copy {
			split := strings.Split(file, ":")
			source := split[0]
			destination := split[1]
			command := "copy-in -p " + potName + " -s " + source + " -d " + destination
			argvCopy = append(argvCopy, command)
		}
		se.argvCopy = argvCopy
	}

	if len(taskCfg.Mount) > 0 {
		for _, file := range taskCfg.Mount {
			split := strings.Split(file, ":")
			source := split[0]
			destination := split[1]
			command := "mount-in -p " + potName + " -d " + source + " -m " + destination
			se.argvMount = append(se.argvMount, command)
		}
	}

	if len(taskCfg.MountReadOnly) > 0 {
		argvMountReadOnly := make([]string, 0, 50)
		for _, file := range taskCfg.MountReadOnly {
			split := strings.Split(file, ":")
			source := split[0]
			destination := split[1]
			command := "mount-in -p " + potName + " -d " + source + " -m " + destination + " -r"
			argvMountReadOnly = append(argvMountReadOnly, command)
		}
		se.argvMountReadOnly = argvMountReadOnly
	}

	// Set env variables
	if len(cfg.EnvList()) > 0 {
		command := potBIN + " set-env -p " + potName + " "
		for name, env := range cfg.Env {
			command = command + " -E " + name + "=" + env
		}
		argvEnv := command
		se.argvEnv = argvEnv
	}

	if len(taskCfg.ExtraHosts) > 0 {
		hostCommand := potBIN + " set-hosts -p " + potName
		for _, host := range taskCfg.ExtraHosts {
			hostCommand = hostCommand + " -H " + host
		}
		se.argvExtraHosts = hostCommand
	}

	//Set soft memory limit
	memoryLimit := cfg.Resources.NomadResources.Memory.MemoryMB
	sMemoryLimit := strconv.FormatInt(memoryLimit, 10)
	argvMem := potBIN + " set-rss -M " + sMemoryLimit + "M -p " + potName
	se.argvMem = argvMem

	argvStart := make([]string, 0, 50)
	argvStart = append(argvStart, "start", potName)
	se.argvStart = argvStart

	argvStop := make([]string, 0, 50)
	argvStop = append(argvStop, "stop", potName)
	se.argvStop = argvStop

	argvDestroy := make([]string, 0, 50)
	argvDestroy = append(argvDestroy, "destroy", "-p", potName)
	se.argvDestroy = argvDestroy

	argvStats := make([]string, 0, 50)
	argvStats = append(argvStats, "get-rss", "-p", potName, "-J")
	se.argvStats = argvStats

	return se, nil
}

type nopCloser struct {
	io.Writer
}

func (nopCloser) Close() error { return nil }

// Stdout returns a writer for the configured file descriptor
func (s *syexec) Stdout() (io.WriteCloser, error) {
	if s.stdout == nil {
		if s.cfg.StdoutPath != "" {
			f, err := fifo.OpenWriter(s.cfg.StdoutPath)
			if err != nil {
				return nil, fmt.Errorf("failed to create stdout: %v", err)
			}
			s.stdout = f
		} else {
			s.stdout = nopCloser{ioutil.Discard}
		}
	}
	return s.stdout, nil
}

// Stderr returns a writer for the configured file descriptor
func (s *syexec) Stderr() (io.WriteCloser, error) {
	if s.stderr == nil {
		if s.cfg.StderrPath != "" {
			f, err := fifo.OpenWriter(s.cfg.StderrPath)
			if err != nil {
				return nil, fmt.Errorf("failed to create stderr: %v", err)
			}
			s.stderr = f
		} else {
			s.stderr = nopCloser{ioutil.Discard}
		}
	}
	return s.stderr, nil
}

func (s *syexec) Close() {
	if s.stdout != nil {
		s.stdout.Close()
	}
	if s.stderr != nil {
		s.stderr.Close()
	}
}

func prepareStop(cfg *drivers.TaskConfig, taskCfg TaskConfig) syexec {
	argv := make([]string, 0, 50)
	var se syexec
	se.taskConfig = taskCfg
	se.cfg = cfg
	se.env = cfg.EnvList()

	// action can be run/exec

	argv = append(argv, "stop")

	completeName := cfg.JobName + cfg.Name + "_" + cfg.AllocID

	argv = append(argv, completeName)

	se.argvStop = append(argv, taskCfg.Args...)

	return se
}

func prepareDestroy(cfg *drivers.TaskConfig, taskCfg TaskConfig) syexec {
	argv := make([]string, 0, 50)
	var se syexec
	se.taskConfig = taskCfg
	se.cfg = cfg
	se.env = cfg.EnvList()

	// action can be run/exec

	argv = append(argv, "destroy")

	completeName := cfg.JobName + cfg.Name + "_" + cfg.AllocID

	argv = append(argv, "-p", completeName)

	se.argvDestroy = append(argv, taskCfg.Args...)

	return se

}
