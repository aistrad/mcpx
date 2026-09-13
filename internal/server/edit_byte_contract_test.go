package server

import "testing"

func TestPublicEditRequiresRevisionForExistingFiles(t *testing.T) {
	for _, op := range []string{"update", "rename"} {
		_, err := parseCleanEdits(map[string]any{"edits": []any{map[string]any{"operation": op, "path": "test.txt", "content": "new", "new_path": "target.txt"}}})
		if err == nil {
			t.Fatalf("%s 允许无旧 SHA 写入", op)
		}
	}
}

func TestPublicExactEditRejectsEvenEmptyLogicalPayload(t *testing.T) {
	for _, key := range []string{"content", "range", "replacements"} {
		item := map[string]any{"operation": "create", "path": "test.txt", "content_base64": "", "newline_policy": "exact", key: ""}
		if _, err := parseCleanEdits(map[string]any{"edits": []any{item}}); err == nil {
			t.Fatalf("错误接受同时包含 %s 的字节写入", key)
		}
	}
}
