package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestIssue823ArgvChild(t *testing.T) {
	for i, arg := range os.Args {
		if arg != "--issue823-argv-child" {
			continue
		}
		f, err := os.OpenFile(os.Args[i+1], os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			os.Exit(2)
		}
		err = json.NewEncoder(f).Encode(os.Args[i+2:])
		_ = f.Close()
		if err != nil {
			os.Exit(3)
		}
		os.Exit(0)
	}
}

func TestExecuteArgvRejectsMalformedPayload(t *testing.T) {
	for _, payload := range []map[string]any{
		{"argv": []any{"git"}}, {"argv": []any{"git"}, "shell": true},
		{"argv": []any{}, "shell": false}, {"argv": []any{42}, "shell": false},
		{"argv": []any{""}, "shell": false}, {"argv": []any{"git", "bad\x00arg"}, "shell": false},
		{"argv": []any{"git"}, "shell": false, "command": "git status"},
		{"argv": []any{"git"}, "shell": false, "runtime": "node"}, {"shell": false},
	} {
		if _, _, _, err := executeArgv(payload); err == nil {
			t.Fatalf("错误接受参数: %+v", payload)
		}
	}
}

func TestExecuteArgvPreservesWindowsParametersAndReplaysOnce(t *testing.T) {
	rt := newWorkspaceRuntime(t, "中文 workspace")
	rt.cfg.Security.Commands.Default = "confirm"
	rt.cfg.Security.Commands.Allow = nil
	session := operationTestSession(t, rt, "中文 workspace")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(session.WorkspacePath, "child-argv.jsonl")
	want := []string{"中文提交 消息", "", `a"b`, `C:\含空格 路径\`, `.items[] | select(.name == "中文")`, "$(literal); & >", "single'quote", "line1\nline2"}
	argv := []any{executable, "-test.run=^TestIssue823ArgvChild$", "--", "--issue823-argv-child", output}
	for _, value := range want {
		argv = append(argv, value)
	}
	payload := map[string]any{"remote_session_id": session.ID, "action": "run", "purpose": "验证真实子进程参数及单次效果", "scope": "workspace", "argv": argv, "shell": false, "idempotency_key": "issue823-argv-1"}
	first := callEnvelope(t, rt.toolExecute, context.Background(), payload)
	if first["status"] != "waiting_confirmation" {
		t.Fatalf("未经过确认: %+v", first)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("确认前产生副作用")
	}
	payload["user_confirmed"] = true
	changed := cloneMap(payload)
	changedArgv := append([]any(nil), argv...)
	changedArgv[len(changedArgv)-1] = "不同参数"
	changed["argv"] = changedArgv
	before := callEnvelope(t, rt.toolExecute, context.Background(), changed)
	if before["status"] != "waiting_confirmation" {
		t.Fatalf("变更参数复用了旧确认: %+v", before)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("变更参数未经确认产生副作用")
	}
	for attempt := range 2 {
		result := callEnvelope(t, rt.toolExecute, context.Background(), payload)
		if !statusOK(result) {
			t.Fatalf("已确认参数无法执行或重放: %+v", result)
		}
		if attempt == 1 && result["data"].(map[string]any)["idempotent_replay"] != true {
			t.Fatalf("缺少重放回执: %+v", result)
		}
	}
	content, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	// 若重复执行会出现两个 JSON 文档，Unmarshal 必须失败。
	if err := json.Unmarshal(content, &got); err != nil {
		t.Fatalf("效果不唯一: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("参数漂移: got=%q want=%q", got, want)
	}
	conflict := callEnvelope(t, rt.toolExecute, context.Background(), changed)
	if statusOK(conflict) || errorCode(conflict) != "idempotency_conflict" {
		t.Fatalf("同键变参未拒绝: %+v", conflict)
	}
}

func TestExecuteArgvPolicyDenyPreventsProcess(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	rt.cfg.Security.Commands.Deny = []string{`^git\b`}
	session := operationTestSession(t, rt, "demo")
	result := callEnvelope(t, rt.toolExecute, context.Background(), map[string]any{
		"remote_session_id": session.ID, "action": "run", "purpose": "拒绝策略验证", "argv": []any{"git", "--version"}, "shell": false,
	})
	if statusOK(result) || result["status"] == "waiting_confirmation" {
		t.Fatalf("拒绝策略失效: %+v", result)
	}
}
