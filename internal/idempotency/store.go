// Package idempotency provides durable, scoped request replay records for
// mutating clean-core tools. It deliberately stores opaque fingerprints and
// caller-supplied response/metadata JSON rather than raw request arguments.
package idempotency

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	StatePending   = "pending"
	StateSucceeded = "succeeded"
	StateFailed    = "failed"
	StateInDoubt   = "in_doubt"

	// PendingLease allows a process that starts after a crash to take over a
	// record that no longer has an in-memory owner.
	PendingLease = 30 * time.Second
)

var (
	ErrConflict = errors.New("idempotency key has a different request fingerprint")
	ErrInDoubt  = errors.New("idempotency record is in doubt")
	ErrPending  = errors.New("idempotency record is still pending")
)

type Key struct {
	RemoteSessionID string
	PrincipalID     string
	Operation       string
	Value           string
}

func (k Key) valid() bool {
	return strings.TrimSpace(k.RemoteSessionID) != "" &&
		strings.TrimSpace(k.PrincipalID) != "" &&
		strings.TrimSpace(k.Operation) != "" &&
		strings.TrimSpace(k.Value) != ""
}

func (k Key) identity() string {
	return strings.Join([]string{k.RemoteSessionID, k.PrincipalID, k.Operation, k.Value}, "\x00")
}

type Record struct {
	Key         Key
	Fingerprint string
	State       string
	Response    []byte
	Metadata    []byte
	CreatedAt   time.Time
	UpdatedAt   time.Time
	ExpiresAt   time.Time
}

type ClaimKind string

const (
	ClaimOwner    ClaimKind = "owner"
	ClaimReplay   ClaimKind = "replay"
	ClaimWait     ClaimKind = "wait"
	ClaimPending  ClaimKind = "pending"
	ClaimInDoubt  ClaimKind = "in_doubt"
	ClaimConflict ClaimKind = "conflict"
)

type Claim struct {
	Kind   ClaimKind
	Record Record
	Done   <-chan struct{}
}

type flight struct {
	fingerprint string
	done        chan struct{}
}

type Store struct {
	db        *sql.DB
	now       func() time.Time
	mu        sync.Mutex
	flights   map[string]*flight
	uncertain map[string]string
}

func NewStore(db *sql.DB) *Store {
	return &Store{db: db, now: time.Now, flights: make(map[string]*flight), uncertain: make(map[string]string)}
}

// Lookup returns the durable record without claiming execution. It is used by
// confirmation-gated tools to replay an already completed request without
// asking the user to confirm the same destructive action again.
func (s *Store) Lookup(ctx context.Context, key Key) (Record, bool, error) {
	if s == nil || s.db == nil || !key.valid() {
		return Record{}, false, fmt.Errorf("idempotency store and key are required")
	}
	record, err := s.get(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, err
	}
	return record, true, nil
}

