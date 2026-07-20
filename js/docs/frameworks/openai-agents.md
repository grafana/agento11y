# OpenAI Agents Handler (`@grafana/agento11y/openai-agents`)

Use `Agento11yOpenAIAgentsHandler` to map OpenAI Agents lifecycle callbacks to Sigil generations.

## Install

```bash
pnpm add @grafana/agento11y @openai/agents
```

## Quickstart

```ts
import { Agento11yClient } from '@grafana/agento11y';
import { withAgento11yOpenAIAgentsHooks } from '@grafana/agento11y/openai-agents';
import { Runner } from '@openai/agents';

const client = new Agento11yClient();
const runner = new Runner();
const agento11yHooks = withAgento11yOpenAIAgentsHooks(runner, client, {
  providerResolver: 'auto',
  agentName: 'openai-agents-app',
  agentVersion: '1.0.0',
});

// optional cleanup if the runner lifecycle ends
agento11yHooks.detach();
```

`withAgento11yOpenAIAgentsHooks(...)` attaches Sigil listeners directly to OpenAI Agents `RunHooks`/`AgentHooks` emitters (`Runner` or `Agent`).

## Streaming snippet

```ts
import { Agento11yClient } from '@grafana/agento11y';
import { Agento11yOpenAIAgentsHandler } from '@grafana/agento11y/openai-agents';

const client = new Agento11yClient();
const handler = new Agento11yOpenAIAgentsHandler(client, { providerResolver: 'auto' });

await handler.handleLLMStart(
  { kwargs: { model: 'gpt-5' } },
  ['stream status'],
  'run-1',
  undefined,
  { invocation_params: { model: 'gpt-5', stream: true, session_id: 'session-42' } }
);
await handler.handleLLMNewToken('hello ', undefined, 'run-1');
await handler.handleLLMNewToken('world', undefined, 'run-1');
await handler.handleLLMEnd({ llm_output: { model_name: 'gpt-5' } }, 'run-1');
```

## Conversation mapping

Conversation ID precedence:

1. `conversation_id` / `session_id` / `group_id` from callback metadata or invocation payload
2. framework thread id (`thread_id`)
3. deterministic fallback: `agento11y:framework:openai-agents:<run_id>`

## Metadata and lineage

Injected metadata keys:

- `agento11y.framework.run_type` (required)
- `agento11y.framework.run_id`
- `agento11y.framework.thread_id`
- `agento11y.framework.parent_run_id`
- `agento11y.framework.component_name`
- `agento11y.framework.retry_attempt`
- `agento11y.framework.event_id`

Required framework tags:

- `agento11y.framework.name=openai-agents`
- `agento11y.framework.source=handler`
- `agento11y.framework.language=javascript`

## Provider resolver

Order: explicit provider option -> framework payload provider -> model prefix inference -> `custom`.

## Troubleshooting

- No events exported: ensure you passed a `Runner`/`Agent` instance (not run options) to `withAgento11yOpenAIAgentsHooks`.
- Missing conversation grouping: pass `conversation_id` or `session_id` in callback metadata/config.
- Unknown provider: set `provider` explicitly in handler options.
- No flush at shutdown: call `await client.shutdown()`.
