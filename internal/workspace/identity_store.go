package workspace

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// FreezeGitIdentity 只创建首次身份；同一 run_id 的后续 Session 打开不能重新冻结新值。
func FreezeGitIdentity(ctx context.Context, db *sql.DB, sessionID string, identity GitIdentity) error {
	if sessionID == "" || identity.RunID == "" {
		return fmt.Errorf("session and run identity are required")
	}
	encoded, err := json.Marshal(identity)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO workspace_run_identities(run_id,remote_session_id,identity_json)
		VALUES(?,?,?) ON CONFLICT(run_id) DO NOTHING`, identity.RunID, sessionID, string(encoded)); err != nil {
		return err
	}
	var storedSession, storedJSON string
	if err := tx.QueryRowContext(ctx, `SELECT remote_session_id,identity_json FROM workspace_run_identities WHERE run_id=?`, identity.RunID).Scan(&storedSession, &storedJSON); err != nil {
		return err
	}
	if storedSession != sessionID || storedJSON != string(encoded) {
		return fmt.Errorf("run identity is already frozen; observed changes cannot replace it")
	}
	return tx.Commit()
}

// VerifyFrozenGitIdentity 要求执行请求与持久前置身份完全一致，不从传入的新值隐式创建记录。
func VerifyFrozenGitIdentity(ctx context.Context, db *sql.DB, sessionID string, identity GitIdentity) error {
	var storedSession, storedJSON string
	if err := db.QueryRowContext(ctx, `SELECT remote_session_id,identity_json FROM workspace_run_identities WHERE run_id=?`, identity.RunID).Scan(&storedSession, &storedJSON); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("run identity must be frozen by session open before execution")
		}
		return err
	}
	encoded, err := json.Marshal(identity)
	if err != nil {
		return err
	}
	if storedSession != sessionID || storedJSON != string(encoded) {
		return fmt.Errorf("execution does not match the frozen run identity")
	}
	return nil
}
