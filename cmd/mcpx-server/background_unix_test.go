//go:build !windows

package main

import (
	"testing"
)

func TestBackgroundCommandMatchesExactExecutable(t *testing.T) {
	executable := "/opt/mcpx/bin/mcpx"
	for _, command := range []string{
		executable,
		executable + " -addr 127.0.0.1:9999",
	} {
		if !backgroundCommandMatches(command, executable) {
			t.Fatalf("expected command to match: %q", command)
		}
	}
	for _, command := range []string{
		"/opt/other/bin/mcpx -addr 127.0.0.1:9999",
		executable + "-helper",
	} {
		if backgroundCommandMatches(command, executable) {
			t.Fatalf("unexpected command match: %q", command)
		}
	}
}
