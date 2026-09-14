//go:build !windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func configureBackgroundProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func terminateBackgroundProcess(pid int, executable string, timeout time.Duration) (bool, error) {
	if pid <= 0 || pid == os.Getpid() {
		return false, fmt.Errorf("invalid daemon pid %d", pid)
	}
	alive, err := backgroundProcessAlive(pid)
	if err != nil {
		return false, err
	}
	if !alive {
		return false, nil
	}
	matches, err := backgroundProcessMatches(pid, executable)
	if err != nil {
		return false, fmt.Errorf("verify daemon pid %d: %w", pid, err)
	}
	if !matches {
		// 进程还活着，但命令行不是我们的可执行文件，说明这个 PID 已经被系统
		// 复用给了别的程序。状态文件里的记录是陈旧的，调用方据此丢弃即可。
		//
		// 这里绝不能报错：报错会让调用方在删除状态文件之前就早退，陈旧记录
		// 永远留在盘上，后续每一次 `mcpx -d` 都会重复失败。也绝不能对不匹配的
		// 进程发信号——那是别人的进程。
		return false, nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return false, nil
		}
		return false, fmt.Errorf("terminate daemon pid %d: %w", pid, err)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		alive, err := backgroundProcessAlive(pid)
		if err != nil {
			return false, err
		}
		if !alive {
			return true, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	// 升级到 SIGKILL 之前必须再确认一次身份：SIGTERM 之后 daemon 可能已经退出，
	// 而这个 PID 被系统复用给了别的程序，此时 SIGKILL 打的就是无关进程。
	if backgroundProcessGone(pid, executable) {
		return true, nil
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return false, fmt.Errorf("kill daemon pid %d: %w", pid, err)
	}
	return true, nil
}

// backgroundProcessGone 判断"这个 PID 上已经没有我们的 daemon 了"——进程不存在，
// 或者还在但命令行已经不是我们的可执行文件（PID 被系统复用给了别的程序）。
// 后者同样意味着 daemon 已经退出，绝不能再对这个 PID 发信号。
func backgroundProcessGone(pid int, executable string) bool {
	alive, err := backgroundProcessAlive(pid)
	if err != nil || !alive {
		return true
	}
	matches, err := backgroundProcessMatches(pid, executable)
	if err != nil {
		// 查不出身份时保守认为它还是我们的 daemon，交给调用方继续等待，
		// 而不是据此升级到 SIGKILL。
		return false
	}
	return !matches
}

func backgroundProcessAlive(pid int) (bool, error) {
	err := syscall.Kill(pid, 0)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.ESRCH):
		return false, nil
	case errors.Is(err, syscall.EPERM):
		return true, nil
	default:
		return false, fmt.Errorf("check daemon pid %d: %w", pid, err)
	}
}

func backgroundProcessMatches(pid int, executable string) (bool, error) {
	output, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return false, err
	}
	return backgroundCommandMatches(strings.TrimSpace(string(output)), executable), nil
}

func backgroundCommandMatches(command, executable string) bool {
	executable = filepath.Clean(strings.TrimSpace(executable))
	command = strings.TrimSpace(command)
	if command == "" || executable == "" {
		return false
	}
	return command == executable || strings.HasPrefix(command, executable+" ")
}
