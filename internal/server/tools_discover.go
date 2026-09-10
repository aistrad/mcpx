package server

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/approval"
	"mcpx/internal/envelope"
	"mcpx/internal/idempotency"
	"mcpx/internal/mcpproxy"
	"mcpx/internal/mcpresult"
	"mcpx/internal/remotesession"
	"mcpx/internal/skill"
)

type discoveryLease struct {
	ID              string
	Revision        string
	RemoteSessionID string
	PrincipalID     string
	WorkspacePath   string
	Kind            string
	Object          string
}

func (r *Runtime) toolSkillTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	switch action := toolAction(req); action {
	case "list":
		return r.skillToolList(ctx, req)
	case "describe":
		return r.skillToolDescribe(ctx, req)
	case "call":
		return r.withCleanIdempotency(ctx, req, "skill_tool", mcpresult.Arguments(req), r.toolSkillExecute, r.preflightSkillToolCall)
	default:
		envReq, _, fail := r.remoteRequest(ctx, req)
		if fail != nil {
			return fail, nil
		}
		return r.terminalError(envReq, envReq.RemoteSessionID, envReq.Workspace, "INVALID_ACTION", fmt.Sprintf("skill_tool does not support action %q", action))
	}
}

func (r *Runtime) skillToolList(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	envReq, _, session, fail := r.changeRequest(ctx, req, false)
	if fail != nil {
		return fail, nil
	}
	effective := r.effectiveConfig(session.WorkspacePath)
	if !effective.Discovery.Skills.Enabled {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "SKILL_DISABLED", "skills are disabled")
	}
	items := skill.LoadAll(effective.Discovery.Skills.Dirs, session.WorkspacePath)
	items = filterSkillsByQuery(items, strings.TrimSpace(stringPayload(envReq.Payload, "query")))
	return r.remoteResult(envReq, session.ID, session.WorkspaceName, map[string]any{"skills": compactSkillInventory(items)})
}

func (r *Runtime) skillToolDescribe(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	envReq, principal, session, fail := r.changeRequest(ctx, req, false)
	if fail != nil {
		return fail, nil
	}
	effective := r.effectiveConfig(session.WorkspacePath)
	if !effective.Discovery.Skills.Enabled {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "SKILL_DISABLED", "skills are disabled")
	}
	name := strings.TrimSpace(stringPayload(envReq.Payload, "name"))
	items := skill.LoadAll(effective.Discovery.Skills.Dirs, session.WorkspacePath)
	sk, ok := skill.Find(items, name)
	if !ok {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "SKILL_NOT_FOUND", fmt.Sprintf("skill %q was not found", name))
	}
	descriptor := skillItems([]skill.Skill{sk})[0]
	revision, _ := descriptor["revision"].(string)
	r.upsertDiscoveryLease(discoveryLease{
		Revision: revision, RemoteSessionID: session.ID, PrincipalID: principal.ID,
		WorkspacePath: session.WorkspacePath, Kind: "skill", Object: name,
	})
	risk := skillExecutionRisk(sk)
	result := map[string]any{
		"name":             sk.Manifest.Name,
		"description":      sk.Manifest.Description,
		"arguments_schema": descriptor["arguments_schema"],
		"execution_mode":   descriptor["kind"],
		"permissions":      sk.Manifest.Permissions,
		"risk":             risk.publicData(),
	}
	if instructions, err := skillInstructions(sk); err == nil && instructions != "" {
		result["instructions"] = instructions
	}
	return r.remoteResult(envReq, session.ID, session.WorkspaceName, result)
}

