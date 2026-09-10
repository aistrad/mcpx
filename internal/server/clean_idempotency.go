package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/envelope"
	"mcpx/internal/idempotency"
	"mcpx/internal/mcpresult"
	"mcpx/internal/remotesession"
)

// cleanIdempotencyFingerprint deliberately stores only a digest. The request
// payload can contain upstream arguments or one-shot values, so raw business
// parameters must never be persisted in the idempotency table.
func cleanIdempotencyFingerprint(operation string, payload map[string]any) string {
	canonical := make(map[string]any, len(payload)+1)
	canonical["operation"] = operation
	for key, value := range payload {
		switch key {
		case "idempotency_key", "user_confirmed", "confirmation_key", "confirmation_token", "client_request_id",
			"request_id", "purpose", "intent", "progress_summary", "execution_mode":
			// These fields are retry/auth/audit metadata, not the effect itself.
		default:
			canonical[key] = value
		}
	}
	raw, _ := json.Marshal(canonical)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

type storedCleanToolResult struct {
	Structured json.RawMessage `json:"structured"`
	Text       string          `json:"text,omitempty"`
	IsError    bool            `json:"is_error,omitempty"`
}

func encodeCleanToolResult(operation string, result *mcp.CallToolResult) ([]byte, error) {
	if result == nil {
		return nil, fmt.Errorf("tool result is nil")
	}
	if operation == "mcp_tool" {
		return json.Marshal(result)
	}
	structured, err := json.Marshal(result.StructuredContent)
	if err != nil {
		return nil, err
	}
	return json.Marshal(storedCleanToolResult{
		Structured: structured,
		Text:       mcpresult.FirstText(result),
		IsError:    result.IsError,
	})
}

func decodeCleanToolResult(operation string, encoded []byte, replay bool) (*mcp.CallToolResult, error) {
	if operation == "mcp_tool" {
		var result mcp.CallToolResult
		if err := json.Unmarshal(encoded, &result); err != nil {
			return nil, err
		}
		if replay {
			if result.Meta == nil {
				result.Meta = mcp.Meta{}
			}
			result.Meta["mcpx/idempotent_replay"] = true
		}
		return &result, nil
	}
	var stored storedCleanToolResult
	if err := json.Unmarshal(encoded, &stored); err != nil {
		return nil, err
	}
	if len(stored.Structured) == 0 || string(stored.Structured) == "null" {
		return nil, fmt.Errorf("stored tool result has no structured content")
	}
	var structured any
	if err := json.Unmarshal(stored.Structured, &structured); err != nil {
		return nil, err
	}
	if replay {
		markCleanReplay(structured)
	}
	result := mcpresult.NewStructured(structured, stored.Text)
	result.IsError = stored.IsError
	return result, nil
}

func markCleanReplay(value any) {
	wire, ok := value.(map[string]any)
	if !ok {
		return
	}
	if data, ok := wire["data"].(map[string]any); ok {
		data["idempotent_replay"] = true
		return
	}
	wire["idempotent_replay"] = true
}

func cleanResultState(operation string, result *mcp.CallToolResult) string {
	if result == nil {
		return idempotency.StateInDoubt
	}
	if operation == "mcp_tool" && result.IsError {
		return idempotency.StateFailed
	}
	if wire, ok := result.StructuredContent.(map[string]any); ok {
		if status, _ := wire["status"].(string); status == string(envelope.StatusError) {
			return idempotency.StateFailed
		}
	}
	return idempotency.StateSucceeded
}

// withCleanIdempotency surrounds a mutating clean-core handler. The wrapped
// handler is deliberately called only after Claim has made this request the
// durable owner, so local concurrency and process-restart retries cannot
// launch a second effect.
//
// Requests that carry an idempotency_key resolve the durable record before
// semantic preflight. A completed request replays its persisted result even if
// the current Skill/MCP definition or argument schema changed; fresh and
// pending requests Claim before preflight, so concurrent retries with the same
// key merge here and only the durable owner needs the current environment.
func (r *Runtime) withCleanIdempotency(
	ctx context.Context,
	req *mcp.CallToolRequest,
	operation string,
	payload map[string]any,
	handler func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error),
	preflight ...func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error),
) (*mcp.CallToolResult, error) {
	keyValue := strings.TrimSpace(stringPayload(payload, "idempotency_key"))
	if keyValue == "" || r.idempotency == nil {
		// Semantic preflight is independent of idempotency. Extension calls use
		// it to validate the current Skill/MCP revision and arguments on every
		// call, including calls that intentionally omit an idempotency key.
		if len(preflight) > 0 && preflight[0] != nil {
			result, preflightErr := preflight[0](ctx, req)
			if result != nil || preflightErr != nil {
				return result, preflightErr
			}
		}
		return handler(ctx, req)
	}

	envReq, principal, session, fail := r.changeRequest(ctx, req, true)
	if fail != nil {
		return fail, nil
	}
	key := idempotency.Key{
		RemoteSessionID: session.ID,
		PrincipalID:     principal.ID,
		Operation:       operation,
		Value:           keyValue,
	}
	fingerprint := cleanIdempotencyFingerprint(operation, payload)

	// A completed request must replay its persisted result without preflight:
	// the current Skill/MCP definition may have changed, and an upstream MCP
	// process would otherwise be restarted for a cached response. Conflict and
	// in-doubt records are also resolved here, before any effectful preflight.
	record, ok, lookupErr := r.idempotency.Lookup(ctx, key)
	if lookupErr != nil {
		return r.cleanIdempotencyFailure(envReq, session, operation, payload, "IDEMPOTENCY_STORE_ERROR", lookupErr.Error()), nil
	}
	if ok {
		switch record.State {
		case idempotency.StateSucceeded, idempotency.StateFailed:
			if record.Fingerprint != fingerprint {
				return r.cleanIdempotencyConflict(envReq, session, operation, payload, fingerprint, record.Fingerprint)
			}
			result, decodeErr := decodeCleanToolResult(operation, record.Response, true)
			if decodeErr != nil {
				return r.cleanIdempotencyFailure(envReq, session, operation, payload, "IDEMPOTENCY_IN_DOUBT", decodeErr.Error()), nil
			}
			return result, nil
		case idempotency.StateInDoubt:
			return r.cleanIdempotencyFailure(envReq, session, operation, payload, "IDEMPOTENCY_IN_DOUBT", "the previous request may have partially reached its target; reconcile before retrying"), nil
		}
	}

	// Fresh and pending requests Claim before semantic preflight so that
	// concurrent retries with the same key merge here: only the durable owner
	// resolves the current environment or starts an upstream process.
	claim, result := r.claimCleanIdempotency(ctx, envReq, session, operation, payload, key, fingerprint)
	if result != nil {
		return result, nil
	}
	if claim.Kind != idempotency.ClaimOwner {
		return r.cleanIdempotencyFailure(envReq, session, operation, payload, "IDEMPOTENCY_STORE_ERROR", "unexpected idempotency claim state"), nil
	}

	// Only the durable owner runs semantic preflight. If it aborts
	// (confirmation required, revision/schema changed, invalid arguments), the
	// pending placeholder is abandoned so a retry is not blocked by a stale
	// pending state.
	if len(preflight) > 0 && preflight[0] != nil {
		result, preflightErr := preflight[0](ctx, req)
		if result != nil || preflightErr != nil {
			_ = r.idempotency.Abandon(ctx, key, fingerprint)
			return result, preflightErr
		}
	}

	result, callErr := handler(ctx, req)
	if result == nil {
		_ = r.idempotency.MarkInDoubt(ctx, key, fingerprint, nil)
		return r.cleanIdempotencyFailure(envReq, session, operation, payload, "IDEMPOTENCY_IN_DOUBT", "handler returned no durable result; reconcile before retrying"), callErr
	}
	encoded, encodeErr := encodeCleanToolResult(operation, result)
	if encodeErr != nil {
		_ = r.idempotency.MarkInDoubt(ctx, key, fingerprint, nil)
		return r.cleanIdempotencyFailure(envReq, session, operation, payload, "IDEMPOTENCY_IN_DOUBT", encodeErr.Error()), callErr
	}
	state := cleanResultState(operation, result)
	if callErr != nil && state == idempotency.StateSucceeded {
		state = idempotency.StateFailed
	}
	if completeErr := r.idempotency.Complete(ctx, key, fingerprint, state, encoded, []byte(`{"operation":"clean-core"}`)); completeErr != nil {
		_ = r.idempotency.MarkInDoubt(ctx, key, fingerprint, []byte(`{"recovery":"reconcile the target before retrying"}`))
		return r.cleanIdempotencyFailure(envReq, session, operation, payload, "IDEMPOTENCY_IN_DOUBT", completeErr.Error()), callErr
	}
	return result, callErr
}

