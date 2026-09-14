package environment

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"mcpx/internal/screenshot"
	"mcpx/internal/terminal"
	buildversion "mcpx/internal/version"
	"mcpx/internal/winproc"
)

var ValidSections = []string{
	"runtime", "os", "architecture", "execution", "shell", "resources", "filesystem", "toolchains",
}

type Report struct {
	SnapshotID   string                   `json:"snapshot_id,omitempty"`
	CapturedAt   time.Time                `json:"captured_at"`
	Runtime      *RuntimeInfo             `json:"runtime,omitempty"`
	OS           *OSInfo                  `json:"os,omitempty"`
	Architecture *ArchitectureInfo        `json:"architecture,omitempty"`
	Execution    *ExecutionInfo           `json:"execution,omitempty"`
	Shell        *ShellInfo               `json:"shell,omitempty"`
	Resources    *ResourceInfo            `json:"resources,omitempty"`
	Filesystem   *FilesystemInfo          `json:"filesystem,omitempty"`
	Toolchains   map[string]ToolchainInfo `json:"toolchains,omitempty"`
	Comparison   *Comparison              `json:"comparison,omitempty"`
}

type RuntimeInfo struct {
	MCPXVersion string `json:"mcpx_version"`
	GoVersion   string `json:"go_version"`
	PID         int    `json:"pid"`
	CPUs        int    `json:"logical_cpus"`
}

type OSInfo struct {
	Type                    string               `json:"type"`
	Distribution            string               `json:"distribution,omitempty"`
	Version                 string               `json:"version,omitempty"`
	KernelName              string               `json:"kernel_name,omitempty"`
	KernelRelease           string               `json:"kernel_release,omitempty"`
	DisplayCount            int                  `json:"display_count"`
	PrimaryScreenResolution string               `json:"primary_screen_resolution,omitempty"`
	Displays                []screenshot.Display `json:"displays,omitempty"`
}

type ArchitectureInfo struct {
	Process     string `json:"process"`
	Host        string `json:"host,omitempty"`
	ProcessBits int    `json:"process_bits"`
}

type ExecutionInfo struct {
	Container              bool     `json:"container"`
	ContainerType          string   `json:"container_type,omitempty"`
	WSL                    bool     `json:"wsl"`
	CI                     bool     `json:"ci"`
	CIProvider             string   `json:"ci_provider,omitempty"`
	SensitiveVariableNames []string `json:"sensitive_variable_names,omitempty"`
}

type ShellInfo struct {
	ExecutionShell   string `json:"execution_shell"`
	InteractiveShell string `json:"interactive_shell,omitempty"`
}

type ResourceInfo struct {
	LogicalCPUs        int    `json:"logical_cpus"`
	MemoryTotalBytes   uint64 `json:"memory_total_bytes,omitempty"`
	MemorySource       string `json:"memory_source,omitempty"`
	WorkspaceFreeBytes uint64 `json:"workspace_free_bytes,omitempty"`
}

type FilesystemInfo struct {
	PathSeparator     string `json:"path_separator"`
	CaseSensitivity   string `json:"case_sensitivity"`
	Symlinks          bool   `json:"symlinks"`
	WorkspaceExists   bool   `json:"workspace_exists"`
	WorkspaceWritable bool   `json:"workspace_writable"`
}

type ToolchainInfo struct {
	Available bool   `json:"available"`
	Version   string `json:"version,omitempty"`
}

// Inspect captures a bounded, non-secret view of the current execution host.
func Inspect(ctx context.Context, workspacePath string, sections []string) Report {
	wanted := normalizeSections(sections)
	report := Report{CapturedAt: time.Now().UTC()}
	if wanted["runtime"] {
		report.Runtime = &RuntimeInfo{MCPXVersion: buildversion.Current, GoVersion: runtime.Version(), PID: os.Getpid(), CPUs: runtime.NumCPU()}
	}
	if wanted["os"] {
		report.OS = inspectOS(ctx)
	}
	if wanted["architecture"] {
		report.Architecture = inspectArchitecture(ctx)
	}
	if wanted["execution"] {
		report.Execution = inspectExecution()
	}
	if wanted["shell"] {
		report.Shell = inspectShell()
	}
	if wanted["resources"] {
		report.Resources = inspectResources(ctx, workspacePath)
	}
	if wanted["filesystem"] {
		report.Filesystem = inspectFilesystem(workspacePath)
	}
	if wanted["toolchains"] {
		report.Toolchains = inspectToolchains(ctx)
	}
	return report
}

