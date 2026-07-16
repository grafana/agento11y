# Getting Started - Python + Microsoft Agent Framework Foundry

Runs a Microsoft Agent Framework agent backed by Microsoft Foundry and records agent, model, and local tool activity to Sigil.

This is the full Agent Framework path:

- `Agent(client=FoundryChatClient(...))`
- `FoundryAgent(...)` for service-managed Prompt/Hosted agents
- Agent Framework middleware for model calls and local tools

## Setup

```bash
cd examples/getting-started/python-foundry-agent-framework
python -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt
cp .env.example .env
az login
```

For an in-process code agent, set:

```bash
FOUNDRY_AGENT_MODE=chat
AZURE_FOUNDRY_PROJECT_ENDPOINT=https://<resource-name>.services.ai.azure.com/api/projects/<project-name>
AZURE_FOUNDRY_MODEL=gpt-5.2
```

For a service-managed Foundry agent, set:

```bash
FOUNDRY_AGENT_MODE=hosted
AZURE_FOUNDRY_PROJECT_ENDPOINT=https://<resource-name>.services.ai.azure.com/api/projects/<project-name>
AZURE_FOUNDRY_AGENT_NAME=<agent-name>
AZURE_FOUNDRY_AGENT_VERSION=<optional-version>
```

Configure Sigil and OTel endpoints from your Grafana Cloud stack:

```bash
SIGIL_PROTOCOL=http
SIGIL_ENDPOINT=https://sigil-prod-<region>.grafana.net
SIGIL_AUTH_MODE=basic
SIGIL_AUTH_TENANT_ID=<Grafana Cloud instance ID>
SIGIL_AUTH_TOKEN=<access policy token with sigil:write>
OTEL_EXPORTER_OTLP_ENDPOINT=https://<your-otlp-gateway-url>
OTEL_EXPORTER_OTLP_HEADERS="Authorization=Basic <base64 of OTLP_INSTANCE_ID:SIGIL_AUTH_TOKEN>"
```

## Run

```bash
python main.py
```

The recorded generation should have `model.provider=azure_foundry` and `sigil.framework.name=agent-framework-foundry`.
