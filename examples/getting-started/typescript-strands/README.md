# Getting Started - TypeScript + Strands Agents

Runs a Strands TypeScript agent and records model/tool activity to Sigil Cloud.

## Setup

```bash
cd examples/getting-started/typescript-strands
npm install
cp .env.example .env
```

Configure Sigil, OpenTelemetry, and OpenAI from your Grafana Cloud stack. See the [Grafana Cloud AI Observability getting started docs](https://grafana.com/docs/grafana-cloud/machine-learning/ai-observability/get-started/grafana-cloud/) for where to find each value:

```bash
AGENTO11Y_PROTOCOL=http
AGENTO11Y_AUTH_MODE=basic
AGENTO11Y_ENDPOINT=https://sigil-prod-<region>.grafana.net
AGENTO11Y_AUTH_TENANT_ID=...
AGENTO11Y_AUTH_TOKEN=...
AGENTO11Y_CONVERSATION_ID=agento11y-strands-demo
OTEL_EXPORTER_OTLP_ENDPOINT=https://otlp-gateway-prod-<region>.grafana.net/otlp
OTEL_EXPORTER_OTLP_HEADERS="Authorization=Basic <base64 of OTLP_INSTANCE_ID:AGENTO11Y_AUTH_TOKEN>"
OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
OTEL_METRIC_EXPORT_INTERVAL_MILLIS=1000
OTEL_SERVICE_NAME=agento11y-strands-typescript-example
OPENAI_MODEL=gpt-4o-mini
OPENAI_API_KEY=...
```

The example configures OpenTelemetry tracing and metrics. Generations go to
`AGENTO11Y_ENDPOINT`; SDK metrics go through the OpenTelemetry HTTP exporter.

## Run

```bash
npm start
```

When the agent response prints, followed by `Done`, open Sigil in your Grafana Cloud stack to inspect the recorded generation and tool execution.
