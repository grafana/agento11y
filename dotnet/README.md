# Grafana Sigil .NET SDKs

Sigil extends OpenTelemetry-style instrumentation with normalized AI generation records.

## Packages

- `Grafana.Sigil`: core runtime (`SigilClient`, generation/tool recorders, generation export)
- `Grafana.Sigil.OpenAI`: OpenAI Responses + Chat Completions + Embeddings wrappers and mappers
- `Grafana.Sigil.Anthropic`: Anthropic Messages wrappers and mappers
- `Grafana.Sigil.Gemini`: Gemini GenerateContent + EmbedContent wrappers and mappers

Package docs:

- Core: [`src/Grafana.Sigil/README.md`](src/Grafana.Sigil/README.md)
- OpenAI: [`src/Grafana.Sigil.OpenAI/README.md`](src/Grafana.Sigil.OpenAI/README.md)
- Anthropic: [`src/Grafana.Sigil.Anthropic/README.md`](src/Grafana.Sigil.Anthropic/README.md)
- Gemini: [`src/Grafana.Sigil.Gemini/README.md`](src/Grafana.Sigil.Gemini/README.md)

## Target frameworks

- Core package: `net8.0`, `netstandard2.0`
- Provider packages: `net8.0`

## Install

```bash
dotnet add package Grafana.Sigil
dotnet add package Grafana.Sigil.OpenAI
# or: Grafana.Sigil.Anthropic / Grafana.Sigil.Gemini
```

