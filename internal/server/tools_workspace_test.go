package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"mcpx/internal/auth"
	"mcpx/internal/config"
)

func TestWorkspaceListDoesNotRequireRemoteSession(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MCPX_HOME", home)

	alpha := filepath.Join(home, "alpha")
	beta := filepath.Join(home, "beta")
	for _, path := range []string{alpha, beta} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cfg := config.DefaultConfig()
	cfg.Auth.Mode = "bearer"
	cfg.Auth.Token = "workspace-token"
	cfg.Logging.Enabled = false
	cfg.Workspaces = []config.WorkspaceEntry{
		{Name: "alpha", Path: alpha, Description: "Alpha workspace", ApprovalMode: config.WorkspaceApprovalModeExternalManualAutoContinue},
		{Name: "beta", Path: beta, Description: "Beta workspace"},
	}
	if err := config.WriteGlobal(filepath.Join(home, "config.yaml"), cfg); err != nil {
		t.Fatal(err)
	}

	runtime, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })

	ctx := auth.ContextWithAuthorization(context.Background(), "Bearer workspace-token")
	response := callEnvelope(t, runtime.toolWorkspace, ctx, map[string]any{})
	if !statusOK(response) {
		t.Fatalf("workspace list failed: %+v", response)
	}
	data, _ := response["data"].(map[string]any)
	items, _ := data["workspaces"].([]any)
	if len(items) != 2 {
		t.Fatalf("workspaces=%+v", data["workspaces"])
	}
	first := items[0].(map[string]any)
	second := items[1].(map[string]any)
	if first["name"] != "alpha" || first["path"] != alpha || first["description"] != "Alpha workspace" {
		t.Fatalf("first workspace=%+v", first)
	}
	if first["approval_mode"] != config.WorkspaceApprovalModeExternalManualAutoContinue {
		t.Fatalf("first workspace approval mode=%+v", first["approval_mode"])
	}
	if second["name"] != "beta" || second["path"] != beta || second["description"] != "Beta workspace" {
		t.Fatalf("second workspace=%+v", second)
	}
}
