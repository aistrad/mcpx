package server

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"mcpx/internal/mcpresult"
)

func TestReviewFirstEditCompleteFailureRecoversSameStore(t *testing.T) {
	rt := newWorkspaceRuntime(t, "review")
	s := operationTestSession(t, rt, "review")
	if _, err := rt.state.DB().Exec(`CREATE TRIGGER first_complete_failure BEFORE UPDATE ON clean_idempotency_records WHEN NEW.state='succeeded' BEGIN SELECT RAISE(FAIL, 'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"remote_session_id": s.ID, "purpose": "首次完成持久化故障", "idempotency_key": "first-complete", "edits": []any{map[string]any{"path": "once.txt", "operation": "create", "content": "唯一字节\r\n"}}}
	first := callOperationTool(t, rt, "edit", args)
	if first["status"] != "failed" {
		t.Fatalf("故障仍成功: %+v", first)
	}
	path := filepath.Join(s.WorkspacePath, "once.txt")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.state.DB().Exec(`DROP TRIGGER first_complete_failure`); err != nil {
		t.Fatal(err)
	}
	var previous any
	for i := range 2 {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		result, err := rt.toolHandlers["edit"](ctx, mcpresult.Request(args))
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		decoded := decodeToolResult(t, result)
		if decoded["public_status"] != "succeeded" {
			t.Fatalf("同 Store 重试未收敛: %+v", decoded)
		}
		if i == 0 {
			previous = decoded["data"]
		} else if !reflect.DeepEqual(previous, decoded["data"]) {
			t.Fatalf("重复回执不一致: %v / %v", previous, decoded["data"])
		}
	}
	after, err := os.Stat(path)
	if err != nil || !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("同键重试发生第二次文件写入")
	}
}
