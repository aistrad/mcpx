package edit

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mcpx/internal/file"
)

// ApplyBatch applies all file edits under workspaceRoot. It either applies
// every write or none (best-effort: prepares all in memory first, then writes).
func ApplyBatch(req BatchRequest) (BatchResult, error) {
	return ApplyBatchWithHook(req, nil)
}

// ApplyBatchWithHook prepares and validates the complete batch, invokes
// beforeWrite exactly once immediately before the first filesystem write, and
// then commits the prepared writes. The hook receives the exact result that
// the batch will return, including each expected post-write SHA. Callers can
// persist an idempotency record before the crash window between preparation
// and filesystem mutation.
func ApplyBatchWithHook(req BatchRequest, beforeWrite func(BatchResult) error) (BatchResult, error) {
	if strings.TrimSpace(req.WorkspaceRoot) == "" {
		return BatchResult{}, &ApplyError{Code: "INVALID_INPUT", Message: "workspace root required", Index: -1, Err: ErrInvalidInput}
	}
	if len(req.Edits) == 0 {
		return BatchResult{}, &ApplyError{Code: "INVALID_INPUT", Message: "edits required", Index: -1, Err: ErrInvalidInput}
	}

	type prepared struct {
		edit     FileEdit
		absPath  string
		absNew   string
		original []byte
		proposed []byte
		mode     os.FileMode
		diff     string
		changed  int
		newHash  string
		deleted  bool
	}

	preparedList := make([]prepared, 0, len(req.Edits))
	totalChanged := 0
	var diffParts []string
	seenPaths := make(map[string]int, len(req.Edits)*2)

	for fileIndex, item := range req.Edits {
		op := strings.TrimSpace(item.Operation)
		if op == "" {
			op = OpUpdate
		}
		path := strings.TrimSpace(item.Path)
		if path == "" {
			return BatchResult{}, &ApplyError{Code: "INVALID_INPUT", Message: "path required", Index: fileIndex, Err: ErrInvalidInput}
		}
		lexical, lexicalErr := file.LexicalPath(req.WorkspaceRoot, path)
		if lexicalErr != nil {
			return BatchResult{}, &ApplyError{Code: "INVALID_PATH", Message: lexicalErr.Error(), Path: path, Index: fileIndex, Err: lexicalErr}
		}
		abs, err := file.Resolve(req.WorkspaceRoot, path)
		if err != nil {
			return BatchResult{}, &ApplyError{Code: "INVALID_PATH", Message: err.Error(), Path: path, Index: fileIndex, Err: err}
		}
		if previous, exists := seenPaths[abs]; exists {
			return BatchResult{}, &ApplyError{
				Code: "INVALID_INPUT", Message: fmt.Sprintf("path %q appears in edits[%d] and edits[%d]", path, previous, fileIndex),
				Path: path, Index: fileIndex, Err: ErrInvalidInput,
			}
		}
		if req.ValidatePath != nil {
			if err := req.ValidatePath(abs); err != nil {
				return BatchResult{}, err
			}
		}
		seenPaths[abs] = fileIndex

		p := prepared{edit: item, absPath: abs}
		p.edit.Operation = op
		var exactBytes []byte
		if item.ContentBase64 != nil {
			if op == OpUpdate && strings.TrimSpace(item.BaseSHA256) == "" {
				return BatchResult{}, &ApplyError{Code: "INVALID_INPUT", Message: "exact byte update requires base_sha256", Path: path, Index: fileIndex, Err: ErrInvalidInput}
			}
			if item.NewlinePolicy != "exact" || item.ExpectedFormat == nil || (op != OpCreate && op != OpUpdate) || item.Content != "" || len(item.Replacements) > 0 || item.Range != nil {
				return BatchResult{}, &ApplyError{Code: "INVALID_INPUT", Message: "content_base64 requires newline_policy=exact, expected_format and create/update without logical content, replacements or range", Path: path, Index: fileIndex, Err: ErrInvalidInput}
			}
			exactBytes, err = base64.StdEncoding.Strict().DecodeString(*item.ContentBase64)
			if err != nil {
				return BatchResult{}, &ApplyError{Code: "INVALID_INPUT", Message: "invalid content_base64", Path: path, Index: fileIndex, Err: ErrInvalidInput}
			}
		} else if item.NewlinePolicy != "" && item.NewlinePolicy != "preserve" {
			return BatchResult{}, &ApplyError{Code: "INVALID_INPUT", Message: "newline_policy=exact requires content_base64", Path: path, Index: fileIndex, Err: ErrInvalidInput}
		}

		switch op {
		case OpCreate:
			if _, err := os.Stat(abs); err == nil {
				return BatchResult{}, &ApplyError{Code: "TARGET_EXISTS", Message: "create target already exists", Path: path, Index: fileIndex, Err: ErrTargetExists}
			} else if err != nil && !os.IsNotExist(err) {
				return BatchResult{}, err
			}
			if len(item.Replacements) > 0 || item.Range != nil {
				return BatchResult{}, &ApplyError{Code: "INVALID_INPUT", Message: "create requires content, not replacements or range", Path: path, Index: fileIndex, Err: ErrInvalidInput}
			}
			logical := stripUTF8BOM(normalizeNewlines(item.Content))
			format := file.DetectFormat([]byte(item.Content))
			if format.LineEnding == "" || format.LineEnding == "mixed" {
				format.LineEnding = "LF"
			}
			p.mode = 0o644
			p.diff, p.changed = UnifiedDiff(path, "", logical)
			p.proposed = encodeWithFormat(logical, format)
			if item.ContentBase64 != nil {
				p.proposed = exactBytes
				logical, _, err = normalizeToLogical(exactBytes)
				if err != nil {
					return BatchResult{}, err
				}
				p.diff, p.changed = UnifiedDiff(path, "", logical)
			}
			p.newHash = hashBytes(p.proposed)

		case OpUpdate:
			original, mode, err := readFile(abs)
			if err != nil {
				return BatchResult{}, &ApplyError{Code: "NOT_FOUND", Message: err.Error(), Path: path, Index: fileIndex, Err: err}
			}
			p.original = original
			p.mode = mode
			if err := checkBase(original, item.BaseSHA256, path, fileIndex); err != nil {
				return BatchResult{}, err
			}
			logical, format, normalizeErr := normalizeToLogical(original)
			if normalizeErr != nil {
				return BatchResult{}, &ApplyError{Code: "UNSUPPORTED_ENCODING", Message: normalizeErr.Error(), Path: path, Index: fileIndex, Err: normalizeErr}
			}
			proposedLogical, err := applyUpdate(logical, item, path, fileIndex)
			if err != nil {
				return BatchResult{}, err
			}
			p.diff, p.changed = UnifiedDiff(path, logical, proposedLogical)
			p.proposed = encodeWithOriginalFormat(proposedLogical, format, original)
			if item.ContentBase64 != nil {
				p.proposed = exactBytes
				proposedLogical, _, err = normalizeToLogical(exactBytes)
				if err != nil {
					return BatchResult{}, err
				}
				p.diff, p.changed = UnifiedDiff(path, logical, proposedLogical)
			}
			p.newHash = hashBytes(p.proposed)

		case OpDelete:
			linkInfo, linkErr := os.Lstat(lexical)
			if linkErr != nil {
				return BatchResult{}, &ApplyError{Code: "NOT_FOUND", Message: linkErr.Error(), Path: path, Index: fileIndex, Err: linkErr}
			}
			if linkInfo.Mode()&os.ModeSymlink != 0 {
				return BatchResult{}, &ApplyError{Code: "SYMLINK_NOT_ALLOWED", Message: "symlink deletion is not allowed", Path: path, Index: fileIndex}
			}
			if !linkInfo.Mode().IsRegular() {
				return BatchResult{}, &ApplyError{Code: "DELETE_FILE_ONLY", Message: "clean edit delete accepts regular files only", Path: path, Index: fileIndex}
			}
			original, mode, err := readFile(abs)
			if err != nil {
				return BatchResult{}, &ApplyError{Code: "NOT_FOUND", Message: err.Error(), Path: path, Index: fileIndex, Err: err}
			}
			p.original = original
			p.mode = mode
			if err := checkBase(original, item.BaseSHA256, path, fileIndex); err != nil {
				return BatchResult{}, err
			}
			logical, _, normalizeErr := normalizeToLogical(original)
			if normalizeErr != nil {
				return BatchResult{}, &ApplyError{Code: "UNSUPPORTED_ENCODING", Message: normalizeErr.Error(), Path: path, Index: fileIndex, Err: normalizeErr}
			}
			p.diff, p.changed = UnifiedDiff(path, logical, "")
			p.deleted = true

		case OpRename:
			newPath := strings.TrimSpace(item.NewPath)
			if newPath == "" {
				return BatchResult{}, &ApplyError{Code: "INVALID_INPUT", Message: "new_path required for rename", Path: path, Index: fileIndex, Err: ErrInvalidInput}
			}
			absNew, err := file.Resolve(req.WorkspaceRoot, newPath)
			if err != nil {
				return BatchResult{}, &ApplyError{Code: "INVALID_PATH", Message: err.Error(), Path: newPath, Index: fileIndex, Err: err}
			}
			if previous, exists := seenPaths[absNew]; exists {
				return BatchResult{}, &ApplyError{
					Code: "INVALID_INPUT", Message: fmt.Sprintf("rename target %q conflicts with edits[%d]", newPath, previous),
					Path: newPath, Index: fileIndex, Err: ErrInvalidInput,
				}
			}
			seenPaths[absNew] = fileIndex
			if req.ValidatePath != nil {
				if err := req.ValidatePath(absNew); err != nil {
					return BatchResult{}, err
				}
			}
			if _, err := os.Stat(absNew); err == nil {
				return BatchResult{}, &ApplyError{Code: "TARGET_EXISTS", Message: "rename target already exists", Path: newPath, Index: fileIndex, Err: ErrTargetExists}
			}
			original, mode, err := readFile(abs)
			if err != nil {
				return BatchResult{}, &ApplyError{Code: "NOT_FOUND", Message: err.Error(), Path: path, Index: fileIndex, Err: err}
			}
			if err := checkBase(original, item.BaseSHA256, path, fileIndex); err != nil {
				return BatchResult{}, err
			}
			p.original = original
			p.mode = mode
			p.absNew = absNew
			p.proposed = original
			p.newHash = hashBytes(original)
			p.diff = fmt.Sprintf("rename from %s\nrename to %s\n", path, newPath)
			p.changed = 0

		default:
			return BatchResult{}, &ApplyError{Code: "UNSUPPORTED", Message: "unsupported operation " + op, Path: path, Index: fileIndex, Err: ErrUnsupportedOp}
		}

		if expected := item.ExpectedFormat; expected != nil {
			actual := file.DetectFormat(p.proposed)
			if op == OpDelete || expected.Charset == "" || expected.BOM == "" || expected.LineEnding == "" || actual.Charset != expected.Charset || actual.BOM != expected.BOM || actual.LineEnding != expected.LineEnding {
				return BatchResult{}, &ApplyError{Code: "FORMAT_MISMATCH", Message: "proposed bytes do not match expected_format", Path: path, Index: fileIndex, Err: ErrInvalidInput}
			}
		}
		totalChanged += p.changed
		if totalChanged > MaxChangedLines {
			return BatchResult{}, &ApplyError{
				Code:         "TOO_MANY_CHANGES",
				Message:      fmt.Sprintf("total changed lines would be %d (max %d)", totalChanged, MaxChangedLines),
				Path:         path,
				Index:        fileIndex,
				ChangedLines: totalChanged,
				Err:          ErrTooManyChanges,
			}
		}
		if p.diff != "" {
			diffParts = append(diffParts, p.diff)
		}
		preparedList = append(preparedList, p)
	}

	// Build the complete result before writing. This is also the durable
	// reconcile record for callers that use ApplyBatchWithHook.
	plannedResults := make([]FileResult, 0, len(preparedList))
	for _, p := range preparedList {
		originalSHA := ""
		if p.edit.Operation != OpCreate {
			originalSHA = hashBytes(p.original)
		}
		switch p.edit.Operation {
		case OpCreate, OpUpdate:
			plannedResults = append(plannedResults, FileResult{
				Path: p.edit.Path, Operation: p.edit.Operation,
				OriginalSHA256: originalSHA, NewSHA256: p.newHash,
				ChangedLines: p.changed, Diff: p.diff,
			})
		case OpDelete:
			plannedResults = append(plannedResults, FileResult{
				Path: p.edit.Path, Operation: OpDelete,
				OriginalSHA256: originalSHA, ChangedLines: p.changed,
				Diff: p.diff, Deleted: true,
			})
		case OpRename:
			plannedResults = append(plannedResults, FileResult{
				Path: p.edit.Path, NewPath: p.edit.NewPath, Operation: OpRename,
				OriginalSHA256: originalSHA, NewSHA256: p.newHash,
				ChangedLines: p.changed, Diff: p.diff,
			})
		}
	}
	batchResult := BatchResult{
		Results:           plannedResults,
		TotalChangedLines: totalChanged,
		DiffSummary:       strings.Join(diffParts, "\n"),
	}
	if req.DryRun {
		return batchResult, nil
	}
	if beforeWrite != nil {
		if err := beforeWrite(batchResult); err != nil {
			return BatchResult{}, err
		}
	}
	// 持久化 hook 可能阻塞或允许外部写入；在任何本批次副作用前重新核对整批。
	for index, p := range preparedList {
		currentPath, err := file.Resolve(req.WorkspaceRoot, p.edit.Path)
		if err != nil || currentPath != p.absPath {
			return BatchResult{}, &ApplyError{Code: "STALE_REVISION", Message: "file physical path changed before write", Path: p.edit.Path, Index: index, Err: ErrStale}
		}
		if p.edit.Operation == OpCreate {
			// 最终物理路径策略不依赖调用者传入的字面路径。
			if _, err := os.Lstat(currentPath); !os.IsNotExist(err) {
				return BatchResult{}, &ApplyError{Code: "TARGET_EXISTS", Message: "create target appeared before write", Path: p.edit.Path, Index: index, Err: ErrTargetExists}
			}
		} else {
			current, err := os.ReadFile(currentPath)
			if err != nil {
				return BatchResult{}, &ApplyError{Code: "STALE_REVISION", Message: "file unavailable before write", Path: p.edit.Path, Index: index, Err: ErrStale}
			}
			if err := checkBase(current, hashBytes(p.original), p.edit.Path, index); err != nil {
				return BatchResult{}, err
			}
		}
		if req.ValidatePath != nil {
			if err := req.ValidatePath(currentPath); err != nil {
				return BatchResult{}, err
			}
		}
		if p.edit.Operation == OpRename {
			currentNew, err := file.Resolve(req.WorkspaceRoot, p.edit.NewPath)
			if err != nil || currentNew != p.absNew {
				return BatchResult{}, &ApplyError{Code: "STALE_REVISION", Message: "rename target physical path changed before write", Path: p.edit.NewPath, Index: index, Err: ErrStale}
			}
			if _, err := os.Lstat(currentNew); !os.IsNotExist(err) {
				return BatchResult{}, &ApplyError{Code: "TARGET_EXISTS", Message: "rename target appeared before write", Path: p.edit.NewPath, Index: index, Err: ErrTargetExists}
			}
			if req.ValidatePath != nil {
				if err := req.ValidatePath(currentNew); err != nil {
					return BatchResult{}, err
				}
			}
		}
	}

	// Apply writes only after all validation, line counting, and durable
	// pre-write hooks have completed.
	var deleteRoot *os.Root
	for _, p := range preparedList {
		if p.edit.Operation == OpDelete {
			var openErr error
			deleteRoot, openErr = os.OpenRoot(req.WorkspaceRoot)
			if openErr != nil {
				return BatchResult{}, openErr
			}
			break
		}
	}
	if deleteRoot != nil {
		defer deleteRoot.Close()
	}
	for _, p := range preparedList {
		switch p.edit.Operation {
		case OpCreate, OpUpdate:
			if err := atomicWrite(p.absPath, p.proposed, p.mode); err != nil {
				return BatchResult{}, err
			}
		case OpDelete:
			if err := deleteRoot.Remove(p.edit.Path); err != nil {
				return BatchResult{}, err
			}
		case OpRename:
			if err := os.MkdirAll(filepath.Dir(p.absNew), 0o755); err != nil {
				return BatchResult{}, err
			}
			if err := os.Rename(p.absPath, p.absNew); err != nil {
				return BatchResult{}, err
			}
		}
	}

	// 成功回执来自实际落盘字节；预演和写前持久化记录不伪造 readback。
	for index, p := range preparedList {
		if p.deleted {
			continue
		}
		path := p.absPath
		if p.edit.Operation == OpRename {
			path = p.absNew
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return BatchResult{}, err
		}
		sha := hashBytes(content)
		if sha != p.newHash {
			return BatchResult{}, &ApplyError{Code: "READBACK_MISMATCH", Message: "written bytes differ from prepared result", Path: p.edit.Path, Index: index, Current: sha, Err: ErrStale}
		}
		tail := content
		if len(tail) > 16 {
			tail = tail[len(tail)-16:]
		}
		batchResult.Results[index].Readback = &ByteReadback{ByteLength: len(content), SHA256: sha, Format: file.DetectFormat(content), TailHex: hex.EncodeToString(tail)}
	}
	return batchResult, nil
}

