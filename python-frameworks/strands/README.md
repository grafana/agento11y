# Sigil Python Framework Module: Strands Agents

`agento11y-strands` provides a Strands `HookProvider` bridge that maps agent, model, and tool lifecycle events into Sigil generation and tool recording.

## Installation

```bash
pip install agento11y agento11y-strands
pip install strands-agents
```

## Quickstart

```python
from agento11y import Client
from agento11y_strands import with_sigil_strands_hooks
from strands import Agent

client = Client()
agent_config = with_sigil_strands_hooks(
    {"name": "support-agent"},
    client=client,
    provider_resolver="auto",
)

agent = Agent(**agent_config)
agent(
    "Explain what LLM observability is in one sentence.",
    invocation_state={"conversation_id": "demo-strands"},
)

client.shutdown()
```

## Existing Agents

```python
from agento11y import Client
from agento11y_strands import with_sigil_strands_hooks

client = Client()
with_sigil_strands_hooks(agent, client=client, provider_resolver="auto")
```

## Conversation Mapping

Conversation ID precedence:

1. `conversation_id` / `session_id` / `group_id` from Strands `invocation_state`
2. `thread_id` from Strands `invocation_state`
3. deterministic fallback `sigil:framework:strands:<run_id>`

Pass a stable value per user conversation:

```python
agent("Remember my timezone is UTC+1.", invocation_state={"conversation_id": "customer-42"})
agent("What timezone did I give you?", invocation_state={"conversation_id": "customer-42"})
```

## Metadata and Lineage

Required framework tags:

- `sigil.framework.name=strands`
- `sigil.framework.source=hooks`
- `sigil.framework.language=python`

Metadata includes:

- required: `sigil.framework.run_type`
- optional: `sigil.framework.run_id`, `sigil.framework.thread_id`, `sigil.framework.parent_run_id`, `sigil.framework.component_name`, `sigil.framework.event_id`

## Provider Resolver

Resolver order: explicit provider option -> Strands model config metadata -> model prefix inference -> `custom`.

For Bedrock model IDs that do not infer cleanly, pass `provider="anthropic"` or another provider value when creating the hook provider.

## Troubleshooting

- If conversations are fragmented, pass stable `conversation_id` or `session_id` in `invocation_state`.
- If provider is inferred as `custom`, set `provider="openai"` / `provider="anthropic"` / `provider="gemini"` on hook creation.
- Always call `client.shutdown()` during teardown.
