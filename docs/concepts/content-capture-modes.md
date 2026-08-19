# Content Capture Modes

Content capture decides which fields the agento11y SDK includes in exported generations and OTel span attributes. Use it to keep prompts, tool I/O, and model responses inside the process when the destination (Grafana stack, OTel collector, shared infrastructure) should not receive them.

Each SDK README links here for language-specific examples.

## Modes

| Mode | Meaning |
| --- | --- |
| `default` | Inherit from the next layer in the resolution chain. At the client level this resolves to `no_tool_content`. |
| `full` | Export all content: generation messages, thinking, system prompts, tool arguments and results, embedding input texts. |
| `no_tool_content` | Export full generation content but omit tool-execution arguments and results from span attributes. Matches the pre-`ContentCaptureMode` SDK default. |
| `metadata_only` | Preserve structure (message roles, part kinds, tool names, media kind/MIME metadata, model, token usage, timing, IDs) and strip text, tool arguments, tool results, media URLs/data URLs, thinking, system prompts, conversation titles, raw artifacts, tool descriptions, tool input schemas, and detailed error messages. |
| `full_with_metadata_spans` | Send full content over the gRPC/HTTP generation ingest, but omit content fields from OTel spans. Useful when ingest is private but traces and metrics are shared. |

`default` is never written into telemetry. It is the placeholder for "inherit"; the SDK resolves it to one of the four concrete modes before exporting anything.

## Behavior matrix

| Mode | Generation export (gRPC/HTTP) | Generation span | Tool execution span | Embedding span |
| --- | --- | --- | --- | --- |
| `full` | Full content. | Content attributes included. | Arguments and results included. | Input texts included when `EmbeddingCapture.CaptureInput` is on. |
| `no_tool_content` | Full content. | Content attributes included. | Arguments and results omitted. | Input texts included when `EmbeddingCapture.CaptureInput` is on. |
| `metadata_only` | Structure only. Messages, tool args/results, media URLs/data URLs, thinking, system prompts, conversation titles, artifacts, error text removed. | Content attributes omitted. | Arguments and results omitted. | Input texts omitted. |
| `full_with_metadata_spans` | Full content. | Content attributes omitted (`agento11y.conversation.title` and related fields). | Arguments and results omitted. Equivalent to `metadata_only` for tool spans because there is no separate gRPC export for tool executions. | Input texts omitted. Equivalent to `metadata_only` for embedding spans for the same reason. |

Embedding input text capture is gated by both the effective capture mode and `EmbeddingCapture.CaptureInput` / `embeddingCapture.captureInput`. Setting the flag does not bypass `metadata_only` or `full_with_metadata_spans`.

## Caveats

No capture mode strips user-provided `metadata` or `tags` on a generation or tool execution. SDK-internal metadata keys that carry content (e.g. `call_error`, `agento11y.conversation.title`) are stripped along with the matching content. Keep sensitive content out of `metadata` and `tags` when using `metadata_only` or `full_with_metadata_spans`.

Capture modes decide *which fields ship*, not what's inside them. To sanitize the fields that do ship (e.g. strip secrets out of assistant text under `full`), use the pre-ingest redactor: `GenerationSanitizer` in Go, `generationSanitizer` in JS/TS, the equivalent in Python.

`full_with_metadata_spans` only protects spans. Generation content still flows through the SDK's generation export channel. Use `metadata_only` if you also want the ingest channel to receive no content.

A locally captured coding-agent session always records full prompts, responses, and tool I/O on that machine, whatever the capture mode.

## Defaults

The default differs between SDK clients and coding-agent destinations.

| Surface | Default mode |
| --- | --- |
| Core SDK client (Go, Python, JS/TS, Java, .NET) | `no_tool_content`. Generation content is captured; tool-execution arguments and results stay out of spans. |
| Coding-agent plugin exporting to Grafana Cloud | `metadata_only`. Only metadata leaves the machine unless the user selects another mode. |
| Coding-agent plugin capturing locally | `full`. Local capture keeps full content; the configured mode still applies to anything sent to Grafana Cloud. |

