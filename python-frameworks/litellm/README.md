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

## Guards

`Agento11yLiteLLMGuardrail` evaluates Agent Observability preflight guards before a request reaches the provider, and blocks it with HTTP 400 when a guard denies. It is a `CustomGuardrail`, so it gets LiteLLM's per-request and per-team opt-in, `default_on`, guardrail results in the standard logging payload, and an OTel guardrail span.

Enforcement only works behind the LiteLLM proxy. Pre-call hooks are a proxy-only LiteLLM feature; a direct `litellm.completion()` SDK call never invokes them and is never guarded.

Only preflight is supported. The guardrail defaults to `event_hook="pre_call"`, and constructing it with `event_hook="post_call"` or `event_hook="during_call"` raises at proxy startup rather than registering a guardrail that silently never runs.

Guards are configured in Agent Observability, not in `config.yaml`. Refer to [Set up guards](https://grafana.com/docs/grafana-cloud/observe-and-act/agent-observability/guides/guards/) for guard types, priority, and match filters.

Evaluator guards work as documented there, against the request: messages, system prompt, and tool definitions. `deny` returns 400 to the proxy client and the provider is never called; `warn` allows the request and records the verdict. Three things behave differently through this adapter:

- Redact guards do not change what the model sees. That page says the SDK uses `transformed_input` for the LLM call; this adapter ignores it. Redaction still applies to evaluators in later guards, which run server-side against the redacted input.
- Tool filter guards match tool calls that are already in the request history, which means the client has executed them. They block the next call in an agent loop rather than the tool itself. Blocking a proposed call before it runs is postflight, which is not implemented here.
- `model.provider` match filters are unreliable, because the provider is not known until LiteLLM's router picks a deployment, which happens after the guard runs. Match on `agent_name`, `model.name`, or tags instead.

### Guarded routes

The guard reads the request body as the client sent it, before LiteLLM translates it to provider format, and every route puts the input somewhere else:

| Route | Messages | System prompt |
|---|---|---|
| `/v1/chat/completions` | `messages` | `system` and `developer` messages |
| `/v1/completions` | `prompt` (string or list of strings; token ids carry no text) | none |
| `/v1/messages` | `messages`, including Anthropic content blocks | top-level `system` |
| `/v1/responses` | `input` (string or input items) | `instructions` |
| `/v1/images/generations` | `prompt` | none |

Every other route LiteLLM runs pre-call hooks on is skipped: embeddings, moderation, rerank, audio, realtime, MCP tool calls, and native pass-through endpoints (`/anthropic/v1/messages`, `/vertex_ai/...`). Their bodies are provider-native or carry no messages, so there is nothing for a content or system-prompt rule to match. They are skipped rather than evaluated against empty input, because an evaluation with no input returns allow and records a verdict that reads like a completed check. A skipped request records no verdict at all, and nothing on those routes is ever blocked, including by rules that only match on `agent_name`, `model.name`, or tags.

A guarded request whose input maps to no text is skipped the same way: a token-id `prompt`, or content that is entirely images or audio.

Enable hooks on the agento11y client and list the guardrail next to the logger:

```python
# agento11y_callback.py, next to config.yaml
from agento11y import Client, ClientConfig, HooksConfig
from agento11y_litellm import Agento11yLiteLLMGuardrail, Agento11yLiteLLMLogger

client = Client(ClientConfig(hooks=HooksConfig(enabled=True, timeout_seconds=2.0)))

agento11y_handler = Agento11yLiteLLMLogger(client=client, agent_name="litellm-proxy")
agento11y_guards = Agento11yLiteLLMGuardrail(client=client, agent_name="litellm-proxy", default_on=True)
```

```yaml
# config.yaml
litellm_settings:
  callbacks:
    - agento11y_callback.agento11y_handler
    - agento11y_callback.agento11y_guards
```

LiteLLM runs any `CustomGuardrail` in `litellm.callbacks` on the pre-call path, so the guardrail needs no separate `guardrails:` block.

With `default_on=False`, a request opts in with `"guardrails": ["agento11y"]` in its metadata.

Lower `HooksConfig.timeout_seconds` from its 15 second default for proxy use. It bounds how long a worker thread stays occupied after the guardrail has already given up on the evaluation.

Guardrail options are keyword-only, and `create_agento11y_litellm_guardrail` accepts the same ones:

| Parameter | Type | Default | Description |
|---|---|---|---|
| `client` | `agento11y.Client` | required | agento11y SDK client instance, with `hooks.enabled=True` and `"preflight"` in `hooks.phases` |
| `agent_name` | `str` | `""` | Fallback agent name when the request carries no agent identity |
| `agent_name_metadata_keys` | `Sequence[str]` | `("agent_name", "agent_id")` | Metadata keys consulted, in order, to name the agent |
| `agent_version` | `str` | `""` | Fallback agent version |
| `max_concurrent_evaluations` | `int` | `32` | Ceiling on hook evaluations in flight at once |
| `request_timeout_seconds` | `float` | `2.0` | How long the proxy waits for a free thread plus a verdict |
| `extra_tags` | `dict[str, str]` | `None` | Additional tags merged into every hook evaluation context |
| `guardrail_name` | `str` | `"agento11y"` | Name used for per-request opt-in and in the 400 response |
| `default_on` | `bool` | `False` | Run on every request instead of only on opted-in ones |

Runtime behavior:

- Evaluation runs on a pool of `max_concurrent_evaluations` threads, so it does not block the proxy event loop. `request_timeout_seconds` covers waiting for a free thread as well as the evaluation itself, and a thread stays busy until its evaluation actually finishes, so a slow evaluator can keep the pool saturated for longer than that timeout.
- A transport failure, a timeout, or an unexpected error follows `HooksConfig.fail_open`: allow and log at WARNING when `True` (the default), raise `HookTransportError` when `False`. Either way the verdict is recorded as `guardrail_failed_to_respond`, not `success`, so a dead evaluator shows up in spend logs and logging callbacks.
- Hook evaluations correlate to the proxy request span, so a guard verdict lines up with its request in traces.
- Register both the guardrail and the logger. The guardrail does not export generations, and having both in `litellm.callbacks` still exports exactly one generation per request.

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
- Provider detection: uses `custom_llm_provider` from LiteLLM's standard logging object. A call that fails before the router picks a deployment, for example a budget or auth rejection, has no provider. It is reported under the provider `litellm` instead of being dropped.
- Model name: a proxied call is reported with the bare model name (`gpt-4o-mini`), not the router deployment string (`openai/gpt-4o-mini`), so cost and catalog lookups match it. The router names are kept in generation metadata under `agento11y.framework.litellm.model`, `.model_group`, and `.model_id`. Embedding spans carry the model name only. An Azure deployment alias (`azure/my-deployment`) is reported as it is, because there is no catalog model with that name.
- Failed calls are recorded with the error attached via `set_call_error`.
- Recorded call types:
  - chat completions (`completion`, `acompletion`)
  - text completions (`text_completion`, `atext_completion`). The prompt is recorded as a user message. Pre-tokenized prompts (lists of token ids) carry no text and are skipped.
  - the Responses API (`responses`, `aresponses`, `/v1/responses`). Text, reasoning, and tool calls in the output are recorded. That route has no `finish_reason`, so the stop reason comes from the response `status`: `completed` becomes `stop`, and an incomplete response reports `incomplete_details.reason`.
  - the Anthropic Messages API (`anthropic_messages`, `aanthropic_messages`, `/v1/messages`)
- Only text content is recorded. Images, audio, and other non-text parts are skipped. Tool history is recorded in each request shape that carries it: OpenAI-format `tool_calls` and `tool` messages, Anthropic `tool_use` and `tool_result` blocks on `/v1/messages`, and top-level `function_call` and `function_call_output` items in `/v1/responses` input. The remaining Responses input items (`computer_call`, `mcp_call`, `image_generation_call`) are skipped. Tool calls in `/v1/responses` output are recorded.
- Tool definitions are read from the request `tools` in the OpenAI chat schema (`{"type": "function", "function": {...}}`). The flat Responses API tool schema is not recognized, so those requests report no tool definitions.
- Embedding call types (`embedding`, `aembedding`) are recorded as OTel embedding spans (no generation export). The span carries input/token counts and dimensions; the input text is attached only when the handler's `capture_inputs` is set and the SDK's `EmbeddingCaptureConfig.capture_input=True`. Embedding spans require a configured OTel tracer.
- Image generation, audio, and transcription call types are skipped.
- Framework tags are always set:
  - `agento11y.framework.name=litellm`
  - `agento11y.framework.source=handler` (`guardrail` on hook evaluation contexts)
  - `agento11y.framework.language=python`
- LiteLLM `request_tags` are forwarded as `litellm.tag.<value>`.
- Token usage includes detailed breakdowns (cached tokens, reasoning tokens) when the provider returns them.
- Tool calls and tool results in messages are mapped to agento11y tool call/result parts.
- Reasoning/thinking text is captured as `THINKING` parts, ordered before the assistant text. It is read from `thinking_blocks` when present (including redacted blocks), otherwise from the flat `reasoning_content` string.

Call `client.shutdown()` during teardown to flush buffered telemetry.
