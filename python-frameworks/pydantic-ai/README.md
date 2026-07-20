# Grafana AI Observability Python Framework Module: Pydantic AI

`agento11y-pydantic-ai` provides an `AbstractCapability` implementation that maps Pydantic AI lifecycle hooks into Sigil generation recorder lifecycles.

## Installation

```bash
pip install agento11y agento11y-pydantic-ai
pip install pydantic-ai
```

## Quickstart

```python
from pydantic_ai import Agent
from agento11y import Client
from agento11y_pydantic_ai import create_sigil_pydantic_ai_capability

client = Client()
capability = create_sigil_pydantic_ai_capability(client=client, provider_resolver="auto")

agent = Agent("openai:gpt-4o", capabilities=[capability])
result = agent.run_sync("What is the weather?")
print(result.output)

client.shutdown()
```

## End-to-end example (run + run_stream)

```python
from pydantic_ai import Agent
from agento11y import Client
from agento11y_pydantic_ai import with_sigil_pydantic_ai_capability

client = Client()
capabilities = with_sigil_pydantic_ai_capability(
    None,
    client=client,
    provider_resolver="auto",
    agent_name="pydantic-ai-example",
    agent_version="1.0.0",
)

agent = Agent("openai:gpt-4o-mini", capabilities=capabilities)

# Non-stream call -> SYNC generation mode.
result = agent.run_sync("Summarize why retry budgets matter.")
print(result.output)

# Stream call -> STREAM generation mode + TTFT tracking.
async def stream_example() -> None:
    async with agent.run_stream("Give me three short reliability tips.") as stream:
        async for chunk in stream.stream_text():
            print(chunk, end="", flush=True)
        print()

import asyncio
asyncio.run(stream_example())

client.shutdown()
```

## Conversation mapping

Primary mapping is Pydantic AI run identity:

1. `conversation_id` / `session_id` from `ctx.deps` or `ctx.metadata`
2. `thread_id` from `ctx.deps` or `ctx.metadata`
3. fallback `sigil:framework:pydantic-ai:<run_id>`

## Metadata and lineage

- Required: `sigil.framework.run_type`
- Optional: `sigil.framework.run_id`, `sigil.framework.parent_run_id`, `sigil.framework.thread_id`, `sigil.framework.event_id`, `sigil.framework.component_name`, `sigil.framework.retry_attempt`

## Provider resolver

Resolver order: explicit provider option -> callback payload -> model prefix inference -> `custom`.

## Troubleshooting

- Provide stable `conversation_id` via `ctx.deps` or `ctx.metadata` to avoid fragmented conversations.
- If model aliases are custom, set explicit `provider` on the handler.
- Always call `client.shutdown()` during teardown to flush buffered telemetry.
