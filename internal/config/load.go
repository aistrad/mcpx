package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// HomeDir returns MCPX home (~/.mcpx), overridable via MCPX_HOME.
func HomeDir() (string, error) {
	if v := os.Getenv("MCPX_HOME"); v != "" {
		return v, nil
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, ".mcpx"), nil
}

// GlobalConfigPath is ~/.mcpx/config.yaml (or $MCPX_HOME/config.yaml).
func GlobalConfigPath() (string, error) {
	home, err := HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "config.yaml"), nil
}

// GlobalMCPPath is ~/.mcpx/.mcp.json
func GlobalMCPPath() (string, error) {
	home, err := HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".mcp.json"), nil
}

// ProjectConfigPath is {workspace}/.mcpx.yaml
func ProjectConfigPath(workspacePath string) string {
	return filepath.Join(workspacePath, ".mcpx.yaml")
}

// ProjectRootMCPPath is {workspace}/.mcp.json
func ProjectRootMCPPath(workspacePath string) string {
	return filepath.Join(workspacePath, ".mcp.json")
}

// ProjectAgentsMCPPath is {workspace}/.agents/mcp.json
func ProjectAgentsMCPPath(workspacePath string) string {
	return filepath.Join(workspacePath, ".agents", "mcp.json")
}

// ProjectMCPPath is {workspace}/.mcpx/.mcp.json
func ProjectMCPPath(workspacePath string) string {
	return filepath.Join(workspacePath, ".mcpx", ".mcp.json")
}

// ProjectMCPConfigPaths returns workspace MCP files in merge order; later files win.
func ProjectMCPConfigPaths(workspacePath string) []string {
	return []string{
		ProjectRootMCPPath(workspacePath),
		ProjectAgentsMCPPath(workspacePath),
		ProjectMCPPath(workspacePath),
	}
}

// LoadGlobal loads global YAML or returns defaults if missing.
func LoadGlobal(path string) (Config, error) {
	base := DefaultConfig()
	if path == "" {
		var err error
		path, err = GlobalConfigPath()
		if err != nil {
			return base, err
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return base, nil
		}
		return base, err
	}
	var overlay Config
	if err := yaml.Unmarshal(data, &overlay); err != nil {
		return base, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := ValidateSecurityRules(overlay.Security); err != nil {
		return base, fmt.Errorf("validate %s: %w", path, err)
	}
	merged := merge(base, overlay, true)
	if err := ValidateRetention(merged.State.Retention); err != nil {
		return base, fmt.Errorf("validate %s: %w", path, err)
	}
	return merged, nil
}

// LoadProject loads project .mcpx.yaml; missing file yields zero Config overlay.
func LoadProject(workspacePath string) (Config, error) {
	path := ProjectConfigPath(workspacePath)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Config{}, nil
		}
		return Config{}, err
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := ValidateSecurityRules(c.Security); err != nil {
		return Config{}, fmt.Errorf("validate %s: %w", path, err)
	}
	return c, nil
}

// Merge applies project over global. security.commands replaced as a whole if project sets any list.
// skills.dirs: project Dirs replace; ExtraDirs append after.
func Merge(global, project Config) Config {
	return merge(global, project, false)
}

