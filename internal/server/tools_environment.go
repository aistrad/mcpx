package server

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/auth"
	"mcpx/internal/envelope"
	"mcpx/internal/environment"
	"mcpx/internal/remotesession"
)

func (r *Runtime) toolEnvironmentInspect(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	envReq, principal, fail := r.remoteRequest(ctx, req)
	if fail != nil {
		return fail, nil
	}
	remoteSessionID := remoteSessionID(envReq)
	workspaceName := strings.TrimSpace(envReq.Workspace)
	if workspaceName == "" {
		workspaceName, _ = envReq.Payload["workspace"].(string)
	}
	workspacePath := ""
	if remoteSessionID != "" {
		session, err := r.remote.Get(ctx, principal, remoteSessionID)
		if err != nil {
			return r.remoteError(envReq, remoteSessionID, workspaceName, err)
		}
		if workspaceName != "" && workspaceName != session.WorkspaceName {
			return r.environmentError(envReq, remoteSessionID, workspaceName, fmt.Errorf("workspace does not match Remote Session"), "invalid_request")
		}
		workspaceName, workspacePath = session.WorkspaceName, session.WorkspacePath
	} else if workspaceName != "" {
		workspace, ok := r.reg.Get(workspaceName)
		if !ok {
			return r.remoteError(envReq, "", workspaceName, fmt.Errorf("%w: %q", errWorkspaceNotFound, workspaceName))
		}
		workspacePath = workspace.Path
	}

	sections, err := environmentSections(envReq.Payload["sections"])
	if err != nil {
		return r.environmentError(envReq, remoteSessionID, workspaceName, err, "invalid_request")
	}
	// Persisted snapshots are complete even when the caller requests a subset.
	fullReport := environment.Inspect(ctx, workspacePath, nil)
	report := fullReport
	if len(sections) > 0 {
		report = environment.SelectSections(fullReport, sections)
	}

	compareTo, _ := envReq.Payload["compare_to"].(string)
	if compareTo = strings.TrimSpace(compareTo); compareTo != "" {
		base, err := r.environment.Get(ctx, compareTo)
		if err != nil {
			code := "environment_snapshot_error"
			if errors.Is(err, environment.ErrSnapshotNotFound) {
				code = "environment_snapshot_not_found"
			}
			return r.environmentError(envReq, remoteSessionID, workspaceName, err, code)
		}
		if base.RemoteSessionID != "" {
			if _, err := r.remote.Get(ctx, principal, base.RemoteSessionID); err != nil {
				return r.remoteError(envReq, base.RemoteSessionID, workspaceName, err)
			}
		}
		comparison := environment.Compare(base.ID, base.Report, fullReport)
		report.Comparison = &comparison
	}

	saveSnapshot := remoteSessionID != ""
	if explicit, ok := envReq.Payload["save_snapshot"].(bool); ok {
		saveSnapshot = explicit
	}
	if saveSnapshot && remoteSessionID == "" {
		return r.environmentError(envReq, "", workspaceName, fmt.Errorf("remote_session_id is required to save a snapshot"), "remote_session_required")
	}
	if saveSnapshot {
		snapshot, err := r.environment.Save(ctx, remoteSessionID, fullReport)
		if err != nil {
			return r.environmentError(envReq, remoteSessionID, workspaceName, err, "environment_snapshot_error")
		}
		if err := r.remote.SetEnvironmentSnapshot(ctx, principal, remoteSessionID, snapshot.ID); err != nil {
			return r.remoteError(envReq, remoteSessionID, workspaceName, err)
		}
		report.SnapshotID = snapshot.ID
	}

	return r.remoteResult(envReq, remoteSessionID, workspaceName, report)
}

func (r *Runtime) ensureSessionEnvironment(ctx context.Context, principal auth.Principal, result *remotesession.CreateResult) error {
	current, err := r.remote.Get(ctx, principal, result.Session.ID)
	if err != nil {
		return err
	}
	if current.EnvironmentSnapshotID != "" {
		snapshot, err := r.environment.Get(ctx, current.EnvironmentSnapshotID)
		if err != nil {
			return err
		}
		result.Session = current
		result.EnvironmentSnapshotID = snapshot.ID
		result.EnvironmentStaticDigest = snapshot.StaticDigest
		return nil
	}
	report := environment.Inspect(ctx, current.WorkspacePath, nil)
	snapshot, err := r.environment.Save(ctx, current.ID, report)
	if err != nil {
		return err
	}
	if err := r.remote.SetEnvironmentSnapshot(ctx, principal, current.ID, snapshot.ID); err != nil {
		return err
	}
	updated, err := r.remote.Get(ctx, principal, current.ID)
	if err != nil {
		return err
	}
	result.Session = updated
	result.EnvironmentSnapshotID = snapshot.ID
	result.EnvironmentStaticDigest = snapshot.StaticDigest
	return nil
}

func environmentSections(value any) ([]string, error) {
	var sections []string
	switch typed := value.(type) {
	case nil:
		return nil, nil
	case string:
		sections = strings.Split(typed, ",")
	case []string:
		sections = append(sections, typed...)
	case []any:
		for _, item := range typed {
			section, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("sections must contain strings")
			}
			sections = append(sections, section)
		}
	default:
		return nil, fmt.Errorf("sections must be an array or comma-separated string")
	}
	valid := map[string]bool{}
	for _, section := range environment.ValidSections {
		valid[section] = true
	}
	result := make([]string, 0, len(sections))
	seen := map[string]bool{}
	for _, section := range sections {
		section = strings.TrimSpace(section)
		if !valid[section] {
			return nil, fmt.Errorf("unknown environment section %q", section)
		}
		if !seen[section] {
			seen[section] = true
			result = append(result, section)
		}
	}
	return result, nil
}

func (r *Runtime) environmentError(envReq envelope.Request, remoteSessionID, workspace string, err error, code string) (*mcp.CallToolResult, error) {
	response := envelope.Fail(envelope.StatusError, envReq.RequestID, workspace, nil, code, err.Error())
	response.RemoteSessionID = remoteSessionID
	return r.resultJSON(response)
}