func applyUpdate(logical string, item FileEdit, path string, fileIndex int) (string, error) {
	if item.Range != nil {
		if len(item.Replacements) > 0 || strings.TrimSpace(item.Content) != "" {
			return "", &ApplyError{Code: "INVALID_INPUT", Message: "range, content and replacements are mutually exclusive", Path: path, Index: fileIndex, Err: ErrInvalidInput}
		}
		if strings.TrimSpace(item.BaseSHA256) == "" {
			return "", &ApplyError{Code: "INVALID_INPUT", Message: "range update requires base_sha256", Path: path, Index: fileIndex, Err: ErrInvalidInput}
		}
		return applyLineRange(logical, *item.Range, path, fileIndex)
	}
	if len(item.Replacements) > 0 {
		if strings.TrimSpace(item.Content) != "" {
			return "", &ApplyError{Code: "INVALID_INPUT", Message: "content and replacements are mutually exclusive", Path: path, Index: fileIndex, Err: ErrInvalidInput}
		}
		return applyReplacements(logical, item.Replacements, path, fileIndex)
	}
	// Full content replace
	return stripUTF8BOM(normalizeNewlines(item.Content)), nil
}

func applyLineRange(logical string, lineRange LineRange, path string, fileIndex int) (string, error) {
	startLine, endLine := lineRange.StartLine, lineRange.EndLine
	if startLine < 1 || endLine < startLine {
		return "", &ApplyError{Code: "INVALID_INPUT", Message: "range requires 1-based start_line <= end_line", Path: path, Index: fileIndex, Err: ErrInvalidInput}
	}
	lineCount := strings.Count(logical, "\n")
	if !strings.HasSuffix(logical, "\n") || logical == "" {
		lineCount++
	}
	if endLine > lineCount {
		return "", &ApplyError{Code: "RANGE_OUT_OF_BOUNDS", Message: fmt.Sprintf("range ends at line %d but file has %d line(s)", endLine, lineCount), Path: path, Index: fileIndex, Err: ErrInvalidInput}
	}
	startOffset, endOffset := 0, len(logical)
	endHadNewline := false
	lineStart := 0
	for line := 1; line <= endLine; line++ {
		relativeEnd := strings.IndexByte(logical[lineStart:], '\n')
		lineEnd := len(logical)
		hasNewline := relativeEnd >= 0
		if hasNewline {
			lineEnd = lineStart + relativeEnd
		}
		if line == startLine {
			startOffset = lineStart
		}
		if line == endLine {
			endOffset = lineEnd
			endHadNewline = hasNewline
			if hasNewline {
				endOffset++
			}
			break
		}
		lineStart = lineEnd + 1
	}
	replacement := stripUTF8BOM(normalizeNewlines(lineRange.Replacement))
	if endHadNewline && replacement != "" && !strings.HasSuffix(replacement, "\n") {
		replacement += "\n"
	}
	return logical[:startOffset] + replacement + logical[endOffset:], nil
}

