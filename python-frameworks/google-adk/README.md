# Sigil Python Framework Module: Google ADK

`agento11y-google-adk` provides callback handlers that map Google ADK invocation/session events into Sigil generation recorder lifecycles.

## Installation

```bash
pip install agento11y agento11y-google-adk
pip install google-adk
```

## Quickstart

```python
from agento11y import Client
from agento11y_google_adk import with_sigil_google_adk_plugins

client = Client()
runner_config = with_sigil_google_adk_plugins(None, client=client, provider_resolver="auto")
# Runner(..., **runner_config)
```

## Callback-field wiring

```python
from agento11y import Client
from agento11y_google_adk import with_sigil_google_adk_callbacks

client = Client()
agent_config = with_sigil_google_adk_callbacks(None, client=client, provider_resolver="auto")
# LlmAgent(..., **agent_config)
```

## Conversation mapping

Primary mapping is ADK session identity:

1. `conversation_id` / `session_id` / `group_id`
2. `thread_id`
3. fallback `sigil:framework:google-adk:<run_id>`

## Metadata and lineage

- Required: `sigil.framework.run_type`
- Optional: `sigil.framework.run_id`, `sigil.framework.parent_run_id`, `sigil.framework.thread_id`, `sigil.framework.event_id`, `sigil.framework.component_name`, `sigil.framework.retry_attempt`

## Provider resolver

Resolver order: explicit provider option -> callback payload -> model prefix inference -> `custom`.

## Troubleshooting

- Provide stable ADK `session_id` to avoid fragmented conversations.
- If model aliases are custom, set explicit `provider` on the handler.
- Always call `client.shutdown()` during teardown.
