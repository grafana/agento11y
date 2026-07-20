# Grafana.Agento11y.Gemini

Google Gemini GenerateContent instrumentation helpers for `Grafana.Agento11y`.

## Public API

- `GeminiRecorder.GenerateContentAsync(...)`
- `GeminiRecorder.GenerateContentStreamAsync(...)`
- `GeminiRecorder.EmbedContentAsync(...)`
- `GeminiGenerationMapper.FromRequestResponse(...)`
- `GeminiGenerationMapper.FromStream(...)`
- `GeminiGenerationMapper.EmbeddingFromResponse(...)`

## Install

```bash
dotnet add package Grafana.Agento11y
dotnet add package Grafana.Agento11y.Gemini
dotnet add package Google.GenAI
```

## Sync wrapper (`GenerateContentAsync`)

```csharp
using Google.GenAI;
using Google.GenAI.Types;
using Grafana.Sigil;
using Grafana.Sigil.Gemini;
using GPart = Google.GenAI.Types.Part;

var sigilConfig = new SigilClientConfig
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
var sigil = new SigilClient(sigilConfig);

var gemini = new Client(apiKey: Environment.GetEnvironmentVariable("GEMINI_API_KEY")!);

var model = "gemini-2.5-pro";
var contents = new List<Content>
{
    new Content
    {
        Role = "user",
        Parts = new List<GPart>
        {
            new GPart { Text = "What's the weather in Paris?" },
        },
    },
};
var config = new GenerateContentConfig
{
    SystemInstruction = new Content
    {
        Role = "user",
        Parts = new List<GPart>
        {
            new GPart { Text = "Be concise." },
        },
    },
};

GenerateContentResponse response = await GeminiRecorder.GenerateContentAsync(
    sigil,
    gemini,
    model,
    contents,
    config,
    options: new GeminiSigilOptions
    {
        ConversationId = "conv-gemini-1",
        AgentName = "assistant-core",
        AgentVersion = "1.0.0",
    },
    cancellationToken: CancellationToken.None
);
```

## Stream wrapper (`GenerateContentStreamAsync`)

```csharp
GeminiStreamSummary summary = await GeminiRecorder.GenerateContentStreamAsync(
    sigil,
    gemini,
    model,
    contents,
    config,
    options: new GeminiSigilOptions
    {
        ConversationId = "conv-gemini-stream-1",
        AgentName = "assistant-core",
        AgentVersion = "1.0.0",
    },
    cancellationToken: CancellationToken.None
);

foreach (var update in summary.Responses)
{
    // Consume incremental GenerateContentResponse payloads.
}
```

The wrapper records mode as `STREAM` and aggregates the normalized generation from collected responses.

## Embedding wrapper (`EmbedContentAsync`)

```csharp
EmbedContentResponse embeddingResponse = await GeminiRecorder.EmbedContentAsync(
    sigil,
    gemini,
    "gemini-embedding-001",
    new List<Content>
    {
        new() { Parts = new List<GPart> { new() { Text = "hello" } } },
        new() { Parts = new List<GPart> { new() { Text = "world" } } },
    },
    config: null,
    options: new GeminiSigilOptions
    {
        ConversationId = "conv-gemini-embeddings-1",
        AgentName = "assistant-core",
        AgentVersion = "1.0.0",
    },
    cancellationToken: CancellationToken.None
);
```

## Raw artifacts (debug opt-in)

```csharp
var sigilOptions = new GeminiSigilOptions
{
    ConversationId = "conv-gemini-debug-1",
    AgentName = "assistant-core",
    AgentVersion = "1.0.0",
}.WithRawArtifacts();
```

Raw artifacts are off by default and should be enabled only for diagnostics.

## Delegate overload for custom call pipelines

```csharp
var response = await GeminiRecorder.GenerateContentAsync(
    sigil,
    model,
    contents,
    (requestModel, requestContents, requestConfig, ct) => gemini.Models.GenerateContentAsync(requestModel, requestContents, requestConfig, ct),
    config,
    options: new GeminiSigilOptions { ModelName = "gemini-2.5-pro" },
    cancellationToken: CancellationToken.None
);
```

## Behavior notes

- Wrapper sets generation mode automatically (`SYNC` or `STREAM`).
- Candidate text, tool calls, and function responses map to normalized Sigil message parts.
- Stop reason and usage fields are normalized from Gemini responses.
- Provider exceptions are captured as generation `CallError` and rethrown.
- Call `SigilClient.ShutdownAsync(...)` during application shutdown to flush pending exports.

## Provider metadata mapping

Gemini-specific fields are mapped as follows:

- `usage.ThoughtsTokenCount` -> normalized `usage.reasoning_tokens`
- `usage.ToolUsePromptTokenCount` -> metadata `sigil.gen_ai.usage.tool_use_prompt_tokens`
- `config.ThinkingConfig.ThinkingBudget` -> metadata `sigil.gen_ai.request.thinking.budget_tokens`
- `config.ThinkingConfig.ThinkingLevel` -> metadata `sigil.gen_ai.request.thinking.level`
