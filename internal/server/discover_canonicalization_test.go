package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/skill"
)

func TestSkillInventoryDoesNotExposeLegacyDiscoveryProtocol(t *testing.T) {
	items := skillItems([]skill.Skill{{Manifest: skill.Manifest{
		Name: "review", Description: "review code", Runtime: "markdown", Format: "skill_md",
	}, Dir: "/tmp/review", Source: "/tmp"}})
	if len(items) != 1 {
		t.Fatalf("skill items=%+v", items)
	}
	encoded, err := json.Marshal(items[0])
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, legacy := range []string{"discover", "skill_call", "discovery_id", "discovery_revision", "invocation_template"} {
		if strings.Contains(text, legacy) {
			t.Fatalf("skill inventory exposes legacy protocol %q: %s", legacy, text)
		}
	}
}

func TestExtensionToolSchemasUseUnifiedActionsWithoutPublicRevisionTokens(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	for _, name := range []string{"skill_tool", "mcp_tool"} {
		tool, ok := rt.toolIndex[name]
		if !ok {
			t.Fatalf("missing %s", name)
		}
		encoded, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		text := string(encoded)
		for _, action := range []string{"list", "describe", "call"} {
			if !strings.Contains(text, `"`+action+`"`) {
				t.Fatalf("%s schema missing action %q: %s", name, action, text)
			}
		}
		for _, legacy := range []string{"discovery_id", "discovery_revision"} {
			if strings.Contains(text, legacy) {
				t.Fatalf("%s schema exposes legacy token %q: %s", name, legacy, text)
			}
		}
	}
	for _, removed := range []string{"discover", "skill_call", "mcp_call"} {
		if _, exists := rt.toolIndex[removed]; exists {
			t.Fatalf("legacy public tool %s must not be registered", removed)
		}
	}
}

func TestMCPExecutionRiskPreservesAnnotationsForClientJudgment(t *testing.T) {
	falseValue := false
	trueValue := true

	closedReadOnly := mcpExecutionRisk(&mcp.Tool{Annotations: &mcp.ToolAnnotations{
		ReadOnlyHint: true, DestructiveHint: &falseValue, OpenWorldHint: &falseValue,
	}})
	if !closedReadOnly.ReadOnly || closedReadOnly.Destructive || closedReadOnly.OpenWorld {
		t.Fatalf("closed read-only MCP risk=%+v", closedReadOnly)
	}

	openReadOnly := mcpExecutionRisk(&mcp.Tool{Annotations: &mcp.ToolAnnotations{
		ReadOnlyHint: true, DestructiveHint: &falseValue, OpenWorldHint: &trueValue,
	}})
	if !openReadOnly.ReadOnly || openReadOnly.Destructive || !openReadOnly.OpenWorld {
		t.Fatalf("open-world annotation was not preserved=%+v", openReadOnly)
	}

	unknown := mcpExecutionRisk(&mcp.Tool{})
	if !unknown.OpenWorld || unknown.Classification != "upstream_mcp_unknown_risk" {
		t.Fatalf("missing MCP annotations must remain visibly unknown=%+v", unknown)
	}
	if _, exists := unknown.publicData()["confirmation_required"]; exists {
		t.Fatalf("risk metadata must not impose a server-side confirmation decision: %+v", unknown.publicData())
	}
}

func TestDiscoveryObservationLivesForTheRemoteSession(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "demo"})
	remoteID, _ := opened["remote_session_id"].(string)
	if remoteID == "" {
		t.Fatalf("open session=%+v", opened)
	}
	rt.upsertDiscoveryLease(discoveryLease{
		RemoteSessionID: remoteID, PrincipalID: "test-principal", WorkspacePath: "workspace",
		Kind: "skill", Object: "review", Revision: "rev-1",
	})
	rt.discoveryMu.Lock()
	observedBeforeClose := len(rt.discoveries)
	rt.discoveryMu.Unlock()
	if observedBeforeClose != 1 {
		t.Fatalf("discovery observation=%d, want 1", observedBeforeClose)
	}

	closed := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "close", "remote_session_id": remoteID})
	if !statusOK(closed) {
		t.Fatalf("close session=%+v", closed)
	}
	rt.discoveryMu.Lock()
	observedAfterClose := len(rt.discoveries)
	rt.discoveryMu.Unlock()
	if observedAfterClose != 0 {
		t.Fatalf("closed session must clear its discovery observations, remaining=%d", observedAfterClose)
	}
}
