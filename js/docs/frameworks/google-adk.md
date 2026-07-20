# Google ADK Handler (`@grafana/agento11y/google-adk`)

Use `SigilGoogleAdkHandler` to map Google ADK session/invocation callbacks to Sigil generations.

## Install

```bash
pnpm add @grafana/agento11y @google/adk
```

## Quickstart

```ts
import { SigilClient } from '@grafana/agento11y';
import { withSigilGoogleAdkPlugins } from '@grafana/agento11y/google-adk';

const client = new SigilClient();
const runnerConfig = withSigilGoogleAdkPlugins(undefined, client, {
  providerResolver: 'auto',
  agentName: 'adk-app',
});
```

Or create the plugin explicitly:

```ts
import { createSigilGoogleAdkPlugin } from '@grafana/agento11y/google-adk';

const sigilPlugin = createSigilGoogleAdkPlugin(client, { providerResolver: 'auto' });
const runnerConfig = { plugins: [sigilPlugin] };
```

`withSigilGoogleAdkPlugins(...)` appends Sigil instrumentation to ADK plugin config while preserving existing plugins.
The appended plugin implements the ADK callback surface (`beforeRunCallback`, `onEventCallback`, `afterRunCallback`, model/tool lifecycle callbacks).

## Streaming snippet

```ts
import { SigilClient } from '@grafana/agento11y';
import { SigilGoogleAdkHandler } from '@grafana/agento11y/google-adk';

const client = new SigilClient();
const handler = new SigilGoogleAdkHandler(client, { providerResolver: 'auto' });

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
3. fallback: `sigil:framework:google-adk:<run_id>`

## Metadata and lineage

- Required: `sigil.framework.run_type`
- Optional lineage: `sigil.framework.run_id`, `sigil.framework.parent_run_id`, `sigil.framework.thread_id`, `sigil.framework.event_id`, `sigil.framework.component_name`

Tags:

- `sigil.framework.name=google-adk`
- `sigil.framework.source=handler`
- `sigil.framework.language=javascript`

## Provider resolver

Uses explicit provider first, then payload, then model-prefix inference.

## Troubleshooting

- Reused ADK sessions should pass stable `session_id` for correct grouping.
- Use `provider` option when model names are custom aliases.
- Call `await client.shutdown()` to guarantee export flush.
