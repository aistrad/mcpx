package server

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/envelope"
	"mcpx/internal/mcpresult"
	"mcpx/internal/observation"
	"mcpx/internal/remotesession"
)

func TestProgressRecordsPauseStateAndPreviousResult(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	principal, err := rt.principalFromContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	registered, ok := rt.reg.Get("demo")
	if !ok {
		t.Fatal("workspace was not registered")
	}
	created, err := rt.remote.Create(context.Background(), principal, remotesession.CreateInput{WorkspaceName: "demo", WorkspacePath: registered.Path})
	if err != nil {
		t.Fatal(err)
	}
	request := mcpresult.Request(map[string]any{
		"purpose": "告知用户当前进度并等待确认", "progress_summary": "已完成文件读取，当前不再继续调用工具",
		"remote_session_id": created.Session.ID, "current": "已定位到供应商表单问题", "result": []any{"read 返回了目标字段代码"},
		"status": "waiting_for_user", "next": "请用户确认是否继续修改", "related_tool": "read",
	})
	wrapped := rt.instrumentTool("progress", rt.toolProgress)
	result, err := wrapped(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("progress returned an error: %+v", result)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok || !strings.Contains(text.Text, "已定位到供应商表单问题") || !strings.Contains(text.Text, "请用户确认是否继续修改") {
		t.Fatalf("progress display=%v", result.Content)
	}
	var started, completed bool
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		events, err := rt.observation.store.History(context.Background(), "demo", 0, 50)
		if err != nil {
			t.Fatal(err)
		}
		started, completed = false, false
		for _, event := range events {
			if event.Tool != "progress" {
				continue
			}
			if event.Type == observation.TypeToolStarted {
				started = event.ProgressSummary == "已完成文件读取，当前不再继续调用工具"
			}
			if event.Type == observation.TypeToolCompleted {
				completed = strings.Contains(string(event.Output), "已定位到供应商表单问题") || strings.Contains(event.Summary, "progress")
			}
		}
		if started && completed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !started || !completed {
		t.Fatalf("progress was not fully recorded: started=%v completed=%v", started, completed)
	}
}

func TestProgressAcceptsTerminalFailedStateAndRestoresLatestModelState(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	principal, err := rt.principalFromContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	registered, ok := rt.reg.Get("demo")
	if !ok {
		t.Fatal("workspace was not registered")
	}
	created, err := rt.remote.Create(context.Background(), principal, remotesession.CreateInput{WorkspaceName: "demo", WorkspacePath: registered.Path})
	if err != nil {
		t.Fatal(err)
	}
	wrapped := rt.instrumentTool("progress", rt.toolProgress)
	result, err := wrapped(context.Background(), mcpresult.Request(map[string]any{
		"purpose": "向用户汇报当前任务因验证失败而停止", "remote_session_id": created.Session.ID, "status": "failed",
		"current": "全仓测试仍有两个失败", "result": []any{"internal/server 两个 case 未通过"}, "next": "修复失败用例后重新验证", "phase": "verification",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("progress returned an error: %+v", result)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok || !strings.Contains(text.Text, "全仓测试仍有两个失败") || !strings.Contains(text.Text, "results: 1") {
		t.Fatalf("progress display=%v", result.Content)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		page, queryErr := rt.observation.store.QueryMemory(context.Background(), observation.MemoryQuery{Workspace: "demo", SessionID: created.Session.ID, Type: "progress", Latest: 1})
		if queryErr != nil {
			t.Fatal(queryErr)
		}
		if len(page.Items) == 1 && page.Items[0].Status == "failed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("progress was not persisted: %+v", page)
		}
		time.Sleep(10 * time.Millisecond)
	}
	attached := callEnvelope(t, rt.toolSessionOpen, context.Background(), map[string]any{"remote_session_id": created.Session.ID})
	latest, ok := attached["data"].(map[string]any)["latest_model_state"].(map[string]any)
	if !ok || latest["status"] != "failed" || latest["summary"] != "全仓测试仍有两个失败" {
		t.Fatalf("latest model state=%+v", attached["data"])
	}
}

func TestProgressAllowsResultUpToConfiguredLimit(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	principal, err := rt.principalFromContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	registered, ok := rt.reg.Get("demo")
	if !ok {
		t.Fatal("workspace was not registered")
	}
	created, err := rt.remote.Create(context.Background(), principal, remotesession.CreateInput{WorkspaceName: "demo", WorkspacePath: registered.Path})
	if err != nil {
		t.Fatal(err)
	}
	wrapped := rt.instrumentTool("progress", rt.toolProgress)
	arguments := map[string]any{
		"purpose": "验证 progress 结果长度限制", "remote_session_id": created.Session.ID, "current": "已完成长度限制验证",
		"result": []any{strings.Repeat("x", envelope.MaxResultSummaryBytes)}, "status": "completed",
	}
	result, err := wrapped(context.Background(), mcpresult.Request(arguments))
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("progress result at limit was rejected: %+v", result)
	}
	arguments["result"] = []any{strings.Repeat("x", envelope.MaxResultSummaryBytes+1)}
	result, err = wrapped(context.Background(), mcpresult.Request(arguments))
	if err != nil {
		t.Fatal(err)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	want := fmt.Sprintf("progress result exceeds %d bytes", envelope.MaxResultSummaryBytes)
	if !ok || (!strings.Contains(text.Text, want) && !strings.Contains(fmt.Sprint(result.StructuredContent), want)) {
		t.Fatalf("unexpected over-limit error: text=%q result=%+v", text.Text, result)
	}
}

func TestProgressRejectsLegacyFieldShape(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	principal, err := rt.principalFromContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	registered, _ := rt.reg.Get("demo")
	created, err := rt.remote.Create(context.Background(), principal, remotesession.CreateInput{WorkspaceName: "demo", WorkspacePath: registered.Path})
	if err != nil {
		t.Fatal(err)
	}
	result, err := rt.instrumentTool("progress", rt.toolProgress)(context.Background(), mcpresult.Request(map[string]any{
		"remote_session_id": created.Session.ID, "summary": "旧字段不再接受", "status": "completed",
	}))
	if err != nil {
		t.Fatal(err)
	}
	text, _ := result.Content[0].(*mcp.TextContent)
	if text == nil || (!strings.Contains(text.Text, "progress current is required") && !strings.Contains(fmt.Sprint(result.StructuredContent), "progress current is required")) {
		t.Fatalf("legacy field shape was not rejected: %+v", result)
	}
}
