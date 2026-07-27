# Agent Observability Python Framework Module: LiteLLM

`agento11y-litellm` is a LiteLLM callback handler that exports generation telemetry to Agent Observability.

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
| `client` | `agento11y.Client` | required | agento11y SDK client instance |
| `capture_inputs` | `bool` | `True` | Record input messages |
| `capture_outputs` | `bool` | `True` | Record output messages |
| `agent_name` | `str` | `""` | Fallback agent name, used when the request carries no agent identity (see below for per-request) |
| `agent_name_metadata_keys` | `Sequence[str]` | `("agent_name", "agent_id")` | Metadata keys consulted, in order, to name the agent |
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

When no `agent_name` is set, the handler falls back to LiteLLM's own `agent_id`, which the proxy fills in from the `x-litellm-agent-id` header or from an `agent_id` on the calling virtual key. Callers behind a proxy are then attributed to themselves without having to know about agento11y:

```bash
curl http://localhost:4000/v1/chat/completions \
  -H 'x-litellm-agent-id: search-agent' \
  -H 'Content-Type: application/json' \
  -d '{"model": "gpt-4o-mini", "messages": [{"role": "user", "content": "Hello!"}]}'
```

Metadata is read from `litellm_params["metadata"]`, from `litellm_metadata` (used by assistant and thread routes), and from metadata nested one level deeper under `metadata.metadata` (where the Router puts SDK-supplied metadata). Keys are matched in priority order across all of those, so an `agent_name` in any of them beats an `agent_id` in another.

`agent_name_metadata_keys` controls which keys are consulted. Add your own, or LiteLLM's key alias for deployments where one virtual key means one agent:

```python
from agento11y_litellm import DEFAULT_AGENT_NAME_METADATA_KEYS, Agento11yLiteLLMLogger

handler = Agento11yLiteLLMLogger(
    client=client,
    agent_name_metadata_keys=(*DEFAULT_AGENT_NAME_METADATA_KEYS, "user_api_key_alias"),
    agent_name="litellm-proxy",
)
```

A key alias names a credential rather than an agent, so it is not consulted by default: rotating a key would rename the agent, and a shared key would merge unrelated callers. Conversely, pass `("agent_name",)` to ignore LiteLLM's `agent_id` and keep every generation under the static name.

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
- Tool calls and tool results in messages are mapped to agento11y tool call/result parts.
- Reasoning/thinking text is captured as `THINKING` parts, ordered before the assistant text. It is read from `thinking_blocks` when present (including redacted blocks), otherwise from the flat `reasoning_content` string.

Call `client.shutdown()` during teardown to flush buffered telemetry.