func skillInstructions(sk skill.Skill) (string, error) {
	if sk.Manifest.Runtime != "markdown" && sk.Manifest.Format != "skill_md" {
		return "", nil
	}
	path, err := skill.ResolveEntry(sk)
	if err != nil {
		return "", err
	}
	body, err := os.ReadFile(path)
	if err != nil && (sk.Manifest.Entry == "" || sk.Manifest.Entry == "SKILL.md") {
		if alt, altErr := skill.ResolveEntryName(sk, "skill.md"); altErr == nil {
			if altBody, altErr2 := os.ReadFile(alt); altErr2 == nil {
				return string(altBody), nil
			}
		}
	}
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func compactSkillInventory(skills []skill.Skill) []map[string]any {
	items := make([]map[string]any, 0, len(skills))
	for _, sk := range skills {
		items = append(items, map[string]any{
			"name":        sk.Manifest.Name,
			"description": sk.Manifest.Description,
		})
	}
	return items
}

func compactSkillMaps(skills []map[string]any) []map[string]any {
	items := make([]map[string]any, 0, len(skills))
	for _, sk := range skills {
		item := map[string]any{"name": sk["name"]}
		if description := sk["description"]; description != nil {
			item["description"] = description
		}
		items = append(items, item)
	}
	return items
}

func (r *Runtime) preflightSkillToolCall(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	envReq, principal, remote, fail := r.changeRequest(ctx, req, true)
	if fail != nil {
		return fail, nil
	}
	effective := r.effectiveConfig(remote.WorkspacePath)
	if !effective.Discovery.Skills.Enabled {
		return r.terminalError(envReq, remote.ID, remote.WorkspaceName, "SKILL_DISABLED", "skills are disabled")
	}
	name := strings.TrimSpace(stringPayload(envReq.Payload, "name"))
	sk, ok := skill.Find(skill.LoadAll(effective.Discovery.Skills.Dirs, remote.WorkspacePath), name)
	if !ok {
		return r.terminalError(envReq, remote.ID, remote.WorkspaceName, "SKILL_NOT_FOUND", fmt.Sprintf("skill %q was not found", name))
	}
	current := skillItems([]skill.Skill{sk})[0]
	currentRevision, _ := current["revision"].(string)
	if observed, ok := r.latestDiscoveryLease(remote, principal.ID, "skill", name); ok && observed.Revision != currentRevision {
		response := envelope.Fail(envelope.StatusError, envReq.RequestID, remote.WorkspaceName, nil, "SKILL_REVISION_CHANGED", "Skill changed after it was described")
		response.RemoteSessionID = remote.ID
		if response.Error != nil {
			addRecoveryAction(&response, "skill_tool", "重新读取 Skill 详情后再调用", map[string]any{
				"action": "describe", "remote_session_id": remote.ID, "name": name,
			})
		}
		return r.resultJSON(response)
	}
	arguments, argumentsOK := envReq.Payload["arguments"].(map[string]any)
	if raw, exists := envReq.Payload["arguments"]; exists && raw != nil && !argumentsOK {
		return r.terminalError(envReq, remote.ID, remote.WorkspaceName, "SKILL_ARGUMENT_INVALID", "arguments must be an object")
	}
	if err := validateDiscoveryArguments(sk.Manifest.ArgumentsSchema, arguments); err != nil {
		return r.terminalError(envReq, remote.ID, remote.WorkspaceName, "SKILL_ARGUMENT_INVALID", err.Error())
	}
	risk := skillExecutionRisk(sk)
	if confirmation := r.extensionConfirmationGate(ctx, envReq, principal.ID, remote, "skill_tool", name, currentRevision, risk); confirmation != nil {
		return confirmation, nil
	}
	return nil, nil
}

func (r *Runtime) toolMCPTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	switch action := toolAction(req); action {
	case "list":
		return r.mcpToolList(ctx, req)
	case "describe":
		return r.mcpToolDescribe(ctx, req)
	case "call":
		return r.mcpToolCallWithObservedSession(ctx, req)
	default:
		envReq, _, fail := r.remoteRequest(ctx, req)
		if fail != nil {
			return fail, nil
		}
		return r.terminalError(envReq, envReq.RemoteSessionID, envReq.Workspace, "INVALID_ACTION", fmt.Sprintf("mcp_tool does not support action %q", action))
	}
}

