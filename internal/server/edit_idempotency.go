package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/edit"
	"mcpx/internal/envelope"
	"mcpx/internal/file"
	"mcpx/internal/idempotency"
	"mcpx/internal/remotesession"
)

func cleanEditFingerprint(_ envelope.Request, edits []edit.FileEdit) string {
	payload := struct {
		Operation string          `json:"operation"`
		Edits     []edit.FileEdit `json:"edits"`
	}{
		Operation: cleanEditIdempotencyOperation,
		Edits:     edits,
	}
	encoded, _ := json.Marshal(payload)
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func editPaths(result edit.BatchResult) []string {
	paths := make([]string, 0, len(result.Results)*2)
	for _, item := range result.Results {
		if item.Path != "" {
			paths = append(paths, item.Path)
		}
		if item.NewPath != "" {
			paths = append(paths, item.NewPath)
		}
	}
	return paths
}

func storedApplyError(err error) *storedEditError {
	var ae *edit.ApplyError
	if errors.As(err, &ae) {
		return &storedEditError{Code: ae.Code, Message: ae.Error(), Path: ae.Path, Index: ae.Index, Current: ae.Current, ChangedLines: ae.ChangedLines}
	}
	return &storedEditError{Code: "EDIT_FAILED", Message: err.Error(), Index: -1}
}

func decodeStoredEdit(encoded []byte) (storedEditResult, error) {
	if len(encoded) == 0 || string(encoded) == "{}" {
		return storedEditResult{}, fmt.Errorf("stored edit result is not prepared")
	}
	var stored storedEditResult
	if err := json.Unmarshal(encoded, &stored); err != nil {
		return storedEditResult{}, err
	}
	if stored.EditID == "" && stored.Error == nil {
		return storedEditResult{}, fmt.Errorf("stored edit result has no edit_id")
	}
	return stored, nil
}

func (r *Runtime) replayStoredEdit(envReq envelope.Request, session remotesession.Session, encoded []byte) (*mcp.CallToolResult, error) {
	stored, err := decodeStoredEdit(encoded)
	if err != nil {
		return r.editIdempotencyPending(envReq, session, err.Error())
	}
	if stored.Error != nil {
		return r.editToolError(envReq, session, stored.Error.applyError())
	}
	return r.editToolSuccess(envReq, session, stored.EditID, stored.Result, true, nil)
}

func (r *Runtime) editIdempotencyConflict(envReq envelope.Request, session remotesession.Session, current, original string) (*mcp.CallToolResult, error) {
	response := envelope.Fail(envelope.StatusError, envReq.RequestID, session.WorkspaceName, nil,
		"IDEMPOTENCY_CONFLICT", "idempotency_key is already bound to different business parameters")
	response.RemoteSessionID = session.ID
	response.Error.Details["current_fingerprint"] = current
	response.Error.Details["original_fingerprint"] = original
	response.Error.Details["recovery"] = "use a new idempotency_key for changed edit parameters"
	response.Error.Recovery = &envelope.Recovery{Action: "edit", Tool: "edit", Arguments: map[string]any{
		"remote_session_id": session.ID,
		"note":              "use a new idempotency_key; do not reuse the conflicting key",
	}}
	return r.resultJSON(response)
}

func (r *Runtime) editIdempotencyPending(envReq envelope.Request, session remotesession.Session, reason string) (*mcp.CallToolResult, error) {
	response := envelope.Fail(envelope.StatusError, envReq.RequestID, session.WorkspaceName, nil,
		"IDEMPOTENCY_IN_PROGRESS", reason)
	response.RemoteSessionID = session.ID
	response.Error.Details["retry_with_same_key"] = true
	response.Error.Details["next_action"] = map[string]any{
		"tool":              "edit",
		"remote_session_id": session.ID,
		"note":              "retry the same business request with the same idempotency_key after the current request finishes",
	}
	response.Error.Recovery = &envelope.Recovery{Action: "edit", Tool: "edit", Arguments: map[string]any{
		"remote_session_id": session.ID,
	}}
	return r.resultJSON(response)
}

func (r *Runtime) editIdempotencyInDoubt(envReq envelope.Request, session remotesession.Session, record idempotency.Record) (*mcp.CallToolResult, error) {
	response := envelope.Fail(envelope.StatusError, envReq.RequestID, session.WorkspaceName, nil,
		"IDEMPOTENCY_IN_DOUBT", "the edit may have partially reached the filesystem; reconcile current file SHA values before retrying")
	response.RemoteSessionID = session.ID
	response.Error.Details["idempotency_key"] = record.Key.Value
	response.Error.Details["fingerprint"] = record.Fingerprint
	if len(record.Metadata) > 0 && string(record.Metadata) != "{}" {
		var metadata map[string]any
		if json.Unmarshal(record.Metadata, &metadata) == nil {
			for key, value := range metadata {
				response.Error.Details[key] = value
			}
		}
	}
	response.Error.Details["next_action"] = map[string]any{
		"tool":              "read",
		"remote_session_id": session.ID,
		"note":              "read the affected files and compare sha256 before choosing a new edit key",
	}
	response.Error.Recovery = &envelope.Recovery{Action: "read", Tool: "read", Arguments: map[string]any{
		"remote_session_id": session.ID,
		"view":              "file",
	}}
	return r.resultJSON(response)
}

func reconcileEditResult(workspaceRoot string, result edit.BatchResult) (expected, original bool, err error) {
	if len(result.Results) == 0 {
		return true, true, nil
	}
	expected, original = true, true
	for _, item := range result.Results {
		itemExpected, itemOriginal, itemErr := reconcileFileResult(workspaceRoot, item)
		if itemErr != nil {
			return false, false, itemErr
		}
		expected = expected && itemExpected
		original = original && itemOriginal
	}
	return expected, original, nil
}

func reconcileFileResult(workspaceRoot string, item edit.FileResult) (expected, original bool, err error) {
	path, err := file.Resolve(workspaceRoot, item.Path)
	if err != nil {
		return false, false, err
	}
	switch item.Operation {
	case edit.OpCreate:
		current, exists, readErr := currentSHA(path)
		if readErr != nil {
			return false, false, readErr
		}
		return exists && current == item.NewSHA256, !exists, nil
	case edit.OpUpdate:
		current, exists, readErr := currentSHA(path)
		if readErr != nil {
			return false, false, readErr
		}
		return exists && current == item.NewSHA256, exists && current == item.OriginalSHA256, nil
	case edit.OpDelete:
		current, exists, readErr := currentSHA(path)
		if readErr != nil {
			return false, false, readErr
		}
		return !exists, exists && current == item.OriginalSHA256, nil
	case edit.OpRename:
		newPath, resolveErr := file.Resolve(workspaceRoot, item.NewPath)
		if resolveErr != nil {
			return false, false, resolveErr
		}
		oldSHA, oldExists, oldErr := currentSHA(path)
		if oldErr != nil {
			return false, false, oldErr
		}
		newSHA, newExists, newErr := currentSHA(newPath)
		if newErr != nil {
			return false, false, newErr
		}
		return !oldExists && newExists && newSHA == item.NewSHA256,
			oldExists && oldSHA == item.OriginalSHA256 && !newExists, nil
	default:
		return false, false, fmt.Errorf("unsupported reconcile operation %q", item.Operation)
	}
}

func currentSHA(path string) (string, bool, error) {
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:]), true, nil
}

