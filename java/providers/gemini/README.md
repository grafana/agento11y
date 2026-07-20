# Grafana Sigil Java Provider Helpers: Gemini

This module provides strict wrappers around official Google GenAI Java SDK types for `models.generateContent`.

No simplified public DTO layer is exposed.

## Public API

- Wrappers:
  - `GeminiAdapter.completion(...)`
  - `GeminiAdapter.completionStream(...)`
  - `GeminiAdapter.embedContent(...)`
- Manual mappers:
  - `GeminiAdapter.fromRequestResponse(...)`
  - `GeminiAdapter.fromStream(...)`
  - `GeminiAdapter.embeddingFromResponse(...)`

## Official SDK Types

These wrappers use official types from `com.google.genai:google-genai`:

- `Content`
- `GenerateContentConfig`
- `GenerateContentResponse`

## Wrapper Example (sync)

```java
List<Content> contents = List.of(
    Content.builder().role("user").parts(Part.fromText("Summarize this run.")).build()
);

GenerateContentResponse response = GeminiAdapter.completion(
    agento11yClient,
    "gemini-2.5-pro",
    contents,
    GenerateContentConfig.builder().maxOutputTokens(512).build(),
    (model, input, cfg) -> genai.models.generateContent(model, input, cfg),
    new GeminiOptions()
        .setConversationId("conv-1")
        .setAgentName("assistant-gemini")
        .setAgentVersion("1.0.0")
);
```

## Wrapper Example (stream)

```java
GeminiStreamSummary summary = GeminiAdapter.completionStream(
    agento11yClient,
    "gemini-2.5-pro",
    contents,
    GenerateContentConfig.builder().maxOutputTokens(512).build(),
    (model, input, cfg) -> genai.models.generateContentStream(model, input, cfg),
    new GeminiOptions()
);
```

## Embedding Example

```java
EmbedContentResponse embeddingResponse = GeminiAdapter.embedContent(
    agento11yClient,
    "gemini-embedding-001",
    java.util.List.of("hello", "world"),
    null,
    (model, input, cfg) -> genai.models.embedContent(model, input, cfg),
    new GeminiOptions()
        .setConversationId("conv-1")
        .setAgentName("assistant-gemini")
        .setAgentVersion("1.0.0")
);
```

## Raw Artifact Policy

- Default: OFF
- Opt-in: `new GeminiOptions().setRawArtifacts(true)`

## Provider metadata mapping

Gemini-specific fields are mapped as follows:

- `usage.thoughtsTokenCount` -> normalized `usage.reasoning_tokens`
- `usage.toolUsePromptTokenCount` -> metadata `agento11y.gen_ai.usage.tool_use_prompt_tokens`
- `config.thinkingConfig.thinkingBudget` -> metadata `agento11y.gen_ai.request.thinking.budget_tokens`
- `config.thinkingConfig.thinkingLevel` -> metadata `agento11y.gen_ai.request.thinking.level`
