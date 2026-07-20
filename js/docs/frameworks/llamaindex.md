# LlamaIndex Handler (`@grafana/agento11y/llamaindex`)

Use `Agento11yLlamaIndexHandler` to map LlamaIndex workflow/agent callback lifecycles to Sigil generations.

## Install

```bash
pnpm add @grafana/agento11y llamaindex
```

## Quickstart

```ts
import { Agento11yClient } from '@grafana/agento11y';
import { withAgento11yLlamaIndexCallbacks } from '@grafana/agento11y/llamaindex';
import { CallbackManager, Settings } from 'llamaindex';

const client = new Agento11yClient();
const callbackManager = new CallbackManager();
const config = withAgento11yLlamaIndexCallbacks({ callbackManager }, client, {
  providerResolver: 'auto',
  agentName: 'llamaindex-app',
});

Settings.callbackManager = config.callbackManager;
```

`withAgento11yLlamaIndexCallbacks(...)` registers Sigil listeners through LlamaIndex's callback-manager API and returns the configured `callbackManager`.
If you already own a manager instance, use `attachAgento11yLlamaIndexCallbacks(existingManager, client, options)`.

## Streaming snippet

```ts
import { Agento11yClient } from '@grafana/agento11y';
import { Agento11yLlamaIndexHandler } from '@grafana/agento11y/llamaindex';

const client = new Agento11yClient();
const handler = new Agento11yLlamaIndexHandler(client, { providerResolver: 'auto' });

await handler.handleLLMStart(
  { kwargs: { model: 'claude-sonnet-4-5' } },
  ['stream workflow update'],
  'run-1',
  undefined,
  { invocation_params: { model: 'claude-sonnet-4-5', streaming: true, session_id: 'workflow-42' } }
);
await handler.handleLLMNewToken('partial ', undefined, 'run-1');
await handler.handleLLMNewToken('answer', undefined, 'run-1');
await handler.handleLLMEnd({ llm_output: { model_name: 'claude-sonnet-4-5' } }, 'run-1');
```

## Conversation mapping

Conversation ID precedence:

1. `conversation_id` / `session_id` / `group_id`
2. `thread_id`
3. fallback: `agento11y:framework:llamaindex:<run_id>`

## Metadata and lineage

- `agento11y.framework.run_type` is always set.
- Lineage keys are set when present: `run_id`, `thread_id`, `parent_run_id`, `component_name`, `retry_attempt`, `event_id`.

Required tags:

- `agento11y.framework.name=llamaindex`
- `agento11y.framework.source=handler`
- `agento11y.framework.language=javascript`

## Provider resolver

- `gpt-`/`o1`/`o3`/`o4` -> `openai`
- `claude-` -> `anthropic`
- `gemini-` -> `gemini`
- otherwise `custom`

## Troubleshooting

- If lineage metadata is missing, include it in callback metadata payload.
- Keep `captureInputs` and `captureOutputs` enabled for full generation reconstruction.
- Call `await client.flush()` at checkpoints in long-running workers.
