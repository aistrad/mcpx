package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"mcpx/internal/winproc"
	"mcpx/internal/workspace"
)

type workspaceTransition struct {
	OperationID string `json:"operation_id"`
	HEAD        string `json:"head"`
	Tree        string `json:"tree"`
}

// 身份推进只接受预先明确的 commit 对象及带旧值条件的 update-ref。
// 普通 git commit 不能凭观察到的 HEAD 自动归因；调用方先制作 commit 对象，再条件更新 ref。
func validateWorkspaceTransition(ctx context.Context, payload map[string]any, before *workspace.GitIdentity) (*workspaceTransition, error) {
	raw, exists := payload["workspace_transition"]
	if !exists {
		return nil, nil
	}
	if before == nil {
		return nil, fmt.Errorf("workspace_transition requires expected_workspace")
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var transition workspaceTransition
	if err := json.Unmarshal(encoded, &transition); err != nil || strings.TrimSpace(transition.OperationID) == "" || len(transition.OperationID) > 200 {
		return nil, fmt.Errorf("invalid workspace transition operation ID")
	}
	sha := regexp.MustCompile(`^[0-9a-f]{40}$`)
	if !sha.MatchString(transition.HEAD) || !sha.MatchString(transition.Tree) || transition.HEAD == before.HEAD || !strings.HasPrefix(before.Ref, "refs/heads/") {
		return nil, fmt.Errorf("transition requires a new exact commit and tree on the frozen branch")
	}
	spec, _, _, err := executeArgv(payload)
	if err != nil || spec == nil || len(spec.Args) != 4 || spec.Args[0] != "update-ref" || spec.Args[1] != before.Ref || spec.Args[2] != transition.HEAD || spec.Args[3] != before.HEAD {
		return nil, fmt.Errorf("transition requires argv git update-ref frozen-ref new-head old-head")
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("Git executable unavailable")
	}
	actualPath, err := exec.LookPath(spec.Executable)
	if err != nil {
		return nil, fmt.Errorf("Git executable unavailable")
	}
	gitPath, err = filepath.EvalSymlinks(gitPath)
	if err != nil {
		return nil, err
	}
	actualPath, err = filepath.EvalSymlinks(actualPath)
	if err != nil || actualPath != gitPath {
		return nil, fmt.Errorf("transition executable must resolve to the identity probe Git")
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	read := func(args ...string) (string, error) {
		cmd := exec.CommandContext(probeCtx, gitPath, append([]string{"-C", before.CanonicalPath}, args...)...)
		winproc.ConfigureNoWindow(cmd)
		out, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("cannot verify planned commit object")
		}
		return strings.TrimSpace(string(out)), nil
	}
	parents, err := read("show", "-s", "--format=%P", transition.HEAD)
	if err != nil || parents != before.HEAD {
		return nil, fmt.Errorf("planned commit must have exactly the frozen HEAD as parent")
	}
	tree, err := read("rev-parse", "--verify", transition.HEAD+"^{tree}")
	if err != nil || tree != transition.Tree {
		return nil, fmt.Errorf("planned commit tree does not match")
	}
	return &transition, nil
}

func (r *Runtime) prepareWorkspaceTransition(ctx context.Context, sessionID, command string, before workspace.GitIdentity, planned workspaceTransition) error {
	after := before
	after.HEAD, after.Tree = planned.HEAD, planned.Tree
	beforeJSON, _ := json.Marshal(before)
	afterJSON, _ := json.Marshal(after)
	result, err := r.state.DB().ExecContext(ctx, `INSERT INTO workspace_identity_transitions(run_id,operation_id,remote_session_id,before_json,after_json,command)
		SELECT ?,?,?,?,?,? WHERE EXISTS (SELECT 1 FROM workspace_run_identities WHERE run_id=? AND remote_session_id=? AND identity_json=?)`,
		before.RunID, planned.OperationID, sessionID, string(beforeJSON), string(afterJSON), command, before.RunID, sessionID, string(beforeJSON))
	if err != nil {
		return fmt.Errorf("transition already recorded or another transition is pending; recover original execution")
	}
	count, err := result.RowsAffected()
	if err != nil || count != 1 {
		return fmt.Errorf("frozen identity changed before transition")
	}
	return nil
}

func (r *Runtime) reconcileWorkspaceTransition(ctx context.Context, sessionID, runID string) error {
	var operationID, beforeJSON, afterJSON, command, taskID string
	err := r.state.DB().QueryRowContext(ctx, `SELECT operation_id,before_json,after_json,command,task_id FROM workspace_identity_transitions
		WHERE run_id=? AND remote_session_id=? AND phase='pending'`, runID, sessionID).Scan(&operationID, &beforeJSON, &afterJSON, &command, &taskID)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if taskID == "" {
		return fmt.Errorf("transition has no durable execution receipt; outcome remains unknown")
	}
	var status, taskCommand, workDir string
	var exitCode sql.NullInt64
	if err := r.state.DB().QueryRowContext(ctx, `SELECT status,exit_code,command,workspace_path FROM terminal_tasks WHERE id=? AND remote_session_id=?`, taskID, sessionID).Scan(&status, &exitCode, &taskCommand, &workDir); err != nil {
		return fmt.Errorf("transition execution receipt unavailable")
	}
	var after workspace.GitIdentity
	if err := json.Unmarshal([]byte(afterJSON), &after); err != nil {
		return err
	}
	if status != "exited" || !exitCode.Valid || exitCode.Int64 != 0 || taskCommand != command || workDir != after.CanonicalPath {
		return fmt.Errorf("transition execution is not confirmed successful")
	}
	if err := workspace.VerifyGitIdentity(ctx, after); err != nil {
		return fmt.Errorf("transition post-state does not match exact candidate")
	}
	tx, err := r.state.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE workspace_run_identities SET identity_json=?,version=version+1 WHERE run_id=? AND remote_session_id=? AND identity_json=?`, afterJSON, runID, sessionID, beforeJSON)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		var current, phase string
		if err := tx.QueryRowContext(ctx, `SELECT i.identity_json,t.phase FROM workspace_run_identities i JOIN workspace_identity_transitions t ON t.run_id=i.run_id
			WHERE i.run_id=? AND i.remote_session_id=? AND t.operation_id=?`, runID, sessionID, operationID).Scan(&current, &phase); err == nil && current == afterJSON && phase == "confirmed" {
			return tx.Commit()
		}
		return fmt.Errorf("transition cannot replace another frozen identity")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workspace_identity_transitions SET phase='confirmed' WHERE run_id=? AND operation_id=? AND phase='pending'`, runID, operationID); err != nil {
		return err
	}
	return tx.Commit()
}
