package sigil

import (
	"context"
	"fmt"
	"strings"
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
	// when using MetadataOnly mode.
	ContentCaptureModeMetadataOnly
	// ContentCaptureModeFullWithMetadataSpans splits the proto and span paths
	// for generation content. Use this mode when the gRPC ingest destination is
	// private but the OTel traces/metrics destination is shared and must not
	// receive any content.
	//
	// Per-entity behaviour:
	//   - Generation: full content goes to the gRPC export; the generation span
	//     omits sigil.conversation.title.
	//   - ToolExecution: there is no separate gRPC export — the tool execution
	//     span omits gen_ai.tool.call.arguments, gen_ai.tool.call.result, and
	//     sigil.conversation.title. Equivalent to MetadataOnly for tool spans.
	//   - Embedding: there is no separate gRPC export — the embedding span
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
	metadataKeyContentCaptureMode                = "sigil.sdk.content_capture_mode"
	contentCaptureModeValueFull                  = "full"
	contentCaptureModeValueNoToolContent         = "no_tool_content"
	contentCaptureModeValueMetaOnly              = "metadata_only"
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

// isContentStripped reports whether the generation has been through MetadataOnly
// stripping, based on the stamped metadata marker.
func isContentStripped(g Generation) bool {
	if g.Metadata == nil {
		return false
	}
	v, _ := g.Metadata[metadataKeyContentCaptureMode].(string)
	return v == contentCaptureModeValueMetaOnly
}

// stripContent removes sensitive content from a generation while preserving
// message structure (roles, part kinds), tool names/IDs, usage, timing, and
// all other metadata fields. errorCategory is the classified error category
// (e.g. "rate_limit", "timeout") used to replace the raw CallError text.
func stripContent(g *Generation, errorCategory string) {
	g.SystemPrompt = ""
	g.Artifacts = nil

	if g.CallError != "" {
		if errorCategory != "" {
			g.CallError = errorCategory
		} else {
			g.CallError = "sdk_error"
		}
	}
	delete(g.Metadata, "call_error")

	g.ConversationTitle = ""
	delete(g.Metadata, spanAttrConversationTitle)

	for i := range g.Input {
		stripMessageContent(&g.Input[i])
	}
	for i := range g.Output {
		stripMessageContent(&g.Output[i])
	}
	for i := range g.Tools {
		g.Tools[i].Description = ""
		g.Tools[i].InputSchema = nil
	}
}

func stripMessageContent(m *Message) {
	for i := range m.Parts {
		m.Parts[i].Text = ""
		m.Parts[i].Thinking = ""
		if m.Parts[i].ToolCall != nil {
			m.Parts[i].ToolCall.InputJSON = nil
		}
		if m.Parts[i].ToolResult != nil {
			m.Parts[i].ToolResult.Content = ""
			m.Parts[i].ToolResult.ContentJSON = nil
		}
	}
}

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
