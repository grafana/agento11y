package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
)

// Line represents a single JSONL line from a Claude Code transcript.
type Line struct {
	Type        string          `json:"type"`
	UUID        string          `json:"uuid"`
	ParentUUID  string          `json:"parentUuid"`
	Timestamp   string          `json:"timestamp"`
	SessionID   string          `json:"sessionId"`
	Version     string          `json:"version"`
	GitBranch   string          `json:"gitBranch"`
	CWD         string          `json:"cwd"`
	Entrypoint  string          `json:"entrypoint"`
	RequestID   string          `json:"requestId"`
	IsSidechain bool            `json:"isSidechain"`
	Message     json.RawMessage `json:"message"`

	// AgentID identifies the subagent that produced a sidechain line. Claude
	// Code names the subagent's own transcript file after it
	// (subagents/agent-<agentId>.jsonl); the history importer matches a
	// subagent transcript to its parent from that filename rather than from
	// this field.
	AgentID string `json:"agentId"`
	// AttributionAgent is the subagent type a sidechain line ran under, for
	// example "general-purpose". The mapper appends it to the agent name so a
	// subagent turn is distinguishable from a main-thread one.
	AttributionAgent string `json:"attributionAgent"`

	// EndOffset is the byte position after this line in the transcript file.
	// Set by Read(), not deserialized from JSON.
	EndOffset int64 `json:"-"`
}

// AssistantMessage is the decoded message for type="assistant" lines.
type AssistantMessage struct {
	Model      string         `json:"model"`
	ID         string         `json:"id"`
	Content    []ContentBlock `json:"content"`
	StopReason string         `json:"stop_reason"`
	Usage      Usage          `json:"usage"`
}

// ContentBlock is a single block within an assistant message.
type ContentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// Thinking holds the reasoning text of a "thinking" block, which Claude
	// Code writes under its own key rather than under "text". The mapper does
	// not export the text (a block can exceed 50 KB); the field exists so a
	// decoded block reflects the on-disk shape.
	Thinking string `json:"thinking,omitempty"`
}

// Usage tracks token consumption for an assistant message.
type Usage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

// UserMessage is the decoded message for type="user" lines.
// Content can be a string or []UserContentBlock; use ParseUserContent.
type UserMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// UserContentBlock is a typed block within a user message array content.
// RawContent can be a plain string or an array of content blocks
// (e.g. [{"type":"text","text":"..."}]) depending on the tool.
type UserContentBlock struct {
	Type       string          `json:"type"`
	ToolUseID  string          `json:"tool_use_id,omitempty"`
	RawContent json.RawMessage `json:"content,omitempty"`
	IsError    bool            `json:"is_error,omitempty"`
	Text       string          `json:"text,omitempty"`
}

// Content returns the tool result content as a string, handling both
// plain string and array-of-blocks formats from Claude Code transcripts.
func (b *UserContentBlock) Content() string {
	if len(b.RawContent) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(b.RawContent, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(b.RawContent, &blocks); err == nil {
		var parts []string
		for _, bl := range blocks {
			if bl.Text != "" {
				parts = append(parts, bl.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return string(b.RawContent)
}

// ParseUserContent parses the polymorphic Content field of a UserMessage.
// Returns the text prompt if content is a string, or the parsed blocks if it's an array.
func ParseUserContent(raw json.RawMessage) (text string, blocks []UserContentBlock, err error) {
	if err = json.Unmarshal(raw, &text); err == nil {
		return text, nil, nil
	}
	err = json.Unmarshal(raw, &blocks)
	return "", blocks, err
}

// skipTypes are line types we never process.
var skipTypes = map[string]bool{
	"file-history-snapshot": true,
	"queue-operation":       true,
	"attachment":            true,
	"permission-mode":       true,
	"last-prompt":           true,
	// Claude Code emits these for UI title generation; not LLM turns.
	"ai-title": true,
	// local_command / stop_hook_summary / turn_duration metadata.
	"system": true,
}

// maxLineBytes bounds one transcript line. Tool results can be large, so this
// is generous. A line past it is skipped like an unparseable one and reading
// continues, so one oversized line never costs the rest of the file. A var so
// tests can lower it.
var maxLineBytes = 10 * 1024 * 1024

// Read reads JSONL lines from path starting at the given byte offset.
// Returns parsed lines, the new byte offset, and any I/O error.
// Unparseable lines and lines longer than maxLineBytes are skipped; the
// returned offset always advances past them.
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

	r := bufio.NewReaderSize(f, 64*1024)

	var lines []Line
	pos := offset

	for {
		data, consumed, tooLong, err := readLine(r, maxLineBytes)
		if err == io.EOF {
			break
		}
		if err != nil {
			return lines, pos, err
		}
		pos += consumed
		if tooLong {
			continue
		}

		var line Line
		if err := json.Unmarshal(data, &line); err != nil {
			continue
		}

		if skipTypes[line.Type] {
			continue
		}

		line.EndOffset = pos
		lines = append(lines, line)
	}

	return lines, pos, nil
}

// readLine returns the next line without its trailing newline, and the bytes
// consumed including the newline. A line longer than max is discarded as it
// streams past, never buffered, and reported through tooLong, so one oversized
// line costs itself and nothing after it. This is the behavior bufio.Scanner
// cannot give: its ErrTooLong is terminal and abandons the rest of the file.
// io.EOF is returned only when nothing remains.
func readLine(r *bufio.Reader, max int) (line []byte, consumed int64, tooLong bool, err error) {
	var buf []byte
	for {
		chunk, rerr := r.ReadSlice('\n')
		consumed += int64(len(chunk))
		if !tooLong {
			if len(buf)+len(chunk) > max {
				tooLong = true
				buf = nil
			} else {
				buf = append(buf, chunk...)
			}
		}
		if rerr == nil {
			break
		}
		if rerr == bufio.ErrBufferFull {
			continue
		}
		if rerr == io.EOF {
			if consumed == 0 {
				return nil, 0, false, io.EOF
			}
			break
		}
		return nil, consumed, tooLong, rerr
	}
	line = bytes.TrimSuffix(buf, []byte("\n"))
	line = bytes.TrimSuffix(line, []byte("\r"))
	return line, consumed, tooLong, nil
}
