//go:build !windows

package terminal

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type processGroup struct{ cmd *exec.Cmd }

func startManagedProcess(cmd *exec.Cmd) (*processGroup, error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &processGroup{cmd: cmd}, nil
}

func (g *processGroup) kill() error { killProcessTree(g.cmd); return nil }

// A task is not finished when its original parent exits if descendants in its
// managed group can still produce effects. Keep the task cancellable until the
// group is quiescent, matching the job-object contract on Windows.
func (g *processGroup) wait() error {
	if g == nil || g.cmd == nil || g.cmd.Process == nil {
		return nil
	}
	for {
		alive, err := processGroupAlive(g.cmd.Process.Pid)
		if err != nil {
			_ = g.kill()
			return err
		}
		if !alive {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func processGroupAlive(pgid int) (bool, error) {
	err := syscall.Kill(-pgid, 0)
	if errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	if err != nil && !errors.Is(err, syscall.EPERM) {
		return false, err
	}
	// kill(0) alone counts zombies. They cannot execute and may await a different
	// parent/reaper; waiting for that external reaper can hang a completed task.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "ps", "-A", "-o", "pgid=", "-o", "stat=").Output()
	if err != nil {
		return false, fmt.Errorf("inspect managed process group: %w", err)
	}
	group := strconv.Itoa(pgid)
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == group && !strings.HasPrefix(fields[1], "Z") {
			return true, nil
		}
	}
	return false, nil
}
