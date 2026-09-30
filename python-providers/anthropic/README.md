# Agent Observability Python Provider Helper: Anthropic

`agento11y-anthropic` provides strict Anthropic Messages wrappers and mappers for agento11y.

## Embeddings support

This helper currently supports Anthropic Messages APIs only. Native Anthropic embeddings endpoints are not available in the official SDK/API surface used in this repository.

## Installation

```bash
pip install agento11y agento11y-anthropic anthropic
```

This package supports Anthropic Python SDK 0.79 through 1.x. SDK 1.x rejects `temperature`, `top_p`, and `top_k` as arguments to `messages.create()` and `messages.stream()`. Pass them through `extra_body` when an older model still needs them. The wrapper still records `temperature` and `top_p` when they are present on the request object it is given.

## Wrapper Mode (Sync)

```python
from anthropic.types.message_create_params import MessageCreateParams
from agento11y import Client, ClientConfig
from agento11y_anthropic import AnthropicOptions, messages

client = Client(ClientConfig())

request: MessageCreateParams = {
    "model": "claude-sonnet-4-5",
    "max_tokens": 256,
    "messages": [{"role": "user", "content": "Hello"}],
}

def provider_call(req: MessageCreateParams):
    return anthropic_client.messages.create(**req)

response = messages.create(
    client,
    request,
    provider_call,
    AnthropicOptions(conversation_id="conv-1", agent_name="assistant", agent_version="1.0.0"),
)
```

## Wrapper Mode (Stream)

```python
from agento11y_anthropic import AnthropicStreamSummary, messages

summary = messages.stream(
    client,
    request,
    lambda req: AnthropicStreamSummary(
        output_text="streamed text",
        events=[{"type": "content_block_delta", "delta": {"type": "text_delta", "text": "streamed text"}}],
    ),
)
```

## Mapper Mode

```python
generation = messages.from_request_response(request, response)
stream_generation = messages.from_stream(request, summary)
```

## Raw Provider Artifacts (Opt-In)

```python
options = AnthropicOptions(raw_artifacts=True)
```

Raw artifacts are default OFF and should only be enabled for diagnostics.

## Provider metadata mapping

In addition to normalized usage fields, Anthropic server-tool counters are mapped into agento11y metadata when present:

- `agento11y.gen_ai.usage.server_tool_use.web_search_requests`
- `agento11y.gen_ai.usage.server_tool_use.web_fetch_requests`
- `agento11y.gen_ai.usage.server_tool_use.total_requests`
