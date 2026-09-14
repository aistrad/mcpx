package operation

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestReviewFanoutDoesNotBlockWorker(t *testing.T) {
	for _, workers := range []int{1, 4} {
		t.Run(fmt.Sprint(workers), func(t *testing.T) {
			s := newTestService(t, workers)
			steps := []StepSpec{{ID: "root", Tool: "edit", Exclusive: true}}
			for i := 0; i < 12; i++ {
				steps = append(steps, StepSpec{ID: fmt.Sprint(i), Tool: "edit", Exclusive: true, DependsOn: []string{"root"}})
			}
			var calls atomic.Int32
			r, err := s.Submit(context.Background(), SubmitSpec{RemoteSessionID: "session", WorkspaceName: "workspace", Steps: steps}, func(context.Context, ExecuteInput) ExecuteResult { calls.Add(1); return ExecuteResult{} })
			if err != nil {
				t.Fatal(err)
			}
			final, timeout, err := s.Wait(context.Background(), r.ID, 3*time.Second)
			if err != nil || timeout || final.State != StateSucceeded || calls.Load() != 13 {
				t.Fatalf("fanout 未完成: state=%s calls=%d timeout=%v err=%v", final.State, calls.Load(), timeout, err)
			}
		})
	}
}

func TestReviewConcurrentResumeHasSingleWinner(t *testing.T) {
	s := newTestService(t, 4)
	var calls atomic.Int32
	execute := func(_ context.Context, in ExecuteInput) ExecuteResult {
		calls.Add(1)
		if in.Arguments["user_confirmed"] != true {
			return ExecuteResult{WaitingConfirmation: true}
		}
		return ExecuteResult{}
	}
	r, err := s.Submit(context.Background(), SubmitSpec{RemoteSessionID: "session", WorkspaceName: "workspace", Steps: []StepSpec{{ID: "a", Tool: "execute"}}}, execute)
	if err != nil {
		t.Fatal(err)
	}
	waiting, _, err := s.Wait(context.Background(), r.ID, time.Second)
	if err != nil || waiting.State != StateWaitingConfirmation {
		t.Fatal("未进入等待")
	}
	token := waiting.Steps[0].ConfirmationToken
	if token == "" {
		t.Fatal("缺少可恢复确认标识")
	}
	var wg sync.WaitGroup
	var wins atomic.Int32
	start := make(chan struct{})
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := s.Resume(context.Background(), r.ID, "a", token, execute); err == nil {
				wins.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	final, timeout, err := s.Wait(context.Background(), r.ID, time.Second)
	if err != nil || timeout || final.State != StateSucceeded || wins.Load() != 1 || calls.Load() != 2 {
		t.Fatalf("resume: wins=%d calls=%d state=%s err=%v", wins.Load(), calls.Load(), final.State, err)
	}
	if _, err := s.Resume(context.Background(), r.ID, "a", token, execute); err == nil {
		t.Fatal("终态重新恢复")
	}
}

func TestReviewRestartPreservesWaitingDependency(t *testing.T) {
	s := newTestService(t, 1)
	execute := func(_ context.Context, in ExecuteInput) ExecuteResult {
		if in.StepID == "a" && in.Arguments["user_confirmed"] != true {
			return ExecuteResult{WaitingConfirmation: true}
		}
		return ExecuteResult{}
	}
	r, err := s.Submit(context.Background(), SubmitSpec{RemoteSessionID: "session", WorkspaceName: "workspace", Steps: []StepSpec{{ID: "a", Tool: "execute"}, {ID: "b", Tool: "read", DependsOn: []string{"a"}}}}, execute)
	if err != nil {
		t.Fatal(err)
	}
	waiting, _, err := s.Wait(context.Background(), r.ID, time.Second)
	if err != nil || waiting.State != StateWaitingConfirmation {
		t.Fatal("未等待")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(s.db, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	var effects atomic.Int32
	_, err = restarted.Resume(context.Background(), r.ID, "a", waiting.Steps[0].ConfirmationToken, func(ctx context.Context, in ExecuteInput) ExecuteResult {
		if in.StepID == "b" {
			effects.Add(1)
		}
		return execute(ctx, in)
	})
	if err != nil {
		t.Fatal(err)
	}
	final, timeout, err := restarted.Wait(context.Background(), r.ID, time.Second)
	if err != nil || timeout || final.State != StateSucceeded || effects.Load() != 1 {
		t.Fatalf("restart DAG: %s effects=%d err=%v", final.State, effects.Load(), err)
	}
}

func TestReviewCancelWaitsForWorkspaceBlockedStep(t *testing.T) {
	s := newTestService(t, 1)
	lock := s.workspaceLock("workspace")
	lock.Lock()
	var calls atomic.Int32
	r, err := s.Submit(context.Background(), SubmitSpec{RemoteSessionID: "session", WorkspaceName: "workspace", Steps: []StepSpec{{ID: "a", Tool: "edit", Exclusive: true}}}, func(context.Context, ExecuteInput) ExecuteResult { calls.Add(1); return ExecuteResult{} })
	if err != nil {
		lock.Unlock()
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		s.mu.Lock()
		registered := len(s.active[r.ID].stepCancel) > 0
		s.mu.Unlock()
		if registered {
			break
		}
		if time.Now().After(deadline) {
			lock.Unlock()
			t.Fatal("步骤未注册")
		}
		time.Sleep(time.Millisecond)
	}
	_, err = s.Cancel(context.Background(), r.ID)
	current, _ := s.Get(context.Background(), r.ID)
	if err != nil || current.State.terminal() {
		lock.Unlock()
		t.Fatal("等待锁期间过早终态")
	}
	lock.Unlock()
	final, timeout, err := s.Wait(context.Background(), r.ID, time.Second)
	if err != nil || timeout || final.State != StateCancelled || calls.Load() != 0 {
		t.Fatalf("cancel: state=%s calls=%d err=%v", final.State, calls.Load(), err)
	}
}

func TestReviewResumeCancelRaceDoesNotResurrectStep(t *testing.T) {
	s := newTestService(t, 4)
	for trial := 0; trial < 12; trial++ {
		execute := func(_ context.Context, in ExecuteInput) ExecuteResult {
			if in.Arguments["user_confirmed"] != true {
				return ExecuteResult{WaitingConfirmation: true}
			}
			return ExecuteResult{}
		}
		r, err := s.Submit(context.Background(), SubmitSpec{RemoteSessionID: "session", WorkspaceName: "workspace", Steps: []StepSpec{{ID: "a", Tool: "execute"}}}, execute)
		if err != nil {
			t.Fatal(err)
		}
		waiting, _, err := s.Wait(context.Background(), r.ID, time.Second)
		if err != nil || waiting.State != StateWaitingConfirmation {
			t.Fatal("未等待")
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			s.Resume(context.Background(), r.ID, "a", waiting.Steps[0].ConfirmationToken, execute)
		}()
		go func() { defer wg.Done(); <-start; s.Cancel(context.Background(), r.ID) }()
		close(start)
		wg.Wait()
		final, timeout, err := s.Wait(context.Background(), r.ID, time.Second)
		if err != nil || timeout || !final.State.terminal() || !final.Steps[0].State.terminal() {
			t.Fatalf("终态父项留下未完成步骤: %s/%s %v", final.State, final.Steps[0].State, err)
		}
		if _, err := s.Resume(context.Background(), r.ID, "a", waiting.Steps[0].ConfirmationToken, execute); err == nil {
			t.Fatal("迟到恢复复活步骤")
		}
	}
}
