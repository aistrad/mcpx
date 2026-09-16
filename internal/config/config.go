package config

// Config is the Runtime YAML schema (global + project merge result).
type Config struct {
	Server     ServerConfig     `yaml:"server"`
	Auth       AuthConfig       `yaml:"auth"`
	Security   SecurityConfig   `yaml:"security"`
	State      StateConfig      `yaml:"state"`
	Workspaces []WorkspaceEntry `yaml:"workspaces"`
	Terminal   TerminalConfig   `yaml:"terminal"`
	FileWatch  FileWatchConfig  `yaml:"file_watch"`
	Discovery  DiscoveryConfig  `yaml:"discovery"`
	Logging    LoggingConfig    `yaml:"logging"`
	Transport  TransportConfig  `yaml:"transport"`
	Limits     LimitsConfig     `yaml:"limits"`
	// Project-only fields merged into effective view:
	Description string `yaml:"description,omitempty"`
}

type ServerConfig struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
	// DisableLocalhostProtection turns off MCP DNS-rebinding Host checks.
	// Required when clients hit a public IP/domain while the process accepts
	// the connection on loopback (reverse proxy, frp, ssh -L, etc.).
	// Prefer pairing with auth.token when enabled.
	DisableLocalhostProtection bool `yaml:"disable_localhost_protection"`
	// TrustProxyHeaders uses X-Forwarded-Proto/Host when building public URLs.
	TrustProxyHeaders bool `yaml:"trust_proxy_headers"`
	// AllowedOrigins for CORS; empty means reflect request Origin when present (dev-friendly).
	AllowedOrigins []string `yaml:"allowed_origins"`
}

// AuthConfig controls HTTP/MCP credential checks.
// Mode: open | bearer | oauth | dual. Empty mode: bearer if Token set, else open.
type AuthConfig struct {
	Mode  string      `yaml:"mode"`
	Token string      `yaml:"token"`
	OAuth OAuthConfig `yaml:"oauth"`
}

// OAuthConfig is the in-process authorization server settings.
type OAuthConfig struct {
	Password     string   `yaml:"password"`
	ServerURL    string   `yaml:"server_url"`   // public origin, no trailing slash
	TokenTTL     int      `yaml:"token_ttl"`    // seconds
	TokenSecret  string   `yaml:"token_secret"` // hex-encoded HMAC key
	ClientID     string   `yaml:"client_id"`
	ClientSecret string   `yaml:"client_secret"`
	RedirectURIs []string `yaml:"redirect_uris"`
}

type TransportConfig struct {
	SessionIdleTTL string `yaml:"session_idle_ttl"` // Go duration, e.g. "1h"
}

type LimitsConfig struct {
	MaxResultBytes int `yaml:"max_result_bytes"`
}

// StateConfig controls durable state retention. It is process-wide and is
// intentionally not overridable by a project-level .mcpx.yaml.
type StateConfig struct {
	Retention RetentionConfig `yaml:"retention"`
}

// RetentionConfig contains conservative cleanup thresholds for the state DB.
// The EnabledSet and *Set fields distinguish omitted YAML fields from an
// explicit zero/false value while merging the global configuration.
type RetentionConfig struct {
	Enabled             bool   `yaml:"enabled"`
	EnabledSet          bool   `yaml:"-"`
	Interval            string `yaml:"interval"`
	IntervalSet         bool   `yaml:"-"`
	ProcessEventTTL     string `yaml:"process_event_ttl"`
	ProcessEventTTLSet  bool   `yaml:"-"`
	ProcessEventMaxRows int    `yaml:"process_event_max_rows"`
	ProcessEventMaxSet  bool   `yaml:"-"`
	MemoryEventTTL      string `yaml:"memory_event_ttl"`
	MemoryEventTTLSet   bool   `yaml:"-"`
	MemoryEventMaxRows  int    `yaml:"memory_event_max_rows"`
	MemoryEventMaxSet   bool   `yaml:"-"`
	TerminalTaskTTL     string `yaml:"terminal_task_ttl"`
	TerminalTaskTTLSet  bool   `yaml:"-"`
	SnapshotTTL         string `yaml:"snapshot_ttl"`
	SnapshotTTLSet      bool   `yaml:"-"`
	VacuumThresholdRows int    `yaml:"vacuum_threshold_rows"`
	VacuumThresholdSet  bool   `yaml:"-"`
}

type SecurityConfig struct {
	Commands CommandRules `yaml:"commands"`
	Files    FileRules    `yaml:"files"`
}

type CommandRules struct {
	// Default is the decision when no allow/confirm/deny regex matches.
	// Values: "allow" | "confirm" | "deny". Empty => "confirm" (safe built-in).
	Default string   `yaml:"default"`
	Allow   []string `yaml:"allow"`
	Confirm []string `yaml:"confirm"`
	Deny    []string `yaml:"deny"`
	// AutoAllowReadonly enables the built-in read-only command whitelist
	// (git status/diff/log/show, ls, cat, pwd, head, tail). nil or true enables
	// it; false restores the default confirm decision. Explicit deny/confirm
	// rules always take precedence.
	AutoAllowReadonly *bool `yaml:"auto_allow_readonly"`
}

