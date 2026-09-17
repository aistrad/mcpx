package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/envelope"
	"mcpx/internal/file"
	"mcpx/internal/security"
	"mcpx/internal/source"
	"mcpx/internal/winproc"
)

func (r *Runtime) toolProjectInspect(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	envReq, _, session, fail := r.changeRequest(ctx, req, false)
	if fail != nil {
		return fail, nil
	}
	data := inspectProject(ctx, session.WorkspacePath)
	data["agent_instructions"] = r.agentInstructions(session.WorkspacePath)
	return r.remoteResult(envReq, session.ID, session.WorkspaceName, data)
}

func (r *Runtime) sourcePathAllowed(workspacePath string) func(string) bool {
	rules := r.effectiveConfig(workspacePath).Security.Files
	return func(path string) bool { return security.MatchFile(rules, path) == security.Allow }
}

func (r *Runtime) sourceError(envReq envelope.Request, remoteSessionID, workspace string, err error) (*mcp.CallToolResult, error) {
	code := "source_error"
	message := strings.ReplaceAll(err.Error(), "statat ", "stat ")
	category := "internal"
	if errors.Is(err, os.ErrNotExist) || strings.Contains(strings.ToLower(err.Error()), "no such file") || strings.Contains(strings.ToLower(err.Error()), "not found") || strings.Contains(strings.ToLower(err.Error()), "does not exist") {
		code = "file_not_found"
		category = "not_found"
		message = "source path not found"
	}
	var sizeLimit *file.SizeLimitError
	var requestLimit *source.LimitError
	switch {
	case errors.As(err, &sizeLimit):
		code = "FILE_TOO_LARGE"
		category = "capacity"
		message = fmt.Sprintf("source file is %d bytes; full read maximum is %d bytes", sizeLimit.Actual, sizeLimit.Max)
	case errors.As(err, &requestLimit):
		code = "LIMIT_EXCEEDED"
		category = "validation"
		message = requestLimit.Error()
	}
	response := envelope.Fail(envelope.StatusError, envReq.RequestID, workspace, nil, code, message)
	response.RemoteSessionID = remoteSessionID
	if response.Error != nil {
		response.Error.Category = category
		if sizeLimit != nil {
			response.Error.Details["path"] = sizeLimit.Path
			response.Error.Details["actual_bytes"] = sizeLimit.Actual
			response.Error.Details["max_source_bytes"] = sizeLimit.Max
			response.Error.Details["recovery"] = map[string]any{
				"action":  "read_window",
				"message": "full read is capped; use a bounded window read instead",
			}
			response.Error.Recovery = &envelope.Recovery{Action: "file", Tool: "read", Arguments: map[string]any{
				"remote_session_id": remoteSessionID, "view": "file", "path": sizeLimit.Path,
				"mode": "window", "offset": 0, "limit": 120,
			}}
		}
		if requestLimit != nil {
			response.Error.Details["resource"] = requestLimit.Resource
			response.Error.Details["actual"] = requestLimit.Actual
			response.Error.Details["max"] = requestLimit.Max
			response.Error.Details["max_items"] = MaxReadItems
		}
	}
	errorText := strings.ToLower(err.Error())
	if strings.Contains(errorText, "no such file") || strings.Contains(errorText, "not found") || strings.Contains(errorText, "does not exist") {
		addRecoveryAction(&response, "context_query", "locate the file or directory before retrying the read", map[string]any{
			"remote_session_id": remoteSessionID,
			"action":            "list",
		})
	}
	return r.resultJSON(response)
}

