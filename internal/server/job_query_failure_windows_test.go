//go:build windows && mcpx_fault_injection

package server

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"mcpx/internal/operation"
	"mcpx/internal/terminal"
)

func TestReviewJobQueryFailureSurvivesResultConversion(t *testing.T) {
	for _, order := range []string{"done-first", "cancel-first", "cancel-before-commit"} {
		t.Run(order, func(t *testing.T) {
			rt := newWorkspaceRuntime(t, "job-error")
			s := operationTestSession(t, rt, "job-error")
			helper, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			task, err := rt.tasks.StartRemoteProcessWithObservationContext("", "", "execute", s.ID, s.WorkspaceName, s.WorkspacePath, "fixture", terminal.ProcessSpec{Executable: helper, Args: []string{"-test.run=^TestReviewDelayedChild$", "--", "--review-delayed-child", filepath.Join(s.WorkspacePath, "ready"), filepath.Join(s.WorkspacePath, "late")}})
			if err != nil {
				t.Fatal(err)
			}
			defer task.Kill()
			started, run, converted, commit := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
			defer func() {
				select {
				case <-run:
				default:
					close(run)
				}
				select {
				case <-commit:
				default:
					close(commit)
				}
			}()
			r, err := rt.operations.Submit(context.Background(), operation.SubmitSpec{RemoteSessionID: s.ID, WorkspaceName: s.WorkspaceName, Steps: []operation.StepSpec{{ID: "a", Tool: "execute"}}}, func(ctx context.Context, in operation.ExecuteInput) operation.ExecuteResult {
				close(started)
				<-run
				result, callErr := rt.waitForOperationTask(ctx, in, &mcp.CallToolResult{StructuredContent: map[string]any{"execution_task_id": task.ID}})
				out := operationResult(result, callErr)
				close(converted)
				<-commit
				return out
			})
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("未启动步骤")
			}
			if order == "cancel-first" {
				if _, err := rt.operations.Cancel(context.Background(), r.ID); err != nil {
					t.Fatal(err)
				}
			}
			if err := rt.tasks.InvalidateJobForTest(s.ID, task.ID); err != nil {
				t.Fatal(err)
			}
			bounded, cancel := context.WithTimeout(context.Background(), time.Second)
			if !task.Wait(bounded) {
				cancel()
				t.Fatal("Job 查询错误未完成 Task")
			}
			cancel()
			if task.StatusView()["status"] != terminal.TaskFailed {
				t.Fatal("没有命中真实 Job 查询失败路径")
			}
			close(run)
			select {
			case <-converted:
			case <-time.After(time.Second):
				t.Fatal("结果转换卡住")
			}
			if order == "cancel-before-commit" {
				if _, err := rt.operations.Cancel(context.Background(), r.ID); err != nil {
					t.Fatal(err)
				}
			}
			close(commit)
			final, timeout, err := rt.operations.Wait(context.Background(), r.ID, time.Second)
			if err != nil || timeout || final.State != operation.StateInterrupted || final.Steps[0].State != operation.StateInterrupted {
				t.Fatalf("停止未知被降级: %+v %v", final, err)
			}
			stable, err := rt.operations.Get(context.Background(), r.ID)
			if err != nil || !reflect.DeepEqual(final, stable) {
				t.Fatal("终态快照改变")
			}
		})
	}
}
