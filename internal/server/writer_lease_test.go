package server

import (
	"context"
	"errors"
	"testing"

	"mcpx/internal/remotesession"
)

func TestWriterLeaseIsExclusivePerProjectIncludingSameSession(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	principal, err := rt.principalFromContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	registered, ok := rt.reg.Get("demo")
	if !ok {
		t.Fatal("workspace was not registered")
	}
	first, err := rt.remote.Create(context.Background(), principal, remotesession.CreateInput{WorkspaceName: "demo", WorkspacePath: registered.Path})
	if err != nil {
		t.Fatal(err)
	}
	second, err := rt.remote.Create(context.Background(), principal, remotesession.CreateInput{WorkspaceName: "demo", WorkspacePath: registered.Path})
	if err != nil {
		t.Fatal(err)
	}
	one, err := rt.acquireWriterLease(context.Background(), first.Session, principal.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.releaseWriterLease(context.Background(), one)
	if _, err := rt.acquireWriterLease(context.Background(), first.Session, principal.ID); err == nil {
		t.Fatal("same session acquired a second concurrent project lease")
	} else if !errors.Is(err, ErrWorkspaceBusy) {
		t.Fatalf("unexpected same-session lease conflict: %v", err)
	}
	if _, err := rt.acquireWriterLease(context.Background(), second.Session, principal.ID); err == nil {
		t.Fatal("second session acquired an active project lease")
	} else if !errors.Is(err, ErrWorkspaceBusy) {
		t.Fatalf("unexpected lease conflict: %v", err)
	}
}
