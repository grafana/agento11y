// Package codexlog reads Codex rollout transcripts.
//
// It is the one rollout reader. The live hook uses it to recover a turn's token
// usage and its subagent spawn link; the history importer uses the same
// scanner and payload parsers to rebuild whole sessions from disk. Anything
// that decodes a rollout line belongs here, so the two paths cannot drift.
package codexlog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

const (
	// DefaultMaxLineBytes bounds one rollout line. A tool result can be large,
	// so this is generous, but a line past it is corruption.
	DefaultMaxLineBytes = 16 * 1024 * 1024
	// liveMaxLineBytes and liveMaxTranscriptBytes bound the live hook, which
	// runs inside an agent's stop handler and must not spend unbounded time on
	// a huge rollout. The importer sets its own budget: it is expected to read
	// whole sessions, and rollouts of several hundred megabytes exist.
	liveMaxLineBytes       = 1024 * 1024
	liveMaxTranscriptBytes = 32 * 1024 * 1024
)

type SessionMeta struct {
	SessionID       string
	ThreadSource    string
	ParentSessionID string
	AgentRole       string
	AgentNickname   string
	AgentDepth      int
	// Cwd is the workspace the session ran in. Only session_meta carries it,
	// which is what makes a rollout attributable to a repository.
	Cwd string
}

type SpawnLink struct {
	ChildSessionID     string
	ParentSessionID    string
	ParentTurnID       string
	ParentGenerationID string
	SpawnCallID        string
	AgentNickname      string
}

