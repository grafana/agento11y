# Vercel AI SDK Hooks (`@grafana/sigil-sdk-js/vercel-ai-sdk`)

Use `createSigilVercelAiSdk(...)` to instrument Vercel AI SDK v5 calls with Sigil generation export, spans, metrics, tool execution spans, and streaming TTFT.

Supported AI SDK line:

- `ai` v5 (`generateText`, `streamText`)
- Baseline generation export uses `onStepFinish` (and `onError` for streams). This path is v5-compatible and records tool executions from step `toolResults`.
- When `experimental_onStepStart`, `experimental_onToolCallStart`, and `experimental_onToolCallFinish` are available (v6+), Sigil also captures richer per-step input messages and tool timing correlation.
- Experimental callbacks can change in patch releases. Keep `ai` pinned to a tested minor line (`v5.x` or `v6.x`) in production.

## Install

```bash
pnpm add @grafana/sigil-sdk-js ai
```

## Quickstart

```ts
import { SigilClient } from '@grafana/sigil-sdk-js';
import { createSigilVercelAiSdk } from '@grafana/sigil-sdk-js/vercel-ai-sdk';
import { generateText } from 'ai';
import { openai } from '@ai-sdk/openai';

const client = new SigilClient();
const sigil = createSigilVercelAiSdk(client, {
  agentName: 'research-agent',
  agentVersion: '1.0.0',
});

const result = await generateText({
  model: openai('gpt-5'),
  prompt: 'Summarize this ticket in one paragraph.',
  ...sigil.generateTextHooks({ conversationId: 'chat-123' }),
});
```

The model object stays untouched. Sigil only consumes hook callbacks.

## Preflight guards

Set `enableHooks: true` when you want Sigil guard rules to evaluate each Vercel AI SDK model step before it reaches the provider:

```ts
import { HookDeniedError, SigilClient } from '@grafana/sigil-sdk-js';
import { createSigilVercelAiSdk } from '@grafana/sigil-sdk-js/vercel-ai-sdk';
import { generateText } from 'ai';
import { openai } from '@ai-sdk/openai';

const client = new SigilClient({
  hooks: {
    enabled: true,
    phases: ['preflight'],
    timeoutMs: 15_000,
    failOpen: true,
  },
});

const sigil = createSigilVercelAiSdk(client, {
  agentName: 'research-agent',
  agentVersion: '1.0.0',
  enableHooks: true,
});

try {
  await generateText({
    model: openai('gpt-5'),
    prompt: 'Summarize this ticket in one paragraph.',
    ...sigil.generateTextHooks({ conversationId: 'chat-123' }),
  });
} catch (error) {
  if (error instanceof HookDeniedError) {
    return new Response(`Blocked by guard: ${error.reason}`, { status: 400 });
  }
  throw error;
}
```

The adapter sends the step messages, model, agent name/version, and conversation preview to Sigil. If a guard returns `action: "deny"`, the adapter throws `HookDeniedError` and the provider call is aborted. If a guard returns `transformed_input.messages`, the adapter sends those transformed messages to the provider and records the transformed input in the generation.

`enableHooks` overrides the client-level switch for this instrumentation. Leave it unset to use `client.config.hooks.enabled`, or set it to `false` to disable hook evaluation for calls made through this adapter. With `failOpen: true`, hook transport errors resolve to allow; set `failOpen: false` for strict paths that should fail closed.

## Conversation ID (required for multi-turn continuity)

Vercel AI SDK is stateless on the server side. For multi-turn grouping, pass a stable `conversationId` per call:

```ts
const hooks = sigil.generateTextHooks({ conversationId: 'customer-42' });
```

Precedence:

1. `generateTextHooks({ conversationId })` / `streamTextHooks({ conversationId })`
2. `resolveConversationId(stepStartEvent)` from global integration options
3. fallback `sigil:framework:vercel-ai-sdk:<response.id>` (single-response scope)

## Multi-step agentic loop (`generateText`)

```ts
import { generateText, stopWhen } from 'ai';

const result = await generateText({
  model: openai('gpt-5'),
  prompt: 'What is the weather in Paris?',
  tools: {
    weather: {
      description: 'Read weather by city',
      inputSchema: { type: 'object', properties: { city: { type: 'string' } }, required: ['city'] },
      execute: async ({ city }) => ({ city, temp_c: 18 }),
    },
  },
  stopWhen: stopWhen.stepCountIs(2),
  ...sigil.generateTextHooks({ conversationId: 'weather-chat-1' }),
});
```

Each model step emits one generation with:

- `metadata["sigil.framework.step_type"]` (`initial`, `continue`, `tool-result`)
- per-step input captured when `experimental_onStepStart` is available (including prior tool result messages)

## Streaming (`streamText`) and TTFT

```ts
import { streamText } from 'ai';
import { anthropic } from '@ai-sdk/anthropic';

const stream = streamText({
  model: anthropic('claude-sonnet-4-5'),
  prompt: 'Stream a concise status update.',
  ...sigil.streamTextHooks({ conversationId: 'stream-chat-1' }),
});

for await (const _chunk of stream.textStream) {
  // consume stream
}
```

`streamTextHooks()` records TTFT from the first text chunk (`onChunk` with `chunk.type === "text"`).

## Privacy controls

Disable model/tool payload capture:

```ts
const sigil = createSigilVercelAiSdk(client, {
  captureInputs: false,
  captureOutputs: false,
});
```

- `captureInputs=false`: no generation input messages and no tool arguments
- `captureOutputs=false`: no generation output text and no tool results

## Troubleshooting

- Missing usage numbers:
  - Provider may not return usage fields in AI SDK `onStepFinish`.
  - Sigil handles missing usage safely and exports zeros only when usage payloads exist.
- Missing TTFT:
  - TTFT is only emitted for `streamText` steps where text chunks are observed.
- Tool span not appearing:
  - Ensure tool has `execute` and AI SDK emits tool lifecycle callbacks.
  - `toolCallId` must be present in callback payloads.
- Stream errors not exported:
  - Make sure `onError` is reachable in your stream-consumption path.
