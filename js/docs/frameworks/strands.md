# Strands Agents Hooks (`@grafana/agento11y/strands`)

Use `withSigilStrandsHooks(...)` to instrument Strands Agents TypeScript agents with Sigil generation export, spans, metrics, tool execution spans, and streaming TTFT.

## Install

```bash
pnpm add @grafana/agento11y @strands-agents/sdk
```

## Quickstart

```ts
import { Agent } from '@strands-agents/sdk';
import { OpenAIModel } from '@strands-agents/sdk/models/openai';
import { SigilClient } from '@grafana/agento11y';
import { withSigilStrandsHooks } from '@grafana/agento11y/strands';

const sigil = new SigilClient();
const model = new OpenAIModel({ api: 'chat', modelId: 'gpt-4o-mini' });

const agent = new Agent(
  withSigilStrandsHooks(
    {
      name: 'support-agent',
      model,
      systemPrompt: 'You are concise.',
      appState: { conversation_id: 'chat-123' },
    },
    sigil,
    { providerResolver: 'auto' },
  ),
);

const result = await agent.invoke('Answer in one sentence.');
console.log(result.toString());
await sigil.shutdown();
```

`withSigilStrandsHooks(...)` can also register hooks on an already-created agent:

```ts
withSigilStrandsHooks(agent, sigil, { conversationId: 'chat-123' });
```

## Conversation ID

Use a stable conversation ID for multi-turn continuity. Precedence:

1. `withSigilStrandsHooks(..., { conversationId })`
2. `resolveConversationId(event, agent)` option
3. `agent.appState` keys: `conversation_id`, `conversationId`, `session_id`, `sessionId`, `group_id`, `groupId`
4. fallback `sigil:framework:strands:<run_id>`

For per-request routing, set `agent.appState.set('conversation_id', id)` before `agent.invoke(...)` and omit the fixed `conversationId` option.

## Metadata

Tags:

- `sigil.framework.name=strands`
- `sigil.framework.source=hooks`
- `sigil.framework.language=typescript`

Metadata includes:

- `sigil.framework.run_id`
- `sigil.framework.parent_run_id`
- `sigil.framework.component_name`
- `sigil.framework.run_type`
- `sigil.framework.event_id` when a Strands tool call ID is available

## Privacy Controls

Disable model/tool payload capture:

```ts
withSigilStrandsHooks(agent, sigil, {
  captureInputs: false,
  captureOutputs: false,
});
```

## Notes

- The adapter uses Strands lifecycle hooks and plugins. It does not replace the model or tool implementations.
- OpenAI usage requires installing Strands' OpenAI peer dependency (`openai`) and setting `OPENAI_API_KEY`.
- Call `await sigil.shutdown()` to flush queued generation export before process exit.
