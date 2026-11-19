# Agent Observability Python Framework Module: LlamaIndex

`agento11y-llamaindex` provides callback handlers that map LlamaIndex workflow/agent events into agento11y generation recorder lifecycles.

## Installation

```bash
pip install agento11y agento11y-llamaindex
pip install llama-index
```

## Quickstart

```python
from agento11y import Client
from agento11y_llamaindex import with_agento11y_llamaindex_callbacks

client = Client()
config = with_agento11y_llamaindex_callbacks(None, client=client, provider_resolver="auto")
# query_engine = index.as_query_engine(callback_manager=config["callback_manager"])
```

## Native callback manager wiring

```python
from agento11y import Client
from agento11y_llamaindex import with_agento11y_llamaindex_callbacks

client = Client()
config = with_agento11y_llamaindex_callbacks(None, client=client, provider_resolver="auto")
# query_engine = index.as_query_engine(callback_manager=config["callback_manager"])
```

## Workflow agents

Set the callback manager on the LLM that a `FunctionAgent` (or `AgentWorkflow`) uses:

```python
from llama_index.core.agent.workflow import FunctionAgent
from llama_index.llms.openai import OpenAI

llm = OpenAI(
    model="gpt-4o-mini",
    # OpenAI only reports usage on a stream when asked to.
    additional_kwargs={"stream_options": {"include_usage": True}},
    callback_manager=config["callback_manager"],
)
agent = FunctionAgent(tools=tools, llm=llm)
```

Each generation records the provider's token usage (including Anthropic cache reads and writes), the stop reason, and its tool calls with their ids. Streamed calls (`stream_chat`, and every `FunctionAgent` turn) record `STREAM` mode and time to first token. Each tool the agent runs gets an `execute_tool` span with `gen_ai.tool.call.id`. The handler reads streamed chunks and tool steps from the LlamaIndex instrumentation dispatcher, which it subscribes to when it is created.

## Conversation mapping

Conversation ID precedence:

1. `conversation_id` / `session_id` / `group_id`
2. `thread_id`
3. the handler's `conversation_id` option
4. fallback `agento11y:framework:llamaindex:<run_id>`

Pass `conversation_id` (and optionally `conversation_title`) when LlamaIndex events carry no session key, so every generation from one handler lands in one conversation:

```python
config = with_agento11y_llamaindex_callbacks(
    None, client=client, agent_name="support-agent", conversation_id="session-42"
)
```

## Metadata and lineage

- Required: `agento11y.framework.run_type`
- Optional lineage: `agento11y.framework.run_id`, `agento11y.framework.thread_id`, `agento11y.framework.parent_run_id`, `agento11y.framework.component_name`, `agento11y.framework.retry_attempt`, `agento11y.framework.event_id`

## Provider resolver

Model prefix inference:

- `gpt-`/`o1`/`o3`/`o4` -> `openai`
- `claude-` -> `anthropic`
- `gemini-` -> `gemini`
- fallback `custom`

## Troubleshooting

- Reuse stable workflow/session IDs to keep conversation grouping stable.
- Keep `capture_inputs`/`capture_outputs` enabled while validating mappings.
- Call `client.flush()` at checkpoints and `client.shutdown()` on exit.