// Claim returns an owner for a new or expired pending request, a replay for a
// terminal request, or a wait/pending result for another active owner.
func (s *Store) Claim(ctx context.Context, key Key, fingerprint string, ttl time.Duration) (Claim, error) {
	if s == nil || s.db == nil {
		return Claim{}, fmt.Errorf("idempotency store database is required")
	}
	if !key.valid() || strings.TrimSpace(fingerprint) == "" {
		return Claim{}, fmt.Errorf("idempotency key and fingerprint are required")
	}
	identity := key.identity()
	s.mu.Lock()
	if active, ok := s.flights[identity]; ok {
		if active.fingerprint != fingerprint {
			s.mu.Unlock()
			return Claim{Kind: ClaimConflict}, nil
		}
		done := active.done
		s.mu.Unlock()
		return Claim{Kind: ClaimWait, Done: done}, nil
	}
	uncertain := s.uncertain[identity]
	s.mu.Unlock()
	if uncertain != "" {
		if uncertain != fingerprint {
			return Claim{Kind: ClaimConflict}, nil
		}
		record, err := s.get(ctx, key)
		if err != nil {
			return Claim{}, err
		}
		if record.Fingerprint != fingerprint {
			return Claim{Kind: ClaimConflict, Record: record}, nil
		}
		if record.State == StateSucceeded || record.State == StateFailed {
			return Claim{Kind: ClaimReplay, Record: record}, nil
		}
		// 持久化失败后的本地墓碑只允许回读恢复，不重新取得执行权。
		record.State = StateInDoubt
		return Claim{Kind: ClaimInDoubt, Record: record}, nil
	}

	now := s.now().UTC()
	record, err := s.get(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		expiresAt := now.Add(ttl)
		if ttl <= 0 {
			expiresAt = now.Add(24 * time.Hour)
		}
		_, insertErr := s.db.ExecContext(ctx, `INSERT INTO clean_idempotency_records
			(remote_session_id, principal_id, operation, idempotency_key, fingerprint, state,
			 response_json, metadata_json, created_at, updated_at, expires_at)
			VALUES (?, ?, ?, ?, ?, ?, '{}', '{}', ?, ?, ?)`,
			key.RemoteSessionID, key.PrincipalID, key.Operation, key.Value, fingerprint,
			StatePending, now.UnixMilli(), now.UnixMilli(), expiresAt.UnixMilli())
		if insertErr == nil {
			s.setFlight(identity, fingerprint)
			return Claim{Kind: ClaimOwner, Record: Record{Key: key, Fingerprint: fingerprint, State: StatePending, CreatedAt: now, UpdatedAt: now, ExpiresAt: expiresAt}}, nil
		}
		// Another process may have won the insert race. Re-read its record and
		// continue through the normal fingerprint/state checks.
		record, err = s.get(ctx, key)
	}
	if err != nil {
		return Claim{}, err
	}
	if record.Fingerprint != fingerprint {
		return Claim{Kind: ClaimConflict, Record: record}, nil
	}
	switch record.State {
	case StateSucceeded, StateFailed:
		return Claim{Kind: ClaimReplay, Record: record}, nil
	case StateInDoubt:
		return Claim{Kind: ClaimInDoubt, Record: record}, nil
	case StatePending:
		// The owner may be running in this process (it won an insert race or
		// lease takeover above): wait on its flight instead of reporting a
		// foreign pending record.
		s.mu.Lock()
		active, ok := s.flights[identity]
		s.mu.Unlock()
		if ok && active.fingerprint == fingerprint {
			return Claim{Kind: ClaimWait, Done: active.done}, nil
		}
		if now.Sub(record.UpdatedAt) >= PendingLease {
			result, updateErr := s.db.ExecContext(ctx, `UPDATE clean_idempotency_records
				SET updated_at = ? WHERE remote_session_id = ? AND principal_id = ? AND operation = ?
				AND idempotency_key = ? AND fingerprint = ? AND state = ? AND updated_at = ?`,
				now.UnixMilli(), key.RemoteSessionID, key.PrincipalID, key.Operation, key.Value,
				fingerprint, StatePending, record.UpdatedAt.UnixMilli())
			if updateErr != nil {
				return Claim{}, updateErr
			}
			if count, rowsErr := result.RowsAffected(); rowsErr == nil && count == 1 {
				s.setFlight(identity, fingerprint)
				record.UpdatedAt = now
				return Claim{Kind: ClaimOwner, Record: record}, nil
			}
			record, err = s.get(ctx, key)
			if err != nil {
				return Claim{}, err
			}
			if record.Fingerprint != fingerprint {
				return Claim{Kind: ClaimConflict, Record: record}, nil
			}
			if record.State != StatePending {
				return s.claimTerminal(record), nil
			}
		}
		return Claim{Kind: ClaimPending, Record: record}, nil
	default:
		return Claim{}, fmt.Errorf("unknown idempotency state %q", record.State)
	}
}

