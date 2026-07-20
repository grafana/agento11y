# Sigil Python Provider Helper: Anthropic

`agento11y-anthropic` provides strict Anthropic Messages wrappers and mappers for Sigil.

## Embeddings support

This helper currently supports Anthropic Messages APIs only. Native Anthropic embeddings endpoints are not available in the official SDK/API surface used in this repository.

## Installation

```bash
pip install agento11y agento11y-anthropic anthropic
```

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

In addition to normalized usage fields, Anthropic server-tool counters are mapped into Sigil metadata when present:

- `sigil.gen_ai.usage.server_tool_use.web_search_requests`
- `sigil.gen_ai.usage.server_tool_use.web_fetch_requests`
- `sigil.gen_ai.usage.server_tool_use.total_requests`
