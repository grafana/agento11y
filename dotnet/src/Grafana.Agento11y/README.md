# Grafana.Agento11y

Core runtime for normalized generation export and OpenTelemetry-aligned instrumentation.

## Install

```bash
dotnet add package Grafana.Agento11y
```

## Configure `SigilClient`

```csharp
using Grafana.Sigil;

var client = new SigilClient(new SigilClientConfig
{
    GenerationExport = new GenerationExportConfig
    {
        Protocol = GenerationExportProtocol.Http,
        Endpoint = "https://sigil-prod-<region>.grafana.net",
        Auth = new AuthConfig
        {
            Mode = ExportAuthMode.Basic,
            TenantId = Environment.GetEnvironmentVariable("AGENTO11Y_AUTH_TENANT_ID"),
            BasicPassword = Environment.GetEnvironmentVariable("AGENTO11Y_AUTH_TOKEN"),
        },
        BatchSize = 100,
        FlushInterval = TimeSpan.FromSeconds(1),
        QueueSize = 2000,
        MaxRetries = 5,
        InitialBackoff = TimeSpan.FromMilliseconds(100),
        MaxBackoff = TimeSpan.FromSeconds(5),
    },
    Api = new ApiConfig
    {
        Endpoint = "https://sigil-prod-<region>.grafana.net",
    },
});
```

Configure OTEL exporters (traces/metrics) separately in your application's OTEL setup.

Quick OTEL setup pattern before creating the Sigil client:

```csharp
using OpenTelemetry.Metrics;
using OpenTelemetry.Trace;

using var tracerProvider = Sdk.CreateTracerProviderBuilder()
    .AddSource("github.com/grafana/sigil/sdks/dotnet")
    .AddOtlpExporter()
    .Build();

using var meterProvider = Sdk.CreateMeterProviderBuilder()
    .AddMeter("github.com/grafana/sigil/sdks/dotnet")
    .AddOtlpExporter()
    .Build();
```

Generation export auth is configured in `GenerationExport.Auth`.
`Api.Endpoint` configures helper API calls such as `SubmitConversationRatingAsync(...)`.

Generation export transport protocols:

- `GenerationExportProtocol.Grpc`
- `GenerationExportProtocol.Http`
- `GenerationExportProtocol.None` (instrumentation-only; no generation transport)

## Instrumentation-only mode (no generation send)

```csharp
var client = new SigilClient(new SigilClientConfig
{
    GenerationExport = new GenerationExportConfig
    {
        Protocol = GenerationExportProtocol.None,
    },
});
```

## Secrets redaction

Use the built-in sanitizer to redact high-confidence secret formats before
generation data is exported:

```csharp
var client = new SigilClient(new SigilClientConfig
{
    GenerationSanitizer = SecretRedactionSanitizer.Create(),
});
```

User input messages are not redacted by default. To opt in:

```csharp
var client = new SigilClient(new SigilClientConfig
{
    GenerationSanitizer = SecretRedactionSanitizer.Create(new SecretRedactionOptions
    {
        RedactInputMessages = true,
    }),
});
```

You can also set `AGENTO11Y_REDACT_INPUT_MESSAGES=true`. Explicit options take
precedence over the environment variable.

## Manual generation instrumentation (sync)

Use this API for unsupported providers or custom pipelines.

```csharp
var recorder = client.StartGeneration(new GenerationStart
{
    ConversationId = "conv-9b2f",
    AgentName = "assistant-core",
    AgentVersion = "1.0.0",
    Model = new ModelRef
    {
        Provider = "openai",
        Name = "gpt-5",
    },
});

try
{
    var providerResponseText = await Task.FromResult("Paris is 18C and sunny.");

    recorder.SetResult(new Generation
    {
        Input = new List<Message>
        {
            Message.UserTextMessage("Give me a short weather summary for Paris."),
        },
        Output = new List<Message>
        {
            Message.AssistantTextMessage(providerResponseText),
        },
        Usage = new TokenUsage
        {
            InputTokens = 120,
            OutputTokens = 42,
            TotalTokens = 162,
        },
        StopReason = "stop",
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

## Manual generation instrumentation (stream)

```csharp
var recorder = client.StartStreamingGeneration(new GenerationStart
{
    ConversationId = "conv-stream",
    AgentName = "assistant-core",
    AgentVersion = "1.0.0",
    Model = new ModelRef
    {
        Provider = "openai",
        Name = "gpt-5",
    },
});

