package server

import (
	"context"
	"database/sql"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/mcpresult"

	"mcpx/internal/envelope"
	"mcpx/internal/workspacechanges"
)

func (r *Runtime) toolWorkspaceChanges(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	envReq, _, session, fail := r.changeRequest(ctx, req, false)
	if fail != nil {
		return fail, nil
	}
	includeDiff, present := envReq.Payload["include_diff"].(bool)
	if !present {
		includeDiff = true
	}
	report, err := r.workspaceDiff.Inspect(ctx, session.ID, session.WorkspaceName, session.WorkspacePath, includeDiff)
	if err != nil {
		code := "workspace_changes_error"
		switch {
		case errors.Is(err, workspacechanges.ErrNotGitRepository):
			code = "not_git_repository"
		case errors.Is(err, sql.ErrNoRows):
			code = "baseline_not_found"
		}
		response := envelope.Fail(envelope.StatusError, envReq.RequestID, session.WorkspaceName, nil, code, err.Error())
		response.RemoteSessionID = session.ID
		return r.resultJSON(response)
	}
	fallback := report.UnifiedDiff
	if !report.GitAvailable {
		fallback = "当前 Workspace 不是 Git 仓库；Git 状态和 diff 不适用，但 source_read、change 等文件操作仍可用。"
	}
	if fallback == "" {
		fallback = "No tracked Git diff."
	} else {
		fallback = "```diff\n" + fallback + "```"
	}
	return mcpresult.NewStructured(report, fallback), nil
}