func sourceReadItemError(err error, path, remoteSessionID string) map[string]any {
	code, category, retryable := "READ_FAILED", "runtime", false
	message := strings.ReplaceAll(err.Error(), "statat ", "stat ")
	details := map[string]any{"path": path}
	var sizeLimit *file.SizeLimitError
	var requestLimit *source.LimitError
	switch {
	case errors.As(err, &sizeLimit):
		code, category = "FILE_TOO_LARGE", "capacity"
		message = fmt.Sprintf("source file is %d bytes; full read maximum is %d bytes", sizeLimit.Actual, sizeLimit.Max)
		details["actual_bytes"] = sizeLimit.Actual
		details["max_source_bytes"] = sizeLimit.Max
		details["recovery"] = map[string]any{"action": "read_window", "message": "use mode=window to read a bounded range"}
		details["next_action"] = nextActionWithReason("read", "use a bounded window for this oversized source", map[string]any{
			"remote_session_id": remoteSessionID, "view": "file", "path": path,
			"mode": "window", "offset": 0, "limit": 120,
		})
	case errors.As(err, &requestLimit):
		code, category = "LIMIT_EXCEEDED", "validation"
		details["resource"] = requestLimit.Resource
		details["actual"] = requestLimit.Actual
		details["max"] = requestLimit.Max
	case errors.Is(err, os.ErrNotExist) || strings.Contains(strings.ToLower(err.Error()), "no such file"):
		code, category = "FILE_NOT_FOUND", "not_found"
		message = "source path not found"
		details["next_action"] = nextActionWithReason("read", "locate this path before retrying read", map[string]any{
			"remote_session_id": remoteSessionID, "view": "list",
		})
	}
	return map[string]any{"code": code, "message": message, "category": category, "retryable": retryable, "details": details}
}

func inspectProject(ctx context.Context, root string) map[string]any {
	manifestStacks := map[string]string{
		"go.mod": "go", "package.json": "node", "Cargo.toml": "rust", "pyproject.toml": "python",
		"requirements.txt": "python", "pom.xml": "java-maven", "build.gradle": "java-gradle",
		"composer.json": "php", "Gemfile": "ruby", "Makefile": "make",
	}
	var manifests, stacks []string
	for manifest, stack := range manifestStacks {
		if info, err := os.Stat(filepath.Join(root, manifest)); err == nil && info.Mode().IsRegular() {
			manifests = append(manifests, manifest)
			stacks = append(stacks, stack)
		}
	}
	sort.Strings(manifests)
	sort.Strings(stacks)
	var instructions []string
	for _, name := range []string{"AGENTS.md", "CLAUDE.md", "CODEX.md", "CONTRIBUTING.md", "README.md"} {
		if info, err := os.Stat(filepath.Join(root, name)); err == nil && info.Mode().IsRegular() {
			instructions = append(instructions, name)
		}
	}
	entryCandidates := []string{"main.go", "cmd", "src/main.go", "src/index.ts", "src/index.js", "app", "pages", "manage.py"}
	var entrypoints []string
	for _, name := range entryCandidates {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(name))); err == nil {
			entrypoints = append(entrypoints, name)
		}
	}
	tasks := map[string]string{}
	if content, err := os.ReadFile(filepath.Join(root, "package.json")); err == nil {
		var packageFile struct {
			Scripts map[string]string `json:"scripts"`
		}
		if json.Unmarshal(content, &packageFile) == nil {
			for name := range packageFile.Scripts {
				tasks[name] = "npm run " + name
			}
		}
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
		tasks["test"] = "go test ./..."
		tasks["build"] = "go build ./..."
	}
	gitStatus := boundedCommand(ctx, root, "git", "status", "--short", "--branch")
	return map[string]any{
		"stacks": stacks, "manifests": manifests, "entrypoints": entrypoints,
		"instructions": instructions, "tasks": tasks, "git_status": gitStatus,
	}
}

func boundedCommand(parent context.Context, workDir, name string, args ...string) string {
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, name, args...)
	winproc.ConfigureNoWindow(command)
	command.Dir = workDir
	output, err := command.CombinedOutput()
	if err != nil && len(output) == 0 {
		return ""
	}
	value := strings.TrimSpace(string(output))
	if len(value) > 20_000 {
		value = value[:20_000]
	}
	return value
}
