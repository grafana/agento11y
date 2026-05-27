# Grafana Sigil Go SDK

The Sigil Go SDK records LLM generations and tool calls for [Grafana AI observability](https://grafana.com/docs/grafana-cloud/machine-learning/ai-observability/). It emits OpenTelemetry spans and metrics through your existing OTel setup and sends normalized generation payloads through Sigil's ingest channel.

## Install

```sh
go get github.com/grafana/sigil-sdk/go
```

## Quick start

```go
client := sigil.NewClient(sigil.Config{}) // reads SIGIL_* env vars
defer func() { _ = client.Shutdown(context.Background()) }()

ctx, rec := client.StartGeneration(ctx, sigil.GenerationStart{
	ConversationID: "conv-9b2f",
	AgentName:      "assistant-core",
	AgentVersion:   "1.0.0",
	Model:          sigil.ModelRef{Provider: "anthropic", Name: "claude-sonnet-4-5"},
})
defer rec.End()

resp, err := provider.Call(ctx, req)
if err != nil {
	rec.SetCallError(err)
	return err
}

rec.SetResult(sigil.Generation{
	Input:  []sigil.Message{sigil.UserTextMessage("Hello")},
	Output: []sigil.Message{sigil.AssistantTextMessage(resp.Text)},
	Usage:  sigil.TokenUsage{InputTokens: 120, OutputTokens: 42},
}, nil)
```

See `Configuration` for the explicit-config form and `Recording API` for the full surface.

Framework helpers:

- Google ADK: [`go-frameworks/google-adk`](../go-frameworks/google-adk/README.md)

## Core model

- `Generation` is the canonical entity.
- `Generation.Mode` is explicit: `SYNC` or `STREAM`.
- `OperationName` defaults are mode-aware:
  - `SYNC` -> `generateText`
  - `STREAM` -> `streamText`
- `ModelRef` bundles `provider + model`.
- `ConversationTitle` is an optional human-readable label for the conversation.
- `AgentName` and `AgentVersion` are optional generation/tool identity fields.
- `SystemPrompt` is separate from messages.
- `ToolDefinition.Deferred` records whether a tool is marked as deferred.
- Request controls are optional first-class fields:
  - `MaxTokens`
  - `Temperature`
  - `TopP`
  - `ToolChoice`
  - `ThinkingEnabled`
- `Message` contains typed parts: `text`, `thinking`, `tool_call`, `tool_result`.
- Normalized `tool_result` correlation is provider-safe:
  - Preserve `tool_result.tool_call_id` whenever the upstream provider exposes a stable per-call identifier.
  - When the upstream surface omits a per-call ID, populate `tool_result.name` with the tool/function name as the fallback correlation key.
  - Local validation requires at least one of `tool_result.tool_call_id` or `tool_result.name`.
- `TokenUsage` includes token/cache/reasoning fields.
- Raw provider `Artifacts` are optional debug payloads.

## Recording API (explicit, OTel-like)

- `StartGeneration(ctx, start)` -> `(ctx, *GenerationRecorder)`
- `StartStreamingGeneration(ctx, start)` -> `(ctx, *GenerationRecorder)`
- `StartToolExecution(ctx, start)` -> `(ctx, *ToolExecutionRecorder)`
- `rec.SetResult(...)` / `rec.SetCallError(...)`
- `rec.End()` is defer-safe and idempotent.
- `rec.Err()` reports local validation/enqueue failures only.
- Background export failures are retried and logged.
- Generation spans emit request controls using GenAI keys where standardized:
  - `gen_ai.request.max_tokens`
  - `gen_ai.request.temperature`
  - `gen_ai.request.top_p`
  - `sigil.gen_ai.request.tool_choice`
  - `sigil.gen_ai.request.thinking.enabled`
  - `sigil.gen_ai.request.thinking.budget_tokens` (provider-specific)
  - `gen_ai.response.finish_reasons` is emitted as a string array.
- Generation/tool spans always include SDK identity attributes:
  - `sigil.sdk.name=sdk-go`
- Normalized generation metadata always includes the same SDK identity key; conflicting caller values are overwritten.
- Context helpers are available for defaults:
  - `WithConversationID(ctx, id)`
  - `WithConversationTitle(ctx, title)`
  - `WithAgentName(ctx, name)`
  - `WithAgentVersion(ctx, version)`

## Configuration

The snippet below configures the SDK explicitly. As an alternative, set `SIGIL_*` environment variables and pass an empty `sigil.Config{}` — refer to the [Grafana Cloud setup guide](https://grafana.com/docs/grafana-cloud/machine-learning/ai-observability/get-started/grafana-cloud/) for the variable names.

```go
client := sigil.NewClient(sigil.Config{})
defer func() { _ = client.Shutdown(context.Background()) }()
```

For explicit configuration with custom auth or batch tuning:

```go
cfg := sigil.DefaultConfig()

// Optional: inject tracer/meter explicitly.
// If unset, the SDK uses otel.Tracer(...) and otel.Meter(...).
// cfg.Tracer = myTracer
// cfg.Meter = myMeter

// Generation export (custom ingest)
cfg.GenerationExport.Protocol = sigil.GenerationExportProtocolGRPC // default; or sigil.GenerationExportProtocolHTTP / sigil.GenerationExportProtocolNone
cfg.GenerationExport.Endpoint = "localhost:4317" // HTTP parity: "http://localhost:8080" (SDK auto-appends /api/v1/generations:export)
cfg.GenerationExport.Auth = sigil.AuthConfig{
	Mode:     sigil.ExportAuthModeTenant,
	TenantID: "dev-tenant",
}
cfg.GenerationExport.BatchSize = 100
cfg.GenerationExport.FlushInterval = time.Second
cfg.GenerationExport.QueueSize = 2000
cfg.GenerationExport.MaxRetries = 5
cfg.GenerationExport.InitialBackoff = 100 * time.Millisecond
cfg.GenerationExport.MaxBackoff = 5 * time.Second
cfg.GenerationExport.GRPCMaxSendMessageBytes = 16 << 20
cfg.GenerationExport.GRPCMaxReceiveMessageBytes = 16 << 20
cfg.GenerationExport.PayloadMaxBytes = 16 << 20

// Sigil API base used by helpers like SubmitConversationRating.
cfg.API.Endpoint = "http://localhost:8080"

client := sigil.NewClient(cfg)
defer func() {
	_ = client.Shutdown(context.Background())
}()
```

Configure OTEL exporters (traces/metrics) in your application OTEL SDK setup.

Quick OTEL setup pattern before creating the Sigil client:

```go
tp := sdktrace.NewTracerProvider()
otel.SetTracerProvider(tp)

mp := sdkmetric.NewMeterProvider()
otel.SetMeterProvider(mp)
```

### Instrumentation-only mode (no generation send)

Use `GenerationExportProtocolNone` to keep generation and tool instrumentation active while disabling generation transport:

```go
cfg := sigil.DefaultConfig()
cfg.GenerationExport.Protocol = sigil.GenerationExportProtocolNone

client := sigil.NewClient(cfg)
defer func() { _ = client.Shutdown(context.Background()) }()
```

## Generation export auth modes

Auth is configured for generation export.

- `none`
- `tenant` (requires `TenantID`, injects `X-Scope-OrgID`)
- `bearer` (requires `BearerToken`, injects `Authorization: Bearer <token>`)
- `basic` (requires `BasicPassword` + `BasicUser` or `TenantID`, injects `Authorization: Basic <base64(user:password)>`; also injects `X-Scope-OrgID` when `TenantID` is set — for multi-tenant deployments only, not needed for Grafana Cloud)

Invalid combinations fail fast during `NewClient(...)`.

```go
cfg.GenerationExport.Auth = sigil.AuthConfig{
	Mode:        sigil.ExportAuthModeBearer,
	BearerToken: "token-from-secret-manager",
}
```

Explicit transport headers remain the highest-precedence escape hatch. If `Headers` already contains `Authorization` or `X-Scope-OrgID`, the SDK does not overwrite them.

### Grafana Cloud auth (basic)

For Grafana Cloud, use `basic` auth mode. The username is your Grafana Cloud instance/tenant ID and the password is your Grafana Cloud API key:

```go
cfg.GenerationExport.Auth = sigil.AuthConfig{
	Mode:          sigil.ExportAuthModeBasic,
	TenantID:      os.Getenv("SIGIL_AUTH_TENANT_ID"),
	BasicPassword: os.Getenv("SIGIL_AUTH_TOKEN"),
}
```

If your deployment requires a distinct username (different from the tenant ID), set `BasicUser` explicitly:

```go
cfg.GenerationExport.Auth = sigil.AuthConfig{
	Mode:          sigil.ExportAuthModeBasic,
	TenantID:      os.Getenv("SIGIL_AUTH_TENANT_ID"),
	BasicUser:     os.Getenv("SIGIL_AUTH_TENANT_ID"),
	BasicPassword: os.Getenv("SIGIL_AUTH_TOKEN"),
}
```

## Wiring custom env vars

The SDK only auto-loads `SIGIL_*` env vars (`SIGIL_ENDPOINT`, `SIGIL_PROTOCOL`, `SIGIL_AUTH_MODE`, `SIGIL_AUTH_TOKEN`, etc.) when you call `sigil.NewClient(sigil.Config{})`. For any other env var (for example one your secret manager exposes under a different name), read it in your app and pass the value into the config:

```go
genToken := strings.TrimSpace(os.Getenv("MY_APP_SIGIL_TOKEN"))
if genToken != "" {
	cfg.GenerationExport.Auth = sigil.AuthConfig{
		Mode:        sigil.ExportAuthModeBearer,
		BearerToken: genToken,
	}
}
```

Common topology:

- Grafana Cloud: generation `basic` mode with instance ID and API key.
- Self-hosted direct to Sigil: generation `tenant` mode.
- Traces/metrics via OTEL Collector/Alloy: configure exporters in your app OTEL SDK setup.
- Enterprise proxy: generation `bearer` mode to proxy; proxy authenticates and forwards tenant header upstream.

## Content Capture Mode

`ContentCaptureMode` controls what content the SDK includes in exported generation payloads and OTel span attributes. See [Content Capture Modes](../docs/concepts/content-capture-modes.md) for the canonical mode matrix and defaults; the snippets below show how to wire it up in Go.

Client-level default:

```go
cfg := sigil.DefaultConfig()
cfg.ContentCapture = sigil.ContentCaptureModeMetadataOnly

client := sigil.NewClient(cfg)
defer func() { _ = client.Shutdown(context.Background()) }()
```

The core SDK client treats `ContentCaptureModeDefault` as `ContentCaptureModeNoToolContent`: generation content is captured but tool-execution arguments and results stay out of spans.

Per-generation override:

```go
ctx, rec := client.StartGeneration(ctx, sigil.GenerationStart{
    Model:          sigil.ModelRef{Provider: "openai", Name: "gpt-5"},
    ContentCapture: sigil.ContentCaptureModeFull,
})
defer rec.End()
```

Per-tool-execution override (here `Full` opts into capturing tool arguments and results in the span):

```go
ctx, tool := client.StartToolExecution(ctx, sigil.ToolExecutionStart{
    ToolName:       "search",
    ContentCapture: sigil.ContentCaptureModeFull,
})
defer tool.End()
```

Tool executions also inherit the parent generation's resolved mode via context, so explicit overrides are rarely needed inside an instrumented generation block.

Dynamic resolution via `ContentCaptureResolver`:

```go
cfg.ContentCaptureResolver = func(ctx context.Context, metadata map[string]any) sigil.ContentCaptureMode {
    if metadata["sigil.tenant"] == "healthcare" {
        return sigil.ContentCaptureModeMetadataOnly
    }
    return sigil.ContentCaptureModeDefault // defer to Config.ContentCapture
}
```

Resolver panics are recovered and treated as `ContentCaptureModeMetadataOnly` (fail-closed).

Resolution precedence for generations (highest to lowest):

1. Per-generation `ContentCapture`
2. `ContentCaptureResolver` return value
3. `Config.ContentCapture` (defaults to `ContentCaptureModeNoToolContent`)

Resolution precedence for tool executions (highest to lowest):

1. Per-tool `ContentCapture`
2. Parent generation's resolved mode, propagated through `context.Context`
3. `ContentCaptureResolver` return value
4. `Config.ContentCapture` (defaults to `ContentCaptureModeNoToolContent`)

User-provided `Metadata` and `Tags` are not stripped by any capture mode. SDK-internal metadata keys that carry content (e.g. `call_error`, `sigil.conversation.title`) are stripped along with the matching content.

## Pre-Ingest Redaction

Use `GenerationSanitizer` when you want to redact substrings from normalized generations before validation, span sync, and export.

```go
redactEmails := true
cfg := sigil.DefaultConfig()
cfg.GenerationSanitizer = sigil.NewSecretRedactionSanitizer(sigil.SecretRedactionOptions{
    RedactInputMessages:  false,         // also redact user messages in Generation.Input
    RedactEmailAddresses: &redactEmails, // nil defaults to true; point to false to preserve
})

client := sigil.NewClient(cfg)
```

The built-in sanitizer:

- redacts high-confidence secret formats in assistant text and thinking
- redacts secret formats plus env-style secret values in tool call inputs and tool results
- redacts email addresses by default
- redacts historic assistant turns and tool messages in `Generation.Input`
- leaves user messages in `Generation.Input` unchanged unless `RedactInputMessages: true` is set

To preserve email addresses, opt out explicitly:

```go
preserveEmails := false
cfg.GenerationSanitizer = sigil.NewSecretRedactionSanitizer(sigil.SecretRedactionOptions{
    RedactEmailAddresses: &preserveEmails,
})
```

If a sanitizer panics during `Recorder.End`, the SDK falls back to `ContentCaptureModeMetadataOnly` for that generation and logs a warning via `Config.Logger`, so a partially redacted payload is never shipped.

## Conversation Ratings

Use the SDK helper to submit user-facing ratings:

```go
rating, err := client.SubmitConversationRating(ctx, "conv-123", sigil.ConversationRatingInput{
	RatingID: "rat-123",
	Rating:   sigil.ConversationRatingValueBad,
	Comment:  "Answer ignored user context",
	Metadata: map[string]any{
		"channel": "assistant-ui",
	},
	Source: "sdk-go",
})
if err != nil {
	panic(err)
}

fmt.Printf("rating=%s has_bad=%v\n", rating.Rating.Rating, rating.Summary.HasBadRating)
```

`SubmitConversationRating` sends requests to `cfg.API.Endpoint` (default `http://localhost:8080`) and uses the same generation-export auth headers (`tenant` or `bearer`) that your client config already resolves.

## Lifecycle requirement

- Always call `client.Shutdown(ctx)` before process exit.
- `Shutdown` flushes pending generation batches and closes generation exporters.
- Optional `client.Flush(ctx)` is available for explicit flush points.

## SDK metrics

The SDK emits four OTel histograms automatically through your configured OTel meter provider:

- `gen_ai.client.operation.duration`
- `gen_ai.client.token.usage`
- `gen_ai.client.time_to_first_token`
- `gen_ai.client.tool_calls_per_operation`

## Streaming example

```go
ctx, rec := client.StartStreamingGeneration(ctx, sigil.GenerationStart{
	ConversationID: "conv-stream",
	AgentName:      "assistant-core",
	AgentVersion:   "1.0.0",
	Model:          sigil.ModelRef{Provider: "openai", Name: "gpt-5"},
})
defer rec.End()

// accumulate stream output...
rec.SetResult(sigil.Generation{
	Input:  []sigil.Message{sigil.UserTextMessage("Say hello")},
	Output: []sigil.Message{sigil.AssistantTextMessage(stitchedOutput)},
}, nil)
```

## Embedding observability

Use `StartEmbedding` for embedding API calls. Embedding recording emits OTel spans and SDK metrics only, and does not enqueue generation export payloads.

```go
ctx, rec := client.StartEmbedding(ctx, sigil.EmbeddingStart{
	AgentName:    "retrieval-worker",
	AgentVersion: "1.0.0",
	Model:        sigil.ModelRef{Provider: "openai", Name: "text-embedding-3-small"},
})
defer rec.End()

resp, err := provider.Embeddings.New(ctx, req)
if err != nil {
	rec.SetCallError(err)
	return err
}

rec.SetResult(sigil.EmbeddingResult{
	InputCount:    len(req.Input),
	InputTokens:   resp.Usage.PromptTokens,
	InputTexts:    req.Input, // captured only when EmbeddingCapture.CaptureInput=true
	ResponseModel: resp.Model,
})
if err := rec.Err(); err != nil {
	return err
}
```

Input text capture is opt-in and should stay off in production unless you need short-term debugging:

```go
cfg.EmbeddingCapture = sigil.EmbeddingCaptureConfig{
	CaptureInput:  true,
	MaxInputItems: 20,
	MaxTextLength: 1024,
}
```

`CaptureInput` can expose PII/document content in spans. Keep it disabled by default and enable only for scoped diagnostics.

TraceQL examples:

- `traces{gen_ai.operation.name="embeddings"}`
- `traces{gen_ai.operation.name="embeddings" && gen_ai.request.model="text-embedding-3-small"}`
- `traces{gen_ai.operation.name="embeddings" && error.type!=""}`

## Provider wrappers

Provider modules are documented wrapper-first for ergonomics and include explicit-flow alternatives.

Current Go provider helpers:

- `go-providers/openai` (OpenAI Chat Completions + Responses wrappers and mappers)
- `go-providers/anthropic` (Anthropic Messages wrappers and mappers; embeddings currently unsupported by the upstream SDK/API surface)
- `go-providers/gemini`

## Raw artifact policy

- Default: raw artifacts OFF in provider wrappers.
- Opt-in only for debug workflows (`WithRawArtifacts()` in provider helper packages).
- Normalized generation fields remain always on.

## Conformance harness

The Go SDK ships a local no-Docker conformance harness for the current cross-SDK baseline.

- Shared spec: `docs/references/sdk-conformance-spec.md` (in the sigil repo)
- Default local command: `mise run sdk:conformance`
- Direct Go command: `cd go && GOWORK=off go test ./sigil -run '^TestConformance' -count=1`
- Current baseline coverage: sync roundtrip, conversation title resolution, user ID resolution, agent name/version resolution, streaming mode + TTFT, tool execution, embeddings, validation/error handling, rating submission, and shutdown flush semantics across exported generation payloads, OTLP spans, OTLP metrics, and local rating HTTP capture
