package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/audit"
	"mcpx/internal/envelope"
	"mcpx/internal/remotesession"
	"mcpx/internal/skill"
)

func nativeRuntimeContext(request envelope.Request, remote remotesession.Session) skill.NativeRuntimeContext {
	return skill.NativeRuntimeContext{
		RemoteSessionID: remote.ID, Workspace: remote.WorkspaceName, WorkspacePath: remote.WorkspacePath,
		RequestID: request.RequestID, IdempotencyKey: stringPayload(request.Payload, "idempotency_key"),
	}
}

// Native descriptors are supplied by the original owner, not a second MCPX DAG.
// Definition leases include both approved executable bytes and its stable contract.
func nativeSkillDescriptor(ctx context.Context, sk skill.Skill, rc skill.NativeRuntimeContext, env []string) (map[string]any, error) {
	output, err := skill.ExecuteNative(ctx, sk, "describe", nil, rc, env)
	if err != nil {
		return nil, err
	}
	if output["status"] == "error" {
		return nil, fmt.Errorf("native Skill descriptor is unavailable")
	}
	schema, ok := output["arguments_schema"].(map[string]any)
	if !ok || len(schema) == 0 {
		return nil, fmt.Errorf("native Skill descriptor must supply arguments_schema")
	}
	if _, err := resolveNativeArguments(schema); err != nil {
		return nil, err
	}
	descriptor := skillItems([]skill.Skill{sk})[0]
	descriptor["arguments_schema"] = schema
	descriptor["entrypoints"] = output["entrypoints"]
	descriptor["instructions"] = output["instructions"]
	descriptor["contract_revision"] = output["contract_revision"]
	body, err := json.Marshal(map[string]any{
		"definition": descriptor["revision"], "arguments_schema": schema,
		"contract_revision": output["contract_revision"], "entrypoints": output["entrypoints"],
		"instructions": output["instructions"],
	})
	if err != nil {
		return nil, err
	}
	descriptor["revision"] = skillRevision(string(body))
	return descriptor, nil
}

func resolveNativeArguments(value map[string]any) (*jsonschema.Resolved, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("invalid native arguments schema")
	}
	var schema jsonschema.Schema
	if err = json.Unmarshal(raw, &schema); err != nil {
		return nil, fmt.Errorf("invalid native arguments schema: %w", err)
	}
	// No remote schema loader: a descriptor cannot initiate network/file reads.
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return nil, fmt.Errorf("invalid native arguments schema: %w", err)
	}
	return resolved, nil
}

func validateNativeArguments(schema map[string]any, arguments map[string]any) error {
	if arguments == nil {
		arguments = map[string]any{}
	}
	if _, exists := arguments["runtime_context"]; exists {
		return fmt.Errorf("runtime_context is server-owned")
	}
	resolved, err := resolveNativeArguments(schema)
	if err != nil {
		return err
	}
	if err = resolved.Validate(arguments); err != nil {
		message := err.Error()
		if len(message) > 1200 {
			message = message[:1200] + " (details omitted)"
		}
		return fmt.Errorf("%s", message)
	}
	return nil
}

func (r *Runtime) nativeSkillResponse(request envelope.Request, remote remotesession.Session, name string, output map[string]any, tool string) (*mcp.CallToolResult, error) {
	status := "ok"
	if output["status"] == "error" {
		status = "error"
		ownerError, _ := output["error"].(map[string]any)
		code, _ := ownerError["code"].(string)
		message, _ := ownerError["message"].(string)
		if code == "" {
			code = "SKILL_NATIVE_FAILED"
		}
		if message == "" {
			message = "native owner rejected this operation"
		}
		response := envelope.Fail(envelope.StatusError, request.RequestID, remote.WorkspaceName, output, code, message)
		response.RemoteSessionID = remote.ID
		if retryable, ok := ownerError["retryable"].(bool); ok {
			response.Error.Retryable = retryable
		}
		if details, ok := ownerError["details"].(map[string]any); ok {
			response.Error.Details = details
		}
		r.logAudit(audit.Event{RequestID: request.RequestID, RemoteSessionID: remote.ID, Workspace: remote.WorkspaceName, Tool: tool, Status: status, Detail: map[string]any{"name": name, "error_code": code}})
		return r.resultJSON(response)
	}
	r.logAudit(audit.Event{RequestID: request.RequestID, RemoteSessionID: remote.ID, Workspace: remote.WorkspaceName, Tool: tool, Status: status, Detail: map[string]any{"name": name, "native_status": output["status"]}})
	return r.remoteResult(request, remote.ID, remote.WorkspaceName, output)
}

func (r *Runtime) toolObserveSkill(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	request, _, remote, fail := r.changeRequest(ctx, req, false)
	if fail != nil {
		return fail, nil
	}
	name := strings.TrimSpace(stringPayload(request.Payload, "name"))
	runID := strings.TrimSpace(stringPayload(request.Payload, "run_id"))
	if name == "" || runID == "" {
		return r.terminalError(request, remote.ID, remote.WorkspaceName, "SKILL_RUN_REQUIRED", "view=skill requires name and run_id")
	}
	effective := r.effectiveConfig(remote.WorkspacePath)
	if !effective.Discovery.Skills.Enabled {
		return r.terminalError(request, remote.ID, remote.WorkspaceName, "SKILL_DISABLED", "skills are disabled")
	}
	sk, ok := skill.Find(skill.LoadConfigured(effective.Discovery.Skills.Dirs, effective.Discovery.Skills.NativeDirs, remote.WorkspacePath), name)
	if !ok || !sk.NativeApproved {
		return r.terminalError(request, remote.ID, remote.WorkspaceName, "SKILL_NATIVE_UNAVAILABLE", "Skill does not expose an approved native observation handler")
	}
	// Do not forward arbitrary caller arguments into an allegedly read-only view.
	output, err := skill.ExecuteNative(ctx, sk, "observe", map[string]any{"run_id": runID}, nativeRuntimeContext(request, remote), effective.Discovery.Skills.NativeEnv)
	if err != nil {
		return r.terminalError(request, remote.ID, remote.WorkspaceName, "SKILL_NATIVE_ERROR", err.Error())
	}
	return r.nativeSkillResponse(request, remote, name, output, "observe")
}
