//go:build windows

package terminal

import (
	"fmt"
	"os/exec"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

type processGroup struct {
	mu  sync.Mutex
	job windows.Handle
}

func startManagedProcess(cmd *exec.Cmd) (*processGroup, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		windows.CloseHandle(job)
		return nil, err
	}
	// 暂停主线程，在任何用户代码运行前加入 Job，避免启动与取消之间漏掉后代。
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
	if err = cmd.Start(); err != nil {
		windows.CloseHandle(job)
		return nil, err
	}
	failed := func(cause error) (*processGroup, error) {
		_ = cmd.Process.Kill()
		_ = windows.TerminateJobObject(job, 1)
		_ = cmd.Wait()
		windows.CloseHandle(job)
		return nil, cause
	}
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		return failed(err)
	}
	err = windows.AssignProcessToJobObject(job, process)
	windows.CloseHandle(process)
	if err != nil {
		return failed(err)
	}
	if err = resumePrimaryThread(uint32(cmd.Process.Pid)); err != nil {
		return failed(err)
	}
	return &processGroup{job: job}, nil
}

func resumePrimaryThread(pid uint32) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	for err = windows.Thread32First(snapshot, &entry); err == nil; err = windows.Thread32Next(snapshot, &entry) {
		if entry.OwnerProcessID != pid {
			continue
		}
		thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
		if err != nil {
			return err
		}
		_, err = windows.ResumeThread(thread)
		windows.CloseHandle(thread)
		return err
	}
	return fmt.Errorf("找不到受控进程的暂停主线程: %w", err)
}

func (g *processGroup) kill() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.job == 0 {
		return nil
	}
	return windows.TerminateJobObject(g.job, 1)
}

func (g *processGroup) wait() error {
	for {
		g.mu.Lock()
		// JOBOBJECT_BASIC_ACCOUNTING_INFORMATION，布局来自 Windows SDK。
		var accounting struct {
			TotalUserTime, TotalKernelTime, ThisPeriodTotalUserTime, ThisPeriodTotalKernelTime int64
			TotalPageFaultCount, TotalProcesses, ActiveProcesses, TotalTerminatedProcesses     uint32
		}
		err := windows.QueryInformationJobObject(g.job, windows.JobObjectBasicAccountingInformation, uintptr(unsafe.Pointer(&accounting)), uint32(unsafe.Sizeof(accounting)), nil)
		if err == nil && accounting.ActiveProcesses == 0 {
			windows.CloseHandle(g.job)
			g.job = 0
			g.mu.Unlock()
			return nil
		}
		if err != nil {
			// 关闭最后一个 Job 句柄触发尽力终止；错误仍向上传播，不能证明收敛。
			windows.CloseHandle(g.job)
			g.job = 0
		}
		g.mu.Unlock()
		if err != nil {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
}
