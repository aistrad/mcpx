package server

import (
	"context"
	"fmt"
	"strings"

	"mcpx/internal/auth"
	"mcpx/internal/envelope"
	"mcpx/internal/remotesession"
	"mcpx/internal/workspace"
)

// resolveExplicitWorkspace resolves only caller-provided state. It never reads
// or mutates an MCP transport session.
func (r *Runtime) resolveExplicitWorkspace(ctx context.Context, principal auth.Principal, req envelope.Request) (workspace.Workspace, string, error) {
	name := strings.TrimSpace(req.Workspace)
	if name == "" {
		name, _ = req.Payload["workspace"].(string)
		name = strings.TrimSpace(name)
	}
	remoteID := remoteSessionID(req)
	if remoteID != "" {
		session, err := r.remote.Get(ctx, principal, remoteID)
		if err != nil {
			return workspace.Workspace{}, remoteID, err
		}
		if name != "" && name != session.WorkspaceName {
			return workspace.Workspace{}, remoteID, fmt.Errorf("%w: workspace does not match Remote Session", remotesession.ErrInvalidInput)
		}
		registered, ok := r.reg.Get(session.WorkspaceName)
		if ok {
			registered.Path = session.WorkspacePath
			return registered, remoteID, nil
		}
		return workspace.Workspace{
			ID: session.WorkspaceName, Name: session.WorkspaceName, Path: session.WorkspacePath,
		}, remoteID, nil
	}
	if name == "" {
		return workspace.Workspace{}, "", nil
	}
	registered, ok := r.reg.Get(name)
	if !ok {
		return workspace.Workspace{}, "", fmt.Errorf("%w: %q", errWorkspaceNotFound, name)
	}
	return registered, "", nil
}
