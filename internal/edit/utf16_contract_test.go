package edit

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"unicode/utf16"
)

func TestExactUTF16LineEndingContract(t *testing.T) {
	for _, charset := range []string{"utf-16le", "utf-16be"} {
		for _, fixture := range []struct{ name, text, ending string }{
			{"lf", "中文\n", "LF"}, {"crlf", "中文\r\n", "CRLF"}, {"cr", "中文\r", "CR"},
			{"mixed", "中\r\n文\n尾\r", "mixed"}, {"no-tail", "中\r\n文", "CRLF"}, {"none", "中文", "none"},
		} {
			t.Run(charset+"/"+fixture.name, func(t *testing.T) {
				var order binary.ByteOrder = binary.LittleEndian
				content := []byte{0xff, 0xfe}
				if charset == "utf-16be" {
					order = binary.BigEndian
					content = []byte{0xfe, 0xff}
				}
				for _, unit := range utf16.Encode([]rune(fixture.text)) {
					var encodedUnit [2]byte
					order.PutUint16(encodedUnit[:], unit)
					content = append(content, encodedUnit[:]...)
				}
				encoded := base64.StdEncoding.EncodeToString(content)
				root := t.TempDir()
				result, err := ApplyBatch(BatchRequest{WorkspaceRoot: root, Edits: []FileEdit{{Path: "proof.txt", Operation: OpCreate, ContentBase64: &encoded, NewlinePolicy: "exact", ExpectedFormat: &ExpectedFormat{Charset: charset, BOM: charset, LineEnding: fixture.ending}}}})
				if err != nil {
					t.Fatal(err)
				}
				got, err := os.ReadFile(filepath.Join(root, "proof.txt"))
				if err != nil || !bytes.Equal(got, content) {
					t.Fatal("UTF16 原始字节变化")
				}
				readback := result.Results[0].Readback
				if readback == nil || readback.Format.LineEnding != fixture.ending || readback.SHA256 != hashBytes(content) {
					t.Fatalf("回读合同错误: %+v", readback)
				}
			})
		}
	}
}