// claimCleanIdempotency resolves the durable owner for a clean-core request.
// It returns a final response for every claim kind except ClaimOwner; for
// ClaimOwner it returns the claim with a nil response so the caller can run
// preflight and execute.
func (r *Runtime) claimCleanIdempotency(
	ctx context.Context,
	envReq envelope.Request,
	session remotesession.Session,
	operation string,
	payload map[string]any,
	key idempotency.Key,
	fingerprint string,
) (idempotency.Claim, *mcp.CallToolResult) {
	claim, err := r.idempotency.Claim(ctx, key, fingerprint, cleanIdempotencyRetention)
	if err != nil {
		return claim, r.cleanIdempotencyFailure(envReq, session, operation, payload, "IDEMPOTENCY_STORE_ERROR", err.Error())
	}
	switch claim.Kind {
	case idempotency.ClaimConflict:
		result, _ := r.cleanIdempotencyConflict(envReq, session, operation, payload, fingerprint, claim.Record.Fingerprint)
		return claim, result
	case idempotency.ClaimReplay:
		result, decodeErr := decodeCleanToolResult(operation, claim.Record.Response, true)
		if decodeErr != nil {
			return claim, r.cleanIdempotencyFailure(envReq, session, operation, payload, "IDEMPOTENCY_IN_DOUBT", decodeErr.Error())
		}
		return claim, result
	case idempotency.ClaimInDoubt:
		return claim, r.cleanIdempotencyFailure(envReq, session, operation, payload, "IDEMPOTENCY_IN_DOUBT", "the previous request may have partially reached its target; reconcile before retrying")
	case idempotency.ClaimWait:
		record, waitErr := r.idempotency.Wait(ctx, claim, key)
		if waitErr != nil {
			return claim, r.cleanIdempotencyFailure(envReq, session, operation, payload, "IDEMPOTENCY_IN_PROGRESS", waitErr.Error())
		}
		result, decodeErr := decodeCleanToolResult(operation, record.Response, true)
		if decodeErr != nil {
			return claim, r.cleanIdempotencyFailure(envReq, session, operation, payload, "IDEMPOTENCY_IN_DOUBT", decodeErr.Error())
		}
		return claim, result
	case idempotency.ClaimPending:
		return claim, r.cleanIdempotencyFailure(envReq, session, operation, payload, "IDEMPOTENCY_IN_PROGRESS", "the same idempotency request is still running")
	case idempotency.ClaimOwner:
		return claim, nil
	default:
		return claim, r.cleanIdempotencyFailure(envReq, session, operation, payload, "IDEMPOTENCY_STORE_ERROR", "unknown idempotency claim state")
	}
}

