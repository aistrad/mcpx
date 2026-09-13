package file

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// MaxSourceBytes is the maximum source payload that an explicit full read may
// materialize. Window reads are streaming and may inspect a larger source file
// while returning only the requested bounded text.
const MaxSourceBytes int64 = 4 << 20

// SizeLimitError is returned only when an operation explicitly asks to
// materialize a source larger than its advertised capacity.
type SizeLimitError struct {
	Path   string
	Actual int64
	Max    int64
}

func (e *SizeLimitError) Error() string {
	if e == nil {
		return "source file exceeds the maximum size"
	}
	return fmt.Sprintf("source file %q is %d bytes; maximum is %d bytes", e.Path, e.Actual, e.Max)
}

// ReadOptions controls file.read.
type ReadOptions struct {
	WorkspaceRoot string
	Path          string // relative
	Offset        int    // 0-based line start
	Limit         int    // max lines; 0 => default
	MaxBytes      int64
}

// LineEndingCounts records line terminators in the original bytes.
type LineEndingCounts struct {
	LF   int `json:"lf"`
	CRLF int `json:"crlf"`
	CR   int `json:"cr"`
}

// Format describes the source bytes that a client may edit. It is separate
// from the transport encoding used by the MCP response.
type Format struct {
	Charset          string           `json:"charset"`
	BOM              string           `json:"bom"`
	LineEnding       string           `json:"line_ending"`
	LineEndingCounts LineEndingCounts `json:"line_ending_counts"`
	FinalNewline     *bool            `json:"final_newline"`
}

// ReadResult is file.read data.
type ReadResult struct {
	Path       string `json:"path"`
	Content    string `json:"content"`
	TotalLines int    `json:"total_lines"`
	Offset     int    `json:"offset"`
	Limit      int    `json:"limit"`
	Truncated  bool   `json:"truncated"`
	LineEnding string `json:"line_ending,omitempty"`
	Format     Format `json:"format"`
	SHA256     string `json:"-"`
}

// FullReadOptions controls an explicit whole-file read. Unlike ReadOptions,
// FullReadOptions permits binary content but always enforces MaxBytes.
type FullReadOptions struct {
	WorkspaceRoot string
	Path          string
	MaxBytes      int64
}

// FullReadResult contains the complete file bytes and their display metadata.
// Content is intentionally excluded from JSON serialization so callers choose
// the appropriate MCP content type instead of duplicating large payloads.
type FullReadResult struct {
	Path       string `json:"path"`
	Content    []byte `json:"-"`
	Size       int64  `json:"size_bytes"`
	MIMEType   string `json:"mime_type"`
	LineEnding string `json:"line_ending,omitempty"`
	Format     Format `json:"format"`
	SHA256     string `json:"sha256"`
}

