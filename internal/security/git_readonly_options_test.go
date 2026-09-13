package security

import "testing"

func TestReadonlyGitRejectsEffectfulAndAbbreviatedOptions(t *testing.T) {
	for _, args := range [][]string{
		{"diff", "--output=C:/outside/probe.diff"}, {"diff", "--out=C:/outside/probe.diff"},
		{"log", "--output", "probe"}, {"show", "--textconv"}, {"diff", "--ext-diff"},
		{"diff", "--ext"}, {"status", "--unknown"},
	} {
		if isReadonlyGit(args) {
			t.Fatalf("自动放行有副作用或未知参数: %q", args)
		}
	}
	for _, args := range [][]string{{"status", "--short"}, {"log", "--oneline"}, {"diff", "--stat"}, {"show", "HEAD"}} {
		if !isReadonlyGit(args) {
			t.Fatalf("拒绝已知只读参数: %q", args)
		}
	}
}
