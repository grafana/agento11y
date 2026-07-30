# Agent Observability + LangChain weather example

A deliberately small FastAPI service that demonstrates **two ways to instrument LLM calls with agento11y from the same app**:


| Path                        | How it's instrumented                                        | Where to look                              |
| --------------------------- | ------------------------------------------------------------ | ------------------------------------------ |
| LangChain weather agent     | `with_agento11y_langchain_callbacks(...)` on the runnable config | `[app/agent.py](./app/agent.py)`           |
| Direct Anthropic classifier | `agento11y.start_generation(...)` + `rec.set_result(...)`        | `[app/classifier.py](./app/classifier.py)` |


Both LLM calls run under the same `conversation_id`, so they land grouped together in the Agent Observability UI. That makes it easy to compare how each path shows up, and to see that framework callbacks and raw SDK recording produce the same canonical generation shape.

## What the demo teaches

1. **Setting up OpenTelemetry for a FastAPI app.** `[app/telemetry.py](./app/telemetry.py)` wires a `TracerProvider` and `MeterProvider` with OTLP gRPC exporters.
2. **Creating an agento11y client.** `[app/agento11y_client.py](./app/agento11y_client.py)` builds an `agento11y.Client` that reuses those OTel providers, so `gen_ai.`* spans and metrics flow through the same pipeline as the rest of the app.
3. **Instrumenting a LangChain agent.** One line in `[app/agent.py](./app/agent.py)` — `with_agento11y_langchain_callbacks(config, client=agento11y, ...)` — attaches the agento11y callback handler to the agent's config.
4. **Instrumenting arbitrary LLM code.** `[app/classifier.py](./app/classifier.py)` shows the raw SDK pattern for any provider call, regardless of framework.
5. **Grouping related generations.** Passing a common `conversation_id` ties both calls together in the Agent Observability UI.

## Prerequisites

- Python 3.11+
- [uv](https://docs.astral.sh/uv/)
- An `ANTHROPIC_API_KEY`

## Setup

```bash
cd examples/python-langchain
cp .env.example .env
uv sync
```

By default `pyproject.toml` pins `agento11y` and `agento11y-langchain` to the local packages in this monorepo via `[tool.uv.sources]`. Remove that block to consume the published PyPI releases instead.

## Run

```bash
uv run uvicorn app.main:app --reload --port 8000
```

## Try it

```bash
# On-topic: agent uses the tool, classifier returns ON_TOPIC
curl -s http://<app-host>:8000/chat \
  -H 'content-type: application/json' \
  -d '{"message": "Whats the weather in Paris on 2026-04-18?"}' | jq

# Off-topic: agent declines, classifier returns OFF_TOPIC
curl -s http://<app-host>:8000/chat \
  -H 'content-type: application/json' \
  -d '{"message": "Write me a limerick about kubernetes."}' | jq
```

FastAPI also serves interactive docs at `http://<app-host>:8000/docs`.

## What to look for in Agent Observability

Open the Agent Observability UI. For each request you should see:

- A **conversation** identified by the `conv-…` id returned in the response.
- Two generations under it:
  - `weather-agent` — emitted by the LangChain callback handler, with a child `execute_tool` span for `get_weather` when invoked.
  - `topic-classifier` — emitted by the manual SDK instrumentation.
- In Tempo: a `chat.request` parent span containing `gen_ai.`* child spans from both paths, plus LangChain chain/tool spans.

## Project layout

```
app/
  telemetry.py      # OTel TracerProvider / MeterProvider bootstrap
  agento11y_client.py   # agento11y SDK client, wired to the OTel providers
  agent.py          # LangChain agent + get_weather tool
  classifier.py     # Direct Anthropic call, manually recorded to Agent Observability
  weather.py        # Stubbed April 2026 weather data
  main.py           # FastAPI app (POST /chat, GET /healthz)
```

## Environment variables

See `[.env.example](./.env.example)`.


| Variable                           | Purpose                                                                                                                               |
| ---------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------- |
| `ANTHROPIC_API_KEY`                | Required. Used by both the agent and the classifier.                                                                                  |
| `AGENTO11Y_ENDPOINT`                   | API URL from Agent Observability → Configuration. Required for Grafana Cloud.                                                            |
| `AGENTO11Y_AUTH_TENANT_ID`             | Numeric instance ID. Sent as `X-Scope-OrgID` and used as basic-auth user.                                                             |
| `AGENTO11Y_AUTH_TOKEN`                 | Cloud Access Policy Token (`glc_…`) with `sigil:write` scope. Required for Cloud.                                                     |
| `OTEL_EXPORTER_OTLP_ENDPOINT`      | OTLP target for traces + metrics. Use the Grafana Cloud OTLP gateway URL or your collector endpoint.                                  |
| `OTEL_EXPORTER_OTLP_INSECURE`      | Set only when your collector endpoint explicitly requires plaintext gRPC.                                                             |
| `OTEL_SERVICE_NAME`                | Service name tag on spans / metrics.                                                                                                  |
| `AGENT_MODEL` / `CLASSIFIER_MODEL` | Anthropic model IDs.                                                                                                                  |