type TokenUsage struct {
	InputTokens           int64 `json:"input_tokens"`
	CachedInputTokens     int64 `json:"cached_input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	ReasoningOutputTokens int64 `json:"reasoning_output_tokens"`
	TotalTokens           int64 `json:"total_tokens"`
}

type TokenUsageInfo struct {
	TotalTokenUsage    TokenUsage `json:"total_token_usage"`
	LastTokenUsage     TokenUsage `json:"last_token_usage"`
	ModelContextWindow int64      `json:"model_context_window"`
}

type TokenSnapshot struct {
	TurnID             string
	TurnUsage          TokenUsage
	BaselineUsage      TokenUsage
	LastUsage          TokenUsage
	TotalUsage         TokenUsage
	ModelContextWindow int64
	Source             string
}

// Record is one decoded rollout line: the envelope, with the payload left as
// raw JSON for the Parse* helpers below.
type Record struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

// ScanOptions bounds one rollout scan.
type ScanOptions struct {
	// MaxLineBytes caps a single line. Zero uses DefaultMaxLineBytes.
	MaxLineBytes int
	// MaxBytes caps the whole scan and fails past it. Zero means no cap, which
	// is what the importer needs to read a large rollout to the end.
	MaxBytes int64
	// SkipMalformedLines drops an undecodable line instead of failing. The
	// importer sets it: one torn line in a months-old rollout should cost that
	// line, not the session.
	SkipMalformedLines bool
}

// LiveScanOptions is the budget for the live hook, which runs inside an agent's
// stop handler. Malformed lines are skipped: the hook reads a rollout the
// running Codex process is still appending to, so a half-written final line is
// ordinary and must cost that line rather than the whole scan.
func LiveScanOptions() ScanOptions {
	return ScanOptions{
		MaxLineBytes:       liveMaxLineBytes,
		MaxBytes:           liveMaxTranscriptBytes,
		SkipMalformedLines: true,
	}
}

// ImportScanOptions is the budget for the history importer. It sets no total
// cap: an import is expected to read a whole rollout, and rollouts of several
// hundred megabytes exist.
func ImportScanOptions() ScanOptions {
	return ScanOptions{SkipMalformedLines: true}
}

// ScanRecords decodes path one line at a time and calls visit for each record.
// visit returning true stops the scan. Nothing is buffered beyond the current
// line, so a caller can stream a rollout of any size.
func ScanRecords(path string, opts ScanOptions, visit func(Record) (bool, error)) error {
	maxLine := opts.MaxLineBytes
	if maxLine <= 0 {
		maxLine = DefaultMaxLineBytes
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLine)
	var read int64
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		read += int64(len(scanner.Bytes())) + 1
		if opts.MaxBytes > 0 && read > opts.MaxBytes {
			return fmt.Errorf("codexlog: transcript byte budget exceeded")
		}
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			continue
		}
		var rec Record
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			if opts.SkipMalformedLines {
				continue
			}
			return fmt.Errorf("codexlog: line %d: %w", lineNo, err)
		}
		done, err := visit(rec)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("codexlog: scan: %w", err)
	}
	return nil
}

// ReadSessionMeta returns the session_meta record of a rollout. opts bounds the
// scan: the live hook passes [LiveScanOptions], the importer
// [ImportScanOptions], which is what lets it read a rollout past the live cap.
func ReadSessionMeta(path string, opts ScanOptions) (SessionMeta, bool, error) {
	var found SessionMeta
	ok := false
	err := ScanRecords(path, opts, func(rec Record) (bool, error) {
		if rec.Type != "session_meta" {
			return false, nil
		}
		meta, metaOK := ParseSessionMeta(rec.Payload)
		if !metaOK {
			return true, nil
		}
		found = meta
		ok = true
		return true, nil
	})
	return found, ok, err
}

// ResolveSpawnLink finds the turn in a parent rollout that spawned one child
// session. opts bounds the scan; see [ReadSessionMeta].
func ResolveSpawnLink(parentTranscriptPath, childSessionID string, opts ScanOptions, generationID func(sessionID, turnID string) string) (SpawnLink, bool, error) {
	var (
		parentSessionID string
		latestTurnID    string
		calls           = map[string]string{}
		found           SpawnLink
		ok              bool
	)

	err := ScanRecords(parentTranscriptPath, opts, func(rec Record) (bool, error) {
		switch rec.Type {
		case "session_meta":
			if meta, metaOK := ParseSessionMeta(rec.Payload); metaOK && meta.SessionID != "" {
				parentSessionID = meta.SessionID
			}
		case "turn_context":
			if turnID := ParseTurnContext(rec.Payload).TurnID; turnID != "" {
				latestTurnID = turnID
			}
		case "response_item":
			item, itemOK := ParseResponseItem(rec.Payload)
			if !itemOK {
				return false, nil
			}
			switch item.Type {
			case "function_call":
				if item.Name == "spawn_agent" && item.CallID != "" {
					calls[item.CallID] = latestTurnID
				}
			case "function_call_output":
				parentTurnID, callOK := calls[item.CallID]
				if !callOK || parentTurnID == "" {
					return false, nil
				}
				agentID, nickname := ParseSpawnOutput(item.Output)
				if agentID == "" || agentID != childSessionID {
					return false, nil
				}
				found = SpawnLink{
					ChildSessionID:  childSessionID,
					ParentSessionID: parentSessionID,
					ParentTurnID:    parentTurnID,
					SpawnCallID:     item.CallID,
					AgentNickname:   nickname,
				}
				if found.ParentSessionID != "" && generationID != nil {
					found.ParentGenerationID = generationID(found.ParentSessionID, found.ParentTurnID)
				}
				ok = true
				return true, nil
			}
		}
		return false, nil
	})
	return found, ok, err
}

// ReadTokenUsageForTurn recovers one turn's token usage from a rollout. opts
// bounds the scan; see [ReadSessionMeta].
func ReadTokenUsageForTurn(path, turnID string, opts ScanOptions) (TokenSnapshot, bool, error) {
	if path == "" || turnID == "" {
		return TokenSnapshot{}, false, nil
	}

	var (
		activeTurnID      string
		seenAnyTurn       bool
		targetStarted     bool
		targetIsFirstTurn bool
		targetModelActive bool
		haveBaseline      bool
		baseline          TokenUsage
		haveLastTotal     bool
		lastTotal         TokenUsage
		haveFinal         bool
		finalInfo         TokenUsageInfo
	)

	err := ScanRecords(path, opts, func(rec Record) (bool, error) {
		switch rec.Type {
		case "turn_context":
			nextTurnID := ParseTurnContext(rec.Payload).TurnID
			if nextTurnID == "" {
				return false, nil
			}
			if !seenAnyTurn {
				targetIsFirstTurn = nextTurnID == turnID
			}
			seenAnyTurn = true
			activeTurnID = nextTurnID
			if nextTurnID == turnID && !targetStarted {
				targetStarted = true
				targetModelActive = false
				if haveLastTotal {
					baseline = lastTotal
					haveBaseline = true
				}
			}
		case "response_item":
			if activeTurnID != turnID || !targetStarted {
				return false, nil
			}
			item, ok := ParseResponseItem(rec.Payload)
			if !ok {
				return false, nil
			}
			if IsModelActivity(item) {
				targetModelActive = true
			}
		case "event_msg":
			info, ok := ParseTokenUsageInfo(rec.Payload)
			if !ok {
				return false, nil
			}
			if activeTurnID == turnID && targetStarted {
				if !targetModelActive {
					baseline = info.TotalTokenUsage
					haveBaseline = true
					lastTotal = info.TotalTokenUsage
					haveLastTotal = true
					return false, nil
				}
				finalInfo = info
				haveFinal = true
			}
			lastTotal = info.TotalTokenUsage
			haveLastTotal = true
		}
		return false, nil
	})
	if err != nil {
		return TokenSnapshot{}, false, err
	}
	if !targetStarted || !haveFinal {
		return TokenSnapshot{}, false, nil
	}
	if !haveBaseline {
		if !targetIsFirstTurn {
			return TokenSnapshot{}, false, nil
		}
		baseline = TokenUsage{}
	}
	turnUsage, ok := SubtractUsage(finalInfo.TotalTokenUsage, baseline)
	if !ok || !HasPositiveUsage(turnUsage) {
		return TokenSnapshot{}, false, nil
	}
	return TokenSnapshot{
		TurnID:             turnID,
		TurnUsage:          turnUsage,
		BaselineUsage:      baseline,
		LastUsage:          finalInfo.LastTokenUsage,
		TotalUsage:         finalInfo.TotalTokenUsage,
		ModelContextWindow: finalInfo.ModelContextWindow,
		Source:             "turn_context_delta",
	}, true, nil
}

// ParseSessionMeta decodes a session_meta payload. Codex has moved these
// fields around across releases, so each one is read from every spelling it
// has used.
func ParseSessionMeta(raw json.RawMessage) (SessionMeta, bool) {
	var p struct {
		ID              string `json:"id"`
		SessionID       string `json:"session_id"`
		Cwd             string `json:"cwd"`
		ThreadSource    string `json:"thread_source"`
		ParentSessionID string `json:"parent_session_id"`
		ParentThreadID  string `json:"parent_thread_id"`
		AgentRole       string `json:"agent_role"`
		AgentNickname   string `json:"agent_nickname"`
		AgentDepth      int    `json:"agent_depth"`
		Depth           int    `json:"depth"`
		Source          struct {
			Subagent struct {
				ThreadSpawn struct {
					ParentThreadID string `json:"parent_thread_id"`
					Depth          int    `json:"depth"`
					AgentNickname  string `json:"agent_nickname"`
					AgentRole      string `json:"agent_role"`
				} `json:"thread_spawn"`
			} `json:"subagent"`
		} `json:"source"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return SessionMeta{}, false
	}
	meta := SessionMeta{
		SessionID:       firstNonEmpty(p.ID, p.SessionID),
		Cwd:             p.Cwd,
		ThreadSource:    p.ThreadSource,
		ParentSessionID: firstNonEmpty(p.ParentSessionID, p.ParentThreadID, p.Source.Subagent.ThreadSpawn.ParentThreadID),
		AgentRole:       firstNonEmpty(p.AgentRole, p.Source.Subagent.ThreadSpawn.AgentRole),
		AgentNickname:   firstNonEmpty(p.AgentNickname, p.Source.Subagent.ThreadSpawn.AgentNickname),
		AgentDepth:      firstNonZero(p.AgentDepth, p.Depth, p.Source.Subagent.ThreadSpawn.Depth),
	}
	if meta.ThreadSource == "" && meta.ParentSessionID != "" {
		meta.ThreadSource = "subagent"
	}
	return meta, meta.SessionID != "" || meta.ParentSessionID != "" || meta.ThreadSource != ""
}