// SelectSections returns a view over an already captured report without
// repeating external probes such as toolchain version commands.
func SelectSections(report Report, sections []string) Report {
	wanted := normalizeSections(sections)
	selected := Report{SnapshotID: report.SnapshotID, CapturedAt: report.CapturedAt, Comparison: report.Comparison}
	if wanted["runtime"] {
		selected.Runtime = report.Runtime
	}
	if wanted["os"] {
		selected.OS = report.OS
	}
	if wanted["architecture"] {
		selected.Architecture = report.Architecture
	}
	if wanted["execution"] {
		selected.Execution = report.Execution
	}
	if wanted["shell"] {
		selected.Shell = report.Shell
	}
	if wanted["resources"] {
		selected.Resources = report.Resources
	}
	if wanted["filesystem"] {
		selected.Filesystem = report.Filesystem
	}
	if wanted["toolchains"] {
		selected.Toolchains = report.Toolchains
	}
	return selected
}

func normalizeSections(sections []string) map[string]bool {
	valid := make(map[string]bool, len(ValidSections))
	for _, section := range ValidSections {
		valid[section] = len(sections) == 0
	}
	for _, section := range sections {
		if _, ok := valid[section]; ok {
			valid[section] = true
		}
	}
	return valid
}

func inspectOS(ctx context.Context) *OSInfo {
	info := &OSInfo{Type: runtime.GOOS}
	if runtime.GOOS == "linux" {
		values := readKeyValues("/etc/os-release")
		info.Distribution = firstNonEmpty(values["PRETTY_NAME"], values["NAME"], values["ID"])
		info.Version = firstNonEmpty(values["VERSION_ID"], values["VERSION"])
	} else if runtime.GOOS == "darwin" {
		info.Distribution = "macOS"
		info.Version = commandOutput(ctx, "sw_vers", "-productVersion")
	} else if runtime.GOOS == "windows" {
		info.Distribution = "Windows"
		info.Version = commandOutput(ctx, "cmd", "/C", "ver")
	}
	if runtime.GOOS != "windows" {
		info.KernelName = commandOutput(ctx, "uname", "-s")
		info.KernelRelease = commandOutput(ctx, "uname", "-r")
	}
	info.Displays = screenshot.Displays(ctx)
	info.DisplayCount = len(info.Displays)
	for _, display := range info.Displays {
		if display.Primary {
			info.PrimaryScreenResolution = fmt.Sprintf("%dx%d", display.Width, display.Height)
			break
		}
	}
	return info
}

func inspectArchitecture(ctx context.Context) *ArchitectureInfo {
	host := ""
	if runtime.GOOS == "windows" {
		host = firstNonEmpty(os.Getenv("PROCESSOR_ARCHITEW6432"), os.Getenv("PROCESSOR_ARCHITECTURE"))
	} else {
		host = commandOutput(ctx, "uname", "-m")
	}
	return &ArchitectureInfo{Process: runtime.GOARCH, Host: host, ProcessBits: strconv.IntSize}
}

func inspectExecution() *ExecutionInfo {
	info := &ExecutionInfo{}
	if fileExists("/.dockerenv") {
		info.Container, info.ContainerType = true, "docker"
	} else if fileExists("/run/.containerenv") {
		info.Container, info.ContainerType = true, "container"
	} else if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		info.Container, info.ContainerType = true, "kubernetes"
	} else if data, err := os.ReadFile("/proc/1/cgroup"); err == nil {
		value := strings.ToLower(string(data))
		for _, marker := range []string{"docker", "containerd", "kubepods", "podman"} {
			if strings.Contains(value, marker) {
				info.Container, info.ContainerType = true, marker
				break
			}
		}
	}
	if os.Getenv("WSL_DISTRO_NAME") != "" {
		info.WSL = true
	} else if data, err := os.ReadFile("/proc/version"); err == nil {
		info.WSL = strings.Contains(strings.ToLower(string(data)), "microsoft")
	}
	for _, candidate := range []struct{ Variable, Provider string }{
		{"GITHUB_ACTIONS", "github-actions"}, {"GITLAB_CI", "gitlab-ci"}, {"JENKINS_URL", "jenkins"},
		{"CIRCLECI", "circleci"}, {"BUILDKITE", "buildkite"}, {"TF_BUILD", "azure-pipelines"}, {"CI", "generic"},
	} {
		if os.Getenv(candidate.Variable) != "" {
			info.CI, info.CIProvider = true, candidate.Provider
			break
		}
	}
	info.SensitiveVariableNames = sensitiveVariableNames()
	return info
}

