# Getting Started - Python + OpenAI Agents SDK

A small order-support assistant uses two local tools to check order `DEMO-1001`, then its shipping ETA.
Both variants share the agent and two-turn conversation in [`agent.py`](agent.py).

## Setup

Use Python 3.11 or later:

```bash
cd examples/getting-started/python-openai-agents
uv venv .venv
source .venv/bin/activate
uv pip install -r requirements.txt
cp .env.example .env
```

The requirements install the SDKs from this checkout, like the sibling examples.
An OTel-only copy can drop the two `-e` lines. Edit `.env` with your OpenAI and Grafana Cloud settings.
Both variants need traces and metrics through OTLP:

```bash
OPENAI_API_KEY=...
OPENAI_MODEL=gpt-4o-mini
AGENTO11Y_CONVERSATION_ID=openai-agents-support-demo
OTEL_EXPORTER_OTLP_ENDPOINT=https://otlp-gateway-prod-<region>.grafana.net/otlp
OTEL_EXPORTER_OTLP_HEADERS="Authorization=Basic <base64 of OTLP_INSTANCE_ID:OTLP_TOKEN>"
OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
OTEL_METRIC_EXPORT_INTERVAL_MILLIS=1000
OTEL_SERVICE_NAME=agento11y-openai-agents-example
```

Copy OTLP settings from your Cloud portal or use an Alloy/Collector endpoint that forwards to Cloud.
The HTTP exporters append `/v1/traces` and `/v1/metrics`; signal-specific overrides take full URLs.
See the [OTLP setup guide](https://grafana.com/docs/grafana-cloud/send-data/otlp/send-data-otlp/).
The native variant also needs generation-export settings:

```bash
AGENTO11Y_PROTOCOL=http
AGENTO11Y_AUTH_MODE=basic
AGENTO11Y_ENDPOINT=https://agento11y-prod-<region>.grafana.net
AGENTO11Y_AUTH_TENANT_ID=...
AGENTO11Y_AUTH_TOKEN=...
AGENTO11Y_AGENT_VERSION=dev
```

Find these in the [Agent Observability setup docs](https://grafana.com/docs/grafana-cloud/machine-learning/agent-observability/get-started/grafana-cloud/).
Generation export and OTLP use separate endpoints/authentication; their instance IDs can differ.

## Which variant should I use?

| | Standard OpenTelemetry | Native agento11y |
| --- | --- | --- |
| Script | [`main_otel.py`](main_otel.py) | [`main_agento11y.py`](main_agento11y.py) |
| Integration | Agents + OpenAI OTel instrumentors | `agento11y-openai-agents` run hooks |
| Export | GenAI spans and metrics through OTLP | Generation API plus SDK spans and metrics through OTLP |
| Conversation view | Public preview ingestion of opted-in chat spans | Native records with prompt/response text and token usage |
| Grouping | `RunConfig.group_id` | Run-context conversation mapping |
| Framework detail | Workflow, agent, tool, and model-call spans | Framework tags, agent/LLM/handoff lineage, and tool spans |

Choose OTel for an existing collector pipeline, vendor-neutral instrumentation, and the preview conversation view.
Choose native agento11y for generation records and framework lineage, or to extend the app with core SDK features.
The example uses one agent; the native bridge also handles handoff callbacks when your application uses them.

## Run

Choose one script per process to avoid duplicate telemetry. Both print two replies followed by `Done`, preserve
history with `result.to_input_list()`, and reuse `AGENTO11Y_CONVERSATION_ID`; change it for a new conversation.

### Variant 1: standard OpenTelemetry

```bash
python main_otel.py
```

The beta [`opentelemetry-instrumentation-genai-openai-agents`](https://pypi.org/project/opentelemetry-instrumentation-genai-openai-agents/)
succeeds the deprecated `opentelemetry-instrumentation-openai-agents-v2` package (security fixes only).
It emits workflow/agent/tool spans; the companion OpenAI instrumentor emits Responses API `chat` spans and metrics.

[OTel ingestion is public preview](https://grafana.com/docs/grafana-cloud/observe-and-act/agent-observability/get-started/opentelemetry/).
The script appends the `agento11y.record=true` opt-in to existing `OTEL_RESOURCE_ATTRIBUTES` and defaults
`OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT=span_only` so input/output messages are span attributes.
Chat spans become the generations; event/log-only content capture does not populate the conversation view.
`RunConfig.group_id` propagates to agent/chat spans as `gen_ai.conversation.id`.
`AgentLabels` adds only `gen_ai.agent.name` for this single agent, including to chat/tool spans;
`invoke_agent` already has its name and keeps provider/model unset to avoid duplicate generations.

`instrument(disable_openai_trace_export=True)` replaces OpenAI's hosted exporter with the OTel processor.
Alternatively, call `agents.set_trace_processors([])` **before** instrumentation; calling it afterward removes
the OTel processor too. Keep Agents tracing enabled so the processor receives callbacks.

### Variant 2: native agento11y

```bash
python main_agento11y.py
```

Do **not** set `agento11y.record=true` in environment or collector resources: native generation export already
populates conversations, so preview ingestion could duplicate them. This script installs neither OTel instrumentor
and clears OpenAI's hosted tracing processors; native run hooks work independently.

`context={"conversation_id": ...}` supplies the stable ID. Mapping precedence is `conversation_id` / `session_id` /
`group_id`, then `thread_id`, then a deterministic per-run fallback; see the [adapter README](../../../python-frameworks/openai-agents/README.md).
The adapter records model prompt/response text, token usage, framework tags/metadata, agent/LLM/handoff parent IDs,
and agent name/version. Tool executions are OTel spans with result capture, not separate generation requests;
arguments depend on `context.tool_input`. Model inputs are flattened to text. The core SDK emits spans and GenAI
metrics through OTLP; providers are registered before `Client`, and the client shuts down before the providers.

## What to look for

- Find your conversation with both turns and the order-status / two-business-day ETA replies.
- OTel: inspect `chat` spans for operation, provider, model, conversation ID, and input/output messages.
  Inspect workflow/agent/tool nesting in Traces; `invoke_agent` spans have no provider/model attributes.
- Native: inspect model generations for text, model, usage, framework tags, and parent run IDs, plus tool spans.
  The adapter does not enable workflow-step capture for the Workflow tab.
- Check Traces and Metrics for spans, `gen_ai.client.operation.duration`, and `gen_ai.client.token.usage`.

## Next steps

The run hooks record lifecycle events; guards, experiments/scores, and conversation titles need core SDK wiring.
See [Python hooks and guards](../python-hooks/), [experiments](../../experiments/), and
[`with_conversation_title`](../../../python/agento11y/context.py).
