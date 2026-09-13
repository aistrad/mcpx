package operation

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestStateIdentitySurvivesRestartAndRejectsConflictingTerminal(t *testing.T) {
	s := newTestService(t, 1)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	_, err := s.db.Exec(`INSERT INTO operations (id, remote_session_id, workspace_name, request_id, purpose, state,
		result_json, error_json, created_at, expires_at) VALUES ('crashed', 'session', 'workspace', 'req', '恢复测试',
		'running', '{}', '{}', ?, ?)`, now, now+60000)
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.Get(context.Background(), "crashed")
	if err != nil || before.StateSequence != 1 || before.StateEventID == "" {
		t.Fatalf("初始状态身份缺失: %+v %v", before, err)
	}
	recovered, err := New(s.db, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recovered.Close() })
	terminal, err := recovered.Get(context.Background(), "crashed")
	if err != nil || terminal.State != StateInterrupted || terminal.StateSequence != 2 || terminal.StateEventID == before.StateEventID {
		t.Fatalf("恢复终态未持久推进: %+v %v", terminal, err)
	}
	var attempts sync.WaitGroup
	for range 8 {
		attempts.Add(1)
		go func() {
			defer attempts.Done()
			if _, err := s.db.Exec(`UPDATE operations SET state='succeeded' WHERE id='crashed'`); err == nil {
				t.Error("冲突终态被接受")
			}
		}()
	}
	attempts.Wait()
	if _, err := s.db.Exec(`UPDATE operations SET state='interrupted' WHERE id='crashed'`); err != nil {
		t.Fatal(err)
	}
	if err := recovered.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := New(s.db, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	readback, err := again.Get(context.Background(), "crashed")
	if err != nil || readback.StateSequence != terminal.StateSequence || readback.StateEventID != terminal.StateEventID || readback.State != StateInterrupted {
		t.Fatalf("重复完成或重启改变终态身份: %+v %v", readback, err)
	}
}

