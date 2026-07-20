# Grafana Sigil Python SDK

`agento11y` records normalized LLM generation and tool-execution telemetry. It exports normalized generations to Sigil ingest and uses your OpenTelemetry tracer/meter setup for traces and metrics.

Use this package when you want:

- A provider-agnostic generation record (same schema for OpenAI, Anthropic, Gemini, or custom adapters).
- OTel-aligned tracing attributes for generation and tool spans.
- Async export with retry/backoff, queueing, batching, and explicit shutdown semantics.

## Installation

```bash
pip install agento11y
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
pip install agento11y-openai
pip install agento11y-anthropic
pip install agento11y-gemini
```

Optional framework modules:

```bash
pip install agento11y-langchain
pip install agento11y-langgraph
pip install agento11y-openai-agents
pip install agento11y-llamaindex
pip install agento11y-google-adk
pip install agento11y-strands
pip install agento11y-claude-agent-sdk
pip install agento11y-litellm
pip install agento11y-pydantic-ai
```

Framework handler usage:

```python
from agento11y import Client
from agento11y_langchain import with_agento11y_langchain_callbacks
from agento11y_langgraph import with_agento11y_langgraph_callbacks
from agento11y_openai_agents import with_agento11y_openai_agents_hooks
from agento11y_llamaindex import with_agento11y_llamaindex_callbacks
from agento11y_google_adk import with_agento11y_google_adk_callbacks
from agento11y_strands import with_agento11y_strands_hooks
from agento11y_claude_agent import with_agento11y_claude_agent_options
from agento11y_pydantic_ai import with_agento11y_pydantic_ai_capability

client = Client()
chain_config = with_agento11y_langchain_callbacks(None, client=client, provider_resolver="auto")
graph_config = with_agento11y_langgraph_callbacks(None, client=client, provider_resolver="auto")
openai_agents_run_options = with_agento11y_openai_agents_hooks(None, client=client, provider_resolver="auto")
llamaindex_config = with_agento11y_llamaindex_callbacks(None, client=client, provider_resolver="auto")
google_adk_agent_config = with_agento11y_google_adk_callbacks(None, client=client, provider_resolver="auto")
strands_agent_config = with_agento11y_strands_hooks(None, client=client, provider_resolver="auto")
claude_agent_options = with_agento11y_claude_agent_options(None, client=client)
pydantic_ai_capabilities = with_agento11y_pydantic_ai_capability(None, client=client, provider_resolver="auto")
```

LiteLLM uses a callback class instead of a `with_agento11y_*` helper:

```python
import litellm
from agento11y import Client
from agento11y_litellm import Agento11yLiteLLMLogger

client = Client()
litellm.callbacks = [Agento11yLiteLLMLogger(client=client)]
```

Framework handlers use the `Client` instance you pass in. If that client is configured with
`generation_sanitizer`, the same redaction policy applies automatically to generations recorded
through LangChain, LangGraph, OpenAI Agents, LlamaIndex, Google ADK, Strands, Claude Agent SDK, LiteLLM, and Pydantic AI integrations.

Framework handlers inject framework tags/metadata on recorded generations:

- `agento11y.framework.name` (`langchain`, `langgraph`, `openai-agents`, `llamaindex`, `google-adk`, `strands`, `claude-agent-sdk`, `litellm`, or `pydantic-ai`)
- `agento11y.framework.source=handler` (or `hooks` for Strands Agents and Claude Agent SDK)
- `agento11y.framework.language=python`
- `metadata["agento11y.framework.run_id"]`
- `metadata["agento11y.framework.thread_id"]` (when present)
- `metadata["agento11y.framework.parent_run_id"]` (when available)
- `metadata["agento11y.framework.component_name"]`
- `metadata["agento11y.framework.run_type"]`
- `metadata["agento11y.framework.tags"]`
- `metadata["agento11y.framework.retry_attempt"]` (when available)
- `metadata["agento11y.framework.event_id"]` (when available)
- `metadata["agento11y.framework.langgraph.node"]` (LangGraph when available)

Conversation mapping is conversation-first:

