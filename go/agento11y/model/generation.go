// Package model defines the provider-agnostic generation data model used
// by the agento11y SDK exporter and other producers that need to construct
// generation payloads without depending on the full SDK client.
//
// The types here mirror what the SDK accepts and what the codec package
// translates into the wire-level agento11y v1 protobuf messages.
package model

import (
	"encoding/json"
	"time"
)

// GenerationMode signals whether a generation was a single-shot or streaming
// call. SDK callers should always set one of GenerationModeSync or
// GenerationModeStream.
type GenerationMode string

const (
	GenerationModeSync   GenerationMode = "SYNC"
	GenerationModeStream GenerationMode = "STREAM"
)

// ModelRef identifies the LLM provider and model used for a generation.
type ModelRef struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
}

// ToolDefinition describes a callable tool visible to the model.
type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Type        string          `json:"type,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
	Deferred    bool            `json:"deferred,omitempty"`
}

// Generation is the normalized, provider-agnostic generation payload. It
// can represent both request/response and streaming outcomes.
type Generation struct {
	// ID is the generation identifier. If empty, End assigns one.
	ID                string         `json:"id,omitempty"`
	ConversationID    string         `json:"conversation_id,omitempty"`
	ConversationTitle string         `json:"conversation_title,omitempty"`
	UserID            string         `json:"user_id,omitempty"`
	AgentName         string         `json:"agent_name,omitempty"`
	AgentVersion      string         `json:"agent_version,omitempty"`
	Mode              GenerationMode `json:"mode,omitempty"`
	// OperationName maps to gen_ai.operation.name.
	// Defaults are mode-aware:
	//   - SYNC   -> "generateText"
	//   - STREAM -> "streamText"
	OperationName string `json:"operation_name,omitempty"`
	// TraceID and SpanID identify the OTel span created by StartGeneration or
	// StartStreamingGeneration.
	TraceID             string           `json:"trace_id,omitempty"`
	SpanID              string           `json:"span_id,omitempty"`
	Model               ModelRef         `json:"model"`
	ResponseID          string           `json:"response_id,omitempty"`
	ResponseModel       string           `json:"response_model,omitempty"`
	SystemPrompt        string           `json:"system_prompt,omitempty"`
	Input               []Message        `json:"input,omitempty"`
	Output              []Message        `json:"output,omitempty"`
	Tools               []ToolDefinition `json:"tools,omitempty"`
	MaxTokens           *int64           `json:"max_tokens,omitempty"`
	Temperature         *float64         `json:"temperature,omitempty"`
	TopP                *float64         `json:"top_p,omitempty"`
	ToolChoice          *string          `json:"tool_choice,omitempty"`
	ThinkingEnabled     *bool            `json:"thinking_enabled,omitempty"`
	ParentGenerationIDs []string         `json:"parent_generation_ids,omitempty"`
	EffectiveVersion    string           `json:"effective_version,omitempty"`
	// Usage/StartedAt/CompletedAt are value-type structs where `omitempty` has
	// no effect (gostructs are never "empty" in encoding/json's sense). The tag
	// is intentionally omitted so the JSON shape matches the actual behavior.
	Usage       TokenUsage        `json:"usage"`
	StopReason  string            `json:"stop_reason,omitempty"`
	StartedAt   time.Time         `json:"started_at"`
	CompletedAt time.Time         `json:"completed_at"`
	Tags        map[string]string `json:"tags,omitempty"`
	Metadata    map[string]any    `json:"metadata,omitempty"`
	Artifacts   []Artifact        `json:"artifacts,omitempty"`
	// CallError captures upstream call failure text when End receives callErr.
	CallError string `json:"call_error,omitempty"`
}
