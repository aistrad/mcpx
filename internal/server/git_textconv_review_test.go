package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"mcpx/internal/winproc"
)

func TestReviewDefaultGitTextconvRequiresConfirmation(t *testing.T) {
	rt := newWorkspaceRuntime(t, "review")
	rt.cfg.Security.Commands.Default = "confirm"
	rt.cfg.Security.Commands.Allow = nil
	s := operationTestSession(t, rt, "review")
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = s.WorkspacePath
		winproc.ConfigureNoWindow(cmd)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("隔离 Git fixture: %v %s", err, output)
		}
	}
	git("init", "--quiet")
	git("config", "diff.review.textconv", "sh driver.sh")
	for name, body := range map[string]string{
		".gitattributes": "sample.txt diff=review\n",
		"driver.sh":      "printf 'driver-ran' > driver-marker\ncat \"$1\"\n",
		"sample.txt":     "before\n",
	} {
		if err := os.WriteFile(filepath.Join(s.WorkspacePath, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	git("add", "sample.txt")
	if err := os.WriteFile(filepath.Join(s.WorkspacePath, "sample.txt"), []byte("after\n"), 0600); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"remote_session_id": s.ID, "action": "run", "purpose": "默认转换器副作用回归", "argv": []any{"git", "diff"}, "shell": false, "idempotency_key": "default-driver"}
	first := callOperationTool(t, rt, "execute", args)
	if first["status"] != "waiting_confirmation" {
		t.Fatalf("默认 textconv 绕过确认: %+v", first)
	}
	read := callOperationTool(t, rt, "execute", map[string]any{"remote_session_id": s.ID, "action": "run", "purpose": "显式禁用外部转换的读取", "argv": []any{"git", "diff", "--no-ext-diff", "--no-textconv"}, "shell": false})
	if read["status"] != "succeeded" {
		t.Fatalf("安全读取失败: %+v", read)
	}
	if _, err := os.Stat(filepath.Join(s.WorkspacePath, "driver-marker")); !os.IsNotExist(err) {
		t.Fatal("确认前执行了转换器")
	}
	args["user_confirmed"] = true
	confirmed := callOperationTool(t, rt, "execute", args)
	if confirmed["status"] != "succeeded" {
		t.Fatalf("确认后执行失败: %+v", confirmed)
	}
	marker, err := os.ReadFile(filepath.Join(s.WorkspacePath, "driver-marker"))
	if err != nil || string(marker) != "driver-ran" {
		t.Fatalf("fixture 未实际触发默认转换器: %v", err)
	}
}
