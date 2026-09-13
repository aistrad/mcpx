package workspace

import (
	"context"
	"path/filepath"
	"testing"

	"mcpx/internal/state"
)

func TestFrozenIdentitySurvivesDatabaseReopenAndCannotBeReplaced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.db")
	store, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, err = store.DB().Exec(`INSERT INTO principals(id,kind,subject_hash,created_at,last_seen_at) VALUES('p','test','test',1,1);
		INSERT INTO remote_sessions(id,workspace_name,workspace_path,label,description,status,owner_principal_id,version,created_at,last_active_at)
		VALUES('session','fixture','fixture','','','active','p',1,1,1);`)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	identityRepo(t, root)
	identity, err := CaptureGitIdentity(context.Background(), root, "origin", "frozen-run")
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyFrozenGitIdentity(context.Background(), store.DB(), "session", identity); err == nil {
		t.Fatal("未冻结身份可用于执行")
	}
	if err := FreezeGitIdentity(context.Background(), store.DB(), "session", identity); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := VerifyFrozenGitIdentity(context.Background(), reopened.DB(), "session", identity); err != nil {
		t.Fatal(err)
	}
	if err := FreezeGitIdentity(context.Background(), reopened.DB(), "session", identity); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"HEAD", "remote", "path", "ref"} {
		changed := identity
		switch field {
		case "HEAD":
			changed.HEAD = "external-head"
		case "remote":
			changed.RemoteSHA256 = "external-remote"
		case "path":
			changed.CanonicalPath = root + "-other"
		case "ref":
			changed.Ref = "refs/heads/other"
		}
		if err := FreezeGitIdentity(context.Background(), reopened.DB(), "session", changed); err == nil {
			t.Fatalf("允许重冻结 %s", field)
		}
	}
	if err := VerifyFrozenGitIdentity(context.Background(), reopened.DB(), "other-session", identity); err == nil {
		t.Fatal("允许跨 Session 使用冻结身份")
	}
}
