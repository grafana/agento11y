# AgentCore example onboarding

This guide runs the example locally with Amazon Bedrock AgentCore Runtime dev
mode and exports telemetry to Grafana Cloud Agent Observability.

## Prerequisites

- Node.js 20 or newer
- Python 3.10 or newer
- [`uv`](https://docs.astral.sh/uv/getting-started/installation/)
- An AWS account and AWS CLI credentials with access to Amazon Bedrock
- Access to the configured Claude model in the selected AWS Region
- Grafana Cloud Agent Observability credentials and a Grafana Cloud OTLP
  endpoint

Install the AgentCore CLI:

```bash
npm install -g @aws/agentcore
agentcore --help
```

Verify the AWS identity and region that local Bedrock calls will use:

```bash
aws sts get-caller-identity
aws configure get region
```

## 1. Configure the environment

From this example directory:

```bash
cp .env.example agentcore/.env.local
```

Edit `agentcore/.env.local`. The default model path uses Amazon Bedrock:

```bash
AGENT_MODEL_PROVIDER=bedrock
BEDROCK_MODEL_ID=us.anthropic.claude-sonnet-4-5-20250929-v1:0
BEDROCK_REGION=us-west-2
```

Set Agent Observability generation export:

```bash
AGENTO11Y_PROTOCOL=http
AGENTO11Y_ENDPOINT=https://agento11y-prod-<region>.grafana.net
AGENTO11Y_AUTH_MODE=basic
AGENTO11Y_AUTH_TENANT_ID=...
AGENTO11Y_AUTH_TOKEN=...
AGENTO11Y_CONTENT_CAPTURE_MODE=no_tool_content
```

Set Grafana Cloud OTLP traces and metrics:

```bash
OTEL_EXPORTER_OTLP_ENDPOINT=https://otlp-gateway-prod-<region>.grafana.net/otlp
OTEL_EXPORTER_OTLP_HEADERS="Authorization=Basic <base64(OTLP_INSTANCE_ID:AGENTO11Y_AUTH_TOKEN)>"
OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
OTEL_EXPORTER_OTLP_INSECURE=false
```

Keep this enabled so AgentCore's default ADOT CloudWatch configuration does not
replace the explicit Grafana Cloud OTLP settings:

```bash
DISABLE_ADOT_OBSERVABILITY=true
```

The `.env.local` file is ignored by Git and is only used for local development.

## 2. Validate the project

```bash
agentcore validate
```

Before deployment, replace the placeholder AWS account in
`agentcore/aws-targets.json` and make sure its region matches
`BEDROCK_REGION`.

## 3. Start the local AgentCore runtime

```bash
agentcore dev --logs
```

AgentCore creates a virtual environment, installs the dependencies from the
application's `pyproject.toml`, and starts the runtime on port 8080. If that
port is occupied, it prints the replacement port. Leave this terminal running.

## 4. Invoke the local agent

In a second terminal, from this example directory:

```bash
agentcore dev "Check inventory for ENV-100 and order DEMO-1007."
```

If the server selected another port, pass it explicitly:

```bash
agentcore dev -p 8081 "Check inventory for ENV-100 and order DEMO-1007."
```

Other useful prompts:

```text
Do we have Air Quality Sensor units in the Northeast warehouse?
Check order DEMO-1007 and tell me if anything needs escalation.
Create a return for 2 ENV-100 units damaged in transit for acct-demo-1042.
Which products are low stock, and what should support communicate?
```

## 5. Verify Agent Observability

In Grafana Cloud Agent Observability, look for:

- Agent name `agentcore-operations-demo`
- LangChain model generations
- Tool execution spans for inventory, order status, support cases, and returns
- Custom metrics `agentcore_demo.agent.requests`,
  `agentcore_demo.agent.duration`, and `agentcore_demo.tool.calls`

AgentCore dev mode can also run its own local trace collection for the
inspector. If local dev telemetry differs from a deployment, first verify the
agent response, then validate export from the hosted runtime.

## 6. Deploy and invoke

Deploy only after the local behavior and telemetry work:

`agentcore/.env.local` is not copied into the hosted runtime. Before deployment,
configure the runtime with the same non-secret settings and arrange for your
approved AWS secret-delivery mechanism to expose `AGENTO11Y_AUTH_TOKEN` and
`OTEL_EXPORTER_OTLP_HEADERS` to the process. AgentCore supports non-secret
runtime settings through the runtime's `envVars` configuration. Never put
tokens or authorization headers in the tracked `agentcore.json`.

```bash
agentcore deploy --dry-run
agentcore deploy
agentcore invoke --session-id agentcore-demo-session-0000000000000001 \
  "Check inventory for ENV-100 and order DEMO-1007."
```

If the AWS account has not been bootstrapped for CDK, an AWS administrator may
need to perform that one-time setup before deployment succeeds.

For current AWS requirements and troubleshooting, see:

- [AgentCore CLI getting started](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/runtime-get-started-cli.html)
- [AgentCore Runtime permissions](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/runtime-permissions.html)

## 7. Clean up

AgentCore resources can incur AWS charges. To remove the deployed resources:

```bash
agentcore remove all
agentcore deploy
```
