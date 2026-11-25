# Amazon Bedrock AgentCore + Agent Observability

This example runs a LangChain service-operations agent in Amazon Bedrock
AgentCore Runtime and exports model generations, tool executions, traces, and
metrics to Grafana Cloud Agent Observability.

The agent defaults to Claude through Amazon Bedrock and uses synthetic,
company-neutral inventory, order, support-case, and return data.
`AGENT_MODEL_PROVIDER` also sets the recorded generation provider: `bedrock`
for Amazon Bedrock or `anthropic` for direct Anthropic calls.

## Project layout

```text
agentcore/
  agentcore.json       # AgentCore project and runtime configuration
  aws-targets.json     # AWS deployment target
app/AgentCoreObservabilityDemo/
  main.py              # BedrockAgentCoreApp entrypoint
  agent.py             # LangChain agent and tools
  demo_data.py         # Synthetic demo data
  agento11y_client.py  # Agent Observability SDK bootstrap
  content_capture.py   # Tool-specific content capture policy
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
agentcore invoke --runtime AgentCoreObsDemo --local \
  --payload '{"prompt":"Check inventory for ENV-100 and order DEMO-1007."}'
```

`--no-traces` disables the CLI's local collector so the app's own instrumentation
exports traces and metrics directly to Grafana Cloud.

AgentCore loads local credentials from `agentcore/.env.local`, which is ignored
by Git. No secret-manager-specific scripts are required.

## What the instrumentation records

- LangChain model generations and tool executions through
  `agento11y-langchain`
- An invocation span with the AgentCore session's conversation ID
- `agentcore_demo.agent.requests`
- `agentcore_demo.agent.duration`
- `agentcore_demo.tool.calls`

Account IDs appear only on spans and generation metadata, not as metric labels.

The default `AGENTO11Y_CONTENT_CAPTURE_MODE=no_tool_content` preserves prompts
and replies, plus tool names and call IDs. This example overrides the adapter's
legacy tool-content opt-in and removes tool arguments and results from both
tool spans and generation messages. Tool calls therefore have no JSON payload
to expand in the conversation view, matching the OTLP-only example.

Set the mode to `full` to inspect tool JSON, or `metadata_only` to omit prompts
and replies as well.

## Offline verification

From this example directory, run the capture-policy tests without AWS or
Grafana Cloud credentials:

```bash
uv run --project app/AgentCoreObservabilityDemo python -m unittest discover -s tests -v
```
