# Sigil Python Framework Module: LlamaIndex

`agento11y-llamaindex` provides callback handlers that map LlamaIndex workflow/agent events into Sigil generation recorder lifecycles.

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

## Conversation mapping

Conversation ID precedence:

1. `conversation_id` / `session_id` / `group_id`
2. `thread_id`
3. fallback `agento11y:framework:llamaindex:<run_id>`

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
