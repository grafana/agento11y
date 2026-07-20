# Sigil Python Framework Module: LiteLLM

`agento11y-litellm` is a LiteLLM callback handler that exports generation telemetry to Sigil.

## Installation

```bash
pip install agento11y agento11y-litellm
pip install litellm
```

## Quickstart

```python
import litellm
from agento11y import Client
from agento11y_litellm import Agento11yLiteLLMLogger

client = Client()
handler = Agento11yLiteLLMLogger(client=client)

litellm.callbacks = [handler]

response = litellm.completion(
    model="openai/gpt-4o-mini",
    messages=[{"role": "user", "content": "Hello!"}],
)
print(response.choices[0].message.content)

client.shutdown()
```

## Streaming

```python
import litellm
from agento11y import Client
from agento11y_litellm import Agento11yLiteLLMLogger

client = Client()
litellm.callbacks = [Agento11yLiteLLMLogger(client=client)]

response = litellm.completion(
    model="openai/gpt-4o-mini",
    messages=[{"role": "user", "content": "Give me three reliability tips."}],
    stream=True,
)
for chunk in response:
    content = chunk.choices[0].delta.content
    if content:
        print(content, end="", flush=True)
print()

client.shutdown()
```

## Configuration

All options are keyword-only on `Agento11yLiteLLMLogger`:

| Parameter | Type | Default | Description |
|---|---|---|---|
| `client` | `agento11y.Client` | required | Sigil SDK client instance |
| `capture_inputs` | `bool` | `True` | Record input messages |
| `capture_outputs` | `bool` | `True` | Record output messages |
| `agent_name` | `str` | `""` | Default agent name (see below for per-request) |
| `agent_version` | `str` | `""` | Default agent version (see below for per-request) |
| `conversation_id` | `str` | `""` | Default conversation ID (see below for per-request) |
| `extra_tags` | `dict[str, str]` | `None` | Additional tags merged into every generation |
| `extra_metadata` | `dict[str, Any]` | `None` | Additional metadata merged into every generation |

The `create_agento11y_litellm_logger` factory accepts the same parameters.

## Per-Request Metadata

The handler resolves `agent_name`, `agent_version`, and `conversation_id` from per-request LiteLLM metadata, falling back to the static values from handler init. This is useful when multiple agents share a single LiteLLM proxy.

```python
response = litellm.completion(
    model="openai/gpt-4o-mini",
    messages=[{"role": "user", "content": "Continue our chat."}],
    metadata={
        "agent_name": "search-agent",
        "agent_version": "v2",
        "conversation_id": "conv-abc-123",
    },
)
```

For `conversation_id`, the handler also checks `session_id` and `thread_id` metadata keys as fallbacks.

## LiteLLM Proxy (Docker)

When running LiteLLM as a proxy server in Docker, register the handler via a callback file next to your config.

**1. Extend the Docker image:**

```dockerfile
FROM ghcr.io/berriai/litellm:v1.82.3-stable.patch.2
RUN pip install agento11y agento11y-litellm
```

**2. Create a callback file** (`agento11y_callback.py`, same directory as `config.yaml`):

```python
import os

from agento11y import Client
from agento11y.config import AuthConfig, ClientConfig, GenerationExportConfig
from agento11y_litellm import Agento11yLiteLLMLogger

client = Client(ClientConfig(
    generation_export=GenerationExportConfig(
        protocol="http",
        endpoint=os.environ["AGENTO11Y_ENDPOINT"],
        auth=AuthConfig(
            mode="basic",
            tenant_id=os.environ.get("AGENTO11Y_AUTH_TENANT_ID", ""),
            basic_password=os.environ.get("AGENTO11Y_AUTH_TOKEN", ""),
        ),
    ),
))
agento11y_handler = Agento11yLiteLLMLogger(
    client=client,
    agent_name="litellm-proxy",
)
```

**3. Reference it in `config.yaml`:**

```yaml
model_list:
  - model_name: gpt-4o-mini
    litellm_params:
      model: openai/gpt-4o-mini

litellm_settings:
  callbacks: agento11y_callback.agento11y_handler
```

The proxy resolves `agento11y_callback.agento11y_handler` by importing `agento11y_callback.py` from the config directory and using the `agento11y_handler` instance.

**4. Mount both files and run:**

```bash
docker run -d \
  -v $(pwd)/config.yaml:/app/config.yaml \
  -v $(pwd)/agento11y_callback.py:/app/agento11y_callback.py \
  -e AGENTO11Y_ENDPOINT=https://your-agento11y-endpoint \
  -e AGENTO11Y_AUTH_TENANT_ID=your-tenant \
  -e AGENTO11Y_AUTH_TOKEN=your-key \
  -p 4000:4000 \
  your-litellm-image \
  --config /app/config.yaml
```

The callback file reads connection details from environment variables. Adjust the `AuthConfig` mode to match your deployment (see `agento11y.config` for `tenant`, `bearer`, and `basic` modes).

## Behavior

- Mode mapping: non-stream calls -> `SYNC`, stream calls -> `STREAM` with first-token timestamp.
- Provider detection: uses `custom_llm_provider` from LiteLLM's standard logging object.
- Failed calls are recorded with the error attached via `set_call_error`.
- Chat completion call types (`completion`, `acompletion`, `text_completion`, `atext_completion`) are recorded as generations.
- Embedding call types (`embedding`, `aembedding`) are recorded as OTel embedding spans (no generation export). The span carries input/token counts and dimensions; the input text is attached only when the handler's `capture_inputs` is set and the SDK's `EmbeddingCaptureConfig.capture_input=True`. Embedding spans require a configured OTel tracer.
- Image, audio, and transcription call types are skipped.
- Framework tags are always set:
  - `agento11y.framework.name=litellm`
  - `agento11y.framework.source=handler`
  - `agento11y.framework.language=python`
- LiteLLM `request_tags` are forwarded as `litellm.tag.<value>`.
- Token usage includes detailed breakdowns (cached tokens, reasoning tokens) when the provider returns them.
- Tool calls and tool results in messages are mapped to Sigil's tool call/result parts.
- Reasoning/thinking text is captured as `THINKING` parts, ordered before the assistant text. It is read from `thinking_blocks` when present (including redacted blocks), otherwise from the flat `reasoning_content` string.

Call `client.shutdown()` during teardown to flush buffered telemetry.
