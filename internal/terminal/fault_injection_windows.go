//go:build windows && mcpx_fault_injection

package terminal

import (
	"fmt"
	"golang.org/x/sys/windows"
)

// InvalidateJobForTest 仅在显式故障测试构建中可用，正式制品不包含此入口。
// 关闭自有 Job 终止 fixture，替换成无效句柄，让真实 Windows 查询返回错误。
func (m *TaskManager) InvalidateJobForTest(sessionID, taskID string) error {
	task, err := m.Get(sessionID, taskID)
	if err != nil {
		return err
	}
	g := task.group
	if g == nil {
		return fmt.Errorf("fixture 无 Job")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.job == 0 {
		return fmt.Errorf("fixture Job 已结束")
	}
	if err := windows.CloseHandle(g.job); err != nil {
		return err
	}
	g.job = windows.InvalidHandle
	return nil
}
