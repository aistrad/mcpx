package edit

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"mcpx/internal/file"
)

func TestExactByteContractAndReadback(t *testing.T) {
	fixtures := map[string][]byte{
		"empty": {}, "no-lf": []byte("中文尾部"), "lf": []byte("中文\n"), "crlf": []byte("中文\r\n"),
		"mixed": []byte("a\r\nb\nc\r"), "bom": append([]byte{239, 187, 191}, []byte("中文\r\n")...),
		"utf16": {255, 254, 45, 78, 135, 101, 13, 0, 10, 0},
	}
	for name, content := range fixtures {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "file.txt")
			if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
				t.Fatal(err)
			}
			encoded := base64.StdEncoding.EncodeToString(content)
			format := file.DetectFormat(content)
			request := BatchRequest{WorkspaceRoot: root, Edits: []FileEdit{{Path: "file.txt", Operation: OpUpdate, BaseSHA256: hashBytes([]byte("old")), ContentBase64: &encoded, NewlinePolicy: "exact", ExpectedFormat: &ExpectedFormat{Charset: format.Charset, BOM: format.BOM, LineEnding: format.LineEnding}}}}
			preview := request
			preview.DryRun = true
			planned, err := ApplyBatch(preview)
			if err != nil || planned.Results[0].Readback != nil {
				t.Fatalf("预演错误: %v", err)
			}
			result, err := ApplyBatch(request)
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, content) {
				t.Fatalf("目标字节变化: %v", err)
			}
			readback := result.Results[0].Readback
			tail := content
			if len(tail) > 16 {
				tail = tail[len(tail)-16:]
			}
			if readback == nil || readback.ByteLength != len(content) || readback.SHA256 != hashBytes(content) || readback.TailHex != hex.EncodeToString(tail) || readback.Format.Charset != format.Charset || readback.Format.BOM != format.BOM || readback.Format.LineEnding != format.LineEnding {
				t.Fatalf("回读不匹配: %+v", readback)
			}
		})
	}
}

func TestExpectedFormatMismatchHasNoWrite(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "file.txt")
	if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	encoded := base64.StdEncoding.EncodeToString([]byte("new\r\n"))
	_, err := ApplyBatch(BatchRequest{WorkspaceRoot: root, Edits: []FileEdit{{Path: "file.txt", Operation: OpUpdate, BaseSHA256: hashBytes([]byte("old")), ContentBase64: &encoded, NewlinePolicy: "exact", ExpectedFormat: &ExpectedFormat{Charset: "utf-8", BOM: "none", LineEnding: "LF"}}}})
	if err == nil {
		t.Fatal("接受了错误换行预期")
	}
	got, _ := os.ReadFile(path)
	if string(got) != "old" {
		t.Fatal("拒绝前已写入")
	}
}

func TestExactByteUpdateRequiresBaseHash(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "file.txt")
	if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	encoded := base64.StdEncoding.EncodeToString([]byte("new"))
	_, err := ApplyBatch(BatchRequest{WorkspaceRoot: root, Edits: []FileEdit{{Path: "file.txt", Operation: OpUpdate, ContentBase64: &encoded, NewlinePolicy: "exact", ExpectedFormat: &ExpectedFormat{Charset: "utf-8", BOM: "none", LineEnding: "none"}}}})
	if err == nil {
		t.Fatal("未携带旧哈希却写入")
	}
	got, _ := os.ReadFile(path)
	if string(got) != "old" {
		t.Fatal("覆盖原内容")
	}
}
