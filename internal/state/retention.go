package state

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mcpx/internal/config"
)

const retentionBatchSize = 500

// RetentionService removes only bounded, reconstructable state. It never
// deletes Remote Sessions, Plans, or their source evidence.
type RetentionService struct {
	db     *sql.DB
	logDir string
	policy config.RetentionConfig
	now    func() time.Time
	remove func(string) error
}

// RetentionReport describes one housekeeping pass.
type RetentionReport struct {
	Disabled                 bool
	DeletedObservationEvents int
	DeletedTerminalTasks     int
	DeletedFileSnapshots     int
	DeletedEnvironmentSnaps  int
	DeletedEphemeralRecords  int
	DeletedOperations        int
	Vacuumed                 bool
	Errors                   []string
}

// NewRetentionService creates a state cleanup service after validating the
// effective global configuration.
func NewRetentionService(db *sql.DB, logDir string, policy config.RetentionConfig) (*RetentionService, error) {
	if db == nil {
		return nil, fmt.Errorf("retention database is required")
	}
	if _, _, _, _, _, err := policy.RetentionDurations(); err != nil {
		return nil, err
	}
	return &RetentionService{
		db:     db,
		logDir: strings.TrimSpace(logDir),
		policy: policy,
		now:    time.Now,
		remove: os.Remove,
	}, nil
}

// RunOnce executes one bounded cleanup pass. Database errors are returned;
// maintenance errors are collected in the report so callers can continue.
func (s *RetentionService) RunOnce(ctx context.Context) (RetentionReport, error) {
	var report RetentionReport
	if s == nil || s.db == nil {
		return report, fmt.Errorf("retention service is not initialized")
	}
	if !s.policy.Enabled {
		report.Disabled = true
		return report, nil
	}
	_, processTTL, memoryTTL, terminalTaskTTL, snapshotTTL, err := s.policy.RetentionDurations()
	if err != nil {
		return report, err
	}
	now := s.now().UTC()

	for _, category := range []struct {
		name   string
		cutoff time.Time
		limit  int
	}{
		{name: "process", cutoff: now.Add(-processTTL), limit: s.policy.ProcessEventMaxRows},
		{name: "memory", cutoff: now.Add(-memoryTTL), limit: s.policy.MemoryEventMaxRows},
	} {
		deleted, err := s.deleteObservationBatch(ctx, category.name, category.cutoff, category.limit)
		if err != nil {
			return report, fmt.Errorf("delete %s observation events: %w", category.name, err)
		}
		report.DeletedObservationEvents += deleted
	}

	deleted, errors := s.deleteExpiredEphemeral(ctx, now.UnixMilli())
	if errors != nil {
		return report, errors
	}
	report.DeletedEphemeralRecords += deleted

	deleted, cleanupErrors, err := s.deleteTerminalTasks(ctx, now.Add(-terminalTaskTTL))
	if err != nil {
		return report, err
	}
	report.DeletedTerminalTasks += deleted
	report.Errors = append(report.Errors, cleanupErrors...)

	deleted, err = s.deleteExpiredOperations(ctx, now.UnixMilli())
	if err != nil {
		return report, err
	}
	report.DeletedOperations += deleted

	snapshotCounts, err := s.deleteClosedSessionSnapshots(ctx, now.Add(-snapshotTTL))
	if err != nil {
		return report, err
	}
	report.DeletedFileSnapshots += snapshotCounts.file
	report.DeletedEnvironmentSnaps += snapshotCounts.environment

	busy, err := s.checkpoint(ctx)
	if err != nil {
		report.Errors = append(report.Errors, fmt.Sprintf("wal checkpoint: %v", err))
	}
	// Skip full VACUUM on the shared state DB: with MaxOpenConns(1) it exclusive-locks
	// for tens of seconds on large files (~100MB+) and starves observation/tool writes
	// (context deadline exceeded). PASSIVE checkpoint above is enough for WAL health.
	// Operators can still vacuum offline if reclaiming disk is required.
	_ = busy
	return report, nil
}

func (s *RetentionService) checkpoint(ctx context.Context) (bool, error) {
	var busy, logFrames, checkpointed int
	if err := s.db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(PASSIVE)").Scan(&busy, &logFrames, &checkpointed); err != nil {
		return false, err
	}
	return busy != 0, nil
}

