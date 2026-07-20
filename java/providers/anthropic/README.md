# Grafana Sigil Java Provider Helpers: Anthropic

This module provides strict wrappers around official Anthropic Java SDK request/response types for Messages.

No simplified public DTO layer is exposed.

## Embeddings support

This helper currently supports Anthropic Messages APIs only. Native Anthropic embeddings endpoints are not available in the official SDK/API surface used in this repository.

## Public API

- Wrappers:
  - `AnthropicAdapter.completion(...)`
  - `AnthropicAdapter.completionStream(...)`
- Manual mappers:
  - `AnthropicAdapter.fromRequestResponse(...)`
  - `AnthropicAdapter.fromStream(...)`

## Official SDK Types

These wrappers accept and return official types from `com.anthropic:anthropic-java`:

- `MessageCreateParams`
- `Message`
- `RawMessageStreamEvent`

## Wrapper Example (sync)

```java
MessageCreateParams request = MessageCreateParams.builder()
    .model("claude-sonnet-4")
    .maxTokens(512)
    .addUserMessage("Summarize this run.")
    .build();

Message response = AnthropicAdapter.completion(
    agento11yClient,
    request,
    params -> anthropic.messages().create(params),
    new AnthropicOptions()
        .setConversationId("conv-1")
        .setAgentName("assistant-anthropic")
        .setAgentVersion("1.0.0")
);
```

## Wrapper Example (stream)

```java
AnthropicStreamSummary summary = AnthropicAdapter.completionStream(
    agento11yClient,
    request,
    params -> anthropic.messages().createStreaming(params),
    new AnthropicOptions()
);
```

## Raw Artifact Policy

- Default: OFF
- Opt-in: `new AnthropicOptions().setRawArtifacts(true)`

## Provider metadata mapping

In addition to normalized usage fields, Anthropic server-tool counters are mapped into Sigil metadata when present:

- `agento11y.gen_ai.usage.server_tool_use.web_search_requests`
- `agento11y.gen_ai.usage.server_tool_use.web_fetch_requests`
- `agento11y.gen_ai.usage.server_tool_use.total_requests`