func inspectShell() *ShellInfo {
	execution := terminal.ExecutionShell()
	interactive := filepath.Base(os.Getenv("SHELL"))
	if runtime.GOOS == "windows" {
		interactive = filepath.Base(os.Getenv("COMSPEC"))
	}
	return &ShellInfo{ExecutionShell: execution, InteractiveShell: interactive}
}

func inspectResources(ctx context.Context, workspacePath string) *ResourceInfo {
	info := &ResourceInfo{LogicalCPUs: runtime.NumCPU()}
	if runtime.GOOS == "linux" {
		values := readKeyValues("/proc/meminfo")
		if fields := strings.Fields(values["MemTotal"]); len(fields) > 0 {
			if kb, err := strconv.ParseUint(fields[0], 10, 64); err == nil {
				info.MemoryTotalBytes, info.MemorySource = kb*1024, "procfs"
			}
		}
	} else if runtime.GOOS == "darwin" {
		if value, err := strconv.ParseUint(commandOutput(ctx, "sysctl", "-n", "hw.memsize"), 10, 64); err == nil {
			info.MemoryTotalBytes, info.MemorySource = value, "sysctl"
		}
	}
	info.WorkspaceFreeBytes = workspaceFreeBytes(workspacePath)
	return info
}

func inspectFilesystem(workspacePath string) *FilesystemInfo {
	info := &FilesystemInfo{
		PathSeparator: string(os.PathSeparator), Symlinks: runtime.GOOS != "windows",
		CaseSensitivity: "usually-sensitive",
	}
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		info.CaseSensitivity = "usually-insensitive"
	}
	if stat, err := os.Stat(workspacePath); err == nil && stat.IsDir() {
		info.WorkspaceExists = true
		info.WorkspaceWritable = stat.Mode().Perm()&0o222 != 0
	}
	return info
}

func inspectToolchains(ctx context.Context) map[string]ToolchainInfo {
	return inspectToolchainsWith(ctx, exec.LookPath, commandOutput)
}

func inspectToolchainsWith(
	ctx context.Context,
	lookPath func(string) (string, error),
	output func(context.Context, string, ...string) string,
) map[string]ToolchainInfo {
	commands := map[string][]string{
		"go": {"go", "version"}, "git": {"git", "--version"}, "node": {"node", "--version"},
		"npm": {"npm", "--version"}, "pnpm": {"pnpm", "--version"}, "yarn": {"yarn", "--version"},
		"python": {"python3", "--version"}, "java": {"java", "-version"}, "rust": {"rustc", "--version"},
		"cargo": {"cargo", "--version"}, "docker": {"docker", "--version"},
	}
	names := make([]string, 0, len(commands))
	for name := range commands {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make(map[string]ToolchainInfo, len(names))
	type probeResult struct {
		name string
		info ToolchainInfo
	}
	probes := make(chan probeResult, len(names))
	pending := 0
	for _, name := range names {
		command := commands[name]
		if _, err := lookPath(command[0]); err != nil {
			result[name] = ToolchainInfo{}
			continue
		}
		pending++
		go func(name string, command []string) {
			probes <- probeResult{
				name: name,
				info: ToolchainInfo{Available: true, Version: output(ctx, command[0], command[1:]...)},
			}
		}(name, command)
	}
	for range pending {
		probe := <-probes
		result[probe.name] = probe.info
	}
	return result
}

func commandOutput(parent context.Context, name string, args ...string) string {
	ctx, cancel := context.WithTimeout(parent, 1500*time.Millisecond)
	defer cancel()
	command := exec.CommandContext(ctx, name, args...)
	// Windows 下隐藏环境探测命令创建的控制台窗口；其他系统为空操作。
	winproc.ConfigureNoWindow(command)
	output, err := command.CombinedOutput()
	if err != nil && len(output) == 0 {
		return ""
	}
	line := strings.TrimSpace(strings.SplitN(string(output), "\n", 2)[0])
	if len(line) > 256 {
		line = line[:256]
	}
	return line
}

func readKeyValues(path string) map[string]string {
	values := map[string]string{}
	file, err := os.Open(path)
	if err != nil {
		return values
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), "=")
		if ok {
			values[key] = strings.Trim(strings.TrimSpace(value), `"`)
		}
	}
	return values
}

func sensitiveVariableNames() []string {
	seen := map[string]bool{}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		upper := strings.ToUpper(name)
		for _, suffix := range []string{"TOKEN", "SECRET", "PASSWORD", "PASSWD", "API_KEY", "PRIVATE_KEY", "CREDENTIAL"} {
			if strings.Contains(upper, suffix) {
				seen["***_"+suffix] = true
				break
			}
		}
	}
	result := make([]string, 0, len(seen))
	for value := range seen {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
