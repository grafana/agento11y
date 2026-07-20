# Sigil JS Provider Helper: OpenAI

This helper now mirrors official OpenAI SDK shapes for both Chat Completions and Responses.

## Public API

- Chat Completions wrappers:
  - `openai.chat.completions.create(client, request, providerCall, options?)`
  - `openai.chat.completions.stream(client, request, providerCall, options?)`
- Chat Completions mappers:
  - `openai.chat.completions.fromRequestResponse(request, response, options?)`
  - `openai.chat.completions.fromStream(request, summary, options?)`

- Responses wrappers:
  - `openai.responses.create(client, request, providerCall, options?)`
  - `openai.responses.stream(client, request, providerCall, options?)`
- Responses mappers:
  - `openai.responses.fromRequestResponse(request, response, options?)`
  - `openai.responses.fromStream(request, summary, options?)`

- Embeddings wrapper:
  - `openai.embeddings.create(client, request, providerCall, options?)`
- Embeddings mapper:
  - `openai.embeddings.fromRequestResponse(request, response)`

## Integration styles

- Strict wrappers: call OpenAI and record in one step.
- Manual instrumentation: call OpenAI yourself, then map strict OpenAI request/response payloads with `fromRequestResponse` or `fromStream`.

## Responses-first wrapper example

```ts
import OpenAI from 'openai';
import { Agento11yClient, openai } from '@grafana/agento11y';

const agento11y = new Agento11yClient();
const provider = new OpenAI({ apiKey: process.env.OPENAI_API_KEY });

const response = await openai.responses.create(
  agento11y,
  {
    model: 'gpt-5',
    instructions: 'Be concise',
    input: 'Summarize rollout status in 3 bullets',
    max_output_tokens: 300,
  },
  async (request) => provider.responses.create(request)
);

console.log(response.output_text);
```

## Chat Completions stream example

```ts
const summary = await openai.chat.completions.stream(
  agento11y,
  {
    model: 'gpt-5',
    stream: true,
    messages: [{ role: 'user', content: 'Stream a short status update' }],
  },
  async (request) => {
    const stream = await provider.chat.completions.create(request);
    const events = [];
    for await (const event of stream) {
      events.push(event);
    }
    return { events };
  }
);
```

## Manual instrumentation example (strict mapper)

```ts
const request = {
  model: 'gpt-5',
  instructions: 'Be concise',
  input: 'Summarize rollout status in 3 bullets',
};

const options = {
  conversationId: 'conv-1',
  agentName: 'assistant',
  agentVersion: '1.0.0',
};

const recorder = agento11y.startGeneration({
  conversationId: options.conversationId,
  agentName: options.agentName,
  agentVersion: options.agentVersion,
  model: { provider: 'openai', name: request.model },
});

try {
  const response = await provider.responses.create(request);
  recorder.setResult(openai.responses.fromRequestResponse(request, response, options));
} catch (error) {
  recorder.setCallError(error);
  throw error;
} finally {
  recorder.end();
}
```

## Embeddings example

```ts
const embeddingResponse = await openai.embeddings.create(
  agento11y,
  {
    model: 'text-embedding-3-small',
    input: ['hello', 'world'],
  },
  async (request) => provider.embeddings.create(request)
);

console.log(embeddingResponse.model);
```

## Raw artifact policy

Raw artifacts are OFF by default.

- Chat artifact names:
  - `openai.chat.request`
  - `openai.chat.response`
  - `openai.chat.tools`
  - `openai.chat.stream_events`
- Responses artifact names:
  - `openai.responses.request`
  - `openai.responses.response`
  - `openai.responses.tools`
  - `openai.responses.stream_events`

Enable only for debugging:

```ts
{ rawArtifacts: true }
```

## Usage mapping notes

OpenAI usage details map into normalized token usage fields:

- Chat Completions: `prompt_tokens_details.cached_tokens` -> `usage.cacheReadInputTokens`
- Chat Completions: `completion_tokens_details.reasoning_tokens` -> `usage.reasoningTokens`
- Responses: `input_tokens_details.cached_tokens` -> `usage.cacheReadInputTokens`
- Responses: `output_tokens_details.reasoning_tokens` -> `usage.reasoningTokens`
