package transcript

import (
	"bufio"
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
}

const maxScannerBuf = 10 * 1024 * 1024 // 10MB for large tool result lines

// Read reads JSONL lines from path starting at the given byte offset.
// Returns parsed lines, the new byte offset, and any I/O error.
// Unparseable lines are silently skipped.
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

		if skipTypes[line.Type] {
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