// TurnContext is the decoded turn_context payload, which opens a turn.
type TurnContext struct {
	TurnID string `json:"turn_id"`
	Cwd    string `json:"cwd"`
	Model  string `json:"model"`
}

// ParseTurnContext decodes a turn_context payload. A payload that does not
// decode yields the zero value, whose empty TurnID the caller treats as
// "no turn ID recorded".
func ParseTurnContext(raw json.RawMessage) TurnContext {
	var p TurnContext
	_ = json.Unmarshal(raw, &p)
	return p
}

// EventMsg is the decoded event_msg payload shared by the live hook and the
// importer. Only the fields both need are declared; ParseTokenUsageInfo reads
// the token_count body.
type EventMsg struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

func ParseEventMsg(raw json.RawMessage) EventMsg {
	var p EventMsg
	_ = json.Unmarshal(raw, &p)
	return p
}

// ParseTokenUsageInfo decodes a token_count event_msg payload. ok is false for
// any other event.
func ParseTokenUsageInfo(raw json.RawMessage) (TokenUsageInfo, bool) {
	var p struct {
		Type string          `json:"type"`
		Info *TokenUsageInfo `json:"info"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return TokenUsageInfo{}, false
	}
	if p.Type != "token_count" || p.Info == nil {
		return TokenUsageInfo{}, false
	}
	return *p.Info, true
}

// SubtractUsage returns final minus baseline. Codex reports cumulative session
// totals, so a turn's own usage is the difference between two snapshots. ok is
// false when any component would go negative, which means the two snapshots do
// not belong to the same run.
func SubtractUsage(final, baseline TokenUsage) (TokenUsage, bool) {
	out := TokenUsage{
		InputTokens:           final.InputTokens - baseline.InputTokens,
		CachedInputTokens:     final.CachedInputTokens - baseline.CachedInputTokens,
		OutputTokens:          final.OutputTokens - baseline.OutputTokens,
		ReasoningOutputTokens: final.ReasoningOutputTokens - baseline.ReasoningOutputTokens,
		TotalTokens:           final.TotalTokens - baseline.TotalTokens,
	}
	if out.InputTokens < 0 ||
		out.CachedInputTokens < 0 ||
		out.OutputTokens < 0 ||
		out.ReasoningOutputTokens < 0 ||
		out.TotalTokens < 0 {
		return TokenUsage{}, false
	}
	return out, true
}

// HasPositiveUsage reports whether any token count is above zero.
func HasPositiveUsage(u TokenUsage) bool {
	return u.InputTokens > 0 ||
		u.CachedInputTokens > 0 ||
		u.OutputTokens > 0 ||
		u.ReasoningOutputTokens > 0 ||
		u.TotalTokens > 0
}

// ResponseItem is the decoded response_item payload: one model output, tool
// call, tool result, or reasoning block.
type ResponseItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Name      string          `json:"name"`
	CallID    string          `json:"call_id"`
	Arguments json.RawMessage `json:"arguments"`
	Input     json.RawMessage `json:"input"`
	Output    json.RawMessage `json:"output"`
	Content   json.RawMessage `json:"content"`
	// Raw is the undecoded payload. A local_shell_call keeps its command in a
	// shape that has changed across Codex releases, so the importer falls back
	// to the raw payload for it.
	Raw json.RawMessage `json:"-"`
}

// IsModelActivity reports whether an item is evidence the model ran, which is
// what separates a real turn from a bookkeeping segment.
func IsModelActivity(item ResponseItem) bool {
	switch item.Type {
	case "reasoning", "function_call", "custom_tool_call", "local_shell_call":
		return true
	case "message":
		return item.Role == "assistant"
	default:
		return false
	}
}

// ParseResponseItem decodes a response_item payload, accepting both the flat
// shape and the {"item": {...}} wrapper Codex has used.
func ParseResponseItem(raw json.RawMessage) (ResponseItem, bool) {
	var item ResponseItem
	if err := json.Unmarshal(raw, &item); err == nil && item.Type != "" {
		item.Raw = raw
		return item, true
	}
	var wrapped struct {
		Item ResponseItem `json:"item"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil && wrapped.Item.Type != "" {
		wrapped.Item.Raw = raw
		return wrapped.Item, true
	}
	return ResponseItem{Raw: raw}, false
}

// MessageText flattens a message content field, which Codex writes either as a
// plain string or as an array of typed text parts.
func MessageText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case "input_text", "output_text", "text":
			if part.Text != "" {
				out = append(out, part.Text)
			}
		}
	}
	return strings.Join(out, "\n")
}

// ParseSpawnOutput reads the child session ID and nickname out of a
// spawn_agent tool result, which is how a parent rollout names the subagent it
// started.
func ParseSpawnOutput(raw json.RawMessage) (agentID, nickname string) {
	if len(raw) == 0 {
		return "", ""
	}
	payload := raw
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		payload = []byte(asString)
	}
	var out struct {
		AgentID   string `json:"agent_id"`
		SessionID string `json:"session_id"`
		ThreadID  string `json:"thread_id"`
		Nickname  string `json:"nickname"`
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		return "", ""
	}
	return firstNonEmpty(out.AgentID, out.SessionID, out.ThreadID), out.Nickname
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func firstNonZero(values ...int) int {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}
