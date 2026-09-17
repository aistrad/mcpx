package security

import (
	"regexp"
	"strings"

	"mcpx/internal/config"
)

// Decision is the outcome of command policy matching.
type Decision int

const (
	// Allow executes without human confirmation.
	Allow Decision = iota
	// Confirm requires explicit user semantic confirmation before execute.
	Confirm
	// Deny rejects the command.
	Deny
)

func (d Decision) String() string {
	switch d {
	case Allow:
		return "allow"
	case Confirm:
		return "confirm"
	case Deny:
		return "deny"
	default:
		return "unknown"
	}
}

// ParseDefault maps config string to Decision. Unknown/empty => Confirm (safe built-in).
func ParseDefault(s string) Decision {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "allow":
		return Allow
	case "deny":
		return Deny
	case "confirm", "":
		return Confirm
	default:
		return Confirm
	}
}

// CommandSegmentDecision is one independently reviewed shell segment. Operator
// is the control operator that follows this segment (&&, ||, ;), or empty for
// the final segment.
type CommandSegmentDecision struct {
	Command  string
	Operator string
	Decision Decision
}

// CommandAnalysis is the complete preflight result for one shell command.
// Execution is allowed only after every segment has been reviewed.
type CommandAnalysis struct {
	Decision Decision
	Segments []CommandSegmentDecision
	Unsafe   bool
}

// AnalyzeCommand splits supported compound commands before evaluating policy.
// &&, ||, ;, |, and newlines are safe control separators because each segment
// can be judged independently before the original command is passed to the
// shell. Unsupported shell features fail closed.
func AnalyzeCommand(rules config.CommandRules, command string) CommandAnalysis {
	parsed, unsafe := commandSegments(command)
	analysis := CommandAnalysis{Decision: Deny, Unsafe: unsafe}
	if unsafe || len(parsed) == 0 {
		return analysis
	}
	analysis.Decision = Allow
	analysis.Segments = make([]CommandSegmentDecision, 0, len(parsed))
	for _, segment := range parsed {
		decision := matchSegment(rules, segment.Command)
		analysis.Segments = append(analysis.Segments, CommandSegmentDecision{
			Command: segment.Command, Operator: segment.Operator, Decision: decision,
		})
		if decision == Deny {
			analysis.Decision = Deny
			continue
		}
		if decision == Confirm && analysis.Decision == Allow {
			analysis.Decision = Confirm
		}
	}
	return analysis
}

// MatchCommand returns the aggregate decision after every supported compound
// command segment has been reviewed.
func MatchCommand(rules config.CommandRules, command string) Decision {
	return AnalyzeCommand(rules, command).Decision
}

// IsReadonlyCommand exposes the same conservative classifier used by the
// automatic read-only policy so callers can coordinate non-mutating work
// without maintaining a second command vocabulary.
func IsReadonlyCommand(command string) bool { return isReadonlyCommand(command) }

// matchSegment evaluates a single command segment without control operators.
func matchSegment(rules config.CommandRules, segment string) Decision {
	if matchAny(rules.Deny, segment) {
		return Deny
	}
	if matchAny(rules.Confirm, segment) {
		return Confirm
	}
	if matchAny(rules.Allow, segment) {
		return Allow
	}
	if autoAllowReadonlyEnabled(rules) && isReadonlyCommand(segment) {
		return Allow
	}
	decision := ParseDefault(rules.Default)
	return decision
}

// HasUnsafeShellOperator reports active shell syntax that cannot be safely
// preflighted. &&, ||, and ; are supported separators. A quoted heredoc whose
// terminator closes the command is treated as literal stdin and can be audited
// as one segment. Pipes and newlines are split like && / || / ; so each stage
// can be audited. Ordinary redirections stay inside their segment. Background
// operators, command substitution, and unquoted heredocs remain rejected.
// Operators inside quotes or escaped with a backslash are literal content.
func HasUnsafeShellOperator(command string) bool {
	_, unsafe := commandSegments(command)
	return unsafe
}

type commandSegment struct {
	Command  string
	Operator string
}

