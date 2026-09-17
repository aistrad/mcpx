package server

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"mcpx/internal/config"
)

func initGitRoot(t *testing.T, root string) {
	t.Helper()
	for _, args := range [][]string{{"init"}, {"config", "user.email", "test@example.invalid"}, {"config", "user.name", "MCPX Test"}} {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
}

func TestResolveProjectRootRejectsImplicitParentGitRoot(t *testing.T) {
	root := t.TempDir()
	initGitRoot(t, root)
	if _, err := resolveProjectRoot(context.Background(), root, ""); err == nil || !strings.Contains(err.Error(), "project root is required") {
		t.Fatalf("implicit Git root was accepted: %v", err)
	}
}

func TestResolveProjectRootHonorsNavigationOnlyWorkspace(t *testing.T) {
	root := t.TempDir()
	initGitRoot(t, root)
	if _, err := resolveProjectRoot(context.Background(), root, ".", true); err == nil {
		t.Fatal("navigation-only Workspace accepted itself as project root")
	}
}

func TestResolveProjectRootAcceptsExplicitNestedWorktree(t *testing.T) {
	root := t.TempDir()
	initGitRoot(t, root)
	nested := filepath.Join(root, "AIS-worktrees", "topic")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	initGitRoot(t, nested)
	resolved, err := resolveProjectRoot(context.Background(), root, "AIS-worktrees/topic")
	if err != nil {
		t.Fatal(err)
	}
	if resolved != nested {
		t.Fatalf("resolved=%q want %q", resolved, nested)
	}
}

func TestResolveProjectRootRejectsParentTraversalAndInheritedRoot(t *testing.T) {
	root := t.TempDir()
	initGitRoot(t, root)
	nested := filepath.Join(root, "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveProjectRoot(context.Background(), root, "../outside"); err == nil {
		t.Fatal("parent traversal was accepted")
	}
	if _, err := resolveProjectRoot(context.Background(), root, "nested"); err == nil {
		t.Fatal("inherited parent Git root was accepted")
	}
}

func TestSessionOpenBindsProjectRootAndUsesItForReads(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MCPX_HOME", home)
	root := filepath.Join(home, "work")
	nested := filepath.Join(root, "AIS-worktrees", "topic")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	initGitRoot(t, root)
	initGitRoot(t, nested)
	for path, content := range map[string]string{
		filepath.Join(root, "parent.txt"):    "parent\n",
		filepath.Join(nested, "project.txt"): "project\n",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, repo := range []string{nested, root} {
		cmd := exec.Command("git", "-C", repo, "add", ".")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git add: %v: %s", err, output)
		}
		cmd = exec.Command("git", "-C", repo, "commit", "-m", "base")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git commit: %v: %s", err, output)
		}
	}
	cfg := config.DefaultConfig()
	cfg.Workspaces = []config.WorkspaceEntry{{Name: "work", Path: root}}
	if err := config.WriteGlobal(filepath.Join(home, "config.yaml"), cfg); err != nil {
		t.Fatal(err)
	}
	rt, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	missing := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"workspace": "work"})
	if missing["status"] != "failed" {
		t.Fatalf("missing project_root was accepted: %+v", missing)
	}
	missingErr, _ := missing["error"].(map[string]any)
	if !strings.Contains(fmt.Sprint(missingErr["message"]), "project root is required") {
		t.Fatalf("missing project_root error=%+v", missing)
	}
	missingDetails, _ := missingErr["details"].(map[string]any)
	candidates, _ := missingDetails["candidates"].([]any)
	foundCandidate := false
	for _, candidate := range candidates {
		if candidate == "AIS-worktrees" {
			foundCandidate = true
			break
		}
	}
	if !foundCandidate {
		t.Fatalf("missing project_root did not return bounded candidates: %+v", missingDetails)
	}
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"workspace": "work", "project_root": "AIS-worktrees/topic"})
	if opened["status"] != "ok" {
		t.Fatalf("explicit project_root failed: %+v", opened)
	}
	id, _ := opened["remote_session_id"].(string)
	data, _ := opened["data"].(map[string]any)
	workspace, _ := data["workspace"].(map[string]any)
	if workspace["project_root"] != nested || workspace["registered_root"] != root {
		t.Fatalf("binding=%+v", workspace)
	}
	mismatch := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{
		"action": "open", "remote_session_id": id, "project_root": "AIS-worktrees/topic", "git_identity_path": ".",
	})
	if mismatch["status"] != "failed" || errorCode(mismatch) != "project_root_mismatch" {
		t.Fatalf("conflicting project selectors were accepted: %+v", mismatch)
	}
	read := callEnvelope(t, rt.toolRead, context.Background(), map[string]any{"remote_session_id": id, "view": "file", "path": "project.txt"})
	if read["status"] != "ok" {
		t.Fatalf("project read failed: %+v", read)
	}
	leaked := callEnvelope(t, rt.toolRead, context.Background(), map[string]any{"remote_session_id": id, "view": "file", "path": "../parent.txt"})
	if leaked["status"] == "ok" {
		t.Fatalf("project root escaped to parent: %+v", leaked)
	}
}