`default` at the client level resolves to `no_tool_content`. To get full content on a core SDK client, set `contentCapture: 'full'` (or the language equivalent) explicitly.

Which row applies to an `agento11y <agent>` launch follows from the credentials: with Grafana Cloud credentials the session goes to Cloud, without them it is captured locally. [`plugins/README.md`](../../plugins/README.md#launch-your-agent) has the flags and the platform limits.

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

For coding-agent plugins, `AGENTO11Y_CONTENT_CAPTURE_MODE` controls what is sent to Grafana Cloud. It does not reduce what local capture records. All plugins (the shared `agento11y` binary used by Claude Code, Codex, Copilot, Cursor, and Vibe; Pi via `@grafana/agento11y-pi`; OpenCode via `@grafana/agento11y-opencode`) accept `full`, `no_tool_content`, `metadata_only`, and `full_with_metadata_spans`. `default` is accepted as an alias for `metadata_only` so plugins match the Go envconfig resolver rather than the JS SDK's client-level default of `no_tool_content`.

Unknown values fall back to `metadata_only` with a warning in the plugin log. A plugin can still export less than the SDK allows. For example, an adapter may drop a field if the host agent does not pass it through.

## Secret redaction in the plugins

Capture modes choose which fields ship. Redaction scrubs the fields that do ship.

A plugin redacts known secret formats out of every content field it exports: user prompts, system prompts, assistant text, thinking, conversation titles, error messages, tool arguments, and tool results. This covers both copies of the tool payload, the one in the generation and the one on the tool-execution span. Only the prompt has a flag.

Set `AGENTO11Y_REDACT_INPUT_MESSAGES=false` in `~/.config/agento11y/config.env` or the environment to export prompt text without redaction. The flag covers the prompt only: every other field stays redacted, and message structure, roles, token counts, tags, and IDs do not change. An unrecognised value keeps redaction on, so a typo cannot disable it.

### Strength per field

There are two pattern tiers. Tier 1 is high-confidence secret formats (`glc_…`, `AKIA…`, a PEM block, a connection string). Tier 2 is the key/value heuristics (`PASSWORD=…`, `"token": "…"`), which catch a secret with no recognisable format but also fire on ordinary text.

Every plugin applies the same tier per field, and it is the tier the SDKs' generation sanitizer applies:

| Field | Tier | Why |
| --- | --- | --- |
| User prompt | 1 + 2 | Where a human pastes a `.env` file, a config block or a command line. |
| System prompt | 1 + 2 | Assembled content: tool definitions, environment dumps, pasted config. |
| Tool arguments, tool results | 1 + 2 | Structured data, on the generation and on the span. |
| Assistant text, thinking | 1 | Model prose. |
| Conversation title | 1 | Usually the first prompt, truncated. |
| Error messages | 1 | Sentences. |

Tier 2 on a prompt has a real cost: `sort key: name` is exported as `sort key: [REDACTED:env-secret-value]`, because the heuristic cannot tell that `key:` is part of a sentence. Turn prompt redaction off with `AGENTO11Y_REDACT_INPUT_MESSAGES=false` if the prompt text matters more than the coverage. Tier 2 is kept off prose for that reason, and a secret a model repeats in prose is still caught by tier 1 as long as it has a known format.

On a tool payload that decodes as JSON, the shared `agento11y` binary also redacts a value under a secret-looking key (`authorization`, `cookie`, `client_secret`), which the tier 2 key list does not cover. The OpenCode and Pi plugins do not: they redact the encoded JSON as text, so they catch only the key names in the tier 2 patterns.

## Related

- [Tool Calls vs Tool Executions](./tool-call-vs-tool-execution.md): explains why tool-execution spans have their own content-capture story.
- SDK `GenerationSanitizer` / `generationSanitizer`: pre-ingest redaction; runs alongside capture modes, not as a replacement.
