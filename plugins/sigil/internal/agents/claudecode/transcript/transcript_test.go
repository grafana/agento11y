package transcript

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRead(t *testing.T) {
	tests := []struct {
		name      string
		content   string
		offset    int64
		wantTypes []string
		wantErr   bool
	}{
		{
			name: "assistant message",
			content: `{"type":"assistant","sessionId":"sess1","timestamp":"2025-01-01T00:00:00Z","message":{"model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":100,"output_tokens":50}}}
`,
			wantTypes: []string{"assistant"},
		},
		{
			name: "user message with string content",
			content: `{"type":"user","sessionId":"sess1","timestamp":"2025-01-01T00:00:00Z","message":{"role":"user","content":"what is 2+2?"}}
`,
			wantTypes: []string{"user"},
		},
		{
			name: "user message with tool results",
			content: `{"type":"user","sessionId":"sess1","timestamp":"2025-01-01T00:00:00Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_123","content":"file contents here"}]}}
`,
			wantTypes: []string{"user"},
		},
		{
			name: "skipped types are excluded",
			content: `{"type":"file-history-snapshot","sessionId":"sess1"}
{"type":"assistant","sessionId":"sess1","message":{"model":"claude-sonnet-4-20250514","content":[],"usage":{"output_tokens":10}}}
{"type":"queue-operation","sessionId":"sess1"}
{"type":"permission-mode","sessionId":"sess1"}
`,
			wantTypes: []string{"assistant"},
		},
		{
			name: "ai-title and system metadata are excluded",
			content: `{"type":"ai-title","sessionId":"sess1","title":"my chat"}
{"type":"user","sessionId":"sess1","message":{"role":"user","content":"hi"}}
{"type":"system","sessionId":"sess1","subtype":"stop_hook_summary"}
{"type":"assistant","sessionId":"sess1","message":{"model":"claude-sonnet-4-20250514","content":[],"usage":{"output_tokens":5}}}
`,
			wantTypes: []string{"user", "assistant"},
		},
		{
			name: "unparseable lines are skipped",
			content: `not json at all
{"type":"assistant","sessionId":"sess1","message":{"model":"test","content":[],"usage":{"output_tokens":5}}}
{invalid json}
`,
			wantTypes: []string{"assistant"},
		},
		{
			name: "read from byte offset",
			content: `{"type":"user","sessionId":"sess1","message":{"role":"user","content":"first"}}
{"type":"assistant","sessionId":"sess1","message":{"model":"test","content":[],"usage":{"output_tokens":5}}}
`,
			offset:    80, // past the first line
			wantTypes: []string{"assistant"},
		},
		{
			name:      "empty file",
			content:   "",
			wantTypes: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTempFile(t, tt.content)

			lines, newOffset, err := Read(path, tt.offset)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Read() error = %v, wantErr %v", err, tt.wantErr)
			}

			if len(lines) != len(tt.wantTypes) {
				t.Fatalf("got %d lines, want %d", len(lines), len(tt.wantTypes))
			}

			for i, wantType := range tt.wantTypes {
				if lines[i].Type != wantType {
					t.Errorf("line[%d].Type = %q, want %q", i, lines[i].Type, wantType)
				}
			}

			if newOffset <= tt.offset && len(tt.content) > 0 {
				t.Errorf("offset did not advance: got %d, started at %d", newOffset, tt.offset)
			}
		})
	}
}

func TestRead_ByteOffsetAccuracy(t *testing.T) {
	line1 := `{"type":"user","sessionId":"s1","message":{"role":"user","content":"hello"}}`
	line2 := `{"type":"assistant","sessionId":"s1","message":{"model":"test","content":[],"usage":{"output_tokens":1}}}`
	content := line1 + "\n" + line2 + "\n"

	path := writeTempFile(t, content)

	// First read: should get both lines
	lines1, offset1, err := Read(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines1) != 2 {
		t.Fatalf("first read: got %d lines, want 2", len(lines1))
	}

	// Second read from offset: should get nothing
	lines2, offset2, err := Read(path, offset1)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines2) != 0 {
		t.Fatalf("second read: got %d lines, want 0", len(lines2))
	}
	if offset2 != offset1 {
		t.Errorf("offset changed on empty read: %d != %d", offset2, offset1)
	}

	// Append a new line and re-read from offset
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	line3 := `{"type":"assistant","sessionId":"s1","message":{"model":"test2","content":[],"usage":{"output_tokens":2}}}`
	if _, err := f.WriteString(line3 + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close() //nolint:errcheck

	lines3, offset3, err := Read(path, offset1)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines3) != 1 {
		t.Fatalf("third read: got %d lines, want 1", len(lines3))
	}
	if offset3 <= offset1 {
		t.Errorf("offset didn't advance: %d <= %d", offset3, offset1)
	}
}

