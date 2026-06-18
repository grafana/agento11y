package transcript

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRead(t *testing.T) {
	t.Run("real fixture", func(t *testing.T) {
		// Fixtures are copied from a real ~/.vibe/logs/session run so
		// the parser is exercised against the verified shapes (user,
		// assistant tool-call, tool result, assistant text).
		path := filepath.Join("..", "testdata", "messages.jsonl")
		lines, offset, err := Read(path, 0)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if len(lines) != 10 {
			t.Fatalf("got %d lines, want 10", len(lines))
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if offset != info.Size() {
			t.Fatalf("offset %d != file size %d", offset, info.Size())
		}

		// Line 1: user prompt.
		if got, want := lines[0].Role, "user"; got != want {
			t.Errorf("line[0].Role = %q, want %q", got, want)
		}
		if lines[0].Content == "" {
			t.Errorf("line[0].Content is empty")
		}
		// Line 2: assistant tool-call, no Content, exactly one tool call.
		if got, want := lines[1].Role, "assistant"; got != want {
			t.Errorf("line[1].Role = %q, want %q", got, want)
		}
		if lines[1].Content != "" {
			t.Errorf("line[1].Content = %q, want empty (exclude_none drops it)", lines[1].Content)
		}
		if got, want := len(lines[1].ToolCalls), 1; got != want {
			t.Fatalf("line[1].ToolCalls = %d, want %d", got, want)
		}
		tc := lines[1].ToolCalls[0]
		if tc.ID == "" || tc.Function.Name == "" || tc.Function.Arguments == "" {
			t.Errorf("line[1].ToolCalls[0] missing fields: %+v", tc)
		}
		// Line 3: tool result.
		if got, want := lines[2].Role, "tool"; got != want {
			t.Errorf("line[2].Role = %q, want %q", got, want)
		}
		if lines[2].ToolCallID == "" || lines[2].Name == "" || lines[2].Content == "" {
			t.Errorf("line[2] missing tool fields: %+v", lines[2])
		}
	})

	t.Run("incremental read from offset skips earlier lines", func(t *testing.T) {
		path := filepath.Join("..", "testdata", "messages.jsonl")
		first, off1, err := Read(path, 0)
		if err != nil || len(first) == 0 {
			t.Fatalf("first read: %v lines=%d", err, len(first))
		}
		// Take the offset after the first line and re-read.
		off := first[0].EndOffset
		rest, off2, err := Read(path, off)
		if err != nil {
			t.Fatalf("second read: %v", err)
		}
		if len(rest) != len(first)-1 {
			t.Fatalf("rest len = %d, want %d", len(rest), len(first)-1)
		}
		if off2 != off1 {
			t.Errorf("final offset diverged: %d vs %d", off2, off1)
		}
	})

	t.Run("missing file errors", func(t *testing.T) {
		_, _, err := Read(filepath.Join(t.TempDir(), "missing.jsonl"), 0)
		if err == nil {
			t.Fatal("expected error for missing file")
		}
	})

	t.Run("skips unparseable lines", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "msgs.jsonl")
		body := `{"role":"user","content":"ok"}` + "\n" +
			`not json at all` + "\n" +
			`{"role":"assistant","content":"hi"}` + "\n"
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		lines, _, err := Read(p, 0)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if len(lines) != 2 {
			t.Fatalf("got %d lines, want 2 (bad line skipped)", len(lines))
		}
	})
}
