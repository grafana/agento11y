# Content Capture Modes

Content capture decides which fields the Sigil SDK includes in exported generations and OTel span attributes. Use it to keep prompts, tool I/O, and model responses inside the process when the destination (Grafana stack, OTel collector, shared infrastructure) should not receive them.

Each SDK README links here for language-specific examples.

## Modes

| Mode | Meaning |
| --- | --- |
| `default` | Inherit from the next layer in the resolution chain. At the client level this resolves to `no_tool_content`. |
| `full` | Export all content: generation messages, thinking, system prompts, tool arguments and results, embedding input texts. |
| `no_tool_content` | Export full generation content but omit tool-execution arguments and results from span attributes. Matches the pre-`ContentCaptureMode` SDK default. |
| `metadata_only` | Preserve structure (message roles, part kinds, tool names, model, token usage, timing, IDs) and strip text, tool arguments, tool results, thinking, system prompts, conversation titles, raw artifacts, tool descriptions, tool input schemas, and detailed error messages. |
| `full_with_metadata_spans` | Send full content over the gRPC/HTTP generation ingest, but omit content fields from OTel spans. Useful when ingest is private but traces and metrics are shared. |

`default` is never written into telemetry. It is the placeholder for "inherit"; the SDK resolves it to one of the four concrete modes before exporting anything.

## Behavior matrix

| Mode | Generation export (gRPC/HTTP) | Generation span | Tool execution span | Embedding span |
| --- | --- | --- | --- | --- |
| `full` | Full content. | Content attributes included. | Arguments and results included. | Input texts included when `EmbeddingCapture.CaptureInput` is on. |
| `no_tool_content` | Full content. | Content attributes included. | Arguments and results omitted. | Input texts included when `EmbeddingCapture.CaptureInput` is on. |
| `metadata_only` | Structure only. Messages, tool args/results, thinking, system prompts, conversation titles, artifacts, error text removed. | Content attributes omitted. | Arguments and results omitted. | Input texts omitted. |
| `full_with_metadata_spans` | Full content. | Content attributes omitted (`sigil.conversation.title` and related fields). | Arguments and results omitted. Equivalent to `metadata_only` for tool spans because there is no separate gRPC export for tool executions. | Input texts omitted. Equivalent to `metadata_only` for embedding spans for the same reason. |

Embedding input text capture is gated by both the effective capture mode and `EmbeddingCapture.CaptureInput` / `embeddingCapture.captureInput`. Setting the flag does not bypass `metadata_only` or `full_with_metadata_spans`.

## Caveats

No capture mode strips user-provided `metadata` or `tags` on a generation or tool execution. SDK-internal metadata keys that carry content (e.g. `call_error`, `sigil.conversation.title`) are stripped along with the matching content. Keep sensitive content out of `metadata` and `tags` when using `metadata_only` or `full_with_metadata_spans`.

Capture modes decide *which fields ship*, not what's inside them. To sanitize the fields that do ship (e.g. strip secrets out of assistant text under `full`), use the pre-ingest redactor: `GenerationSanitizer` in Go, `generationSanitizer` in JS/TS, the equivalent in Python.

`full_with_metadata_spans` only protects spans. Generation content still flows through the SDK's generation export channel. Use `metadata_only` if you also want the ingest channel to receive no content.

## Defaults

The default differs between SDK clients and coding-agent plugins.

| Surface | Default mode |
| --- | --- |
| Core SDK client (Go, Python, JS/TS, Java, .NET) | `no_tool_content`. Generation content is captured; tool-execution arguments and results stay out of spans. |
| Coding-agent plugins (shared `sigil` binary, `@grafana/sigil-pi`, `@grafana/sigil-opencode`) | `metadata_only`. Coding-agent sessions usually run on shared machines, so the plugins ship metadata-only by default. |

`default` at the client level resolves to `no_tool_content`. To get full content on a core SDK client, set `contentCapture: 'full'` (or the language equivalent) explicitly.

## Resolution precedence

The SDK resolves capture mode differently by recording type and language.

Generation starts:

- Go, JS/TS, Java, and .NET: per-generation `ContentCapture` / `contentCapture` > `ContentCaptureResolver` / `contentCaptureResolver` return value > client-level setting.
- Python: per-generation `content_capture` > `with_content_capture_mode(...)` when set > `content_capture_resolver` return value > client-level setting.

Tool executions:

- Go, Python, Java, and .NET: per-tool `ContentCapture` / `content_capture` > parent generation's resolved mode (or Python's public capture-mode scope) > resolver return value > client-level setting.
- JS/TS: per-tool `contentCapture` > resolver return value > client-level setting. The JS SDK does not propagate capture mode through async context.

Embeddings:

- All SDKs: resolver return value > client-level setting, then the embedding input-capture flag gates whether input texts can be attached to spans.

A resolver return value of `default` defers to the next layer. Resolver exceptions are caught and treated as `metadata_only` (fail-closed).

## Configuring capture

Per-language READMEs include code examples:

- Go: [`go/README.md`](../../go/README.md)
- Python: [`python/README.md`](../../python/README.md)
- JS/TS: [`js/README.md`](../../js/README.md)
- Java: [`java/README.md`](../../java/README.md)
- .NET: [`dotnet/README.md`](../../dotnet/README.md)

For coding-agent plugins, the relevant env var is `SIGIL_CONTENT_CAPTURE_MODE`. All plugins (the shared `sigil` binary used by Claude Code, Codex, Copilot, and Cursor; Pi via `@grafana/sigil-pi`; OpenCode via `@grafana/sigil-opencode`) accept `full`, `no_tool_content`, `metadata_only`, and `full_with_metadata_spans`. `default` is accepted as an alias for `metadata_only` so plugins match the Go envconfig resolver rather than the JS SDK's client-level default of `no_tool_content`.

Unknown values fall back to `metadata_only` with a warning in the plugin log. A plugin can still export less than the SDK allows. For example, an adapter may drop a field if the host agent does not pass it through.

## Related

- [Tool Calls vs Tool Executions](./tool-call-vs-tool-execution.md): explains why tool-execution spans have their own content-capture story.
- SDK `GenerationSanitizer` / `generationSanitizer`: pre-ingest redaction; runs alongside capture modes, not as a replacement.