func TestRead_LargeLines(t *testing.T) {
	// Simulate a large tool result line (>64KB)
	bigContent := strings.Repeat("x", 100*1024)
	line := `{"type":"user","sessionId":"s1","message":{"role":"user","content":"` + bigContent + `"}}`
	path := writeTempFile(t, line+"\n")

	lines, _, err := Read(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}
}

func TestParseUserContent(t *testing.T) {
	tests := []struct {
		name       string
		raw        json.RawMessage
		wantText   string
		wantBlocks int
	}{
		{
			name:     "string content",
			raw:      json.RawMessage(`"hello world"`),
			wantText: "hello world",
		},
		{
			name:       "array content",
			raw:        json.RawMessage(`[{"type":"tool_result","tool_use_id":"tu_1","content":"ok"},{"type":"text","text":"follow-up"}]`),
			wantBlocks: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text, blocks, err := ParseUserContent(tt.raw)
			if err != nil {
				t.Fatal(err)
			}
			if text != tt.wantText {
				t.Errorf("text = %q, want %q", text, tt.wantText)
			}
			if len(blocks) != tt.wantBlocks {
				t.Errorf("got %d blocks, want %d", len(blocks), tt.wantBlocks)
			}
		})
	}
}

func TestUserContentBlock_Content(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "string content",
			raw:  `{"type":"tool_result","tool_use_id":"tu_1","content":"plain text"}`,
			want: "plain text",
		},
		{
			name: "array content with text blocks",
			raw:  `{"type":"tool_result","tool_use_id":"tu_1","content":[{"type":"text","text":"line one"},{"type":"text","text":"line two"}]}`,
			want: "line one\nline two",
		},
		{
			name: "empty content",
			raw:  `{"type":"tool_result","tool_use_id":"tu_1"}`,
			want: "",
		},
		{
			name: "null content",
			raw:  `{"type":"tool_result","tool_use_id":"tu_1","content":null}`,
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var block UserContentBlock
			if err := json.Unmarshal([]byte(tt.raw), &block); err != nil {
				t.Fatalf("unmarshal failed: %v", err)
			}
			got := block.Content()
			if got != tt.want {
				t.Errorf("Content() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRead_EndOffset(t *testing.T) {
	content := `{"type":"user","sessionId":"s1","message":{"role":"user","content":"hello"}}
{"type":"assistant","sessionId":"s1","message":{"model":"test","content":[],"usage":{"output_tokens":1}}}
`
	path := writeTempFile(t, content)

	lines, _, err := Read(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
	// Each line should have a non-zero EndOffset
	for i, line := range lines {
		if line.EndOffset <= 0 {
			t.Errorf("line[%d].EndOffset = %d, want > 0", i, line.EndOffset)
		}
	}
	// Second line's EndOffset should be greater than first
	if lines[1].EndOffset <= lines[0].EndOffset {
		t.Errorf("line[1].EndOffset (%d) should be > line[0].EndOffset (%d)", lines[1].EndOffset, lines[0].EndOffset)
	}
}

func TestRead_EndOffset_CRLF(t *testing.T) {
	line1 := `{"type":"user","sessionId":"s1","message":{"role":"user","content":"hello"}}`
	line2 := `{"type":"assistant","sessionId":"s1","message":{"model":"test","content":[],"usage":{"output_tokens":1}}}`
	content := line1 + "\r\n" + line2 + "\r\n"
	path := writeTempFile(t, content)

	lines, finalOffset, err := Read(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}

	// With \r\n each line ending is 2 bytes
	wantFirst := int64(len(line1) + 2)
	if lines[0].EndOffset != wantFirst {
		t.Errorf("line[0].EndOffset = %d, want %d", lines[0].EndOffset, wantFirst)
	}
	wantSecond := int64(len(line1) + 2 + len(line2) + 2)
	if lines[1].EndOffset != wantSecond {
		t.Errorf("line[1].EndOffset = %d, want %d", lines[1].EndOffset, wantSecond)
	}
	if finalOffset != int64(len(content)) {
		t.Errorf("finalOffset = %d, want %d", finalOffset, len(content))
	}

	// Re-read from first line's EndOffset should yield only the second line
	lines2, _, err := Read(path, lines[0].EndOffset)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines2) != 1 {
		t.Fatalf("re-read got %d lines, want 1", len(lines2))
	}
	if lines2[0].Type != "assistant" {
		t.Errorf("re-read line type = %q, want assistant", lines2[0].Type)
	}
}

func TestRead_FileNotFound(t *testing.T) {
	_, _, err := Read("/nonexistent/path.jsonl", 0)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func writeTempFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
