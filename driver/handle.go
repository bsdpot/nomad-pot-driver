package pot

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	hclog "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/plugins/drivers"
)

type taskHandle struct {
	syexec syexec
	pid    int
	logger hclog.Logger

	// stateLock syncs access to all fields below
	stateLock sync.RWMutex

	taskConfig  *drivers.TaskConfig
	procState   drivers.TaskState
	startedAt   time.Time
	completedAt time.Time
	exitResult  *drivers.ExitResult
}

func (h *taskHandle) TaskStatus() *drivers.TaskStatus {
	h.stateLock.RLock()
	defer h.stateLock.RUnlock()

	return &drivers.TaskStatus{
		ID:          h.taskConfig.ID,
		Name:        h.taskConfig.Name,
		State:       h.procState,
		StartedAt:   h.startedAt,
		CompletedAt: h.completedAt,
		ExitResult:  h.exitResult,
		DriverAttributes: map[string]string{
			"pid": strconv.Itoa(h.pid),
		},
	}
}

func (h *taskHandle) IsRunning() bool {
	h.stateLock.RLock()
	defer h.stateLock.RUnlock()

	return h.procState == drivers.TaskStateRunning
}

func (h *taskHandle) run() {
	h.stateLock.Lock()
	if h.exitResult == nil {
		h.exitResult = &drivers.ExitResult{}
	}
	h.stateLock.Unlock()

	h.stateLock.Lock()
	defer h.stateLock.Unlock()

	if h.syexec.ExitError != nil {
		h.exitResult.Err = h.syexec.ExitError
		h.procState = drivers.TaskStateUnknown
		h.completedAt = time.Now()
		return
	}

	h.procState = drivers.TaskStateRunning
	h.exitResult.ExitCode = h.syexec.exitCode
	h.exitResult.Signal = 0
	h.completedAt = time.Now()
}

// shutdown shuts down the container, with `timeout` grace period
// before killing the container with SIGKILL.
func (h *taskHandle) shutdown(timeout time.Duration) error {
	// Wait for the process to finish or kill it after a timeout (whichever happens first):
	h.procState = drivers.TaskStateExited
	done := make(chan error, 1)
	go func() {
		done <- h.syexec.cmd.Wait()
	}()
	select {
	case <-time.After(timeout * time.Second):

		if err := h.syexec.cmd.Process.Kill(); err != nil {
			return fmt.Errorf("failed to kill process: %v ", err)
		}
	case err := <-done:
		if err != nil {
			return fmt.Errorf("process finished with error = %v", err)
		}
	}

	return nil
}

func (h *taskHandle) stats(ctx context.Context, interval time.Duration) (<-chan *drivers.TaskResourceUsage, error) {
	ch := make(chan *drivers.TaskResourceUsage)
	go h.handleStats(ctx, ch, interval)
	return ch, nil
}

func (h *taskHandle) handleStats(ctx context.Context, ch chan *drivers.TaskResourceUsage, interval time.Duration) {
	defer close(ch)
	timer := time.NewTimer(0)

	for {
		select {
		case <-ctx.Done():
			return

		case <-timer.C:
			timer.Reset(interval)
		}

		t := time.Now()
		potStats, err := h.syexec.containerStats(h.taskConfig)
		if err != nil {
			time.Sleep(5 * time.Second)
			return
		}

		// Get the cpu stats
		cs := &drivers.CpuStats{
			Percent:    float64(potStats.ResourceUsage.CPUStats.Percent),
			TotalTicks: potStats.ResourceUsage.CPUStats.TotalTicks,
			Measured:   []string{"Percent", "TotalTicks"},
		}

		ms := &drivers.MemoryStats{
			RSS:      uint64(potStats.ResourceUsage.MemoryStats.RSS),
			Cache:    0,
			Swap:     0,
			Measured: []string{"RSS", "Cache", "Swap"},
		}

		taskResUsage := drivers.TaskResourceUsage{
			ResourceUsage: &drivers.ResourceUsage{
				CpuStats:    cs,
				MemoryStats: ms,
			},
			Timestamp: t.UTC().UnixNano(),
		}

		select {
		case <-ctx.Done():
			return
		case ch <- &taskResUsage:
		}
	}
}