func (r *Runtime) mcpToolList(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	envReq, _, session, fail := r.changeRequest(ctx, req, false)
	if fail != nil {
		return fail, nil
	}
	if !r.effectiveConfig(session.WorkspacePath).Discovery.MCP.Enabled {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "MCP_SERVER_UNAVAILABLE", "upstream MCP is disabled")
	}
	manager, err := r.mcpManagerForWorkspace(session.WorkspacePath)
	if err != nil {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "MCP_SERVER_UNAVAILABLE", err.Error())
	}
	serverName := strings.TrimSpace(stringPayload(envReq.Payload, "server"))
	if serverName == "" {
		servers := filterExtensionItemsByQuery(manager.List(), strings.TrimSpace(stringPayload(envReq.Payload, "query")))
		return r.remoteResult(envReq, session.ID, session.WorkspaceName, map[string]any{"servers": compactMCPServerInventory(servers)})
	}
	cfg, ok := manager.ServerConfig(serverName)
	if !ok {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "MCP_SERVER_NOT_FOUND", fmt.Sprintf("MCP server %q is not configured", serverName))
	}
	tools, err := mcpproxy.ListTools(ctx, cfg)
	if err != nil {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "MCP_SERVER_UNAVAILABLE", err.Error())
	}
	items := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		if tool == nil {
			continue
		}
		items = append(items, map[string]any{"name": tool.Name, "description": tool.Description})
	}
	return r.remoteResult(envReq, session.ID, session.WorkspaceName, map[string]any{"server": serverName, "tools": items})
}

func compactMCPServerInventory(servers []map[string]any) []map[string]any {
	items := make([]map[string]any, 0, len(servers))
	for _, server := range servers {
		item := map[string]any{"name": server["name"]}
		if description := server["description"]; description != nil {
			item["description"] = description
		}
		if state := server["state"]; state != nil {
			item["state"] = state
		}
		items = append(items, item)
	}
	return items
}

func (r *Runtime) mcpToolDescribe(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	envReq, principal, session, fail := r.changeRequest(ctx, req, false)
	if fail != nil {
		return fail, nil
	}
	if !r.effectiveConfig(session.WorkspacePath).Discovery.MCP.Enabled {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "MCP_SERVER_UNAVAILABLE", "upstream MCP is disabled")
	}
	serverName := strings.TrimSpace(stringPayload(envReq.Payload, "server"))
	toolName := strings.TrimSpace(stringPayload(envReq.Payload, "tool"))
	manager, err := r.mcpManagerForWorkspace(session.WorkspacePath)
	if err != nil {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "MCP_SERVER_UNAVAILABLE", err.Error())
	}
	cfg, ok := manager.ServerConfig(serverName)
	if !ok {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "MCP_SERVER_NOT_FOUND", fmt.Sprintf("MCP server %q is not configured", serverName))
	}
	tools, err := mcpproxy.ListTools(ctx, cfg)
	if err != nil {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "MCP_SERVER_UNAVAILABLE", err.Error())
	}
	upstream, ok := mcpToolForLease(tools, toolName)
	if !ok {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "MCP_TOOL_NOT_FOUND", fmt.Sprintf("MCP tool %q was not found on server %q", toolName, serverName))
	}
	revision := mcpRevision([]*mcp.Tool{upstream})
	r.upsertDiscoveryLease(discoveryLease{
		Revision: revision, RemoteSessionID: session.ID, PrincipalID: principal.ID,
		WorkspacePath: session.WorkspacePath, Kind: "mcp", Object: serverName + "/" + toolName,
	})
	risk := mcpExecutionRisk(upstream)
	return r.remoteResult(envReq, session.ID, session.WorkspaceName, map[string]any{
		"server":       serverName,
		"tool":         toolName,
		"description":  upstream.Description,
		"input_schema": discoverySchemaMap(upstream.InputSchema),
		"risk":         risk.publicData(),
	})
}

