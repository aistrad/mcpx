package operation

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"mcpx/internal/state"
	"mcpx/internal/winproc"
)

func TestReviewCrashFixtureChild(t *testing.T) {
	for i, arg := range os.Args {
		if arg != "--operation-crash-fixture" {
			continue
		}
		store, err := state.Open(os.Args[i+1])
		if err != nil {
			os.Exit(2)
		}
		service, err := New(store.DB(), 1, nil)
		if err != nil {
			os.Exit(3)
		}
		r, err := service.Submit(context.Background(), SubmitSpec{ID: "killed-wait", RemoteSessionID: "session", WorkspaceName: "workspace", Steps: []StepSpec{{ID: "a", Tool: "execute"}, {ID: "b", Tool: "read", DependsOn: []string{"a"}}}}, func(context.Context, ExecuteInput) ExecuteResult {
			return ExecuteResult{WaitingConfirmation: true, ConfirmationToken: "kill-confirm"}
		})
		if err != nil {
			os.Exit(4)
		}
		waiting, timeout, err := service.Wait(context.Background(), r.ID, time.Second)
		if err != nil || timeout || waiting.State != StateWaitingConfirmation {
			os.Exit(5)
		}
		if os.Args[i+3] == "lost-running" {
			if _, err := store.DB().Exec(`UPDATE operation_steps SET state='running' WHERE operation_id='killed-wait' AND step_id='b'`); err != nil {
				os.Exit(7)
			}
		}
		if err := os.WriteFile(os.Args[i+2], []byte("waiting-persisted"), 0600); err != nil {
			os.Exit(6)
		}
		// 不执行 Close：由父测试通过操作系统终止此独立进程。
		for {
			time.Sleep(time.Minute)
		}
	}
}

func TestReviewProcessKillPreservesWaitingDAG(t *testing.T) {
	for _, mode := range []string{"pure-waiting", "lost-running"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newTestService(t, 1)
			if err := fixture.Close(); err != nil {
				t.Fatal(err)
			}
			var seq int
			var name, dbPath string
			if err := fixture.db.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &dbPath); err != nil {
				t.Fatal(err)
			}
			if err := fixture.db.Close(); err != nil {
				t.Fatal(err)
			}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ready := filepath.Join(t.TempDir(), "ready")
			command := exec.Command(executable, "-test.run=^TestReviewCrashFixtureChild$", "--", "--operation-crash-fixture", dbPath, ready, mode)
			winproc.ConfigureNoWindow(command)
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			defer command.Process.Kill()
			deadline := time.Now().Add(5 * time.Second)
			for {
				if _, err := os.Stat(ready); err == nil {
					break
				}
				if time.Now().After(deadline) {
					command.Process.Kill()
					command.Wait()
					t.Fatal("子进程未持久化等待 DAG")
				}
				time.Sleep(5 * time.Millisecond)
			}
			if err := command.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err := command.Wait(); err == nil {
				t.Fatal("未发生进程强制终止")
			}
			reopened, err := state.Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			restored, err := New(reopened.DB(), 1, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			before, err := restored.Get(context.Background(), "killed-wait")
			if mode == "lost-running" {
				if err != nil || before.State != StateInterrupted {
					t.Fatalf("失联图未中断: %+v %v", before, err)
				}
				for _, step := range before.Steps {
					if step.State != StateInterrupted {
						t.Fatalf("步骤未一致中断: %+v", step)
					}
				}
				var calls atomic.Int32
				_, err = restored.Resume(context.Background(), before.ID, "a", "kill-confirm", func(context.Context, ExecuteInput) ExecuteResult { calls.Add(1); return ExecuteResult{} })
				if err == nil || calls.Load() != 0 {
					t.Fatal("失联图被重新执行")
				}
				t.Log("os_process_killed=true database_reopened=true final=interrupted resumed_effects=0")
				return
			}
			if err != nil || before.State != StateWaitingConfirmation || before.Steps[1].State != StateQueued {
				t.Fatalf("强杀恢复破坏 DAG: %+v err=%v", before, err)
			}
			var effects atomic.Int32
			_, err = restored.Resume(context.Background(), before.ID, "a", "kill-confirm", func(_ context.Context, in ExecuteInput) ExecuteResult {
				if in.StepID == "b" {
					effects.Add(1)
				}
				return ExecuteResult{}
			})
			if err != nil {
				t.Fatal(err)
			}
			final, timeout, err := restored.Wait(context.Background(), before.ID, time.Second)
			if err != nil || timeout || final.State != StateSucceeded || effects.Load() != 1 {
				t.Fatalf("强杀恢复失败: %s effects=%d err=%v", final.State, effects.Load(), err)
			}
			t.Logf("os_process_killed=true operation=%s final=%s successor_effects=%d", final.ID, final.State, effects.Load())
		})
	}
}