type FileRules struct {
	MaxReadBytes  int64    `yaml:"max_read_bytes"`
	MaxPatchFiles int      `yaml:"max_patch_files"`
	MaxPatchLines int      `yaml:"max_patch_lines"`
	Allow         []string `yaml:"allow"`
	Confirm       []string `yaml:"confirm"`
	Deny          []string `yaml:"deny"`
}

type WorkspaceEntry struct {
	Name        string `yaml:"name"`
	Path        string `yaml:"path"`
	Description string `yaml:"description"`
}

type TerminalConfig struct {
	Enabled    bool `yaml:"enabled"`
	EnabledSet bool `yaml:"-"`
}

type FileWatchConfig struct {
	Enabled    bool `yaml:"enabled"`
	EnabledSet bool `yaml:"-"`
}

type DiscoveryConfig struct {
	MCP          MCPDiscovery          `yaml:"mcp"`
	Skills       SkillsDiscovery       `yaml:"skills"`
	Instructions InstructionsDiscovery `yaml:"instructions"`
}

type MCPDiscovery struct {
	Enabled    bool `yaml:"enabled"`
	EnabledSet bool `yaml:"-"`
}

type SkillsDiscovery struct {
	Enabled    bool     `yaml:"enabled"`
	EnabledSet bool     `yaml:"-"`
	Dirs       []string `yaml:"dirs"`
	ExtraDirs  []string `yaml:"extra_dirs"`
}

// InstructionsDiscovery controls the process-wide instruction document that
// is provided alongside a Workspace's root-level AGENTS.md.
type InstructionsDiscovery struct {
	GlobalAgentsPath string `yaml:"global_agents_path"`
}

type LoggingConfig struct {
	Enabled    bool   `yaml:"enabled"`
	EnabledSet bool   `yaml:"-"`
	Dir        string `yaml:"dir"`
}

// MCPFile is ~/.mcpx/.mcp.json or a workspace .mcp.json overlay.
type MCPFile struct {
	MCPServers map[string]MCPServer `json:"mcpServers"`
}

// MCPServer describes an upstream MCP process.
type MCPServer struct {
	Type        string            `json:"type"`
	Description string            `json:"description,omitempty"`
	Command     string            `json:"command"`
	Args        []string          `json:"args"`
	Env         map[string]string `json:"env"`
}

// DefaultConfig returns built-in defaults per PRD.
func DefaultConfig() Config {
	return Config{
		Server: ServerConfig{Host: "127.0.0.1", Port: 9090},
		Auth: AuthConfig{
			Mode:  "",
			Token: "",
			OAuth: OAuthConfig{TokenTTL: 86400},
		},
		Transport: TransportConfig{SessionIdleTTL: "24h"},
		Limits:    LimitsConfig{MaxResultBytes: 256 << 10},
		State: StateConfig{Retention: RetentionConfig{
			Enabled:             true,
			Interval:            "24h",
			ProcessEventTTL:     "720h",
			ProcessEventMaxRows: 10000,
			MemoryEventTTL:      "4320h",
			MemoryEventMaxRows:  2000,
			TerminalTaskTTL:     "720h",
			SnapshotTTL:         "2160h",
			VacuumThresholdRows: 10000,
		}},
		Security: SecurityConfig{
			Commands: CommandRules{
				Default: "allow",
				Allow: []string{
					`^ls\b`,
					`^pwd$`,
					`^git status`,
					`^git diff`,
					`^npm test`,
					`^echo\b`,
				},
				Confirm: []string{
					`^git push`,
					`^docker`,
					`^npm install`,
				},
				Deny: []string{
					`^rm -rf /`,
					`^mkfs`,
					`^shutdown`,
				},
			},
			Files: FileRules{
				MaxReadBytes:  1 << 20,
				MaxPatchFiles: 20,
				MaxPatchLines: 2000,
			},
		},
		Workspaces: nil,
		Terminal:   TerminalConfig{Enabled: true},
		FileWatch:  FileWatchConfig{Enabled: true},
		Discovery: DiscoveryConfig{
			MCP: MCPDiscovery{Enabled: true},
			Skills: SkillsDiscovery{
				Enabled: true,
				Dirs: []string{
					"~/.mcpx/skills",
					"~/.agents/skills",
					"~/.agent/skills",
					"~/.codex/skills",
					"~/.grok/skills",
					".skills",
				},
			},
		},
		Logging: LoggingConfig{
			Enabled: true,
			Dir:     "", // empty => {MCPX_HOME|~/.mcpx}/logs at runtime
		},
	}
}
