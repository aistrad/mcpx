package server

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/audit"
	workspacefile "mcpx/internal/file"
	"mcpx/internal/instruction"
	"mcpx/internal/observation"
	"mcpx/internal/projecttask"
	"mcpx/internal/remotesession"
	"mcpx/internal/skill"
	buildversion "mcpx/internal/version"
	workspaceidentity "mcpx/internal/workspace"
)

// toolSessionOpen creates or reuses a Remote Session and returns a full bootstrap bundle
// so clients need only one MCP call to start developing.
func (r *Runtime) toolSessionOpen(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	envReq, principal, fail := r.remoteRequest(ctx, req)
	if fail != nil {
		return fail, nil
	}

	includeInstrContent := false
	if v, ok := envReq.Payload["include_instructions_content"].(bool); ok {
		includeInstrContent = v
	}
	includeProjectTasks := false
	if v, ok := envReq.Payload["include_project_tasks"].(bool); ok {
		includeProjectTasks = v
	}
	var session remotesession.Session
	remoteID, _ := envReq.Payload["remote_session_id"].(string)
	remoteID = strings.TrimSpace(remoteID)
	if remoteID == "" {
		remoteID = strings.TrimSpace(envReq.RemoteSessionID)
	}

	workspaceName := strings.TrimSpace(envReq.Workspace)
	if workspaceName == "" {
		workspaceName, _ = envReq.Payload["workspace"].(string)
	}
	explicitProjectRoot := stringPayload(envReq.Payload, "project_root")
	projectRootInput := explicitProjectRoot
	legacyIdentityPath := stringPayload(envReq.Payload, "git_identity_path")
	if remoteID == "" && projectRootInput == "" {
		projectRootInput = legacyIdentityPath
	}
	if remoteID != "" {
		existing, err := r.remote.Get(ctx, principal, remoteID)
		if err != nil {
			return r.remoteError(envReq, remoteID, workspaceName, err)
		}
		session = existing
		workspaceName = session.WorkspaceName
		registered, ok := r.reg.Get(workspaceName)
		if !ok {
			return r.remoteError(envReq, remoteID, workspaceName, fmt.Errorf("%w: %q", errWorkspaceNotFound, workspaceName))
		}
		selected := sessionProjectPath(session)
		if projectRootInput != "" {
			resolved, resolveErr := resolveProjectRoot(ctx, registered.Path, projectRootInput, registered.ProjectRootRequired)
			if resolveErr != nil {
				return r.terminalError(envReq, session.ID, session.WorkspaceName, "PROJECT_ROOT_INVALID", resolveErr.Error())
			}
			if explicitProjectRoot != "" && legacyIdentityPath != "" {
				legacyResolved, legacyErr := resolveProjectRoot(ctx, registered.Path, legacyIdentityPath, registered.ProjectRootRequired)
				if legacyErr != nil {
					return r.terminalError(envReq, session.ID, session.WorkspaceName, "PROJECT_ROOT_INVALID", legacyErr.Error())
				}
				if filepath.Clean(legacyResolved) != filepath.Clean(resolved) {
					return r.terminalError(envReq, session.ID, session.WorkspaceName, "PROJECT_ROOT_MISMATCH", "project_root and git_identity_path must select the same project root")
				}
			}
			if filepath.Clean(resolved) != filepath.Clean(selected) {
				return r.terminalError(envReq, session.ID, session.WorkspaceName, "PROJECT_ROOT_MISMATCH", "project_root does not match the Remote Session binding")
			}
		}
	} else {
		registered, ok := r.reg.Get(strings.TrimSpace(workspaceName))
		if !ok {
			return r.remoteError(envReq, "", workspaceName, fmt.Errorf("%w: %q", errWorkspaceNotFound, workspaceName))
		}
		projectPath, resolveErr := resolveProjectRoot(ctx, registered.Path, projectRootInput, registered.ProjectRootRequired)
		if resolveErr != nil {
			if errors.Is(resolveErr, errProjectRootRequired) {
				return r.projectRootRequiredError(envReq, "", workspaceName, registered.Path)
			}
			return r.terminalError(envReq, "", workspaceName, projectRootErrorCode(resolveErr), resolveErr.Error())
		}
		if explicitProjectRoot != "" && legacyIdentityPath != "" {
			legacyResolved, legacyErr := resolveProjectRoot(ctx, registered.Path, legacyIdentityPath, registered.ProjectRootRequired)
			if legacyErr != nil {
				return r.terminalError(envReq, "", workspaceName, "PROJECT_ROOT_INVALID", legacyErr.Error())
			}
			if filepath.Clean(legacyResolved) != filepath.Clean(projectPath) {
				return r.terminalError(envReq, "", workspaceName, "PROJECT_ROOT_MISMATCH", "project_root and git_identity_path must select the same project root")
			}
		}
		created, err := r.createRemoteSession(ctx, principal, envReq, workspaceName, projectPath)
		if err != nil {
			return r.remoteError(envReq, "", workspaceName, err)
		}
		session = created.Session
	}

	wsPath := sessionProjectPath(session)
	var gitIdentity *workspaceidentity.GitIdentity
	if runID := stringPayload(envReq.Payload, "run_id"); runID != "" || legacyIdentityPath != "" {
		if err := r.reconcileWorkspaceTransition(ctx, session.ID, runID); err != nil {
			return r.terminalError(envReq, session.ID, session.WorkspaceName, "WORKSPACE_TRANSITION_UNVERIFIED", err.Error())
		}
		identityBase := wsPath
		identityPath := "."
		if remoteID != "" && stringPayload(envReq.Payload, "project_root") == "" && legacyIdentityPath != "" {
			// Existing clients used git_identity_path to inspect a nested Git
			// root without changing the Session's project scope.
			identityBase = session.WorkspacePath
			identityPath = legacyIdentityPath
		}
		resolved, err := workspacefile.Resolve(identityBase, identityPath)
		if err != nil {
			return r.terminalError(envReq, session.ID, session.WorkspaceName, "WORKSPACE_IDENTITY_UNAVAILABLE", "identity target must be inside registered workspace")
		}
		remoteName := stringPayload(envReq.Payload, "remote_name")
		if remoteName == "" {
			remoteName = "origin"
		}
		identity, err := workspaceidentity.CaptureGitIdentity(ctx, resolved, remoteName, runID)
		if err != nil {
			return r.terminalError(envReq, session.ID, session.WorkspaceName, "WORKSPACE_IDENTITY_UNAVAILABLE", err.Error())
		}
		if err := workspaceidentity.FreezeGitIdentity(ctx, r.state.DB(), session.ID, identity); err != nil {
			return r.terminalError(envReq, session.ID, session.WorkspaceName, "WORKSPACE_IDENTITY_MISMATCH", err.Error())
		}
		gitIdentity = &identity
	}
	effective := r.effectiveConfig(wsPath)
	tools := r.runtimeToolCapabilities(effective, &session)

	var (
		servers              = []map[string]any{}
		skills               = []map[string]any{}
		docs                 []instruction.Document
		project              map[string]any
		gitHead              string
		treeDigest           string
		pendingConfirmations []map[string]any
		taskList             any
		artifacts            any
		latestModelState     any
	)
	var tasks any
	var bootstrap sync.WaitGroup
	bootstrap.Add(7)
	go func() {
		defer bootstrap.Done()
		if manager, err := r.mcpManagerForWorkspace(wsPath); err == nil && effective.Discovery.MCP.Enabled {
			servers = manager.List()
		}
	}()
	go func() {
		defer bootstrap.Done()
		if effective.Discovery.Skills.Enabled {
			skills = skillItems(skill.LoadAll(effective.Discovery.Skills.Dirs, wsPath))
		}
	}()
	go func() {
		defer bootstrap.Done()
		docs = instruction.DiscoverAt(
			r.cfg.Discovery.Instructions.GlobalAgentsPath, wsPath, "",
			effective.Security.Files.MaxReadBytes,
		)
	}()
	go func() {
		defer bootstrap.Done()
		project = inspectProject(ctx, wsPath)
		if includeProjectTasks {
			tasks = projecttask.Discover(wsPath)
		}
	}()
	go func() {
		defer bootstrap.Done()
		gitHead, treeDigest = workspaceRevision(ctx, wsPath)
	}()
	go func() {
		defer bootstrap.Done()
		pendingConfirmations = pendingConfirmationItems(r.approvals.ListRemoteSession(session.ID))
		taskList, _ = r.tasks.List(session.ID, 20)
		artifacts, _ = r.artifacts.List(ctx, session.ID, "", 20)
	}()
	go func() {
		defer bootstrap.Done()
		if r.observation == nil || r.observation.store == nil {
			return
		}
		page, err := r.observation.store.QueryMemory(ctx, observation.MemoryQuery{
			Workspace: session.WorkspaceName,
			SessionID: session.ID,
			Type:      "progress",
			Latest:    1,
		})
		if err == nil && len(page.Items) > 0 {
			latestModelState = page.Items[0]
		}
	}()
	bootstrap.Wait()

	var instructionPayload any
	if includeInstrContent {
		items, _ := instruction.ReadContents(docs, 256<<10)
		instructionPayload = map[string]any{"documents": items, "inline": true}
	} else {
		instructionPayload = map[string]any{"documents": docs, "inline": false}
	}
	toolManifest := r.registeredToolManifest()
	build := r.build
	if build.Version == "" {
		build.Version = buildversion.Current
	}

	guidance := agentGuidance()
	clientProtocol := clientProtocolCapabilities()
	revisions := map[string]any{
		"tool_schema_revision":         r.currentToolSchemaRevision(),
		"capability_manifest_revision": capabilityManifestRevision(toolManifest, skills, servers, docs, guidance, clientProtocol),
		"guidance_revision":            agentGuidanceRevision(),
		"instruction_revision":         instructionRevision(docs),
		"session_capability_revision":  sessionCapabilityRevision(&session),
		"client_protocol_revision":     clientProtocolRevision(),
	}

	data := map[string]any{
		"remote_session_id": session.ID,
		"mcpx": map[string]any{
			"version": build.Version, "commit": build.Commit, "build_time": build.Date,
		},
		"remote_session": map[string]any{
			"id": session.ID, "role": session.Role, "status": session.Status,
			"version": session.Version, "label": session.Label, "description": session.Description,
			"workspace_name": session.WorkspaceName, "workspace_path": session.WorkspacePath,
			"project_path":  wsPath,
			"project_bound": session.ProjectBound,
			"approval_mode": r.workspaceApprovalMode(session.WorkspaceName),
		},
		"workspace": map[string]any{
			"name": session.WorkspaceName, "path": session.WorkspacePath,
			"registered_root": session.WorkspacePath, "project_root": wsPath,
			"binding_state": map[bool]string{true: "bound", false: "legacy_unbound"}[session.ProjectBound],
			"git_head":      gitHead, "tree_digest": treeDigest,
			"approval_mode": r.workspaceApprovalMode(session.WorkspaceName),
		},
		"revisions":       revisions,
		"agent_guidance":  guidance,
		"client_protocol": clientProtocol,
		"tools":           tools,
		"extension_inventory": map[string]any{
			"skills":      compactSkillMaps(skills),
			"mcp_servers": compactMCPServerInventory(servers),
		},
		"instructions":  instructionPayload,
		"project":       project,
		"project_tasks": tasks,
		"git": map[string]any{
			"head": gitHead, "tree_digest": treeDigest,
		},
		"pending_confirmations": pendingConfirmations,
		"tasks":                 taskList,
		"artifacts":             artifacts,
		"schema_source":         "tools/list",
		"capability_version":    cleanCoreCapabilityVersion,
		"capability_groups":     capabilityGroups(),
		"recommended_workflows": map[string]any{
			"bootstrap":      []string{"workspace", "session"},
			"source_change":  []string{"read", "edit", "execute", "observe"},
			"plan_delivery":  []string{"plan", "edit", "execute", "artifact", "observe"},
			"extension_call": []string{"skill_tool", "mcp_tool"},
		},
		"opened_at": time.Now().UTC().Format(time.RFC3339),
	}
	if latestModelState != nil {
		data["latest_model_state"] = latestModelState
	}
	if gitIdentity != nil {
		data["git_identity"] = gitIdentity
	}

	r.logAudit(audit.Event{
		RequestID: envReq.RequestID, RemoteSessionID: session.ID, Workspace: session.WorkspaceName,
		Tool: "session", Status: "ok",
	})
	return compactToolResult(data, fmt.Sprintf("Session %s opened for workspace %s.", session.ID, session.WorkspaceName)), nil
}
