//go:build !windows

package skill

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func nativeFixture(t *testing.T) (string, Skill) {
	t.Helper()
	root := t.TempDir()
	directory := filepath.Join(root, "native-fixture")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: native-fixture\ndescription: Native protocol fixture.\nmcpx:\n  protocol: json_stdio_v1\n  entry: entry.sh\n  timeout_seconds: 2\n---\nOriginal instructions.\n"
	if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	shell := "#!/bin/bash\nexec '" + strings.ReplaceAll(executable, "'", "'\"'\"'") + "' -test.run=^TestNativeProtocolHelper$ -- --native-fixture\n"
	if err := os.WriteFile(filepath.Join(directory, "entry.sh"), []byte(shell), 0600); err != nil {
		t.Fatal(err)
	}
	found := LoadConfigured(nil, []string{directory}, root)
	if len(found) != 1 || !found[0].NativeApproved {
		t.Fatalf("approved discovery=%+v", found)
	}
	return root, found[0]
}

func TestNativeProtocolHelper(t *testing.T) {
	if len(os.Args) == 0 || os.Args[len(os.Args)-1] != "--native-fixture" {
		return
	}
	var request map[string]any
	if err := json.NewDecoder(os.Stdin).Decode(&request); err != nil {
		os.Exit(3)
	}
	arguments, _ := request["arguments"].(map[string]any)
	if arguments["mode"] == "timeout" {
		time.Sleep(10 * time.Second)
	}
	if arguments["mode"] == "oversize" {
		fmt.Print(strings.Repeat("x", NativeMaxOutputBytes+1))
		os.Exit(0)
	}
	if arguments["mode"] == "error" {
		fmt.Print(`{"status":"error","error":{"code":"OWNER_REJECTED","message":"fixture","retryable":false}}`)
		os.Exit(7)
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"status": "prepared", "request": request, "secret": os.Getenv("MCPX_NATIVE_TEST_SECRET"), "allowed": os.Getenv("MCPX_NATIVE_TEST_ALLOWED")})
	os.Exit(0)
}

func TestNativeRequiresApprovalAndRejectsUnsafeEntry(t *testing.T) {
	root, approved := nativeFixture(t)
	discovered := LoadConfigured([]string{root}, nil, root)
	if len(discovered) != 1 || discovered[0].NativeApproved {
		t.Fatalf("ordinary discovery=%+v", discovered)
	}
	if _, err := ExecuteNative(context.Background(), discovered[0], "call", nil, NativeRuntimeContext{}, nil); err == nil {
		t.Fatal("unapproved native code executed")
	}
	for _, entry := range []string{"../entry.sh", "/bin/bash"} {
		candidate := approved
		manifest := *approved.Manifest.MCPX
		manifest.Entry = entry
		candidate.Manifest.MCPX = &manifest
		if _, err := ResolveNativeEntry(candidate); err == nil {
			t.Fatalf("unsafe entry %q accepted", entry)
		}
	}
	if err := os.Symlink(filepath.Join(approved.Dir, "entry.sh"), filepath.Join(approved.Dir, "link.sh")); err != nil {
		t.Fatal(err)
	}
	manifest := *approved.Manifest.MCPX
	manifest.Entry = "link.sh"
	approved.Manifest.MCPX = &manifest
	if _, err := ResolveNativeEntry(approved); err == nil {
		t.Fatal("symlink entry accepted")
	}
}

func TestNativeJSONStdinEnvironmentErrorsAndBounds(t *testing.T) {
	_, sk := nativeFixture(t)
	t.Setenv("MCPX_NATIVE_TEST_SECRET", "must-not-leak")
	t.Setenv("MCPX_NATIVE_TEST_ALLOWED", "yes")
	rc := NativeRuntimeContext{RemoteSessionID: "session", Workspace: "workspace", RequestID: "request"}
	large := strings.Repeat("中文\"$(touch NEVER_EXECUTE)\n", 9000)
	output, err := ExecuteNative(context.Background(), sk, "call", map[string]any{"text": large}, rc, []string{"MCPX_NATIVE_TEST_ALLOWED"})
	if err != nil {
		t.Fatal(err)
	}
	if output["secret"] != "" || output["allowed"] != "yes" {
		t.Fatalf("environment filtering=%+v", output)
	}
	request := output["request"].(map[string]any)
	if request["arguments"].(map[string]any)["text"] != large {
		t.Fatal("JSON stdin changed input")
	}
	if request["runtime_context"].(map[string]any)["remote_session_id"] != "session" {
		t.Fatal("Runtime context lost")
	}
	output, err = ExecuteNative(context.Background(), sk, "call", map[string]any{"mode": "error"}, rc, nil)
	if err != nil || output["error"].(map[string]any)["code"] != "OWNER_REJECTED" {
		t.Fatalf("structured owner error=%+v,%v", output, err)
	}
	for _, arguments := range []map[string]any{{"runtime_context": map[string]any{}}, {"text": strings.Repeat("x", NativeMaxInputBytes)}} {
		if _, err := ExecuteNative(context.Background(), sk, "call", arguments, rc, nil); err == nil {
			t.Fatal("invalid/oversized input admitted")
		}
	}
	if _, err := ExecuteNative(context.Background(), sk, "call", map[string]any{"mode": "oversize"}, rc, nil); err == nil {
		t.Fatal("oversized stdout silently truncated")
	}
	start := time.Now()
	if _, err := ExecuteNative(context.Background(), sk, "call", map[string]any{"mode": "timeout"}, rc, nil); err == nil {
		t.Fatal("timeout was ignored")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("native process timeout was not bounded")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ExecuteNative(ctx, sk, "call", nil, rc, nil); err == nil {
		t.Fatal("cancellation ignored")
	}
}