// Read loads a bounded text window while hashing the same stream. It keeps
// only the requested lines instead of materializing the complete file.
func Read(opts ReadOptions) (ReadResult, error) {
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = 1 << 20
	}
	if opts.Limit <= 0 {
		opts.Limit = 500
	}
	if opts.Offset < 0 {
		opts.Offset = 0
	}
	if _, err := Resolve(opts.WorkspaceRoot, opts.Path); err != nil {
		return ReadResult{}, err
	}
	root, err := os.OpenRoot(opts.WorkspaceRoot)
	if err != nil {
		return ReadResult{}, err
	}
	defer root.Close()
	st, err := root.Stat(opts.Path)
	if err != nil {
		return ReadResult{}, err
	}
	if st.IsDir() {
		return ReadResult{}, fmt.Errorf("is a directory")
	}
	f, err := root.Open(opts.Path)
	if err != nil {
		return ReadResult{}, err
	}
	defer f.Close()
	endsWithNewline := false
	prefix := make([]byte, 4)
	prefixSize := 0
	if st.Size() > 0 {
		prefixSize, _ = f.ReadAt(prefix, 0)
	}
	prefixFormat := detectFormat(prefix[:prefixSize])
	if prefixFormat.Charset == "utf-16le" || prefixFormat.Charset == "utf-16be" {
		return readUTF16Window(f, opts, st.Size())
	}
	if st.Size() > 0 {
		var last [1]byte
		if _, err := f.ReadAt(last[:], st.Size()-1); err == nil {
			endsWithNewline = last[0] == '\n' || last[0] == '\r'
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return ReadResult{}, err
		}
	}

	hasher := sha256.New()
	reader := bufio.NewReaderSize(f, 64*1024)
	selected := make([]string, 0, opts.Limit)
	crlfCount, lfCount, crCount := 0, 0, 0
	lineNumber := 0
	for {
		raw, readErr := reader.ReadBytes('\n')
		if len(raw) > 0 {
			_, _ = hasher.Write(raw)
			observeLineEndings(raw, &crlfCount, &lfCount, &crCount)
			line := raw
			if line[len(line)-1] == '\n' {
				line = line[:len(line)-1]
			}
			if lineNumber == 0 && bytes.HasPrefix(prefix[:prefixSize], []byte{0xef, 0xbb, 0xbf}) {
				line = bytes.TrimPrefix(line, []byte{0xef, 0xbb, 0xbf})
			}
			if !utf8.Valid(line) {
				return ReadResult{}, fmt.Errorf("binary or non-utf8 content")
			}
			if lineNumber >= opts.Offset && lineNumber < opts.Offset+opts.Limit {
				selected = append(selected, string(line))
			}
			lineNumber++
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return ReadResult{}, readErr
		}
	}

	start := opts.Offset
	if start > lineNumber {
		start = lineNumber
	}
	chunk := strings.Join(selected, "\n")
	if len(selected) > 0 && (endsWithNewline || start+len(selected) < lineNumber) {
		chunk += "\n"
	}
	truncated := start+len(selected) < lineNumber
	if int64(len(chunk)) > opts.MaxBytes {
		chunk = truncateUTF8(chunk, int(opts.MaxBytes))
		truncated = true
	}
	digest := hasher.Sum(nil)
	format := formatFromReadStats(prefix[:prefixSize], crlfCount, lfCount, crCount, endsWithNewline)
	return ReadResult{
		Path:       opts.Path,
		Content:    chunk,
		TotalLines: lineNumber,
		Offset:     start,
		Limit:      opts.Limit,
		Truncated:  truncated,
		LineEnding: format.LineEnding,
		Format:     format,
		SHA256:     "sha256:" + hex.EncodeToString(digest),
	}, nil
}

// ReadFull reads a complete regular file for an explicit client-preview
// request. It preserves binary bytes and rejects files larger than MaxBytes.
func ReadFull(opts FullReadOptions) (FullReadResult, error) {
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = MaxSourceBytes
	}
	if _, err := Resolve(opts.WorkspaceRoot, opts.Path); err != nil {
		return FullReadResult{}, err
	}
	root, err := os.OpenRoot(opts.WorkspaceRoot)
	if err != nil {
		return FullReadResult{}, err
	}
	defer root.Close()
	info, err := root.Stat(opts.Path)
	if err != nil {
		return FullReadResult{}, err
	}
	if info.IsDir() {
		return FullReadResult{}, fmt.Errorf("is a directory")
	}
	if info.Size() > opts.MaxBytes {
		return FullReadResult{}, &SizeLimitError{Path: opts.Path, Actual: info.Size(), Max: opts.MaxBytes}
	}

	handle, err := root.Open(opts.Path)
	if err != nil {
		return FullReadResult{}, err
	}
	defer handle.Close()
	content, err := io.ReadAll(io.LimitReader(handle, opts.MaxBytes+1))
	if err != nil {
		return FullReadResult{}, err
	}
	if int64(len(content)) > opts.MaxBytes {
		return FullReadResult{}, &SizeLimitError{Path: opts.Path, Actual: int64(len(content)), Max: opts.MaxBytes}
	}
	digest := sha256.Sum256(content)
	format := detectFormat(content)
	if _, decodedFormat, decodeErr := DecodeText(content); decodeErr == nil {
		format = decodedFormat
	}
	return FullReadResult{
		Path: opts.Path, Content: content, Size: int64(len(content)),
		MIMEType: detectMIME(opts.Path, content), LineEnding: format.LineEnding, Format: format,
		SHA256: "sha256:" + hex.EncodeToString(digest[:]),
	}, nil
}

