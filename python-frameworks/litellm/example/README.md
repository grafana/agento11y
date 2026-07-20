# LiteLLM Proxy + Sigil Example

Runs a LiteLLM proxy with the Sigil callback handler, exporting generations to Grafana Cloud.

## Prerequisites

- A Grafana Cloud stack with Sigil enabled
- A Grafana Cloud API token (`glc_...`)
- At least one LLM API key (`OPENAI_API_KEY` or `ANTHROPIC_API_KEY`)

## Start the proxy

```bash
cd sdks/python-frameworks/litellm/example
AGENTO11Y_ENDPOINT=https://your-agento11y.grafana.net \
  AGENTO11Y_AUTH_TENANT_ID=your-tenant \
  AGENTO11Y_AUTH_TOKEN=glc_... \
  OPENAI_API_KEY=sk-... \
  docker compose up --build
```

The proxy starts on the published Docker Compose port `4000`.

## Make a request

```bash
curlie POST http://<proxy-host>:4000/chat/completions \
  model=gpt-4o-mini \
  messages:='[{"role":"user","content":"What is 2+2?"}]'
```

Or with streaming:

```bash
curlie POST http://<proxy-host>:4000/chat/completions \
  model=gpt-4o-mini \
  messages:='[{"role":"user","content":"Give me three reliability tips."}]' \
  stream:=true
```

## Verify in Sigil

Open `https://<your-stack>.grafana.net/a/grafana-sigil-app/conversations`. Generations appear with:

- `agent_name`: `litellm-proxy-integration-test`
- `agento11y.framework.name`: `litellm`
- `provider`: `openai` (or whichever model you called)

## Configuration

`config.yaml` defines the available models. Add more by following the [LiteLLM model list format](https://docs.litellm.ai/docs/proxy/configs).
