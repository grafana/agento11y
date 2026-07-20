# LangGraph Handler (`@grafana/agento11y/langgraph`)

Use `Agento11yLangGraphHandler` to map LangGraph callback lifecycle events to Sigil generation records.

## Install

```bash
pnpm add @grafana/agento11y @langchain/core @langchain/langgraph @langchain/openai
```

## Usage

```ts
import { Agento11yClient } from '@grafana/agento11y';
import { withAgento11yLangGraphCallbacks } from '@grafana/agento11y/langgraph';

const client = new Agento11yClient();
const config = withAgento11yLangGraphCallbacks(undefined, client, { providerResolver: 'auto' });
```

## End-to-end example (graph invoke + stream)

```ts
import { ChatOpenAI } from '@langchain/openai';
import { END, START, StateGraph, Annotation } from '@langchain/langgraph';
import { Agento11yClient } from '@grafana/agento11y';
import {
  Agento11yLangGraphHandler,
  withAgento11yLangGraphCallbacks,
} from '@grafana/agento11y/langgraph';

const GraphState = Annotation.Root({
  prompt: Annotation<string>(),
  answer: Annotation<string>(),
});

const client = new Agento11yClient();
const handler = new Agento11yLangGraphHandler(client, {
  providerResolver: 'auto',
  agentName: 'langgraph-example',
  agentVersion: '1.0.0',
});
const llm = new ChatOpenAI({ model: 'gpt-4o-mini', temperature: 0 });

const graph = new StateGraph(GraphState)
  .addNode('model', async (state) => {
    const response = await llm.invoke(
      state.prompt,
      withAgento11yLangGraphCallbacks(undefined, client, { providerResolver: 'auto' })
    );
    return { answer: String(response.content) };
  })
  .addEdge(START, 'model')
  .addEdge('model', END)
  .compile();

// Non-stream graph invocation.
const out = await graph.invoke({ prompt: 'Explain SLO burn rate in one paragraph.', answer: '' });
console.log(out.answer);

// Streamed graph updates.
for await (const _event of graph.stream({ prompt: 'List three practical alerting tips.', answer: '' })) {
  // Consume events to drive streamed model execution.
}

// Advanced usage: instantiate and pass a handler manually.
const handler = new Agento11yLangGraphHandler(client, { providerResolver: 'auto' });
await llm.invoke('manual handler wiring', { callbacks: [handler] });

await client.shutdown();
```

## Persistent thread example (LangGraph checkpointer)

```ts
import { MemorySaver } from '@langchain/langgraph';

const checkpointer = new MemorySaver();
const persistedGraph = new StateGraph(GraphState)
  .addNode('model', async (state) => {
    const response = await llm.invoke(state.prompt, { callbacks: [handler] });
    return { answer: String(response.content) };
  })
  .addEdge(START, 'model')
  .addEdge('model', END)
  .compile({ checkpointer });
const threadConfig = {
  ...withAgento11yLangGraphCallbacks(undefined, client, { providerResolver: 'auto' }),
  configurable: { thread_id: 'customer-42' },
};

await persistedGraph.invoke({ prompt: 'Remember that my timezone is UTC+1.', answer: '' }, threadConfig);
await persistedGraph.invoke({ prompt: 'What timezone did I just give you?', answer: '' }, threadConfig);
```

When `thread_id` is present, the handler records:

- `conversationId=<thread_id>`
- `metadata["agento11y.framework.run_id"]=<run id>`
- `metadata["agento11y.framework.thread_id"]=<thread id>`
- generation span attributes `agento11y.framework.run_id` and `agento11y.framework.thread_id`

When `conversation_id` / `session_id` / `group_id` is present, that value is used as primary `conversationId`.

## Contract

- `handleLLMStart` / `handleChatModelStart` starts recorder lifecycle.
- `handleLLMNewToken` sets first-token timestamp and accumulates streamed output.
- `handleLLMEnd` maps output + usage and ends recorder.
- `handleLLMError` sets call error and ends recorder.
- `handleToolStart` / `handleToolEnd` / `handleToolError` maps into `startToolExecution(...)`.
- `handleChainStart` / `handleChainEnd` / `handleChainError` emits framework chain spans.
- `handleRetrieverStart` / `handleRetrieverEnd` / `handleRetrieverError` emits framework retriever spans.

Framework tags and metadata are always injected:

- `agento11y.framework.name=langgraph`
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
- `metadata["agento11y.framework.langgraph.node"]=<node id>` (when callback context exposes it)
- generation span attributes mirror low-cardinality framework metadata keys

Fallback conversation mapping uses `agento11y:framework:langgraph:<run_id>` when no framework session key is available.

Provider resolver behavior:

- explicit provider metadata when available
- model prefix inference (`gpt-`/`o1`/`o3`/`o4` -> `openai`, `claude-` -> `anthropic`, `gemini-` -> `gemini`)
- fallback `custom`