func TestStableSubmissionResponseLossAndConcurrentRetryExecuteOnce(t *testing.T) {
	s := newTestService(t, 2)
	var executions atomic.Int32
	executor := func(context.Context, ExecuteInput) ExecuteResult {
		executions.Add(1)
		return ExecuteResult{Result: []byte(`{"effect":"one"}`)}
	}
	spec := SubmitSpec{ID: "stable-op", RunID: "stable-run", RemoteSessionID: "session", WorkspaceName: "workspace",
		Purpose: "重试实际发送去重", Steps: []StepSpec{{ID: "effect", Tool: "edit", Arguments: map[string]any{"text": "中文"}}}}
	var callers sync.WaitGroup
	for range 8 {
		callers.Add(1)
		go func() {
			defer callers.Done()
			if _, err := s.Submit(context.Background(), spec, executor); err != nil {
				t.Errorf("并发同计划恢复失败: %v", err)
			}
		}()
	}
	callers.Wait()
	final, timedOut, err := s.Wait(context.Background(), spec.ID, time.Second)
	if err != nil || timedOut || final.State != StateSucceeded || final.RunID != spec.RunID || executions.Load() != 1 {
		t.Fatalf("重复执行或终态缺失: count=%d state=%s err=%v", executions.Load(), final.State, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(s.db, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	spec.RequestID = "new-transport-request"
	replay, err := restarted.Submit(context.Background(), spec, executor)
	if err != nil || replay.StateEventID != final.StateEventID || replay.StateSequence != final.StateSequence || executions.Load() != 1 {
		t.Fatalf("丢响应后重启恢复未复用终态: %+v %v", replay, err)
	}
	for _, changed := range []SubmitSpec{
		{ID: spec.ID, RunID: "other-run", RemoteSessionID: spec.RemoteSessionID, WorkspaceName: spec.WorkspaceName, Purpose: spec.Purpose, Steps: spec.Steps},
		{ID: spec.ID, RunID: spec.RunID, RemoteSessionID: "other-session", WorkspaceName: spec.WorkspaceName, Purpose: spec.Purpose, Steps: spec.Steps},
		{ID: spec.ID, RunID: spec.RunID, RemoteSessionID: spec.RemoteSessionID, WorkspaceName: spec.WorkspaceName, Purpose: "different-purpose", Steps: spec.Steps},
	} {
		if _, err := restarted.Submit(context.Background(), changed, executor); !errors.Is(err, ErrInvalidSpec) {
			t.Fatalf("同 ID 变更计划未拒绝: %v", err)
		}
	}
	if executions.Load() != 1 {
		t.Fatal("拒绝后产生额外执行")
	}
	// 模拟常规结果保留期清理；提交墓碑继续阻止同一会话中的旧请求重执行。
	if _, err := s.db.Exec(`DELETE FROM operations WHERE id=?`, spec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Submit(context.Background(), spec, executor); !errors.Is(err, ErrResultExpired) {
		t.Fatalf("结果过期后允许重执行: %v", err)
	}
	if executions.Load() != 1 {
		t.Fatal("过期恢复产生额外执行")
	}
}

func TestTerminalNotificationMatchesDurableStatusIdentity(t *testing.T) {
	s := newTestService(t, 1)
	events := make(chan Event, 8)
	s.sink = func(event Event) { events <- event }
	record, err := s.Submit(context.Background(), SubmitSpec{
		RemoteSessionID: "session", WorkspaceName: "workspace", RequestID: "request", Purpose: "状态事件测试",
		Steps: []StepSpec{{ID: "read", Tool: "source_read"}},
	}, func(context.Context, ExecuteInput) ExecuteResult { return ExecuteResult{Result: []byte(`{"ok":true}`)} })
	if err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case event := <-events:
			if event.Type != operationEventCompleted {
				continue
			}
			current, err := s.Get(context.Background(), record.ID)
			if err != nil || current.StateEventID == "" || current.StateSequence < 2 || event.StateEventID != current.StateEventID || event.StateSequence != current.StateSequence {
				t.Fatalf("通知与状态身份不一致: event=%+v current=%+v err=%v", event, current, err)
			}
			return
		case <-time.After(3 * time.Second):
			t.Fatal("未收到完成事件")
		}
	}
}

func TestCancellationHasNoTerminalUntilExecutorActuallyExits(t *testing.T) {
	s := newTestService(t, 1)
	started, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer close(release)
	record, err := s.Submit(context.Background(), SubmitSpec{
		RemoteSessionID: "session", WorkspaceName: "workspace", Purpose: "延迟取消退出",
		Steps: []StepSpec{{ID: "slow", Tool: "command_run"}},
	}, func(ctx context.Context, _ ExecuteInput) ExecuteResult {
		close(started)
		<-ctx.Done()
		close(cancelled)
		<-release
		return ExecuteResult{Err: ctx.Err()}
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("执行器未启动")
	}
	before, err := s.Cancel(context.Background(), record.ID)
	if err != nil || before.State.terminal() {
		t.Fatalf("步骤未退出却提前给终态: %s %v", before.State, err)
	}
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("执行器未收到取消")
	}
	stillRunning, timedOut, err := s.Wait(context.Background(), record.ID, time.Millisecond)
	if err != nil || !timedOut || stillRunning.State.terminal() {
		t.Fatalf("等待未退出步骤错误: %s %v", stillRunning.State, err)
	}
	// 使用一次发送解除阻塞，让 defer close 仍可负责异常退出清理。
	release <- struct{}{}
	final, timedOut, err := s.Wait(context.Background(), record.ID, time.Second)
	if err != nil || timedOut || final.State != StateCancelled || final.StateSequence <= before.StateSequence || final.StateEventID == before.StateEventID {
		t.Fatalf("取消终态缺失或未推进: %+v %v", final, err)
	}
}
