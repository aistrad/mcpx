//go:build windows

package main

import (
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestStopPreviousBackgroundTreatsReusedPIDAsStale(t *testing.T) {
	// Keep a harmless, unrelated process alive long enough to simulate Windows
	// reusing a PID that is still recorded in mcpx-daemon.json.
	child := exec.Command("ping", "-n", "30", "127.0.0.1")
	if err := child.Start(); err != nil {
		t.Fatalf("start unrelated process: %v", err)
	}
	defer func() {
		_ = child.Process.Kill()
		_, _ = child.Process.Wait()
	}()

	statePath := filepath.Join(t.TempDir(), daemonStateFilename)
	if err := writeDaemonState(statePath, daemonState{
		PID:        child.Process.Pid,
		Executable: filepath.Join(t.TempDir(), "mcpx.exe"),
	}); err != nil {
		t.Fatal(err)
	}

	stopped, err := stopPreviousBackground(statePath)
	if err != nil {
		t.Fatalf("stale reused PID should self-heal, got: %v", err)
	}
	if len(stopped) != 0 {
		t.Fatalf("unrelated process must not be reported as stopped: %v", stopped)
	}
	if _, err := readDaemonState(statePath); err == nil {
		t.Fatal("stale daemon state should be removed")
	}

	alive, _, err := windowsBackgroundProcessState(child.Process.Pid, "ping.exe")
	if err != nil {
		t.Fatalf("inspect unrelated process: %v", err)
	}
	if !alive {
		t.Fatal("unrelated process was unexpectedly terminated")
	}
}

func TestTerminateBackgroundProcessStopsWindowsProcess(t *testing.T) {
	ping, err := exec.LookPath("ping")
	if err != nil {
		t.Skipf("ping not available: %v", err)
	}
	child := exec.Command(ping, "-n", "30", "127.0.0.1")
	if err := child.Start(); err != nil {
		t.Fatalf("start test process: %v", err)
	}
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_, _ = child.Process.Wait()
	})

	stopped, err := terminateBackgroundProcess(child.Process.Pid, ping, 4*time.Second)
	if err != nil {
		t.Fatalf("terminate background process: %v", err)
	}
	if !stopped {
		t.Fatal("expected tracked process to be stopped")
	}
	alive, _, err := windowsBackgroundProcessState(child.Process.Pid, ping)
	if err != nil {
		t.Fatalf("inspect stopped process: %v", err)
	}
	if alive {
		t.Fatal("process still alive after terminateBackgroundProcess")
	}
}
