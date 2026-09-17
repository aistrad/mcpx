package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/envelope"
	"mcpx/internal/file"
	"mcpx/internal/plan"
	"mcpx/internal/remotesession"
)

const maxProgressResultItems = 12

// toolProgress records a model-authored, user-visible progress state.
// It does not mutate workspace files; the instrumented lifecycle persists
// semantic milestones and terminal states for observers and reconnects.
func (r *Runtime) toolProgress(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	envReq, _, session, fail := r.changeRequest(ctx, req, false)
	if fail != nil {
		return fail, nil
	}
	current, _ := envReq.Payload["current"].(string)
	current = strings.TrimSpace(current)
	if current == "" {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "bad_request", "progress current is required")
	}
	if len(current) > envelope.MaxIntentBytes {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "bad_request", fmt.Sprintf("progress current exceeds %d bytes", envelope.MaxIntentBytes))
	}
	rawResults := stringSlicePayload(envReq.Payload, "result")
	if len(rawResults) > maxProgressResultItems {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "bad_request", fmt.Sprintf("progress result supports at most %d items", maxProgressResultItems))
	}
	results := make([]string, 0, len(rawResults))
	resultBytes := 0
	for _, item := range rawResults {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		resultBytes += len(item)
		results = append(results, item)
	}
	if resultBytes > envelope.MaxResultSummaryBytes {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "bad_request", fmt.Sprintf("progress result exceeds %d bytes", envelope.MaxResultSummaryBytes))
	}
	next, _ := envReq.Payload["next"].(string)
	next = strings.TrimSpace(next)
	status, _ := envReq.Payload["status"].(string)
	status = strings.ToLower(strings.TrimSpace(status))
	if status == "" {
		status = "in_progress"
	}
	if !containsString([]string{"in_progress", "completed", "waiting_for_user", "blocked", "failed"}, status) {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "bad_request", fmt.Sprintf("unsupported progress status %q", status))
	}
	phase, _ := envReq.Payload["phase"].(string)
	phase = strings.TrimSpace(phase)
	taskScope, _ := envReq.Payload["task_scope"].(string)
	taskScope = strings.TrimSpace(taskScope)
	evidence, evidenceErr := decodeProgressEvidence(envReq.Payload["evidence"])
	if evidenceErr != nil {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "INVALID_ARGUMENT", evidenceErr.Error())
	}
	if status == "completed" && phase == "verification" {
		if taskScope == "" {
			return r.terminalError(envReq, session.ID, session.WorkspaceName, "VERIFICATION_SCOPE_REQUIRED", "completed verification progress requires task_scope")
		}
		if len(evidence) == 0 {
			return r.terminalError(envReq, session.ID, session.WorkspaceName, "VERIFICATION_REQUIRED", "completed verification progress requires at least one evidence item")
		}
	}
	if len(evidence) > 0 {
		if err := r.validateProgressEvidence(ctx, session, evidence); err != nil {
			return r.terminalError(envReq, session.ID, session.WorkspaceName, "VERIFICATION_INVALID", err.Error())
		}
	}
	relatedTool, _ := envReq.Payload["related_tool"].(string)
	relatedTool = strings.TrimSpace(relatedTool)

	display := current
	if len(results) > 0 {
		display += fmt.Sprintf(" · results: %d", len(results))
	}
	if next != "" {
		display += " · next: " + next
	}
	data := map[string]any{
		"phase":             phase,
		"current":           current,
		"result":            results,
		"status":            status,
		"next":              next,
		"task_scope":        taskScope,
		"evidence":          evidence,
		"related_tool":      relatedTool,
		"remote_session_id": session.ID,
		"workspace":         session.WorkspaceName,
		"workspace_binding": workspaceBindingData(session),
	}
	return compactToolResult(data, display), nil
}

func decodeProgressEvidence(value any) ([]plan.EvidenceInput, error) {
	if value == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("evidence must be an array of {kind, reference_id}")
	}
	var evidence []plan.EvidenceInput
	if err := json.Unmarshal(encoded, &evidence); err != nil {
		return nil, fmt.Errorf("evidence must be an array of {kind, reference_id}")
	}
	if len(evidence) > maxProgressResultItems {
		return nil, fmt.Errorf("evidence supports at most %d items", maxProgressResultItems)
	}
	for index := range evidence {
		evidence[index].Kind = strings.ToLower(strings.TrimSpace(evidence[index].Kind))
		evidence[index].ReferenceID = strings.TrimSpace(evidence[index].ReferenceID)
		if !plan.IsEvidenceKind(evidence[index].Kind) || evidence[index].ReferenceID == "" {
			return nil, fmt.Errorf("evidence[%d] requires a supported kind and non-empty reference_id", index)
		}
	}
	return evidence, nil
}

