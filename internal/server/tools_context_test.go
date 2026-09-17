package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadPathsAreHardScope(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	workspace, ok := rt.reg.Get("demo")
	if !ok {
		t.Fatal("demo workspace was not registered")
	}
	if err := os.MkdirAll(filepath.Join(workspace.Path, "mcpx-stress", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{
		"mcpx-stress/alpha.txt":       "inside-token\n",
		"mcpx-stress/nested/beta.txt": "inside-token\n",
		"index.html":                  "outside-token\n",
	} {
		if err := os.WriteFile(filepath.Join(workspace.Path, filepath.FromSlash(path)), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "demo"})
	remoteID, _ := opened["remote_session_id"].(string)
	if remoteID == "" {
		t.Fatalf("session open did not return remote_session_id: %+v", opened)
	}

	response := callEnvelope(t, rt.toolRead, context.Background(), map[string]any{
		"remote_session_id": remoteID,
		"view":              "context",
		"query":             "inside-token",
		"paths":             []any{"mcpx-stress"},
		"max_results":       20,
	})
	if !statusOK(response) {
		t.Fatalf("context query failed: %+v", response)
	}
	data, _ := response["data"].(map[string]any)
	files, _ := data["files"].([]any)
	if len(files) != 2 {
		t.Fatalf("context scope returned %d files, want 2: %+v", len(files), data)
	}
	for _, raw := range files {
		file, _ := raw.(map[string]any)
		path, _ := file["path"].(string)
		if path != "mcpx-stress" && !strings.HasPrefix(path, "mcpx-stress/") {
			t.Fatalf("context scope leaked path %q: %+v", path, data)
		}
	}

	searchOutside := callEnvelope(t, rt.toolRead, context.Background(), map[string]any{
		"remote_session_id": remoteID,
		"view":              "search",
		"query":             "outside-token",
		"paths":             []any{"mcpx-stress"},
	})
	if !statusOK(searchOutside) {
		t.Fatalf("scoped search failed: %+v", searchOutside)
	}
	outsideData, _ := searchOutside["data"].(map[string]any)
	outsideMatches, _ := outsideData["matches"].([]any)
	if len(outsideMatches) != 0 {
		t.Fatalf("search scope leaked outside matches: %+v", outsideData)
	}

	searchInside := callEnvelope(t, rt.toolRead, context.Background(), map[string]any{
		"remote_session_id": remoteID,
		"view":              "search",
		"query":             "inside-token",
		"paths":             []any{"mcpx-stress"},
	})
	if !statusOK(searchInside) {
		t.Fatalf("in-scope search failed: %+v", searchInside)
	}
	insideData, _ := searchInside["data"].(map[string]any)
	insideMatches, _ := insideData["matches"].([]any)
	if len(insideMatches) != 2 {
		t.Fatalf("in-scope search returned %d matches, want 2: %+v", len(insideMatches), insideData)
	}
	for _, raw := range insideMatches {
		match, _ := raw.(map[string]any)
		path, _ := match["path"].(string)
		if path != "mcpx-stress" && !strings.HasPrefix(path, "mcpx-stress/") {
			t.Fatalf("search returned outside path %q: %+v", path, insideData)
		}
	}
}
