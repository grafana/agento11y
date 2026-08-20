package agento11y

import (
	"context"
	"fmt"
	"strings"

	"github.com/grafana/agento11y/go/agento11y/contentcapture"
	"github.com/grafana/agento11y/go/agento11y/model"
)

// ContentCaptureMode controls what content is included in exported generation
// payloads and OTel span attributes.
type ContentCaptureMode int

const (
	// ContentCaptureModeDefault uses the parent or client-level default.
	// On Config this resolves to NoToolContent for backward compatibility.
	// On GenerationStart this inherits from Config.
	// On ToolExecutionStart this inherits from the parent generation context,
	// falling back to Config.
	ContentCaptureModeDefault ContentCaptureMode = iota
	// ContentCaptureModeFull exports all content.
	ContentCaptureModeFull
	// ContentCaptureModeNoToolContent exports full generation content but
	// excludes tool execution content (arguments and results) from span
	// attributes unless explicitly opted in via IncludeContent or a per-tool
	// ContentCapture override. This matches the pre-ContentCaptureMode SDK
	// default behavior.
	ContentCaptureModeNoToolContent
	// ContentCaptureModeMetadataOnly preserves message structure, tool names,
	// usage, and timing but strips text, tool arguments, tool results,
	// thinking, system prompts, conversation titles, and raw artifacts.
	//
	// Note: user-provided Metadata and Tags are NOT stripped — callers are
	// responsible for ensuring these maps do not contain sensitive content
	// when using MetadataOnly mode. The exception is the metadata keys the SDK
	// itself mirrors content into. The call error and the conversation title,
	// under both the current and the pre-rename spelling, are removed no matter
	// who wrote them.
	ContentCaptureModeMetadataOnly
	// ContentCaptureModeFullWithMetadataSpans splits the generation-export and
	// span paths for generation content. Use this mode when the generation-export
	// destination is private but the OTel traces/metrics destination is shared and
	// must not receive any content. On a client with the OTel generation-export
	// protocol and its experimental gate enabled, this mode resolves to Full.
	//
	// Per-entity behaviour on the gRPC and HTTP generation-export protocols:
	//   - Generation: full content goes to the generation export; the generation
	//     span omits agento11y.conversation.title.
	//   - ToolExecution: there is no separate generation export. The tool execution
	//     span omits gen_ai.tool.call.arguments, gen_ai.tool.call.result, and
	//     agento11y.conversation.title. Equivalent to MetadataOnly for tool spans.
	//   - Embedding: there is no separate generation export. The embedding span
	//     omits gen_ai.embeddings.input_texts. Equivalent to MetadataOnly for
	//     embedding spans.
	//   - Rating: the Rating.Comment field is preserved; only MetadataOnly
	//     strips it.
	//
	// EmbeddingStart has no per-call ContentCapture field; embedding input
	// text capture is gated by EmbeddingCaptureConfig.CaptureInput and the
	// effective client mode from ContentCaptureResolver and Config.ContentCapture.
	ContentCaptureModeFullWithMetadataSpans
)

const (
	// Pinned to the model package so the shared validator and SDK stripping logic stay
	// in lockstep.
	metadataKeyContentCaptureMode = model.MetadataKeyContentCaptureMode
	// Pinned to the contentcapture package. See its package doc for why the
	// policy lives there.
	metadataKeyCallError                         = contentcapture.MetadataKeyCallError
	contentCaptureModeValueMetaOnly              = model.ContentCaptureModeMetadataOnly
	contentCaptureModeValueFull                  = "full"
	contentCaptureModeValueNoToolContent         = "no_tool_content"
	contentCaptureModeValueFullWithMetadataSpans = "full_with_metadata_spans"
)

// resolveContentCaptureMode returns the effective mode from an override and a
// fallback. Default is transparent — it falls through to the fallback.
func resolveContentCaptureMode(override, fallback ContentCaptureMode) ContentCaptureMode {
	if override != ContentCaptureModeDefault {
		return override
	}
	return fallback
}

// callContentCaptureResolver invokes the resolver callback safely, recovering
// from panics. Returns ContentCaptureModeDefault when the resolver is nil.
// Panics are treated as ContentCaptureModeMetadataOnly (fail-closed).
func callContentCaptureResolver(ctx context.Context, resolver func(ctx context.Context, metadata map[string]any) ContentCaptureMode, metadata map[string]any) (mode ContentCaptureMode) {
	if resolver == nil {
		return ContentCaptureModeDefault
	}
	defer func() {
		if r := recover(); r != nil {
			mode = ContentCaptureModeMetadataOnly
		}
	}()
	return resolver(ctx, metadata)
}

// resolveClientContentCaptureMode resolves the effective mode for the client.
// Default at the client level means NoToolContent (backward compatibility):
// generation content is always captured, but tool content requires explicit
// opt-in via IncludeContent or ContentCapture on ToolExecutionStart.
func resolveClientContentCaptureMode(mode ContentCaptureMode) ContentCaptureMode {
	if mode == ContentCaptureModeDefault {
		return ContentCaptureModeNoToolContent
	}
	return mode
}

