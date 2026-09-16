package skill

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"mcpx/internal/config"
	"mcpx/internal/terminal"
)

const NativeProtocol = "json_stdio_v1"
const NativeWireProtocol = "mcpx_native_skill.v1"
const NativeMaxInputBytes = 8 << 20
const NativeMaxOutputBytes = 1 << 20

type NativeManifest struct {
	Protocol       string `yaml:"protocol" json:"protocol"`
	Entry          string `yaml:"entry" json:"entry"`
	TimeoutSeconds int    `yaml:"timeout_seconds" json:"timeout_seconds"`
}

type NativeRuntimeContext struct {
	RemoteSessionID string `json:"remote_session_id"`
	Workspace       string `json:"workspace"`
	WorkspacePath   string `json:"workspace_path"`
	RequestID       string `json:"request_id"`
	IdempotencyKey  string `json:"idempotency_key"`
}

// LoadConfigured preserves ordinary discovery and adds only exact approved native
// directories. A workspace cannot turn a discovered instruction into executable code.
func LoadConfigured(dirs, nativeDirs []string, workspacePath string) []Skill {
	result := LoadAll(dirs, workspacePath)
	for _, configured := range nativeDirs {
		path := config.ExpandHome(configured)
		if !filepath.IsAbs(path) {
			continue
		}
		root, err := filepath.EvalSymlinks(path)
		if err != nil {
			continue
		}
		manifest, ok := loadManifest(root, filepath.Base(root))
		if !ok || manifest.MCPX == nil {
			continue
		}
		candidate := Skill{Manifest: manifest, Dir: root, Source: filepath.Dir(root), NativeApproved: true}
		if _, err := ResolveNativeEntry(candidate); err != nil {
			continue
		}
		found := false
		for i, current := range result {
			if current.Manifest.Name != candidate.Manifest.Name {
				continue
			}
			found = true
			currentRoot, err := filepath.EvalSymlinks(current.Dir)
			// Name collisions never elevate an unapproved package.
			if err == nil && currentRoot == root {
				result[i] = candidate
			}
			break
		}
		if !found {
			result = append(result, candidate)
		}
	}
	return result
}

func ResolveNativeEntry(sk Skill) (string, error) {
	if runtime.GOOS == "windows" {
		return "", fmt.Errorf("native json_stdio_v1 currently requires a POSIX host with /bin/bash")
	}
	if !sk.NativeApproved || sk.Manifest.MCPX == nil || sk.Manifest.MCPX.Protocol != NativeProtocol {
		return "", fmt.Errorf("native Skill execution is not approved")
	}
	entry := sk.Manifest.MCPX.Entry
	if entry == "" || filepath.IsAbs(entry) {
		return "", fmt.Errorf("native entry must be a nonempty relative path")
	}
	for _, part := range strings.Split(filepath.ToSlash(entry), "/") {
		if part == ".." {
			return "", fmt.Errorf("native entry cannot traverse directories")
		}
	}
	root, err := filepath.EvalSymlinks(sk.Dir)
	if err != nil {
		return "", err
	}
	current := root
	for _, part := range strings.Split(filepath.ToSlash(filepath.Clean(entry)), "/") {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("native entry cannot contain a symlink")
		}
	}
	info, err := os.Stat(current)
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("native entry must be a regular file")
	}
	seconds := sk.Manifest.MCPX.TimeoutSeconds
	if seconds < 1 || seconds > 900 {
		return "", fmt.Errorf("native timeout_seconds must be between 1 and 900")
	}
	return current, nil
}

// ExecuteNative's operation and runtime context are supplied by the Runtime,
// never read from caller arguments. Only the approved entry chooses business behavior.
func ExecuteNative(ctx context.Context, sk Skill, operation string, arguments map[string]any, runtimeContext NativeRuntimeContext, envNames []string) (map[string]any, error) {
	if operation != "describe" && operation != "call" && operation != "observe" {
		return nil, fmt.Errorf("invalid native operation")
	}
	entry, err := ResolveNativeEntry(sk)
	if err != nil {
		return nil, err
	}
	if arguments == nil {
		arguments = map[string]any{}
	}
	if _, exists := arguments["runtime_context"]; exists {
		return nil, fmt.Errorf("runtime_context is server-owned")
	}
	payload := map[string]any{"protocol": NativeWireProtocol, "operation": operation, "arguments": arguments, "runtime_context": runtimeContext}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	if len(encoded) > NativeMaxInputBytes {
		return nil, fmt.Errorf("native Skill input exceeds byte limit")
	}
	baseline := []string{"PATH", "HOME", "LANG", "LC_ALL", "TZ", "TMPDIR", "SYSTEMROOT"}
	env := terminal.FilterEnvironment(os.Environ(), append(baseline, envNames...))
	result, err := terminal.ExecDirect(ctx, terminal.DirectOptions{
		Executable: "/bin/bash", Args: []string{entry}, WorkDir: sk.Dir, Stdin: encoded, Env: env,
		Timeout: time.Duration(sk.Manifest.MCPX.TimeoutSeconds) * time.Second, MaxOutputBytes: NativeMaxOutputBytes,
	})
	if err != nil {
		return nil, err
	}
	var value map[string]any
	if err = json.Unmarshal([]byte(result.Stdout), &value); err != nil || value == nil {
		return nil, fmt.Errorf("native Skill returned invalid JSON (exit code %d)", result.ExitCode)
	}
	if result.ExitCode != 0 && value["status"] != "error" {
		return nil, fmt.Errorf("native Skill failed without a structured error (exit code %d)", result.ExitCode)
	}
	if value["status"] == "error" {
		if _, ok := value["error"].(map[string]any); !ok {
			return nil, fmt.Errorf("native Skill error has no structured details")
		}
	}
	return value, nil
}
