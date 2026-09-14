package server

import (
	"context"
	"encoding/json"
	"fmt"
	"mcpx/internal/file"
	"mcpx/internal/workspace"
	"path/filepath"
	"strings"
)

func (r *Runtime) expectedExecutionWorkspace(ctx context.Context, sessionID, root string, payload map[string]any) (string, *workspace.GitIdentity, error) {
	raw, present := payload["expected_workspace"]
	if !present {
		if _, transition := payload["workspace_transition"]; transition {
			return "", nil, fmt.Errorf("workspace_transition requires expected_workspace")
		}
		return root, nil, nil
	}
	if _, ok := payload["argv"]; !ok {
		return "", nil, fmt.Errorf("expected_workspace requires structured argv execution")
	}
	spec, _, _, err := executeArgv(payload)
	if err != nil || spec == nil {
		return "", nil, fmt.Errorf("expected_workspace requires valid structured argv")
	}
	// Git 全局选项可通过 -C、--git-dir、-c 等覆盖刚验证的目标。
	// 绑定身份的 Git 调用直接从子命令开始，目录由 Runtime 提供。
	name := strings.ToLower(filepath.Base(spec.Executable))
	if (name == "git" || name == "git.exe") && (len(spec.Args) == 0 || strings.HasPrefix(spec.Args[0], "-")) {
		return "", nil, fmt.Errorf("workspace-bound Git invocation must start with a subcommand, without global overrides")
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return "", nil, fmt.Errorf("invalid expected_workspace")
	}
	var expected workspace.GitIdentity
	if err := json.Unmarshal(encoded, &expected); err != nil {
		return "", nil, fmt.Errorf("invalid expected_workspace")
	}
	if err := r.reconcileWorkspaceTransition(ctx, sessionID, expected.RunID); err != nil {
		return "", nil, err
	}
	if err := workspace.VerifyFrozenGitIdentity(ctx, r.state.DB(), sessionID, expected); err != nil {
		return "", nil, err
	}
	physicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", nil, fmt.Errorf("cannot resolve registered workspace")
	}
	physicalRoot, err = filepath.Abs(physicalRoot)
	if err != nil {
		return "", nil, err
	}
	rel, err := filepath.Rel(physicalRoot, expected.CanonicalPath)
	if err != nil {
		return "", nil, fmt.Errorf("expected workspace outside registered root")
	}
	resolved, err := file.Resolve(physicalRoot, rel)
	if err != nil || resolved != expected.CanonicalPath {
		return "", nil, fmt.Errorf("expected workspace path does not match physical target")
	}
	if err := workspace.VerifyGitIdentity(ctx, expected); err != nil {
		return "", nil, err
	}
	if _, err := validateWorkspaceTransition(ctx, payload, &expected); err != nil {
		return "", nil, err
	}
	return resolved, &expected, nil
}
