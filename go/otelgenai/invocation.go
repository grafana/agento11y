package otelgenai

import (
	"encoding/json"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Operation is the value of gen_ai.operation.name. A span is named
// "<operation> <subject>", and the operation decides which field is the
// subject:
//
//	execute_tool                      ToolName
//	invoke_agent, create_agent, plan  AgentName
//	retrieval                         DataSourceID
//	invoke_workflow                   WorkflowName
//	fetch_response                    no subject
//	every other operation             RequestModel
//
// Except for fetch_response, an empty operation-specific subject falls back
// to RequestModel. If the subject field and RequestModel are both empty, the
// span name is the operation alone.
type Operation string

const (
	OperationChat            Operation = "chat"
	OperationTextCompletion  Operation = "text_completion"
	OperationGenerateContent Operation = "generate_content"
	OperationEmbeddings      Operation = "embeddings"
	OperationExecuteTool     Operation = "execute_tool"
	OperationInvokeAgent     Operation = "invoke_agent"
	OperationRetrieval       Operation = "retrieval"
	OperationFetchResponse   Operation = "fetch_response"
	OperationInvokeWorkflow  Operation = "invoke_workflow"
	OperationCreateAgent     Operation = "create_agent"
	OperationPlan            Operation = "plan"
)

// Role is the author of a message in the conventions' message schema.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// PartType is the discriminator of a message part in the conventions'
// message schema.
type PartType string

const (
	PartTypeText                   PartType = "text"
	PartTypeReasoning              PartType = "reasoning"
	PartTypeToolCall               PartType = "tool_call"
	PartTypeToolCallResponse       PartType = "tool_call_response"
	PartTypeServerToolCall         PartType = "server_tool_call"
	PartTypeServerToolCallResponse PartType = "server_tool_call_response"
	PartTypeCompaction             PartType = "compaction"
	PartTypeBlob                   PartType = "blob"
	PartTypeFile                   PartType = "file"
	PartTypeURI                    PartType = "uri"
)

// Message is one entry of gen_ai.input.messages or gen_ai.output.messages.
type Message struct {
	Role  Role
	Name  string
	Parts []Part
	// FinishReason belongs on output messages, where the schema requires the
	// key even when the value is empty: there the encoder emits a nil pointer
	// as an empty value. On an input message, a nil pointer omits the key.
	FinishReason *string
}

// Part is one element of a message's parts array. On a part type that
// requires Content or FileID, the encoder emits a nil pointer as an empty
// value. Modality is different: its enum has no empty value, so the encoder
// omits and reports a missing or empty modality. Optional pointer fields are
// omitted when nil.
//
// Type selects which fields the encoder writes: a text part carries Content
// and nothing else, whatever the other fields hold. A type the schema does
// not name is encoded as the schema's generic part, which carries the type
// and the extensions and no other field. A part with no type at all is
// dropped, because the schema requires one.
type Part struct {
	Type PartType
	// Content holds the text of a text, reasoning, blob, or compaction part.
	Content *string
	// ID identifies tool calls, tool call responses, server tool calls, server
	// tool call responses, and compaction parts.
	ID string
	// Name is the tool name on a tool_call or server_tool_call part.
	Name string
	// Arguments carries tool_call and server_tool_call payloads. Response carries
	// tool_call_response and server_tool_call_response payloads. Both must be
	// compact, HTML-escaped JSON, which is what json.Marshal produces. The
	// enclosing marshal rewrites anything else, so it would not survive a
	// round trip.
	Arguments json.RawMessage
	Response  json.RawMessage

	MimeType string
	// Modality is required for blob, file, and URI parts. The encoder omits and
	// reports a nil or empty value because the schema enum has no empty member.
	Modality *string
	URI      string
	FileID   *string

	// Extensions are vendor keys merged into the encoded part object, sorted
	// by key. An end hook uses them to carry data the conventions have
	// no field for. Namespace them: a key the schema already uses is dropped
	// rather than emitted twice.
	Extensions map[string]any
}

// TextPart returns a text part carrying content.
func TextPart(content string) Part {
	return Part{Type: PartTypeText, Content: &content}
}

// ToolDefinition is one entry of gen_ai.tool.definitions.
type ToolDefinition struct {
	Type        string
	Name        string
	Description string
	// Parameters carries the tool's JSON schema raw, under the same
	// constraint as Part.Arguments.
	Parameters json.RawMessage
	// Extensions are vendor keys merged into the encoded tool object, under
	// the same constraint as Part.Extensions.
	Extensions map[string]any
}

// Usage holds the token counts of one invocation. Every field maps to a
// gen_ai.usage.* attribute; zero values are omitted except for input and
// output tokens, which are always emitted when the usage counts as reported.
// The handler leaves unreported usage off the span and out of
// gen_ai.client.token.usage, so counts a provider did not return are never
// summed as zeros.
type Usage struct {
	// Reported marks that the provider returned usage. Set it for an
	// all-zero usage the provider really did return; any non-zero count
	// already counts as reported without it.
	Reported             bool
	InputTokens          int64
	OutputTokens         int64
	CacheReadInputTokens int64
	// CacheWriteInputTokens maps to gen_ai.usage.cache_creation.input_tokens.
	CacheWriteInputTokens int64
	ReasoningTokens       int64
}

// reported reports whether the invocation carries usage data. A non-zero
// count counts on its own; Reported is needed only for the all-zero usage a
// provider did return.
func (u Usage) reported() bool {
	return u.Reported ||
		u.InputTokens != 0 ||
		u.OutputTokens != 0 ||
		u.CacheReadInputTokens != 0 ||
		u.CacheWriteInputTokens != 0 ||
		u.ReasoningTokens != 0
}

// Invocation is one GenAI client call: everything the instrumentation knows
// about the request and its outcome. A caller fills the request fields before
// Start, the response fields before End, and never touches the unexported
// span handle.
type Invocation struct {
	Operation Operation
	// Kind overrides the span kind implied by Operation. With the zero value,
	// trace.SpanKindUnspecified, the handler derives the kind: execute_tool,
	// invoke_workflow, and plan are INTERNAL; every other operation is CLIENT.
	// Set Kind before Start. Changing it later does not change the open span,
	// although End can update the span name from other Invocation fields. An
	// in-process agent can set Kind to trace.SpanKindInternal when its operation
	// would otherwise use the conventions' CLIENT default.
	Kind             trace.SpanKind
	Provider         string
	RequestModel     string
	ResponseModel    string
	ResponseID       string
	ResponseStatus   string
	ConversationID   string
	AgentName        string
	AgentVersion     string
	AgentID          string
	AgentDescription string
	DataSourceID     string
	WorkflowName     string
	// Stream marks a streaming call. It maps to gen_ai.request.stream and
	// enables the first-chunk span attribute and End's metric fallback.
	// RecordChunk sets it to true.
	Stream       bool
	StreamCursor string

	// ToolName, ToolCallID, ToolType, and ToolDescription describe the tool an
	// execute_tool invocation runs. ToolName is the subject of that operation's
	// span name.
	ToolName        string
	ToolCallID      string
	ToolType        string
	ToolDescription string
	SkillName       string

	// ServerAddress and ServerPort locate the provider endpoint. The
	// conventions treat them as sampling-relevant, so set them before Start.
	ServerAddress string
	ServerPort    int

	SystemInstructions []Part
	InputMessages      []Message
	OutputMessages     []Message
	ToolDefinitions    []ToolDefinition

	ToolCallArguments  json.RawMessage
	ToolCallResult     json.RawMessage
	RetrievalQueryText string
	RetrievalDocuments json.RawMessage

	FinishReasons []string
	Usage         Usage

	MaxTokens        *int64
	Temperature      *float64
	TopP             *float64
	TopK             *int64
	FrequencyPenalty *float64
	PresencePenalty  *float64
	StopSequences    []string
	Seed             *int64
	ChoiceCount      *int64
	OutputType       string
	EncodingFormats  []string
	DimensionCount   *int64

	StartedAt   time.Time
	CompletedAt time.Time
	// FirstChunkAt is when the first streamed chunk arrived. A zero value
	// means the handler timed no chunk and records no time-to-first-chunk
	// value.
	FirstChunkAt time.Time

	// ErrorType is the low-cardinality error.type classification, and
	// ErrorMessage is the free-form text that rides on the span status.
	ErrorType    string
	ErrorMessage string

	// Capture overrides the handler's content capture mode for this
	// invocation. The zero value, CaptureUnset, keeps the handler's mode.
	// ParseCaptureMode normalizes a non-zero value. An unrecognized value is
	// reported to the OTel error handler and falls back to the handler's mode.
	// End resolves it before the end hooks run, so a hook that assigns
	// to it changes nothing; a hook withholds content by clearing the
	// messages instead.
	Capture CaptureMode

	// Attributes are extra span attributes the caller attaches to this
	// invocation. They are emitted after the semantic-convention attributes,
	// so a caller can override one it disagrees with.
	Attributes []attribute.KeyValue

	// MetricAttributes are extra dimensions for this invocation's metrics.
	// They land on every instrument the invocation records, so keep them
	// low-cardinality.
	MetricAttributes []attribute.KeyValue

	// Vendor carries instrumentation-specific data for end hooks. This
	// package never reads it.
	Vendor any

	span trace.Span
	// spanStartedAt is the timestamp the span opened with. Metrics
	// derive from it rather than from StartedAt, which a caller may rewrite
	// between Start and End.
	spanStartedAt time.Time
	lastChunkAt   time.Time
	ttfcRecorded  bool
	// ended marks that End ran, so a second call records nothing.
	ended bool
}

// operation returns the invocation's operation name, defaulting to chat.
func (inv *Invocation) operation() Operation {
	if inv.Operation == "" {
		return OperationChat
	}
	return inv.Operation
}

// spanName is the conventions' span name, "<operation> <subject>". The
// Operation doc lists the subject field of each operation, including the
// fallbacks and the fetch_response exception.
func (inv *Invocation) spanName() string {
	op := inv.operation()
	var subject string
	switch op {
	case OperationExecuteTool:
		subject = inv.ToolName
	case OperationInvokeAgent, OperationCreateAgent, OperationPlan:
		subject = inv.AgentName
	case OperationRetrieval:
		subject = inv.DataSourceID
	case OperationInvokeWorkflow:
		subject = inv.WorkflowName
	case OperationFetchResponse:
		return string(op)
	default:
	}
	if subject == "" {
		subject = inv.RequestModel
	}
	if subject == "" {
		return string(op)
	}
	return string(op) + " " + subject
}

// spanKind returns an explicit Kind when set. Otherwise, in-process
// operations are INTERNAL and the remaining operations are CLIENT.
func (inv *Invocation) spanKind() trace.SpanKind {
	if inv.Kind != trace.SpanKindUnspecified {
		return inv.Kind
	}
	switch inv.operation() {
	case OperationExecuteTool, OperationInvokeWorkflow, OperationPlan:
		return trace.SpanKindInternal
	default:
		return trace.SpanKindClient
	}
}

// errorType is the error.type value for a failed invocation. The conventions
// require the attribute whenever the operation ended in an error, so a
// failure the caller described only in prose still classifies as _OTHER.
func (inv *Invocation) errorType() string {
	if inv.ErrorType != "" {
		return inv.ErrorType
	}
	if inv.ErrorMessage != "" {
		return errorTypeOther
	}
	return ""
}

// errorTypeOther is the conventions' fallback classification.
const errorTypeOther = "_OTHER"

// startTime is the instant the span opened, which is what the metrics measure
// from. It falls back to StartedAt when End runs without Start.
func (inv *Invocation) startTime() time.Time {
	if !inv.spanStartedAt.IsZero() {
		return inv.spanStartedAt
	}
	return inv.StartedAt
}

// duration is the wall-clock length of the invocation in seconds. It reports
// false when either timestamp is missing, and clamps a completion that
// precedes the start to zero so the sample still lands on the instrument.
func (inv *Invocation) duration() (float64, bool) {
	start := inv.startTime()
	if start.IsZero() || inv.CompletedAt.IsZero() {
		return 0, false
	}
	seconds := inv.CompletedAt.Sub(start).Seconds()
	if seconds < 0 {
		return 0, true
	}
	return seconds, true
}

// timeToFirstChunk is the delay before the first streamed chunk, in seconds.
// It reports false for non-streaming calls and calls with no timed chunk. A
// first-chunk timestamp before the start is clamped to zero.
func (inv *Invocation) timeToFirstChunk() (float64, bool) {
	start := inv.startTime()
	if !inv.Stream || inv.FirstChunkAt.IsZero() || start.IsZero() {
		return 0, false
	}
	seconds := inv.FirstChunkAt.Sub(start).Seconds()
	if seconds < 0 {
		return 0, true
	}
	return seconds, true
}
