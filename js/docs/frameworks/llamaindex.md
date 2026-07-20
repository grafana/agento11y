# LlamaIndex Handler (`@grafana/agento11y/llamaindex`)

Use `SigilLlamaIndexHandler` to map LlamaIndex workflow/agent callback lifecycles to Sigil generations.

## Install

```bash
pnpm add @grafana/agento11y llamaindex
```

## Quickstart

```ts
import { SigilClient } from '@grafana/agento11y';
import { withSigilLlamaIndexCallbacks } from '@grafana/agento11y/llamaindex';
import { CallbackManager, Settings } from 'llamaindex';

const client = new SigilClient();
const callbackManager = new CallbackManager();
const config = withSigilLlamaIndexCallbacks({ callbackManager }, client, {
  providerResolver: 'auto',
  agentName: 'llamaindex-app',
});

Settings.callbackManager = config.callbackManager;
```

`withSigilLlamaIndexCallbacks(...)` registers Sigil listeners through LlamaIndex's callback-manager API and returns the configured `callbackManager`.
If you already own a manager instance, use `attachSigilLlamaIndexCallbacks(existingManager, client, options)`.

## Streaming snippet

```ts
import { SigilClient } from '@grafana/agento11y';
import { SigilLlamaIndexHandler } from '@grafana/agento11y/llamaindex';

const client = new SigilClient();
const handler = new SigilLlamaIndexHandler(client, { providerResolver: 'auto' });

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
3. fallback: `sigil:framework:llamaindex:<run_id>`

## Metadata and lineage

- `sigil.framework.run_type` is always set.
- Lineage keys are set when present: `run_id`, `thread_id`, `parent_run_id`, `component_name`, `retry_attempt`, `event_id`.

Required tags:

- `sigil.framework.name=llamaindex`
- `sigil.framework.source=handler`
- `sigil.framework.language=javascript`

## Provider resolver

- `gpt-`/`o1`/`o3`/`o4` -> `openai`
- `claude-` -> `anthropic`
- `gemini-` -> `gemini`
- otherwise `custom`

## Troubleshooting

- If lineage metadata is missing, include it in callback metadata payload.
- Keep `captureInputs` and `captureOutputs` enabled for full generation reconstruction.
- Call `await client.flush()` at checkpoints in long-running workers.
