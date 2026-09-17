package workspacechanges

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"mcpx/internal/winproc"
)

var ErrNotGitRepository = errors.New("workspace is not a Git repository")

const maxDiffBytes = 1 << 20

type Service struct {
	db  *sql.DB
	now func() time.Time
}

type Entry struct {
	Path           string `json:"path"`
	OriginalPath   string `json:"original_path,omitempty"`
	Status         string `json:"status"`
	IndexStatus    string `json:"index_status"`
	WorktreeStatus string `json:"worktree_status"`
	Attribution    string `json:"attribution"`
}

// GitRoot describes one Git repository root inside the workspace.
type GitRoot struct {
	Path string `json:"path"` // relative to the workspace root; "" means the root itself
	Head string `json:"git_head"`
}

type Report struct {
	RemoteSessionID string    `json:"remote_session_id"`
	Workspace       string    `json:"workspace"`
	ProjectRoot     string    `json:"project_root"`
	GitAvailable    bool      `json:"git_available"`
	GitHead         string    `json:"git_head,omitempty"`
	GitRoots        []GitRoot `json:"git_roots,omitempty"`
	BaselineHead    string    `json:"baseline_git_head,omitempty"`
	Entries         []Entry   `json:"entries"`
	UnifiedDiff     string    `json:"unified_diff,omitempty"`
	DiffTruncated   bool      `json:"diff_truncated,omitempty"`
	InspectedAt     time.Time `json:"inspected_at"`
}

func NewService(db *sql.DB) *Service { return &Service{db: db, now: time.Now} }

// DiscoverGitRoots returns the workspace root itself when it is a Git
// repository, otherwise the one-level nested directories that contain a .git
// entry (directory or gitfile). An empty result means the workspace is not a
// Git repository.
func DiscoverGitRoots(workspaceRoot string) ([]string, error) {
	if _, err := os.Stat(filepath.Join(workspaceRoot, ".git")); err == nil {
		return []string{""}, nil
	}
	dirs, err := os.ReadDir(workspaceRoot)
	if err != nil {
		return nil, err
	}
	var roots []string
	for _, dir := range dirs {
		if !dir.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(workspaceRoot, dir.Name(), ".git")); err == nil {
			roots = append(roots, dir.Name())
		}
	}
	sort.Strings(roots)
	return roots, nil
}

