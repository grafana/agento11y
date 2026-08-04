# Agent Observability for LiteLLM

The `agento11y-litellm` package sends what your [LiteLLM](https://docs.litellm.ai/) calls do to Grafana Cloud Agent Observability.
Register one callback and every completion is exported as a generation, with its messages, tokens, cost, tool calls, and errors.
Add a second callback and Agent Observability guards can block a request before it reaches the provider.

It works in your own process and inside the LiteLLM proxy.
Guards need the proxy, because LiteLLM only runs call hooks there.

## Before you begin

You need Python 3.10 or later, and an endpoint, instance ID, and token.
To enable the product and collect those three values, refer to [Set up Agent Observability](https://grafana.com/docs/grafana-cloud/observe-and-act/agent-observability/get-started/grafana-cloud/).

Install the packages:

```bash
pip install agento11y agento11y-litellm litellm
```

## Export generations from your app

The client reads its connection details from the environment, so `Client()` takes no arguments:

```bash
export AGENTO11Y_ENDPOINT=https://your-agento11y.grafana.net
export AGENTO11Y_PROTOCOL=http
export AGENTO11Y_AUTH_MODE=basic
export AGENTO11Y_AUTH_TENANT_ID=your-instance-id
export AGENTO11Y_AUTH_TOKEN=glc_your_token
```

Grafana Cloud needs both `AGENTO11Y_PROTOCOL=http` and `AGENTO11Y_AUTH_MODE=basic`.
The SDK otherwise defaults to gRPC with no authentication, and a Cloud endpoint then answers `401` with no other signal.
For content capture, batching, and the rest of the settings, refer to [Configure the Agent Observability SDK](https://grafana.com/docs/grafana-cloud/observe-and-act/agent-observability/configure/sdk/).

To export generations, follow these steps:

1. Create a client and a handler.

   ```python
   import litellm
   from agento11y import Client
   from agento11y_litellm import Agento11yLiteLLMLogger

   client = Client()
   litellm.callbacks = [Agento11yLiteLLMLogger(client=client, agent_name="my-agent")]
   ```

1. Call LiteLLM as you normally do.

   ```python
   response = litellm.completion(
       model="openai/gpt-4o-mini",
       messages=[{"role": "user", "content": "Hello!"}],
   )
   print(response.choices[0].message.content)
   ```

1. Flush buffered telemetry before your process exits.

   ```python
   client.shutdown()
   ```

Streaming works the same way.
A streamed call is exported as one generation in `STREAM` mode, carrying the time of the first token:

```python
response = litellm.completion(
    model="openai/gpt-4o-mini",
    messages=[{"role": "user", "content": "Give me three reliability tips."}],
    stream=True,
)
for chunk in response:
    content = chunk.choices[0].delta.content
    if content:
        print(content, end="", flush=True)
```

Your generations appear under the agent name you passed.
To find and read them, refer to [Browse and debug conversations](https://grafana.com/docs/grafana-cloud/observe-and-act/agent-observability/guides/conversations/).
For every option the handler takes, refer to the [LiteLLM adapter reference](docs/reference.md).

## Export generations from the LiteLLM proxy

The proxy loads callbacks by dotted path from a Python file next to `config.yaml`.
Put a `Client` and a handler in that file, then name the handler in `config.yaml`:

```yaml
litellm_settings:
  callbacks:
    - agento11y_callback.agento11y_handler
```

For the callback file, the Dockerfile, and the `docker run` command, refer to [Deploy in the LiteLLM proxy](docs/reference.md#deploy-in-the-litellm-proxy).
For a proxy you can start in one command, refer to the [LiteLLM proxy example](example/README.md).

## Name the calling agent

One proxy usually serves several agents.
The handler reads the agent name from each request, so each caller gets its own agent in Agent Observability.
When a request names no agent, the handler falls back to the `agent_name` you configured.

A client that knows about Agent Observability can pass metadata:

```python
response = litellm.completion(
    model="openai/gpt-4o-mini",
    messages=[{"role": "user", "content": "Continue our chat."}],
    metadata={
        "agent_name": "search-agent",
        "agent_version": "v2",
        "conversation_id": "conv-abc-123",
    },
)
```

A client that doesn't can send the header LiteLLM already understands:

```bash
curl http://localhost:4000/v1/chat/completions \
  -H 'x-litellm-agent-id: search-agent' \
  -H 'Content-Type: application/json' \
  -d '{"model": "gpt-4o-mini", "messages": [{"role": "user", "content": "Hello!"}]}'
```

Both work on every recorded route.
To name callers after their virtual key instead, or to learn which metadata containers the adapter reads, refer to [Agent identity](docs/reference.md#agent-identity).

## Enforce guards on proxy requests

A guard is a rule that runs on the request path and can fail the request.
`Agento11yLiteLLMGuardrail` evaluates your Agent Observability guards inside the proxy: preflight before the provider is called, postflight against the provider's response.

Guards live in Agent Observability, not in `config.yaml`.
To create one, refer to [Set up guards](https://grafana.com/docs/grafana-cloud/observe-and-act/agent-observability/guides/guards/).

To enforce them, follow these steps:

1. Enable hooks on the client and build the guardrail next to the handler.

   ```python
   from agento11y import Client, ClientConfig, HooksConfig
   from agento11y_litellm import Agento11yLiteLLMGuardrail, Agento11yLiteLLMLogger

   client = Client(
       ClientConfig(
           hooks=HooksConfig(
               enabled=True,
               timeout_seconds=3.0,
               # Drop "postflight" to evaluate request rules only.
               phases=["preflight", "postflight"],
           )
       )
   )

   agento11y_handler = Agento11yLiteLLMLogger(client=client, agent_name="litellm-proxy")
   agento11y_guards = Agento11yLiteLLMGuardrail(
       client=client,
       agent_name="litellm-proxy",
       default_on=True,
       event_hook=["pre_call", "post_call"],
   )
   ```

1. List both objects in `config.yaml`.

   ```yaml
   litellm_settings:
     callbacks:
       - agento11y_callback.agento11y_handler
       - agento11y_callback.agento11y_guards
   ```

1. Send a request that a rule denies, and confirm the proxy answers `400` with the rule's reason.

LiteLLM runs any `CustomGuardrail` in `litellm.callbacks` on both hook paths, so the guardrail needs no separate `guardrails` block.

Three limits are worth knowing before you rely on guards:

- A postflight deny can't stop a streamed response, because the caller already has it. Use preflight to block a streaming request.
- A redact rule rewrites the request all or nothing. When the guardrail can't apply the whole rewrite, it forwards the original and logs a warning starting with `agento11y: skipping`.
- A deny answers `400` from LiteLLM 1.87.0 on, and `500` before that.

For the routes each phase covers, the deny outcome per delivery, and every guardrail option, refer to the [LiteLLM guard reference](docs/guards.md).

## Learn more

- [LiteLLM adapter reference](docs/reference.md): options, recorded calls, exported fields, proxy deployment
- [LiteLLM guard reference](docs/guards.md): phases, guarded routes, deny outcomes, request rewriting
- [LiteLLM proxy example](example/README.md): a proxy with both callbacks, runnable with no provider key
- [Instrument Python agents](https://grafana.com/docs/grafana-cloud/observe-and-act/agent-observability/get-started/python/): the SDK on its own, without LiteLLM
- [Agent Observability documentation](https://grafana.com/docs/grafana-cloud/observe-and-act/agent-observability/)
