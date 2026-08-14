// Package contentcapture owns the metadata_only content-capture policy: which
// generation fields carry user content, which OTel span attribute keys carry
// user content, and the protocol values that replace removed content.
//
// The policy is one algorithm, Strip, written against the Target interfaces
// instead of a concrete generation type. Content-bearing generations exist in
// two shapes. The SDK reduces the public agento11y.Generation struct before
// encoding it. A forwarder such as the local daemon never runs through that
// path: it launches the agent with a full content-capture mode so the local
// viewer keeps everything, and reduces the copy it sends on at forward time,
// when all it holds is the wire proto. Each shape supplies a small adapter,
// while the field list, the traversal, the metadata keys, and the
// error-category rule live here. StripGeneration is the entry point for the
// proto shape.
//
// Adding a content-bearing field means adding a method to Target or
// PartTarget, which does not compile until every adapter handles it.
//
// Strip does not stamp model.MetadataKeyContentCaptureMode. The SDK stamps
// every mode before reducing, while a forwarder has to overwrite an incoming
// "full" stamp, so the relabel stays with the caller.
package contentcapture

import (
	agento11yv1 "github.com/grafana/agento11y/go/proto/agento11y/v1"
)

// Keys whose values carry user content. IsTraceContentAttribute returns true
// for every key in this block and for nothing else;
// TestContentKeyBlockMatchesPredicate reads this file and fails if the two
// disagree, because adding a key here and forgetting the predicate compiles
// and leaves a content attribute relayed under a reduced mode.
//
// A key that carries no content belongs in the block below.
//
//contentcapture:content-keys
const (
	// ConversationTitleKey is the generation metadata key, and the span
	// attribute key, that carries the conversation title. The proto
	// Generation has no conversation_title field, so an SDK title reaches the
	// wire only through this metadata mirror and the strip deletes the key
	// rather than clearing a field.
	ConversationTitleKey = "agento11y.conversation.title"

	// LegacyConversationTitleKey is the pre-rename spelling of
	// ConversationTitleKey. Nothing in this module writes it, but an older
	// installed exporter (@grafana/agento11y-pi, @grafana/agento11y-opencode,
	// or a sigil-era SDK) can still send it to a current forwarder, and a
	// caller can put it in user metadata, so both spellings are declared here
	// and both are stripped.
	LegacyConversationTitleKey = "sigil.conversation.title"

	// EmbeddingInputTextsAttributeKey holds the raw embedding input texts.
	EmbeddingInputTextsAttributeKey = "gen_ai.embeddings.input_texts"

	// ToolDescriptionAttributeKey holds a tool's description text.
	ToolDescriptionAttributeKey = "gen_ai.tool.description"

	// ToolCallArgumentsAttributeKey holds the arguments a tool was called with.
	ToolCallArgumentsAttributeKey = "gen_ai.tool.call.arguments"

	// ToolCallResultAttributeKey holds the value a tool call returned.
	ToolCallResultAttributeKey = "gen_ai.tool.call.result"

	// The four gen_ai keys below are the semantic conventions' content
	// documents. A generation span carries them only under the experimental
	// otel export protocol, where the span is the generation export rather than
	// metadata beside it. Every other mode leaves them absent. Listing them
	// here costs a forwarder nothing and closes the one path where prompt text
	// can reach a span.

	// SystemInstructionsAttributeKey holds the system prompt.
	SystemInstructionsAttributeKey = "gen_ai.system_instructions"

	// InputMessagesAttributeKey holds the request messages.
	InputMessagesAttributeKey = "gen_ai.input.messages"

	// OutputMessagesAttributeKey holds the response messages.
	OutputMessagesAttributeKey = "gen_ai.output.messages"

	// ToolDefinitionsAttributeKey holds the tool descriptions and schemas
	// visible to the model.
	ToolDefinitionsAttributeKey = "gen_ai.tool.definitions"

	// RawArtifactsAttributeKey holds the raw provider request and response
	// payloads. Only the otel export protocol puts them on a span.
	RawArtifactsAttributeKey = "agento11y.generation.raw_artifacts"
)

// Protocol values that must not be treated as content. A forwarder that
// deleted the error category or the stripped-error marker would drop the last
// signal that a call failed, so these keys and values stay out of the block
// above even though they sit next to redacted content.
//
//contentcapture:non-content-values
const (
	// ExceptionEventName is the span event name the OTel SDK's RecordError
	// emits. Its exception.message and exception.stacktrace attributes can
	// carry raw provider text, so a reduced forward drops the whole event.
	ExceptionEventName = "exception"

	// ErrorCategoryAttributeKey is the span attribute holding the classified
	// error category. The category carries no content, so it is the
	// replacement for a redacted error status message.
	ErrorCategoryAttributeKey = "error.category"

	// MetadataKeyCallError is the metadata key mirroring
	// Generation.call_error, which the SDK writes alongside the field. The key
	// is not a content attribute, but its value is raw provider text, so the
	// strip deletes the mirror and replaces the field.
	MetadataKeyCallError = "call_error"

	// StrippedCallError replaces a generation's raw call error when no
	// classified category is available.
	StrippedCallError = "sdk_error"
)

