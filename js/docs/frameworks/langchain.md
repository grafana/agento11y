# LangChain Handler (`@grafana/agento11y/langchain`)

Use `Agento11yLangChainHandler` to map LangChain callback lifecycle events to Sigil generation records.

## Install

```bash
pnpm add @grafana/agento11y @langchain/core @langchain/openai
```

## Usage

```ts
import { Agento11yClient } from '@grafana/agento11y';
import { withAgento11yLangChainCallbacks } from '@grafana/agento11y/langchain';

const client = new Agento11yClient();
const config = withAgento11yLangChainCallbacks(undefined, client, {
  providerResolver: 'auto',
  agentName: 'langchain-app',
});
```

## End-to-end example (invoke + stream)

```ts
import { ChatOpenAI } from '@langchain/openai';
import { Agento11yClient } from '@grafana/agento11y';
import {
  Agento11yLangChainHandler,
  withAgento11yLangChainCallbacks,
} from '@grafana/agento11y/langchain';

const client = new Agento11yClient();
const handler = new Agento11yLangChainHandler(client, {
  providerResolver: 'auto',
  agentName: 'langchain-example',
  agentVersion: '1.0.0',
});

const llm = new ChatOpenAI({ model: 'gpt-4o-mini', temperature: 0 });

// Non-stream call -> SYNC generation mode.
const result = await llm.invoke(
  'Summarize why retry budgets matter.',
  withAgento11yLangChainCallbacks(undefined, client, { providerResolver: 'auto' })
);
console.log(result.content);

// Stream call -> STREAM generation mode + TTFT tracking.
const stream = await llm.stream(
  'Give me three short reliability tips.',
  withAgento11yLangChainCallbacks(undefined, client, { providerResolver: 'auto' })
);
for await (const chunk of stream) {
  if (chunk.content) process.stdout.write(String(chunk.content));
}
process.stdout.write('\n');

// Advanced usage: instantiate and pass a handler manually.
const handler = new Agento11yLangChainHandler(client, { providerResolver: 'auto' });
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

- `agento11y.framework.name=langchain`
- `agento11y.framework.source=handler`
- `agento11y.framework.language=javascript`
- `metadata["agento11y.framework.run_id"]=<framework run id>`
- `metadata["agento11y.framework.thread_id"]=<thread id>` (when present in callback metadata/config)
- `metadata["agento11y.framework.parent_run_id"]=<parent run id>` (when available)
- `metadata["agento11y.framework.component_name"]=<serialized component name>`
- `metadata["agento11y.framework.run_type"]=<llm|chat|tool|chain|retriever>`
- `metadata["agento11y.framework.tags"]=<normalized callback tags>`
- `metadata["agento11y.framework.retry_attempt"]=<attempt>` (when available)
- `metadata["agento11y.framework.event_id"]=<event id>` (when available)
- generation span attributes mirror low-cardinality framework metadata keys

Conversation mapping is conversation-first:

- `conversation_id` / `session_id` / `group_id` first
- then `thread_id`
- deterministic fallback `agento11y:framework:langchain:<run_id>`

Provider resolver behavior:

- explicit provider metadata when available
- model prefix inference (`gpt-`/`o1`/`o3`/`o4` -> `openai`, `claude-` -> `anthropic`, `gemini-` -> `gemini`)
- fallback `custom`
