package security

import (
	"strings"
	"testing"

	"mcpx/internal/config"
)

func TestMatchCommandDenyPrefixBlocksCommand(t *testing.T) {
	rules := config.CommandRules{Deny: []string{`^rm\b`, `^git push`}}
	for _, command := range []string{
		"rm -rf ./x",
		"git push origin main",
		"git status && rm -rf ./x",
		"git status; git push",
		"ls -la && rm x",
	} {
		if got := MatchCommand(rules, command); got != Deny {
			t.Errorf("%q: got %s, want deny", command, got)
		}
	}
	for _, command := range []string{
		"git status",
		"git status && git diff --no-ext-diff --no-textconv",
		"go test ./...",
		"echo hi",
	} {
		want := Confirm
		if strings.HasPrefix(command, "git status") {
			want = Allow
		}
		if got := MatchCommand(rules, command); got != want {
			t.Errorf("%q: got %s, want %s", command, got, want)
		}
	}
}

func TestMatchCommandDefaultsToConfirm(t *testing.T) {
	rules := config.CommandRules{}
	for _, command := range []string{
		"rm -rf ./x", "git push origin main", "echo hi", "npm test",
	} {
		if got := MatchCommand(rules, command); got != Confirm {
			t.Errorf("%q: got %s, want confirm", command, got)
		}
	}
	for _, command := range []string{"git status", "git -C sub status --short", "ls -la", "cat internal/a.go"} {
		if got := MatchCommand(rules, command); got != Allow {
			t.Errorf("readonly %q: got %s, want allow", command, got)
		}
	}
}

func TestDefaultCommandPolicyAllowsUnmatchedCommands(t *testing.T) {
	rules := config.DefaultConfig().Security.Commands
	for _, command := range []string{"go test ./...", "rm -rf ./x", "echo hi"} {
		if got := MatchCommand(rules, command); got != Allow {
			t.Errorf("%q: got %s, want allow", command, got)
		}
	}
	if got := MatchCommand(rules, "git push origin main"); got != Confirm {
		t.Fatalf("explicit confirm rule got %s, want confirm", got)
	}
}

func TestMatchCommandRejectsUnsafeOperators(t *testing.T) {
	// Active pipes, redirections, background operators, and command substitution
	// cannot be split into independently judged segments and are rejected.
	rules := config.CommandRules{}
	for _, command := range []string{
		"ls | sh",
		"ls > out.txt",
		"cat < in.txt",
		"ls $(dangerous)",
		"echo \"$(dangerous)\"",
		"ls `id`",
		"echo \"`id`\"",
		"sleep 5 &",
		"ls\nrm -rf x",
	} {
		if got := MatchCommand(rules, command); got != Deny {
			t.Errorf("%q: got %s, want deny", command, got)
		}
	}
}

func TestMatchCommandAllowsQuotedHeredocAfterAudit(t *testing.T) {
	command := "python3 - <<'PY'\nprint('body may contain | ; && || $(literal) `literal`')\nPY"
	rules := config.DefaultConfig().Security.Commands
	analysis := AnalyzeCommand(rules, command)
	if analysis.Unsafe || analysis.Decision != Allow || len(analysis.Segments) != 1 {
		t.Fatalf("quoted heredoc analysis=%+v", analysis)
	}
	if HasUnsafeShellOperator(command) {
		t.Fatalf("quoted heredoc reported unsafe")
	}

	confirmRules := config.CommandRules{Default: "allow", Confirm: []string{`^python3\b`}}
	if got := MatchCommand(confirmRules, command); got != Confirm {
		t.Fatalf("quoted heredoc confirm policy got %s", got)
	}
	denyRules := config.CommandRules{Default: "allow", Deny: []string{`^python3\b`}}
	if got := MatchCommand(denyRules, command); got != Deny {
		t.Fatalf("quoted heredoc deny policy got %s", got)
	}
}

