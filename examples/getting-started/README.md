# Getting Started with AI Observability

Minimal, self-contained examples that make a real LLM call and record the generation to Grafana Cloud AI Observability. Pick your language and you should be up and running in under five minutes.

### Single generation


| Language             | Directory                                    | LLM provider                   |
| -------------------- | -------------------------------------------- | ------------------------------ |
| Python               | `[python/](python/)`                         | OpenAI                         |
| Python + Strands     | `[python-strands/](python-strands/)`         | OpenAI by default, Sigil Cloud |
| Python + Claude Agent SDK | `[python-claude-agent-sdk/](python-claude-agent-sdk/)` | Claude Code CLI, Sigil Cloud |
| TypeScript           | `[typescript/](typescript/)`                 | OpenAI                         |
| TypeScript + Strands | `[typescript-strands/](typescript-strands/)` | OpenAI by default, Sigil Cloud |
| Go                   | `[go/](go/)`                                 | OpenAI                         |


### Hooks and guards


| Language   | Directory                                  | LLM provider | Guard phase |
| ---------- | ------------------------------------------ | ------------ | ----------- |
| Go         | `[go-hooks/](go-hooks/)`                   | OpenAI       | Preflight   |
| Python     | `[python-hooks/](python-hooks/)`           | OpenAI       | Preflight   |
| TypeScript | `[typescript-hooks/](typescript-hooks/)`   | OpenAI       | Preflight   |


### Multi-agent dependency graph


| Language | Directory                                    | LLM provider      |
| -------- | -------------------------------------------- | ----------------- |
| Python   | `[python-multi-agent/](python-multi-agent/)` | OpenAI            |


Each example configures OpenTelemetry, creates an SDK client, makes LLM calls, records generations, and shuts down cleanly.

## Credentials

Each example needs an **OpenAI API key** ([platform.openai.com/api-keys](https://platform.openai.com/api-keys)) and your **Grafana Cloud credentials** (instance ID, API token, endpoint URL).

See the [credentials section in the SDK README](../../README.md#grafana-cloud-credentials) for where to find each value in your Grafana Cloud stack.

### OTel endpoint for traces and metrics

The SDK emits OpenTelemetry spans and metrics (`gen_ai.client.operation.duration`, `gen_ai.client.token.usage`, etc.). These need an OTLP endpoint:

- **Direct to Cloud** — set `OTEL_EXPORTER_OTLP_ENDPOINT` to your Cloud OTLP gateway URL (find it in the Grafana Cloud portal → stack Details page, [docs](https://grafana.com/docs/grafana-cloud/send-data/otlp/send-data-otlp)) and `OTEL_EXPORTER_OTLP_HEADERS` with Basic auth credentials.
- **Via Alloy or an OTel Collector** — set `OTEL_EXPORTER_OTLP_ENDPOINT` to the collector endpoint your deployment provides, with the collector forwarding to Grafana Cloud.

Set all values as environment variables before running an example.

## What to look for

After running an example, open the AI Observability plugin in your Grafana Cloud stack. You should see:

- A new generation under the conversation ID used in the example.
- Model name, provider, token usage, and latency filled in.
- The input prompt and assistant response visible in the conversation drilldown.
- Traces in your Grafana Cloud Traces datasource and metrics in Grafana Cloud Metrics.

## Custom tags and metadata

The Python, TypeScript, and Go single-generation examples set a client-level tag, a per-generation tag, `metadata`, and `user_id` so you can see where each one shows up. Client tags reach the generation plus (on Go/JS) OTel spans/metrics as `sigil.tag.<key>`; per-generation tags and metadata are export-only; `user_id` becomes the `user.id` span attribute. See [Tags and Metadata](../../docs/concepts/tags-and-metadata.md) for the full routing table and cardinality rules.

## Next steps

- **Provider wrappers** — reduce boilerplate by using pre-built wrappers for [OpenAI](../../go-providers/openai/), [Anthropic](../../python-providers/anthropic/), and [Gemini](../../go-providers/gemini/).
- **Framework adapters** — instrument [LangChain](../../python-frameworks/langchain/), [Strands Agents for Python](../../python-frameworks/strands/), [Claude Agent SDK](../../python-frameworks/claude-agent-sdk/), [Strands Agents for TypeScript](../../js/docs/frameworks/strands.md), [Vercel AI SDK](../../js/docs/frameworks/vercel-ai-sdk.md), [Google ADK](../../go-frameworks/google-adk/), and more with a single line.
- **Full example app** — see `[examples/python-langchain/](../python-langchain/)` for a FastAPI service with LangChain agent + manual instrumentation side by side.
