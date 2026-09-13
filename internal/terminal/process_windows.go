//go:build windows

package terminal

import (
	"os/exec"
	"time"

	"mcpx/internal/winproc"
)

func configureProcess(cmd *exec.Cmd) {
	// 显式进程与 Shell 进程统一隐藏 Windows 控制台窗口。
	winproc.ConfigureNoWindow(cmd)
}

func processCPUTime(int) (time.Duration, bool) {
	return 0, false
}