- `conversation_id` / `session_id` / `group_id` from framework context first
- then `thread_id`
- deterministic fallback `agento11y:framework:<framework_name>:<run_id>`

When present in generation metadata, low-cardinality framework keys are copied onto generation span attributes.

For LangGraph persistence, pass `configurable.thread_id` and reuse it across invocations:

```python
thread_config = {
    **with_agento11y_langgraph_callbacks(None, client=client, provider_resolver="auto"),
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
- Claude Agent SDK: `../python-frameworks/claude-agent-sdk/README.md`
- LiteLLM: `../python-frameworks/litellm/README.md`
- Pydantic AI: `../python-frameworks/pydantic-ai/README.md`

## Quick Start (Sync Generation)

`Client()` reads `AGENTO11Y_*` env vars by default. See the [Grafana Cloud setup guide](https://grafana.com/docs/grafana-cloud/machine-learning/ai-observability/get-started/grafana-cloud/) for the variable names. Pass an explicit `ClientConfig` only when you need to override.

```python
from agento11y import (
    Client,
    GenerationStart,
    ModelRef,
    assistant_text_message,
    user_text_message,
)

client = Client()  # reads AGENTO11Y_* env vars

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
import os
from agento11y import AuthConfig, Client, ClientConfig, GenerationExportConfig

client = Client(
    ClientConfig(
        generation_export=GenerationExportConfig(
            protocol="http",
            endpoint="https://sigil-prod-<region>.grafana.net",
            auth=AuthConfig(
                mode="basic",
                tenant_id=os.environ["AGENTO11Y_AUTH_TENANT_ID"],
                basic_password=os.environ["AGENTO11Y_AUTH_TOKEN"],
            ),
        ),
    )
)
```

## Pre-Ingest Redaction

Use `generation_sanitizer` when you want to redact substrings from normalized generations before
validation, span sync, and export.

```python
from agento11y import (
    Client,
    ClientConfig,
    SecretRedactionOptions,
    create_secret_redaction_sanitizer,
)