// IsTraceContentAttribute reports whether an OTel span attribute key carries
// user content and therefore must not leave the host under a reduced
// content-capture mode. Its cases are exactly the content-key block above.
// Exposing a predicate instead of a set keeps callers from adding or removing
// keys.
//
// Generation prompt and response text reaches a span only under the
// experimental otel export protocol, where the span replaces the proto
// generation export. On every other path that text travels in the proto
// generation export.
func IsTraceContentAttribute(key string) bool {
	switch key {
	case ConversationTitleKey,
		LegacyConversationTitleKey,
		EmbeddingInputTextsAttributeKey,
		ToolDescriptionAttributeKey,
		ToolCallArgumentsAttributeKey,
		ToolCallResultAttributeKey,
		SystemInstructionsAttributeKey,
		InputMessagesAttributeKey,
		OutputMessagesAttributeKey,
		ToolDefinitionsAttributeKey,
		RawArtifactsAttributeKey:
		return true
	default:
		return false
	}
}

// Target is one generation shape Strip can reduce. An implementation adapts a
// concrete generation type and holds no policy of its own, so a method that
// has no meaning for a shape is a documented no-op rather than a branch in
// Strip.
type Target interface {
	// ClearSystemPrompt clears the system prompt.
	ClearSystemPrompt()

	// ClearArtifacts drops the raw request and response artifacts.
	ClearArtifacts()

	// ClearConversationTitle clears a conversation title held in a field. The
	// wire proto has no such field: a title reaches it only through the
	// ConversationTitleKey metadata mirror, which Strip deletes on its own, so
	// the proto shape implements this as a no-op.
	ClearConversationTitle()

	// CallError returns the current call error. Strip reads before writing
	// because an absent call error has to stay absent.
	CallError() string

	// SetCallError replaces the call error with a value that carries no
	// content.
	SetCallError(callError string)

	// DeleteMetadata removes one metadata key, and does nothing when the key
	// is absent.
	DeleteMetadata(key string)

	// NormalizeMetadata runs after the metadata deletions. It reconciles an
	// emptied metadata container with the way the encoder represents an empty
	// one, so a generation reduced in either shape encodes to the same bytes.
	NormalizeMetadata()

	// EachPart calls fn for every part of every input and output message. No
	// message-level field carries content, so Strip never handles a message
	// itself.
	EachPart(fn func(PartTarget))

	// EachTool calls fn for every tool definition.
	EachTool(fn func(ToolTarget))
}

// PartTarget is one message part. Strip calls every method on every part: a
// part holds one kind of payload, and the calls that do not apply to it are
// no-ops. A new content-bearing payload kind belongs here.
type PartTarget interface {
	// ClearText clears message text.
	ClearText()

	// ClearThinking clears reasoning text.
	ClearThinking()

	// ClearToolCallInput clears the arguments a tool was called with.
	ClearToolCallInput()

	// ClearToolResult clears both the text and the JSON form of a tool result.
	ClearToolResult()

	// ClearMediaURL clears a media URL, which can hold the bytes inline as a
	// data: URI.
	ClearMediaURL()
}

// ToolTarget is one tool definition.
type ToolTarget interface {
	// ClearDescription clears the description text.
	ClearDescription()

	// ClearInputSchema clears the input schema, whose property names and
	// descriptions are authored text.
	ClearInputSchema()
}

// Strip reduces target to metadata_only: it clears the system prompt, raw
// artifacts, per-part text, thinking, tool arguments, tool results and media
// payloads, tool descriptions and schemas, and the content-bearing metadata
// mirrors. Part and message structure, roles, tool names and IDs, usage,
// timing, tags, and all other metadata survive.
//
// errorCategory replaces a non-empty call error. Pass the classified category
// ("rate_limit", "timeout") when one is available, or "" to fall back to
// StrippedCallError. An absent call error stays absent.
func Strip(target Target, errorCategory string) {
	target.ClearSystemPrompt()
	target.ClearArtifacts()

	if target.CallError() != "" {
		if errorCategory != "" {
			target.SetCallError(errorCategory)
		} else {
			target.SetCallError(StrippedCallError)
		}
	}
	target.DeleteMetadata(MetadataKeyCallError)

	target.ClearConversationTitle()
	target.DeleteMetadata(ConversationTitleKey)
	target.DeleteMetadata(LegacyConversationTitleKey)

	target.EachPart(func(part PartTarget) {
		part.ClearText()
		part.ClearThinking()
		part.ClearToolCallInput()
		part.ClearToolResult()
		part.ClearMediaURL()
	})

	target.EachTool(func(tool ToolTarget) {
		tool.ClearDescription()
		tool.ClearInputSchema()
	})

	target.NormalizeMetadata()
}

