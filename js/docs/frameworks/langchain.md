# LangChain Handler (`@grafana/agento11y/langchain`)

Use `SigilLangChainHandler` to map LangChain callback lifecycle events to Sigil generation records.

## Install

```bash
pnpm add @grafana/agento11y @langchain/core @langchain/openai
```

## Usage

```ts
import { SigilClient } from '@grafana/agento11y';
import { withSigilLangChainCallbacks } from '@grafana/agento11y/langchain';

const client = new SigilClient();
const config = withSigilLangChainCallbacks(undefined, client, {
  providerResolver: 'auto',
  agentName: 'langchain-app',
});
```

## End-to-end example (invoke + stream)

```ts
import { ChatOpenAI } from '@langchain/openai';
import { SigilClient } from '@grafana/agento11y';
import {
  SigilLangChainHandler,
  withSigilLangChainCallbacks,
} from '@grafana/agento11y/langchain';

const client = new SigilClient();
const handler = new SigilLangChainHandler(client, {
  providerResolver: 'auto',
  agentName: 'langchain-example',
  agentVersion: '1.0.0',
});

const llm = new ChatOpenAI({ model: 'gpt-4o-mini', temperature: 0 });

// Non-stream call -> SYNC generation mode.
const result = await llm.invoke(
  'Summarize why retry budgets matter.',
  withSigilLangChainCallbacks(undefined, client, { providerResolver: 'auto' })
);
console.log(result.content);

// Stream call -> STREAM generation mode + TTFT tracking.
const stream = await llm.stream(
  'Give me three short reliability tips.',
  withSigilLangChainCallbacks(undefined, client, { providerResolver: 'auto' })
);
for await (const chunk of stream) {
  if (chunk.content) process.stdout.write(String(chunk.content));
}
process.stdout.write('\n');

// Advanced usage: instantiate and pass a handler manually.
const handler = new SigilLangChainHandler(client, { providerResolver: 'auto' });
await llm.invoke('manual handler wiring', { callbacks: [handler] });

await client.shutdown();
```

## Contract

- `handleLLMStart` / `handleChatModelStart` starts recorder lifecycle.
- `handleLLMNewToken` sets first-token timestamp and accumulates streamed output.
- `handleLLMEnd` maps output + usage and ends recorder.
- `handleLLMError` sets call error and ends recorder.
- `handleToolStart` / `handleToolEnd` / `handleToolError` maps into `startToolExecution(...)`.
- `handleChainStart` / `handleChainEnd` / `handleChainError` emits framework chain spans.
- `handleRetrieverStart` / `handleRetrieverEnd` / `handleRetrieverError` emits framework retriever spans.

Framework tags and metadata are always injected:

- `sigil.framework.name=langchain`
- `sigil.framework.source=handler`
- `sigil.framework.language=javascript`
- `metadata["sigil.framework.run_id"]=<framework run id>`
- `metadata["sigil.framework.thread_id"]=<thread id>` (when present in callback metadata/config)
- `metadata["sigil.framework.parent_run_id"]=<parent run id>` (when available)
- `metadata["sigil.framework.component_name"]=<serialized component name>`
- `metadata["sigil.framework.run_type"]=<llm|chat|tool|chain|retriever>`
- `metadata["sigil.framework.tags"]=<normalized callback tags>`
- `metadata["sigil.framework.retry_attempt"]=<attempt>` (when available)
- `metadata["sigil.framework.event_id"]=<event id>` (when available)
- generation span attributes mirror low-cardinality framework metadata keys

Conversation mapping is conversation-first:

- `conversation_id` / `session_id` / `group_id` first
- then `thread_id`
- deterministic fallback `sigil:framework:langchain:<run_id>`

Provider resolver behavior:

- explicit provider metadata when available
- model prefix inference (`gpt-`/`o1`/`o3`/`o4` -> `openai`, `claude-` -> `anthropic`, `gemini-` -> `gemini`)
- fallback `custom`
