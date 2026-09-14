//go:build !windows

package terminal

import "os/exec"

type processGroup struct{ cmd *exec.Cmd }

func startManagedProcess(cmd *exec.Cmd) (*processGroup, error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &processGroup{cmd: cmd}, nil
}

func (g *processGroup) kill() error { killProcessTree(g.cmd); return nil }
func (g *processGroup) wait() error { return nil }
