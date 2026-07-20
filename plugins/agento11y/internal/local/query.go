package local

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/grafana/agento11y/go/sigil"
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
	// TokenBuckets sums the disjoint per-generation buckets across the
	// conversation, for the list's token breakdown tooltip.
	TokenBuckets TokenBuckets `json:"token_buckets"`
	Agents       []string     `json:"agents"`
	Models       []string     `json:"models"`
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
	GenerationID    string    `json:"generation_id"`
	AgentName       string    `json:"agent_name,omitempty"`
	Model           string    `json:"model,omitempty"`
	Provider        string    `json:"provider,omitempty"`
	StartedAt       time.Time `json:"started_at"`
	CompletedAt     time.Time `json:"completed_at"`
	DurationSeconds float64   `json:"duration_seconds"`
	InputTokens     int64     `json:"input_tokens"`
	OutputTokens    int64     `json:"output_tokens"`
	TotalTokens     int64     `json:"total_tokens"`
	// TokenBuckets is this step's disjoint usage split, so the viewer
	// can show where the step's tokens went (cache hit vs fresh input).
	TokenBuckets TokenBuckets    `json:"token_buckets"`
	Messages     []sigil.Message `json:"messages,omitempty"`
	Input        []sigil.Message `json:"input,omitempty"`
	Output       []sigil.Message `json:"output,omitempty"`
	Tools        []string        `json:"tools,omitempty"`
	ToolPreview  string          `json:"tool_preview,omitempty"`
	StopReason   string          `json:"stop_reason,omitempty"`
	CallError    string          `json:"call_error,omitempty"`
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
		sum.TokenBuckets = sum.TokenBuckets.plus(disjointTokenUsage(usage, gen.Model.Provider))

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
			TokenBuckets: disjointTokenUsage(usage, gen.Model.Provider),
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

