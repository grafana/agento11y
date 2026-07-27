# Amazon Bedrock AgentCore + Agent Observability

This example runs a LangChain service-operations agent in Amazon Bedrock
AgentCore Runtime and exports model generations, tool executions, traces, and
metrics to Grafana Cloud Agent Observability.

The agent defaults to Claude through Amazon Bedrock and uses synthetic,
company-neutral inventory, order, support-case, and return data.

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
agentcore dev --logs
agentcore dev "Check inventory for ENV-100 and order DEMO-1007."
```

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
