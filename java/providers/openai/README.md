# Grafana Sigil Java OpenAI Provider Helpers

This module provides strict wrappers around official OpenAI Java SDK request/response types for both:

- Chat Completions
- Responses

No simplified OpenAI DTO layer is exposed.

## Public API

- Chat Completions:
  - `OpenAiChatCompletions.create(...)`
  - `OpenAiChatCompletions.createStreaming(...)`
  - `OpenAiChatCompletions.fromRequestResponse(...)`
  - `OpenAiChatCompletions.fromStream(...)`
- Responses:
  - `OpenAiResponses.create(...)`
  - `OpenAiResponses.createStreaming(...)`
  - `OpenAiResponses.fromRequestResponse(...)`
  - `OpenAiResponses.fromStream(...)`
- Embeddings:
  - `OpenAiEmbeddings.create(...)`
  - `OpenAiEmbeddings.fromRequestResponse(...)`

## Integration styles

- Strict wrappers: call OpenAI and record in one step.
- Manual instrumentation: call OpenAI directly, then map strict OpenAI request/response payloads with `fromRequestResponse` or `fromStream`.

## Official SDK Types

These wrappers accept and return official types from `com.openai:openai-java`:

- Chat: `ChatCompletionCreateParams`, `ChatCompletion`, `ChatCompletionChunk`
- Responses: `ResponseCreateParams`, `Response`, `ResponseStreamEvent`

## Chat Completions Example

```java
ChatCompletionCreateParams request = ChatCompletionCreateParams.builder()
    .model("gpt-5")
    .addSystemMessage("Be concise.")
    .addUserMessage("Summarize this run.")
    .build();

ChatCompletion response = OpenAiChatCompletions.create(
    agento11yClient,
    request,
    params -> openAI.chat().completions().create(params),
    new OpenAiOptions()
        .setConversationId("conv-1")
        .setAgentName("assistant-openai")
        .setAgentVersion("1.0.0")
);
```

## Responses Example

```java
ResponseCreateParams request = ResponseCreateParams.builder()
    .model("gpt-5")
    .instructions("Be concise.")
    .input("Summarize this run.")
    .build();

Response response = OpenAiResponses.create(
    agento11yClient,
    request,
    params -> openAI.responses().create(params),
    new OpenAiOptions()
        .setConversationId("conv-1")
        .setAgentName("assistant-openai")
        .setAgentVersion("1.0.0")
);
```

## Embeddings Example

```java
CreateEmbeddingResponse embeddingResponse = OpenAiEmbeddings.create(
    agento11yClient,
    EmbeddingCreateParams.builder()
        .model("text-embedding-3-small")
        .inputOfArrayOfStrings(java.util.List.of("hello", "world"))
        .build(),
    params -> openAI.embeddings().create(params),
    new OpenAiOptions()
        .setConversationId("conv-1")
        .setAgentName("assistant-openai")
        .setAgentVersion("1.0.0")
);
```

## Manual instrumentation example (strict mapper)

```java
OpenAiOptions options = new OpenAiOptions()
    .setConversationId("conv-1")
    .setAgentName("assistant-openai")
    .setAgentVersion("1.0.0");

var recorder = agento11yClient.startGeneration(new GenerationStart()
    .setConversationId(options.getConversationId())
    .setAgentName(options.getAgentName())
    .setAgentVersion(options.getAgentVersion())
    .setModel(new ModelRef().setProvider("openai").setName("gpt-5")));

try {
    Response response = openAI.responses().create(request);
    recorder.setResult(OpenAiResponses.fromRequestResponse(request, response, options));
} catch (Exception ex) {
    recorder.setCallError(ex);
    throw ex;
} finally {
    recorder.end();
}
```

## Raw Artifacts

- Default: `false` (off)
- Opt-in: `new OpenAiOptions().setRawArtifacts(true)`

When enabled, provider request/response/tools/events artifacts are attached with OpenAI-specific keys.
