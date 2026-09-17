package server

import (
	"context"
	"testing"
	"time"

	"mcpx/internal/mcpresult"
	"mcpx/internal/remotesession"
)

func TestProgressVerificationEvidenceRequiresSuccessfulTaskInCurrentSession(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	principal, err := rt.principalFromContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ws, _ := rt.reg.Get("demo")
	session, err := rt.remote.Create(context.Background(), principal, remotesession.CreateInput{WorkspaceName: "demo", WorkspacePath: ws.Path})
	if err != nil {
		t.Fatal(err)
	}
	task, err := rt.tasks.StartRemoteWithObservationContext(context.Background(), "req-verification", "call-verification", "execute", session.Session.ID, session.Session.WorkspaceName, ws.Path, "echo verified")
	if err != nil {
		t.Fatal(err)
	}
	if !task.Wait(context.Background()) {
		t.Fatal("verification task did not finish")
	}
	result, err := rt.toolProgress(context.Background(), mcpresult.Request(map[string]any{
		"remote_session_id": session.Session.ID, "status": "completed", "phase": "verification",
		"task_scope": "unit verification", "current": "verification completed",
		"evidence": []any{map[string]any{"kind": "execute", "reference_id": task.ID}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if decodeToolResult(t, result)["public_status"] != "succeeded" {
		t.Fatalf("successful evidence was rejected: %+v", result)
	}
}

func TestProgressVerificationRejectsFailedTaskEvidence(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	principal, err := rt.principalFromContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ws, _ := rt.reg.Get("demo")
	session, err := rt.remote.Create(context.Background(), principal, remotesession.CreateInput{WorkspaceName: "demo", WorkspacePath: ws.Path})
	if err != nil {
		t.Fatal(err)
	}
	task, err := rt.tasks.StartRemoteWithObservationContext(context.Background(), "req-verification-failed", "call-verification-failed", "execute", session.Session.ID, session.Session.WorkspaceName, ws.Path, "false")
	if err != nil {
		t.Fatal(err)
	}
	if !task.Wait(context.Background()) {
		t.Fatal("failed verification task did not finish")
	}
	deadline := time.Now().Add(time.Second)
	for task.StatusView()["status"] == "running" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	result, err := rt.toolProgress(context.Background(), mcpresult.Request(map[string]any{
		"remote_session_id": session.Session.ID, "status": "completed", "phase": "verification",
		"task_scope": "failed verification", "current": "verification completed",
		"evidence": []any{map[string]any{"kind": "execute", "reference_id": task.ID}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if response := decodeToolResult(t, result); response["status"] == "succeeded" {
		t.Fatalf("failed task evidence was accepted: %+v", response)
	}
}
