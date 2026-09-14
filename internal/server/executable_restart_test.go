package server

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"mcpx/internal/config"
	"mcpx/internal/winproc"
)

func TestReviewAppendEffectChild(t *testing.T) {
	for i, arg := range os.Args {
		if arg != "--review-append-effect" || i+1 >= len(os.Args) {
			continue
		}
		file, err := os.OpenFile(os.Args[i+1], os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.WriteString("once\n"); err != nil {
			file.Close()
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		return
	}
}

// 制品集成验证显式提供冻结二进制；不读取用户配置、不调用 daemon stop。
func TestReviewActualExecutableKillAndHTTPResume(t *testing.T) {
	binary := os.Getenv("MCPX_REVIEW_EXECUTABLE")
	if binary == "" {
		t.Skip("须显式提供已冻结 MCPX_REVIEW_EXECUTABLE")
	}
	home := t.TempDir()
	workspace := filepath.Join(home, "workspace")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.Auth = config.AuthConfig{Mode: "open"}
	cfg.Security.Commands = config.CommandRules{Default: "confirm"}
	cfg.Workspaces = []config.WorkspaceEntry{{Name: "binary-fixture", Path: workspace}}
	cfg.Discovery.MCP.Enabled = false
	cfg.Discovery.Skills.Enabled = false
	cfg.Discovery.Skills.Dirs = nil
	cfg.FileWatch.Enabled = false
	cfg.Logging.Dir = filepath.Join(home, "logs")
	if err := config.WriteGlobal(filepath.Join(home, "config.yaml"), cfg); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()
	endpoint := "http://" + addr
	start := func() *exec.Cmd {
		t.Helper()
		cmd := exec.Command(binary, "__background-child", "-addr", addr, "-log-level", "error")
		cmd.Dir = home
		for _, item := range os.Environ() {
			if !strings.HasPrefix(strings.ToUpper(item), "MCPX_") {
				cmd.Env = append(cmd.Env, item)
			}
		}
		cmd.Env = append(cmd.Env, "MCPX_HOME="+home)
		winproc.ConfigureNoWindow(cmd)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			connection, err := net.DialTimeout("tcp", addr, time.Second)
			if err == nil {
				connection.Close()
				return cmd
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("隔离候选未就绪")
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	connect := func() *mcp.ClientSession {
		t.Helper()
		client, err := mcp.NewClient(&mcp.Implementation{Name: "binary-review", Version: "test"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint + "/mcp"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { client.Close() })
		return client
	}
	call := func(client *mcp.ClientSession, name string, args map[string]any) map[string]any {
		t.Helper()
		result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		return decodeToolResult(t, result)
	}
	firstProcess := start()
	first := connect()
	opened := call(first, "session", map[string]any{"action": "open", "workspace": "binary-fixture"})
	sessionID, ok := opened["remote_session_id"].(string)
	if !ok || sessionID == "" {
		t.Fatal("缺持久 Session")
	}
	helper, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(workspace, "effects.txt")
	batch := map[string]any{"remote_session_id": sessionID, "purpose": "制品强杀恢复", "run_id": "binary-run", "operation_id": "binary-operation", "operations": []any{
		map[string]any{"id": "a", "tool": "execute", "arguments": map[string]any{"action": "run", "argv": []any{helper, "-test.run=^TestReviewAppendEffectChild$", "--", "--review-append-effect", output}, "shell": false, "idempotency_key": "binary-a"}},
		map[string]any{"id": "b", "tool": "read", "depends_on": []any{"a"}, "arguments": map[string]any{"view": "file", "path": "effects.txt"}},
	}}
	call(first, "operation_batch", batch)
	waitArgs := map[string]any{"remote_session_id": sessionID, "operation_id": "binary-operation", "action": "wait", "timeout_ms": 4000}
	waiting := call(first, "operation_manage", waitArgs)["data"].(map[string]any)
	steps := waiting["steps"].([]any)
	token := steps[0].(map[string]any)["confirmation_token"]
	if waiting["state"] != "waiting_confirmation" || token == nil || steps[1].(map[string]any)["state"] != "queued" {
		t.Fatal("HTTP 未保持确认等待")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("确认前存在效果")
	}
	first.Close()
	if err := firstProcess.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := firstProcess.Wait(); err == nil {
		t.Fatal("未强杀候选")
	}
	start()
	second := connect()
	resumed := call(second, "session", map[string]any{"action": "open", "remote_session_id": sessionID})
	if resumed["remote_session_id"] != sessionID {
		t.Fatal("恢复了不同 Session")
	}
	call(second, "operation_manage", map[string]any{"remote_session_id": sessionID, "operation_id": "binary-operation", "action": "resume", "step_id": "a", "confirmation_token": token})
	final := call(second, "operation_manage", waitArgs)["data"].(map[string]any)
	if final["state"] != "succeeded" || final["terminal_event_id"] == nil {
		t.Fatalf("未完成: %+v", final)
	}
	call(second, "operation_batch", batch)
	replayed := call(second, "operation_manage", waitArgs)["data"].(map[string]any)
	if final["terminal_event_id"] != replayed["terminal_event_id"] || final["state_sequence"] != replayed["state_sequence"] {
		t.Fatal("重复提交改变终态")
	}
	content, err := os.ReadFile(output)
	if err != nil || string(content) != "once\n" {
		t.Fatal("效果非唯一")
	}
	t.Log("actual_executable_killed=true same_session=true http_confirmation_restored=true effect_count=1 stable_terminal=true")
}
