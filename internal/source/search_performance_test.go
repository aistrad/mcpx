package source

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 使用独立合成工作区测量未命中扫描，不读取用户工作区或运行状态。
func BenchmarkSearchUnscopedMiss(b *testing.B) {
	root := b.TempDir()
	content := []byte(strings.Repeat("这是一行普通源码 without the requested symbol\r\n", 10000))
	for _, name := range []string{"a.go", "b.go", "c.go", "d.go"} {
		if err := os.WriteFile(filepath.Join(root, name), content, 0600); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		result, err := SearchWith(root, SearchOptions{Query: "UNIQUE_ABSENT_SYMBOL", CaseSensitive: true, IncludeSHA256: true}, nil)
		if err != nil || len(result.Matches) != 0 {
			b.Fatalf("搜索结果错误: %+v %v", result, err)
		}
	}
}

func TestSearchOptimizationPreservesResults(t *testing.T) {
	root := t.TempDir()
	content := "前文\r\n中文 needle needle\r\n后文\r\nNEEDLE\r\n"
	if err := os.WriteFile(filepath.Join(root, "样本.txt"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name          string
		query         string
		regex         bool
		caseSensitive bool
		want          int
	}{
		{"literal", "needle", false, true, 2},
		{"case", "needle", false, false, 3},
		{"regex", "n.*e", true, true, 1},
		{"unicode", "中文", false, true, 1},
		{"miss", "absent", false, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := SearchOptions{Query: tc.query, Regex: tc.regex, CaseSensitive: tc.caseSensitive, IncludeSHA256: true, ContextBefore: 1, ContextAfter: 1, Limit: 1}
			var matches []Match
			for {
				result, err := SearchWith(root, opts, nil)
				if err != nil {
					t.Fatal(err)
				}
				matches = append(matches, result.Matches...)
				if result.NextCursor == "" {
					break
				}
				opts.Cursor = result.NextCursor
				if len(matches) > 4 {
					t.Fatal("分页未收敛")
				}
			}
			if len(matches) != tc.want {
				t.Fatalf("匹配数 = %d，需要 %d", len(matches), tc.want)
			}
			for _, match := range matches {
				if match.SHA256 != digest([]byte(content)) || match.Path != "样本.txt" {
					t.Fatalf("字节身份改变: %+v", match)
				}
				if match.Line == 2 && (len(match.Before) != 1 || match.Before[0] != "前文" || len(match.After) != 1 || match.After[0] != "后文") {
					t.Fatalf("上下文改变: %+v", match)
				}
			}
		})
	}
}
