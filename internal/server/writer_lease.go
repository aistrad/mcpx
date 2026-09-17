package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"mcpx/internal/config"
	"mcpx/internal/remotesession"
)

const defaultWriterLeaseTTL = 2 * time.Minute

var ErrWorkspaceBusy = errors.New("project root is already owned by another writer")

type writerLease struct {
	ID          string
	ProjectPath string
	SessionID   string
	PrincipalID string
	ExpiresAt   time.Time
}

type writerLeaseConflict struct {
	ProjectPath string
	LeaseID     string
	SessionID   string
	ExpiresAt   time.Time
}

func (e *writerLeaseConflict) Error() string {
	return fmt.Sprintf("%v: %s", ErrWorkspaceBusy, e.ProjectPath)
}

func (e *writerLeaseConflict) Unwrap() error { return ErrWorkspaceBusy }

func (r *Runtime) acquireWriterLease(ctx context.Context, session remotesession.Session, principalID string) (writerLease, error) {
	if r == nil || r.state == nil || r.state.DB() == nil {
		return writerLease{}, nil
	}
	projectPath := sessionProjectPath(session)
	if projectPath == "" {
		return writerLease{}, fmt.Errorf("project root is required for a writer lease")
	}
	canonical, err := filepath.EvalSymlinks(projectPath)
	if err != nil {
		return writerLease{}, fmt.Errorf("resolve project root for writer lease: %w", err)
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return writerLease{}, err
	}
	now := time.Now().UTC()
	expires := now.Add(r.writerLeaseTTL())
	tx, err := r.state.DB().BeginTx(ctx, nil)
	if err != nil {
		return writerLease{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM workspace_writer_leases
		WHERE expires_at <= ? AND NOT EXISTS (
			SELECT 1 FROM terminal_tasks t
			WHERE t.remote_session_id = workspace_writer_leases.remote_session_id AND t.status = 'running'
		)`, now.UnixMilli()); err != nil {
		return writerLease{}, err
	}
	var existing writerLease
	var expiresAt int64
	err = tx.QueryRowContext(ctx, `SELECT lease_id, remote_session_id, principal_id, expires_at
		FROM workspace_writer_leases WHERE project_path = ?`, canonical).Scan(
		&existing.ID, &existing.SessionID, &existing.PrincipalID, &expiresAt)
	if err == nil {
		existing.ProjectPath = canonical
		existing.ExpiresAt = time.UnixMilli(expiresAt).UTC()
		// A Session can have multiple Web clients. Reusing the lease by
		// session would let two concurrent edits proceed at once, so every
		// active lease blocks a second writer, including another request from
		// the same Session. Long-running tasks renew their original lease
		// directly instead of reacquiring it.
		return writerLease{}, &writerLeaseConflict{ProjectPath: canonical, LeaseID: existing.ID, SessionID: existing.SessionID, ExpiresAt: existing.ExpiresAt}
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return writerLease{}, err
	}
	lease := writerLease{ID: newRuntimeID("lease", 12), ProjectPath: canonical, SessionID: session.ID, PrincipalID: principalID, ExpiresAt: expires}
	if _, err := tx.ExecContext(ctx, `INSERT INTO workspace_writer_leases
		(project_path, lease_id, remote_session_id, principal_id, expires_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`, lease.ProjectPath, lease.ID, lease.SessionID, lease.PrincipalID, lease.ExpiresAt.UnixMilli(), now.UnixMilli()); err != nil {
		return writerLease{}, err
	}
	if err := tx.Commit(); err != nil {
		return writerLease{}, err
	}
	r.rememberWriterLease(lease)
	return lease, nil
}

func (r *Runtime) renewWriterLease(ctx context.Context, lease writerLease) error {
	if r == nil || r.state == nil || r.state.DB() == nil || lease.ID == "" {
		return nil
	}
	expires := time.Now().UTC().Add(r.writerLeaseTTL())
	result, err := r.state.DB().ExecContext(ctx, `UPDATE workspace_writer_leases
		SET expires_at = ?, updated_at = ? WHERE project_path = ? AND lease_id = ? AND remote_session_id = ?`,
		expires.UnixMilli(), time.Now().UTC().UnixMilli(), lease.ProjectPath, lease.ID, lease.SessionID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrWorkspaceBusy
	}
	return nil
}

func (r *Runtime) releaseWriterLease(ctx context.Context, lease writerLease) {
	r.forgetWriterLease(lease)
	if r == nil || r.state == nil || r.state.DB() == nil || lease.ID == "" {
		return
	}
	_, _ = r.state.DB().ExecContext(ctx, `DELETE FROM workspace_writer_leases WHERE project_path = ? AND lease_id = ? AND remote_session_id = ?`, lease.ProjectPath, lease.ID, lease.SessionID)
}

func (r *Runtime) releaseWriterLeaseForSession(ctx context.Context, sessionID string) {
	if r != nil {
		r.writerLeaseMu.Lock()
		for id, lease := range r.writerLeases {
			if lease.SessionID == sessionID {
				delete(r.writerLeases, id)
			}
		}
		r.writerLeaseMu.Unlock()
	}
	if r == nil || r.state == nil || r.state.DB() == nil || strings.TrimSpace(sessionID) == "" {
		return
	}
	_, _ = r.state.DB().ExecContext(ctx, `DELETE FROM workspace_writer_leases WHERE remote_session_id = ?`, sessionID)
}

func (r *Runtime) releaseAllWriterLeases(ctx context.Context) {
	if r == nil || r.state == nil || r.state.DB() == nil {
		return
	}
	r.writerLeaseMu.Lock()
	leases := make([]writerLease, 0, len(r.writerLeases))
	for id, lease := range r.writerLeases {
		leases = append(leases, lease)
		delete(r.writerLeases, id)
	}
	r.writerLeaseMu.Unlock()
	for _, lease := range leases {
		_, _ = r.state.DB().ExecContext(ctx, `DELETE FROM workspace_writer_leases WHERE project_path = ? AND lease_id = ? AND remote_session_id = ?`, lease.ProjectPath, lease.ID, lease.SessionID)
	}
}

func (r *Runtime) rememberWriterLease(lease writerLease) {
	if r == nil || lease.ID == "" {
		return
	}
	r.writerLeaseMu.Lock()
	if r.writerLeases == nil {
		r.writerLeases = make(map[string]writerLease)
	}
	r.writerLeases[lease.ID] = lease
	r.writerLeaseMu.Unlock()
}

func (r *Runtime) forgetWriterLease(lease writerLease) {
	if r == nil || lease.ID == "" {
		return
	}
	r.writerLeaseMu.Lock()
	delete(r.writerLeases, lease.ID)
	r.writerLeaseMu.Unlock()
}

// keepWriterLease renews a lease while a persistent task is still running.
// The task owns its process context, so the renewal loop intentionally lives
// outside the request context and stops only after the task's done channel.
func (r *Runtime) keepWriterLease(lease writerLease, taskDone <-chan struct{}) {
	if lease.ID == "" {
		return
	}
	go func() {
		interval := r.writerLeaseTTL() / 3
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-taskDone:
				r.releaseWriterLease(context.Background(), lease)
				return
			case <-ticker.C:
				// Keep retrying while the task is alive. Expired rows are also
				// retained by acquireWriterLease when this session still has a
				// running Task, so a transient database error cannot release the
				// project to a second writer.
				_ = r.renewWriterLease(context.Background(), lease)
			}
		}
	}()
}

func (r *Runtime) writerLeaseTTL() time.Duration {
	if r == nil {
		return defaultWriterLeaseTTL
	}
	return config.WriterLeaseTTL(r.cfg.Transport)
}

func writerLeaseData(lease writerLease) map[string]any {
	if lease.ID == "" {
		return nil
	}
	return map[string]any{
		"lease_id": lease.ID, "project_root": lease.ProjectPath,
		"remote_session_id": lease.SessionID, "expires_at": lease.ExpiresAt.UTC().Format(time.RFC3339Nano),
	}
}

func writerLeaseError(err error) (map[string]any, bool) {
	var conflict *writerLeaseConflict
	if !errors.As(err, &conflict) {
		return nil, false
	}
	return map[string]any{
		"project_root": conflict.ProjectPath, "lease_id": conflict.LeaseID,
		"remote_session_id": conflict.SessionID, "expires_at": conflict.ExpiresAt.UTC().Format(time.RFC3339Nano),
	}, true
}
