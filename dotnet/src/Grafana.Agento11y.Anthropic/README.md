# Grafana.Agento11y.Anthropic

Anthropic Messages API instrumentation helpers for `Grafana.Agento11y`.

## Embeddings support

This helper currently supports Anthropic Messages APIs only. Native Anthropic embeddings endpoints are not available in the official SDK/API surface used in this repository.

## Install

```bash
dotnet add package Grafana.Agento11y
dotnet add package Grafana.Agento11y.Anthropic
dotnet add package Anthropic
```

## Sync wrapper (`MessageAsync`)

```csharp
using Anthropic;
using Anthropic.Models.Messages;
using Grafana.Agento11y;
using Grafana.Agento11y.Anthropic;

var agento11yConfig = new Agento11yClientConfig
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
    },
    Api = new ApiConfig
    {
        Endpoint = "https://sigil-prod-<region>.grafana.net",
    },
};
var agento11y = new Agento11yClient(agento11yConfig);

var anthropic = new AnthropicClient(new Anthropic.Core.ClientOptions
{
    APIKey = Environment.GetEnvironmentVariable("ANTHROPIC_API_KEY")!,
});

var request = new MessageCreateParams
{
    Model = Model.ClaudeSonnet4_5,
    MaxTokens = 512,
    System = "Be concise.",
    Messages = new List<MessageParam>
    {
        new MessageParam
        {
            Role = Role.User,
            Content = "What's the weather in Paris?",
        },
    },
};

var response = await AnthropicRecorder.MessageAsync(
    agento11y,
    anthropic,
    request,
    options: new AnthropicAgento11yOptions
    {
        ConversationId = "conv-anthropic-1",
        AgentName = "assistant-core",
        AgentVersion = "1.0.0",
    },
    cancellationToken: CancellationToken.None
);
```

## Stream wrapper (`MessageStreamAsync`)

```csharp
AnthropicStreamSummary summary = await AnthropicRecorder.MessageStreamAsync(
    agento11y,
    anthropic,
    request,
    options: new AnthropicAgento11yOptions
    {
        ConversationId = "conv-anthropic-stream-1",
        AgentName = "assistant-core",
        AgentVersion = "1.0.0",
    },
    cancellationToken: CancellationToken.None
);

foreach (var streamEvent in summary.Events)
{
    // Inspect raw stream events if needed.
}
```

The wrapper records mode as `STREAM` and aggregates final usage/stop-reason/output fields.

## Raw artifacts (debug opt-in)

```csharp
var agento11yOptions = new AnthropicAgento11yOptions
{
    ConversationId = "conv-anthropic-debug-1",
    AgentName = "assistant-core",
    AgentVersion = "1.0.0",
}.WithRawArtifacts();
```

Raw artifacts are off by default and should be enabled only for troubleshooting.

## Delegate overload for custom call pipelines

```csharp
var response = await AnthropicRecorder.MessageAsync(
    agento11y,
    request,
    (payload, ct) => anthropic.Messages.Create(payload, ct),
    options: new AnthropicAgento11yOptions { ModelName = "claude-sonnet-4-5" },
    cancellationToken: CancellationToken.None
);
```

## Behavior notes

- Wrapper sets generation mode automatically (`SYNC` or `STREAM`).
- `System` prompt is normalized into `Generation.SystemPrompt`.
- Thinking blocks, tool-use blocks, and tool-result blocks map to typed Sigil parts.
- Provider exceptions are captured as generation `CallError` and rethrown.
- Call `Agento11yClient.ShutdownAsync(...)` during application shutdown to flush pending exports.

## Provider metadata mapping

In addition to normalized usage fields, Anthropic server-tool counters are mapped into Sigil metadata when present:

- `agento11y.gen_ai.usage.server_tool_use.web_search_requests`
- `agento11y.gen_ai.usage.server_tool_use.web_fetch_requests`
- `agento11y.gen_ai.usage.server_tool_use.total_requests`
