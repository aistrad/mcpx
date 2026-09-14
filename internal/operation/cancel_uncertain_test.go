package operation

import (
	"context"
	"testing"
	"time"
)

func TestReviewUnconfirmedStopCannotBecomeCancelled(t *testing.T) {
	s := newTestService(t, 1)
	started := make(chan struct{})
	r, err := s.Submit(context.Background(), SubmitSpec{RemoteSessionID: "session", WorkspaceName: "workspace", Steps: []StepSpec{{ID: "a", Tool: "execute"}}}, func(ctx context.Context, _ ExecuteInput) ExecuteResult {
		close(started)
		<-ctx.Done()
		return ExecuteResult{Err: ErrEffectsUnconfirmed}
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("未开始")
	}
	if _, err := s.Cancel(context.Background(), r.ID); err != nil {
		t.Fatal(err)
	}
	final, timeout, err := s.Wait(context.Background(), r.ID, time.Second)
	if err != nil || timeout || final.State != StateInterrupted || final.Steps[0].State != StateInterrupted {
		t.Fatalf("未知停止被误报取消完成: %+v %v", final, err)
	}
}
