# Agent Observability Python Framework Module: OpenAI Agents

`agento11y-openai-agents` provides callback handlers that map OpenAI Agents lifecycle events into agento11y generation recorder lifecycles.

See the [getting-started cookbook](../../examples/getting-started/python-openai-agents/) for a two-turn tool-using agent with standard OpenTelemetry and native agento11y variants.

## Installation

```bash
pip install agento11y agento11y-openai-agents
pip install openai-agents
```

## Quickstart

```python
from agento11y import Client
from agento11y_openai_agents import with_agento11y_openai_agents_hooks

client = Client()
run_options = with_agento11y_openai_agents_hooks(None, client=client, provider_resolver="auto")
# Runner.run(agent, input="...", hooks=run_options["hooks"])
```

## Native hooks wiring

```python
from agento11y import Client
from agento11y_openai_agents import with_agento11y_openai_agents_hooks

client = Client()
run_options = with_agento11y_openai_agents_hooks(None, client=client, provider_resolver="auto")
# Runner.run(agent, input="...", hooks=run_options["hooks"])
```

## Conversation mapping

Conversation ID precedence:

1. `conversation_id` / `session_id` / `group_id`
2. `thread_id`
3. deterministic fallback `agento11y:framework:openai-agents:<run_id>`

## Metadata and lineage

Required framework tags:

- `agento11y.framework.name=openai-agents`
- `agento11y.framework.source=handler`
- `agento11y.framework.language=python`

Metadata includes:

- required: `agento11y.framework.run_type`
- optional: `agento11y.framework.run_id`, `agento11y.framework.thread_id`, `agento11y.framework.parent_run_id`, `agento11y.framework.component_name`, `agento11y.framework.retry_attempt`, `agento11y.framework.event_id`

## Provider resolver

Resolver order: explicit provider option -> framework metadata -> model prefix inference -> `custom`.

## Token usage with streamed Chat Completions models

`Runner.run_streamed` generations carry the token usage that OpenAI Agents reports for each model response. The default Responses model always reports it. `OpenAIChatCompletionsModel` only requests usage on a stream (`stream_options={"include_usage": True}`) when its client points at `https://api.openai.com`. With any other OpenAI-compatible `base_url`, the stream has no usage, so the generation records no input or output tokens. Opt in through `ModelSettings`:

```python
from agents import Agent, ModelSettings, OpenAIChatCompletionsModel
from openai import AsyncOpenAI

agent = Agent(
    name="assistant",
    model=OpenAIChatCompletionsModel("my-model", AsyncOpenAI(base_url="https://llm.example.com/v1")),
    model_settings=ModelSettings(include_usage=True),
)
```

The endpoint has to support `stream_options`. If it rejects the field or ignores it, usage stays empty.

## Troubleshooting

- If conversations are fragmented, pass stable `session_id` or `conversation_id` in callback metadata.
- If provider is inferred as `custom`, set `provider="openai"` (or another provider) on handler init.
- Always call `client.shutdown()` during teardown.
