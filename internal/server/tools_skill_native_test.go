//go:build !windows

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcpx/internal/mcpresult"
)

func TestNativeOwnerHelper(t *testing.T) {
	if len(os.Args) == 0 || os.Args[len(os.Args)-1] != "--owner-fixture" {
		return
	}
	var request map[string]any
	if json.NewDecoder(os.Stdin).Decode(&request) != nil {
		os.Exit(3)
	}
	arguments, _ := request["arguments"].(map[string]any)
	switch request["operation"] {
	case "describe":
		fmt.Print(`{"status":"ready","instructions":"Original native owner instructions.","contract_revision":"fixture-v1","entrypoints":[{"name":"profile"}],"arguments_schema":{"type":"object","additionalProperties":false,"required":["operation","value"],"properties":{"operation":{"enum":["prepare","submit"]},"value":{"type":"integer","minimum":1},"mode":{"enum":["ok","error"]}}}}`)
	case "observe":
		if len(arguments) != 1 || arguments["run_id"] == nil {
			os.Exit(4)
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"status": "awaiting_external", "run_id": arguments["run_id"], "read_only": true, "runtime_context": request["runtime_context"]})
	case "call":
		if arguments["mode"] == "error" {
			fmt.Print(`{"status":"error","error":{"code":"NATIVE_TEST_REJECTED","message":"original owner error","retryable":false,"details":{"field":"value"}}}`)
			os.Exit(2)
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"status": "prepared", "value": arguments["value"], "native_operation": arguments["operation"], "runtime_context": request["runtime_context"]})
	default:
		os.Exit(5)
	}
	os.Exit(0)
}

func nativeRuntimeFixture(t *testing.T) (*Runtime, string, string) {
	t.Helper()
	rt := newWorkspaceRuntime(t, "native")
	ws, _ := rt.reg.Get("native")
	directory := filepath.Join(ws.Path, "original-skill")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	manifest := "---\nname: native-owner\ndescription: Test native owner.\nmcpx:\n  protocol: json_stdio_v1\n  entry: entry.sh\n  timeout_seconds: 5\n---\nOriginal instructions.\n"
	if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	executable, _ := os.Executable()
	body := "#!/bin/bash\nexec '" + strings.ReplaceAll(executable, "'", "'\"'\"'") + "' -test.run=^TestNativeOwnerHelper$ -- --owner-fixture\n"
	if err := os.WriteFile(filepath.Join(directory, "entry.sh"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	rt.cfg.Discovery.Skills.Enabled = true
	rt.cfg.Discovery.Skills.Dirs = nil
	rt.cfg.Discovery.Skills.NativeDirs = []string{directory}
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "native"})
	return rt, opened["remote_session_id"].(string), directory
}

func TestNativeSkillExistingToolsDescribeCallObserveAndRetry(t *testing.T) {
	rt, id, directory := nativeRuntimeFixture(t)
	if len(rt.toolHandlers) != 19 {
		t.Fatalf("public tool count changed: %d", len(rt.toolHandlers))
	}
	described := callEnvelope(t, rt.toolSkillTool, context.Background(), map[string]any{"action": "describe", "remote_session_id": id, "name": "native-owner"})
	if !statusOK(described) {
		t.Fatalf("describe=%+v", described)
	}
	data := described["data"].(map[string]any)
	if data["external_steps"] != true || data["instructions"] != "Original native owner instructions." {
		t.Fatalf("owner contract=%+v", data)
	}
	request := map[string]any{"action": "call", "remote_session_id": id, "name": "native-owner", "purpose": "save a fixture output", "idempotency_key": "native-once", "arguments": map[string]any{"operation": "submit", "value": 2}}
	first := callEnvelope(t, rt.toolSkillTool, context.Background(), request)
	if !statusOK(first) {
		t.Fatalf("native call=%+v", first)
	}
	data = first["data"].(map[string]any)
	if data["status"] != "prepared" || data["native_operation"] != "submit" {
		t.Fatalf("owner result not structured=%+v", data)
	}
	if data["runtime_context"].(map[string]any)["remote_session_id"] != id {
		t.Fatal("Runtime identity not injected")
	}
	replay := callEnvelope(t, rt.toolSkillTool, context.Background(), request)
	if !statusOK(replay) {
		t.Fatalf("replay=%+v", replay)
	}
	invalid := callEnvelope(t, rt.toolSkillTool, context.Background(), map[string]any{"action": "call", "remote_session_id": id, "name": "native-owner", "purpose": "invalid fixture", "arguments": map[string]any{"operation": "submit", "value": 0}})
	if statusOK(invalid) || errorCode(invalid) != "skill_argument_invalid" {
		t.Fatalf("numeric Schema constraint ignored=%+v", invalid)
	}
	rejected := callEnvelope(t, rt.toolSkillTool, context.Background(), map[string]any{"action": "call", "remote_session_id": id, "name": "native-owner", "purpose": "owner rejects fixture", "arguments": map[string]any{"operation": "submit", "value": 1, "mode": "error"}})
	if statusOK(rejected) || errorCode(rejected) != "native_test_rejected" {
		t.Fatalf("owner error lost=%+v", rejected)
	}
	observed, err := rt.toolHandlers["observe"](context.Background(), mcpresult.Request(map[string]any{"remote_session_id": id, "view": "skill", "name": "native-owner", "run_id": "run-1"}))
	if err != nil {
		t.Fatal(err)
	}
	view := decodeToolResult(t, observed)
	if !statusOK(view) || view["data"].(map[string]any)["read_only"] != true {
		t.Fatalf("observe=%+v", view)
	}
	conflict, err := rt.toolHandlers["observe"](context.Background(), mcpresult.Request(map[string]any{"remote_session_id": id, "view": "skill", "name": "native-owner", "run_id": "run-1", "execution_task_id": "fake"}))
	if err != nil {
		t.Fatal(err)
	}
	if statusOK(decodeToolResult(t, conflict)) {
		t.Fatal("observe accepted conflicting execution target")
	}
	// Entry bytes participate in the existing definition lease.
	path := filepath.Join(directory, "entry.sh")
	body, _ := os.ReadFile(path)
	if err := os.WriteFile(path, append(body, []byte("\n# changed definition\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	changed := callEnvelope(t, rt.toolSkillTool, context.Background(), map[string]any{"action": "call", "remote_session_id": id, "name": "native-owner", "purpose": "changed fixture", "arguments": map[string]any{"operation": "prepare", "value": 1}})
	if statusOK(changed) || errorCode(changed) != "skill_revision_changed" {
		t.Fatalf("native entry revision ignored=%+v", changed)
	}
}
