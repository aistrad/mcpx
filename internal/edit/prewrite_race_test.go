package edit

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPrewriteHookDriftRejectsWholeBatch(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"first.txt", "second.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("old"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	_, err := ApplyBatchWithHook(BatchRequest{WorkspaceRoot: root, Edits: []FileEdit{
		{Path: "first.txt", Operation: OpUpdate, BaseSHA256: hashBytes([]byte("old")), Content: "new"},
		{Path: "second.txt", Operation: OpUpdate, BaseSHA256: hashBytes([]byte("old")), Content: "new"},
	}}, func(BatchResult) error {
		return os.WriteFile(filepath.Join(root, "second.txt"), []byte("external"), 0600)
	})
	if !errors.Is(err, ErrStale) {
		t.Fatalf("未拒绝 hook 后漂移: %v", err)
	}
	for name, want := range map[string]string{"first.txt": "old", "second.txt": "external"} {
		got, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || string(got) != want {
			t.Fatalf("批次覆盖文件 %s", name)
		}
	}
}

func TestPrewriteHookCannotOverwriteNewTarget(t *testing.T) {
	for _, op := range []string{OpCreate, OpRename} {
		t.Run(op, func(t *testing.T) {
			root := t.TempDir()
			item := FileEdit{Path: "target.txt", Operation: op, Content: "candidate"}
			if op == OpRename {
				item.Path, item.NewPath, item.BaseSHA256 = "source.txt", "target.txt", hashBytes([]byte("source"))
				if err := os.WriteFile(filepath.Join(root, item.Path), []byte("source"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := ApplyBatchWithHook(BatchRequest{WorkspaceRoot: root, Edits: []FileEdit{item}}, func(BatchResult) error {
				return os.WriteFile(filepath.Join(root, "target.txt"), []byte("external"), 0600)
			})
			if !errors.Is(err, ErrTargetExists) {
				t.Fatalf("未拒绝新目标: %v", err)
			}
			got, err := os.ReadFile(filepath.Join(root, "target.txt"))
			if err != nil || string(got) != "external" {
				t.Fatal("覆盖了外部文件")
			}
			if op == OpRename {
				got, err := os.ReadFile(filepath.Join(root, "source.txt"))
				if err != nil || string(got) != "source" {
					t.Fatal("移动了源文件")
				}
			}
		})
	}
}
