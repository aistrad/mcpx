package server

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"

	"mcpx/internal/workspace"
)

func transitionFixture(t *testing.T) (*Runtime, string, workspace.GitIdentity, string) {
	t.Helper()
	rt := newWorkspaceRuntime(t, "target")
	session := operationTestSession(t, rt, "target")
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", session.WorkspacePath}, args...)...).Output()
		if err != nil {
			t.Fatalf("fixture Git failed: %v", err)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-b", "main")
	git("-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-m", "初始")
	bare := t.TempDir()
	if err := exec.Command("git", "init", "--bare", bare).Run(); err != nil {
		t.Fatal(err)
	}
	git("remote", "add", "origin", bare)
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "remote_session_id": session.ID, "run_id": "transition-run"})
	if !statusOK(opened) {
		t.Fatal("无法冻结 fixture")
	}
	encoded, _ := json.Marshal(opened["data"].(map[string]any)["git_identity"])
	var before workspace.GitIdentity
	if err := json.Unmarshal(encoded, &before); err != nil {
		t.Fatal(err)
	}
	newHEAD := git("-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit-tree", before.Tree, "-p", before.HEAD, "-m", "可归因候选提交")
	return rt, session.ID, before, newHEAD
}

func TestWorkspaceTransitionAdvancesOnlyExecutedExactCandidate(t *testing.T) {
	rt, sessionID, before, newHEAD := transitionFixture(t)
	payload := map[string]any{"action": "run", "remote_session_id": sessionID, "purpose": "条件更新已批准候选 ref",
		"argv": []any{"git", "update-ref", before.Ref, newHEAD, before.HEAD}, "shell": false, "expected_workspace": before,
		"workspace_transition": map[string]any{"operation_id": "commit-ref", "head": newHEAD, "tree": before.Tree}, "idempotency_key": "transition-once", "yield_time_ms": 3000}
	result := callEnvelope(t, rt.toolExecute, context.Background(), payload)
	if result["status"] == "waiting_confirmation" {
		payload["user_confirmed"] = true
		result = callEnvelope(t, rt.toolExecute, context.Background(), payload)
	}
	if !statusOK(result) {
		t.Fatalf("候选执行失败: %s", errorCode(result))
	}
	data := result["data"].(map[string]any)
	if taskID, ok := data["execution_task_id"].(string); ok {
		task, err := rt.tasks.Get(sessionID, taskID)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if !task.Wait(ctx) {
			t.Fatal("条件更新任务未完成")
		}
	}
	resumed := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "remote_session_id": sessionID, "run_id": before.RunID})
	if !statusOK(resumed) {
		t.Fatalf("成功候选不能继续: %s", errorCode(resumed))
	}
	after := before
	after.HEAD = newHEAD
	if err := workspace.VerifyFrozenGitIdentity(context.Background(), rt.state.DB(), sessionID, after); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := rt.state.DB().QueryRow(`SELECT version FROM workspace_run_identities WHERE run_id=?`, before.RunID).Scan(&version); err != nil || version != 2 {
		t.Fatalf("身份未唯一推进: version=%d err=%v", version, err)
	}
	next := map[string]any{"action": "run", "remote_session_id": sessionID, "purpose": "继续使用新候选", "argv": []any{"git", "branch", "after-candidate"}, "shell": false, "expected_workspace": after}
	if result := callEnvelope(t, rt.toolExecute, context.Background(), next); !statusOK(result) {
		t.Fatalf("合法后置身份被误拒绝: %s", errorCode(result))
	}
	push := map[string]any{"action": "run", "remote_session_id": sessionID, "purpose": "向隔离本地 bare fixture 推送精确候选", "argv": []any{"git", "push", "origin", "HEAD:refs/heads/candidate"}, "shell": false, "expected_workspace": after, "yield_time_ms": 3000}
	pushed := callEnvelope(t, rt.toolExecute, context.Background(), push)
	if pushed["status"] == "waiting_confirmation" {
		push["user_confirmed"] = true
		pushed = callEnvelope(t, rt.toolExecute, context.Background(), push)
	}
	if !statusOK(pushed) {
		t.Fatalf("合法提交后 push 被误拒绝: %s", errorCode(pushed))
	}
	remoteOID, err := exec.Command("git", "-C", before.CanonicalPath, "ls-remote", "origin", "refs/heads/candidate").Output()
	if err != nil || !strings.HasPrefix(string(remoteOID), newHEAD+"\t") {
		t.Fatalf("本地 bare fixture 未收到精确候选: %v", err)
	}
	next["expected_workspace"] = before
	if statusOK(callEnvelope(t, rt.toolExecute, context.Background(), next)) {
		t.Fatal("推进后旧身份仍能执行")
	}
}

func TestWorkspaceTransitionMissingReceiptCannotAdoptObservedHEAD(t *testing.T) {
	rt, sessionID, before, newHEAD := transitionFixture(t)
	planned := workspaceTransition{OperationID: "unproven", HEAD: newHEAD, Tree: before.Tree}
	if err := rt.prepareWorkspaceTransition(context.Background(), sessionID, "planned-command", before, planned); err != nil {
		t.Fatal(err)
	}
	// 注入只有同样后置值、没有原 Task 执行回执的场景。
	if err := exec.Command("git", "-C", before.CanonicalPath, "update-ref", before.Ref, newHEAD, before.HEAD).Run(); err != nil {
		t.Fatal(err)
	}
	if err := rt.reconcileWorkspaceTransition(context.Background(), sessionID, before.RunID); err == nil {
		t.Fatal("没有执行回执却吸收新 HEAD")
	}
	if err := workspace.VerifyFrozenGitIdentity(context.Background(), rt.state.DB(), sessionID, before); err != nil {
		t.Fatal("未知场景改变了冻结身份")
	}
}
