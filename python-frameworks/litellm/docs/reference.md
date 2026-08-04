# LiteLLM adapter reference

`Agento11yLiteLLMLogger` is the LiteLLM callback that exports generations to Agent Observability.
This topic lists its options, the calls it records, and what each exported field holds.

To wire the callback up, refer to [Export generations from your app](../README.md#export-generations-from-your-app).
For guards, refer to the [LiteLLM guard reference](guards.md).

## Logger options

All options are keyword-only.
`create_agento11y_litellm_logger` accepts the same ones.

| Option | Type | Default | Description |
| --- | --- | --- | --- |
| `agent_name` | `str` | `""` | Fallback agent name for a request that carries no agent identity |
| `agent_name_metadata_keys` | `Sequence[str]` | `("agent_name", "agent_id")` | Metadata keys consulted, in order, to name the agent |
| `agent_version` | `str` | `""` | Default agent version |
| `capture_inputs` | `bool` | `True` | Record input messages and the system prompt |
| `capture_outputs` | `bool` | `True` | Record output messages |
| `client` | `agento11y.Client` | Required | SDK client instance |
| `conversation_id` | `str` | `""` | Default conversation ID |
| `extra_metadata` | `dict[str, Any]` | `None` | Additional metadata merged into every generation |
| `extra_tags` | `dict[str, str]` | `None` | Additional tags merged into every generation |

## Recorded calls

| Call type | Route | Notes |
| --- | --- | --- |
| `anthropic_messages`, `aanthropic_messages` | `/v1/messages` | Anthropic content blocks, `tool_use`, and `tool_result` are mapped |
| `completion`, `acompletion` | `/v1/chat/completions` | |
| `embedding`, `aembedding` | `/v1/embeddings` | Exported as an OpenTelemetry (OTel) span, not a generation |
| `responses`, `aresponses` | `/v1/responses` | `instructions` becomes the system prompt |
| `text_completion`, `atext_completion` | `/v1/completions` | The prompt is recorded as a user message |

Image generation, audio, and transcription calls are skipped.
A pre-tokenized prompt, which is a list of token IDs, carries no text, so it's skipped too.

## Generation fields

| Field | Source |
| --- | --- |
| `agent_name`, `agent_version`, `conversation_id` | Per-request metadata, then the static option. Refer to [Agent identity](#agent-identity). |
| `max_tokens`, `temperature`, `top_p` | The request's model parameters |
| `mode` | `SYNC` for a non-streamed call, `STREAM` for a streamed one, with the first-token timestamp |
| `model.name` | The bare catalog name, such as `gpt-4o-mini`, so cost and catalog lookups match it |
| `model.provider` | `custom_llm_provider` from LiteLLM's logging payload |
| `stop_reason` | `finish_reason` on a chat payload, or the `status` on a Responses payload |
| `system_prompt` | A `system` or `developer` message, or `instructions` on `/v1/responses` |
| `usage` | Token counts, including cached and reasoning tokens when the provider returns them |

An Azure deployment alias such as `azure/my-deployment` is reported as it is, because no catalog model carries that name.
The router names are kept in metadata under `agento11y.framework.litellm.model`, `.model_group`, and `.model_id`.

### Output mapping

Output is mapped by the shape of the logged response, not by the call type:

- A payload carrying `choices` is read as a chat completion.
- A payload carrying an `output` list is read as a Responses payload.
- A payload with neither falls back to the call type.

The shape matters because LiteLLM serves `/v1/responses` on a provider without a native Responses API by bridging the call to chat completions.
That bridge logs a chat payload under call type `aresponses`.

Only text content is recorded.
Images, audio, and other non-text parts are skipped.

Reasoning is captured as `THINKING` parts, ordered before the assistant text.
It's read from `thinking_blocks` when present, including redacted blocks, and otherwise from the flat `reasoning_content` string.

### Tool calls and definitions

Tool history is recorded in each request shape that carries it:

- Anthropic `tool_use` and `tool_result` blocks on `/v1/messages`
- OpenAI `tool_calls` and `tool` messages on chat routes
- Top-level `function_call` and `function_call_output` items in `/v1/responses` input

The remaining Responses input items, `computer_call`, `mcp_call`, and `image_generation_call`, are skipped.
Tool calls in `/v1/responses` output are recorded.

Tool definitions are read from the request `tools`.
The adapter reads the Anthropic schema, the OpenAI chat schema, and the flat Responses schema.

### Tags and metadata

These tags are always set:

- `agento11y.framework.language=python`
- `agento11y.framework.name=litellm`
- `agento11y.framework.source=handler`, or `guardrail` on a hook evaluation context

LiteLLM `request_tags` are forwarded as `litellm.tag.<value>`.
LiteLLM adds a tag for the caller's user agent by default, so expect one tag per user agent string.

### Failures

A failed call is recorded with the error attached.

A call that fails before the router picks a deployment, such as a budget or authentication rejection, has no provider.
It's reported under the provider `litellm` rather than dropped.

A request LiteLLM rejects before routing reaches the failure callback twice for one call ID.
Both exports carry the same generation ID, so the server keeps the first and refuses the second.
That logs one `generation rejected ... generation already exists` line per rejected request, and nothing is lost or duplicated.

## Agent identity

The adapter resolves `agent_name`, `agent_version`, and `conversation_id` per request, and falls back to the static options.
Metadata is read from these containers, in this order:

1. `litellm_params["metadata"]`
1. `metadata.metadata`, where the Router puts SDK-supplied metadata
1. `metadata.requester_metadata`, the copy the proxy keeps of what the caller sent
1. `litellm_params["litellm_metadata"]`, used by assistant and thread routes
1. `litellm_metadata.metadata`
1. `litellm_metadata.requester_metadata`

Keys are matched in priority order across all containers, so an `agent_name` in any of them beats an `agent_id` in another.

`requester_metadata` is the only copy on `/v1/messages` and `/v1/responses`.
Both routes hand the callback a `litellm_metadata` holding proxy state and no client-supplied key.

`agent_name_metadata_keys` controls which keys the adapter consults.
The default, `("agent_name", "agent_id")`, ends with LiteLLM's own `agent_id`.
The proxy fills that in from the `x-litellm-agent-id` header, or from an `agent_id` on the calling virtual key.

A key alias names a credential rather than an agent, so `user_api_key_alias` isn't consulted by default.
Rotating a key would rename the agent, and a shared key would merge unrelated callers.

### Conversation ID

`conversation_id` resolves from the first of these that's set:

1. The `conversation_id`, `session_id`, or `thread_id` metadata key
1. LiteLLM's `litellm_session_id` or `litellm_trace_id` in `litellm_params`
1. The `trace_id` on the logged payload
1. The `conversation_id` option

The payload's `trace_id` is what groups `/v1/messages` turns.
That route leaves `litellm_params` without a trace ID, so a generation with no other source would export ungrouped and stay out of conversation search.

## Embeddings

An embedding call is exported as an OTel span, not a generation, and the span needs a configured tracer.

The span carries the input count, token count, and dimensions.
LiteLLM clears the input before it invokes callbacks when message logging is off, so the span honors `turn_off_message_logging`.

The input text is attached only when the handler's `capture_inputs` is set and the SDK's `EmbeddingCaptureConfig.capture_input` is `True`.
For that setting, refer to [Embedding capture](https://grafana.com/docs/grafana-cloud/observe-and-act/agent-observability/configure/sdk/#embedding-capture).

## Deploy in the LiteLLM proxy

The proxy loads a callback by dotted path from a file next to `config.yaml`.
For a complete, runnable version of the following, refer to the [LiteLLM proxy example](../example/README.md).

Extend the image, bootstrapping `pip` because recent LiteLLM images ship a `uv` virtual environment without it:

```dockerfile
FROM ghcr.io/berriai/litellm:v1.95.0
RUN python -m ensurepip --upgrade && \
    python -m pip install --no-cache-dir agento11y agento11y-litellm
```

Create `agento11y_callback.py` next to `config.yaml`:

```python
import os

from agento11y import Client
from agento11y.config import ApiConfig, AuthConfig, ClientConfig, GenerationExportConfig
from agento11y_litellm import Agento11yLiteLLMLogger

endpoint = os.environ["AGENTO11Y_ENDPOINT"]

client = Client(
    ClientConfig(
        generation_export=GenerationExportConfig(
            protocol="http",
            endpoint=endpoint,
            auth=AuthConfig(
                mode="basic",
                tenant_id=os.environ["AGENTO11Y_AUTH_TENANT_ID"],
                basic_password=os.environ["AGENTO11Y_AUTH_TOKEN"],
            ),
        ),
        api=ApiConfig(endpoint=endpoint),
    )
)

agento11y_handler = Agento11yLiteLLMLogger(client=client, agent_name="litellm-proxy")
```

Reference it from `config.yaml`:

```yaml
litellm_settings:
  callbacks:
    - agento11y_callback.agento11y_handler
```

Mount both files and run the proxy:

```bash
docker run -d \
  -v $(pwd)/config.yaml:/app/config.yaml \
  -v $(pwd)/agento11y_callback.py:/app/agento11y_callback.py \
  -e AGENTO11Y_ENDPOINT=https://your-agento11y-endpoint \
  -e AGENTO11Y_AUTH_TENANT_ID=your-tenant \
  -e AGENTO11Y_AUTH_TOKEN=your-token \
  -p 4000:4000 \
  your-litellm-image \
  --config /app/config.yaml
```

The callback file reads its connection details from environment variables.
Set `AuthConfig` to the mode your deployment uses; `agento11y.config` documents the `basic`, `bearer`, and `tenant` modes.