func (r *Runtime) preflightMCPToolCallOnSession(ctx context.Context, req *mcp.CallToolRequest, client *mcpproxy.ClientSession) (*mcp.CallToolResult, *mcp.Tool, error) {
	envReq, principal, remote, fail := r.changeRequest(ctx, req, true)
	if fail != nil {
		return fail, nil, nil
	}
	serverName := strings.TrimSpace(stringPayload(envReq.Payload, "server"))
	toolName := strings.TrimSpace(stringPayload(envReq.Payload, "tool"))
	tools, err := client.ListTools(ctx)
	if err != nil {
		result, resultErr := r.terminalError(envReq, remote.ID, remote.WorkspaceName, "MCP_SERVER_UNAVAILABLE", err.Error())
		return result, nil, resultErr
	}
	upstream, ok := mcpToolForLease(tools, toolName)
	if !ok {
		result, resultErr := r.terminalError(envReq, remote.ID, remote.WorkspaceName, "MCP_TOOL_NOT_FOUND", fmt.Sprintf("MCP tool %q was not found on server %q", toolName, serverName))
		return result, nil, resultErr
	}
	currentRevision := mcpRevision([]*mcp.Tool{upstream})
	object := serverName + "/" + toolName
	if observed, ok := r.latestDiscoveryLease(remote, principal.ID, "mcp", object); ok && observed.Revision != currentRevision {
		response := envelope.Fail(envelope.StatusError, envReq.RequestID, remote.WorkspaceName, nil, "MCP_TOOL_SCHEMA_CHANGED", "MCP tool schema changed after it was described")
		response.RemoteSessionID = remote.ID
		if response.Error != nil {
			addRecoveryAction(&response, "mcp_tool", "重新读取 MCP Tool schema 后再调用", map[string]any{
				"action": "describe", "remote_session_id": remote.ID, "server": serverName, "tool": toolName,
			})
		}
		result, resultErr := r.resultJSON(response)
		return result, nil, resultErr
	}
	arguments, argumentsOK := envReq.Payload["arguments"].(map[string]any)
	if raw, exists := envReq.Payload["arguments"]; exists && raw != nil && !argumentsOK {
		result, resultErr := r.terminalError(envReq, remote.ID, remote.WorkspaceName, "MCP_ARGUMENT_INVALID", "arguments must be an object")
		return result, nil, resultErr
	}
	if err := validateDiscoveryArguments(discoverySchemaMap(upstream.InputSchema), arguments); err != nil {
		result, resultErr := r.terminalError(envReq, remote.ID, remote.WorkspaceName, "MCP_ARGUMENT_INVALID", err.Error())
		return result, nil, resultErr
	}
	risk := mcpExecutionRisk(upstream)
	if confirmation := r.extensionConfirmationGate(ctx, envReq, principal.ID, remote, "mcp_tool", object, currentRevision, risk); confirmation != nil {
		return confirmation, nil, nil
	}
	return nil, upstream, nil
}

type extensionRisk struct {
	ReadOnly             bool
	Destructive          bool
	Idempotent           bool
	OpenWorld            bool
	ConfirmationRequired bool
	Classification       string
	Permissions          []string
}

func (risk extensionRisk) publicData() map[string]any {
	data := map[string]any{
		"read_only": risk.ReadOnly, "destructive": risk.Destructive, "idempotent": risk.Idempotent,
		"open_world": risk.OpenWorld, "confirmation_required": risk.ConfirmationRequired,
		"classification": risk.Classification,
	}
	if len(risk.Permissions) > 0 {
		data["permissions"] = append([]string(nil), risk.Permissions...)
	}
	return data
}

func skillExecutionRisk(sk skill.Skill) extensionRisk {
	if sk.Manifest.Runtime == "markdown" || sk.Manifest.Format == "skill_md" {
		return extensionRisk{ReadOnly: true, Idempotent: true, Classification: "skill_instruction_read", Permissions: append([]string(nil), sk.Manifest.Permissions...)}
	}
	destructive := false
	for _, permission := range sk.Manifest.Permissions {
		normalized := strings.ToLower(strings.TrimSpace(permission))
		if strings.Contains(normalized, "delete") || strings.Contains(normalized, "remove") || strings.Contains(normalized, "destructive") {
			destructive = true
		}
	}
	return extensionRisk{
		Destructive: destructive, OpenWorld: true, ConfirmationRequired: true,
		Classification: "skill_executable", Permissions: append([]string(nil), sk.Manifest.Permissions...),
	}
}

