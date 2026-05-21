package local

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/grafana/sigil-sdk/go/sigil"
)

// ConversationSummary is one row in the viewer's list screen. Numeric
// fields are raw so the client can format them (k/M, ms/s/m) and reuse
// them for tooltips, sort, and the activity histogram.
type ConversationSummary struct {
	ID           string    `json:"id"`
	Title        string    `json:"title,omitempty"`
	StartedAt    time.Time `json:"started_at"`
	LastActivity time.Time `json:"last_activity"`
	Calls        int       `json:"calls"`
	InputTokens  int64     `json:"input_tokens"`
	OutputTokens int64     `json:"output_tokens"`
	TotalTokens  int64     `json:"total_tokens"`
	Agents       []string  `json:"agents"`
	Models       []string  `json:"models"`
	// Status is "ok" or "err". "err" means at least one generation in
	// the conversation recorded a call_error.
	Status string `json:"status"`
}

// GenerationView is one step in the conversation thread.
//
// Messages is the display-order thread for the local viewer. Input and
// Output keep the raw SDK split: user/tool-result messages on input,
// assistant messages on output. They are empty under the default
// metadata_only mode, in which case the viewer should fall back to the
// token counts and tool preview.
type GenerationView struct {
	GenerationID    string          `json:"generation_id"`
	AgentName       string          `json:"agent_name,omitempty"`
	Model           string          `json:"model,omitempty"`
	Provider        string          `json:"provider,omitempty"`
	StartedAt       time.Time       `json:"started_at"`
	CompletedAt     time.Time       `json:"completed_at"`
	DurationSeconds float64         `json:"duration_seconds"`
	InputTokens     int64           `json:"input_tokens"`
	OutputTokens    int64           `json:"output_tokens"`
	TotalTokens     int64           `json:"total_tokens"`
	Messages        []sigil.Message `json:"messages,omitempty"`
	Input           []sigil.Message `json:"input,omitempty"`
	Output          []sigil.Message `json:"output,omitempty"`
	Tools           []string        `json:"tools,omitempty"`
	ToolPreview     string          `json:"tool_preview,omitempty"`
	StopReason      string          `json:"stop_reason,omitempty"`
	CallError       string          `json:"call_error,omitempty"`
}

// ConversationDetail is the payload for the detail screen — the
// conversation header plus its chronological generation list.
type ConversationDetail struct {
	ID          string           `json:"id"`
	Title       string           `json:"title,omitempty"`
	Generations []GenerationView `json:"generations"`
}

// ListConversations walks the conversations directory and produces one
// ConversationSummary per file, sorted newest-first by last_activity.
// A missing directory returns an empty slice (first-launch case).
// limit ≤ 0 means unbounded.
func (s *Storage) ListConversations(limit int) ([]ConversationSummary, error) {
	dir := filepath.Join(s.dir, ConversationsDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]ConversationSummary, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".jsonl" {
			continue
		}
		sum, ok, err := summariseConversationFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		if !ok {
			continue // empty or all-invalid file
		}
		out = append(out, sum)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].LastActivity.After(out[j].LastActivity)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// summariseConversationFile reads one per-conversation JSONL file and
// returns its aggregated summary. Returns (_, false, nil) when the file
// has no decodable records.
func summariseConversationFile(path string) (ConversationSummary, bool, error) {
	agents := map[string]struct{}{}
	models := map[string]struct{}{}
	var sum ConversationSummary
	var hasError, seen bool

	err := scanGenerationRecords(path, func(r generationRecord, gen storedGeneration) {
		seen = true
		if sum.ID == "" {
			sum.ID = r.ConversationID
		}
		sum.Calls++
		usage := gen.Usage.toSDK()
		sum.InputTokens += usage.InputTokens
		sum.OutputTokens += usage.OutputTokens
		sum.TotalTokens += usage.Normalize().TotalTokens

		if !gen.StartedAt.IsZero() && (sum.StartedAt.IsZero() || gen.StartedAt.Before(sum.StartedAt)) {
			sum.StartedAt = gen.StartedAt
		}
		// last_activity tracks the latest known timestamp on any
		// generation, falling back to received_at when started/completed
		// aren't populated so freshly-arrived records still bubble up.
		when := gen.CompletedAt
		if when.IsZero() {
			when = gen.StartedAt
		}
		if when.IsZero() {
			when, _ = time.Parse(time.RFC3339Nano, r.ReceivedAt)
		}
		if when.After(sum.LastActivity) {
			sum.LastActivity = when
		}

		if gen.AgentName != "" {
			agents[gen.AgentName] = struct{}{}
		}
		if name := gen.modelName(); name != "" {
			models[name] = struct{}{}
		}
		if sum.Title == "" && gen.title() != "" {
			sum.Title = gen.title()
		}
		if gen.CallError != "" {
			hasError = true
		}
	})
	if err != nil {
		return ConversationSummary{}, false, err
	}
	if !seen {
		return ConversationSummary{}, false, nil
	}
	sum.Agents = sortedKeys(agents)
	sum.Models = sortedKeys(models)
	sum.Status = "ok"
	if hasError {
		sum.Status = "err"
	}
	return sum, true, nil
}

