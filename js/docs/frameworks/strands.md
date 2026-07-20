# Strands Agents Hooks (`@grafana/agento11y/strands`)

Use `withAgento11yStrandsHooks(...)` to instrument Strands Agents TypeScript agents with Sigil generation export, spans, metrics, tool execution spans, and streaming TTFT.

## Install

```bash
pnpm add @grafana/agento11y @strands-agents/sdk
```

## Quickstart

```ts
import { Agent } from '@strands-agents/sdk';
import { OpenAIModel } from '@strands-agents/sdk/models/openai';
import { Agento11yClient } from '@grafana/agento11y';
import { withAgento11yStrandsHooks } from '@grafana/agento11y/strands';

const agento11y = new Agento11yClient();
const model = new OpenAIModel({ api: 'chat', modelId: 'gpt-4o-mini' });

const agent = new Agent(
  withAgento11yStrandsHooks(
    {
      name: 'support-agent',
      model,
      systemPrompt: 'You are concise.',
      appState: { conversation_id: 'chat-123' },
    },
    agento11y,
    { providerResolver: 'auto' },
  ),
);

const result = await agent.invoke('Answer in one sentence.');
console.log(result.toString());
await agento11y.shutdown();
```

`withAgento11yStrandsHooks(...)` can also register hooks on an already-created agent:

```ts
withAgento11yStrandsHooks(agent, agento11y, { conversationId: 'chat-123' });
```

## Conversation ID

Use a stable conversation ID for multi-turn continuity. Precedence:

1. `withAgento11yStrandsHooks(..., { conversationId })`
2. `resolveConversationId(event, agent)` option
3. `agent.appState` keys: `conversation_id`, `conversationId`, `session_id`, `sessionId`, `group_id`, `groupId`
4. fallback `agento11y:framework:strands:<run_id>`

For per-request routing, set `agent.appState.set('conversation_id', id)` before `agent.invoke(...)` and omit the fixed `conversationId` option.

## Metadata

Tags:

- `agento11y.framework.name=strands`
- `agento11y.framework.source=hooks`
- `agento11y.framework.language=typescript`

Metadata includes:

- `agento11y.framework.run_id`
- `agento11y.framework.parent_run_id`
- `agento11y.framework.component_name`
- `agento11y.framework.run_type`
- `agento11y.framework.event_id` when a Strands tool call ID is available

## Privacy Controls

Disable model/tool payload capture:

```ts
withAgento11yStrandsHooks(agent, agento11y, {
  captureInputs: false,
  captureOutputs: false,
});
```

## Notes

- The adapter uses Strands lifecycle hooks and plugins. It does not replace the model or tool implementations.
- OpenAI usage requires installing Strands' OpenAI peer dependency (`openai`) and setting `OPENAI_API_KEY`.
- Call `await agento11y.shutdown()` to flush queued generation export before process exit.
