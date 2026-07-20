# Sigil Python Framework Module: OpenAI Agents

`agento11y-openai-agents` provides callback handlers that map OpenAI Agents lifecycle events into Sigil generation recorder lifecycles.

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

## Troubleshooting

- If conversations are fragmented, pass stable `session_id` or `conversation_id` in callback metadata.
- If provider is inferred as `custom`, set `provider="openai"` (or another provider) on handler init.
- Always call `client.shutdown()` during teardown.
