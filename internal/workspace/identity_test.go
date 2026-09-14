package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func identityGit(t *testing.T, path string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", path}, args...)...)
	if err := cmd.Run(); err != nil {
		t.Fatalf("fixture git failed: %v", err)
	}
}

func identityRepo(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	identityGit(t, path, "init", "-b", "main")
	identityGit(t, path, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-m", "初始化")
	identityGit(t, path, "remote", "add", "origin", "https://example.invalid/owner/project.git")
}

func TestGitIdentityIgnoresOuterChangesAndRejectsTargetDrift(t *testing.T) {
	outer := t.TempDir()
	identityRepo(t, outer)
	target := filepath.Join(outer, "中文 clone")
	identityRepo(t, target)
	expected, err := CaptureGitIdentity(context.Background(), target, "origin", "run-823")
	if err != nil {
		t.Fatal(err)
	}
	identityGit(t, outer, "checkout", "-b", "other-task")
	if err := os.WriteFile(filepath.Join(outer, "output.txt"), []byte("other task"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyGitIdentity(context.Background(), expected); err != nil {
		t.Fatalf("外层变化误阻断: %v", err)
	}
	identityGit(t, target, "remote", "set-url", "origin", "https://example.invalid/owner/other.git")
	if err := VerifyGitIdentity(context.Background(), expected); err == nil {
		t.Fatal("未拒绝远端漂移")
	}
	identityGit(t, target, "remote", "set-url", "origin", "https://example.invalid/owner/project.git")
	identityGit(t, target, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-m", "其他任务提交")
	if err := VerifyGitIdentity(context.Background(), expected); err == nil {
		t.Fatal("未拒绝 HEAD 漂移")
	}
}

func TestGitIdentityRejectsInheritedRootAndMissingRun(t *testing.T) {
	root := t.TempDir()
	identityRepo(t, root)
	child := filepath.Join(root, "uncloned")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := CaptureGitIdentity(context.Background(), child, "origin", "run"); err == nil {
		t.Fatal("把外层仓当作目标仓")
	}
	if _, err := CaptureGitIdentity(context.Background(), root, "origin", ""); err == nil {
		t.Fatal("接受空 run_id")
	}
}

func TestGitIdentityRejectsEnvironmentRedirection(t *testing.T) {
	root := t.TempDir()
	identityRepo(t, root)
	t.Setenv("GIT_DIR", filepath.Join(root, ".git"))
	if _, err := CaptureGitIdentity(context.Background(), root, "origin", "run"); err == nil {
		t.Fatal("接受可能重定向查询和执行的 Git 环境")
	}
}
