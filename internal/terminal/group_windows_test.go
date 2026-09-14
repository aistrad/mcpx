//go:build windows

package terminal

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestReviewManagedStartChild(t *testing.T) {
	for i, arg := range os.Args {
		if arg == "--managed-start-child" {
			if err := os.WriteFile(os.Args[i+1], []byte("ran"), 0600); err != nil {
				os.Exit(2)
			}
			os.Exit(0)
		}
	}
}

func TestReviewManagedStartFailureCleansSuspendedProcess(t *testing.T) {
	for _, stage := range []string{"open", "assign", "resume"} {
		t.Run(stage, func(t *testing.T) {
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(t.TempDir(), "must-not-run")
			cmd := exec.Command(executable, "-test.run=^TestReviewManagedStartChild$", "--", "--managed-start-child", marker)
			configureProcess(cmd)
			api := managedProcessAPIs{windows.OpenProcess, windows.AssignProcessToJobObject, resumePrimaryThread}
			injected := errors.New("fixture Windows API failure")
			switch stage {
			case "open":
				api.open = func(uint32, bool, uint32) (windows.Handle, error) { return 0, injected }
			case "assign":
				api.assign = func(windows.Handle, windows.Handle) error { return injected }
			case "resume":
				api.resume = func(uint32) error { return injected }
			}
			group, err := startManagedProcessWithAPIs(cmd, api)
			if !errors.Is(err, injected) || group != nil {
				t.Fatalf("错误未传播: %v", err)
			}
			if cmd.Process == nil || cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
				t.Fatal("暂停进程未退出并回收")
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatal("失败启动执行了用户代码")
			}
		})
	}
}
