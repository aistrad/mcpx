package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDefaultConfigUsesTransportSessionTTL(t *testing.T) {
	encoded, err := yaml.Marshal(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	if !strings.Contains(text, "transport:\n    session_idle_ttl: 24h") {
		t.Fatalf("transport config missing:\n%s", text)
	}
	if strings.Contains(text, "\nsession:") {
		t.Fatalf("legacy session config must not be emitted:\n%s", text)
	}
}

func TestDefaultConfigUsesStateRetentionDefaults(t *testing.T) {
	cfg := DefaultConfig()
	retention := cfg.State.Retention
	if !retention.Enabled || retention.Interval != "24h" {
		t.Fatalf("retention enabled/interval: %+v", retention)
	}
	if retention.ProcessEventTTL != "720h" || retention.ProcessEventMaxRows != 10000 {
		t.Fatalf("process retention: %+v", retention)
	}
	if retention.MemoryEventTTL != "4320h" || retention.MemoryEventMaxRows != 2000 {
		t.Fatalf("memory retention: %+v", retention)
	}
	if retention.TerminalTaskTTL != "720h" || retention.SnapshotTTL != "2160h" || retention.VacuumThresholdRows != 10000 {
		t.Fatalf("secondary retention: %+v", retention)
	}
}

func TestLoadGlobalParsesAndValidatesStateRetention(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(`state:
  retention:
    enabled: false
    interval: 2h
    process_event_ttl: 48h
    process_event_max_rows: 42
    memory_event_ttl: 72h
    memory_event_max_rows: 24
    terminal_task_ttl: 96h
    snapshot_ttl: 120h
    vacuum_threshold_rows: 8
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatal(err)
	}
	retention := cfg.State.Retention
	if retention.Enabled || retention.Interval != "2h" || retention.ProcessEventMaxRows != 42 || retention.VacuumThresholdRows != 8 {
		t.Fatalf("parsed retention: %+v", retention)
	}
}

func TestLoadGlobalRejectsInvalidStateRetention(t *testing.T) {
	tests := []string{
		"state:\n  retention:\n    interval: nope\n",
		"state:\n  retention:\n    process_event_ttl: -1h\n",
		"state:\n  retention:\n    process_event_max_rows: 0\n",
		"state:\n  retention:\n    vacuum_threshold_rows: -1\n",
	}
	for _, content := range tests {
		t.Run(strings.ReplaceAll(strings.TrimSpace(content), "\n", "/"), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadGlobal(path); err == nil {
				t.Fatalf("expected invalid retention config to fail: %s", content)
			}
		})
	}
}

func TestMergeDoesNotAllowProjectStateRetentionOverride(t *testing.T) {
	global := DefaultConfig()
	project := Config{State: StateConfig{Retention: RetentionConfig{
		Enabled: false, Interval: "1h", ProcessEventMaxRows: 1,
	}}}
	merged := Merge(global, project)
	if merged.State.Retention != global.State.Retention {
		t.Fatalf("project changed global retention: got=%+v want=%+v", merged.State.Retention, global.State.Retention)
	}
}

func TestMergeKeepsGlobalTokenAndProjectDescription(t *testing.T) {
	g := DefaultConfig()
	g.Auth.Token = "global-tok"
	g.Discovery.Instructions.GlobalAgentsPath = "/trusted/AGENTS.md"
	p := Config{
		Auth:        AuthConfig{Token: "proj-tok"},
		Description: "proj desc",
		Discovery: DiscoveryConfig{Instructions: InstructionsDiscovery{
			GlobalAgentsPath: "/untrusted/AGENTS.md",
		}},
	}
	m := Merge(g, p)
	if m.Auth.Token != "global-tok" {
		t.Fatalf("token: %q", m.Auth.Token)
	}
	if m.Description != "proj desc" {
		t.Fatalf("desc: %q", m.Description)
	}
	if m.Discovery.Instructions.GlobalAgentsPath != "/trusted/AGENTS.md" {
		t.Fatalf("project replaced global instruction path: %q", m.Discovery.Instructions.GlobalAgentsPath)
	}
}

func TestLoadGlobalInstructionPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("discovery:\n  instructions:\n    global_agents_path: ~/AGENTS.md\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Discovery.Instructions.GlobalAgentsPath != "~/AGENTS.md" {
		t.Fatalf("global_agents_path: %q", cfg.Discovery.Instructions.GlobalAgentsPath)
	}
}

func TestMergeExplicitFalseFeatureFlags(t *testing.T) {
	global := DefaultConfig()
	var project Config
	if err := yaml.Unmarshal([]byte(`
terminal:
  enabled: false
file_watch:
  enabled: false
discovery:
  mcp:
    enabled: false
  skills:
    enabled: false
logging:
  enabled: false
`), &project); err != nil {
		t.Fatal(err)
	}
	merged := Merge(global, project)
	if merged.Terminal.Enabled || merged.FileWatch.Enabled ||
		merged.Discovery.MCP.Enabled || merged.Discovery.Skills.Enabled || merged.Logging.Enabled {
		t.Fatalf("explicit false flags not preserved: %+v", merged)
	}
}

func TestValidateSecurityRulesRejectsInvalidRegexp(t *testing.T) {
	err := ValidateSecurityRules(SecurityConfig{Commands: CommandRules{Deny: []string{"["}}})
	if err == nil {
		t.Fatal("expected invalid regexp error")
	}
}

func TestMergeCommandsReplace(t *testing.T) {
	g := DefaultConfig()
	p := Config{
		Security: SecurityConfig{
			Commands: CommandRules{Allow: []string{`^echo`}},
		},
	}
	m := Merge(g, p)
	if len(m.Security.Commands.Allow) != 1 || m.Security.Commands.Allow[0] != `^echo` {
		t.Fatalf("allow: %+v", m.Security.Commands.Allow)
	}
	if len(m.Security.Commands.Deny) != 0 {
		t.Fatalf("deny should be replaced empty, got %+v", m.Security.Commands.Deny)
	}
}

func TestRegisterWorkspaceAndLoad(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MCPX_HOME", dir)
	global := filepath.Join(dir, "config.yaml")
	ws := filepath.Join(dir, "myproj")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := RegisterWorkspace(global, ws); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadGlobal(global)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Workspaces) != 1 || cfg.Workspaces[0].Name != "myproj" {
		t.Fatalf("workspaces: %+v", cfg.Workspaces)
	}
	// second register updates same
	if err := RegisterWorkspace(global, ws); err != nil {
		t.Fatal(err)
	}
	cfg, _ = LoadGlobal(global)
	if len(cfg.Workspaces) != 1 {
		t.Fatalf("dup: %+v", cfg.Workspaces)
	}
}

func TestMergeMCP(t *testing.T) {
	g := MCPFile{MCPServers: map[string]MCPServer{
		"github": {Command: "g", Type: "stdio"},
	}}
	p := MCPFile{MCPServers: map[string]MCPServer{
		"github": {Command: "g2", Type: "stdio"},
		"local":  {Command: "l", Type: "stdio"},
	}}
	m := MergeMCP(g, p)
	if m.MCPServers["github"].Command != "g2" {
		t.Fatal("project should override")
	}
	if _, ok := m.MCPServers["local"]; !ok {
		t.Fatal("project add")
	}
	later := MergeMCP(g, p, MCPFile{MCPServers: map[string]MCPServer{
		"local": {Command: "agents", Type: "stdio"},
	}})
	if later.MCPServers["local"].Command != "agents" {
		t.Fatalf("later file should override: %+v", later.MCPServers["local"])
	}
}

func TestProjectMCPConfigPaths(t *testing.T) {
	ws := filepath.Join(string(filepath.Separator), "tmp", "demo")
	paths := ProjectMCPConfigPaths(ws)
	want := []string{
		ProjectRootMCPPath(ws),
		ProjectAgentsMCPPath(ws),
		ProjectMCPPath(ws),
	}
	if len(paths) != len(want) {
		t.Fatalf("paths=%v", paths)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Fatalf("paths[%d]=%q want %q", i, paths[i], want[i])
		}
	}
}

func TestLoadMergedMCPLayers(t *testing.T) {
	home := t.TempDir()
	ws := t.TempDir()
	t.Setenv("MCPX_HOME", home)
	if err := WriteMCPFile(filepath.Join(home, ".mcp.json"), MCPFile{MCPServers: map[string]MCPServer{
		"github": {Command: "global", Type: "stdio"},
		"keep":   {Command: "from-global", Type: "stdio"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := WriteMCPFile(ProjectRootMCPPath(ws), MCPFile{MCPServers: map[string]MCPServer{
		"github": {Command: "root", Type: "stdio"},
		"local":  {Command: "from-root", Type: "stdio"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := WriteMCPFile(ProjectAgentsMCPPath(ws), MCPFile{MCPServers: map[string]MCPServer{
		"local":  {Command: "from-agents", Type: "stdio"},
		"agents": {Command: "from-agents", Type: "stdio"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := WriteMCPFile(ProjectMCPPath(ws), MCPFile{MCPServers: map[string]MCPServer{
		"agents": {Command: "from-mcpx", Type: "stdio"},
	}}); err != nil {
		t.Fatal(err)
	}
	merged, err := LoadMergedMCP(ws)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"github": "root",
		"keep":   "from-global",
		"local":  "from-agents",
		"agents": "from-mcpx",
	}
	if len(merged.MCPServers) != len(want) {
		t.Fatalf("servers=%+v", merged.MCPServers)
	}
	for name, command := range want {
		got, ok := merged.MCPServers[name]
		if !ok || got.Command != command {
			t.Fatalf("%s=%+v want command %q", name, got, command)
		}
	}
}

func TestLoadMergedMCPRootOnly(t *testing.T) {
	home := t.TempDir()
	ws := t.TempDir()
	t.Setenv("MCPX_HOME", home)
	if err := WriteMCPFile(ProjectRootMCPPath(ws), MCPFile{MCPServers: map[string]MCPServer{
		"browser": {Command: "root-only", Type: "stdio"},
	}}); err != nil {
		t.Fatal(err)
	}
	merged, err := LoadMergedMCP(ws)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := merged.MCPServers["browser"]
	if !ok || got.Command != "root-only" {
		t.Fatalf("root .mcp.json not loaded: %+v", merged.MCPServers)
	}
}
