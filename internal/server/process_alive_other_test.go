//go:build !windows

package server

import (
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

func fixtureProcessAlive(pid int) (bool, error) {
	err := syscall.Kill(pid, 0)
	if errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// An orphan may be a zombie until the host reaper runs; it cannot execute
	// further effects. The fixture separately asserts that its late write never occurs.
	output, probeErr := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if probeErr != nil {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return false, nil
		}
		return false, probeErr
	}
	state := strings.TrimSpace(string(output))
	return state != "" && !strings.HasPrefix(state, "Z"), nil
}