func mcpExecutionRisk(tool *mcp.Tool) extensionRisk {
	if tool == nil || tool.Annotations == nil {
		return extensionRisk{OpenWorld: true, ConfirmationRequired: true, Classification: "upstream_mcp_unknown_risk"}
	}
	readOnly := tool.Annotations.ReadOnlyHint
	// MCP defines destructiveHint=true as the default for non-read-only
	// tools. An omitted hint must therefore remain confirmation-gated.
	destructive := !readOnly
	if tool.Annotations.DestructiveHint != nil {
		destructive = *tool.Annotations.DestructiveHint
	}
	openWorld := true
	if tool.Annotations.OpenWorldHint != nil {
		openWorld = *tool.Annotations.OpenWorldHint
	}
	return extensionRisk{
		ReadOnly: readOnly, Destructive: destructive, Idempotent: tool.Annotations.IdempotentHint, OpenWorld: openWorld,
		ConfirmationRequired: destructive || !readOnly || openWorld,
		Classification:       "upstream_mcp_annotations",
	}
}

func extensionConfirmationContentKey(principalID, operation, target, revision string, payload map[string]any) string {
	// purpose is semantic/audit context only. Confirmation matching must follow
	// the actual effect fingerprint so a rephrased requirement does not create
	// a new approval for the same target, revision, and arguments.
	return skillRevision(strings.Join([]string{
		principalID, operation, target, revision, cleanIdempotencyFingerprint(operation, payload),
	}, "\x00"))
}

func (r *Runtime) pendingExtensionConfirmation(remoteSessionID, principalID, operation, contentKey string) (approval.Pending, bool) {
	for _, pending := range r.approvals.ListRemoteSession(remoteSessionID) {
		if pending.PrincipalID == principalID && pending.Tool == operation && pending.ContentKey == contentKey {
			return pending, true
		}
	}
	return approval.Pending{}, false
}

func (r *Runtime) extensionReplayKnown(ctx context.Context, remote remotesession.Session, principalID, operation string, payload map[string]any) bool {
	if r.idempotency == nil || !boolPayload(payload, "user_confirmed") {
		return false
	}
	value := strings.TrimSpace(stringPayload(payload, "idempotency_key"))
	if value == "" {
		return false
	}
	// Only a completed request proves the same operation was confirmed and
	// executed before. A pending placeholder — including the caller's own
	// just-claimed record — must not bypass the confirmation gate.
	record, ok, err := r.idempotency.Lookup(ctx, idempotency.Key{RemoteSessionID: remote.ID, PrincipalID: principalID, Operation: operation, Value: value})
	if err != nil || !ok {
		return false
	}
	return record.State == idempotency.StateSucceeded || record.State == idempotency.StateFailed
}

