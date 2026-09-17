package workspace

import (
	"path/filepath"
	"testing"

	"mcpx/internal/config"
)

func TestRegistryListGet(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "proj")
	reg, err := NewRegistry([]config.WorkspaceEntry{
		{Name: "proj", Path: p, Description: "d", ApprovalMode: config.WorkspaceApprovalModeExternalManualAutoContinue},
	})
	if err != nil {
		t.Fatal(err)
	}
	list := reg.List()
	if len(list) != 1 || list[0].Name != "proj" || list[0].Description != "d" || list[0].ApprovalMode != config.WorkspaceApprovalModeExternalManualAutoContinue {
		t.Fatalf("%+v", list)
	}
	ws, ok := reg.Get("proj")
	if !ok || ws.Path == "" {
		t.Fatal("get")
	}
	info, err := reg.Info("proj", "override")
	if err != nil || info["description"] != "override" {
		t.Fatalf("%v %+v", err, info)
	}
}

func TestRegistryRejectsUnknownApprovalMode(t *testing.T) {
	_, err := NewRegistry([]config.WorkspaceEntry{{Name: "proj", Path: t.TempDir(), ApprovalMode: "unknown"}})
	if err == nil {
		t.Fatal("unknown workspace approval mode must be rejected")
	}
}
