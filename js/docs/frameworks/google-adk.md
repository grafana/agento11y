# Google ADK Handler (`@grafana/agento11y/google-adk`)

Use `Agento11yGoogleAdkHandler` to map Google ADK session/invocation callbacks to Sigil generations.

## Install

```bash
pnpm add @grafana/agento11y @google/adk
```

## Quickstart

```ts
import { Agento11yClient } from '@grafana/agento11y';
import { withAgento11yGoogleAdkPlugins } from '@grafana/agento11y/google-adk';

const client = new Agento11yClient();
const runnerConfig = withAgento11yGoogleAdkPlugins(undefined, client, {
  providerResolver: 'auto',
  agentName: 'adk-app',
});
```

Or create the plugin explicitly:

```ts
import { createAgento11yGoogleAdkPlugin } from '@grafana/agento11y/google-adk';

const agento11yPlugin = createAgento11yGoogleAdkPlugin(client, { providerResolver: 'auto' });
const runnerConfig = { plugins: [agento11yPlugin] };
```

`withAgento11yGoogleAdkPlugins(...)` appends Sigil instrumentation to ADK plugin config while preserving existing plugins.
The appended plugin implements the ADK callback surface (`beforeRunCallback`, `onEventCallback`, `afterRunCallback`, model/tool lifecycle callbacks).

## Streaming snippet

```ts
import { Agento11yClient } from '@grafana/agento11y';
import { Agento11yGoogleAdkHandler } from '@grafana/agento11y/google-adk';

const client = new Agento11yClient();
const handler = new Agento11yGoogleAdkHandler(client, { providerResolver: 'auto' });

await handler.handleLLMStart(
  { kwargs: { model: 'gemini-2.5-pro' } },
  ['stream adk step'],
  'run-1',
  undefined,
  { invocation_params: { model: 'gemini-2.5-pro', stream: true, session_id: 'adk-session-42' } }
);
await handler.handleLLMNewToken('step ', undefined, 'run-1');
await handler.handleLLMNewToken('done', undefined, 'run-1');
await handler.handleLLMEnd({ llm_output: { model_name: 'gemini-2.5-pro' } }, 'run-1');
```

## Conversation mapping

Primary mapping is ADK conversation/session identity:

1. `conversation_id` / `session_id` / `group_id`
2. `thread_id`
3. fallback: `agento11y:framework:google-adk:<run_id>`

## Metadata and lineage

- Required: `agento11y.framework.run_type`
- Optional lineage: `agento11y.framework.run_id`, `agento11y.framework.parent_run_id`, `agento11y.framework.thread_id`, `agento11y.framework.event_id`, `agento11y.framework.component_name`

Tags:

- `agento11y.framework.name=google-adk`
- `agento11y.framework.source=handler`
- `agento11y.framework.language=javascript`

## Provider resolver

Uses explicit provider first, then payload, then model-prefix inference.

## Troubleshooting

- Reused ADK sessions should pass stable `session_id` for correct grouping.
- Use `provider` option when model names are custom aliases.
- Call `await client.shutdown()` to guarantee export flush.