func (r *Runtime) validateProgressEvidence(ctx context.Context, session remotesession.Session, evidence []plan.EvidenceInput) error {
	if r == nil || r.state == nil || r.state.DB() == nil {
		return fmt.Errorf("verification state is unavailable")
	}
	projectRoot := sessionProjectPath(session)
	for _, item := range evidence {
		switch item.Kind {
		case plan.EvidenceEdit:
			var state string
			if err := r.state.DB().QueryRowContext(ctx, `SELECT state FROM clean_edit_records WHERE remote_session_id=? AND id=?`, session.ID, item.ReferenceID).Scan(&state); err != nil {
				return fmt.Errorf("edit evidence %s is not available: %w", item.ReferenceID, err)
			}
			if state != "succeeded" {
				return fmt.Errorf("edit evidence %s is not succeeded (state=%s)", item.ReferenceID, state)
			}
		case plan.EvidenceExecute:
			var status string
			var exitCode sql.NullInt64
			if err := r.state.DB().QueryRowContext(ctx, `SELECT status, exit_code FROM terminal_tasks WHERE remote_session_id=? AND id=?`, session.ID, item.ReferenceID).Scan(&status, &exitCode); err != nil {
				return fmt.Errorf("execute evidence %s is not available: %w", item.ReferenceID, err)
			}
			if status != "exited" || !exitCode.Valid || exitCode.Int64 != 0 {
				return fmt.Errorf("execute evidence %s is not a successful exit (status=%s exit_code=%s)", item.ReferenceID, status, nullIntString(exitCode))
			}
		case plan.EvidenceArtifact:
			var id string
			if err := r.state.DB().QueryRowContext(ctx, `SELECT id FROM artifacts WHERE remote_session_id=? AND id=?`, session.ID, item.ReferenceID).Scan(&id); err != nil {
				return fmt.Errorf("artifact evidence %s is not available: %w", item.ReferenceID, err)
			}
		case plan.EvidenceSource:
			resolved, err := file.Resolve(projectRoot, item.ReferenceID)
			if err != nil {
				return fmt.Errorf("source evidence %s is out of scope: %w", item.ReferenceID, err)
			}
			info, err := os.Stat(resolved)
			if err != nil || !info.Mode().IsRegular() {
				return fmt.Errorf("source evidence %s is not a regular file", item.ReferenceID)
			}
		case plan.EvidenceVerification:
			if evidenceFailedMetadata(item.Metadata) {
				return fmt.Errorf("verification evidence %s reports failure", item.ReferenceID)
			}
		case plan.EvidenceRead, plan.EvidenceObserve:
			sequence, err := strconv.ParseInt(item.ReferenceID, 10, 64)
			if err != nil || sequence <= 0 {
				return fmt.Errorf("%s evidence %s must reference a positive observation sequence", item.Kind, item.ReferenceID)
			}
			var eventType, status string
			if err := r.state.DB().QueryRowContext(ctx, `SELECT event_type,status FROM observation_events WHERE remote_session_id=? AND sequence=?`, session.ID, sequence).Scan(&eventType, &status); err != nil {
				return fmt.Errorf("%s evidence %s is not available: %w", item.Kind, item.ReferenceID, err)
			}
			if eventType != "tool.completed" && eventType != "operation.completed" && eventType != "operation.step.completed" {
				return fmt.Errorf("%s evidence %s is not a completed event", item.Kind, item.ReferenceID)
			}
			if !containsString([]string{"ok", "succeeded", "completed", "exited", "success"}, strings.ToLower(strings.TrimSpace(status))) {
				return fmt.Errorf("%s evidence %s is not successful (status=%s)", item.Kind, item.ReferenceID, status)
			}
		}
	}
	return nil
}

func evidenceFailedMetadata(metadata map[string]any) bool {
	for _, key := range []string{"status", "result", "outcome"} {
		value, _ := metadata[key].(string)
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "failed" || value == "failure" || value == "error" {
			return true
		}
	}
	if value, ok := metadata["passed"].(bool); ok && !value {
		return true
	}
	if value, ok := metadata["success"].(bool); ok && !value {
		return true
	}
	return false
}

func nullIntString(value sql.NullInt64) string {
	if !value.Valid {
		return "null"
	}
	return strconv.FormatInt(value.Int64, 10)
}