func (r *Runtime) reconcilePendingEdit(ctx context.Context, envReq envelope.Request, session remotesession.Session, principalID string, claim idempotency.Claim, key idempotency.Key, fingerprint string) (*mcp.CallToolResult, bool) {
	stored, err := decodeStoredEdit(claim.Record.Response)
	if err != nil || stored.Error != nil || stored.EditID == "" {
		return nil, false
	}
	projectRoot := sessionProjectPath(session)
	expected, original, reconcileErr := reconcileEditResult(projectRoot, stored.Result)
	if reconcileErr != nil {
		return nil, false
	}
	if expected {
		recoveryFailure := func() (*mcp.CallToolResult, bool) {
			_ = r.idempotency.MarkInDoubt(ctx, key, fingerprint, claim.Record.Metadata)
			result, _ := r.editIdempotencyInDoubt(envReq, session, claim.Record)
			return result, true
		}
		for index := range stored.Result.Results {
			item := &stored.Result.Results[index]
			if item.Deleted {
				continue
			}
			path := item.Path
			if item.Operation == edit.OpRename {
				path = item.NewPath
			}
			absolute, err := file.Resolve(projectRoot, path)
			if err != nil {
				return recoveryFailure()
			}
			content, err := os.ReadFile(absolute)
			if err != nil {
				return recoveryFailure()
			}
			digest := sha256.Sum256(content)
			sha := "sha256:" + hex.EncodeToString(digest[:])
			if sha != item.NewSHA256 {
				return recoveryFailure()
			}
			tail := content
			if len(tail) > 16 {
				tail = tail[len(tail)-16:]
			}
			item.Readback = &edit.ByteReadback{ByteLength: len(content), SHA256: sha, Format: file.DetectFormat(content), TailHex: hex.EncodeToString(tail)}
		}
		if err := r.saveCleanEditRecord(ctx, session.ID, principalID, stored.EditID, "succeeded", stored.Result); err != nil {
			return recoveryFailure()
		}
		encoded, err := json.Marshal(stored)
		if err != nil {
			return recoveryFailure()
		}
		if err := r.idempotency.Complete(ctx, key, fingerprint, idempotency.StateSucceeded, encoded, claim.Record.Metadata); err != nil {
			return recoveryFailure()
		}
		result, _ := r.editToolSuccess(envReq, session, stored.EditID, stored.Result, true, nil)
		return result, true
	}
	if !original {
		_ = r.saveCleanEditRecord(ctx, session.ID, principalID, stored.EditID, "in_doubt", stored.Result)
		_ = r.idempotency.MarkInDoubt(ctx, key, fingerprint, claim.Record.Metadata)
		result, _ := r.editIdempotencyInDoubt(envReq, session, claim.Record)
		return result, true
	}
	return nil, false
}
