package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"mcpx/internal/mcpresult"
	"mcpx/internal/remotesession"
)

func TestArtifactRegisterDoesNotAttachResourceLinkByDefault(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	principal, err := rt.principalFromContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ws, _ := rt.reg.Get("demo")
	if err := os.WriteFile(filepath.Join(ws.Path, "report.txt"), []byte("report\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	created, err := rt.remote.Create(context.Background(), principal, remotesession.CreateInput{WorkspaceName: "demo", WorkspacePath: ws.Path})
	if err != nil {
		t.Fatal(err)
	}
	result, err := rt.toolArtifactRegister(context.Background(), mcpresult.Request(map[string]any{
		"remote_session_id": created.Session.ID,
		"purpose":           "register a report without host materialization",
		"path":              "report.txt",
		"kind":              "test_report",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 1 {
		t.Fatalf("default artifact registration attached content: %+v", result.Content)
	}
}
