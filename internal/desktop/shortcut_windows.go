//go:build windows

package desktop

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"mcpx/internal/winproc"
)

const desktopShortcutFilename = "MCPX.lnk"

// ensureDesktopShortcut 创建/刷新用户桌面上的 MCPX 快捷方式。
//
// mcpx.exe 同时承担 CLI 与 Desktop 两种职责，所以它必须保持 Windows
// Console subsystem；否则普通 CLI 命令会没有 stdout/stderr。快捷方式通过
// 隐藏的 powershell.exe 启动 `mcpx.exe desktop`，因此用户双击 MCPX 时不会
// 看到一个长期驻留的控制台窗口，同时又不影响命令行使用。
func ensureDesktopShortcut() (string, error) {
	executable, err := selfExecutable()
	if err != nil {
		return "", err
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return "", fmt.Errorf("resolve MCPX executable path: %w", err)
	}

	workingDir := filepath.Dir(executable)
	launcherArgs := `-NoProfile -NonInteractive -WindowStyle Hidden -Command "& ` +
		psSingleQuoted(executable) + ` desktop"`

	// CreateShortcut is exposed through the built-in WScript.Shell COM object.
	// [Environment]::GetFolderPath('Desktop') respects OneDrive/redirected Desktop
	// locations instead of assuming %USERPROFILE%\Desktop.
	script := strings.Join([]string{
		`$ErrorActionPreference='Stop'`,
		`$desktop=[Environment]::GetFolderPath('Desktop')`,
		`if ([string]::IsNullOrWhiteSpace($desktop)) { throw 'Desktop folder is unavailable' }`,
		`$link=Join-Path $desktop ` + psSingleQuoted(desktopShortcutFilename),
		`$shell=New-Object -ComObject WScript.Shell`,
		`$shortcut=$shell.CreateShortcut($link)`,
		`$shortcut.TargetPath=(Join-Path $env:SystemRoot 'System32\WindowsPowerShell\v1.0\powershell.exe')`,
		`$shortcut.Arguments=` + psSingleQuoted(launcherArgs),
		`$shortcut.WorkingDirectory=` + psSingleQuoted(workingDir),
		`$shortcut.IconLocation=` + psSingleQuoted(executable+",0"),
		`$shortcut.Description='MCPX Desktop'`,
		`$shortcut.WindowStyle=7`,
		`$shortcut.Save()`,
		`Write-Output $link`,
	}, "; ")

	command := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden", "-Command", script)
	winproc.ConfigureNoWindow(command)
	command.Env = os.Environ()
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("create MCPX desktop shortcut: %w: %s", err, strings.TrimSpace(string(output)))
	}
	shortcutPath := strings.TrimSpace(string(output))
	if shortcutPath == "" {
		return "", fmt.Errorf("create MCPX desktop shortcut: PowerShell returned no path")
	}
	return shortcutPath, nil
}

func psSingleQuoted(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}