func detectFormat(content []byte) Format {
	charset, bom := detectCharset(content)
	format := Format{Charset: charset, BOM: bom, LineEnding: "none"}
	if charset != "utf-8" {
		return format
	}
	crlfCount, lfCount, crCount := 0, 0, 0
	observeLineEndings(content, &crlfCount, &lfCount, &crCount)
	format.LineEndingCounts = LineEndingCounts{LF: lfCount, CRLF: crlfCount, CR: crCount}
	format.LineEnding = lineEndingName(crlfCount, lfCount, crCount)
	format.FinalNewline = boolPointer(hasFinalLineEnding(content))
	return format
}

// DetectFormat classifies source bytes without reading from the workspace.
// Edit validation uses the same classifier as reads so clients see one
// consistent charset and line-ending contract across read and edit tools.
func DetectFormat(content []byte) Format {
	// 使用读取路径的解码分类识别 UTF-16 换行，摘要仍由原始字节计算。
	if _, format, err := DecodeText(content); err == nil {
		return format
	}
	return detectFormat(content)
}

// DecodeText decodes a supported source file into model-facing Unicode text
// while preserving the original charset/BOM and decoded line-ending metadata.
// The returned error means the bytes must stay binary/base64 to avoid lossy
// edits. SHA values must still be calculated from the original bytes.
func DecodeText(content []byte) (string, Format, error) {
	format := detectFormat(content)
	switch format.Charset {
	case "utf-8":
		start := 0
		if format.BOM == "utf-8" {
			start = 3
		}
		if start > len(content) || !utf8.Valid(content[start:]) {
			return "", format, fmt.Errorf("invalid utf-8 content")
		}
		text := string(content[start:])
		return text, applyDecodedFormat(format, text), nil
	case "utf-16le", "utf-16be":
		if len(content) < 2 || len(content[2:])%2 != 0 {
			return "", format, fmt.Errorf("invalid utf-16 byte length")
		}
		var order binary.ByteOrder = binary.LittleEndian
		if format.Charset == "utf-16be" {
			order = binary.BigEndian
		}
		units := make([]uint16, len(content[2:])/2)
		for index := range units {
			units[index] = order.Uint16(content[2+index*2:])
		}
		for index, unit := range units {
			switch {
			case unit >= 0xd800 && unit <= 0xdbff:
				if index+1 >= len(units) || units[index+1] < 0xdc00 || units[index+1] > 0xdfff {
					return "", format, fmt.Errorf("invalid utf-16 surrogate pair")
				}
			case unit >= 0xdc00 && unit <= 0xdfff:
				if index == 0 || units[index-1] < 0xd800 || units[index-1] > 0xdbff {
					return "", format, fmt.Errorf("invalid utf-16 surrogate pair")
				}
			}
		}
		text := string(utf16.Decode(units))
		return text, applyDecodedFormat(format, text), nil
	default:
		return "", format, fmt.Errorf("unsupported text charset %q", format.Charset)
	}
}

func applyDecodedFormat(format Format, text string) Format {
	logical := detectFormat([]byte(text))
	format.LineEnding = logical.LineEnding
	format.LineEndingCounts = logical.LineEndingCounts
	format.FinalNewline = logical.FinalNewline
	return format
}

