package operation

import (
	"context"
	"testing"
	"time"
)

func TestWaitMissingHandleDoesNotProveTerminal(t *testing.T) {
	s := newTestService(t, 1)
	now := time.Now().UnixMilli()
	_, err := s.db.Exec(`INSERT INTO operations (id, remote_session_id, workspace_name, request_id, purpose, state,
		result_json, error_json, created_at, expires_at) VALUES ('missing-handle', 'session', 'workspace', 'req', '等待身份测试',
		'running', '{}', '{}', ?, ?)`, now, now+60000)
	if err != nil {
		t.Fatal(err)
	}
	record, timedOut, err := s.Wait(context.Background(), "missing-handle", time.Second)
	if err != nil || !timedOut || record.State != StateRunning {
		t.Fatalf("缺失句柄被当作等待完成: state=%s timedOut=%v err=%v", record.State, timedOut, err)
	}
	if _, err := s.db.Exec(`UPDATE operations SET state='succeeded' WHERE id='missing-handle'`); err != nil {
		t.Fatal(err)
	}
	terminal, timedOut, err := s.Wait(context.Background(), "missing-handle", time.Second)
	if err != nil || timedOut || terminal.State != StateSucceeded || terminal.StateEventID == record.StateEventID {
		t.Fatalf("持久终态没有正确回读: state=%s timedOut=%v err=%v", terminal.State, timedOut, err)
	}
}
