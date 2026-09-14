//go:build windows

package desktop

import (
	"strings"
	"testing"
)

func TestPSSingleQuotedEscapesApostrophes(t *testing.T) {
	got := psSingleQuoted(`C:\Users\O'Brien\mcpx.exe`)
	want := `'C:\Users\O''Brien\mcpx.exe'`
	if got != want {
		t.Fatalf("psSingleQuoted() = %q, want %q", got, want)
	}
}

func TestDesktopShortcutUsesHiddenPowerShellLauncher(t *testing.T) {
	executable := `C:\Program Files\MCPX\mcpx.exe`
	args := `-NoProfile -NonInteractive -WindowStyle Hidden -Command "& ` + psSingleQuoted(executable) + ` desktop"`
	for _, required := range []string{"-WindowStyle Hidden", "mcpx.exe", " desktop"} {
		if !strings.Contains(args, required) {
			t.Fatalf("launcher args %q missing %q", args, required)
		}
	}
}
