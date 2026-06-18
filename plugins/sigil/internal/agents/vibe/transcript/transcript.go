// Package transcript reads vibe session JSONL transcripts incrementally
// from a byte offset, returning the new lines plus the new end offset.
//
// Vibe writes one JSON object per line to <session_dir>/messages.jsonl with
// these shapes (verified against ~/.vibe/logs/session/):
//
//   - user:           {role, content (str), injected, message_id}
//   - assistant tool: {role, reasoning_content, reasoning_message_id,
//     tool_calls:[{id, index, function:{name, arguments}, type}],
//     message_id} — content is dropped by exclude_none
//   - tool result:    {role, content (str), name, tool_call_id}
//   - assistant text: {role, content (str), reasoning_content?, message_id}
//
// tool_calls[].function.arguments is a JSON-encoded string, not an object.
// Its raw bytes go straight into sigil.ToolCall.InputJSON without
// re-encoding.
package transcript

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
)

// Line is a single decoded JSONL entry. Fields are nullable: assistant
// tool-call lines have no Content, user lines have no ToolCalls, etc.
type Line struct {
	Role             string     `json:"role"`
	Content          string     `json:"content,omitempty"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	Name             string     `json:"name,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	MessageID        string     `json:"message_id,omitempty"`
	Injected         bool       `json:"injected,omitempty"`

	// EndOffset is the byte position after this line in the transcript file.
	// Set by Read(), not deserialized from JSON.
	EndOffset int64 `json:"-"`
}

// ToolCall mirrors one entry of an assistant message's tool_calls array.
// Arguments is a JSON-encoded string (vibe serialises the call arguments
// to JSON), so callers pass its bytes through to InputJSON unchanged.
type ToolCall struct {
	ID       string `json:"id"`
	Index    int    `json:"index,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

const maxScannerBuf = 10 * 1024 * 1024 // 10MB for large tool result lines

// Read reads JSONL lines from path starting at the given byte offset.
// Returns the parsed lines, the new end offset, and any I/O error.
// Unparseable lines are skipped so a single bad write does not stall
// every future export for the session.
func Read(path string, offset int64) ([]Line, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, offset, err
	}
	defer func() { _ = f.Close() }()

	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return nil, offset, err
		}
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), maxScannerBuf)

	var lastAdvance int
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		advance, token, err := bufio.ScanLines(data, atEOF)
		lastAdvance = advance
		return advance, token, err
	})

	var lines []Line
	pos := offset

	for scanner.Scan() {
		data := scanner.Bytes()
		lineLen := int64(lastAdvance)

		var line Line
		if err := json.Unmarshal(data, &line); err != nil {
			pos += lineLen
			continue
		}
		line.EndOffset = pos + lineLen
		lines = append(lines, line)
		pos += lineLen
	}

	if err := scanner.Err(); err != nil {
		return lines, pos, err
	}
	return lines, pos, nil
}
