# Sigil JS Provider Helper: Anthropic

This helper maps strict Anthropic Messages payloads into Sigil `Generation` records.

## Embeddings support

This helper currently supports Anthropic Messages APIs only. Native Anthropic embeddings endpoints are not available in the official SDK/API surface used in this repository.

## Scope

- Wrapper calls:
  - `anthropic.messages.create(client, request, providerCall, options?)`
  - `anthropic.messages.stream(client, request, providerCall, options?)`
- Mapper functions:
  - `anthropic.messages.fromRequestResponse(request, response, options?)`
  - `anthropic.messages.fromStream(request, summary, options?)`
- Raw artifacts (debug opt-in):
  - `request`
  - `response` (sync)
  - `provider_event` (stream)

## Wrapper-first example

```ts
import { SigilClient, anthropic } from "@grafana/agento11y";

const client = new SigilClient();

const response = await anthropic.messages.create(
  client,
  {
    model: "claude-sonnet-4-5",
    max_tokens: 256,
    messages: [{ role: "user", content: [{ type: "text", text: "Hello" }] }],
  },
  async (request) => provider.messages.create(request)
);
```

## Explicit flow example

```ts
const recorder = client.startGeneration({
  model: { provider: "anthropic", name: "claude-sonnet-4-5" },
});

try {
  const response = await provider.messages.create(request);
  recorder.setResult(anthropic.messages.fromRequestResponse(request, response));
} catch (error) {
  recorder.setCallError(error);
  throw error;
} finally {
  recorder.end();
}
```

## Raw artifact policy

- Default OFF.
- Enable only for debug workflows with `{ rawArtifacts: true }`.

## Provider metadata mapping

In addition to normalized usage fields, Anthropic server-tool counters are mapped into Sigil metadata when present:

- `sigil.gen_ai.usage.server_tool_use.web_search_requests`
- `sigil.gen_ai.usage.server_tool_use.web_fetch_requests`
- `sigil.gen_ai.usage.server_tool_use.total_requests`
