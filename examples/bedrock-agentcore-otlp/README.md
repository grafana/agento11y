# Amazon Bedrock AgentCore + OpenTelemetry-native Agent Observability

This example runs a LangChain service-operations agent in Amazon Bedrock
AgentCore Runtime and sends it to Grafana Cloud Agent Observability with
OpenTelemetry only. It doesn't use the Agent Observability SDK.

It is the OpenTelemetry-native counterpart of
[`bedrock-agentcore`](../bedrock-agentcore/), which uses the SDK. The agent, tools,
synthetic data, and metrics are the same, so you can run both and compare.

The agent defaults to Claude through Amazon Bedrock and uses synthetic,
company-neutral inventory, order, support-case, and return data.
`AGENT_MODEL_PROVIDER` also sets the recorded provider: `bedrock` for Amazon
Bedrock or `anthropic` for direct Anthropic calls.

## How it differs from the SDK example

| | `bedrock-agentcore` | `bedrock-agentcore-otlp` (this example) |
| --- | --- | --- |
| Generations | `agento11y-langchain` callbacks and the SDK client | Spans from `otel_genai.py` with `gen_ai.*` attributes, sent over OTLP |
| Dependencies | `agento11y`, `agento11y-langchain`, OpenTelemetry | OpenTelemetry only |
| Credentials | An SDK endpoint and token, plus an OTLP token | One OTLP endpoint and token |
| Opt-in | None | `agento11y.record=true` on each chat span |
| Tool calls | Recorded by the SDK callbacks | Recorded as `execute_tool` spans in the trace |
| Content capture | `AGENTO11Y_CONTENT_CAPTURE_MODE` | `GENAI_CONTENT_CAPTURE_MODE`, same three modes |

Use this example if you already export OpenTelemetry traces, or if you can't add
the SDK to your agent. Read
[Instrument agents with OpenTelemetry](https://grafana.com/docs/grafana-cloud/observe-and-act/agent-observability/get-started/opentelemetry/)
for what Agent Observability needs from your spans.

## Project layout

```text
agentcore/
  agentcore.json       # AgentCore project and runtime configuration
  aws-targets.json     # AWS deployment target
app/AgentCoreOtlpDemo/
  main.py              # BedrockAgentCoreApp entrypoint
  agent.py             # LangChain agent and tools
  demo_data.py         # Synthetic demo data
  otel_genai.py        # LangChain callback that records OpenTelemetry GenAI spans
  telemetry.py         # OpenTelemetry bootstrap
  pyproject.toml       # Runtime dependencies
.env.example           # Environment variable template
ONBOARDING.md          # Local setup, deployment, and cleanup
```

## Quick start

Follow [ONBOARDING.md](./ONBOARDING.md) to configure AWS and Grafana Cloud,
run the agent locally, invoke it, verify telemetry, and optionally deploy it.

The local AgentCore workflow is:

```bash
# Terminal 1
agentcore dev --no-traces

# Terminal 2
agentcore invoke --runtime AgentCoreOtlpDemo --local \
  --payload '{"prompt":"Check inventory for ENV-100 and order DEMO-1007."}'
```

`--no-traces` disables the CLI's local collector so the app's own instrumentation
exports traces and metrics directly to Grafana Cloud.

AgentCore loads local credentials from `agentcore/.env.local`, which is ignored
by Git. No secret-manager-specific scripts are required.

## What the instrumentation records

- A `chat <model>` span for each model call, with the provider, model, token
  usage, conversation ID, agent name, and messages. These spans carry
  `agento11y.record=true`, which opts them in to Agent Observability.
- An `execute_tool <name>` span for each tool call.
- An invocation span, `agentcore_demo.agent.invoke`, with the AgentCore session's
  conversation ID. It has no `gen_ai.operation.name`, so it doesn't create a
  duplicate generation.
- `agentcore_demo.agent.requests`
- `agentcore_demo.agent.duration`
- `agentcore_demo.tool.calls`

Account IDs appear only on the invocation span, not as metric labels.

Don't run this example and `bedrock-agentcore` against the same stack at the same
time with the same agent name, or you will see both in one view.