func (s *Store) claimTerminal(record Record) Claim {
	switch record.State {
	case StateSucceeded, StateFailed:
		return Claim{Kind: ClaimReplay, Record: record}
	case StateInDoubt:
		return Claim{Kind: ClaimInDoubt, Record: record}
	default:
		return Claim{Kind: ClaimPending, Record: record}
	}
}

func (s *Store) setFlight(identity, fingerprint string) {
	s.mu.Lock()
	s.flights[identity] = &flight{fingerprint: fingerprint, done: make(chan struct{})}
	s.mu.Unlock()
}

// Wait blocks for a local owner to finish, then returns the durable record.
// A pending record owned by another process has no local channel and returns
// ErrPending so the caller can expose a bounded retry action.
func (s *Store) Wait(ctx context.Context, claim Claim, key Key) (Record, error) {
	if claim.Kind != ClaimWait || claim.Done == nil {
		return Record{}, ErrPending
	}
	select {
	case <-claim.Done:
		record, err := s.get(ctx, key)
		if err == nil && record.State != StateSucceeded && record.State != StateFailed {
			return record, ErrInDoubt
		}
		return record, err
	case <-ctx.Done():
		return Record{}, ctx.Err()
	}
}

func (s *Store) Complete(ctx context.Context, key Key, fingerprint, state string, response, metadata []byte) error {
	if !key.valid() || strings.TrimSpace(fingerprint) == "" {
		return fmt.Errorf("idempotency key and fingerprint are required")
	}
	if state != StateSucceeded && state != StateFailed && state != StateInDoubt {
		return fmt.Errorf("invalid terminal idempotency state %q", state)
	}
	now := s.now().UTC()
	if response == nil {
		response = []byte(`{}`)
	}
	if metadata == nil {
		metadata = []byte(`{}`)
	}
	result, err := s.db.ExecContext(ctx, `UPDATE clean_idempotency_records SET state = ?, response_json = ?, metadata_json = ?, updated_at = ?
		WHERE remote_session_id = ? AND principal_id = ? AND operation = ? AND idempotency_key = ? AND fingerprint = ?`,
		state, string(response), string(metadata), now.UnixMilli(), key.RemoteSessionID, key.PrincipalID,
		key.Operation, key.Value, fingerprint)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected != 1 {
		return fmt.Errorf("idempotency record disappeared before completion")
	}
	s.finishFlight(key.identity())
	return nil
}

// UpdatePending records the prepared terminal response while retaining the
// pending state. This closes the crash window between edit preparation and
// filesystem writes without making a partially written batch look successful.
func (s *Store) UpdatePending(ctx context.Context, key Key, fingerprint string, response, metadata []byte) error {
	if !key.valid() || strings.TrimSpace(fingerprint) == "" {
		return fmt.Errorf("idempotency key and fingerprint are required")
	}
	if response == nil {
		response = []byte(`{}`)
	}
	if metadata == nil {
		metadata = []byte(`{}`)
	}
	_, err := s.db.ExecContext(ctx, `UPDATE clean_idempotency_records SET response_json = ?, metadata_json = ?, updated_at = ?
		WHERE remote_session_id = ? AND principal_id = ? AND operation = ? AND idempotency_key = ? AND fingerprint = ? AND state = ?`,
		string(response), string(metadata), s.now().UTC().UnixMilli(), key.RemoteSessionID, key.PrincipalID,
		key.Operation, key.Value, fingerprint, StatePending)
	return err
}