try
{
    var chunks = new List<string>();
    await foreach (var chunk in StreamAsync())
    {
        chunks.Add(chunk);
    }

    recorder.SetResult(new Generation
    {
        Input = new List<Message>
        {
            Message.UserTextMessage("Stream a short weather summary for Paris."),
        },
        Output = new List<Message>
        {
            Message.AssistantTextMessage(string.Concat(chunks)),
        },
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

static async IAsyncEnumerable<string> StreamAsync()
{
    yield return "Paris is ";
    yield return "18C and sunny.";
    await Task.CompletedTask;
}
```

## Tool execution instrumentation

```csharp
var tool = client.StartToolExecution(new ToolExecutionStart
{
    ToolName = "weather",
    ToolCallId = "call_weather_1",
    ToolType = "function",
    ToolDescription = "Get weather by city",
    ConversationId = "conv-9b2f",
    AgentName = "assistant-core",
    AgentVersion = "1.0.0",
    IncludeContent = true,
});

try
{
    var args = new { city = "Paris" };
    var result = await Task.FromResult<object>(new
    {
        city = "Paris",
        tempC = 18,
        condition = "sunny",
    });

    tool.SetResult(new ToolExecutionEnd
    {
        Arguments = args,
        Result = result,
    });
}
catch (Exception ex)
{
    tool.SetExecutionError(ex);
    throw;
}
finally
{
    tool.End();
}
```

## Context defaults with async-local scopes

```csharp
using var _conversation = SigilContext.WithConversationId("conv-default");
using var _agentName = SigilContext.WithAgentName("assistant-core");
using var _agentVersion = SigilContext.WithAgentVersion("1.0.0");

var recorder = client.StartGeneration(new GenerationStart
{
    Model = new ModelRef
    {
        Provider = "openai",
        Name = "gpt-5",
    },
});
```

If `ConversationId`, `AgentName`, or `AgentVersion` are omitted in `GenerationStart` or `ToolExecutionStart`, the current `SigilContext` values are applied.

## Error semantics

- `SetCallError(...)` records provider-call failures as generation `CallError` and span status.
- `SetResult(..., mappingError)` lets you keep generation data while marking mapper failures.
- `GenerationRecorder.Error` and `ToolExecutionRecorder.Error` represent local SDK failures only (validation/enqueue/shutdown path), not provider-call failures.
- Generation/tool spans always include:
  - `sigil.sdk.name=sdk-dotnet`
- Normalized generation metadata always includes the same key.
- If caller metadata provides a conflicting value for this key, the SDK overwrites it.

## Auth modes and header precedence

Per export path, supported auth modes are:

- `ExportAuthMode.None`
- `ExportAuthMode.Tenant` (`X-Scope-OrgID`)
- `ExportAuthMode.Bearer` (`Authorization: Bearer <token>`)
- `ExportAuthMode.Basic` (requires `BasicPassword` + `BasicUser` or `TenantId`, injects `Authorization: Basic <base64(user:password)>`; also injects `X-Scope-OrgID` when `TenantId` is set — for multi-tenant deployments only, not needed for Grafana Cloud)

Explicit transport headers take precedence over auth-derived headers (`Authorization`, `X-Scope-OrgID`, case-insensitive).

### Grafana Cloud auth (basic)

For Grafana Cloud, use `Basic` auth mode. The username is your Grafana Cloud instance/tenant ID and the password is your Grafana Cloud API key:

```csharp
Auth = new AuthConfig
{
    Mode = ExportAuthMode.Basic,
    TenantId = Environment.GetEnvironmentVariable("AGENTO11Y_AUTH_TENANT_ID") ?? "",
    BasicPassword = Environment.GetEnvironmentVariable("AGENTO11Y_AUTH_TOKEN") ?? "",
},
```

If your deployment requires a distinct username, set `BasicUser` explicitly:

```csharp
Auth = new AuthConfig
{
    Mode = ExportAuthMode.Basic,
    TenantId = Environment.GetEnvironmentVariable("AGENTO11Y_AUTH_TENANT_ID") ?? "",
    BasicUser = Environment.GetEnvironmentVariable("AGENTO11Y_AUTH_TENANT_ID") ?? "",
    BasicPassword = Environment.GetEnvironmentVariable("AGENTO11Y_AUTH_TOKEN") ?? "",
},
```

## Lifecycle and performance guidance

- Reuse one `SigilClient` for the process lifetime.
- Call `ShutdownAsync(...)` on graceful shutdown to flush pending generation batches.
- Use `FlushAsync(...)` before short-lived jobs exit if needed.
- Keep raw artifacts disabled by default in production.
