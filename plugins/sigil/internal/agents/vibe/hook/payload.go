// Package hook defines the JSON shapes vibe writes to a hook command's stdin
// and the handlers that act on them. See vibe/core/hooks/models.py upstream
// (mistral-vibe) for the source of truth.
package hook

import (
	"encoding/json"
	"strings"
)

// Payload is the union of fields vibe writes to a hook command's stdin
// across the three event types it fires. post_agent_turn carries only the
// session-locating fields; before_tool adds the tool name/call-id/input;
// after_tool adds the tool outcome (status, output, error, duration).
//
// The base payload is intentionally thin: it carries enough to locate the
// session's on-disk transcript (messages.jsonl) plus meta.json, and the
// post_agent_turn handler reads the actual turn content from there.
type Payload struct {
	HookEventName   string `json:"hook_event_name"`
	SessionID       string `json:"session_id"`
	TranscriptPath  string `json:"transcript_path"`
	CWD             string `json:"cwd,omitempty"`
	ParentSessionID string `json:"parent_session_id,omitempty"`

	// before_tool / after_tool fields. ToolInput is the validated argument
	// object; ToolError is the failure detail (string or object) populated
	// on a non-success status.
	ToolNameValue   string          `json:"tool_name,omitempty"`
	ToolCallIDValue string          `json:"tool_call_id,omitempty"`
	ToolInputValue  json.RawMessage `json:"tool_input,omitempty"`
	ToolStatusValue string          `json:"tool_status,omitempty"`
	ToolErrorValue  json.RawMessage `json:"tool_error,omitempty"`
	DurationMsValue *float64        `json:"duration_ms,omitempty"`
}

// ToolName returns the tool a before_tool/after_tool event refers to.
func (p Payload) ToolName() string { return strings.TrimSpace(p.ToolNameValue) }

// ToolCallID correlates the tool call across before_tool, after_tool, and the
// transcript so spans and events line up.
func (p Payload) ToolCallID() string { return strings.TrimSpace(p.ToolCallIDValue) }

// ToolInput is the raw JSON argument object for guard evaluation. Returns nil
// when absent or explicitly null.
func (p Payload) ToolInput() json.RawMessage {
	if len(p.ToolInputValue) == 0 || string(p.ToolInputValue) == "null" {
		return nil
	}
	return p.ToolInputValue
}

// ToolStatus is vibe's after_tool outcome: "success", "failure", or
// "cancelled".
func (p Payload) ToolStatus() string { return strings.TrimSpace(p.ToolStatusValue) }

// DurationMs is the measured tool-body duration from after_tool, or 0 when
// absent.
func (p Payload) DurationMs() float64 {
	if p.DurationMsValue == nil {
		return 0
	}
	return *p.DurationMsValue
}

// ToolErrorText renders the after_tool error as a plain string, unwrapping a
// JSON string and falling back to the raw JSON for object/array errors.
func (p Payload) ToolErrorText() string {
	if len(p.ToolErrorValue) == 0 || string(p.ToolErrorValue) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(p.ToolErrorValue, &s); err == nil {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(string(p.ToolErrorValue))
}
