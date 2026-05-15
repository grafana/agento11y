package codexlog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

const (
	maxLineBytes       = 1024 * 1024
	maxTranscriptBytes = 32 * 1024 * 1024
)

type SessionMeta struct {
	SessionID       string
	ThreadSource    string
	ParentSessionID string
	AgentRole       string
	AgentNickname   string
	AgentDepth      int
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

type line struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

func ReadSessionMeta(path string) (SessionMeta, bool, error) {
	var found SessionMeta
	ok := false
	err := scanJSONLines(path, func(raw []byte) (bool, error) {
		var l line
		if err := json.Unmarshal(raw, &l); err != nil || l.Type != "session_meta" {
			return false, nil
		}
		meta, metaOK := parseSessionMeta(l.Payload)
		if !metaOK {
			return true, nil
		}
		found = meta
		ok = true
		return true, nil
	})
	return found, ok, err
}

func ResolveSpawnLink(parentTranscriptPath, childSessionID string, generationID func(sessionID, turnID string) string) (SpawnLink, bool, error) {
	var (
		parentSessionID string
		latestTurnID    string
		calls           = map[string]string{}
		found           SpawnLink
		ok              bool
	)

	err := scanJSONLines(parentTranscriptPath, func(raw []byte) (bool, error) {
		var l line
		if err := json.Unmarshal(raw, &l); err != nil {
			return false, nil
		}
		switch l.Type {
		case "session_meta":
			if meta, metaOK := parseSessionMeta(l.Payload); metaOK && meta.SessionID != "" {
				parentSessionID = meta.SessionID
			}
		case "turn_context":
			if turnID := parseTurnID(l.Payload); turnID != "" {
				latestTurnID = turnID
			}
		case "response_item":
			item, itemOK := parseResponseItem(l.Payload)
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
				agentID, nickname := parseSpawnOutput(item.Output)
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

func ReadTokenUsageForTurn(path, turnID string) (TokenSnapshot, bool, error) {
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

	err := scanJSONLines(path, func(raw []byte) (bool, error) {
		var l line
		if err := json.Unmarshal(raw, &l); err != nil {
			return false, nil
		}
		switch l.Type {
		case "turn_context":
			nextTurnID := parseTurnID(l.Payload)
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
			item, ok := parseResponseItem(l.Payload)
			if !ok {
				return false, nil
			}
			if isModelActivity(item) {
				targetModelActive = true
			}
		case "event_msg":
			info, ok := parseTokenUsageInfo(l.Payload)
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
	turnUsage, ok := subtractUsage(finalInfo.TotalTokenUsage, baseline)
	if !ok || !hasPositiveUsage(turnUsage) {
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

func scanJSONLines(path string, visit func(raw []byte) (bool, error)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
	var read int64
	for scanner.Scan() {
		read += int64(len(scanner.Bytes())) + 1
		if read > maxTranscriptBytes {
			return fmt.Errorf("codexlog: transcript byte budget exceeded")
		}
		done, err := visit(scanner.Bytes())
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

func parseSessionMeta(raw json.RawMessage) (SessionMeta, bool) {
	var p struct {
		ID              string `json:"id"`
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
		SessionID:       p.ID,
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

func parseTurnID(raw json.RawMessage) string {
	var p struct {
		TurnID string `json:"turn_id"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return ""
	}
	return p.TurnID
}

func parseTokenUsageInfo(raw json.RawMessage) (TokenUsageInfo, bool) {
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

func subtractUsage(final, baseline TokenUsage) (TokenUsage, bool) {
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

func hasPositiveUsage(u TokenUsage) bool {
	return u.InputTokens > 0 ||
		u.CachedInputTokens > 0 ||
		u.OutputTokens > 0 ||
		u.ReasoningOutputTokens > 0 ||
		u.TotalTokens > 0
}

type responseItem struct {
	Type   string          `json:"type"`
	Role   string          `json:"role"`
	Name   string          `json:"name"`
	CallID string          `json:"call_id"`
	Output json.RawMessage `json:"output"`
}

func isModelActivity(item responseItem) bool {
	switch item.Type {
	case "reasoning", "function_call", "custom_tool_call", "local_shell_call":
		return true
	case "message":
		return item.Role == "assistant"
	default:
		return false
	}
}

func parseResponseItem(raw json.RawMessage) (responseItem, bool) {
	var item responseItem
	if err := json.Unmarshal(raw, &item); err == nil && item.Type != "" {
		return item, true
	}
	var wrapped struct {
		Item responseItem `json:"item"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil && wrapped.Item.Type != "" {
		return wrapped.Item, true
	}
	return responseItem{}, false
}

func parseSpawnOutput(raw json.RawMessage) (agentID, nickname string) {
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
