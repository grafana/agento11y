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
from agento11y_llamaindex import with_sigil_llamaindex_callbacks

client = Client()
config = with_sigil_llamaindex_callbacks(None, client=client, provider_resolver="auto")
# query_engine = index.as_query_engine(callback_manager=config["callback_manager"])
```

## Native callback manager wiring

```python
from agento11y import Client
from agento11y_llamaindex import with_sigil_llamaindex_callbacks

client = Client()
config = with_sigil_llamaindex_callbacks(None, client=client, provider_resolver="auto")
# query_engine = index.as_query_engine(callback_manager=config["callback_manager"])
```

## Conversation mapping

Conversation ID precedence:

1. `conversation_id` / `session_id` / `group_id`
2. `thread_id`
3. fallback `sigil:framework:llamaindex:<run_id>`

## Metadata and lineage

- Required: `sigil.framework.run_type`
- Optional lineage: `sigil.framework.run_id`, `sigil.framework.thread_id`, `sigil.framework.parent_run_id`, `sigil.framework.component_name`, `sigil.framework.retry_attempt`, `sigil.framework.event_id`

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
