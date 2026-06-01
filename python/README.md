# Grafana Sigil Python SDK

`sigil-sdk` records normalized LLM generation and tool-execution telemetry. It exports normalized generations to Sigil ingest and uses your OpenTelemetry tracer/meter setup for traces and metrics.

Use this package when you want:

- A provider-agnostic generation record (same schema for OpenAI, Anthropic, Gemini, or custom adapters).
- OTel-aligned tracing attributes for generation and tool spans.
- Async export with retry/backoff, queueing, batching, and explicit shutdown semantics.

## Installation

```bash
pip install sigil-sdk
```

For a Grafana Cloud setup walkthrough (where to find the endpoint URL, instance ID, and API token), refer to the [Grafana Cloud setup guide](https://grafana.com/docs/grafana-cloud/machine-learning/ai-observability/get-started/grafana-cloud/).

## Validation

Run the shared core conformance suite for the Python SDK from the repo root:

```bash
mise run test:py:sdk-conformance
```

Run the cross-language aggregate core conformance suite from the repo root:

```bash
mise run sdk:conformance
```

Optional provider helper packages:

```bash
pip install sigil-sdk-openai
pip install sigil-sdk-anthropic
pip install sigil-sdk-gemini
```

Optional framework modules:

```bash
pip install sigil-sdk-langchain
pip install sigil-sdk-langgraph
pip install sigil-sdk-openai-agents
pip install sigil-sdk-llamaindex
pip install sigil-sdk-google-adk
pip install sigil-sdk-strands
pip install sigil-sdk-litellm
pip install sigil-sdk-pydantic-ai
```

Framework handler usage:

```python
from sigil_sdk import Client
from sigil_sdk_langchain import with_sigil_langchain_callbacks
from sigil_sdk_langgraph import with_sigil_langgraph_callbacks
from sigil_sdk_openai_agents import with_sigil_openai_agents_hooks
from sigil_sdk_llamaindex import with_sigil_llamaindex_callbacks
from sigil_sdk_google_adk import with_sigil_google_adk_callbacks
from sigil_sdk_strands import with_sigil_strands_hooks
from sigil_sdk_pydantic_ai import with_sigil_pydantic_ai_capability

client = Client()
chain_config = with_sigil_langchain_callbacks(None, client=client, provider_resolver="auto")
graph_config = with_sigil_langgraph_callbacks(None, client=client, provider_resolver="auto")
openai_agents_run_options = with_sigil_openai_agents_hooks(None, client=client, provider_resolver="auto")
llamaindex_config = with_sigil_llamaindex_callbacks(None, client=client, provider_resolver="auto")
google_adk_agent_config = with_sigil_google_adk_callbacks(None, client=client, provider_resolver="auto")
strands_agent_config = with_sigil_strands_hooks(None, client=client, provider_resolver="auto")
pydantic_ai_capabilities = with_sigil_pydantic_ai_capability(None, client=client, provider_resolver="auto")
```

LiteLLM uses a callback class instead of a `with_sigil_*` helper:

```python
import litellm
from sigil_sdk import Client
from sigil_sdk_litellm import SigilLiteLLMLogger

client = Client()
litellm.callbacks = [SigilLiteLLMLogger(client=client)]
```

Framework handlers use the `Client` instance you pass in. If that client is configured with
`generation_sanitizer`, the same redaction policy applies automatically to generations recorded
through LangChain, LangGraph, OpenAI Agents, LlamaIndex, Google ADK, Strands, LiteLLM, and Pydantic AI integrations.

Framework handlers inject framework tags/metadata on recorded generations:

- `sigil.framework.name` (`langchain`, `langgraph`, `openai-agents`, `llamaindex`, `google-adk`, `strands`, `litellm`, or `pydantic-ai`)
- `sigil.framework.source=handler` (or `hooks` for Strands Agents)
- `sigil.framework.language=python`
- `metadata["sigil.framework.run_id"]`
- `metadata["sigil.framework.thread_id"]` (when present)
- `metadata["sigil.framework.parent_run_id"]` (when available)
- `metadata["sigil.framework.component_name"]`
- `metadata["sigil.framework.run_type"]`
- `metadata["sigil.framework.tags"]`
- `metadata["sigil.framework.retry_attempt"]` (when available)
- `metadata["sigil.framework.event_id"]` (when available)
- `metadata["sigil.framework.langgraph.node"]` (LangGraph when available)

Conversation mapping is conversation-first:

- `conversation_id` / `session_id` / `group_id` from framework context first
- then `thread_id`
- deterministic fallback `sigil:framework:<framework_name>:<run_id>`

When present in generation metadata, low-cardinality framework keys are copied onto generation span attributes.

For LangGraph persistence, pass `configurable.thread_id` and reuse it across invocations:

```python
thread_config = {
    **with_sigil_langgraph_callbacks(None, client=client, provider_resolver="auto"),
    "configurable": {"thread_id": "customer-42"},
}
graph.invoke({"prompt": "Remember my timezone is UTC+1.", "answer": ""}, config=thread_config)
graph.invoke({"prompt": "What timezone did I give you?", "answer": ""}, config=thread_config)
```

Full framework examples:

- LangChain: `../python-frameworks/langchain/README.md`
- LangGraph: `../python-frameworks/langgraph/README.md`
- OpenAI Agents: `../python-frameworks/openai-agents/README.md`
- LlamaIndex: `../python-frameworks/llamaindex/README.md`
- Google ADK: `../python-frameworks/google-adk/README.md`
- Strands Agents: `../python-frameworks/strands/README.md`
- LiteLLM: `../python-frameworks/litellm/README.md`
- Pydantic AI: `../python-frameworks/pydantic-ai/README.md`

## Quick Start (Sync Generation)

`Client()` reads `SIGIL_*` env vars by default. See the [Grafana Cloud setup guide](https://grafana.com/docs/grafana-cloud/machine-learning/ai-observability/get-started/grafana-cloud/) for the variable names. Pass an explicit `ClientConfig` only when you need to override.

```python
from sigil_sdk import (
    Client,
    GenerationStart,
    ModelRef,
    assistant_text_message,
    user_text_message,
)

client = Client()  # reads SIGIL_* env vars

with client.start_generation(
    GenerationStart(
        conversation_id="conv-1",
        agent_name="my-service",
        agent_version="1.0.0",
        model=ModelRef(provider="openai", name="gpt-5"),
    )
) as rec:
    rec.set_result(
        input=[user_text_message("What is the weather in Paris?")],
        output=[assistant_text_message("It is 18C and sunny.")],
    )

    # Recorder errors are local SDK errors (validation/enqueue/shutdown),
    # not provider call failures.
    if rec.err() is not None:
        raise rec.err()

client.shutdown()
```

Explicit configuration form:

```python
from sigil_sdk import AuthConfig, Client, ClientConfig, GenerationExportConfig

client = Client(
    ClientConfig(
        generation_export=GenerationExportConfig(
            protocol="http",
            endpoint="http://localhost:8080",
            auth=AuthConfig(mode="tenant", tenant_id="dev-tenant"),
        ),
    )
)
```

## Pre-Ingest Redaction

Use `generation_sanitizer` when you want to redact substrings from normalized generations before
validation, span sync, and export.

```python
from sigil_sdk import (
    Client,
    ClientConfig,
    SecretRedactionOptions,
    create_secret_redaction_sanitizer,
)

client = Client(
    ClientConfig(
        generation_sanitizer=create_secret_redaction_sanitizer(
            SecretRedactionOptions(
                redact_input_messages=False,
                redact_email_addresses=True,
            )
        )
    )
)
```

The built-in sanitizer:

- redacts high-confidence secret formats in assistant text and thinking
- redacts secret formats plus env-style secret values in tool call inputs and tool results
- redacts email addresses by default
- leaves user input unchanged unless `redact_input_messages=True` is set

To preserve email addresses, opt out explicitly:

```python
client = Client(
    ClientConfig(
        generation_sanitizer=create_secret_redaction_sanitizer(
            SecretRedactionOptions(redact_email_addresses=False)
        )
    )
)
```

## Hooks and Guards

Use hooks when you want Sigil guard rules to run before an LLM call. The SDK evaluates the hook on your request path; guard rules configured in Grafana Cloud decide whether to allow, deny, or transform the input.

Hooks are disabled by default. Enable them on the client and call `evaluate_hook(...)` before the provider request:

```python
from sigil_sdk import (
    Client,
    ClientConfig,
    HookContext,
    HookEvaluateRequest,
    HookInput,
    HookModel,
    HookPhase,
    HooksConfig,
    Message,
    MessageRole,
    hook_denied_from_response,
    text_part,
)

client = Client(ClientConfig(hooks=HooksConfig(enabled=True)))

messages = [
    Message(role=MessageRole.USER, parts=[text_part("Summarize this customer note...")]),
]
response = client.evaluate_hook(
    HookEvaluateRequest(
        phase=HookPhase.PREFLIGHT.value,
        context=HookContext(
            agent_name="support-agent",
            agent_version="1.0.0",
            model=HookModel(provider="openai", name="gpt-5"),
        ),
        input=HookInput(
            messages=messages,
            system_prompt="You are a helpful support agent.",
            conversation_preview="Summarize this customer note...",
        ),
    )
)

denied = hook_denied_from_response(response)
if denied is not None:
    raise denied

if response.transformed_input is not None:
    messages = response.transformed_input.messages or messages
```

`HooksConfig` defaults to `phases=["preflight"]`, `timeout_seconds=15.0`, and `fail_open=True`. With fail-open enabled, hook transport errors resolve to allow so an unavailable evaluator does not block production traffic. Set `fail_open=False` for strict paths that should fail closed.

If you use transformed input, pass the transformed messages/system prompt to the provider and record those same values in `start_generation(...)`. For a runnable example, see `../examples/getting-started/python-hooks/`.

Configure OTEL exporters (traces/metrics) in your application OTEL SDK setup. You can optionally pass `tracer` and `meter` via `ClientConfig`.

Quick OTEL setup pattern before creating the Sigil client:

```python
from opentelemetry import metrics, trace
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.trace import TracerProvider

trace.set_tracer_provider(TracerProvider())
metrics.set_meter_provider(MeterProvider())
```

## Streaming Generation

Use `start_streaming_generation(...)` when the upstream provider call is streaming.

```python
from sigil_sdk import GenerationStart, ModelRef

with client.start_streaming_generation(
    GenerationStart(
        conversation_id="conv-stream",
        model=ModelRef(provider="anthropic", name="claude-sonnet-4-5"),
    )
) as rec:
    rec.set_result(output=[assistant_text_message("partial stream summary")])
```

## Embedding Observability

Use `start_embedding(...)` for embedding API calls. Embedding recording emits OTel spans and SDK metrics only, and does not enqueue generation exports.

```python
from sigil_sdk import EmbeddingResult, EmbeddingStart, ModelRef

with client.start_embedding(
    EmbeddingStart(
        agent_name="retrieval-worker",
        agent_version="1.0.0",
        model=ModelRef(provider="openai", name="text-embedding-3-small"),
    )
) as rec:
    response = openai.embeddings.create(model="text-embedding-3-small", input=["hello", "world"])
    rec.set_result(
        EmbeddingResult(
            input_count=2,
            input_tokens=response.usage.prompt_tokens,
            input_texts=["hello", "world"],  # captured only when embedding_capture.capture_input=True
            response_model=response.model,
        )
    )
```

Input text capture is opt-in:

```python
from sigil_sdk import ClientConfig, EmbeddingCaptureConfig

cfg = ClientConfig(
    embedding_capture=EmbeddingCaptureConfig(
        capture_input=True,
        max_input_items=20,
        max_text_length=1024,
    )
)
```

`capture_input` may expose PII/document content in spans. Keep it disabled by default and enable only for scoped debugging.

TraceQL examples:

- `traces{gen_ai.operation.name="embeddings"}`
- `traces{gen_ai.operation.name="embeddings" && gen_ai.request.model="text-embedding-3-small"}`
- `traces{gen_ai.operation.name="embeddings" && error.type!=""}`

## Tool Execution Span Recording

Tool spans are recorded independently of generation export.

```python
from sigil_sdk import ToolExecutionStart

with client.start_tool_execution(
    ToolExecutionStart(
        tool_name="weather",
        tool_call_id="call_weather_1",
        tool_type="function",
        include_content=True,
    )
) as rec:
    rec.set_result(arguments={"city": "Paris"}, result={"temp_c": 18})
```

## SDK identity attributes

- Generation and tool spans always include:
  - `sigil.sdk.name=sdk-python`
- Normalized generation metadata always includes the same key.
- If caller metadata provides a conflicting value for this key, the SDK overwrites it.

## Context Defaults

Use context helpers to set defaults once per request/task boundary.

```python
from sigil_sdk import with_agent_name, with_agent_version, with_conversation_id

with with_conversation_id("conv-ctx"), with_agent_name("planner"), with_agent_version("2026.02"):
    with client.start_generation(
        GenerationStart(model=ModelRef(provider="gemini", name="gemini-2.5-pro"))
    ) as rec:
        rec.set_result(output=[assistant_text_message("ok")])
```

## Content Capture Mode

`ContentCaptureMode` controls what content is included in exported generation payloads and OTel span attributes. Use it to prevent sensitive text (prompts, tool I/O, model responses) from leaving the process. See [Content Capture Modes](../docs/concepts/content-capture-modes.md) for the cross-SDK reference, including the per-surface behavior matrix.

| Mode                            | Generation export                            | Generation span             | Tool spans                              | Embedding span                          |
| ------------------------------- | -------------------------------------------- | --------------------------- | --------------------------------------- | --------------------------------------- |
| `FULL`                          | Full content                                 | Content attributes included | Arguments and results included          | Input texts included when capture is on |
| `NO_TOOL_CONTENT` (SDK default) | Full content                                 | Content attributes included | Arguments and results excluded          | Input texts included when capture is on |
| `METADATA_ONLY`                 | Structure only; text and tool I/O stripped   | Content attributes omitted  | Arguments and results excluded          | Input texts omitted                     |
| `FULL_WITH_METADATA_SPANS`      | Full content                                 | Content attributes omitted  | Arguments and results excluded          | Input texts omitted                     |

`DEFAULT` is a placeholder for "inherit from the next layer"; at the client level it resolves to `NO_TOOL_CONTENT`. The SDK default is `NO_TOOL_CONTENT`, which matches the SDK's behavior before this feature was added.

`FULL_WITH_METADATA_SPANS` is the right mode when the gRPC ingest destination is private but the OTel trace/metric destination is shared and must not receive any content. Tool execution and embedding spans behave like `METADATA_ONLY` under this mode because they have no separate gRPC export.

User-provided `metadata` and `tags` are **not** stripped by any capture mode; callers must avoid putting sensitive content in those dicts when using `METADATA_ONLY` or `FULL_WITH_METADATA_SPANS`. SDK-internal metadata keys that carry content (e.g. `call_error`, `sigil.conversation.title`) are stripped along with the matching content.

### Client-level default

```python
from sigil_sdk import Client, ClientConfig, ContentCaptureMode

client = Client(ClientConfig(
    content_capture=ContentCaptureMode.METADATA_ONLY,
))
```

### Per-generation override

```python
from sigil_sdk import ContentCaptureMode, GenerationStart, ModelRef

with client.start_generation(
    GenerationStart(
        model=ModelRef(provider="openai", name="gpt-5"),
        content_capture=ContentCaptureMode.FULL,
    )
) as rec:
    rec.set_result(
        input=[user_text_message("What is the weather?")],
        output=[assistant_text_message("18C and sunny.")],
    )
```

### Context propagation

Child tool executions inherit the active capture mode from the parent generation via `ContextVar`. You can also set it explicitly for a block:

```python
from sigil_sdk import ContentCaptureMode, with_content_capture_mode

with with_content_capture_mode(ContentCaptureMode.METADATA_ONLY):
    with client.start_tool_execution(
        ToolExecutionStart(tool_name="search")
    ) as rec:
        rec.set_result(arguments={"q": "weather"}, result={"temp_c": 18})
```

### Dynamic resolution via resolver

A callback on `ClientConfig` that resolves the capture mode per-recording at runtime. Useful for feature flags, per-tenant policies, or context-dependent decisions:

```python
from sigil_sdk import Client, ClientConfig, ContentCaptureMode

def resolve_capture(metadata: dict) -> ContentCaptureMode:
    if metadata.get("sigil.tenant") == "healthcare":
        return ContentCaptureMode.METADATA_ONLY
    return ContentCaptureMode.DEFAULT  # fall through to client default

client = Client(ClientConfig(
    content_capture_resolver=resolve_capture,
))
```

### Resolution precedence

For generations, highest to lowest:

1. `GenerationStart.content_capture`
2. `with_content_capture_mode(...)` when set
3. `content_capture_resolver` return value
4. `ClientConfig.content_capture` (defaults to `NO_TOOL_CONTENT`; `DEFAULT` at the client level resolves to `NO_TOOL_CONTENT`)

For tool executions, highest to lowest:

1. `ToolExecutionStart.content_capture`
2. Parent generation's resolved mode, or `with_content_capture_mode(...)` when set
3. `content_capture_resolver` return value
4. `ClientConfig.content_capture` (defaults to `NO_TOOL_CONTENT`; `DEFAULT` at the client level resolves to `NO_TOOL_CONTENT`)

Exceptions in the resolver are caught and treated as `METADATA_ONLY` (fail-closed).

## Export Configuration

### HTTP generation export

```python
from sigil_sdk import ApiConfig, AuthConfig, ClientConfig, GenerationExportConfig

cfg = ClientConfig(
    generation_export=GenerationExportConfig(
        protocol="http",
        endpoint="http://localhost:8080",
        auth=AuthConfig(mode="tenant", tenant_id="dev-tenant"),
    ),
    api=ApiConfig(endpoint="http://localhost:8080"),
)
```

### gRPC generation export

```python
cfg = ClientConfig(
    generation_export=GenerationExportConfig(
        protocol="grpc",
        endpoint="localhost:50051",
        insecure=True,
        auth=AuthConfig(mode="tenant", tenant_id="dev-tenant"),
    ),
    api=ApiConfig(endpoint="http://localhost:8080"),
)
```

## Generation export auth modes

Auth is resolved for `generation_export`.

- `mode="none"`
- `mode="tenant"` (requires `tenant_id`, injects `X-Scope-OrgID`)
- `mode="bearer"` (requires `bearer_token`, injects `Authorization: Bearer <token>`)
- `mode="basic"` (requires `basic_password` + `basic_user` or `tenant_id`, injects `Authorization: Basic <base64(user:password)>`; also injects `X-Scope-OrgID` when `tenant_id` is set — for multi-tenant deployments only, not needed for Grafana Cloud)

Invalid mode/field combinations fail fast in `resolve_config(...)`.

If explicit `headers` already include `Authorization` or `X-Scope-OrgID`, explicit headers win.

```python
from sigil_sdk import ApiConfig, AuthConfig, ClientConfig, GenerationExportConfig

cfg = ClientConfig(
    generation_export=GenerationExportConfig(
        protocol="http",
        endpoint="http://localhost:8080",
        auth=AuthConfig(mode="tenant", tenant_id="prod-tenant"),
    ),
    api=ApiConfig(endpoint="http://localhost:8080"),
)
```

### Grafana Cloud auth (basic)

For Grafana Cloud, use `basic` auth mode. The username is your Grafana Cloud instance/tenant ID and the password is your Grafana Cloud API key. See the [Grafana Cloud AI Observability getting started docs](https://grafana.com/docs/grafana-cloud/machine-learning/ai-observability/get-started/grafana-cloud/) for full setup steps; for this SDK endpoint, copy the **API URL** from **Observability → AI Observability → Configuration**. It looks like `https://sigil-prod-<region>.grafana.net`.

```python
import os
from sigil_sdk import AuthConfig, ClientConfig, GenerationExportConfig

cfg = ClientConfig(
    generation_export=GenerationExportConfig(
        protocol="http",
        endpoint="https://sigil-prod-<region>.grafana.net",
        auth=AuthConfig(
            mode="basic",
            tenant_id=os.environ["SIGIL_AUTH_TENANT_ID"],
            basic_password=os.environ["SIGIL_AUTH_TOKEN"],
        ),
    ),
)
```

If your deployment requires a distinct username, set `basic_user` explicitly:

```python
auth=AuthConfig(
    mode="basic",
    tenant_id=os.environ["SIGIL_AUTH_TENANT_ID"],
    basic_user=os.environ["SIGIL_AUTH_TENANT_ID"],
    basic_password=os.environ["SIGIL_AUTH_TOKEN"],
)
```

## Wiring custom env vars

The SDK only auto-loads `SIGIL_*` env vars (`SIGIL_ENDPOINT`, `SIGIL_PROTOCOL`, `SIGIL_AUTH_MODE`, `SIGIL_AUTH_TOKEN`, etc.) when you call `Client()`. For any other env var (for example one your secret manager exposes under a different name), read it in your app and pass the value into the config:

```python
import os
from sigil_sdk import AuthConfig, ClientConfig

cfg = ClientConfig()

gen_token = (os.getenv("MY_APP_SIGIL_TOKEN") or "").strip()
if gen_token:
    cfg.generation_export.auth = AuthConfig(mode="bearer", bearer_token=gen_token)
```

Common topology:

- Grafana Cloud: generation `basic` mode with instance ID and API key.
- Self-hosted direct to Sigil: generation `tenant` mode.
- Traces/metrics via OTEL Collector/Alloy: configure exporters in your app OTEL SDK setup.
- Enterprise proxy: generation `bearer` mode to proxy; proxy authenticates and forwards tenant header upstream.

## Conversation Ratings

Use the SDK helper to submit user-facing ratings:

```python
from sigil_sdk import ConversationRatingInput, ConversationRatingValue

result = client.submit_conversation_rating(
    "conv-123",
    ConversationRatingInput(
        rating_id="rat-123",
        rating=ConversationRatingValue.BAD,
        comment="Answer ignored user context",
        metadata={"channel": "assistant-ui"},
        source="sdk-python",
    ),
)

print(result.rating.rating, result.summary.has_bad_rating)
```

`submit_conversation_rating(...)` sends requests to `ClientConfig.api.endpoint` (default `http://localhost:8080`) and uses the same generation-export auth headers (`tenant` or `bearer`) already configured on the SDK client.

## Instrumentation-only mode (no generation send)

Set `generation_export.protocol="none"` to keep generation/tool instrumentation and spans while disabling generation transport.

```python
from sigil_sdk import Client, ClientConfig, GenerationExportConfig

cfg = ClientConfig(
    generation_export=GenerationExportConfig(
        protocol="none",
    ),
)

client = Client(cfg)
```

## Lifecycle and Error Semantics

- `flush()` forces immediate export of queued generations.
- `shutdown()` flushes pending generations, then closes generation exporters.
- Always call `shutdown()` during process teardown to avoid dropped telemetry.
- `recorder.set_call_error(exc)` marks provider-call failures on the generation payload and span status.
- `recorder.err()` is for local SDK runtime errors only (validation, queue full, payload too large, shutdown).

## SDK metrics

The SDK emits these OTel histograms through your configured OTEL meter provider:

- `gen_ai.client.operation.duration`
- `gen_ai.client.token.usage`
- `gen_ai.client.time_to_first_token`
- `gen_ai.client.tool_calls_per_operation`

## Experiments (offline evaluation)

Run any agent over a dataset as a Sigil **experiment**, grade locally, and
publish scores you can compare in the Sigil UI — no framework adapter required.
The runner rides on the core generation recording above: record the agent's call
through `run.start_generation(...)` and every generation is auto-tagged with the
experiment `run_id` and captured for scoring.

```python
from sigil_sdk import (
    DatasetItem, ExperimentRunner, Generation, GenerationStart, ModelRef,
    ScoreOutput, ScoreValue, TargetResult, assistant_text_message, user_text_message,
)

dataset = [
    DatasetItem(id="capital-fr", input="Capital of France?", expected="Paris",
                metadata={"task_id": "capital", "task_category": "trivia"}),
]

def target(item, run):
    # Record the agent's call so the generation carries the experiment run_id.
    with run.start_generation(GenerationStart(model=ModelRef(provider="openai", name="gpt-4o-mini"))) as rec:
        answer = my_agent(item.input)  # your code
        rec.set_result(Generation(
            model=ModelRef(provider="openai", name="gpt-4o-mini"),
            input=[user_text_message(str(item.input))],
            output=[assistant_text_message(answer)],
        ))
    return TargetResult(output=answer)  # generation ids captured automatically

def exact_match(item, result):
    passed = str(item.expected).lower() in str(result.output).lower()
    return [ScoreOutput(evaluator_id="suite.exact_match", evaluator_version="2026-05-30",
                        score_key="exact_match", value=ScoreValue(number=1.0 if passed else 0.0),
                        passed=passed)]

runner = ExperimentRunner(client=client, run_id="pr-123", name="PR 123",
                          dataset={"id": "smoke", "version": "2026-05-30"}, tags=["ci"])
result = runner.run(dataset, target, [exact_match])
print(result.url)  # deep link to the experiment in Sigil
```

The runner creates the run (`source="external"`), runs + grades each item,
exports scores attributed to the `run_id`, and finalizes the run (`succeeded` on
clean exit, `failed` on exception, `canceled` on Ctrl-C). For ad-hoc loops use
the lower-level `experiment(...)` context manager. A/B testing is two runs with
different `run_id`/`tags`. Upload modes: `continuous` (default, publish per
item), `bulk` (publish at the end), `manual` (publish + finalize only when you
call `run.publish()` / `run.finalize()`).

If you use a supported framework, prefer its adapter (e.g. `sigil-sdk-langgraph`)
— it auto-captures generation ids from the framework callback so you don't wrap
`start_generation` yourself. See the `sigil-experiments` skill
(`python/skills/sigil-experiments/SKILL.md`) and the runnable example at
`examples/python-experiment/` for grading patterns (including LLM-as-judge) and
uploading older runs.

## Public API Overview

Core client and lifecycle:

- `Client`
- `Client.start_generation(...)`
- `Client.start_streaming_generation(...)`
- `Client.start_tool_execution(...)`
- `Client.flush()`
- `Client.shutdown()`

Typed payloads:

- `GenerationStart`, `Generation`, `ModelRef`
- `Message`, `Part`, `ToolDefinition`, `TokenUsage`
- `ToolExecutionStart`, `ToolExecutionEnd`
- `ContentCaptureMode`

Helpers:

- `user_text_message(...)`, `assistant_text_message(...)`
- `with_conversation_id(...)`, `with_agent_name(...)`, `with_agent_version(...)`
- `with_content_capture_mode(...)`

Validation:

- `validate_generation(...)`

Experiments (offline evaluation):

- `experiment(...)`, `ExperimentRunner`, `ExperimentRun`
- `DatasetItem`, `TargetResult`, `ScoreOutput`, `ExperimentResult`
- `stable_id(...)`
- `Client.create_experiment(...)`, `Client.export_scores(...)`, `Client.complete_experiment(...)`, `Client.experiment_url(...)`

## Provider Helper Packages

Provider wrappers are wrapper-first and mapper-explicit:

- `sigil-sdk-openai`
- `sigil-sdk-anthropic`
- `sigil-sdk-gemini`

Each package exposes sync + async wrappers and explicit mapper functions for custom integration points.