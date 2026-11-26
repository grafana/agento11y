# Grafana Agent Observability Python Framework Module: Pydantic AI

`agento11y-pydantic-ai` provides an `AbstractCapability` implementation that maps Pydantic AI lifecycle hooks into agento11y generation recorder lifecycles.

## Installation

```bash
pip install agento11y agento11y-pydantic-ai
pip install pydantic-ai
```

## Quickstart

```python
from pydantic_ai import Agent
from agento11y import Client
from agento11y_pydantic_ai import create_agento11y_pydantic_ai_capability

client = Client()
capability = create_agento11y_pydantic_ai_capability(client=client, provider_resolver="auto")

agent = Agent("openai:gpt-4o", capabilities=[capability])
result = agent.run_sync("What is the weather?")
print(result.output)

client.shutdown()
```

## End-to-end example (run + run_stream)

```python
from pydantic_ai import Agent
from agento11y import Client
from agento11y_pydantic_ai import with_agento11y_pydantic_ai_capability

client = Client()
capabilities = with_agento11y_pydantic_ai_capability(
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

A run's conversation is the first of:

1. `conversation_id` / `session_id` from `ctx.deps` or `ctx.metadata`
2. `thread_id` from `ctx.deps` or `ctx.metadata`
3. the run's own conversation id: the `conversation_id` passed to the run, else the one its `message_history` carries, else a new one Pydantic AI makes for the run. Runs that continue one message history therefore share a conversation.
4. fallback `agento11y:framework:pydantic-ai:<run_id>`, only on Pydantic AI releases whose runs have no conversation id, such as 1.86

`ctx.deps` wins over `ctx.metadata`.

## Recorded messages

Each generation records the message history Pydantic AI sent the model and the response it returned, rendered by Pydantic AI itself, on Pydantic AI 1.x and 2.x:

- System prompt parts and instructions are joined into the generation's system prompt, in the order Pydantic AI's models send them.
- Tool calls keep their call id, name, and JSON arguments; tool results keep the call id and name they answer, and failed or retried calls are marked as errors.
- Thinking is recorded as thinking parts. Retry feedback that answers no tool call is recorded as a user message.
- Images, audio, documents, and files are not recorded: the Python SDK has no media part yet.
- Tool executions record the id of the tool call they run.
- A generation's tools are the ones the request offered the model: function tools, then output tools such as `final_result`. A tool that deferred loading withholds from the request is not recorded.

## Recorded settings and usage

- `temperature`, `max_tokens`, `top_p`, and `tool_choice` are recorded from the request's model settings.
- The unified `thinking` model setting, which the `Thinking` capability also sets, is recorded as the generation's thinking flag, and an effort level as `agento11y.gen_ai.request.thinking.level` metadata. Provider-specific thinking settings are not read.
- The provider's response id is recorded as the generation's response id.
- Token usage includes cache read and cache write tokens, and reasoning tokens: on Pydantic AI 2.x whenever the model reports them, on 1.x for OpenAI, xAI, Google, and Gemini models.
- A streamed request is recorded as a stream generation. Its time to first token is recorded for `run_stream()` and for a run with an `event_stream_handler`.

## Metadata and lineage

- Required: `agento11y.framework.run_type`
- Optional: `agento11y.framework.run_id`, `agento11y.framework.parent_run_id`, `agento11y.framework.thread_id`, `agento11y.framework.event_id`, `agento11y.framework.component_name`, `agento11y.framework.retry_attempt`

## Provider resolver

Resolver order: explicit provider option -> callback payload -> model prefix inference -> `custom`.

## Troubleshooting

- To keep a conversation's runs together, pass the same `conversation_id` to each run or continue from `message_history`. On Pydantic AI releases whose runs have no conversation id, provide `conversation_id` via `ctx.deps` or `ctx.metadata`.
- If model aliases are custom, set explicit `provider` on the handler.
- Always call `client.shutdown()` during teardown to flush buffered telemetry.
