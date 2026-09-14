package server

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"mcpx/internal/operation"
)

func TestOperationViewerCannotMutateWaitingOperation(t *testing.T) {
	for _, action := range []string{"cancel", "resume"} {
		t.Run(action, func(t *testing.T) {
			rt := newWorkspaceRuntime(t, "permission-test")
			session := operationTestSession(t, rt, "permission-test")
			record, err := rt.operations.Submit(context.Background(), operation.SubmitSpec{
				RemoteSessionID: session.ID, WorkspaceName: session.WorkspaceName, RequestID: "permission-test", Purpose: "权限回归",
				Steps: []operation.StepSpec{{ID: "step", Tool: "execute"}},
			}, func(context.Context, operation.ExecuteInput) operation.ExecuteResult {
				return operation.ExecuteResult{WaitingConfirmation: true, ConfirmationToken: "fixture-confirmation"}
			})
			if err != nil {
				t.Fatal(err)
			}
			before, timedOut, err := rt.operations.Wait(context.Background(), record.ID, 3*time.Second)
			if err != nil || timedOut || before.State != operation.StateWaitingConfirmation {
				t.Fatalf("等待 fixture 失败: %v", err)
			}
			if _, err := rt.state.DB().Exec(`UPDATE remote_session_members SET role='viewer' WHERE remote_session_id=?`, session.ID); err != nil {
				t.Fatal(err)
			}
			status := callOperationTool(t, rt, "operation_manage", map[string]any{"remote_session_id": session.ID, "operation_id": record.ID, "action": "status"})
			if status["status"] == "failed" {
				t.Fatal("Viewer 读取被拒绝")
			}
			result := callOperationTool(t, rt, "operation_manage", map[string]any{"remote_session_id": session.ID, "operation_id": record.ID, "action": action, "step_id": "step", "confirmation_token": "fixture-confirmation"})
			if result["status"] != "failed" || !strings.Contains(operationErrorMessage(result), "owner or editor") {
				t.Fatalf("Viewer 未被写权限拒绝: %+v", result)
			}
			after, err := rt.operations.Get(context.Background(), record.ID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("拒绝变更改变持久快照")
			}
		})
	}
}

func TestReviewOperationRoleAndSessionMatrix(t *testing.T) {
	rt := newWorkspaceRuntime(t, "permission-test")
	for _, role := range []string{"owner", "editor"} {
		for _, action := range []string{"cancel", "resume"} {
			session := operationTestSession(t, rt, "permission-test")
			other := operationTestSession(t, rt, "permission-test")
			record, err := rt.operations.Submit(context.Background(), operation.SubmitSpec{RemoteSessionID: session.ID, WorkspaceName: session.WorkspaceName, Steps: []operation.StepSpec{{ID: "step", Tool: "read", Arguments: map[string]any{"view": "list"}}}}, func(context.Context, operation.ExecuteInput) operation.ExecuteResult {
				return operation.ExecuteResult{WaitingConfirmation: true, ConfirmationToken: "fixture"}
			})
			if err != nil {
				t.Fatal(err)
			}
			before, _, err := rt.operations.Wait(context.Background(), record.ID, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := rt.state.DB().Exec(`UPDATE remote_session_members SET role=? WHERE remote_session_id=?`, role, session.ID); err != nil {
				t.Fatal(err)
			}
			args := map[string]any{"remote_session_id": other.ID, "operation_id": record.ID, "action": action, "step_id": "step", "confirmation_token": "fixture"}
			denied := callOperationTool(t, rt, "operation_manage", args)
			if denied["status"] != "failed" || !strings.Contains(operationErrorMessage(denied), "another Remote Session") {
				t.Fatalf("跨 Session 未拒绝: %+v", denied)
			}
			after, _ := rt.operations.Get(context.Background(), record.ID)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("跨 Session 拒绝改变快照")
			}
			args["remote_session_id"] = session.ID
			allowed := callOperationTool(t, rt, "operation_manage", args)
			if allowed["status"] == "failed" {
				t.Fatalf("%s/%s 被拒绝: %+v", role, action, allowed)
			}
		}
	}
}
