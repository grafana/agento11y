package sigilmodel

import "time"

// WorkflowStep describes one execution node in an agentic workflow.
//
// A step can represent non-LLM work such as routing, retrieval, validation, or
// tool orchestration, and can link to the generations that ran inside it.
type WorkflowStep struct {
	ID                  string            `json:"id,omitempty"`
	ConversationID      string            `json:"conversation_id,omitempty"`
	StepName            string            `json:"step_name,omitempty"`
	Framework           string            `json:"framework,omitempty"`
	StartedAt           time.Time         `json:"started_at"`
	CompletedAt         time.Time         `json:"completed_at"`
	InputState          map[string]any    `json:"input_state,omitempty"`
	OutputState         map[string]any    `json:"output_state,omitempty"`
	Error               string            `json:"error,omitempty"`
	Tags                map[string]string `json:"tags,omitempty"`
	LinkedGenerationIDs []string          `json:"linked_generation_ids,omitempty"`
	ParentStepIDs       []string          `json:"parent_step_ids,omitempty"`
	AgentName           string            `json:"agent_name,omitempty"`
	AgentVersion        string            `json:"agent_version,omitempty"`
	TraceID             string            `json:"trace_id,omitempty"`
	SpanID              string            `json:"span_id,omitempty"`
	Metadata            map[string]any    `json:"metadata,omitempty"`
}
