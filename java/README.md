# Grafana Sigil Java SDK (Core)

If you already use OpenTelemetry, Sigil is a thin extension for AI observability.

The Java SDK records normalized generation payloads, correlates them with traces, and exports generations through transport-aware clients.

## Requirements

- Java 17+
- OpenTelemetry SDK already in your app (optional but recommended)

For a Grafana Cloud setup walkthrough (where to find the endpoint URL, instance ID, and API token), refer to the [Grafana Cloud setup guide](https://grafana.com/docs/grafana-cloud/machine-learning/ai-observability/get-started/grafana-cloud/).

## Modules

- `:core`: runtime client, models, validation, generation exporters
- `:providers:openai`: OpenAI wrapper + mapper helpers
- `:providers:anthropic`: Anthropic wrapper + mapper helpers
- `:providers:gemini`: Gemini wrapper + mapper helpers
- `:frameworks:google-adk`: Google ADK framework lifecycle adapter
- `:benchmarks`: JMH benchmark suite

## Core Model

- `Generation` is the canonical record.
- `GenerationMode` is explicit: `SYNC` or `STREAM`.
- Operation defaults are mode-aware:
  - `SYNC` -> `generateText`
  - `STREAM` -> `streamText`
- `ModelRef` is `provider + model`.
- `Message` has typed `MessagePart` variants: `text`, `thinking`, `tool_call`, `tool_result`.
- Raw provider artifacts are optional and default OFF.

## Recording API

- `startGeneration(start)`
- `startStreamingGeneration(start)`
- `withGeneration(start, fn)`
- `withStreamingGeneration(start, fn)`
- `startToolExecution(start)` — see [Tool Calls vs Tool Executions](../docs/concepts/tool-call-vs-tool-execution.md)
- `withToolExecution(start, fn)`
- `flush()` and `shutdown()`

`GenerationRecorder#error()` reports local SDK failures only (validation/enqueue/shutdown), not provider call exceptions.

- Generation/tool spans always include:
  - `sigil.sdk.name=sdk-java`
- Normalized generation metadata always includes the same key.
- If caller metadata provides a conflicting value for this key, the SDK overwrites it.

## Quick Start (sync)

The snippet below configures the SDK explicitly. As an alternative, set `SIGIL_*` environment variables and call `new SigilClient()` with no arguments — refer to the [Grafana Cloud setup guide](https://grafana.com/docs/grafana-cloud/machine-learning/ai-observability/get-started/grafana-cloud/) for the variable names.

```java
SigilClient client = new SigilClient(new SigilClientConfig()
    .setApi(new ApiConfig()
        .setEndpoint("https://sigil-prod-<region>.grafana.net"))
    .setGenerationExport(new GenerationExportConfig()
        .setProtocol(GenerationExportProtocol.HTTP)
        .setEndpoint("https://sigil-prod-<region>.grafana.net")
        .setAuth(new AuthConfig()
            .setMode(AuthMode.BASIC)
            .setTenantId(System.getenv("SIGIL_AUTH_TENANT_ID"))
            .setBasicPassword(System.getenv("SIGIL_AUTH_TOKEN")))));

try {
    client.withGeneration(
        new GenerationStart()
            .setConversationId("conv-1")
            .setModel(new ModelRef().setProvider("openai").setName("gpt-5")),
        recorder -> {
            recorder.setResult(new GenerationResult()
                .setOutput(java.util.List.of(
                    new Message().setRole(MessageRole.ASSISTANT)
                        .setParts(java.util.List.of(MessagePart.text("Hello from Sigil"))))));
            return null;
        }
    );
} finally {
    client.shutdown();
}
```

Configure OTEL exporters (traces/metrics) in your application OTEL SDK setup. You can optionally inject `Tracer` and `Meter` via `SigilClientConfig`.

Quick OTEL setup pattern before creating the Sigil client:

```java
import io.opentelemetry.sdk.autoconfigure.AutoConfiguredOpenTelemetrySdk;

AutoConfiguredOpenTelemetrySdk.initialize();
```

## Streaming Pattern

Use `startStreamingGeneration(...)` or `withStreamingGeneration(...)`. The SDK sets mode to `STREAM` and keeps operation naming consistent.

## Tool Execution Observability

Use `startToolExecution(...)` / `withToolExecution(...)` to instrument your tool handler code. This is distinct from **tool calls** (what the LLM requests) which are captured automatically by provider wrappers as part of generation output.

```java
// Instrument your tool handler
try (ToolExecutionRecorder rec = client.startToolExecution(
        new ToolExecutionStart()
            .setToolName("weather")
            .setToolCallId("call_abc123"))) {  // Optional: links to LLM's tool_call
    
    String result = weatherService.getWeather("Paris");
    rec.setResult(result);
}
```

Tool executions create OTel spans (`execute_tool <name>`) and appear in the Sigil Tools tab with timing metrics.

For RAG retrieval or other preprocessing steps, use tool execution even though there's no corresponding LLM tool call:

```java
try (ToolExecutionRecorder rec = client.startToolExecution(
        new ToolExecutionStart()
            .setToolName("document_retriever")
            .setContentCapture(ContentCaptureMode.FULL))) {
    
    rec.setArguments(Map.of("query", searchQuery, "limit", 5));
    List<Document> docs = vectorStore.similaritySearch(searchQuery, 5);
    rec.setResult(Map.of("count", docs.size()));
}
```

See [Tool Calls vs Tool Executions](../docs/concepts/tool-call-vs-tool-execution.md) for the full conceptual guide, and [Content Capture Modes](../docs/concepts/content-capture-modes.md) for the modes and defaults.

## Content Capture

`ContentCaptureMode` controls what content the SDK includes in exported generation payloads and OTel span attributes. See [Content Capture Modes](../docs/concepts/content-capture-modes.md) for the canonical mode matrix; the snippets below show how to wire it up in Java.

Client-level default:

```java
SigilClient client = new SigilClient(new SigilClientConfig()
    .setContentCapture(ContentCaptureMode.METADATA_ONLY));
```

The core SDK client treats `ContentCaptureMode.DEFAULT` as `NO_TOOL_CONTENT`.

Per-generation override:

```java
client.withGeneration(
    new GenerationStart()
        .setModel(new ModelRef().setProvider("openai").setName("gpt-5"))
        .setContentCapture(ContentCaptureMode.FULL),
    recorder -> {
        recorder.setResult(new GenerationResult()
            .setOutput(java.util.List.of(
                new Message().setRole(MessageRole.ASSISTANT)
                    .setParts(java.util.List.of(MessagePart.text("hello"))))));
        return null;
    }
);
```

Per-tool-execution override (here `FULL` opts into capturing arguments and results in the span):

```java
try (ToolExecutionRecorder rec = client.startToolExecution(
        new ToolExecutionStart()
            .setToolName("search")
            .setContentCapture(ContentCaptureMode.FULL))) {
    rec.setArguments(Map.of("q", "weather"));
    rec.setResult(Map.of("temp_c", 18));
}
```

Dynamic resolution via `ContentCaptureResolver`:

```java
SigilClient client = new SigilClient(new SigilClientConfig()
    .setContentCaptureResolver(metadata -> {
        if (metadata != null && "healthcare".equals(metadata.get("sigil.tenant"))) {
            return ContentCaptureMode.METADATA_ONLY;
        }
        return ContentCaptureMode.DEFAULT;
    }));
```

Resolver exceptions are caught and treated as `METADATA_ONLY` (fail-closed).

Resolution precedence for generations (highest to lowest):

1. Per-generation `ContentCapture`
2. `ContentCaptureResolver` return value
3. `SigilClientConfig.contentCapture` (defaults to `NO_TOOL_CONTENT`)

Resolution precedence for tool executions (highest to lowest):

1. Per-tool `ContentCapture`
2. Parent generation's resolved mode
3. `ContentCaptureResolver` return value
4. `SigilClientConfig.contentCapture` (defaults to `NO_TOOL_CONTENT`)

User-provided `metadata` and `tags` are not stripped by any capture mode. SDK-internal metadata keys that carry content (e.g. `call_error`, `sigil.conversation.title`) are stripped along with the matching content. See [Tags and Metadata](../docs/concepts/tags-and-metadata.md) for where client tags, per-generation tags, metadata, and `userId` each show up. Java merges client tags into the generation export only; it does not emit `sigil.tag.<key>` on spans/metrics.

## Embedding Observability

Use `startEmbedding(...)` / `withEmbedding(...)` for embedding API calls. Embedding recording emits OTel spans and SDK metrics only, and does not enqueue generation exports.

```java
client.withEmbedding(
    new EmbeddingStart()
        .setAgentName("retrieval-worker")
        .setAgentVersion("1.0.0")
        .setModel(new ModelRef().setProvider("openai").setName("text-embedding-3-small")),
    recorder -> {
        var response = openAiClient.embeddings().create(/* request */);
        recorder.setResult(new EmbeddingResult()
            .setInputCount(2)
            .setInputTokens(response.usage().promptTokens())
            .setInputTexts(java.util.List.of("hello", "world")) // captured only when enabled
            .setResponseModel(response.model()));
        return response;
    }
);
```

Input text capture is opt-in:

```java
SigilClientConfig config = new SigilClientConfig()
    .setEmbeddingCapture(new EmbeddingCaptureConfig()
        .setCaptureInput(true)
        .setMaxInputItems(20)
        .setMaxTextLength(1024));
```

`setCaptureInput(true)` may expose PII/document content in spans. Keep it disabled by default and enable only for scoped debugging.

TraceQL examples:

- `traces{gen_ai.operation.name="embeddings"}`
- `traces{gen_ai.operation.name="embeddings" && gen_ai.request.model="text-embedding-3-small"}`
- `traces{gen_ai.operation.name="embeddings" && error.type!=""}`

## Auth Modes

Auth is configured for generation export:

- `NONE`
- `TENANT` (injects `X-Scope-OrgID`)
- `BEARER` (injects `Authorization: Bearer <token>`)
- `BASIC` (requires `basicPassword` + `basicUser` or `tenantId`, injects `Authorization: Basic <base64(user:password)>`; also injects `X-Scope-OrgID` when `tenantId` is set — for multi-tenant deployments only, not needed for Grafana Cloud)

Invalid combinations fail fast at client construction. If explicit headers already contain `Authorization` or `X-Scope-OrgID`, explicit headers win.

### Grafana Cloud auth (basic)

For Grafana Cloud, use `BASIC` auth mode. The username is your Grafana Cloud instance/tenant ID and the password is your Grafana Cloud API key:

```java
.setAuth(new AuthConfig()
    .setMode(AuthMode.BASIC)
    .setTenantId(System.getenv("SIGIL_AUTH_TENANT_ID"))
    .setBasicPassword(System.getenv("SIGIL_AUTH_TOKEN")))
```

If your deployment requires a distinct username, set `basicUser` explicitly:

```java
.setAuth(new AuthConfig()
    .setMode(AuthMode.BASIC)
    .setTenantId(System.getenv("SIGIL_AUTH_TENANT_ID"))
    .setBasicUser(System.getenv("SIGIL_AUTH_TENANT_ID"))
    .setBasicPassword(System.getenv("SIGIL_AUTH_TOKEN")))
```

Generation export transport protocols:

- `GenerationExportProtocol.HTTP`
- `GenerationExportProtocol.GRPC`
- `GenerationExportProtocol.NONE` (instrumentation-only; no generation transport)

## Context Defaults

You can set defaults via OTel context and override per-call in `GenerationStart`:

```java
try (Scope ignored = SigilContext.withConversationId("conv-ctx")) {
    GenerationRecorder rec = client.startGeneration(new GenerationStart()
        .setModel(new ModelRef().setProvider("openai").setName("gpt-5")));
    // ...
    rec.end();
}
```

Helpers:

- `SigilContext.withConversationId(...)`
- `SigilContext.withAgentName(...)`
- `SigilContext.withAgentVersion(...)`

## Conversation Ratings

Use the SDK helper to submit user-facing ratings:

```java
SubmitConversationRatingResponse result = client.submitConversationRating(
    "conv-123",
    new SubmitConversationRatingRequest()
        .setRatingId("rat-123")
        .setRating(ConversationRatingValue.BAD)
        .setComment("Answer ignored user context")
        .setMetadata(Map.of("channel", "assistant-ui"))
        .setSource("sdk-java"));

System.out.println(result.getRating().getRating() + " hasBad=" + result.getSummary().isHasBadRating());
```

`submitConversationRating(...)` sends requests to `ApiConfig.endpoint`, which should be the Grafana Cloud Sigil API URL from AI Observability configuration, and uses the same generation-export auth headers already configured on the SDK client.

## Lifecycle

- Always call `shutdown()` before process exit.
- `shutdown()` flushes generation batches and closes generation exporter resources.
- `flush()` is available for explicit synchronization points.

## Instrumentation-only mode (no generation send)

```java
SigilClient client = new SigilClient(new SigilClientConfig()
    .setGenerationExport(new GenerationExportConfig()
        .setProtocol(GenerationExportProtocol.NONE)));
```

## SDK metrics

The SDK emits these OTel histograms through your configured OTel meter provider:

- `gen_ai.client.operation.duration`
- `gen_ai.client.token.usage`
- `gen_ai.client.time_to_first_token`
- `gen_ai.client.tool_calls_per_operation`

## Provider Wrappers

Provider modules are wrapper-first for ergonomics, with explicit mapper APIs for full control:

- OpenAI: `providers/openai/README.md`
- Anthropic: `providers/anthropic/README.md`
- Gemini: `providers/gemini/README.md`

Framework helpers:

- Google ADK: `frameworks/google-adk/README.md`

## Environment variables

The SDK reads `SIGIL_*` environment variables at client construction. Caller-supplied
fields on `SigilClientConfig` win; env vars fill anything left at the default;
SDK schema defaults fill the rest.

| Env var | Field |
| --- | --- |
| `SIGIL_ENDPOINT` | `GenerationExportConfig.endpoint` |
| `SIGIL_PROTOCOL` | `GenerationExportConfig.protocol` (`http`/`grpc`/`none`) |
| `SIGIL_INSECURE` | `GenerationExportConfig.insecure` (tri-state) |
| `SIGIL_HEADERS` | `GenerationExportConfig.headers` (CSV: `K=V,...`) |
| `SIGIL_AUTH_MODE` | `AuthConfig.mode` (`none`/`tenant`/`bearer`/`basic`) |
| `SIGIL_AUTH_TENANT_ID` | `AuthConfig.tenantId` |
| `SIGIL_AUTH_TOKEN` | `AuthConfig.bearerToken` and/or `basicPassword` (filled when empty) |
| `SIGIL_AGENT_NAME` | `SigilClientConfig.agentName` |
| `SIGIL_AGENT_VERSION` | `SigilClientConfig.agentVersion` |
| `SIGIL_USER_ID` | `SigilClientConfig.userId` |
| `SIGIL_TAGS` | `SigilClientConfig.tags` (CSV; merged into generation export only. Only Go/JS emit `sigil.tag.<key>` on spans/metrics; see [Tags and Metadata](../docs/concepts/tags-and-metadata.md)) |
| `SIGIL_CONTENT_CAPTURE_MODE` | `SigilClientConfig.contentCapture` |
| `SIGIL_DEBUG` | `SigilClientConfig.debug` (tri-state) |

Use `SigilEnvConfig.fromEnv()` to inspect the resolved config without
constructing a client. Invalid values (bad auth mode, etc.) are skipped with a
warning so a single typo does not discard the rest of the env layer.

## Breaking changes (unreleased)

- `GenerationExportConfig.insecure` is now tri-state (`Boolean` instead of
  `boolean`). The default flips from `true` to `false` (TLS on) when neither
  caller nor `SIGIL_INSECURE` provides a value. Existing callers that call
  `setInsecure(true)` keep working through autoboxing. The previous
  `isInsecure()` boolean accessor was replaced by `getInsecure()`
  (returns `Boolean`) and `isInsecureResolved()` (returns `boolean`,
  treats null as TLS on).
- `AuthHeaders.resolve` no longer rejects mode-irrelevant fields (e.g.
  `tenantId` set under `mode=BEARER`). This matches Go/JS/Python and lets env
  layering populate any field independently of mode.

## Best Practices

- Keep raw artifacts disabled in production unless actively debugging.
- Prefer callback APIs (`withGeneration` / `withStreamingGeneration`) to guarantee `end()` runs.
- Configure traces/metrics in your OpenTelemetry SDK setup (or inject `Tracer`/`Meter` into `SigilClientConfig`).
- Keep `batchSize` and `queueSize` conservative first, then tune with benchmark data.

## Build, Test, Benchmark

From `sdks/java`:

```bash
./gradlew test
./gradlew :benchmarks:jmh
```

Or from repo root:

```bash
mise run test:java:sdk-all
mise run benchmark:java:sdk
```
