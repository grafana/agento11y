# Grafana.Agento11y.OpenAI

OpenAI instrumentation helpers for `Grafana.Agento11y` with strict official OpenAI .NET SDK types for both:

- Chat Completions
- Responses

## Integration styles

- Strict wrappers: call OpenAI and record in one step.
- Manual instrumentation: call OpenAI directly, then map strict OpenAI request/response payloads with `OpenAIGenerationMapper`.

## Public API

- Wrappers:
  - `OpenAIRecorder.CompleteChatAsync(...)`
  - `OpenAIRecorder.CompleteChatStreamingAsync(...)`
  - `OpenAIRecorder.CreateResponseAsync(...)`
  - `OpenAIRecorder.CreateResponseStreamingAsync(...)`
  - `OpenAIRecorder.GenerateEmbeddingsAsync(...)`
- Mappers:
  - `OpenAIGenerationMapper.ChatCompletionsFromRequestResponse(...)`
  - `OpenAIGenerationMapper.ChatCompletionsFromStream(...)`
  - `OpenAIGenerationMapper.ResponsesFromRequestResponse(...)`
  - `OpenAIGenerationMapper.ResponsesFromStream(...)`
  - `OpenAIGenerationMapper.EmbeddingsFromRequestResponse(...)`

## Install

```bash
dotnet add package Grafana.Agento11y
dotnet add package Grafana.Agento11y.OpenAI
dotnet add package OpenAI
```

## Responses Wrapper (Sync)

```csharp
using Grafana.Agento11y;
using Grafana.Agento11y.OpenAI;
using OpenAI.Responses;

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

var responsesClient = new ResponsesClient(
    "gpt-5",
    Environment.GetEnvironmentVariable("OPENAI_API_KEY")!
);

var inputItems = new List<ResponseItem>
{
    ResponseItem.CreateUserMessageItem("What's the weather in Paris?"),
};

var requestOptions = new CreateResponseOptions
{
    Instructions = "You are concise.",
    MaxOutputTokenCount = 320,
};

ResponseResult response = await OpenAIRecorder.CreateResponseAsync(
    agento11y,
    responsesClient,
    inputItems,
    requestOptions: requestOptions,
    options: new OpenAIAgento11yOptions
    {
        ConversationId = "conv-openai-responses-1",
        AgentName = "assistant-core",
        AgentVersion = "1.0.0",
    },
    cancellationToken: CancellationToken.None
);
```

## Responses Wrapper (Stream)

```csharp
OpenAIResponsesStreamSummary summary = await OpenAIRecorder.CreateResponseStreamingAsync(
    agento11y,
    responsesClient,
    inputItems,
    requestOptions: requestOptions,
    options: new OpenAIAgento11yOptions
    {
        ConversationId = "conv-openai-responses-stream-1",
        AgentName = "assistant-core",
        AgentVersion = "1.0.0",
    },
    cancellationToken: CancellationToken.None
);

foreach (var evt in summary.Events)
{
    // Inspect raw stream events if needed.
}
```

## Chat Completions Wrapper (Sync)

```csharp
using OpenAI.Chat;

var chatClient = new ChatClient(
    "gpt-5",
    Environment.GetEnvironmentVariable("OPENAI_API_KEY")!
);

var messages = new List<ChatMessage>
{
    new SystemChatMessage("You are concise."),
    new UserChatMessage("What's the weather in Paris?"),
};

ChatCompletion chat = await OpenAIRecorder.CompleteChatAsync(
    agento11y,
    chatClient,
    messages,
    requestOptions: null,
    options: new OpenAIAgento11yOptions
    {
        ConversationId = "conv-openai-chat-1",
        AgentName = "assistant-core",
        AgentVersion = "1.0.0",
    },
    cancellationToken: CancellationToken.None
);
```

## Chat Completions Wrapper (Stream)

```csharp
OpenAIChatCompletionsStreamSummary streamSummary = await OpenAIRecorder.CompleteChatStreamingAsync(
    agento11y,
    chatClient,
    messages,
    requestOptions: null,
    options: new OpenAIAgento11yOptions
    {
        ConversationId = "conv-openai-chat-stream-1",
        AgentName = "assistant-core",
        AgentVersion = "1.0.0",
    },
    cancellationToken: CancellationToken.None
);
```

## Embeddings Wrapper

```csharp
using OpenAI.Embeddings;

OpenAIEmbeddingCollection embeddingResponse = await OpenAIRecorder.GenerateEmbeddingsAsync(
    agento11y,
    new EmbeddingClient(Environment.GetEnvironmentVariable("OPENAI_API_KEY")!),
    new[] { "hello", "world" },
    requestOptions: new EmbeddingGenerationOptions { Dimensions = 256 },
    options: new OpenAIAgento11yOptions
    {
        ConversationId = "conv-openai-embeddings-1",
        AgentName = "assistant-core",
        AgentVersion = "1.0.0",
        ModelName = "text-embedding-3-small",
    },
    cancellationToken: CancellationToken.None
);
```

## Manual instrumentation example (strict mapper)

```csharp
var agento11yOptions = new OpenAIAgento11yOptions
{
    ConversationId = "conv-openai-manual-1",
    AgentName = "assistant-core",
    AgentVersion = "1.0.0",
};

var recorder = agento11y.StartGeneration(new GenerationStart
{
    ConversationId = agento11yOptions.ConversationId,
    AgentName = agento11yOptions.AgentName,
    AgentVersion = agento11yOptions.AgentVersion,
    Model = new ModelRef { Provider = "openai", Name = "gpt-5" },
});

try
{
    ResponseResult response = await responsesClient.CreateResponseAsync(
        inputItems,
        requestOptions,
        CancellationToken.None
    );

    recorder.SetResult(OpenAIGenerationMapper.ResponsesFromRequestResponse(
        "gpt-5",
        inputItems,
        requestOptions,
        response,
        agento11yOptions
    ));
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

## Raw Artifacts (Debug Opt-In)

Raw provider request/response/tools/stream-events artifacts are disabled by default.

```csharp
var agento11yOptions = new OpenAIAgento11yOptions
{
    ConversationId = "conv-openai-debug-1",
    AgentName = "assistant-core",
    AgentVersion = "1.0.0",
}.WithRawArtifacts();
```

## Delegate Overloads

All wrappers also provide delegate overloads so you can inject custom retry/transport/test-double behavior while keeping Sigil recording intact.
