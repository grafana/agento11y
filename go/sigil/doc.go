// Package sigil provides manual recording helpers for LLM generations.
//
// The primary API follows an OTel-like pattern:
//
//	ctx, rec := client.StartGeneration(ctx, start)
//	defer rec.End()
//	resp, err := provider.Call(ctx, req)
//	if err != nil { rec.SetCallError(err); return err }
//	rec.SetResult(mapper.FromRequestResponse(req, resp))
//
// For streaming calls, use:
//
//	ctx, rec := client.StartStreamingGeneration(ctx, start)
//	defer rec.End()
//	// process stream...
//	rec.SetResult(mapper.FromStream(req, summary))
//
// Tool calls can be wrapped with Client.StartToolExecution + ToolExecutionRecorder.End.
//
// Start returns a context for your provider call. End is a no-arg, no-return
// finalizer safe for defer. It is idempotent and nil-safe.
//
// # Content Capture
//
// Config.ContentCapture sets the default ContentCaptureMode. The SDK supports
// four concrete modes plus the Default placeholder:
//
//   - ContentCaptureModeDefault: inherit from the next layer. At the client
//     level this resolves to ContentCaptureModeNoToolContent.
//   - ContentCaptureModeFull: export all content.
//   - ContentCaptureModeNoToolContent: full generation content, but tool
//     execution arguments and results are excluded from span attributes.
//     This is the effective client default and matches pre-ContentCaptureMode
//     SDK behavior.
//   - ContentCaptureModeMetadataOnly: preserve structure, tool names, usage,
//     timing, and IDs; strip text, tool arguments, tool results, thinking,
//     system prompts, conversation titles, raw artifacts, tool descriptions,
//     tool input schemas, and detailed error text.
//   - ContentCaptureModeFullWithMetadataSpans: keep full content on the
//     generation export (gRPC/HTTP) but omit content fields from OTel spans.
//     Use this when the gRPC ingest destination is private but the OTel
//     traces/metrics destination is shared. Tool execution and embedding
//     spans behave like MetadataOnly under this mode because they have no
//     separate gRPC export.
//
// Per-generation and per-tool-execution overrides are available via the
// ContentCapture field on GenerationStart and ToolExecutionStart.
// Tool executions inherit the parent generation's resolved mode via context.
//
// Config.ContentCaptureResolver enables dynamic per-request resolution. When set,
// the resolver is called before each generation start, embedding start, tool
// execution, and rating submission with the request context and recording
// metadata (nil for tool executions, which carry no metadata). This is the
// recommended integration point for multi-tenant services that need to resolve
// content capture mode based on tenant-level policies (e.g., data-sharing
// opt-out).
//
// Generation resolution precedence (highest to lowest):
//   - GenerationStart.ContentCapture (explicit override)
//   - ContentCaptureResolver return value
//   - Config.ContentCapture (static default; ContentCaptureModeDefault here
//     resolves to ContentCaptureModeNoToolContent)
//
// Tool execution resolution precedence (highest to lowest):
//   - ToolExecutionStart.ContentCapture (explicit override)
//   - Parent generation's resolved mode (via context)
//   - ContentCaptureResolver return value
//   - Config.ContentCapture (static default; ContentCaptureModeDefault here
//     resolves to ContentCaptureModeNoToolContent)
//
// Resolver panics are recovered and treated as MetadataOnly (fail-closed).
//
// Capture modes do not filter user-provided Metadata or Tags maps.
// Callers are responsible for not including sensitive content in those fields
// when using MetadataOnly or FullWithMetadataSpans mode.
//
// See docs/concepts/content-capture-modes.md in the repository for the full
// per-surface behavior matrix shared across SDKs.
//
// # Linking
//
// Linking is bi-directional:
//   - Generation.TraceID/SpanID point to the created span.
//   - The span includes the generation ID in attribute "sigil.generation.id".
package sigil
