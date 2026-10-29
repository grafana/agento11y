package state

import "time"

// ToolState stores observation checkpoints separately from generation checkpoints.
// Pending records contain source ranges and structural metadata, never payloads.
type ToolState struct {
	Version            int                     `json:"version,omitempty"`
	Offsets            map[string]int64        `json:"offsets,omitempty"`
	LegacyOffsets      map[string]int64        `json:"legacy_offsets,omitempty"`
	LegacySpawnOffsets map[string]int64        `json:"legacy_spawn_offsets,omitempty"`
	CompletedChildren  map[string]bool         `json:"completed_children,omitempty"`
	Pending            map[string]*PendingTool `json:"pending,omitempty"`
	Spawns             map[string]*ToolSpawn   `json:"spawns,omitempty"`
}

type ToolSource struct {
	Start int64     `json:"start"`
	End   int64     `json:"end"`
	At    time.Time `json:"at"`
}

type ToolResult struct {
	ToolSource
	IsError  bool     `json:"is_error,omitempty"`
	ChildIDs []string `json:"child_ids,omitempty"`
}

type ToolParent struct {
	ID             string `json:"id"`
	ConversationID string `json:"conversation_id"`
	AgentName      string `json:"agent_name,omitempty"`
	AgentVersion   string `json:"agent_version,omitempty"`
	Model          string `json:"model,omitempty"`
	Provider       string `json:"provider,omitempty"`
	Ready          bool   `json:"ready,omitempty"`
	TraceID        string `json:"trace_id,omitempty"`
	SpanID         string `json:"span_id,omitempty"`
	Flags          byte   `json:"flags,omitempty"`
	TraceState     string `json:"trace_state,omitempty"`
	Remote         bool   `json:"remote,omitempty"`
}

type PendingTool struct {
	SessionID string      `json:"session_id"`
	Scope     string      `json:"scope"`
	Path      string      `json:"path"`
	ID        string      `json:"id"`
	Name      string      `json:"name,omitempty"`
	SkillName string      `json:"skill_name,omitempty"`
	Call      *ToolSource `json:"call,omitempty"`
	Result    *ToolResult `json:"result,omitempty"`
	Parent    ToolParent  `json:"parent"`
	Ambiguous bool        `json:"ambiguous,omitempty"`
	Emitted   bool        `json:"emitted,omitempty"`
}

// ToolSpawn survives a late acknowledgement until mapping and child association finish.
// This prevents a background acknowledgement from completing a growing child.
type ToolSpawn struct {
	Path       string `json:"path"`
	Background bool   `json:"background,omitempty"`
	AgentName  string `json:"agent_name,omitempty"`
	ResultEnd  int64  `json:"result_end,omitempty"`
}