client = Client(
    ClientConfig(
        generation_sanitizer=create_secret_redaction_sanitizer(
            SecretRedactionOptions(
                redact_input_messages=False,  # None falls back to AGENTO11Y_REDACT_INPUT_MESSAGES, then False
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
- leaves user input unchanged unless input redaction is enabled

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

### Configuring redaction via environment variables

`create_secret_redaction_sanitizer()` reads `AGENTO11Y_REDACT_INPUT_MESSAGES` (accepts `1/0`, `true/false`, `yes/no`, `on/off`) when `redact_input_messages` is left `None`. Precedence is explicit option > env var > `False`. An unrecognised env value logs a warning through the `agento11y` logger and falls back to the next layer, so a typo cannot silently flip redaction.

```python
from agento11y import (
    Client,
    ClientConfig,
    create_secret_redaction_sanitizer,
)

# Leave redact_input_messages unset so AGENTO11Y_REDACT_INPUT_MESSAGES decides.
client = Client(
    ClientConfig(
        generation_sanitizer=create_secret_redaction_sanitizer(),
    )
)
```

## Hooks and Guards

Use hooks when you want Sigil guard rules to run before an LLM call. The SDK evaluates the hook on your request path; guard rules configured in Grafana Cloud decide whether to allow, deny, or transform the input.

Hooks are disabled by default. Enable them on the client and call `evaluate_hook(...)` before the provider request:

```python
from agento11y import (
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
from agento11y import GenerationStart, ModelRef

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
from agento11y import EmbeddingResult, EmbeddingStart, ModelRef

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
from agento11y import ClientConfig, EmbeddingCaptureConfig

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
from agento11y import ToolExecutionStart

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
  - `agento11y.sdk.name=sdk-python`
- Normalized generation metadata always includes the same key.
- If caller metadata provides a conflicting value for this key, the SDK overwrites it.

## Context Defaults

Use context helpers to set defaults once per request/task boundary.

```python
from agento11y import with_agent_name, with_agent_version, with_conversation_id

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

User-provided `metadata` and `tags` are **not** stripped by any capture mode; callers must avoid putting sensitive content in those dicts when using `METADATA_ONLY` or `FULL_WITH_METADATA_SPANS`. SDK-internal metadata keys that carry content (e.g. `call_error`, `agento11y.conversation.title`) are stripped along with the matching content. See [Tags and Metadata](../docs/concepts/tags-and-metadata.md) for where client tags, per-generation tags, metadata, and `user_id` each show up (export vs spans vs metrics).

### Client-level default

```python
from agento11y import Client, ClientConfig, ContentCaptureMode

client = Client(ClientConfig(
    content_capture=ContentCaptureMode.METADATA_ONLY,
))
```

### Per-generation override

```python
from agento11y import ContentCaptureMode, GenerationStart, ModelRef

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
from agento11y import ContentCaptureMode, with_content_capture_mode

with with_content_capture_mode(ContentCaptureMode.METADATA_ONLY):
    with client.start_tool_execution(
        ToolExecutionStart(tool_name="search")
    ) as rec:
        rec.set_result(arguments={"q": "weather"}, result={"temp_c": 18})
```

### Dynamic resolution via resolver

A callback on `ClientConfig` that resolves the capture mode per-recording at runtime. Useful for feature flags, per-tenant policies, or context-dependent decisions:

```python
from agento11y import Client, ClientConfig, ContentCaptureMode

def resolve_capture(metadata: dict) -> ContentCaptureMode:
    if metadata.get("tenant") == "healthcare":
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
import os
from agento11y import ApiConfig, AuthConfig, ClientConfig, GenerationExportConfig

cfg = ClientConfig(
    generation_export=GenerationExportConfig(
        protocol="http",
        endpoint="https://sigil-prod-<region>.grafana.net",
        auth=AuthConfig(
            mode="basic",
            tenant_id=os.environ["AGENTO11Y_AUTH_TENANT_ID"],
            basic_password=os.environ["AGENTO11Y_AUTH_TOKEN"],
        ),
    ),
    api=ApiConfig(endpoint="https://sigil-prod-<region>.grafana.net"),
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
import os
from agento11y import ApiConfig, AuthConfig, ClientConfig, GenerationExportConfig

cfg = ClientConfig(
    generation_export=GenerationExportConfig(
        protocol="http",
        endpoint="https://sigil-prod-<region>.grafana.net",
        auth=AuthConfig(
            mode="basic",
            tenant_id=os.environ["AGENTO11Y_AUTH_TENANT_ID"],
            basic_password=os.environ["AGENTO11Y_AUTH_TOKEN"],
        ),
    ),
    api=ApiConfig(endpoint="https://sigil-prod-<region>.grafana.net"),
)
```

### Grafana Cloud auth (basic)

For Grafana Cloud, use `basic` auth mode. The username is your Grafana Cloud instance/tenant ID and the password is your Grafana Cloud API key. See the [Grafana Cloud AI Observability getting started docs](https://grafana.com/docs/grafana-cloud/machine-learning/ai-observability/get-started/grafana-cloud/) for full setup steps; for this SDK endpoint, copy the **API URL** from **Observability → AI Observability → Configuration**. It looks like `https://sigil-prod-<region>.grafana.net`.

```python
import os
from agento11y import AuthConfig, ClientConfig, GenerationExportConfig

cfg = ClientConfig(
    generation_export=GenerationExportConfig(
        protocol="http",
        endpoint="https://sigil-prod-<region>.grafana.net",
        auth=AuthConfig(
            mode="basic",
            tenant_id=os.environ["AGENTO11Y_AUTH_TENANT_ID"],
            basic_password=os.environ["AGENTO11Y_AUTH_TOKEN"],
        ),
    ),
)
```

If your deployment requires a distinct username, set `basic_user` explicitly:

```python
auth=AuthConfig(
    mode="basic",
    tenant_id=os.environ["AGENTO11Y_AUTH_TENANT_ID"],
    basic_user=os.environ["AGENTO11Y_AUTH_TENANT_ID"],
    basic_password=os.environ["AGENTO11Y_AUTH_TOKEN"],
)
```

## Wiring custom env vars

The SDK only auto-loads `AGENTO11Y_*` env vars (`AGENTO11Y_ENDPOINT`, `AGENTO11Y_PROTOCOL`, `AGENTO11Y_AUTH_MODE`, `AGENTO11Y_AUTH_TOKEN`, etc.) when you call `Client()`. For any other env var (for example one your secret manager exposes under a different name), read it in your app and pass the value into the config:

```python
import os
from agento11y import AuthConfig, ClientConfig

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
from agento11y import ConversationRatingInput, ConversationRatingValue

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

`submit_conversation_rating(...)` sends requests to `ClientConfig.api.endpoint`, which should be the Grafana Cloud Sigil API URL from AI Observability configuration, and uses the same generation-export auth headers already configured on the SDK client.

## Instrumentation-only mode (no generation send)

Set `generation_export.protocol="none"` to keep generation/tool instrumentation and spans while disabling generation transport.

```python
from agento11y import Client, ClientConfig, GenerationExportConfig

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

## Experiments

Run any agent over a dataset as a Sigil **experiment** (offline evaluation),
grade its outputs, and publish scores you can compare in the Sigil UI. This is
the framework-free path for Cloud users: one ingestion API key writes the run,
trials, generations, scores, and final status.

```python
from agento11y import experiments

suite = experiments.TestSuite(
    suite_id="smoke",
    name="Smoke",
    test_cases=[
        experiments.TestCase(test_case_id="capital-fr", input="Capital of France?", expected="Paris"),
    ],
)
verifier = experiments.Evaluator(evaluator_id="exact_match", version="2026-06-29")

with agento11y.experiment("PR 123", experiment_id="pr-123", suite=suite, tags=["ci"]) as exp:
    for case in suite.test_cases:
        with exp.trial(case) as trial:
            answer = my_agent(case.input)

            # If your normal instrumentation already produced ids, bind them instead:
            # trial.bind_conversation(conversation_id)
            # trial.bind_generation(generation_id, conversation_id=conversation_id)
            trial.record_io(input=case.input, output=answer, model_provider="openai", model_name="gpt-4o-mini")

            passed = str(case.expected).lower() in answer.lower()
            trial.final_score(1.0 if passed else 0.0, passed=passed, evaluator=verifier)

    print(exp.url)  # deep link to the experiment in Sigil
```

`experiment(...)` creates the run (`source="external"`), each `exp.trial(...)`
creates a typed trial, and the trial exports scores on exit. On normal exit the
run finalizes as `completed`; on exception or Ctrl-C it finalizes as `failed`.
A/B testing is two runs with different `experiment_id`/`tags` over the same
suite and evaluators.

Experiment writes use the same Grafana Cloud ingestion API key as generation
ingest. They do not require a control-plane URL or a separate eval API key.
Experimental OTel eval spans/events are disabled by default; opt in with
`use_experimental_otel=True` on `agento11y.experiment(...)` or
`AGENTO11Y_USE_EXPERIMENTAL_OTEL=true`.

If you use a supported framework, prefer its adapter (e.g. `agento11y-langgraph`)
— it can expose conversation or generation ids that you bind to the trial, so
the experiment points at the same trace your agent already emits. See the
`agento11y-experiments` skill
(`python/skills/agento11y-experiments/SKILL.md`) and the runnable example at
`examples/experiments/python/` for grading patterns, including LLM-as-judge.

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

Experiments:

- `agento11y.experiments.experiment(...)`
- `agento11y.experiments.Client`
- `agento11y.experiments.Experiment`, `Trial`, `TrialRef`
- `agento11y.experiments.TestSuite`, `TestCase`, `Evaluator`
- `agento11y.experiments.stable_id(...)`

## Provider Helper Packages

Provider wrappers are wrapper-first and mapper-explicit:

- `agento11y-openai`
- `agento11y-anthropic`
- `agento11y-gemini`

Each package exposes sync + async wrappers and explicit mapper functions for custom integration points.