// commandSegments scans shell control syntax without trying to fully parse a
// shell language. It distinguishes active operators from quoted/escaped
// literals and preserves supported && / || / ; / | / newline separators for
// auditing. Empty trailing newline is allowed; dangling && / || / ; / | is not.
func commandSegments(command string) ([]commandSegment, bool) {
	segments := make([]commandSegment, 0, 2)
	start := 0
	var quote byte
	escaped := false
	lastOperator := ""
	appendSegment := func(end int, operator string) bool {
		segment := strings.TrimSpace(command[start:end])
		if segment == "" {
			return false
		}
		segments = append(segments, commandSegment{Command: segment, Operator: operator})
		lastOperator = operator
		return true
	}

	for index := 0; index < len(command); index++ {
		current := command[index]
		if escaped {
			escaped = false
			continue
		}

		switch quote {
		case '\'':
			if current == '\'' {
				quote = 0
			}
			continue
		case '"':
			switch current {
			case '\\':
				escaped = true
			case '"':
				quote = 0
			case '`':
				return nil, true
			case '$':
				if index+1 < len(command) && command[index+1] == '(' {
					return nil, true
				}
			}
			continue
		}

		switch current {
		case '\\':
			escaped = true
		case '\'', '"':
			quote = current
		case '|':
			if index+1 < len(command) && command[index+1] == '|' {
				if !appendSegment(index, "||") {
					return nil, true
				}
				index++
				start = index + 1
				continue
			}
			if !appendSegment(index, "|") {
				return nil, true
			}
			start = index + 1
		case '<':
			if end, ok := quotedHeredocCommandEnd(command, index); ok {
				if !appendSegment(end, "") {
					return nil, true
				}
				return segments, false
			}
			if index+1 < len(command) && command[index+1] == '<' {
				return nil, true
			}
		case '`':
			return nil, true
		case '\n':
			if strings.TrimSpace(command[start:index]) == "" {
				start = index + 1
				continue
			}
			if !appendSegment(index, "\n") {
				return nil, true
			}
			start = index + 1
		case '\r':
			end := index
			next := index + 1
			if next < len(command) && command[next] == '\n' {
				next++
			}
			if strings.TrimSpace(command[start:end]) == "" {
				start = next
				index = next - 1
				continue
			}
			if !appendSegment(end, "\n") {
				return nil, true
			}
			start = next
			index = next - 1
		case '$':
			if index+1 < len(command) && command[index+1] == '(' {
				return nil, true
			}
		case '&':
			if index+1 < len(command) && command[index+1] == '&' {
				if !appendSegment(index, "&&") {
					return nil, true
				}
				index++
				start = index + 1
				continue
			}
			return nil, true
		case ';':
			if !appendSegment(index, ";") {
				return nil, true
			}
			start = index + 1
		}
	}
	if quote != 0 || escaped {
		return nil, true
	}
	tail := strings.TrimSpace(command[start:])
	if tail != "" {
		segments = append(segments, commandSegment{Command: tail, Operator: ""})
		return segments, false
	}
	switch lastOperator {
	case "|", "&&", "||", ";":
		return nil, true
	}
	if len(segments) == 0 {
		return nil, true
	}
	return segments, false
}

// quotedHeredocCommandEnd recognizes the narrow heredoc form that can be
// treated as literal stdin without parsing a full shell grammar:
//
//	command <<'TAG'\n...literal body...\nTAG
//	command <<"TAG"\n...literal body...\nTAG
//
// Quoting the delimiter disables shell expansion in the heredoc body. To keep
// the scanner deterministic, the terminator must close the entire command;
// arbitrary commands after the terminator still fail closed as multiline shell.
func quotedHeredocCommandEnd(command string, operatorIndex int) (int, bool) {
	if operatorIndex+3 >= len(command) || command[operatorIndex] != '<' || command[operatorIndex+1] != '<' {
		return 0, false
	}
	quote := command[operatorIndex+2]
	if quote != '\'' && quote != '"' {
		return 0, false
	}
	delimiterStart := operatorIndex + 3
	delimiterEnd := delimiterStart
	for delimiterEnd < len(command) && command[delimiterEnd] != quote {
		if command[delimiterEnd] == '\n' || command[delimiterEnd] == '\r' {
			return 0, false
		}
		delimiterEnd++
	}
	if delimiterEnd == delimiterStart || delimiterEnd >= len(command) {
		return 0, false
	}
	delimiter := command[delimiterStart:delimiterEnd]
	cursor := delimiterEnd + 1
	for cursor < len(command) && (command[cursor] == ' ' || command[cursor] == '\t') {
		cursor++
	}
	if cursor >= len(command) {
		return 0, false
	}
	switch command[cursor] {
	case '\n':
		cursor++
	case '\r':
		if cursor+1 >= len(command) || command[cursor+1] != '\n' {
			return 0, false
		}
		cursor += 2
	default:
		return 0, false
	}

	for cursor <= len(command) {
		lineStart := cursor
		lineEnd := strings.IndexByte(command[lineStart:], '\n')
		next := len(command)
		if lineEnd >= 0 {
			lineEnd += lineStart
			next = lineEnd + 1
		} else {
			lineEnd = len(command)
		}
		line := strings.TrimSuffix(command[lineStart:lineEnd], "\r")
		if line == delimiter {
			if strings.TrimSpace(command[next:]) != "" {
				return 0, false
			}
			return len(command), true
		}
		if lineEnd == len(command) {
			break
		}
		cursor = next
	}
	return 0, false
}

