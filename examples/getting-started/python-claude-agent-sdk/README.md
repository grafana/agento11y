# Getting Started - Python + Claude Agent SDK

Runs a Claude Agent SDK query and records the session to Grafana Cloud.

## Setup

```bash
cd examples/getting-started/python-claude-agent-sdk
python -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt
cp .env.example .env
```

Configure agento11y and OTel endpoints from your Grafana Cloud stack. See the [Grafana Cloud Agent Observability getting started docs](https://grafana.com/docs/grafana-cloud/machine-learning/agent-observability/get-started/grafana-cloud/) for where to find each value.

You also need Claude Code authentication available to the Claude Agent SDK. Use the same auth setup you use for a normal Claude Code or Claude Agent SDK run.

## Run

```bash
python main.py
```

When the response prints, followed by `Done`, open Grafana Cloud Agent Observability to inspect the recorded generation. The example configures both agento11y SDK traces and metrics; `AGENTO11Y_AGENT_VERSION` defaults to `dev` so version-scoped analytics can join generation exports to Prometheus metrics. If `CLAUDE_CODE_ENABLE_TELEMETRY=1` is set, the Claude Code CLI also exports its native spans, metrics, and log events to the configured OTLP endpoint.