// TokenBuckets is token usage split into five non-overlapping buckets
// (see disjointTokenUsage). Because they are disjoint, the viewer can
// stack or sum them without double-counting; the chart points, the
// conversation summaries, and the per-step views all share this shape.
type TokenBuckets struct {
	FreshInput int64 `json:"fresh_input"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
	Output     int64 `json:"output"`
	Reasoning  int64 `json:"reasoning"`
}

func (b TokenBuckets) plus(o TokenBuckets) TokenBuckets {
	return TokenBuckets{
		FreshInput: b.FreshInput + o.FreshInput,
		CacheRead:  b.CacheRead + o.CacheRead,
		CacheWrite: b.CacheWrite + o.CacheWrite,
		Output:     b.Output + o.Output,
		Reasoning:  b.Reasoning + o.Reasoning,
	}
}

// TokenUsagePoint is one generation's disjoint token buckets tagged
// with the model/provider that produced them and the time it ran. The
// viewer re-buckets these by time to draw the token-usage chart. The
// embedded TokenBuckets fields flatten into the JSON object.
type TokenUsagePoint struct {
	Timestamp time.Time `json:"t"`
	Model     string    `json:"model,omitempty"`
	Provider  string    `json:"provider,omitempty"`
	TokenBuckets
}

// TokenUsagePoints walks every conversation file and returns one point
// per generation that recorded any token usage, sorted oldest-first.
// A missing conversations dir yields no points (first-launch case).
func (s *Storage) TokenUsagePoints() ([]TokenUsagePoint, error) {
	dir := filepath.Join(s.dir, ConversationsDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []TokenUsagePoint
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".jsonl" {
			continue
		}
		err := scanGenerationRecords(filepath.Join(dir, e.Name()), func(r generationRecord, gen storedGeneration) {
			if p, ok := tokenUsagePoint(r, gen); ok {
				out = append(out, p)
			}
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Timestamp.Before(out[j].Timestamp)
	})
	return out, nil
}

// tokenUsagePoint builds a TokenUsagePoint from one record. ok is false
// when the generation recorded no tokens or has no usable timestamp, so
// the caller can skip it rather than plot a zero-height bar at the epoch.
func tokenUsagePoint(r generationRecord, gen storedGeneration) (TokenUsagePoint, bool) {
	buckets := disjointTokenUsage(gen.Usage.toSDK(), gen.Model.Provider)
	if buckets == (TokenBuckets{}) {
		return TokenUsagePoint{}, false
	}
	when := generationTime(gen, r)
	if when.IsZero() {
		return TokenUsagePoint{}, false
	}
	return TokenUsagePoint{
		Timestamp:    when,
		Model:        gen.modelName(),
		Provider:     gen.Model.Provider,
		TokenBuckets: buckets,
	}, true
}

// generationTime is the wall-clock moment a generation ran, preferring
// started_at, then completed_at, then the receiver's arrival time.
func generationTime(gen storedGeneration, r generationRecord) time.Time {
	if !gen.StartedAt.IsZero() {
		return gen.StartedAt
	}
	if !gen.CompletedAt.IsZero() {
		return gen.CompletedAt
	}
	when, _ := time.Parse(time.RFC3339Nano, r.ReceivedAt)
	return when
}

// disjointTokenUsage splits a generation's usage into five buckets that
// don't overlap, so the viewer can stack them without double-counting.
//
// Providers disagree on how cache and reasoning tokens relate to the
// input/output totals, so both carve-outs are provider-aware:
//
//   - cache_read: Anthropic reports input_tokens as the non-cached input,
//     so cache_read/cache_write are extra on top. OpenAI, Gemini, and
//     codex fold cached tokens into input_tokens, so cache_read is a
//     subset that must be carved back out (see cacheReadInsideInput).
//   - reasoning: OpenAI and codex nest reasoning inside output
//     (completion) tokens, so it's carved out. Gemini reports thoughts as
//     additive (output is just the candidate tokens) and Anthropic
//     doesn't populate it, so for those reasoning stands alone (see
//     reasoningInsideOutput).
//
// cache_write is never folded into input by any provider we map, so it
// always stands alone.
//
// For well-formed usage the buckets sum back to what the provider
// reported: Anthropic input + cache_read + cache_write + output; OpenAI
// input + output; Gemini input + output + reasoning (its total also
// counts tool-use prompt tokens, which the SDK's TokenUsage has no field
// for). When a subset field exceeds its total, the nonNeg clamps keep
// the subset and zero the remainder, so the sum can exceed what was
// reported.
func disjointTokenUsage(u sigil.TokenUsage, provider string) TokenBuckets {
	b := TokenBuckets{
		FreshInput: nonNeg(u.InputTokens),
		CacheRead:  nonNeg(u.CacheReadInputTokens),
		CacheWrite: nonNeg(u.CacheWriteInputTokens),
		Output:     nonNeg(u.OutputTokens),
		Reasoning:  nonNeg(u.ReasoningTokens),
	}
	if cacheReadInsideInput(provider) {
		b.FreshInput = nonNeg(b.FreshInput - b.CacheRead)
	}
	if reasoningInsideOutput(provider) {
		b.Output = nonNeg(b.Output - b.Reasoning)
	}
	return b
}

// cacheReadInsideInput reports whether the provider counts cache_read
// tokens within input_tokens (subset semantics). Anthropic keeps them
// separate; OpenAI and Gemini fold them in, and so does codex, the codex
// agent's fallback provider for model names it can't attribute (its
// usage comes from the Responses API). Unknown providers default to
// "separate" so we never subtract tokens we can't account for and end up
// hiding real input.
func cacheReadInsideInput(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "openai", "azure", "azure-openai", "azureopenai", "codex",
		"gemini", "google", "googleai", "google-genai", "vertex", "vertexai", "google-vertex":
		return true
	default:
		return false
	}
}

// reasoningInsideOutput reports whether the provider counts reasoning
// tokens within output_tokens (subset semantics). OpenAI and codex nest
// reasoning inside completion tokens; Gemini reports thoughts as a
// separate additive count and Anthropic doesn't populate reasoning, so
// both keep it standalone. Unknown providers default to "separate" so we
// never subtract reasoning we can't account for and end up hiding real
// output.
func reasoningInsideOutput(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "openai", "azure", "azure-openai", "azureopenai", "codex":
		return true
	default:
		return false
	}
}

func nonNeg(n int64) int64 {
	if n < 0 {
		return 0
	}
	return n
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
