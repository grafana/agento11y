# Getting Started - Python + Microsoft Foundry

Runs a Microsoft Foundry Responses API call through `azure-ai-projects` and records it to Sigil.

## Setup

```bash
cd examples/getting-started/python-foundry
python -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt
cp .env.example .env
```

Configure a Foundry project endpoint and sign in with Azure CLI:

```bash
az login
AZURE_FOUNDRY_PROJECT_ENDPOINT=https://<resource-name>.services.ai.azure.com/api/projects/<project-name>
AZURE_FOUNDRY_MODEL=gpt-5.2
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

When the response prints, followed by `Done`, open Sigil and look for the conversation ID from `SIGIL_CONVERSATION_ID`. The generation provider is recorded as `azure_foundry`, with the Foundry project endpoint in generation metadata.
