# LiteLLM proxy example

This example runs a LiteLLM proxy that exports every generation to Grafana Cloud Agent Observability and enforces Agent Observability guards on the request path.
It starts with one command, and it works without a provider key.

The example installs the SDK from this repository, so it exercises the working tree.
For your own deployment, replace the two `COPY` lines in the `Dockerfile` with a `pip install agento11y agento11y-litellm`.

| File | Contents |
| --- | --- |
| `agento11y_callback.py` | The callback the proxy loads: one SDK client, an export handler, and a guardrail |
| `agento11y_callback_agent_from_key_alias.py` | A drop-in replacement that names unidentified callers after their virtual key |
| `config.yaml` | The model list, and the callbacks by dotted path |
| `docker-compose.yaml` | Builds the image from this checkout and mounts the two callback files |

## Before you begin

You need Docker Compose, and the endpoint, instance ID, and token for a Grafana Cloud stack.
To enable the product and collect those three values, refer to [Set up Agent Observability](https://grafana.com/docs/grafana-cloud/observe-and-act/agent-observability/get-started/grafana-cloud/).

A provider key is optional.
The `mock` model in `config.yaml` answers from LiteLLM itself, so you can run the example with Grafana Cloud credentials alone.

## Start the proxy

To start the proxy, run Docker Compose with your credentials:

```bash
cd python-frameworks/litellm/example
AGENTO11Y_ENDPOINT=https://your-agento11y.grafana.net \
  AGENTO11Y_AUTH_TENANT_ID=your-tenant \
  AGENTO11Y_AUTH_TOKEN=glc_your_token \
  OPENAI_API_KEY=sk-your-key \
  docker compose up --build
```

The proxy listens on port `4000`.
The first build takes a couple of minutes, and the proxy needs about a minute more to load its callbacks.

Wait for the readiness check to answer before you send a request:

```bash
curl --retry 30 --retry-delay 2 --retry-all-errors http://localhost:4000/health/liveliness
```

`Dockerfile.dockerignore` keeps the build context to the two directories the image copies, out of a repository that's otherwise gigabytes.

## Send a request

The `mock` model needs no provider key, so it works on a first run with Grafana Cloud credentials alone:

```bash
curl http://localhost:4000/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model": "mock", "messages": [{"role": "user", "content": "What is 2+2?"}]}'
```

Everything except the provider call is real.
The generation is exported, and guards evaluate both the request and the response.

With a provider key, call `gpt-4o-mini` or `claude-haiku` instead:

```bash
curl http://localhost:4000/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model": "gpt-4o-mini", "messages": [{"role": "user", "content": "What is 2+2?"}]}'
```

A streamed call is exported as one generation carrying the time of the first token:

```bash
curl http://localhost:4000/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model": "gpt-4o-mini", "messages": [{"role": "user", "content": "Three tips."}], "stream": true}'
```

The other recorded routes work the same way, and each exports one generation:

```bash
curl http://localhost:4000/v1/messages \
  -H 'Content-Type: application/json' \
  -d '{"model": "claude-haiku", "max_tokens": 64, "system": "You are terse.", "messages": [{"role": "user", "content": "What is 2+2?"}]}'

curl http://localhost:4000/v1/responses \
  -H 'Content-Type: application/json' \
  -d '{"model": "gpt-4o-mini", "instructions": "You are terse.", "input": "What is 2+2?"}'
```

## Verify the generations arrive

Open Agent Observability in your stack and search for the agent `litellm-proxy`.
Each generation carries the tag `agento11y.framework.name=litellm` and the provider that served the call.
For the search filters and what each panel shows, refer to [Browse and debug conversations](https://grafana.com/docs/grafana-cloud/observe-and-act/agent-observability/guides/conversations/).

To check from a terminal, use the [gcx repository](https://github.com/grafana/gcx):

```bash
gcx agento11y agents list -o json
gcx agento11y conversations search --filters 'agent = "litellm-proxy"' -o json
gcx agento11y conversations get <CONVERSATION_ID> -o json
```

## Attribute generations to the calling agent

`agento11y_callback.py` names generations after the proxy only when a request says nothing about who is calling.
To name the caller, pass metadata in the request body:

```bash
curl http://localhost:4000/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model": "mock", "messages": [{"role": "user", "content": "What is 2+2?"}], "metadata": {"agent_name": "search-agent", "conversation_id": "conv-abc-123"}}'
```

Or send the header LiteLLM already understands, which needs no client-side knowledge of Agent Observability:

```bash
curl http://localhost:4000/v1/chat/completions \
  -H 'x-litellm-agent-id: search-agent' \
  -H 'Content-Type: application/json' \
  -d '{"model": "mock", "messages": [{"role": "user", "content": "What is 2+2?"}]}'
```

Both work on every recorded route, `/v1/messages` and `/v1/responses` included.

### Name agents after the calling key

`agento11y_callback_agent_from_key_alias.py` covers callers that send no agent identity at all.
It adds the virtual key's alias, then the team's, to the keys the handler consults.
Each key then shows up as its own agent, instead of collapsing into one proxy-wide name.

Only use it when your keys map one-to-one onto agents.
A key alias names a credential, not an agent, so rotating a key renames the agent and a shared key merges unrelated callers.

To use it, point `config.yaml` at it, both lines together, so the proxy keeps running one SDK client:

```yaml
litellm_settings:
  callbacks:
    - agento11y_callback_agent_from_key_alias.agento11y_handler
    - agento11y_callback_agent_from_key_alias.agento11y_guards
```

Virtual keys live in the proxy database, so this file changes nothing on its own.
It takes effect once the proxy runs with `DATABASE_URL` and `LITELLM_MASTER_KEY` set, and callers authenticate with a key that has an alias.

This Compose file sets neither, so the proxy runs unauthenticated.
That's fine for a local example and unsafe anywhere else.

To try the variant, add a Postgres service and those two variables.
Then create a key with [LiteLLM virtual keys](https://docs.litellm.ai/docs/proxy/virtual_keys), and send it as the bearer token.

## Enforce guards

`config.yaml` lists `agento11y_callback.agento11y_guards` next to the handler, which evaluates your guards on both phases:

- Preflight, before the request reaches the provider. It can save the spend.
- Postflight, after the provider answers, before a non-streamed response reaches the caller. It can't save the spend.

Guards live in Agent Observability, not in this config.
To create one, refer to [Set up guards](https://grafana.com/docs/grafana-cloud/observe-and-act/agent-observability/guides/guards/).

A request has three possible outcomes:

- Allow, when no rule matches or every rule passes. The request goes through and the generation is exported as usual.
- Deny. The proxy answers `400` with `blocked by guardrail agento11y: <REASON> (rule <RULE_ID>)`.
- Evaluator unreachable. `HooksConfig.fail_open` defaults to `True`, so the request is allowed and the proxy logs `agento11y: guardrail 'agento11y' allowing request (fail_open)`. Pass `fail_open=False` to fail closed instead.

To watch the fail-open path, point `AGENTO11Y_ENDPOINT` at an unreachable host and read the proxy logs while you send a request.

Enforcement only happens through the proxy.
A direct `litellm.completion()` call in your own process is never guarded, because LiteLLM runs call hooks only in the proxy.

For the routes each phase covers and what a deny does to a streamed response, refer to the [LiteLLM guard reference](../docs/guards.md).

## Send OpenTelemetry traces and metrics

Generation export and OpenTelemetry (OTel) are separate channels.
This example configures generation export only, so the performance charts, which read metrics, stay empty.

To fill them, pass the two OpenTelemetry Protocol (OTLP) variables as well:

```bash
OTEL_EXPORTER_OTLP_ENDPOINT=https://otlp-gateway-<REGION>.grafana.net/otlp \
OTEL_EXPORTER_OTLP_HEADERS="Authorization=Basic <BASE64_OF_INSTANCE_ID_AND_TOKEN>" \
  docker compose up --build
```

`docker-compose.yaml` passes both through, and the same token covers both channels.
For the values to put in them, and for sending through Alloy instead, refer to [Set up traces and metrics](https://grafana.com/docs/grafana-cloud/observe-and-act/agent-observability/get-started/grafana-cloud/#set-up-traces-and-metrics).

## Add a model

`config.yaml` defines the available models.
To add more, follow the [LiteLLM model list format](https://docs.litellm.ai/docs/proxy/configs).
