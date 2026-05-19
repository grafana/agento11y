package hook

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

type Payload struct {
	HookEventNameJSON string `json:"hook_event_name,omitempty"`
	HookEventNameJS   string `json:"hookEventName,omitempty"`

	SessionIDJSON string `json:"session_id,omitempty"`
	SessionIDJS   string `json:"sessionId,omitempty"`

	CWD string `json:"cwd,omitempty"`

	SourceValue string `json:"source,omitempty"`

	InitialPromptJSON string `json:"initial_prompt,omitempty"`
	InitialPromptJS   string `json:"initialPrompt,omitempty"`
	Prompt            string `json:"prompt,omitempty"`

	ToolNameJSON string `json:"tool_name,omitempty"`
	ToolNameJS   string `json:"toolName,omitempty"`

	ToolInputJSON json.RawMessage `json:"tool_input,omitempty"`
	ToolInputJS   json.RawMessage `json:"toolInput,omitempty"`

	ToolResultJSON json.RawMessage `json:"tool_result,omitempty"`
	ToolResultJS   json.RawMessage `json:"toolResult,omitempty"`
	ToolOutputJSON json.RawMessage `json:"tool_output,omitempty"`
	ToolOutputJS   json.RawMessage `json:"toolOutput,omitempty"`

	DurationMsJSON *float64 `json:"duration_ms,omitempty"`
	DurationMsJS   *float64 `json:"durationMs,omitempty"`

	Status string `json:"status,omitempty"`

	ErrorJSON json.RawMessage `json:"error,omitempty"`

	ErrorContextJSON string `json:"error_context,omitempty"`
	ErrorContextJS   string `json:"errorContext,omitempty"`

	Recoverable *bool `json:"recoverable,omitempty"`

	ReasonValue string `json:"reason,omitempty"`

	StopReasonJSON string `json:"stop_reason,omitempty"`
	StopReasonJS   string `json:"stopReason,omitempty"`

	TranscriptPathJSON string `json:"transcript_path,omitempty"`
	TranscriptPathJS   string `json:"transcriptPath,omitempty"`

	AgentNameJSON string `json:"agent_name,omitempty"`
	AgentNameJS   string `json:"agentName,omitempty"`

	AgentDisplayNameJSON string `json:"agent_display_name,omitempty"`
	AgentDisplayNameJS   string `json:"agentDisplayName,omitempty"`

	AgentDescriptionJSON string `json:"agent_description,omitempty"`
	AgentDescriptionJS   string `json:"agentDescription,omitempty"`

	Model string `json:"model,omitempty"`

	ProviderName string `json:"provider,omitempty"`

	InputTokensJSON           *int64 `json:"input_tokens,omitempty"`
	InputTokensJS             *int64 `json:"inputTokens,omitempty"`
	OutputTokensJSON          *int64 `json:"output_tokens,omitempty"`
	OutputTokensJS            *int64 `json:"outputTokens,omitempty"`
	CacheReadInputTokensJSON  *int64 `json:"cache_read_input_tokens,omitempty"`
	CacheReadInputTokensJS    *int64 `json:"cacheReadInputTokens,omitempty"`
	CacheWriteInputTokensJSON *int64 `json:"cache_write_input_tokens,omitempty"`
	CacheWriteInputTokensJS   *int64 `json:"cacheWriteInputTokens,omitempty"`
	ReasoningTokensJSON       *int64 `json:"reasoning_tokens,omitempty"`
	ReasoningTokensJS         *int64 `json:"reasoningTokens,omitempty"`

	Timestamp json.RawMessage `json:"timestamp,omitempty"`
}

func (p Payload) EventName() string {
	return firstNonEmpty(p.HookEventNameJSON, p.HookEventNameJS)
}

func (p Payload) SessionID() string {
	return firstNonEmpty(p.SessionIDJSON, p.SessionIDJS)
}

func (p Payload) Source() string {
	return firstNonEmpty(p.SourceValue)
}

func (p Payload) InitialPrompt() string {
	return firstNonEmpty(p.InitialPromptJSON, p.InitialPromptJS)
}