// CaptureBaseline records files already dirty when a Remote Session starts.
func (s *Service) CaptureBaseline(ctx context.Context, remoteSessionID, workspaceRoot string) error {
	roots, err := DiscoverGitRoots(workspaceRoot)
	if err != nil {
		return err
	}
	if len(roots) == 0 {
		// MCPX edits work independently of Git. A non-Git workspace simply
		// has no Git baseline to capture.
		return nil
	}
	head, _, entries, err := gitState(ctx, workspaceRoot)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO workspace_baselines(remote_session_id, git_head, captured_at)
        VALUES (?, ?, ?) ON CONFLICT(remote_session_id) DO UPDATE SET git_head=excluded.git_head, captured_at=excluded.captured_at`,
		remoteSessionID, head, s.now().UTC().UnixMilli()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM workspace_baseline_files WHERE remote_session_id = ?`, remoteSessionID); err != nil {
		return err
	}
	for _, entry := range entries {
		if _, err := tx.ExecContext(ctx, `INSERT INTO workspace_baseline_files(remote_session_id, path, status) VALUES (?, ?, ?)`,
			remoteSessionID, entry.Path, entry.IndexStatus+entry.WorktreeStatus); err != nil {
			return err
		}
		if entry.OriginalPath != "" {
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO workspace_baseline_files(remote_session_id, path, status) VALUES (?, ?, ?)`,
				remoteSessionID, entry.OriginalPath, entry.IndexStatus+entry.WorktreeStatus); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (s *Service) Inspect(ctx context.Context, remoteSessionID, workspaceName, workspaceRoot string, includeDiff bool) (Report, error) {
	roots, err := DiscoverGitRoots(workspaceRoot)
	if err != nil {
		return Report{}, err
	}
	if len(roots) == 0 {
		return Report{
			RemoteSessionID: remoteSessionID,
			Workspace:       workspaceName,
			ProjectRoot:     workspaceRoot,
			GitAvailable:    false,
			Entries:         []Entry{},
			InspectedAt:     s.now().UTC(),
		}, nil
	}
	head, gitRoots, entries, err := gitState(ctx, workspaceRoot)
	if err != nil {
		return Report{}, err
	}
	baselineHead, baseline, err := s.baseline(ctx, remoteSessionID)
	if errors.Is(err, sql.ErrNoRows) {
		// A workspace can become a Git repository after the Remote Session is
		// opened. There is no pre-existing Git baseline in that case, so treat
		// the current repository as an empty baseline instead of failing a
		// read-only status query.
		baselineHead = head
		baseline = map[string]bool{}
	} else if err != nil {
		return Report{}, err
	}
	mcpx, err := s.mcpxPaths(ctx, remoteSessionID)
	if err != nil {
		return Report{}, err
	}
	for index := range entries {
		entry := &entries[index]
		switch {
		case mcpx[entry.Path] || (entry.OriginalPath != "" && mcpx[entry.OriginalPath]):
			entry.Attribution = "mcpx"
		case baseline[entry.Path] || (entry.OriginalPath != "" && baseline[entry.OriginalPath]):
			entry.Attribution = "preexisting"
		default:
			entry.Attribution = "external"
		}
	}
	report := Report{RemoteSessionID: remoteSessionID, Workspace: workspaceName, ProjectRoot: workspaceRoot, GitAvailable: true, GitHead: head, GitRoots: gitRoots, BaselineHead: baselineHead, Entries: entries, InspectedAt: s.now().UTC()}
	if includeDiff {
		var parts []string
		diffRoots, _ := DiscoverGitRoots(workspaceRoot)
		for _, root := range diffRoots {
			rootPath := workspaceRoot
			if root != "" {
				rootPath = filepath.Join(workspaceRoot, root)
			}
			diff, diffErr := gitOutput(ctx, rootPath, "diff", "--no-ext-diff", "--no-color", "HEAD", "--")
			if diffErr != nil {
				diff, diffErr = gitOutput(ctx, rootPath, "diff", "--no-ext-diff", "--no-color", "--")
			}
			if diffErr != nil || diff == "" {
				continue
			}
			if root != "" {
				parts = append(parts, "### "+root+"\n"+diff)
			} else {
				parts = append(parts, diff)
			}
		}
		if joined := strings.Join(parts, "\n"); joined != "" {
			report.UnifiedDiff, report.DiffTruncated = truncateUTF8(joined, maxDiffBytes)
		}
	}
	return report, nil
}

func (s *Service) baseline(ctx context.Context, remoteSessionID string) (string, map[string]bool, error) {
	var head string
	if err := s.db.QueryRowContext(ctx, `SELECT git_head FROM workspace_baselines WHERE remote_session_id = ?`, remoteSessionID).Scan(&head); err != nil {
		return "", nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT path FROM workspace_baseline_files WHERE remote_session_id = ?`, remoteSessionID)
	if err != nil {
		return "", nil, err
	}
	defer rows.Close()
	paths := map[string]bool{}
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return "", nil, err
		}
		paths[path] = true
	}
	return head, paths, rows.Err()
}