func autoAllowReadonlyEnabled(rules config.CommandRules) bool {
	return rules.AutoAllowReadonly == nil || *rules.AutoAllowReadonly
}

// isReadonlyCommand reports whether every supported compound segment is
// read-only. A single non-read-only segment falls back to normal policy.
func isReadonlyCommand(command string) bool {
	segments, unsafe := commandSegments(command)
	if unsafe || len(segments) == 0 {
		return false
	}
	for _, segment := range segments {
		if !isReadonlySegment(segment.Command) {
			return false
		}
	}
	return true
}

// isReadonlySegment accepts only commands with no side effects and no shell
// metacharacters (pipe, redirect, background, command substitution). Arguments
// that can be expanded by the shell are rejected because this matcher does not
// resolve them against the workspace before execution.
func isReadonlySegment(segment string) bool {
	trimmed := strings.TrimSpace(segment)
	if trimmed == "" {
		return false
	}
	if strings.ContainsAny(trimmed, "|&><`\n") || strings.Contains(trimmed, "$(") {
		return false
	}
	fields := strings.Fields(trimmed)
	if len(fields) == 0 {
		return false
	}
	switch fields[0] {
	case "ls":
		return allReadonlyArguments(fields[1:])
	case "pwd":
		return len(fields) == 1
	case "cat", "head", "tail":
		return allReadonlyArguments(fields[1:])
	case "git":
		return isReadonlyGit(fields[1:])
	}
	return false
}

// isReadonlyGit accepts git -C <relative-path> <status|diff|log|show> and the
// bare <status|diff|log|show> forms. -C targets must be relative workspace
// paths.
func isReadonlyGit(args []string) bool {
	for len(args) > 0 {
		if args[0] != "-C" {
			break
		}
		if len(args) < 2 {
			return false
		}
		if !safeReadonlyArgument(args[1]) {
			return false
		}
		args = args[2:]
	}
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "status":
		return readonlyGitArguments(args[1:])
	case "diff", "log", "show":
		// 这些读取可默认启动仓库配置的外部转换器。只有调用者明确
		// 禁用两个入口才自动放行；保留原 argv 与确认摘要，不静默改写。
		noExternal, noTextconv := false, false
		for _, arg := range args[1:] {
			if arg == "--" {
				break
			}
			noExternal = noExternal || arg == "--no-ext-diff"
			noTextconv = noTextconv || arg == "--no-textconv"
		}
		return noExternal && noTextconv && readonlyGitArguments(args[1:])
	}
	return false
}

// Git 支持长选项缩写及外部 diff/textconv。未知选项必须交回显式策略，
// 不能仅凭安全字符将带文件输出或程序执行的参数自动当作只读。
func readonlyGitArguments(args []string) bool {
	paths := false
	for _, arg := range args {
		if !safeReadonlyArgument(arg) {
			return false
		}
		if arg == "--" {
			paths = true
			continue
		}
		if paths || !strings.HasPrefix(arg, "-") {
			continue
		}
		switch arg {
		case "--short", "-s", "--branch", "-b", "--porcelain", "--porcelain=v1", "--porcelain=v2",
			"--oneline", "--stat", "--shortstat", "--numstat", "--name-only", "--name-status",
			"--summary", "--check", "--cached", "--staged", "--no-ext-diff", "--no-textconv",
			"--no-color", "--color=never", "--patch", "-p", "--no-patch", "--raw", "--abbrev-commit",
			"--graph", "--decorate", "--no-decorate", "--all", "--reverse", "--date-order", "--topo-order":
			continue
		default:
			return false
		}
	}
	return true
}

func allReadonlyArguments(args []string) bool {
	for _, arg := range args {
		if !safeReadonlyArgument(arg) {
			return false
		}
	}
	return true
}

func safeReadonlyArgument(arg string) bool {
	if arg == "" || strings.HasPrefix(arg, "/") || strings.Contains(arg, "..") {
		return false
	}
	// These characters trigger shell expansion or make the path differ from
	// the literal value reviewed by the policy matcher.
	return !strings.ContainsAny(arg, "$~{}\\*?[]'\"")
}

func matchAny(patterns []string, command string) bool {
	for _, p := range patterns {
		if p == "" {
			continue
		}
		re, err := regexp.Compile(p)
		if err != nil {
			continue
		}
		if re.MatchString(command) {
			return true
		}
	}
	return false
}

// MatchFile applies file path policy. Once any list is configured, unmatched
// paths require confirmation, making an allow list behave as a real whitelist.
func MatchFile(rules config.FileRules, path string) Decision {
	if matchAny(rules.Deny, path) {
		return Deny
	}
	if matchAny(rules.Confirm, path) {
		return Confirm
	}
	if matchAny(rules.Allow, path) {
		return Allow
	}
	if len(rules.Allow)+len(rules.Confirm)+len(rules.Deny) > 0 {
		return Confirm
	}
	return Allow
}
