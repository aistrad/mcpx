package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"mcpx/internal/config"
)

const (
	daemonStateFilename       = "mcpx-daemon.json"
	backgroundChildSubcommand = "__background-child"
)

type daemonState struct {
	PID        int    `json:"pid"`
	Executable string `json:"executable"`
}

func stopExistingBackground() ([]int, error) {
	layout, err := config.EnsureGlobalLayout()
	if err != nil {
		return nil, fmt.Errorf("prepare mcpx home: %w", err)
	}
	return stopPreviousBackground(filepath.Join(layout.HomeDir, daemonStateFilename))
}

func startBackground(args []string) (int, string, []int, error) {
	layout, err := config.EnsureGlobalLayout()
	if err != nil {
		return 0, "", nil, fmt.Errorf("prepare mcpx home: %w", err)
	}
	executable, err := os.Executable()
	if err != nil {
		return 0, "", nil, fmt.Errorf("resolve executable: %w", err)
	}
	statePath := filepath.Join(layout.HomeDir, daemonStateFilename)
	stoppedPIDs, err := stopPreviousBackground(statePath)
	if err != nil {
		return 0, "", nil, fmt.Errorf("stop previous daemon: %w", err)
	}
	logPath := filepath.Join(layout.LogDir, "mcpx-daemon.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, "", nil, fmt.Errorf("open daemon log: %w", err)
	}
	defer logFile.Close()

	cmd := exec.Command(executable, append([]string{backgroundChildSubcommand}, backgroundChildArgs(args)...)...)
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	configureBackgroundProcess(cmd)
	if err := cmd.Start(); err != nil {
		return 0, "", nil, fmt.Errorf("start child: %w", err)
	}
	pid := cmd.Process.Pid
	if err := writeDaemonState(statePath, daemonState{PID: pid, Executable: executable}); err != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		return 0, "", nil, fmt.Errorf("write daemon state: %w", err)
	}
	if err := cmd.Process.Release(); err != nil {
		_ = os.Remove(statePath)
		_ = cmd.Process.Kill()
		return 0, "", nil, fmt.Errorf("release child process: %w", err)
	}
	return pid, logPath, stoppedPIDs, nil
}

func stopPreviousBackground(statePath string) ([]int, error) {
	stoppedPIDs := make([]int, 0, 1)
	state, err := readDaemonState(statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if state.PID > 0 && state.Executable != "" {
		stopped, err := terminateBackgroundProcess(state.PID, state.Executable, 3*time.Second)
		if err != nil {
			return nil, err
		}
		if stopped {
			stoppedPIDs = append(stoppedPIDs, state.PID)
		}
	}
	if err := os.Remove(statePath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("remove stale daemon state: %w", err)
	}
	return stoppedPIDs, nil
}

func readDaemonState(path string) (daemonState, error) {
	var state daemonState
	data, err := os.ReadFile(path)
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, fmt.Errorf("decode %s: %w", path, err)
	}
	return state, nil
}

func writeDaemonState(path string, state daemonState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".mcpx-daemon-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func backgroundChildArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == "-d" {
			continue
		}
		out = append(out, arg)
	}
	return out
}