// ConversationDetail returns the chronological generation list for one
// conversation. Returns (nil, nil) when no generations are recorded for
// the given id, so the handler can answer 404 cleanly.
func (s *Storage) ConversationDetail(id string) (*ConversationDetail, error) {
	if !validConversationID(id) {
		return nil, errors.New("invalid conversation id")
	}
	path := filepath.Join(s.dir, ConversationsDir, id+".jsonl")
	out := &ConversationDetail{ID: id}
	err := scanGenerationRecords(path, func(_ generationRecord, gen storedGeneration) {
		if out.Title == "" && gen.title() != "" {
			out.Title = gen.title()
		}
		usage := gen.Usage.toSDK()
		input := gen.inputMessages()
		output := gen.outputMessages()
		view := GenerationView{
			GenerationID: gen.ID,
			AgentName:    gen.AgentName,
			Model:        gen.modelName(),
			Provider:     gen.Model.Provider,
			StartedAt:    gen.StartedAt,
			CompletedAt:  gen.CompletedAt,
			InputTokens:  usage.InputTokens,
			OutputTokens: usage.OutputTokens,
			TotalTokens:  usage.Normalize().TotalTokens,
			Messages:     threadMessages(input, output),
			Input:        input,
			Output:       output,
			StopReason:   gen.StopReason,
			CallError:    gen.CallError,
		}
		if !gen.StartedAt.IsZero() && !gen.CompletedAt.IsZero() {
			view.DurationSeconds = gen.CompletedAt.Sub(gen.StartedAt).Seconds()
		}
		view.Tools, view.ToolPreview = extractTools(output)
		out.Generations = append(out.Generations, view)
	})
	if err != nil {
		return nil, err
	}
	if len(out.Generations) == 0 {
		return nil, nil
	}
	sort.SliceStable(out.Generations, func(i, j int) bool {
		return out.Generations[i].StartedAt.Before(out.Generations[j].StartedAt)
	})
	return out, nil
}

// scanGenerationRecords walks one per-conversation JSONL file calling visit
// for every decodable record. A missing file is not an error; lines that
// fail to decode (truncated mid-append, future-schema, …) are skipped.
func threadMessages(input, output []sigil.Message) []sigil.Message {
	if len(input) == 0 && len(output) == 0 {
		return nil
	}

	inputWithoutResults := make([]sigil.Message, 0, len(input))
	toolResults := make([]sigil.Message, 0, len(input))
	for _, msg := range input {
		if messageHasToolResult(msg) {
			toolResults = append(toolResults, msg)
			continue
		}
		inputWithoutResults = append(inputWithoutResults, msg)
	}

	if len(toolResults) == 0 {
		messages := make([]sigil.Message, 0, len(input)+len(output))
		messages = append(messages, input...)
		messages = append(messages, output...)
		return messages
	}

	messages := make([]sigil.Message, 0, len(input)+len(output))
	messages = append(messages, inputWithoutResults...)
	usedResults := make([]bool, len(toolResults))
	for _, outputMsg := range output {
		messages = append(messages, outputMsg)
		callIDs := toolCallIDs(outputMsg)
		if len(callIDs) == 0 {
			continue
		}
		for i, resultMsg := range toolResults {
			if usedResults[i] || !toolResultMatchesAny(resultMsg, callIDs) {
				continue
			}
			messages = append(messages, resultMsg)
			usedResults[i] = true
		}
	}
	for i, resultMsg := range toolResults {
		if !usedResults[i] {
			messages = append(messages, resultMsg)
		}
	}
	return messages
}

func messageHasToolResult(msg sigil.Message) bool {
	for _, part := range msg.Parts {
		if part.Kind == sigil.PartKindToolResult && part.ToolResult != nil {
			return true
		}
	}
	return false
}

func toolCallIDs(msg sigil.Message) map[string]struct{} {
	ids := map[string]struct{}{}
	for _, part := range msg.Parts {
		if part.Kind != sigil.PartKindToolCall || part.ToolCall == nil || part.ToolCall.ID == "" {
			continue
		}
		ids[part.ToolCall.ID] = struct{}{}
	}
	return ids
}

func toolResultMatchesAny(msg sigil.Message, ids map[string]struct{}) bool {
	for _, part := range msg.Parts {
		if part.Kind != sigil.PartKindToolResult || part.ToolResult == nil || part.ToolResult.ToolCallID == "" {
			continue
		}
		if _, ok := ids[part.ToolResult.ToolCallID]; ok {
			return true
		}
	}
	return false
}

func scanGenerationRecords(path string, visit func(generationRecord, storedGeneration)) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	// JSONL lines can hold full transcripts; bump the buffer well above
	// the default 64 KiB.
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec generationRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		var gen storedGeneration
		if err := json.Unmarshal(rec.Generation, &gen); err != nil {
			continue
		}
		visit(rec, gen)
	}
	return sc.Err()
}

// extractTools walks the assistant's output messages and collects the
// distinct tool names in call order. tool_preview is a short, legible
// snippet of the first call's input: we unwrap common single-field
// shapes (`command`, `query`, `file_path`) and otherwise fall back to
// the raw JSON, truncated.
func extractTools(msgs []sigil.Message) (names []string, preview string) {
	seen := map[string]struct{}{}
	for _, m := range msgs {
		for _, p := range m.Parts {
			if p.Kind != sigil.PartKindToolCall || p.ToolCall == nil {
				continue
			}
			if _, ok := seen[p.ToolCall.Name]; !ok {
				seen[p.ToolCall.Name] = struct{}{}
				names = append(names, p.ToolCall.Name)
			}
			if preview == "" {
				preview = renderToolPreview(p.ToolCall.InputJSON)
			}
		}
	}
	return names, preview
}

const toolPreviewMaxLen = 240

func renderToolPreview(input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(input, &m); err == nil {
		for _, key := range []string{"command", "cmd", "query", "prompt", "path", "file_path"} {
			if s, ok := m[key].(string); ok && s != "" {
				return truncate(s, toolPreviewMaxLen)
			}
		}
	}
	return truncate(string(input), toolPreviewMaxLen)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.ValidString(s[:max]) {
		max--
	}
	return s[:max] + "…"
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
