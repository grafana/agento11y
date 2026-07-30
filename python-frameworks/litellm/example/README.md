# LiteLLM Proxy + Agent Observability Example

Runs a LiteLLM proxy with the agento11y callback handler, exporting generations to Grafana Cloud.

## Prerequisites

- A Grafana Cloud stack with Agent Observability enabled
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

## Verify in Agent Observability

Open `https://<your-stack>.grafana.net/a/grafana-agento11y-app/conversations`. Generations appear with:

- `agent_name`: `litellm-proxy-integration-test`
- `agento11y.framework.name`: `litellm`
- `provider`: `openai` (or whichever model you called)

## Configuration

`config.yaml` defines the available models. Add more by following the [LiteLLM model list format](https://docs.litellm.ai/docs/proxy/configs).

## Attributing generations to the calling agent

`agento11y_callback.py` names generations after the proxy only when a request
says nothing about who is calling. Identify the caller in the request body:

```bash
curlie POST http://<proxy-host>:4000/chat/completions \
  model=gpt-4o-mini \
  messages:='[{"role":"user","content":"What is 2+2?"}]' \
  metadata:='{"agent_name":"search-agent"}'
```

or with the header LiteLLM already understands, which needs no client-side
knowledge of agento11y:

```bash
curlie POST http://<proxy-host>:4000/chat/completions \
  x-litellm-agent-id:search-agent \
  model=gpt-4o-mini \
  messages:='[{"role":"user","content":"What is 2+2?"}]'
```

## Naming agents after the calling key

`agento11y_callback_agent_from_key_alias.py` covers callers that send no agent
identity at all: instead of collapsing them into one proxy-wide name, it adds
the virtual key's alias (then the team's) to `agent_name_metadata_keys`, so each
key shows up as its own agent.

Only use it when your keys map one-to-one onto agents. A key alias names a
credential, not an agent, so rotating a key renames the agent and a shared key
merges unrelated callers.

Point `config.yaml` at it:

```yaml
litellm_settings:
  callbacks: agento11y_callback_agent_from_key_alias.agento11y_handler
```

and mount it alongside `config.yaml` in `docker-compose.yaml`:

```yaml
    volumes:
      - ./agento11y_callback_agent_from_key_alias.py:/app/agento11y_callback_agent_from_key_alias.py
```