func (s *Service) mcpxPaths(ctx context.Context, remoteSessionID string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT result_json FROM clean_edit_records
        WHERE remote_session_id = ? AND state = 'succeeded'`, remoteSessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	paths := map[string]bool{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var result struct {
			Results []struct {
				Path    string `json:"path"`
				NewPath string `json:"new_path"`
			} `json:"results"`
		}
		if err := json.Unmarshal([]byte(raw), &result); err != nil {
			return nil, fmt.Errorf("decode clean edit record: %w", err)
		}
		for _, item := range result.Results {
			if item.Path != "" {
				paths[item.Path] = true
			}
			if item.NewPath != "" {
				paths[item.NewPath] = true
			}
		}
	}
	return paths, rows.Err()
}

// gitState aggregates Git state across the workspace root itself or its
// one-level nested Git roots. heads are joined as "<root>:<sha>" (or bare
// "<sha>" for the root itself) so distinct repositories stay distinguishable.
func gitState(ctx context.Context, workspaceRoot string) (string, []GitRoot, []Entry, error) {
	roots, err := DiscoverGitRoots(workspaceRoot)
	if err != nil {
		return "", nil, nil, err
	}
	if len(roots) == 0 {
		return "", nil, nil, ErrNotGitRepository
	}
	heads := make([]string, 0, len(roots))
	rootMeta := make([]GitRoot, 0, len(roots))
	var entries []Entry
	for _, root := range roots {
		rootPath := workspaceRoot
		label := ""
		if root != "" {
			rootPath = filepath.Join(workspaceRoot, root)
			label = root + ":"
		}
		head, rootEntries, stateErr := gitStateAt(ctx, rootPath)
		if stateErr != nil {
			continue
		}
		heads = append(heads, label+head)
		rootMeta = append(rootMeta, GitRoot{Path: root, Head: head})
		for _, entry := range rootEntries {
			if root != "" {
				entry.Path = root + "/" + entry.Path
				if entry.OriginalPath != "" {
					entry.OriginalPath = root + "/" + entry.OriginalPath
				}
			}
			entries = append(entries, entry)
		}
	}
	if len(entries) == 0 && len(heads) == 0 {
		return "", nil, nil, ErrNotGitRepository
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return strings.Join(heads, " "), rootMeta, entries, nil
}

// gitStateAt reads Git state from a single repository root.
func gitStateAt(ctx context.Context, workspaceRoot string) (string, []Entry, error) {
	if _, err := gitOutput(ctx, workspaceRoot, "rev-parse", "--is-inside-work-tree"); err != nil {
		return "", nil, ErrNotGitRepository
	}
	head, _ := gitOutput(ctx, workspaceRoot, "rev-parse", "HEAD")
	status, err := gitOutput(ctx, workspaceRoot, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return "", nil, err
	}
	return strings.TrimSpace(head), parseStatus([]byte(status)), nil
}

func parseStatus(raw []byte) []Entry {
	records := strings.Split(string(raw), "\x00")
	entries := make([]Entry, 0, len(records))
	for index := 0; index < len(records); index++ {
		record := records[index]
		if len(record) < 3 {
			continue
		}
		x, y := record[:1], record[1:2]
		entry := Entry{Path: record[3:], IndexStatus: x, WorktreeStatus: y, Status: statusName(x, y)}
		if (x == "R" || x == "C" || y == "R" || y == "C") && index+1 < len(records) {
			index++
			entry.OriginalPath = records[index]
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries
}

func statusName(x, y string) string {
	code := x + y
	switch {
	case code == "??":
		return "untracked"
	case code == "!!":
		return "ignored"
	case strings.Contains(code, "U") || code == "AA" || code == "DD":
		return "unmerged"
	case strings.Contains(code, "R"):
		return "renamed"
	case strings.Contains(code, "C"):
		return "copied"
	case strings.Contains(code, "D"):
		return "deleted"
	case strings.Contains(code, "A"):
		return "added"
	default:
		return "modified"
	}
}

func gitOutput(ctx context.Context, workspaceRoot string, args ...string) (string, error) {
	commandArgs := append([]string{"-C", workspaceRoot}, args...)
	command := exec.CommandContext(ctx, "git", commandArgs...)
	winproc.ConfigureNoWindow(command)
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(output), nil
}

func truncateUTF8(value string, limit int) (string, bool) {
	if len(value) <= limit {
		return value, false
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value, true
}