// cleanIdempotencyRetention bounds how long a durable idempotency record
// survives; every clean-core operation shares the same retention window.
const cleanIdempotencyRetention = 24 * time.Hour

func (r *Runtime) cleanIdempotencyConflict(envReq envelope.Request, session remotesession.Session, operation string, payload map[string]any, current, original string) (*mcp.CallToolResult, error) {
	return r.cleanIdempotencyFailureWithDetails(envReq, session, operation, payload, "IDEMPOTENCY_CONFLICT", "idempotency_key is already bound to different business parameters", map[string]any{
		"current_fingerprint":  current,
		"original_fingerprint": original,
	})
}

func (r *Runtime) cleanIdempotencyFailure(envReq envelope.Request, session remotesession.Session, operation string, payload map[string]any, code, message string) *mcp.CallToolResult {
	result, _ := r.cleanIdempotencyFailureWithDetails(envReq, session, operation, payload, code, message, nil)
	return result
}

func (r *Runtime) cleanIdempotencyFailureWithDetails(envReq envelope.Request, session remotesession.Session, operation string, payload map[string]any, code, message string, details map[string]any) (*mcp.CallToolResult, error) {
	response := envelope.Fail(envelope.StatusError, envReq.RequestID, session.WorkspaceName, nil, code, message)
	response.RemoteSessionID = session.ID
	if response.Error != nil {
		for key, value := range details {
			response.Error.Details[key] = value
		}
		response.Error.Details["idempotency_key_scope"] = "remote_session_id + principal_id + operation + idempotency_key"
		arguments := map[string]any{"remote_session_id": session.ID, "note": "使用新的 idempotency_key；不要重放未知状态的请求"}
		for _, key := range []string{"action", "plan_task_id", "execution_task_id", "plan_id", "artifact_id", "name", "server", "tool"} {
			if value, ok := payload[key]; ok {
				arguments[key] = value
			}
		}
		recoveryTool, recoveryAction := cleanIdempotencyRecovery(operation, payload)
		response.Error.Details["next_action"] = map[string]any{"tool": recoveryTool, "action": recoveryAction, "arguments": arguments}
		addRecoveryAction(&response, recoveryTool, "根据幂等状态恢复后再继续；变更业务参数时使用新的 idempotency_key", arguments)
	}
	return r.resultJSON(response)
}

func cleanIdempotencyRecovery(operation string, payload map[string]any) (tool, action string) {
	switch operation {
	case "execute":
		return "observe", "status"
	case "plan":
		return "plan", "read"
	case "artifact":
		if stringPayload(payload, "artifact_id") != "" {
			return "artifact", "read"
		}
		return "artifact", "list"
	case "skill_tool", "mcp_tool":
		return operation, "describe"
	default:
		return operation, "read"
	}
}