func applyReplacements(logical string, reps []Replacement, path string, fileIndex int) (string, error) {
	type hit struct {
		index int
		start int
		end   int
		rep   string
	}
	var hits []hit
	for i, r := range reps {
		if r.Match == "" {
			return "", &ApplyError{Code: "INVALID_INPUT", Message: "match required", Path: path, Index: i, Err: ErrInvalidInput}
		}
		count := strings.Count(logical, r.Match)
		switch count {
		case 0:
			return "", &ApplyError{Code: "MATCH_NOT_FOUND", Message: fmt.Sprintf("match not found at replacements[%d]", i), Path: path, Index: i, Err: ErrMatchNotFound}
		case 1:
			start := strings.Index(logical, r.Match)
			hits = append(hits, hit{index: i, start: start, end: start + len(r.Match), rep: r.Replacement})
		default:
			return "", &ApplyError{Code: "MATCH_AMBIGUOUS", Message: fmt.Sprintf("match occurs %d times at replacements[%d]", count, i), Path: path, Index: i, Err: ErrMatchAmbiguous}
		}
	}
	// Apply from end to start so earlier offsets stay valid.
	for i := 0; i < len(hits); i++ {
		for j := i + 1; j < len(hits); j++ {
			if hits[j].start > hits[i].start {
				hits[i], hits[j] = hits[j], hits[i]
			}
		}
		if i > 0 && hits[i-1].start < hits[i].end {
			return "", &ApplyError{
				Code:    "INVALID_INPUT",
				Message: fmt.Sprintf("replacements[%d] overlaps replacements[%d]", hits[i].index, hits[i-1].index),
				Path:    path,
				Index:   hits[i].index,
				Err:     ErrInvalidInput,
			}
		}
	}
	out := logical
	for _, h := range hits {
		out = out[:h.start] + h.rep + out[h.end:]
	}
	return out, nil
}

func checkBase(content []byte, baseSHA, path string, fileIndex int) error {
	baseSHA = strings.TrimSpace(baseSHA)
	if baseSHA == "" {
		return nil
	}
	current := hashBytes(content)
	want := strings.TrimPrefix(strings.ToLower(baseSHA), "sha256:")
	cur := strings.TrimPrefix(strings.ToLower(current), "sha256:")
	if want != cur {
		return &ApplyError{
			Code:    "STALE_REVISION",
			Message: "base_sha256 does not match current file",
			Path:    path,
			Index:   fileIndex,
			Current: current,
			Err:     ErrStale,
		}
	}
	return nil
}

func readFile(abs string) ([]byte, os.FileMode, error) {
	info, err := os.Stat(abs)
	if err != nil {
		return nil, 0, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("not a regular file")
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, 0, err
	}
	return data, info.Mode().Perm(), nil
}

func hashBytes(content []byte) string {
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func normalizeNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return s
}

func stripUTF8BOM(s string) string {
	return strings.TrimPrefix(s, "\ufeff")
}