// StripGeneration reduces a wire generation to metadata_only in place. See
// Strip for what it clears and for the errorCategory argument.
//
// A caller that stamps the content-capture mode afterwards has to handle an
// unset Metadata, because the strip leaves it unset when content keys were all
// it held. A write through g.GetMetadata().GetFields() would assign to a nil
// map.
//
// It is safe to call with a nil generation.
func StripGeneration(g *agento11yv1.Generation, errorCategory string) {
	if g == nil {
		return
	}
	Strip(protoGeneration{g: g}, errorCategory)
}

// protoGeneration adapts the wire Generation to Target.
type protoGeneration struct {
	g *agento11yv1.Generation
}

func (t protoGeneration) ClearSystemPrompt() { t.g.SystemPrompt = "" }

func (t protoGeneration) ClearArtifacts() { t.g.RawArtifacts = nil }

// ClearConversationTitle is a no-op on the wire shape: the title lives only in
// metadata. See Target.
func (t protoGeneration) ClearConversationTitle() {}

func (t protoGeneration) CallError() string { return t.g.GetCallError() }

func (t protoGeneration) SetCallError(callError string) { t.g.CallError = callError }

// DeleteMetadata needs no guard for an unset Struct: delete on a nil map is a
// no-op, and NormalizeMetadata handles the empty case.
func (t protoGeneration) DeleteMetadata(key string) {
	delete(t.g.GetMetadata().GetFields(), key)
}

func (t protoGeneration) NormalizeMetadata() {
	if len(t.g.GetMetadata().GetFields()) == 0 {
		// codec.ToProto encodes an empty metadata map as an unset Struct, so
		// clearing an emptied Struct keeps this shape's output identical to
		// the struct shape's encoding. A forwarder decoding an incoming
		// "metadata": {} from another exporter gets the same normalization.
		t.g.Metadata = nil
	}
}

func (t protoGeneration) EachPart(fn func(PartTarget)) {
	for _, message := range t.g.GetInput() {
		eachProtoPart(message, fn)
	}
	for _, message := range t.g.GetOutput() {
		eachProtoPart(message, fn)
	}
}

func (t protoGeneration) EachTool(fn func(ToolTarget)) {
	for _, tool := range t.g.GetTools() {
		if tool == nil {
			continue
		}
		fn(protoTool{tool: tool})
	}
}

// eachProtoPart visits the parts of one message. GetParts on a nil message
// returns nil, so a nil message needs no guard of its own.
func eachProtoPart(message *agento11yv1.Message, fn func(PartTarget)) {
	for _, part := range message.GetParts() {
		if part == nil {
			continue
		}
		fn(protoPart{part: part})
	}
}

// protoPart adapts one wire Part to PartTarget. The payload is a oneof, so
// each method checks that the part holds the kind it clears.
//
// A typed-nil oneof wrapper is a non-nil interface holding a nil pointer, so
// it satisfies its type assertion and each method has to check the wrapper
// before writing through it. No decoder produces one; the guards keep a
// hand-built Part from panicking here.
type protoPart struct {
	part *agento11yv1.Part
}

func (t protoPart) ClearText() {
	if payload, ok := t.part.GetPayload().(*agento11yv1.Part_Text); ok && payload != nil {
		payload.Text = ""
	}
}

func (t protoPart) ClearThinking() {
	if payload, ok := t.part.GetPayload().(*agento11yv1.Part_Thinking); ok && payload != nil {
		payload.Thinking = ""
	}
}

func (t protoPart) ClearToolCallInput() {
	payload, ok := t.part.GetPayload().(*agento11yv1.Part_ToolCall)
	if ok && payload != nil && payload.ToolCall != nil {
		payload.ToolCall.InputJson = nil
	}
}

func (t protoPart) ClearToolResult() {
	payload, ok := t.part.GetPayload().(*agento11yv1.Part_ToolResult)
	if ok && payload != nil && payload.ToolResult != nil {
		payload.ToolResult.Content = ""
		payload.ToolResult.ContentJson = nil
	}
}

func (t protoPart) ClearMediaURL() {
	payload, ok := t.part.GetPayload().(*agento11yv1.Part_Media)
	if ok && payload != nil && payload.Media != nil {
		// Media.url can hold a data: URI with the file bytes inline
		// (go/agento11y/codec). The kind, mime type, and name around it are
		// references and stay.
		payload.Media.Url = ""
	}
}

// protoTool adapts one wire ToolDefinition to ToolTarget.
type protoTool struct {
	tool *agento11yv1.ToolDefinition
}

func (t protoTool) ClearDescription() { t.tool.Description = "" }

func (t protoTool) ClearInputSchema() { t.tool.InputSchemaJson = nil }
