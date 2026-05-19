// Package hook implements the per-event Cursor hook handlers.
//
// All accumulator handlers (sessionStart, beforeSubmit, afterAgentResponse,
// afterAgentThought, postToolUse) are pure local-disk operations: parse the
// payload, then read-modify-write the per-generation fragment file. The
// fragment package does not lock — Cursor dispatches events serially per
// conversation/generation in practice (see fragment package doc). Telemetry
// itself is emitted by stop and sessionEnd, not by these accumulators.
package hook

import (
	"encoding/json"
	"time"
)

// Payload is the union of every Cursor hook payload we handle. Fields outside
// a given event are simply absent at JSON-decode time. We intentionally
// `json.RawMessage` the polymorphic ones (tool_input/output, error) so the
// hook handler can persist them verbatim — the mapper later decides whether
// to surface or strip them based on content-capture mode.
type Payload struct {
	HookEventName     string   `json:"hook_event_name"`
	ConversationID    string   `json:"conversation_id"`
	GenerationID      string   `json:"generation_id"`
	WorkspaceRoots    []string `json:"workspace_roots"`
	UserEmail         string   `json:"user_email"`
	CursorVersion     string   `json:"cursor_version"`
	IsBackgroundAgent bool     `json:"is_background_agent"`
	Timestamp         string   `json:"timestamp"`

	// beforeSubmitPrompt
	Prompt string `json:"prompt"`

	// afterAgentResponse / stop
	Text             string `json:"text"`
	Model            string `json:"model"`
	Provider         string `json:"provider"`
	InputTokens      *int64 `json:"input_tokens"`
	OutputTokens     *int64 `json:"output_tokens"`
	CacheReadTokens  *int64 `json:"cache_read_tokens"`
	CacheWriteTokens *int64 `json:"cache_write_tokens"`

	// postToolUse(Failure)
	ToolName   string          `json:"tool_name"`
	ToolUseID  string          `json:"tool_use_id"`
	ToolInput  json.RawMessage `json:"tool_input"`
	ToolOutput json.RawMessage `json:"tool_output"`
	Duration   *float64        `json:"duration"`
	Cwd        string          `json:"cwd"`
	Status     string          `json:"status"`

	// stop / postToolUseFailure: error is `string | {message,code}`
	Error json.RawMessage `json:"error"`
}

// Timestamp returns the payload timestamp, falling back to the current time
// if missing or unparseable. ISO-8601 string out so the fragment's
// startedAt/lastEventAt fields keep the same wire format across plugins.
func (p Payload) ResolvedTimestamp() string {
	if p.Timestamp != "" {
		return p.Timestamp
	}
	return time.Now().UTC().Format(time.RFC3339Nano)
}