// TotalDeleted returns the number of rows removed in this pass.
func (r RetentionReport) TotalDeleted() int {
	return r.DeletedObservationEvents + r.DeletedTerminalTasks + r.DeletedFileSnapshots + r.DeletedEnvironmentSnaps + r.DeletedEphemeralRecords + r.DeletedOperations
}

func (s *RetentionService) deleteExpiredOperations(ctx context.Context, now int64) (int, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM operations
		WHERE rowid IN (
			SELECT operations.rowid
			FROM operations
			LEFT JOIN remote_sessions ON remote_sessions.id = operations.remote_session_id
			WHERE operations.state IN ('succeeded', 'failed', 'interrupted', 'cancelled')
			  AND operations.expires_at <= ?
			  AND (remote_sessions.id IS NULL OR remote_sessions.status IN ('closed', 'archived'))
			ORDER BY operations.expires_at ASC, operations.id ASC
			LIMIT ?
		)`, now, retentionBatchSize)
	if err != nil {
		return 0, fmt.Errorf("delete expired operations: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(count), nil
}

func (s *RetentionService) deleteObservationBatch(ctx context.Context, category string, cutoff time.Time, maxRows int) (int, error) {
	query, err := observationDeletionQuery(category)
	if err != nil {
		return 0, err
	}
	workspaces, err := s.observationWorkspaces(ctx)
	if err != nil {
		return 0, err
	}
	deleted := 0
	for _, workspace := range workspaces {
		// Pass workspace twice so the newest window is independent of the outer
		// row. A correlated subquery turns this bounded cleanup into O(n^2).
		rows, err := s.db.QueryContext(ctx, query, workspace, cutoff.UnixMilli(), workspace, maxRows, retentionBatchSize)
		if err != nil {
			return deleted, err
		}
		var sequences []int64
		for rows.Next() {
			var sequence int64
			if err := rows.Scan(&sequence); err != nil {
				rows.Close()
				return deleted, err
			}
			sequences = append(sequences, sequence)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return deleted, err
		}
		rows.Close()
		if len(sequences) == 0 {
			continue
		}
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(sequences)), ",")
		args := make([]any, len(sequences))
		for i, sequence := range sequences {
			args[i] = sequence
		}
		result, err := s.db.ExecContext(ctx, "DELETE FROM observation_events WHERE sequence IN ("+placeholders+")", args...)
		if err != nil {
			return deleted, err
		}
		if _, err := result.RowsAffected(); err != nil {
			return deleted, err
		}
		deleted += len(sequences)
	}
	return deleted, nil
}

func observationDeletionQuery(category string) (string, error) {
	predicate, err := observationPredicate(category, "e")
	if err != nil {
		return "", err
	}
	newestPredicate, err := observationPredicate(category, "newest")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`SELECT e.sequence
FROM observation_events e
WHERE e.workspace_name = ? AND %s
  AND (e.created_at < ? OR e.sequence NOT IN (
      SELECT newest.sequence
      FROM observation_events newest
      WHERE newest.workspace_name = ? AND %s
      ORDER BY newest.created_at DESC, newest.sequence DESC
      LIMIT ?
  ))
ORDER BY e.created_at ASC, e.sequence ASC
LIMIT ?`, predicate, newestPredicate), nil
}

func observationPredicate(category, alias string) (string, error) {
	switch category {
	case "process":
		return fmt.Sprintf(`(%s.event_type IN ('tool.started', 'command.output', 'observer.notice') OR
         (%s.event_type = 'tool.completed' AND %s.tool_name <> 'progress'))`, alias, alias, alias), nil
	case "memory":
		return fmt.Sprintf(`(%s.event_type IN ('file.changed', 'session.lifecycle') OR
         (%s.event_type = 'tool.completed' AND %s.tool_name = 'progress'))`, alias, alias, alias), nil
	default:
		return "", fmt.Errorf("unknown observation retention category %q", category)
	}
}

func (s *RetentionService) observationWorkspaces(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT workspace_name FROM observation_events ORDER BY workspace_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var workspaces []string
	for rows.Next() {
		var workspace string
		if err := rows.Scan(&workspace); err != nil {
			return nil, err
		}
		workspaces = append(workspaces, workspace)
	}
	return workspaces, rows.Err()
}

func (s *RetentionService) deleteExpiredEphemeral(ctx context.Context, now int64) (int, error) {
	total := 0
	for _, table := range []string{"approvals", "secret_requests", "idempotency_records", "clean_idempotency_records", "clean_edit_records", "workspace_writer_leases"} {
		result, err := s.db.ExecContext(ctx, "DELETE FROM "+table+" WHERE rowid IN (SELECT rowid FROM "+table+" WHERE expires_at <= ? ORDER BY expires_at LIMIT ?)", now, retentionBatchSize)
		if err != nil {
			return total, fmt.Errorf("delete expired %s: %w", table, err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return total, err
		}
		total += int(count)
	}
	return total, nil
}

func (s *RetentionService) deleteTerminalTasks(ctx context.Context, cutoff time.Time) (int, []string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.id, t.log_path
FROM terminal_tasks t
WHERE t.status <> 'running'
  AND COALESCE(t.finished_at, t.updated_at) < ?
  AND NOT EXISTS (
      SELECT 1 FROM plan_task_evidence e
      WHERE e.kind = 'execute' AND e.reference_id = t.id
  )
ORDER BY COALESCE(t.finished_at, t.updated_at), t.id
LIMIT ?`, cutoff.UnixMilli(), retentionBatchSize)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	type candidate struct{ id, logPath string }
	var candidates []candidate
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.id, &item.logPath); err != nil {
			return 0, nil, err
		}
		candidates = append(candidates, item)
	}
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}
	deleted := 0
	var cleanupErrors []string
	for _, item := range candidates {
		if err := s.removeTaskLog(item.logPath); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Sprintf("task %s log: %v", item.id, err))
			continue
		}
		result, err := s.db.ExecContext(ctx, "DELETE FROM terminal_tasks WHERE id = ?", item.id)
		if err != nil {
			return deleted, cleanupErrors, err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return deleted, cleanupErrors, err
		}
		deleted += int(count)
	}
	return deleted, cleanupErrors, nil
}

