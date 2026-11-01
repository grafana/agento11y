# Getting Started - Python + OpenAI Agents SDK

Runs one order-support agent with two local tools through a two-turn conversation.
Both variants share [`agent.py`](agent.py) and send telemetry to Grafana Cloud in different ways:

| Script | Integration |
| --- | --- |
| [`main_otel.py`](main_otel.py) | Standard OpenTelemetry GenAI instrumentation with `opentelemetry-instrumentation-genai-openai-agents` + `opentelemetry-instrumentation-genai-openai` (successors to the deprecated `-v2` packages); no agento11y SDK. |
| [`main_agento11y.py`](main_agento11y.py) | Native `agento11y-openai-agents` hooks with generation export: full generation records, conversation mapping, framework lineage, and room to add the SDK's guards and experiments. |

Choose OTel for standard instrumentation and an existing OTel pipeline, or native agento11y for richer generation records and SDK features.

## Setup

Use Python 3.11 or later:

```bash
cd examples/getting-started/python-openai-agents
uv venv .venv
source .venv/bin/activate
uv pip install -r requirements.txt
cp .env.example .env
```

Edit `.env`; [`.env.example`](.env.example) identifies the OpenAI and OTLP variables both variants need and the generation-export variables only the native variant needs.
See the [Grafana Cloud Agent Observability getting started docs](https://grafana.com/docs/grafana-cloud/machine-learning/agent-observability/get-started/grafana-cloud/) for connection settings and the [OTel ingestion docs](https://grafana.com/docs/grafana-cloud/observe-and-act/agent-observability/get-started/opentelemetry/) for the public preview path.

Requirements install the SDKs from this checkout, like the sibling examples; an OTel-only copy can drop the two `-e` lines.

## Run

Run one variant per process.

```bash
python main_otel.py
```

Opts in with `agento11y.record=true`, captures messages as span attributes, and maps `RunConfig.group_id` to `gen_ai.conversation.id`.
Disables OpenAI's hosted trace export; if using `set_trace_processors([])` instead, call it **before** instrumenting so the OTel processor stays installed.

```bash
python main_agento11y.py
```

Do not set `agento11y.record=true` here: native export already sends generations, so opting in the spans creates duplicate generations.
The `conversation_id` comes from the run context.

## Where to look

After both replies and `Done` print, open Agent Observability in your Grafana Cloud stack to find both turns grouped in one conversation.
Check Traces for agent, tool, and model-call spans, and Metrics for `gen_ai.client.token.usage`, `gen_ai.client.operation.duration`, and other GenAI metrics.
