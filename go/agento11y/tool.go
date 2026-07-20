package agento11y

import "time"

// ToolExecutionStart seeds a tool execution span before the tool call runs.
type ToolExecutionStart struct {
	ToolName          string
	ToolCallID        string
	ToolType          string
	ToolDescription   string
	ConversationID    string
	ConversationTitle string
	AgentName         string
	AgentVersion      string
	// RequestModel is the model that requested the tool call (e.g. "gpt-5").
	RequestModel string
	// RequestProvider is the provider that served the model (e.g. "openai").
	RequestProvider string
	StartedAt       time.Time
	// IncludeContent enables gen_ai.tool.call.arguments and gen_ai.tool.call.result attributes.
	// Deprecated: Use ContentCapture instead. ContentCapture takes precedence
	// when set to a non-Default value. IncludeContent is only honored when
	// the resolved mode is Full.
	IncludeContent bool
	// ContentCapture overrides the parent generation's content capture mode for
	// this tool execution. Default (zero value) inherits from context.
	ContentCapture ContentCaptureMode
}

// ToolExecutionEnd finalizes tool execution span attributes.
type ToolExecutionEnd struct {
	Arguments   any
	Result      any
	CompletedAt time.Time
}