func (p Payload) ToolName() string {
	return firstNonEmpty(p.ToolNameJSON, p.ToolNameJS)
}

func (p Payload) ToolInput() json.RawMessage {
	return firstNonEmptyRaw(p.ToolInputJSON, p.ToolInputJS)
}

func (p Payload) ToolResult() json.RawMessage {
	return firstNonEmptyRaw(p.ToolResultJSON, p.ToolResultJS, p.ToolOutputJSON, p.ToolOutputJS)
}

func (p Payload) DurationMs() *float64 {
	if p.DurationMsJSON != nil {
		return p.DurationMsJSON
	}
	return p.DurationMsJS
}

func (p Payload) Error() json.RawMessage {
	return firstNonEmptyRaw(p.ErrorJSON)
}

func (p Payload) ErrorContext() string {
	return firstNonEmpty(p.ErrorContextJSON, p.ErrorContextJS)
}

func (p Payload) Reason() string {
	return firstNonEmpty(p.ReasonValue)
}

func (p Payload) StopReason() string {
	return firstNonEmpty(p.StopReasonJSON, p.StopReasonJS)
}

func (p Payload) TranscriptPath() string {
	return firstNonEmpty(p.TranscriptPathJSON, p.TranscriptPathJS)
}

func (p Payload) AgentName() string {
	return firstNonEmpty(p.AgentNameJSON, p.AgentNameJS)
}

func (p Payload) AgentDisplayName() string {
	return firstNonEmpty(p.AgentDisplayNameJSON, p.AgentDisplayNameJS)
}

func (p Payload) AgentDescription() string {
	return firstNonEmpty(p.AgentDescriptionJSON, p.AgentDescriptionJS)
}

func (p Payload) Provider() string {
	return firstNonEmpty(p.ProviderName)
}

func (p Payload) InputTokens() *int64 {
	if p.InputTokensJSON != nil {
		return p.InputTokensJSON
	}
	return p.InputTokensJS
}

func (p Payload) OutputTokens() *int64 {
	if p.OutputTokensJSON != nil {
		return p.OutputTokensJSON
	}
	return p.OutputTokensJS
}

func (p Payload) CacheReadInputTokens() *int64 {
	if p.CacheReadInputTokensJSON != nil {
		return p.CacheReadInputTokensJSON
	}
	return p.CacheReadInputTokensJS
}

func (p Payload) CacheWriteInputTokens() *int64 {
	if p.CacheWriteInputTokensJSON != nil {
		return p.CacheWriteInputTokensJSON
	}
	return p.CacheWriteInputTokensJS
}

func (p Payload) ReasoningTokens() *int64 {
	if p.ReasoningTokensJSON != nil {
		return p.ReasoningTokensJSON
	}
	return p.ReasoningTokensJS
}

func (p Payload) ResolvedTimestamp() string {
	if len(p.Timestamp) == 0 {
		return time.Now().UTC().Format(time.RFC3339Nano)
	}
	var asString string
	if err := json.Unmarshal(p.Timestamp, &asString); err == nil {
		if parsed, ok := parseTimestampString(asString); ok {
			return parsed
		}
	}
	var asFloat float64
	if err := json.Unmarshal(p.Timestamp, &asFloat); err == nil {
		return time.UnixMilli(int64(asFloat)).UTC().Format(time.RFC3339Nano)
	}
	var asNumber json.Number
	if err := json.Unmarshal(p.Timestamp, &asNumber); err == nil {
		if ms, err := strconv.ParseInt(asNumber.String(), 10, 64); err == nil {
			return time.UnixMilli(ms).UTC().Format(time.RFC3339Nano)
		}
	}
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func parseTimestampString(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	if ms, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return time.UnixMilli(ms).UTC().Format(time.RFC3339Nano), true
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t.UTC().Format(time.RFC3339Nano), true
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.UTC().Format(time.RFC3339Nano), true
	}
	return "", false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func firstNonEmptyRaw(values ...json.RawMessage) json.RawMessage {
	for _, value := range values {
		if len(value) != 0 && strings.TrimSpace(string(value)) != "" && string(value) != "null" {
			return value
		}
	}
	return nil
}
