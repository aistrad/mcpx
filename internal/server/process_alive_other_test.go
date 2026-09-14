//go:build !windows

package server

import (
	"errors"
	"syscall"
)

func fixtureProcessAlive(pid int) (bool, error) {
	err := syscall.Kill(pid, 0)
	if errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	return err == nil, err
}
