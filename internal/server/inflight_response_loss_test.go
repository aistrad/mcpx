package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"mcpx/internal/operation"
)

func TestStreamableHTTPInFlightResponseLossDoesNotResubmitEffect(t *testing.T) {
	rt := newWorkspaceRuntime(t, "loss")
	session := operationTestSession(t, rt, "loss")
	rt.cfg.Security.Commands.Default = "allow"
	protocol := mcp.NewServer(&mcp.Implementation{Name: "inflight-test", Version: "test"}, nil)
	rt.registerTools(protocol)
	gateway := NewGateway(rt.cfg, nil, mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return protocol }, &mcp.StreamableHTTPOptions{DisableLocalhostProtection: true, Stateless: true})).Handler()
	var dropNext atomic.Bool
	var droppedBeforeTerminal atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && dropNext.CompareAndSwap(true, false) {
			recorder := httptest.NewRecorder()
			gateway.ServeHTTP(recorder, r)
			// 请求已进入真实 Runtime 并持久化，但不向客户端交付响应。
			state, err := rt.operations.Get(context.Background(), "loss-operation")
			if err == nil && (state.State == operation.StateQueued || state.State == operation.StateRunning) {
				droppedBeforeTerminal.Store(true)
			}
			connection, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				connection.Close()
			}
			return
		}
		gateway.ServeHTTP(w, r)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	connect := func() *mcp.ClientSession {
		t.Helper()
		client, err := mcp.NewClient(&mcp.Implementation{Name: "loss-client", Version: "test"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return client
	}
	first := connect()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	started := filepath.Join(session.WorkspacePath, "started")
	effect := filepath.Join(session.WorkspacePath, "effect")
	args := map[string]any{"remote_session_id": session.ID, "purpose": "在途丢回执恢复", "operation_id": "loss-operation", "run_id": "loss-run", "operations": []any{map[string]any{"id": "effect", "tool": "execute", "arguments": map[string]any{"action": "run", "yield_time_ms": 1, "argv": []any{executable, "-test.run=^TestReviewDelayedChild$", "--", "--review-delayed-child", started, effect}, "shell": false}}}}
	dropNext.Store(true)
	_, lostErr := first.CallTool(ctx, &mcp.CallToolParams{Name: "operation_batch", Arguments: args})
	first.Close()
	if lostErr == nil || !droppedBeforeTerminal.Load() {
		t.Fatalf("未建立在途响应丢失: err=%v in_flight=%v", lostErr, droppedBeforeTerminal.Load())
	}
	second := connect()
	defer second.Close()
	for _, request := range []*mcp.CallToolParams{
		{Name: "session", Arguments: map[string]any{"action": "open", "remote_session_id": session.ID}},
		{Name: "operation_batch", Arguments: args},
		{Name: "operation_manage", Arguments: map[string]any{"remote_session_id": session.ID, "operation_id": "loss-operation", "action": "wait", "timeout_ms": 5000}},
	} {
		result, err := second.CallTool(ctx, request)
		if err != nil || result.IsError {
			t.Fatalf("恢复调用 %s 失败: %v", request.Name, err)
		}
	}
	final, err := rt.operations.Get(ctx, "loss-operation")
	if err != nil || final.State != operation.StateSucceeded {
		t.Fatalf("恢复未成功: %s %v", final.State, err)
	}
	content, err := os.ReadFile(effect)
	if err != nil || string(content) != "late" {
		t.Fatalf("实际效果缺失: %v", err)
	}
	var taskCount int
	if err := rt.state.DB().QueryRow(`SELECT COUNT(*) FROM terminal_tasks WHERE remote_session_id=?`, session.ID).Scan(&taskCount); err != nil || taskCount != 1 {
		t.Fatalf("重复执行: tasks=%d err=%v", taskCount, err)
	}
	t.Logf("in_flight_response_dropped=true remote_session=%s operation=%s tasks=%d final=%s", session.ID, final.ID, taskCount, final.State)
}