func (s *RetentionService) removeTaskLog(path string) error {
	path = strings.TrimSpace(path)
	if path == "" || s.logDir == "" {
		return nil
	}
	paths := []string{path}
	if strings.HasSuffix(path, ".log") && !strings.HasSuffix(path, ".stdout.log") && !strings.HasSuffix(path, ".stderr.log") {
		base := strings.TrimSuffix(path, ".log")
		paths = append(paths, base+".stdout.log", base+".stderr.log")
	}
	for _, candidate := range paths {
		if err := s.removeTaskLogFile(candidate); err != nil {
			return err
		}
	}
	return nil
}

func (s *RetentionService) removeTaskLogFile(path string) error {
	root, err := filepath.Abs(s.logDir)
	if err != nil {
		return err
	}
	target, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("log path is outside task directory")
	}
	if err := s.remove(target); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

type snapshotDeleteCounts struct {
	file        int
	environment int
}

func (s *RetentionService) deleteClosedSessionSnapshots(ctx context.Context, cutoff time.Time) (snapshotDeleteCounts, error) {
	var counts snapshotDeleteCounts
	result, err := s.db.ExecContext(ctx, `DELETE FROM file_snapshots
WHERE id IN (
    SELECT fs.id FROM file_snapshots fs
    LEFT JOIN remote_sessions rs ON rs.id = fs.remote_session_id
    WHERE fs.created_at < ?
      AND (rs.id IS NULL OR rs.status IN ('closed', 'archived'))
    ORDER BY fs.created_at ASC, fs.id
    LIMIT ?
)`, cutoff.UnixMilli(), retentionBatchSize)
	if err != nil {
		return counts, fmt.Errorf("delete file snapshots: %w", err)
	}
	fileCount, err := result.RowsAffected()
	if err != nil {
		return counts, err
	}
	counts.file = int(fileCount)

	result, err = s.db.ExecContext(ctx, `DELETE FROM environment_snapshots
WHERE id IN (
    SELECT es.id FROM environment_snapshots es
    LEFT JOIN remote_sessions rs ON rs.id = es.remote_session_id
    WHERE es.created_at < ?
      AND (rs.id IS NULL OR rs.status IN ('closed', 'archived'))
      AND NOT EXISTS (
          SELECT 1 FROM remote_sessions current_session
          WHERE current_session.environment_snapshot_id = es.id
      )
    ORDER BY es.created_at ASC, es.id
    LIMIT ?
)`, cutoff.UnixMilli(), retentionBatchSize)
	if err != nil {
		return counts, fmt.Errorf("delete environment snapshots: %w", err)
	}
	environmentCount, err := result.RowsAffected()
	if err != nil {
		return counts, err
	}
	counts.environment = int(environmentCount)
	return counts, nil
}