For a Grafana Cloud setup walkthrough (where to find the endpoint URL, instance ID, and API token), refer to the [Grafana Cloud setup guide](https://grafana.com/docs/grafana-cloud/machine-learning/ai-observability/get-started/grafana-cloud/).

## Quickstart (OpenAI Responses wrapper)

The snippet below configures the SDK explicitly. As an alternative, set `SIGIL_*` environment variables and call `new SigilClient()` with no arguments — refer to the [environment variables](#environment-variables) section.

```csharp
using Grafana.Sigil;
using Grafana.Sigil.OpenAI;
using OpenAI.Responses;

var sigil = new SigilClient(new SigilClientConfig
{
    GenerationExport = new GenerationExportConfig
    {
        Protocol = GenerationExportProtocol.Http,
        Endpoint = "http://localhost:8080",
        Auth = new AuthConfig
        {
            Mode = ExportAuthMode.Tenant,
            TenantId = "dev-tenant",
        },
        BatchSize = 100,
        FlushInterval = TimeSpan.FromSeconds(1),
        QueueSize = 2000,
    },
    Api = new ApiConfig
    {
        Endpoint = "http://localhost:8080",
    },
});

// Configure OTel exporters (traces/metrics) separately in your application's OTel setup.
using var tracerProvider = OpenTelemetry.Sdk.CreateTracerProviderBuilder()
    .AddSource("github.com/grafana/sigil/sdks/dotnet")
    .AddOtlpExporter()
    .Build();

using var meterProvider = OpenTelemetry.Sdk.CreateMeterProviderBuilder()
    .AddMeter("github.com/grafana/sigil/sdks/dotnet")
    .AddOtlpExporter()
    .Build();

var openAI = new ResponsesClient(
    "gpt-5",
    Environment.GetEnvironmentVariable("OPENAI_API_KEY")!
);

var inputItems = new List<ResponseItem>
{
    ResponseItem.CreateUserMessageItem("Give me a short weather summary for Paris."),
};

var requestOptions = new CreateResponseOptions
{
    Instructions = "You are concise.",
    MaxOutputTokenCount = 320,
};

var response = await OpenAIRecorder.CreateResponseAsync(
    sigil,
    openAI,
    inputItems,
    requestOptions: requestOptions,
    options: new OpenAISigilOptions
    {
        ConversationId = "conv-9b2f",
        AgentName = "assistant-core",
        AgentVersion = "1.0.0",
    },
    cancellationToken: CancellationToken.None
);

await sigil.ShutdownAsync(CancellationToken.None);
```

## Core API

- `StartGeneration(...)` for non-stream operations (`SYNC`)
- `StartStreamingGeneration(...)` for stream operations (`STREAM`)
- `StartEmbedding(...)` for embedding operations (`embeddings`)
- `StartToolExecution(...)` for tool spans/events
- `FlushAsync(...)` for explicit export flush points
- `ShutdownAsync(...)` for graceful shutdown

Recorder behavior is explicit and idempotent:

- `GenerationRecorder.SetResult(...)` and `SetCallError(...)`
- `ToolExecutionRecorder.SetResult(...)` and `SetExecutionError(...)`
- `End()` is safe to call once in `finally`
- recorder `Error` only reports local instrumentation/export-queue errors

Generation export transport protocols:

- `GenerationExportProtocol.Grpc`
- `GenerationExportProtocol.Http`
- `GenerationExportProtocol.None` (instrumentation-only; no generation transport)

## Secrets redaction

Use the built-in secrets sanitizer to redact high-confidence secret formats
before generation data is exported. It uses the same gitleaks-derived pattern
set as the other Sigil SDKs and replaces matches with
`[REDACTED:<category>]` placeholders.

```csharp
using Grafana.Sigil;

var sigil = new SigilClient(new SigilClientConfig
{
    GenerationSanitizer = SecretRedactionSanitizer.Create(),
});
```

By default, the sanitizer redacts assistant output, thinking blocks, tool call
arguments, tool results, system prompts, conversation titles, provider call
errors, and email addresses. User input messages are left unchanged unless you
opt in:

```csharp
var sigil = new SigilClient(new SigilClientConfig
{
    GenerationSanitizer = SecretRedactionSanitizer.Create(new SecretRedactionOptions
    {
        RedactInputMessages = true,
    }),
});
```

You can also set `SIGIL_REDACT_INPUT_MESSAGES=true`. An explicit
`RedactInputMessages` value takes precedence over the environment variable.

## Embedding observability

Use `StartEmbedding(...)` for manual embedding instrumentation. Embedding recording emits OTel spans and SDK metrics only, and does not enqueue generation export payloads.

```csharp
var recorder = client.StartEmbedding(new EmbeddingStart
{
    AgentName = "retrieval-worker",
    AgentVersion = "1.0.0",
    Model = new ModelRef
    {
        Provider = "openai",
        Name = "text-embedding-3-small",
    },
});

try
{
    var response = await embeddingClient.GenerateEmbeddingsAsync(new[] { "hello", "world" });
    recorder.SetResult(new EmbeddingResult
    {
        InputCount = 2,
        InputTokens = response.Value.Usage.InputTokenCount,
        InputTexts = new List<string> { "hello", "world" }, // captured only when EmbeddingCapture.CaptureInput=true
        ResponseModel = response.Value.Model,
    });
}
catch (Exception ex)
{
    recorder.SetCallError(ex);
    throw;
}
finally
{
    recorder.End();
}
```

Provider helpers:

- `OpenAIRecorder.GenerateEmbeddingsAsync(...)`
- `GeminiRecorder.EmbedContentAsync(...)`
- Anthropic .NET SDK does not currently expose an embeddings API surface in this repository.

Input text capture is opt-in:

```csharp
var config = new SigilClientConfig
{
    EmbeddingCapture = new EmbeddingCaptureConfig
    {
        CaptureInput = true,
        MaxInputItems = 20,
        MaxTextLength = 1024,
    },
};
```

`CaptureInput` can expose PII/document content in spans. Keep it disabled by default and enable only for scoped diagnostics.

TraceQL examples:

- `traces{gen_ai.operation.name="embeddings"}`
- `traces{gen_ai.operation.name="embeddings" && gen_ai.request.model="text-embedding-3-small"}`
- `traces{gen_ai.operation.name="embeddings" && error.type!=""}`

## Content capture

`ContentCaptureMode` controls what content the SDK includes in exported generation payloads and OTel span attributes. See [Content Capture Modes](../docs/concepts/content-capture-modes.md) for the canonical mode matrix and defaults; the snippets below show how to wire it up in C#.

Client-level default:

```csharp
var sigil = new SigilClient(new SigilClientConfig
{
    ContentCapture = ContentCaptureMode.MetadataOnly,
});
```

The core SDK client treats `ContentCaptureMode.Default` as `ContentCaptureMode.NoToolContent`: generation content is captured but tool-execution arguments and results stay out of spans.

Per-generation override:

```csharp
var recorder = client.StartGeneration(new GenerationStart
{
    Model = new ModelRef { Provider = "openai", Name = "gpt-5" },
    ContentCapture = ContentCaptureMode.Full,
});
```

Per-tool-execution override (here `Full` opts into capturing tool arguments and results in the span):

```csharp
var tool = client.StartToolExecution(new ToolExecutionStart
{
    ToolName = "search",
    ContentCapture = ContentCaptureMode.Full,
});
```

Dynamic resolution via `ContentCaptureResolver`:

```csharp
var sigil = new SigilClient(new SigilClientConfig
{
    ContentCaptureResolver = metadata =>
    {
        if (metadata != null && metadata.TryGetValue("sigil.tenant", out var tenant) && (string?)tenant == "healthcare")
        {
            return ContentCaptureMode.MetadataOnly;
        }
        return ContentCaptureMode.Default; // defer to ContentCapture
    },
});
```

Exception-throwing resolvers are caught and treated as `ContentCaptureMode.MetadataOnly` (fail-closed).

Resolution precedence for generations (highest to lowest):

1. Per-generation `GenerationStart.ContentCapture`
2. `SigilClientConfig.ContentCaptureResolver` return value
3. `SigilClientConfig.ContentCapture` (defaults to `ContentCaptureMode.NoToolContent`)

Resolution precedence for tool executions (highest to lowest):

1. Per-tool `ToolExecutionStart.ContentCapture`
2. Parent generation's resolved mode
3. `SigilClientConfig.ContentCaptureResolver` return value
4. `SigilClientConfig.ContentCapture` (defaults to `ContentCaptureMode.NoToolContent`)

User-provided `Metadata` and `Tags` are not stripped by any capture mode. SDK-internal metadata keys that carry content (e.g. `call_error`, `sigil.conversation.title`) are stripped along with the matching content. See [Tags and Metadata](../docs/concepts/tags-and-metadata.md) for where client tags, per-generation tags, metadata, and `UserId` each show up. .NET merges client tags into the generation export only; it does not emit `sigil.tag.<key>` on spans/metrics.

## Context defaults

`SigilContext` uses async-local scopes:

- `SigilContext.WithConversationId(...)`
- `SigilContext.WithAgentName(...)`
- `SigilContext.WithAgentVersion(...)`

These defaults are used when a start payload omits those fields.

## Conversation Ratings

Use the SDK helper to submit user-facing ratings:

```csharp
var result = await client.SubmitConversationRatingAsync(
    "conv-123",
    new SubmitConversationRatingRequest
    {
        RatingId = "rat-123",
        Rating = ConversationRatingValue.Bad,
        Comment = "Answer ignored user context",
        Metadata = new Dictionary<string, object?>
        {
            ["channel"] = "assistant-ui",
        },
        Source = "sdk-dotnet",
    },
    CancellationToken.None
);

Console.WriteLine($"{result.Rating.Rating} hasBad={result.Summary.HasBadRating}");
```

`SubmitConversationRatingAsync(...)` sends requests to `SigilClientConfig.Api.Endpoint` (default `http://localhost:8080`) and uses the same generation-export auth headers (`tenant` or `bearer`) already configured on the SDK client.

## .NET best practices

- Create one long-lived `SigilClient` per process (for example as a singleton in DI).
- Always call `ShutdownAsync(...)` during process shutdown.
- Keep provider request/response payloads normalized; enable raw artifacts only for debug sessions.
- Use explicit generation export auth config instead of sharing ad-hoc headers.

## Instrumentation-only mode (no generation send)

```csharp
var sigil = new SigilClient(new SigilClientConfig
{
    GenerationExport = new GenerationExportConfig
    {
        Protocol = GenerationExportProtocol.None,
    },
});
```

## SDK metrics

The SDK emits these OTel histograms through your configured OTel meter provider:

- `gen_ai.client.operation.duration`
- `gen_ai.client.token.usage`
- `gen_ai.client.time_to_first_token`
- `gen_ai.client.tool_calls_per_operation`

## Environment variables

The SDK reads `SIGIL_*` environment variables at client construction. Caller-supplied
fields on `SigilClientConfig` win; environment variables fill anything left at the default;
SDK schema defaults fill the rest.

| Env var | Field |
| --- | --- |
| `SIGIL_ENDPOINT` | `GenerationExportConfig.Endpoint` |
| `SIGIL_PROTOCOL` | `GenerationExportConfig.Protocol` (`http`/`grpc`/`none`) |
| `SIGIL_INSECURE` | `GenerationExportConfig.Insecure` (tri-state `bool?`) |
| `SIGIL_HEADERS` | `GenerationExportConfig.Headers` (CSV: `K=V,...`) |
| `SIGIL_AUTH_MODE` | `AuthConfig.Mode` (`none`/`tenant`/`bearer`/`basic`) |
| `SIGIL_AUTH_TENANT_ID` | `AuthConfig.TenantId` |
| `SIGIL_AUTH_TOKEN` | `AuthConfig.BearerToken` and/or `BasicPassword` (filled when empty) |
| `SIGIL_AGENT_NAME` | `SigilClientConfig.AgentName` |
| `SIGIL_AGENT_VERSION` | `SigilClientConfig.AgentVersion` |
| `SIGIL_USER_ID` | `SigilClientConfig.UserId` |
| `SIGIL_TAGS` | `SigilClientConfig.Tags` (CSV; merged into generation export only. Only Go/JS emit `sigil.tag.<key>` on spans/metrics; see [Tags and Metadata](../docs/concepts/tags-and-metadata.md)) |
| `SIGIL_CONTENT_CAPTURE_MODE` | `SigilClientConfig.ContentCapture` |
| `SIGIL_DEBUG` | `SigilClientConfig.Debug` (tri-state `bool?`) |

Use `EnvConfig.FromEnv()` to inspect the resolved config without constructing a
client. Invalid values (bad auth mode, etc.) are skipped with a warning so a
single typo does not discard the rest of the env layer.

## Breaking changes (unreleased)

- `GenerationExportConfig.Insecure` is now `bool?` instead of `bool`. The
  default flips from `true` to `false` (TLS on) when neither caller nor
  `SIGIL_INSECURE` provides a value. Code that reads the property as a plain
  `bool` needs to coalesce (e.g. `cfg.Insecure ?? false`).
- `ConfigResolver.ResolveHeadersWithAuth` no longer rejects mode-irrelevant
  fields (e.g. `TenantId` set under `Mode = Bearer`). This matches Go/JS/Python
  and lets env layering populate any field independently of mode.

## Local tasks

Run from repository root:

- `mise run test:cs:sdk-core`
- `mise run test:cs:sdk-openai`
- `mise run test:cs:sdk-anthropic`
- `mise run test:cs:sdk-gemini`
