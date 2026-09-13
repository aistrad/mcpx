package environment

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestInspectToolchainsRunsAvailableProbesConcurrently(t *testing.T) {
	started := make(chan string, 16)
	release := make(chan struct{})
	done := make(chan map[string]ToolchainInfo, 1)

	go func() {
		done <- inspectToolchainsWith(
			context.Background(),
			func(string) (string, error) { return "available", nil },
			func(_ context.Context, name string, _ ...string) string {
				started <- name
				<-release
				return name + " version"
			},
		)
	}()

	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			close(release)
			t.Fatal("toolchain probes did not overlap")
		}
	}
	close(release)

	select {
	case result := <-done:
		if len(result) != 11 {
			t.Fatalf("toolchain result count = %d, want 11", len(result))
		}
		for name, info := range result {
			if !info.Available || info.Version == "" {
				t.Fatalf("toolchain %q = %+v", name, info)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("toolchain probes did not complete")
	}
}

func TestInspectToolchainsKeepsUnavailableEntries(t *testing.T) {
	result := inspectToolchainsWith(
		context.Background(),
		func(string) (string, error) { return "", errors.New("not found") },
		func(context.Context, string, ...string) string {
			t.Fatal("unavailable toolchain must not be executed")
			return ""
		},
	)
	if len(result) != 11 {
		t.Fatalf("toolchain result count = %d, want 11", len(result))
	}
	for name, info := range result {
		if info.Available || info.Version != "" {
			t.Fatalf("toolchain %q = %+v, want unavailable", name, info)
		}
	}
}
