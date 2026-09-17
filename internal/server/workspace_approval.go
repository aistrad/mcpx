package server

import (
	"mcpx/internal/config"
)

func (r *Runtime) workspaceApprovalMode(name string) string {
	if r == nil || r.reg == nil {
		return ""
	}
	workspace, ok := r.reg.Get(name)
	if !ok {
		return ""
	}
	return workspace.ApprovalMode
}

func (r *Runtime) externalManualAutoContinue(name string) bool {
	return r.workspaceApprovalMode(name) == config.WorkspaceApprovalModeExternalManualAutoContinue
}
