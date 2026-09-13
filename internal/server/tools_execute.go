package server

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/idempotency"
	"mcpx/internal/mcpresult"
	"mcpx/internal/projecttask"
	"mcpx/internal/security"
)

// toolExecute is the execution surface; task continuation points use execute
// or observe.
func (r *Runtime) toolExecute(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ctx = withCleanCoreRequest(ctx)
	action := toolAction(req)
	switch action {
	case "run":
		if r.cleanExecuteReadyForIdempotency(ctx, req) {
			return r.withCleanIdempotency(ctx, req, "execute", mcpresult.Arguments(req), r.toolCommandExecute)
		}
		return r.toolCommandExecute(ctx, req)
	case "attach", "stop", "stdin":
		forwarded := forwardedRequest(req, map[string]any{"action": action})
		return r.withCleanIdempotency(ctx, forwarded, "execute", mcpresult.Arguments(req), r.toolTaskManage)
	default:
		envReq, _, fail := r.remoteRequest(ctx, req)
		if fail != nil {
			return fail, nil
		}
		return r.terminalError(envReq, envReq.RemoteSessionID, envReq.Workspace, "INVALID_ACTION", fmt.Sprintf("execute does not support action %q", action))
	}
}

// cleanExecuteReadyForIdempotency avoids recording the first confirmation
// response as a terminal replay. A confirm-policy command becomes eligible
// only after the same server-side pending digest is presented with
// user_confirmed=true; denied and malformed requests remain ordinary policy
// responses.
func (r *Runtime) cleanExecuteReadyForIdempotency(ctx context.Context, req *mcp.CallToolRequest) bool {
	if strings.TrimSpace(stringPayload(mcpresult.Arguments(req), "idempotency_key")) == "" {
		return false
	}
	envReq, principal, remote, fail := r.changeRequest(ctx, req, true)
	if fail != nil {
		return false
	}
	purpose, scope, intentErr := commandIntent(envReq)
	if intentErr != nil {
		return false
	}
	runtimeSpec, runtimeErr := ephemeralRuntimeSpecFromPayload(envReq.Payload)
	if runtimeErr != nil {
		return false
	}
	command := strings.TrimSpace(stringPayload(envReq.Payload, "command"))
	argvSpec, argvDisplay, argvDigest, argvErr := executeArgv(envReq.Payload)
	if argvErr != nil {
		return false
	}
	if argvSpec != nil {
		command = argvDisplay
	}
	payloadDigest := argvDigest
	if runtimeSpec != nil {
		command = runtimeSpec.Command
		payloadDigest = runtimeSpec.ScriptSHA256
	} else if taskName := strings.TrimSpace(stringPayload(envReq.Payload, "task")); taskName != "" {
		if command != "" {
			return false
		}
		discovered, ok := projecttask.Find(remote.WorkspacePath, taskName)
		if !ok {
			return false
		}
		command = discovered.Command
	}
	if command == "" {
		return false
	}
	decision := security.MatchCommand(r.effectiveConfig(remote.WorkspacePath).Security.Commands, command)
	if decision == security.Deny {
		return false
	}
	if decision != security.Confirm {
		return true
	}
	if !boolPayload(envReq.Payload, "user_confirmed") {
		return false
	}
	digest := commandRequestDigestWithPayload(envReq.RequestID, remote.ID, remote.WorkspaceName, command, purpose, scope, payloadDigest)
	if _, ok := r.pendingCommandConfirmation(remote.ID, principal.ID, command, scope, digest); ok {
		return true
	}
	// After the approval is consumed, an idempotent retry must still reach
	// Claim so it can replay the durable result instead of creating a fresh
	// confirmation request. A pre-existing record also lets Claim return the
	// correct conflict when the caller changed business parameters.
	key := idempotency.Key{RemoteSessionID: remote.ID, PrincipalID: principal.ID, Operation: "execute", Value: stringPayload(envReq.Payload, "idempotency_key")}
	_, err := r.idempotency.Get(ctx, key)
	return err == nil
}

// toolObserveTask routes task-specific status and logs without exposing the
// legacy task_read name in recovery actions or schemas.
func (r *Runtime) toolObserveTask(ctx context.Context, req *mcp.CallToolRequest, view string) (*mcp.CallToolResult, error) {
	args := mcpresult.Arguments(req)
	if strings.TrimSpace(stringPayload(args, "execution_task_id")) == "" {
		envReq, _, fail := r.remoteRequest(ctx, req)
		if fail != nil {
			return fail, nil
		}
		return r.terminalError(envReq, envReq.RemoteSessionID, envReq.Workspace, "EXECUTION_TASK_ID_REQUIRED", "execution_task_id is required for observe(view="+view+")")
	}
	return r.toolTaskManage(withCleanCoreRequest(ctx), forwardedRequest(req, map[string]any{"action": view}))
}
