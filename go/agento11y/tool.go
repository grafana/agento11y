package agento11y

import "time"

type ToolExecutionKind string

const (
	ToolExecutionKindLoadSkill         ToolExecutionKind = "load_skill"
	ToolExecutionKindReadSkillResource ToolExecutionKind = "read_skill_resource"
	ToolExecutionKindCommand           ToolExecutionKind = "command"
)

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
	// Tags are dimensions of this one tool execution. They are emitted on the
	// tool span and on its execute_tool metrics as agento11y.tag.<key>, next to
	// the static Config.Tags, and win over a static tag with the same key.
	//
	// Context tags (WithTag, WithTags) are not applied to tool executions:
	// every tag here becomes a label on a histogram that is already split by
	// tool name, so a caller names the few dimensions it wants explicitly.
	// Keep the values low-cardinality.
	Tags                  map[string]string
	ExecutionKind         ToolExecutionKind
	SkillName             string
	SkillDescription      string
	SkillSourceURI        string
	SkillResourceName     string
	ProcessExecutableName string
	ProcessExecutablePath string
}

// ToolExecutionEnd finalizes tool execution span attributes.
type ToolExecutionEnd struct {
	Arguments       any
	Result          any
	CompletedAt     time.Time
	ProcessExitCode *int64
}