func readUTF16Window(handle *os.File, opts ReadOptions, size int64) (ReadResult, error) {
	if size < 2 || size%2 != 0 {
		return ReadResult{}, fmt.Errorf("unsupported utf-16 content: invalid byte length")
	}
	if _, err := handle.Seek(0, io.SeekStart); err != nil {
		return ReadResult{}, err
	}
	reader := bufio.NewReaderSize(handle, 64*1024)
	hasher := sha256.New()
	var bom [2]byte
	if _, err := io.ReadFull(reader, bom[:]); err != nil {
		return ReadResult{}, err
	}
	_, _ = hasher.Write(bom[:])
	format := Format{Charset: "utf-16le", BOM: "utf-16le", LineEnding: "none"}
	if bom[0] == 0xfe && bom[1] == 0xff {
		format.Charset, format.BOM = "utf-16be", "utf-16be"
	}
	order := binary.ByteOrder(binary.LittleEndian)
	if format.Charset == "utf-16be" {
		order = binary.BigEndian
	}

	start := opts.Offset
	if start < 0 {
		start = 0
	}
	selected := make([]string, 0, opts.Limit)
	var line strings.Builder
	lineNumber := 0
	lineEnded := false
	pendingCR := false
	crlfCount, lfCount, crCount := 0, 0, 0
	finalNewline := false
	emit := func(ending string) {
		if lineNumber >= start && lineNumber < start+opts.Limit {
			value := line.String()
			if int64(len(value)) > opts.MaxBytes {
				value = truncateUTF8(value, int(opts.MaxBytes))
			}
			selected = append(selected, value)
		}
		line.Reset()
		lineNumber++
		lineEnded = true
		finalNewline = true
		switch ending {
		case "CRLF":
			crlfCount++
		case "CR":
			crCount++
		default:
			lfCount++
		}
	}
	appendRune := func(r rune) {
		if lineNumber < start || lineNumber >= start+opts.Limit || int64(line.Len()) >= opts.MaxBytes {
			return
		}
		line.WriteRune(r)
	}
	for {
		var raw [2]byte
		if _, err := io.ReadFull(reader, raw[:]); err != nil {
			if err != io.EOF && err != io.ErrUnexpectedEOF {
				return ReadResult{}, err
			}
			break
		}
		_, _ = hasher.Write(raw[:])
		unit := order.Uint16(raw[:])
		var runeValue rune
		switch {
		case unit >= 0xd800 && unit <= 0xdbff:
			var next [2]byte
			if _, err := io.ReadFull(reader, next[:]); err != nil {
				return ReadResult{}, fmt.Errorf("unsupported utf-16 content: invalid surrogate pair")
			}
			_, _ = hasher.Write(next[:])
			nextUnit := order.Uint16(next[:])
			if nextUnit < 0xdc00 || nextUnit > 0xdfff {
				return ReadResult{}, fmt.Errorf("unsupported utf-16 content: invalid surrogate pair")
			}
			runeValue = utf16.Decode([]uint16{unit, nextUnit})[0]
		case unit >= 0xdc00 && unit <= 0xdfff:
			return ReadResult{}, fmt.Errorf("unsupported utf-16 content: invalid surrogate pair")
		default:
			runeValue = rune(unit)
		}
		if pendingCR {
			if runeValue == '\n' {
				emit("CRLF")
				pendingCR = false
				continue
			}
			emit("CR")
			pendingCR = false
		}
		switch runeValue {
		case '\r':
			pendingCR = true
		case '\n':
			emit("LF")
		default:
			lineEnded = false
			finalNewline = false
			appendRune(runeValue)
		}
	}
	if pendingCR {
		emit("CR")
	} else if !lineEnded {
		if line.Len() > 0 || lineNumber == 0 {
			if lineNumber >= start && lineNumber < start+opts.Limit {
				value := line.String()
				if int64(len(value)) > opts.MaxBytes {
					value = truncateUTF8(value, int(opts.MaxBytes))
				}
				selected = append(selected, value)
			}
			lineNumber++
		}
	}
	if lineNumber == 0 && size == 2 {
		lineNumber = 0
	}
	chunk := strings.Join(selected, "\n")
	if len(selected) > 0 && (finalNewline || start+len(selected) < lineNumber) {
		chunk += "\n"
	}
	truncated := start+len(selected) < lineNumber
	if int64(len(chunk)) > opts.MaxBytes {
		chunk = truncateUTF8(chunk, int(opts.MaxBytes))
		truncated = true
	}
	format.LineEnding = lineEndingName(crlfCount, lfCount, crCount)
	format.LineEndingCounts = LineEndingCounts{LF: lfCount, CRLF: crlfCount, CR: crCount}
	format.FinalNewline = boolPointer(finalNewline)
	digest := hasher.Sum(nil)
	return ReadResult{
		Path: opts.Path, Content: chunk, TotalLines: lineNumber, Offset: minInt(start, lineNumber), Limit: opts.Limit,
		Truncated: truncated, LineEnding: format.LineEnding, Format: format,
		SHA256: "sha256:" + hex.EncodeToString(digest),
	}, nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func formatFromReadStats(prefix []byte, crlfCount, lfCount, crCount int, finalNewline bool) Format {
	charset, bom := detectCharset(prefix)
	if charset == "unknown" {
		// Read validates every returned line as UTF-8, so a non-empty successful
		// window is still editable UTF-8 even when the prefix is shorter than a
		// complete BOM sequence.
		charset = "utf-8"
	}
	return Format{
		Charset: charset, BOM: bom,
		LineEnding:       lineEndingName(crlfCount, lfCount, crCount),
		LineEndingCounts: LineEndingCounts{LF: lfCount, CRLF: crlfCount, CR: crCount},
		FinalNewline:     boolPointer(finalNewline),
	}
}

func detectCharset(content []byte) (charset, bom string) {
	switch {
	case bytes.HasPrefix(content, []byte{0xef, 0xbb, 0xbf}):
		return "utf-8", "utf-8"
	case bytes.HasPrefix(content, []byte{0xff, 0xfe}):
		return "utf-16le", "utf-16le"
	case bytes.HasPrefix(content, []byte{0xfe, 0xff}):
		return "utf-16be", "utf-16be"
	case utf8.Valid(content):
		return "utf-8", "none"
	default:
		return "unknown", "unknown"
	}
}

func boolPointer(value bool) *bool { return &value }

func hasFinalLineEnding(content []byte) bool {
	if len(content) == 0 {
		return false
	}
	last := content[len(content)-1]
	return last == '\n' || last == '\r'
}

func detectLineEnding(content []byte) string {
	crlfCount, lfCount, crCount := 0, 0, 0
	observeLineEndings(content, &crlfCount, &lfCount, &crCount)
	return lineEndingName(crlfCount, lfCount, crCount)
}

func observeLineEndings(content []byte, crlfCount, lfCount, crCount *int) {
	for index := 0; index < len(content); index++ {
		switch content[index] {
		case '\r':
			if index+1 < len(content) && content[index+1] == '\n' {
				*crlfCount++
				index++
			} else {
				*crCount++
			}
		case '\n':
			*lfCount++
		}
	}
}

func lineEndingName(crlfCount, lfCount, crCount int) string {
	kinds := 0
	if crlfCount > 0 {
		kinds++
	}
	if lfCount > 0 {
		kinds++
	}
	if crCount > 0 {
		kinds++
	}
	if kinds == 0 {
		return "none"
	}
	if kinds > 1 {
		return "mixed"
	}
	if crlfCount > 0 {
		return "CRLF"
	}
	if lfCount > 0 {
		return "LF"
	}
	return "CR"
}

// DetectMIME returns a source-friendly MIME type for path/content.
func DetectMIME(path string, content []byte) string { return detectMIME(path, content) }

func detectMIME(path string, content []byte) string {
	extension := strings.ToLower(filepath.Ext(path))
	if detected, ok := sourceMIMEByExtension[extension]; ok {
		return detected
	}
	if detected := mime.TypeByExtension(extension); detected != "" {
		return normalizeMIME(detected)
	}
	return normalizeMIME(http.DetectContentType(content))
}

// sourceMIMEByExtension overrides platform MIME tables for source formats
// whose extensions are also used by binary containers. macOS commonly maps
// .ts to video/mp2t, which makes a TypeScript file look like binary content to
// an MCP client and causes an unnecessary base64/encoding recovery round trip.
var sourceMIMEByExtension = map[string]string{
	".ts":     "text/typescript",
	".tsx":    "text/typescript",
	".mts":    "text/typescript",
	".cts":    "text/typescript",
	".vue":    "text/plain",
	".svelte": "text/plain",
}

func normalizeMIME(detected string) string {
	if separator := strings.IndexByte(detected, ';'); separator >= 0 {
		detected = detected[:separator]
	}
	return strings.TrimSpace(detected)
}

func truncateUTF8(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	for maxBytes > 0 && !utf8.RuneStart(value[maxBytes]) {
		maxBytes--
	}
	return value[:maxBytes]
}