func (r *Runtime) extensionConfirmationGate(ctx context.Context, envReq envelope.Request, principalID string, remote remotesession.Session, operation, target, revision string, risk extensionRisk) *mcp.CallToolResult {
	if !risk.ConfirmationRequired {
		return nil
	}
	contentKey := extensionConfirmationContentKey(principalID, operation, target, revision, envReq.Payload)
	pending, pendingOK := r.pendingExtensionConfirmation(remote.ID, principalID, operation, contentKey)
	if operation == "mcp_tool" {
		confirmationKey := strings.TrimSpace(stringPayload(envReq.Payload, "confirmation_key"))
		if pendingOK && confirmationKey != "" && confirmationKey == pending.ConfirmationToken {
			return nil
		}
	} else if boolPayload(envReq.Payload, "user_confirmed") && (pendingOK || r.extensionReplayKnown(ctx, remote, principalID, operation, envReq.Payload)) {
		return nil
	}
	if !pendingOK {
		var err error
		pending, err = r.approvals.PutPending(approval.Pending{
			Tool: operation, Summary: target, Purpose: stringPayload(envReq.Payload, "purpose"), Scope: "workspace",
			RequestID: envReq.RequestID, Workspace: remote.WorkspaceName, RemoteSessionID: remote.ID,
			PrincipalID: principalID, ContentKey: contentKey,
		})
		if err != nil {
			result, _ := r.terminalError(envReq, remote.ID, remote.WorkspaceName, "CONFIRMATION_STORE_ERROR", err.Error())
			return result
		}
	}
	data := map[string]any{
		"target": target, "purpose": stringPayload(envReq.Payload, "purpose"), "risk": risk.publicData(),
		"confirmation_required": true,
	}
	if operation == "mcp_tool" {
		data["confirmation_key"] = pending.ConfirmationToken
		data["summary"] = "该 MCP 调用可能产生副作用；请向用户展示目标、用途和风险，确认后携带 waiting_confirmation 返回的 confirmation_key，以相同业务参数重试。"
	} else {
		data["user_confirmed_required"] = true
		data["summary"] = "该扩展调用可能产生副作用；请向用户展示目标、用途和风险，确认后以相同业务参数设置 user_confirmed=true 重试。"
	}
	response := envelope.Fail(envelope.StatusNeedConfirmation, envReq.RequestID, remote.WorkspaceName, data, "USER_CONFIRMATION_REQUIRED", "扩展调用等待用户语义确认")
	response.RemoteSessionID = remote.ID
	if response.Error != nil {
		arguments := map[string]any{"action": "call", "remote_session_id": remote.ID, "purpose": stringPayload(envReq.Payload, "purpose")}
		for _, key := range []string{"name", "server", "tool", "idempotency_key"} {
			if value, ok := envReq.Payload[key]; ok {
				arguments[key] = value
			}
		}
		// Extension arguments may contain a secret. Keep the target and require
		// the caller to reuse its original arguments instead of echoing them
		// into an error/recovery payload.
		if operation == "mcp_tool" {
			arguments["confirmation_key"] = pending.ConfirmationToken
			addRecoveryAction(&response, operation, "用户确认后使用相同 MCP 目标、原始 arguments 和用途重试，并携带 waiting_confirmation 返回的 confirmation_key", arguments)
		} else {
			arguments["user_confirmed"] = true
			addRecoveryAction(&response, operation, "用户确认后使用相同扩展目标、原始 arguments 和用途重试，并设置 user_confirmed=true", arguments)
		}
	}
	result, _ := r.resultJSON(response)
	_ = pending
	return result
}

func (r *Runtime) consumeExtensionConfirmation(remoteSessionID, principalID, operation, contentKey string) {
	if pending, ok := r.pendingExtensionConfirmation(remoteSessionID, principalID, operation, contentKey); ok {
		_, _ = r.approvals.Consume(pending.ID)
	}
}

func (r *Runtime) upsertDiscoveryLease(input discoveryLease) discoveryLease {
	r.discoveryMu.Lock()
	defer r.discoveryMu.Unlock()
	for key, existing := range r.discoveries {
		if existing.RemoteSessionID == input.RemoteSessionID && existing.PrincipalID == input.PrincipalID &&
			existing.WorkspacePath == input.WorkspacePath && existing.Kind == input.Kind && existing.Object == input.Object {
			input.ID = existing.ID
			r.discoveries[key] = input
			return input
		}
	}
	input.ID = newRuntimeID("ext", 12)
	r.discoveries[input.ID] = input
	return input
}

func (r *Runtime) latestDiscoveryLease(session remotesession.Session, principalID, kind, object string) (discoveryLease, bool) {
	r.discoveryMu.Lock()
	defer r.discoveryMu.Unlock()
	for _, lease := range r.discoveries {
		if lease.RemoteSessionID == session.ID && lease.PrincipalID == principalID && lease.WorkspacePath == session.WorkspacePath && lease.Kind == kind && lease.Object == object {
			return lease, true
		}
	}
	return discoveryLease{}, false
}