// stampContentCaptureMetadata sets the content capture mode marker on the generation.
func stampContentCaptureMetadata(g *Generation, mode ContentCaptureMode) {
	if g.Metadata == nil {
		g.Metadata = map[string]any{}
	}
	g.Metadata[metadataKeyContentCaptureMode] = mode.String()
}

// stripContent removes sensitive content from a generation while preserving
// message structure (roles, part kinds), tool names/IDs, usage, timing, and
// all other metadata fields. errorCategory is the classified error category
// (e.g. "rate_limit", "timeout") used to replace the raw CallError text.
//
// What counts as content lives in agento11y/contentcapture, shared with the
// wire-proto shape a forwarder reduces.
func stripContent(g *Generation, errorCategory string) {
	contentcapture.Strip(generationTarget{g: g}, errorCategory)
}

// generationTarget adapts the public Generation struct to
// contentcapture.Target.
type generationTarget struct {
	g *Generation
}

func (t generationTarget) ClearSystemPrompt() { t.g.SystemPrompt = "" }

func (t generationTarget) ClearArtifacts() { t.g.Artifacts = nil }

func (t generationTarget) ClearConversationTitle() { t.g.ConversationTitle = "" }

func (t generationTarget) CallError() string { return t.g.CallError }

func (t generationTarget) SetCallError(callError string) { t.g.CallError = callError }

func (t generationTarget) DeleteMetadata(key string) { delete(t.g.Metadata, key) }

// NormalizeMetadata is a no-op on this shape: codec.ToProto encodes an emptied
// metadata map as an unset Struct, so there is nothing to reconcile.
func (t generationTarget) NormalizeMetadata() {}

func (t generationTarget) EachPart(fn func(contentcapture.PartTarget)) {
	for i := range t.g.Input {
		eachStructPart(&t.g.Input[i], fn)
	}
	for i := range t.g.Output {
		eachStructPart(&t.g.Output[i], fn)
	}
}

func (t generationTarget) EachTool(fn func(contentcapture.ToolTarget)) {
	for i := range t.g.Tools {
		fn(toolTarget{tool: &t.g.Tools[i]})
	}
}

func eachStructPart(message *Message, fn func(contentcapture.PartTarget)) {
	for i := range message.Parts {
		fn(partTarget{part: &message.Parts[i]})
	}
}

// partTarget adapts one Part to contentcapture.PartTarget. A struct part
// carries each payload in its own field, so a clear that does not apply to the
// part writes a zero value over a zero value.
type partTarget struct {
	part *Part
}

func (t partTarget) ClearText() { t.part.Text = "" }

func (t partTarget) ClearThinking() { t.part.Thinking = "" }

func (t partTarget) ClearToolCallInput() {
	if t.part.ToolCall != nil {
		t.part.ToolCall.InputJSON = nil
	}
}

func (t partTarget) ClearToolResult() {
	if t.part.ToolResult != nil {
		t.part.ToolResult.Content = ""
		t.part.ToolResult.ContentJSON = nil
	}
}

func (t partTarget) ClearMediaURL() {
	if t.part.Media != nil {
		t.part.Media.URL = ""
	}
}

// toolTarget adapts one ToolDefinition to contentcapture.ToolTarget.
type toolTarget struct {
	tool *ToolDefinition
}

func (t toolTarget) ClearDescription() { t.tool.Description = "" }

func (t toolTarget) ClearInputSchema() { t.tool.InputSchema = nil }

// resolveToolContentCaptureMode resolves the effective content capture mode for
// a tool execution from the per-tool override, parent generation context, and
// client default.
func resolveToolContentCaptureMode(toolMode, ctxMode ContentCaptureMode, ctxSet bool, clientDefault ContentCaptureMode) ContentCaptureMode {
	resolved := resolveClientContentCaptureMode(clientDefault)
	if ctxSet {
		resolved = ctxMode
	}
	if toolMode != ContentCaptureModeDefault {
		resolved = toolMode
	}
	return resolved
}

// String returns the string representation of a ContentCaptureMode.
func (m ContentCaptureMode) String() string {
	switch m {
	case ContentCaptureModeFull:
		return contentCaptureModeValueFull
	case ContentCaptureModeNoToolContent:
		return contentCaptureModeValueNoToolContent
	case ContentCaptureModeMetadataOnly:
		return contentCaptureModeValueMetaOnly
	case ContentCaptureModeFullWithMetadataSpans:
		return contentCaptureModeValueFullWithMetadataSpans
	default:
		return "default"
	}
}

// MarshalText implements encoding.TextMarshaler for ContentCaptureMode.
func (m ContentCaptureMode) MarshalText() ([]byte, error) {
	return []byte(m.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler for ContentCaptureMode.
func (m *ContentCaptureMode) UnmarshalText(text []byte) error {
	switch strings.ToLower(string(text)) {
	case contentCaptureModeValueFull:
		*m = ContentCaptureModeFull
	case contentCaptureModeValueNoToolContent:
		*m = ContentCaptureModeNoToolContent
	case contentCaptureModeValueMetaOnly:
		*m = ContentCaptureModeMetadataOnly
	case contentCaptureModeValueFullWithMetadataSpans:
		*m = ContentCaptureModeFullWithMetadataSpans
	case "default", "":
		*m = ContentCaptureModeDefault
	default:
		return fmt.Errorf("unknown content capture mode: %q", string(text))
	}
	return nil
}