func TestMatchCommandAllowsQuotedHeredocAsFinalCompoundSegment(t *testing.T) {
	command := "git status && python3 - <<\"PY\"\nprint('sample')\nPY"
	analysis := AnalyzeCommand(config.DefaultConfig().Security.Commands, command)
	if analysis.Unsafe || analysis.Decision != Allow || len(analysis.Segments) != 2 {
		t.Fatalf("compound heredoc analysis=%+v", analysis)
	}
	if analysis.Segments[0].Operator != "&&" || analysis.Segments[1].Operator != "" {
		t.Fatalf("compound heredoc operators=%+v", analysis.Segments)
	}
}

func TestMatchCommandRejectsUnquotedOrTrailingHeredocShell(t *testing.T) {
	rules := config.DefaultConfig().Security.Commands
	for _, command := range []string{
		"python3 - <<PY\nprint('expanded')\nPY",
		"python3 - <<'PY'\nprint('missing terminator')",
		"python3 - <<'PY'\nprint('ok')\nPY\ngit status",
	} {
		if got := MatchCommand(rules, command); got != Deny {
			t.Fatalf("unsupported heredoc %q: got %s, want deny", command, got)
		}
	}
}

func TestMatchCommandTreatsQuotedAndEscapedOperatorsAsLiterals(t *testing.T) {
	rules := config.DefaultConfig().Security.Commands
	for _, command := range []string{
		`grep -R -n "changed_lines\|diff_summary\|Created\|Updated" internal cmd`,
		`printf '%s\n' 'left|right'`,
		`printf "%s" "left > right"`,
		`printf "%s" '$(not-a-substitution)'`,
		`printf "%s" '\` + "`" + `not-a-substitution\` + "`" + `'`,
		`printf foo \| bar`,
		`printf "text; rm -rf ignored"`,
		`printf "text && rm -rf ignored"`,
		`printf "text || rm -rf ignored"`,
	} {
		if got := MatchCommand(rules, command); got != Allow {
			t.Errorf("literal operator command %q: got %s, want allow", command, got)
		}
		if HasUnsafeShellOperator(command) {
			t.Errorf("literal operator command %q reported unsafe", command)
		}
	}
}

func TestMatchCommandSegmentsRespectDenyAndAllowLists(t *testing.T) {
	rules := config.CommandRules{Allow: []string{`^go\b`}, Deny: []string{`^rm\b`}}
	for _, command := range []string{"go test && rm -rf x", "go test || rm -rf x"} {
		if got := MatchCommand(rules, command); got != Deny {
			t.Fatalf("deny segment must reject whole command %q, got %s", command, got)
		}
	}
	for _, command := range []string{"go build && go vet", "go build || go vet"} {
		if got := MatchCommand(rules, command); got != Allow {
			t.Fatalf("allowed segments must pass preflight for %q, got %s", command, got)
		}
	}
	if got := MatchCommand(rules, "go build"); got != Allow {
		t.Fatalf("allow list got %s", got)
	}
}

func TestAnalyzeCommandPreservesConditionalOperatorsAndDecisions(t *testing.T) {
	rules := config.CommandRules{
		Allow:   []string{`^go\b`},
		Confirm: []string{`^docker\b`},
		Deny:    []string{`^rm\b`},
	}
	analysis := AnalyzeCommand(rules, "go test && docker build . || rm -rf x")
	if analysis.Unsafe || analysis.Decision != Deny || len(analysis.Segments) != 3 {
		t.Fatalf("analysis=%+v", analysis)
	}
	wantOperators := []string{"&&", "||", ""}
	wantDecisions := []Decision{Allow, Confirm, Deny}
	for index, segment := range analysis.Segments {
		if segment.Operator != wantOperators[index] || segment.Decision != wantDecisions[index] {
			t.Fatalf("segment %d=%+v", index, segment)
		}
	}
}

func TestMatchCommandRejectsMalformedConditionalChains(t *testing.T) {
	rules := config.DefaultConfig().Security.Commands
	for _, command := range []string{"echo ok ||", "|| echo ok", "echo ok &&", "echo ok || || echo no"} {
		if got := MatchCommand(rules, command); got != Deny {
			t.Errorf("malformed conditional %q: got %s, want deny", command, got)
		}
	}
}

