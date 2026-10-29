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
			name: "only user and assistant lines are kept",
			content: `{"type":"user","sessionId":"sess1","message":{"role":"user","content":"hi"}}
{"type":"atis-latch","sessionId":"sess1","atis":"x"}
{"type":"assistant","sessionId":"sess1","message":{"model":"claude-sonnet-4-20250514","content":[],"usage":{"output_tokens":5}}}
{"type":"mode","sessionId":"sess1","mode":"x"}
{"type":"custom-title","sessionId":"sess1","customTitle":"my chat"}
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

// TestRead_SubagentAttribution pins the fields a sidechain line carries. The
// importer maps a subagent transcript back to the Agent tool call that spawned
// it through AgentID, and the mapper names the turn from AttributionAgent, so a
// decoding regression here silently flattens every subagent turn into the main
// thread.
func TestRead_SubagentAttribution(t *testing.T) {
	content := `{"type":"assistant","sessionId":"s1","isSidechain":true,"agentId":"a18f9a9d9f1f3d28e","attributionAgent":"general-purpose","message":{"model":"test","content":[{"type":"thinking","thinking":"weighing the options"},{"type":"text","text":"done"}],"usage":{"output_tokens":1}}}
`
	lines, _, err := Read(writeTempFile(t, content), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}
	line := lines[0]
	if !line.IsSidechain {
		t.Error("IsSidechain = false, want true")
	}
	if line.AgentID != "a18f9a9d9f1f3d28e" {
		t.Errorf("AgentID = %q, want a18f9a9d9f1f3d28e", line.AgentID)
	}
	if line.AttributionAgent != "general-purpose" {
		t.Errorf("AttributionAgent = %q, want general-purpose", line.AttributionAgent)
	}

	var msg AssistantMessage
	if err := json.Unmarshal(line.Message, &msg); err != nil {
		t.Fatal(err)
	}
	if len(msg.Content) != 2 {
		t.Fatalf("got %d content blocks, want 2", len(msg.Content))
	}
	if msg.Content[0].Thinking != "weighing the options" {
		t.Errorf("Thinking = %q, want \"weighing the options\"", msg.Content[0].Thinking)
	}
	if msg.Content[0].Text != "" {
		t.Errorf("Text = %q, want empty: a thinking block writes its own key", msg.Content[0].Text)
	}
}

func TestRead_FileNotFound(t *testing.T) {
	_, _, err := Read("/nonexistent/path.jsonl", 0)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestReadWithTitles(t *testing.T) {
	// Title lines in the shape Claude Code 2.1 writes them. It writes an
	// ai-title before the first answer and again as the session goes on, and a
	// custom-title when the user renames the session.
	content := `{"type":"ai-title","aiTitle":"Fix flaky test","sessionId":"sess1"}
{"type":"user","sessionId":"sess1","message":{"role":"user","content":"the login test fails"}}
{"type":"ai-title","aiTitle":"Flaky login test","sessionId":"sess1"}
{"type":"assistant","sessionId":"sess1","message":{"model":"test","content":[],"stop_reason":"end_turn","usage":{"output_tokens":5}}}
{"type":"custom-title","sessionId":"sess1","customTitle":"login flake"}
{"type":"ai-title","aiTitle":"  ","sessionId":"sess1"}
`
	path := writeTempFile(t, content)

	lines, _, titles, err := ReadWithTitles(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := Titles{AI: "Flaky login test", Custom: "login flake"}
	if titles != want {
		t.Errorf("titles = %+v, want %+v", titles, want)
	}
	// Title lines stay out of the lines, so one never sits at the tail of a
	// read where the Stop settle read would wait for it.
	if len(lines) != 2 || lines[0].Type != "user" || lines[1].Type != "assistant" {
		t.Errorf("got %d lines, want the user and assistant lines only", len(lines))
	}
}

func TestReadCompleteRecords(t *testing.T) {
	prev := maxLineBytes
	maxLineBytes = 512
	defer func() { maxLineBytes = prev }()

	user := `{"type":"user","uuid":"u","message":{"content":"héllo"}}`
	assistant := `{"type":"assistant","uuid":"a","message":{}}`
	aiTitle := `{"type":"ai-title","aiTitle":"  first title  "}`
	customTitle := `{"type":"custom-title","customTitle":" renamed "}`
	oversized := `{"type":"user","uuid":"large","message":{"content":"` + strings.Repeat("x", 128*1024) + `"}}`
	tests := []struct {
		name         string
		prefix       string
		tail         string
		offset       int64
		wantUUIDs    string
		historyUUIDs string
		titles       Titles
		historyTitle Titles
	}{
		{name: "empty"},
		{name: "complete LF", prefix: user + "\n" + assistant + "\n", wantUUIDs: "ua", historyUUIDs: "ua"},
		{name: "complete CRLF", prefix: user + "\r\n" + assistant + "\r\n", wantUUIDs: "ua", historyUUIDs: "ua"},
		{name: "valid assistant tail", prefix: user + "\n", tail: assistant, wantUUIDs: "u", historyUUIDs: "ua"},
		{name: "valid user tail", tail: user, historyUUIDs: "u"},
		{name: "incomplete JSON tail", prefix: user + "\n", tail: `{"type":"assistant"`, wantUUIDs: "u", historyUUIDs: "u"},
		{name: "invalid complete record and tail", prefix: "not json\n" + user + "\n", tail: "still not json", wantUUIDs: "u", historyUUIDs: "u"},
		{name: "oversized tail", prefix: user + "\n", tail: oversized, wantUUIDs: "u", historyUUIDs: "u"},
		{name: "oversized complete record", prefix: oversized + "\n" + assistant + "\n", wantUUIDs: "a", historyUUIDs: "a"},
		{name: "only oversized tail", tail: oversized},
		{name: "CR before missing LF", prefix: user + "\r\n", tail: assistant + "\r", wantUUIDs: "u", historyUUIDs: "ua"},
		{
			name: "title tail", prefix: aiTitle + "\n" + user + "\n", tail: customTitle,
			wantUUIDs: "u", historyUUIDs: "u", titles: Titles{AI: "first title"},
			historyTitle: Titles{AI: "first title", Custom: "renamed"},
		},
		{
			name: "complete titles and metadata", prefix: aiTitle + "\r\n" + user + "\r\n" + customTitle + "\r\n" + `{"type":"system"}` + "\r\n",
			wantUUIDs: "u", historyUUIDs: "u", titles: Titles{AI: "first title", Custom: "renamed"},
			historyTitle: Titles{AI: "first title", Custom: "renamed"},
		},
		{
			name: "nonzero cursor", prefix: aiTitle + "\r\n" + user + "\r\n", tail: assistant,
			offset: int64(len(aiTitle) + 2), wantUUIDs: "u", historyUUIDs: "ua",
		},
		{name: "cursor at incomplete record", prefix: user + "\n", tail: assistant, offset: int64(len(user) + 1), historyUUIDs: "a"},
		{name: "cursor past EOF", offset: 99},
	}
	readers := []struct {
		name     string
		complete bool
		titles   bool
		read     func(string, int64) ([]Line, int64, Titles, error)
	}{
		{name: "Read", read: func(path string, offset int64) ([]Line, int64, Titles, error) {
			lines, pos, err := Read(path, offset)
			return lines, pos, Titles{}, err
		}},
		{name: "ReadWithTitles", titles: true, read: ReadWithTitles},
		{name: "ReadComplete", complete: true, read: func(path string, offset int64) ([]Line, int64, Titles, error) {
			lines, pos, err := ReadComplete(path, offset)
			return lines, pos, Titles{}, err
		}},
		{name: "ReadCompleteWithTitles", complete: true, titles: true, read: ReadCompleteWithTitles},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content := tt.prefix + tt.tail
			path := writeTempFile(t, content)
			for _, reader := range readers {
				t.Run(reader.name, func(t *testing.T) {
					lines, pos, titles, err := reader.read(path, tt.offset)
					if err != nil {
						t.Fatal(err)
					}
					wantPos, wantUUIDs, wantTitles := int64(len(tt.prefix)), tt.wantUUIDs, tt.titles
					if !reader.complete {
						wantPos, wantUUIDs, wantTitles = int64(len(content)), tt.historyUUIDs, tt.historyTitle
					}
					if wantPos < tt.offset {
						wantPos = tt.offset
					}
					if !reader.titles {
						wantTitles = Titles{}
					}
					if pos != wantPos || titles != wantTitles {
						t.Errorf("cursor/titles = %d/%+v, want %d/%+v", pos, titles, wantPos, wantTitles)
					}
					var uuids string
					for _, line := range lines {
						uuids += line.UUID
						record := user
						if line.UUID == "a" {
							record = assistant
						}
						start := int64(strings.Index(content, record))
						end := start + int64(len(record))
						if strings.HasPrefix(content[end:], "\r") {
							end++
						}
						if strings.HasPrefix(content[end:], "\n") {
							end++
						}
						if line.StartOffset != start || line.EndOffset != end {
							t.Errorf("%s range = [%d,%d), want [%d,%d)", line.UUID, line.StartOffset, line.EndOffset, start, end)
						}
					}
					if uuids != wantUUIDs {
						t.Errorf("UUIDs = %q, want %q", uuids, wantUUIDs)
					}
				})
			}
		})
	}
}

func TestReadCompleteResumesTail(t *testing.T) {
	prev := maxLineBytes
	maxLineBytes = 512
	defer func() { maxLineBytes = prev }()
	prefix := `{"type":"user","uuid":"before"}` + "\n"
	next := `{"type":"assistant","uuid":"after"}` + "\n"
	tests := []struct {
		name   string
		tail   string
		suffix string
		uuids  string
		titles Titles
	}{
		{name: "valid", tail: `{"type":"assistant","uuid":"tail"}`, suffix: "\n", uuids: "tailafter"},
		{name: "partial JSON", tail: `{"type":"assistant","uuid":"tail"`, suffix: "}\n", uuids: "tailafter"},
		{name: "CRLF split", tail: `{"type":"assistant","uuid":"tail"}` + "\r", suffix: "\n", uuids: "tailafter"},
		{name: "oversized", tail: strings.Repeat("x", 128*1024), suffix: "\n", uuids: "after"},
		{name: "invalid", tail: "not json", suffix: "\n", uuids: "after"},
		{name: "title", tail: `{"type":"custom-title","customTitle":"tail title"}`, suffix: "\n", uuids: "after", titles: Titles{Custom: "tail title"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTempFile(t, prefix+tt.tail)
			lines, pos, titles, err := ReadCompleteWithTitles(path, 0)
			if err != nil || pos != int64(len(prefix)) || len(lines) != 1 || titles != (Titles{}) {
				t.Fatalf("initial read = %d lines, cursor %d, titles %+v, error %v", len(lines), pos, titles, err)
			}
			for range 2 {
				lines, again, titles, err := ReadCompleteWithTitles(path, pos)
				if err != nil || again != pos || len(lines) != 0 || titles != (Titles{}) {
					t.Fatalf("repeated read = %d lines, cursor %d, titles %+v, error %v", len(lines), again, titles, err)
				}
			}
			content := prefix + tt.tail + tt.suffix + next
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			lines, end, titles, err := ReadCompleteWithTitles(path, pos)
			if err != nil || end != int64(len(content)) || titles != tt.titles {
				t.Fatalf("resumed read cursor/titles/error = %d/%+v/%v", end, titles, err)
			}
			var uuids string
			for _, line := range lines {
				uuids += line.UUID
			}
			if uuids != tt.uuids {
				t.Errorf("resumed UUIDs = %q, want %q", uuids, tt.uuids)
			}
		})
	}
}

func TestLineOffsetsNotSerialized(t *testing.T) {
	line := Line{StartOffset: 10, EndOffset: 20}
	raw, err := json.Marshal(line)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(raw)), "offset") {
		t.Fatalf("offsets serialized: %s", raw)
	}
	if err := json.Unmarshal([]byte(`{"StartOffset":99,"EndOffset":100}`), &line); err != nil {
		t.Fatal(err)
	}
	if line.StartOffset != 10 || line.EndOffset != 20 {
		t.Fatalf("JSON changed offsets: %+v", line)
	}
}

func TestReadCompleteMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.jsonl")
	lines, pos, titles, err := ReadCompleteWithTitles(path, 42)
	if err == nil || len(lines) != 0 || pos != 42 || titles != (Titles{}) {
		t.Fatalf("missing file = %v, %d, %+v, %v", lines, pos, titles, err)
	}
	_, pos, err = ReadComplete(path, 42)
	if err == nil || pos != 42 {
		t.Fatalf("missing file cursor/error = %d/%v", pos, err)
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

func TestReadSkipsOverlongLineAndKeepsReading(t *testing.T) {
	prev := maxLineBytes
	maxLineBytes = 1024
	defer func() { maxLineBytes = prev }()

	content := `{"type":"user","sessionId":"s","message":{"role":"user","content":"before"}}` + "\n" +
		`{"type":"user","sessionId":"s","message":{"role":"user","content":"` + strings.Repeat("x", 4096) + `"}}` + "\n" +
		`{"type":"assistant","sessionId":"s","message":{"model":"m","content":[],"usage":{"output_tokens":1}}}` + "\n"
	path := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	lines, pos, err := Read(path, 0)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(lines) != 2 || lines[0].Type != "user" || lines[1].Type != "assistant" {
		t.Fatalf("lines after the oversized one were lost: %+v", lines)
	}
	if pos != int64(len(content)) {
		t.Fatalf("offset %d, want %d (the oversized line must still advance the offset)", pos, len(content))
	}
	if lines[1].EndOffset != int64(len(content)) {
		t.Fatalf("EndOffset %d, want %d", lines[1].EndOffset, len(content))
	}
}
