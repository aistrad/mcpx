package server

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	workspacefile "mcpx/internal/file"
	"mcpx/internal/remotesession"
	"mcpx/internal/winproc"
)

var (
	errProjectRootRequired = errors.New("project root is required for Git workspaces")
	errProjectRootInvalid  = errors.New("project root must be an in-scope directory and its own Git root")
)

const projectRootCandidateLimit = 16

// sessionProjectPath is the single project-scoped root used by source,
// edit, command, task, artifact and extension operations. WorkspacePath is
// deliberately kept as the registered boundary so the two meanings cannot
// silently drift.
func sessionProjectPath(session remotesession.Session) string {
	path := strings.TrimSpace(session.ProjectPath)
	if path == "" {
		path = session.WorkspacePath
	}
	return filepath.Clean(path)
}

func workspaceBindingData(session remotesession.Session) map[string]any {
	state := "legacy_unbound"
	if session.ProjectBound {
		state = "bound"
	}
	return map[string]any{
		"registered_root": session.WorkspacePath,
		"project_root":    sessionProjectPath(session),
		"project_bound":   session.ProjectBound,
		"binding_state":   state,
	}
}

// resolveProjectRoot validates only the caller-selected path. It deliberately
// does not walk a Workspace looking for repositories: an explicit path is the
// source of truth, and a parent repository must never be accepted implicitly.
func resolveProjectRoot(parent context.Context, registeredRoot, raw string, requireExplicit ...bool) (string, error) {
	registeredRoot, err := filepath.Abs(registeredRoot)
	if err != nil {
		return "", err
	}
	registeredRoot = filepath.Clean(registeredRoot)
	raw = strings.TrimSpace(raw)
	rootOnlyForbidden := len(requireExplicit) > 0 && requireExplicit[0]
	if raw == "" {
		if rootOnlyForbidden || gitTop(parent, registeredRoot) != "" {
			return "", errProjectRootRequired
		}
		return registeredRoot, nil
	}
	if rootOnlyForbidden && raw == "." {
		return "", errProjectRootRequired
	}
	if filepath.IsAbs(raw) || hasParentComponent(raw) {
		return "", errProjectRootInvalid
	}
	resolved, err := workspacefile.Resolve(registeredRoot, raw)
	if err != nil {
		return "", errProjectRootInvalid
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", errProjectRootInvalid
	}
	if top := gitTop(parent, resolved); top != "" {
		physical, physicalErr := filepath.EvalSymlinks(resolved)
		if physicalErr != nil || filepath.Clean(top) != filepath.Clean(physical) {
			return "", errProjectRootInvalid
		}
	}
	return filepath.Clean(resolved), nil
}

func (r *Runtime) validateSessionProjectRoot(session remotesession.Session) error {
	registeredRoot := filepath.Clean(session.WorkspacePath)
	projectRoot := sessionProjectPath(session)
	if registeredRoot == "" || projectRoot == "" {
		return errProjectRootInvalid
	}
	if info, err := os.Stat(registeredRoot); err != nil || !info.IsDir() {
		return errProjectRootInvalid
	}
	if info, err := os.Stat(projectRoot); err != nil || !info.IsDir() {
		return errProjectRootInvalid
	}
	registeredPhysical, err := filepath.EvalSymlinks(registeredRoot)
	if err != nil {
		registeredPhysical, err = filepath.Abs(registeredRoot)
		if err != nil {
			return errProjectRootInvalid
		}
	}
	projectPhysical, err := filepath.EvalSymlinks(projectRoot)
	if err != nil {
		projectPhysical, err = filepath.Abs(projectRoot)
		if err != nil {
			return errProjectRootInvalid
		}
	}
	rel, err := filepath.Rel(filepath.Clean(registeredPhysical), filepath.Clean(projectPhysical))
	if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errProjectRootInvalid
	}
	if top := gitTop(context.Background(), projectPhysical); top != "" && filepath.Clean(top) != filepath.Clean(projectPhysical) {
		return errProjectRootInvalid
	}
	return nil
}

func projectRootErrorCode(err error) string {
	if errors.Is(err, errProjectRootRequired) {
		return "PROJECT_ROOT_REQUIRED"
	}
	return "PROJECT_ROOT_INVALID"
}

// projectRootCandidates provides a small navigation hint when a caller
// omitted project_root. It is intentionally a bounded, direct-child listing;
// it never decides or auto-selects a repository and therefore cannot turn an
// incomplete discovery into an authorization decision.
func projectRootCandidates(registeredRoot string) []string {
	directory, err := os.Open(registeredRoot)
	if err != nil {
		return []string{}
	}
	defer directory.Close()
	entries, err := directory.ReadDir(projectRootCandidateLimit + 1)
	if err != nil {
		return []string{}
	}
	limit := len(entries)
	if limit > projectRootCandidateLimit {
		limit = projectRootCandidateLimit
	}
	candidates := make([]string, 0, limit)
	for _, entry := range entries[:limit] {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		candidates = append(candidates, entry.Name())
	}
	return candidates
}

func hasParentComponent(value string) bool {
	for _, part := range strings.FieldsFunc(filepath.ToSlash(value), func(r rune) bool { return r == '/' }) {
		if part == ".." {
			return true
		}
	}
	return false
}

func gitTop(parent context.Context, path string) string {
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", path, "rev-parse", "--show-toplevel")
	cmd.Env = gitCleanEnvironment()
	winproc.ConfigureNoWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	top, err := filepath.EvalSymlinks(strings.TrimSpace(string(out)))
	if err != nil {
		return ""
	}
	return filepath.Clean(top)
}

func gitCleanEnvironment() []string {
	result := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(strings.ToUpper(name), "GIT_") {
			continue
		}
		result = append(result, entry)
	}
	return result
}
