package server

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"mcpx/internal/workspace"
)

func TestExecuteWorkspaceIdentityBindsNestedTargetAndRejectsDrift(t *testing.T) {
	rt := newWorkspaceRuntime(t, "outer")
	session := operationTestSession(t, rt, "outer")
	target := filepath.Join(session.WorkspacePath, "target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		if err := exec.Command("git", append([]string{"-C", target}, args...)...).Run(); err != nil {
			t.Fatalf("fixture git: %v", err)
		}
	}
	git("init", "-b", "main")
	git("-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-m", "初始")
	git("remote", "add", "origin", "https://example.invalid/repo.git")
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "remote_session_id": session.ID, "run_id": "issue823-identity", "git_identity_path": "target"})
	if !statusOK(opened) {
		t.Fatal("无法读取身份")
	}
	identity := opened["data"].(map[string]any)["git_identity"]
	if identity == nil {
		t.Fatal("未公开身份快照")
	}
	payload := map[string]any{"action": "run", "remote_session_id": session.ID, "purpose": "在精确目标创建本地分支", "argv": []any{"git", "branch", "candidate"}, "shell": false, "expected_workspace": identity}
	result := callEnvelope(t, rt.toolExecute, context.Background(), payload)
	if !statusOK(result) {
		t.Fatalf("冻结目标无法执行: %s", errorCode(result))
	}
	git("show-ref", "--verify", "refs/heads/candidate")
	if result["data"].(map[string]any)["working_directory"] != target {
		t.Fatal("未使用目标 cwd")
	}
	for _, override := range [][]any{
		{"git", "-C", session.WorkspacePath, "branch", "escaped"},
		{"git", "--git-dir=" + filepath.Join(target, ".git"), "branch", "escaped"},
		{"git", "-c", "alias.escape=!echo unexpected", "escape"},
	} {
		payload["argv"] = override
		if statusOK(callEnvelope(t, rt.toolExecute, context.Background(), payload)) {
			t.Fatal("接受 Git 全局重定向选项")
		}
	}
	git("remote", "set-url", "origin", "https://example.invalid/changed.git")
	reopened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "remote_session_id": session.ID, "run_id": "issue823-identity", "git_identity_path": "target"})
	if statusOK(reopened) {
		t.Fatal("重新打开 Session 静默接受了新远端")
	}
	payload["argv"] = []any{"git", "branch", "must-not-exist"}
	rejected := callEnvelope(t, rt.toolExecute, context.Background(), payload)
	if statusOK(rejected) || errorCode(rejected) != "workspace_identity_mismatch" {
		t.Fatalf("漂移未拒绝: %s", errorCode(rejected))
	}
	if err := exec.Command("git", "-C", target, "show-ref", "--verify", "refs/heads/must-not-exist").Run(); err == nil {
		t.Fatal("拒绝前产生副作用")
	}
	observed, err := workspace.CaptureGitIdentity(context.Background(), target, "origin", "issue823-identity")
	if err != nil {
		t.Fatal(err)
	}
	payload["expected_workspace"] = observed
	if statusOK(callEnvelope(t, rt.toolExecute, context.Background(), payload)) {
		t.Fatal("执行请求自行提交观察到的新值绕过持久冻结")
	}
}