// Abandon deletes a pending record that was claimed but rejected before any
// effect ran (for example semantic preflight rejected the request). It only
// removes records still in pending state with the matching fingerprint, so a
// completed effect is never touched and a retry starts from a clean slate.
func (s *Store) Abandon(ctx context.Context, key Key, fingerprint string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("idempotency store database is required")
	}
	if !key.valid() || strings.TrimSpace(fingerprint) == "" {
		return fmt.Errorf("idempotency key and fingerprint are required")
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM clean_idempotency_records
		WHERE remote_session_id = ? AND principal_id = ? AND operation = ? AND idempotency_key = ? AND fingerprint = ? AND state = ?`,
		key.RemoteSessionID, key.PrincipalID, key.Operation, key.Value, fingerprint, StatePending)
	if err != nil {
		return err
	}
	// Only an actual deletion releases the in-process flight. A wrong
	// fingerprint or a non-pending record leaves the owner (if any) untouched,
	// so the next identical Claim must keep waiting on the live flight instead
	// of reporting a foreign pending record.
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected == 1 {
		s.finishFlight(key.identity())
	}
	return nil
}

func (s *Store) finishFlight(identity string) {
	s.mu.Lock()
	if active, ok := s.flights[identity]; ok {
		delete(s.flights, identity)
		close(active.done)
	}
	s.mu.Unlock()
}

func (s *Store) MarkInDoubt(ctx context.Context, key Key, fingerprint string, metadata []byte) error {
	if !key.valid() || strings.TrimSpace(fingerprint) == "" {
		return fmt.Errorf("idempotency key and fingerprint are required")
	}
	// 调用者已停止执行；即使数据库或请求 context 失效，也必须唤醒等待者，
	// 同时禁止同一进程把未知效果当成新的可执行请求。
	defer func() {
		s.mu.Lock()
		if active, ok := s.flights[key.identity()]; ok && active.fingerprint == fingerprint {
			s.uncertain[key.identity()] = fingerprint
			delete(s.flights, key.identity())
			close(active.done)
		}
		s.mu.Unlock()
	}()
	var (
		result sql.Result
		err    error
	)
	if metadata == nil {
		result, err = s.db.ExecContext(ctx, `UPDATE clean_idempotency_records SET state = ?, updated_at = ?
			WHERE remote_session_id = ? AND principal_id = ? AND operation = ? AND idempotency_key = ? AND fingerprint = ?`,
			StateInDoubt, s.now().UTC().UnixMilli(), key.RemoteSessionID, key.PrincipalID, key.Operation, key.Value, fingerprint)
	} else {
		result, err = s.db.ExecContext(ctx, `UPDATE clean_idempotency_records SET state = ?, metadata_json = ?, updated_at = ?
			WHERE remote_session_id = ? AND principal_id = ? AND operation = ? AND idempotency_key = ? AND fingerprint = ?`,
			StateInDoubt, string(metadata), s.now().UTC().UnixMilli(), key.RemoteSessionID, key.PrincipalID, key.Operation, key.Value, fingerprint)
	}
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected != 1 {
		return fmt.Errorf("idempotency record disappeared before marking in doubt")
	}
	s.finishFlight(key.identity())
	return nil
}

func (s *Store) Get(ctx context.Context, key Key) (Record, error) {
	return s.get(ctx, key)
}

func (s *Store) get(ctx context.Context, key Key) (Record, error) {
	var record Record
	var createdAt, updatedAt, expiresAt int64
	err := s.db.QueryRowContext(ctx, `SELECT fingerprint, state, response_json, metadata_json, created_at, updated_at, expires_at
		FROM clean_idempotency_records WHERE remote_session_id = ? AND principal_id = ? AND operation = ? AND idempotency_key = ?`,
		key.RemoteSessionID, key.PrincipalID, key.Operation, key.Value).Scan(
		&record.Fingerprint, &record.State, &record.Response, &record.Metadata, &createdAt, &updatedAt, &expiresAt)
	if err != nil {
		return Record{}, err
	}
	record.Key = key
	record.CreatedAt = time.UnixMilli(createdAt).UTC()
	record.UpdatedAt = time.UnixMilli(updatedAt).UTC()
	record.ExpiresAt = time.UnixMilli(expiresAt).UTC()
	return record, nil
}
