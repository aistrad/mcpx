package server

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/mcpresult"
	"mcpx/internal/remotesession"
)

func TestRegisteredSchemaRejectsUnknownFieldBeforeRuntimeHandler(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	protocol := mcp.NewServer(&mcp.Implementation{Name: "schema-test", Version: "1"}, nil)
	rt.registerTools(protocol)
	result, err := rt.toolHandlers["runtime_read"](context.Background(), mcpresult.Request(map[string]any{
		"view": "capabilities", "include_debug": true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	response := decodeToolResult(t, result)
	if response["status"] != "failed" || !strings.Contains(strings.ToLower(errorMessage(response)), "unknown field") {
		t.Fatalf("unknown runtime_read field was not rejected: %+v", response)
	}
}

func TestRegisteredSchemaRejectsEditActionAliasBeforeMutation(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	protocol := mcp.NewServer(&mcp.Implementation{Name: "schema-test", Version: "1"}, nil)
	rt.registerTools(protocol)
	principal, err := rt.principalFromContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ws, _ := rt.reg.Get("demo")
	created, err := rt.remote.Create(context.Background(), principal, remotesession.CreateInput{WorkspaceName: "demo", WorkspacePath: ws.Path})
	if err != nil {
		t.Fatal(err)
	}
	args := map[string]any{
		"remote_session_id": created.Session.ID,
		"purpose":           "validate edit field contract",
		"edits":             []any{map[string]any{"path": "new.txt", "action": "create", "content": "x"}},
	}
	if schemaErr := rt.validateRegisteredToolArguments("edit", args); schemaErr == nil {
		t.Fatal("schema validator accepted edit action alias")
	}
	result, err := rt.toolHandlers["edit"](context.Background(), mcpresult.Request(args))
	if err != nil {
		t.Fatal(err)
	}
	response := decodeToolResult(t, result)
	if response["status"] != "failed" || !strings.Contains(strings.ToLower(errorMessage(response)), "unknown field") {
		t.Fatalf("edit action alias was not rejected: %+v", response)
	}
}

func TestRegisteredSchemaRejectsMalformedArrayBeforeRuntimeHandler(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	protocol := mcp.NewServer(&mcp.Implementation{Name: "schema-test", Version: "1"}, nil)
	rt.registerTools(protocol)
	args := map[string]any{
		"remote_session_id": "rs_schema",
		"purpose":           "validate malformed edit shape",
		"edits":             "not-an-array",
	}
	if schemaErr := rt.validateRegisteredToolArguments("edit", args); schemaErr == nil || !strings.Contains(schemaErr.Error(), "must be an array") {
		t.Fatalf("schema validator accepted malformed array: %v", schemaErr)
	}
}

func TestRegisteredSchemaRejectsFieldsFromAnotherOperationBranch(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	protocol := mcp.NewServer(&mcp.Implementation{Name: "schema-test", Version: "1"}, nil)
	rt.registerTools(protocol)
	args := map[string]any{
		"remote_session_id":  "rs_schema",
		"action":             "status",
		"operation_ids":      []any{"op_1"},
		"confirmation_token": "must-not-be-used-in-batch",
	}
	if schemaErr := rt.validateRegisteredToolArguments("operation_manage", args); schemaErr == nil || !strings.Contains(schemaErr.Error(), "unknown field") {
		t.Fatalf("schema validator accepted field from another oneOf branch: %v", schemaErr)
	}
}

func errorMessage(response map[string]any) string {
	if body, ok := response["error"].(map[string]any); ok {
		if message, ok := body["message"].(string); ok {
			return message
		}
	}
	return ""
}
