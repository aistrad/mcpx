//go:build !windows

package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Optional real-owner wire check. The caller supplies an approved native Skill
// directory; there are no organization paths or live credentials in this test.
// describe and invalid-run observe do not retrieve data or mutate owner resources.
func TestNativeRealOwnerContract(t *testing.T) {
	directory := os.Getenv("MCPX_NATIVE_INTEGRATION_SKILL_DIR")
	if directory == "" {
		t.Skip("set MCPX_NATIVE_INTEGRATION_SKILL_DIR for a real native-owner wire check")
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
		t.Fatal(err)
	}
	rt := newWorkspaceRuntime(t, "integration")
	rt.cfg.Discovery.Skills.Enabled = true
	rt.cfg.Discovery.Skills.Dirs = nil
	rt.cfg.Discovery.Skills.NativeDirs = []string{absolute}
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "integration"})
	id := opened["remote_session_id"].(string)
	listed := callEnvelope(t, rt.toolSkillTool, context.Background(), map[string]any{"action": "list", "remote_session_id": id})
	skills := asMapSlice(listed["data"].(map[string]any)["skills"])
	if len(skills) != 1 {
		t.Fatalf("native integration discovery=%+v", listed)
	}
	name := skills[0]["name"].(string)
	described := callEnvelope(t, rt.toolSkillTool, context.Background(), map[string]any{"action": "describe", "remote_session_id": id, "name": name})
	if !statusOK(described) {
		t.Fatalf("real owner descriptor failed: %+v", described)
	}
	contract := described["data"].(map[string]any)
	if contract["external_steps"] != true || contract["arguments_schema"] == nil {
		t.Fatalf("real owner contract absent: %+v", contract)
	}
	// Reject an out-of-contract request before any native call/data operation.
	invalid := callEnvelope(t, rt.toolSkillTool, context.Background(), map[string]any{"action": "call", "remote_session_id": id, "name": name, "purpose": "invalid contract test only", "arguments": map[string]any{"operation": "__not_an_operation__"}})
	if statusOK(invalid) || errorCode(invalid) != "skill_argument_invalid" {
		t.Fatalf("real owner Schema not enforced: %+v", invalid)
	}
	t.Logf("real owner %s discovered and validated through existing skill_tool; tool count=%d", name, len(rt.toolHandlers))
}
