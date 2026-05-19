package hook

import "encoding/json"

type Payload struct {
	HookEventName        string          `json:"hook_event_name"`
	SessionID            string          `json:"session_id"`
	TurnID               string          `json:"turn_id,omitempty"`
	TranscriptPath       string          `json:"transcript_path,omitempty"`
	CWD                  string          `json:"cwd,omitempty"`
	Model                string          `json:"model,omitempty"`
	Source               string          `json:"source,omitempty"`
	Prompt               string          `json:"prompt,omitempty"`
	ToolName             string          `json:"tool_name,omitempty"`
	ToolUseID            string          `json:"tool_use_id,omitempty"`
	ToolInput            json.RawMessage `json:"tool_input,omitempty"`
	ToolResponse         json.RawMessage `json:"tool_response,omitempty"`
	ToolOutput           json.RawMessage `json:"tool_output,omitempty"`
	ToolDurationMs       *float64        `json:"tool_duration_ms,omitempty"`
	DurationMs           *float64        `json:"duration_ms,omitempty"`
	Status               string          `json:"status,omitempty"`
	Error                json.RawMessage `json:"error,omitempty"`
	Timestamp            string          `json:"timestamp,omitempty"`
	StopHookActive       bool            `json:"stop_hook_active,omitempty"`
	LastAssistantMessage *string         `json:"last_assistant_message,omitempty"`
}
