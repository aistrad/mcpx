package workspace

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"mcpx/internal/config"
)

// Registry holds workspaces from global config.
type Registry struct {
	mu     sync.RWMutex
	byName map[string]Workspace
	order  []string
}

// NewRegistry builds from config entries.
func NewRegistry(entries []config.WorkspaceEntry) (*Registry, error) {
	r := &Registry{byName: map[string]Workspace{}}
	for _, e := range entries {
		if e.Name == "" || e.Path == "" {
			continue
		}
		abs, err := filepath.Abs(e.Path)
		if err != nil {
			return nil, err
		}
		approvalMode := strings.TrimSpace(e.ApprovalMode)
		if approvalMode != "" && approvalMode != config.WorkspaceApprovalModeExternalManualAutoContinue {
			return nil, fmt.Errorf("workspace %q has unsupported approval_mode %q", e.Name, e.ApprovalMode)
		}
		ws := Workspace{
			ID:                  e.Name,
			Name:                e.Name,
			Path:                abs,
			Description:         e.Description,
			ApprovalMode:        approvalMode,
			ProjectRootRequired: e.ProjectRootRequired,
		}
		r.byName[e.Name] = ws
		r.order = append(r.order, e.Name)
	}
	return r, nil
}

// List returns workspaces in registration order.
func (r *Registry) List() []Workspace {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Workspace, 0, len(r.order))
	for _, n := range r.order {
		if ws, ok := r.byName[n]; ok {
			out = append(out, ws)
		}
	}
	return out
}

// Get returns workspace by name.
func (r *Registry) Get(name string) (Workspace, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ws, ok := r.byName[name]
	return ws, ok
}

// Info returns workspace plus optional description override from project config.
func (r *Registry) Info(name string, descOverride string) (map[string]any, error) {
	ws, ok := r.Get(name)
	if !ok {
		return nil, fmt.Errorf("workspace %q not found", name)
	}
	desc := ws.Description
	if descOverride != "" {
		desc = descOverride
	}
	return map[string]any{
		"name":        ws.Name,
		"path":        ws.Path,
		"description": desc,
	}, nil
}