// mergeAuth allows LoadGlobal to overlay authentication defaults while keeping
// project configuration from redefining process-wide HTTP authentication.
func merge(global, project Config, mergeAuth bool) Config {
	out := global
	if mergeAuth {
		out.State.Retention = mergeRetention(global.State.Retention, project.State.Retention)
	}

	if project.Server.Host != "" {
		out.Server.Host = project.Server.Host
	}
	if project.Server.Port != 0 {
		out.Server.Port = project.Server.Port
	}
	if project.Server.DisableLocalhostProtection {
		out.Server.DisableLocalhostProtection = true
	}
	if mergeAuth {
		if project.Auth.Token != "" {
			out.Auth.Token = project.Auth.Token
		}
		if project.Auth.Mode != "" {
			out.Auth.Mode = project.Auth.Mode
		}
		if project.Auth.OAuth.Password != "" {
			out.Auth.OAuth.Password = project.Auth.OAuth.Password
		}
		if project.Auth.OAuth.ServerURL != "" {
			out.Auth.OAuth.ServerURL = project.Auth.OAuth.ServerURL
		}
		if project.Auth.OAuth.TokenTTL != 0 {
			out.Auth.OAuth.TokenTTL = project.Auth.OAuth.TokenTTL
		}
		if project.Auth.OAuth.TokenSecret != "" {
			out.Auth.OAuth.TokenSecret = project.Auth.OAuth.TokenSecret
		}
		if project.Auth.OAuth.ClientID != "" {
			out.Auth.OAuth.ClientID = project.Auth.OAuth.ClientID
		}
		if project.Auth.OAuth.ClientSecret != "" {
			out.Auth.OAuth.ClientSecret = project.Auth.OAuth.ClientSecret
		}
		if len(project.Auth.OAuth.RedirectURIs) > 0 {
			out.Auth.OAuth.RedirectURIs = append([]string{}, project.Auth.OAuth.RedirectURIs...)
		}
	}
	if project.Server.TrustProxyHeaders {
		out.Server.TrustProxyHeaders = true
	}
	if len(project.Server.AllowedOrigins) > 0 {
		out.Server.AllowedOrigins = append([]string{}, project.Server.AllowedOrigins...)
	}
	if project.Transport.SessionIdleTTL != "" {
		out.Transport.SessionIdleTTL = project.Transport.SessionIdleTTL
	}
	if project.Limits.MaxResultBytes != 0 {
		out.Limits.MaxResultBytes = project.Limits.MaxResultBytes
	}
	if project.Description != "" {
		out.Description = project.Description
	}

	if project.Security.Commands.Default != "" ||
		len(project.Security.Commands.Allow) > 0 ||
		len(project.Security.Commands.Confirm) > 0 ||
		len(project.Security.Commands.Deny) > 0 {
		// Whole-section replace when project sets any commands field.
		out.Security.Commands = project.Security.Commands
		if out.Security.Commands.Default == "" {
			out.Security.Commands.Default = global.Security.Commands.Default
			if out.Security.Commands.Default == "" {
				out.Security.Commands.Default = "confirm"
			}
		}
		if out.Security.Commands.AutoAllowReadonly == nil {
			out.Security.Commands.AutoAllowReadonly = global.Security.Commands.AutoAllowReadonly
		}
	}
	if project.Security.Files.MaxReadBytes != 0 ||
		project.Security.Files.MaxPatchFiles != 0 ||
		project.Security.Files.MaxPatchLines != 0 ||
		len(project.Security.Files.Allow) > 0 ||
		len(project.Security.Files.Confirm) > 0 ||
		len(project.Security.Files.Deny) > 0 {
		if project.Security.Files.MaxReadBytes != 0 {
			out.Security.Files.MaxReadBytes = project.Security.Files.MaxReadBytes
		}
		if project.Security.Files.MaxPatchFiles != 0 {
			out.Security.Files.MaxPatchFiles = project.Security.Files.MaxPatchFiles
		}
		if project.Security.Files.MaxPatchLines != 0 {
			out.Security.Files.MaxPatchLines = project.Security.Files.MaxPatchLines
		}
		if len(project.Security.Files.Allow) > 0 {
			out.Security.Files.Allow = project.Security.Files.Allow
		}
		if len(project.Security.Files.Confirm) > 0 {
			out.Security.Files.Confirm = project.Security.Files.Confirm
		}
		if len(project.Security.Files.Deny) > 0 {
			out.Security.Files.Deny = project.Security.Files.Deny
		}
	}

	// Workspaces only live on global; project does not replace list.
	if len(project.Workspaces) > 0 {
		out.Workspaces = project.Workspaces
	}

	if len(project.Discovery.Skills.Dirs) > 0 {
		out.Discovery.Skills.Dirs = append([]string{}, project.Discovery.Skills.Dirs...)
	}
	if len(project.Discovery.Skills.ExtraDirs) > 0 {
		out.Discovery.Skills.Dirs = append(out.Discovery.Skills.Dirs, project.Discovery.Skills.ExtraDirs...)
	}

	if project.Logging.Dir != "" {
		out.Logging.Dir = project.Logging.Dir
	}
	if project.Terminal.EnabledSet {
		out.Terminal.Enabled = project.Terminal.Enabled
	}
	if project.FileWatch.EnabledSet {
		out.FileWatch.Enabled = project.FileWatch.Enabled
	}
	if project.Discovery.MCP.EnabledSet {
		out.Discovery.MCP.Enabled = project.Discovery.MCP.Enabled
	}
	if project.Discovery.Skills.EnabledSet {
		out.Discovery.Skills.Enabled = project.Discovery.Skills.Enabled
	}
	// Instruction paths are process-wide. A project cannot redirect callers to
	// an arbitrary host file by placing a .mcpx.yaml in its repository.
	if mergeAuth && project.Discovery.Instructions.GlobalAgentsPath != "" {
		out.Discovery.Instructions.GlobalAgentsPath = project.Discovery.Instructions.GlobalAgentsPath
	}
	if project.Logging.EnabledSet {
		out.Logging.Enabled = project.Logging.Enabled
	}

	return out
}

// Effective loads global + project for a workspace path and merges.
func Effective(workspacePath string) (Config, error) {
	g, err := LoadGlobal("")
	if err != nil {
		return Config{}, err
	}
	p, err := LoadProject(workspacePath)
	if err != nil {
		return Config{}, err
	}
	return Merge(g, p), nil
}

// RegisterWorkspace appends or updates workspace in global config file.
func RegisterWorkspace(globalPath, absPath string) error {
	if globalPath == "" {
		var err error
		globalPath, err = GlobalConfigPath()
		if err != nil {
			return err
		}
	}
	absPath, err := filepath.Abs(absPath)
	if err != nil {
		return err
	}
	cfg, err := LoadGlobal(globalPath)
	if err != nil {
		return err
	}
	name := filepath.Base(absPath)
	found := false
	for i := range cfg.Workspaces {
		if cfg.Workspaces[i].Path == absPath || cfg.Workspaces[i].Name == name {
			cfg.Workspaces[i].Path = absPath
			cfg.Workspaces[i].Name = name
			found = true
			break
		}
	}
	if !found {
		cfg.Workspaces = append(cfg.Workspaces, WorkspaceEntry{
			Name: name,
			Path: absPath,
		})
	}
	return WriteGlobal(globalPath, cfg)
}

// WriteGlobal writes config YAML, creating parent dirs.
func WriteGlobal(path string, cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// ExpandHome replaces leading ~/ with home dir.
func ExpandHome(p string) string {
	if p == "" {
		return p
	}
	if strings.HasPrefix(p, "~/") {
		h, err := os.UserHomeDir()
		if err != nil {
			return p
		}
		return filepath.Join(h, p[2:])
	}
	return p
}

// Addr returns host:port from server config.
func (c Config) Addr() string {
	host := c.Server.Host
	if host == "" {
		host = "127.0.0.1"
	}
	port := c.Server.Port
	if port == 0 {
		port = 9090
	}
	return fmt.Sprintf("%s:%d", host, port)
}