func TestMatchCommandConfirmRulesReturnConfirm(t *testing.T) {
	rules := config.CommandRules{Confirm: []string{`^git push`, `^docker`}}
	for _, command := range []string{"git push origin main", "docker build .", "echo hi"} {
		if got := MatchCommand(rules, command); got != Confirm {
			t.Errorf("%q: got %s, want confirm", command, got)
		}
	}
}

func TestMatchCommandRejectsReadonlyShellExpansion(t *testing.T) {
	rules := config.CommandRules{}
	for _, command := range []string{
		"cat $HOME/.ssh/id_rsa", "cat ~/.mcpx/config.yaml", "ls ${HOME}",
		"git -C \\${WORKSPACE} status", "cat secrets/*.txt",
	} {
		if got := MatchCommand(rules, command); got != Confirm {
			t.Errorf("%q: got %s, want confirm after readonly rejection", command, got)
		}
	}
}

func TestMatchCommandExplicitDenyBeatsAllow(t *testing.T) {
	rules := config.CommandRules{Allow: []string{`^git status`}, Deny: []string{`^git status`}}
	if got := MatchCommand(rules, "git status"); got != Deny {
		t.Fatalf("explicit deny got %s", got)
	}
}

func TestMatchCommandAutoAllowReadonlyWithDenyDefault(t *testing.T) {
	// The read-only whitelist still opens read-only commands under a deny
	// default, and can be switched off.
	rules := config.CommandRules{Default: "deny"}
	for _, command := range []string{"git status", "git -C sub status --short", "ls"} {
		if got := MatchCommand(rules, command); got != Allow {
			t.Errorf("%q: got %s, want allow", command, got)
		}
	}
	if got := MatchCommand(rules, "rm -rf ./x"); got != Deny {
		t.Fatalf("non-readonly command got %s, want deny", got)
	}
	disabled := false
	rules = config.CommandRules{Default: "deny", AutoAllowReadonly: &disabled}
	if got := MatchCommand(rules, "git status"); got != Deny {
		t.Fatalf("disabled auto allow got %s, want deny", got)
	}
}

func TestMatchFileConfiguredRulesDefaultToConfirm(t *testing.T) {
	rules := config.FileRules{
		Allow: []string{`^src/`},
		Deny:  []string{`(^|/)\.env$`},
	}
	if got := MatchFile(rules, ".env"); got != Deny {
		t.Fatalf("deny got %s", got)
	}
	if got := MatchFile(rules, "src/main.go"); got != Allow {
		t.Fatalf("allow got %s", got)
	}
	if got := MatchFile(rules, "README.md"); got != Confirm {
		t.Fatalf("unmatched got %s", got)
	}
}

func TestMatchCommandAutoAllowReadonly(t *testing.T) {
	rules := config.CommandRules{}
	for _, command := range []string{
		"git status",
		"git -C fanyi-cloud status --short && git -C fanyi-cloud-ui status --short",
		"git status || git diff --no-ext-diff --no-textconv",
		"git status; git diff --no-ext-diff --no-textconv",
		"git log --no-ext-diff --no-textconv --oneline",
		"git show --no-ext-diff --no-textconv HEAD",
		"ls",
		"ls -la",
		"cat internal/server/tools.go",
		"head -5 go.mod",
		"tail -5 go.mod",
		"pwd",
	} {
		if got := MatchCommand(rules, command); got != Allow {
			t.Errorf("%q: got %s, want allow", command, got)
		}
	}
}

func TestParseDefault(t *testing.T) {
	if ParseDefault("") != Confirm || ParseDefault("confirm") != Confirm {
		t.Fatal("confirm")
	}
	if ParseDefault("allow") != Allow {
		t.Fatal("allow")
	}
	if ParseDefault("deny") != Deny {
		t.Fatal("deny")
	}
}
