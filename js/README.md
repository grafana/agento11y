# Grafana Sigil TypeScript/JavaScript SDK

Sigil records normalized LLM generation and tool-execution telemetry using your OpenTelemetry tracer/meter setup.

## Installation

```bash
pnpm add @grafana/sigil-sdk-js
```

For a Grafana Cloud setup walkthrough (where to find the endpoint URL, instance ID, and API token), refer to the [Grafana Cloud setup guide](https://grafana.com/docs/grafana-cloud/machine-learning/ai-observability/get-started/grafana-cloud/).

## Validation

Run the shared core conformance suite for the JavaScript SDK from the repo root:

```bash
mise run test:ts:sdk-conformance
```

Run the cross-language aggregate core conformance suite from the repo root:

```bash
mise run sdk:conformance
```

## Quick Start

The snippet below configures the SDK explicitly. As an alternative, set `SIGIL_*` environment variables and call `new SigilClient()` with no arguments — refer to the [Grafana Cloud setup guide](https://grafana.com/docs/grafana-cloud/machine-learning/ai-observability/get-started/grafana-cloud/) for the variable names.

```ts
import { SigilClient } from "@grafana/sigil-sdk-js";

const client = new SigilClient({
  generationExport: {
    protocol: "http",
    endpoint: "http://localhost:8080",
    auth: { mode: "tenant", tenantId: "dev-tenant" },
  },
  api: {
    endpoint: "http://localhost:8080",
  },
});

await client.startGeneration(
  {
    conversationId: "conv-1",
    model: { provider: "openai", name: "gpt-5" },
  },
  async (recorder) => {
    const outputText = "Hello from model";
    recorder.setResult({
      output: [{ role: "assistant", content: outputText }],
    });
  }
);

await client.shutdown();
```

## Content Capture

`contentCapture` controls what content the SDK includes in exported generation payloads and OTel span attributes. See [Content Capture Modes](../docs/concepts/content-capture-modes.md) for the canonical mode matrix and defaults; the snippets below show how to wire it up in TypeScript.

Client-level default:

```ts
import { SigilClient } from "@grafana/sigil-sdk-js";

const client = new SigilClient({
  contentCapture: "metadata_only",
});
```

The core SDK client treats `"default"` as `"no_tool_content"`: generation content is captured but tool-execution arguments and results stay out of spans.

Per-generation override:

```ts
await client.startGeneration(
  {
    model: { provider: "openai", name: "gpt-5" },
    contentCapture: "full",
  },
  async (recorder) => {
    recorder.setResult({ output: [{ role: "assistant", content: "hi" }] });
  }
);
```

Per-tool-execution override (here `"full"` opts into capturing tool arguments and results in the span):

```ts
await client.startToolExecution(
  { toolName: "search", contentCapture: "full" },
  async (recorder) => {
    recorder.setResult({ arguments: { q: "weather" }, result: { tempC: 18 } });
  }
);
```

Dynamic resolution via `contentCaptureResolver`:

```ts
const client = new SigilClient({
  contentCaptureResolver: (metadata) => {
    if (metadata?.["sigil.tenant"] === "healthcare") {
      return "metadata_only";
    }
    return "default"; // defer to `contentCapture`
  },
});
```

The resolver receives the recording's metadata (or `undefined` for recording types that have no metadata, like tool executions). Thrown errors are caught and treated as `"metadata_only"` (fail-closed).

Resolution precedence (highest to lowest):

1. Per-recording `contentCapture` on `GenerationStart` / `ToolExecutionStart`
2. `contentCaptureResolver` return value
3. Client-level `contentCapture` (defaults to `"no_tool_content"`)

Unlike the Go, Python, Java, and .NET SDKs, the JS SDK does not propagate the resolved capture mode through async context, so tool executions started inside a generation block do not automatically inherit the generation's mode. Set `contentCapture` on each `ToolExecutionStart` when you need a tool to follow a non-default policy.

User-provided `metadata` and `tags` are not stripped by any capture mode. SDK-internal metadata keys that carry content (e.g. `call_error`, `sigil.conversation.title`) are stripped along with the matching content.

## Pre-Ingest Redaction

Use `generationSanitizer` when you want to redact substrings from normalized generations before
validation, span sync, debug snapshots, and export.

```ts
import {
  SigilClient,
  createSecretRedactionSanitizer,
} from "@grafana/sigil-sdk-js";

const client = new SigilClient({
  generationSanitizer: createSecretRedactionSanitizer({
    redactInputMessages: false,
    redactEmailAddresses: true,
  }),
});
```

The built-in sanitizer:

- redacts high-confidence secret formats in assistant text and thinking
- redacts secret formats plus env-style secret values in tool call inputs and tool results
- redacts email addresses by default
- leaves user input unchanged unless `redactInputMessages: true` is set

To preserve email addresses, opt out explicitly:

```ts
const client = new SigilClient({
  generationSanitizer: createSecretRedactionSanitizer({
    redactEmailAddresses: false,
  }),
});
```

Configure OTEL exporters (traces/metrics) in your application OTEL SDK setup. You can optionally pass `tracer` and `meter` directly to `SigilClient`.

Quick OTEL setup pattern before creating the Sigil client:

```ts
import { NodeSDK } from "@opentelemetry/sdk-node";

const otel = new NodeSDK();
await otel.start();
```

## Core API

- `startGeneration(...)` and `startStreamingGeneration(...)`
- `startToolExecution(...)`
- Recorder methods: `setResult(...)`, `setCallError(...)`, `end()`, `getError()`
- Lifecycle: `flush()`, `shutdown()`

### Manual `try/finally` style

```ts
const recorder = client.startGeneration({
  model: { provider: "anthropic", name: "claude-sonnet-4-5" },
});

try {
  recorder.setResult({
    output: [{ role: "assistant", content: "Done" }],
  });
} catch (error) {
  recorder.setCallError(error);
  throw error;
} finally {
  recorder.end();
}
```

## Embedding Observability

Use `startEmbedding(...)` for embedding API calls. Embedding recording creates OTel spans and SDK metrics only, and does not enqueue generation exports.

```ts
await client.startEmbedding(
  {
    agentName: "retrieval-worker",
    agentVersion: "1.0.0",
    model: { provider: "openai", name: "text-embedding-3-small" },
  },
  async (recorder) => {
    const response = await openai.embeddings.create(request);
    recorder.setResult({
      inputCount: request.input.length,
      inputTokens: response.usage?.prompt_tokens ?? 0,
      inputTexts: request.input,
      responseModel: response.model,
    });
  }
);
```

Input text capture is opt-in:

```ts
const client = new SigilClient({
  embeddingCapture: {
    captureInput: true,
    maxInputItems: 20,
    maxTextLength: 1024,
  },
});
```

`embeddingCapture.captureInput` may expose PII/document content in spans. Keep it disabled by default and enable it only for scoped debugging.

TraceQL examples:

- `traces{gen_ai.operation.name="embeddings"}`
- `traces{gen_ai.operation.name="embeddings" && gen_ai.request.model="text-embedding-3-small"}`
- `traces{gen_ai.operation.name="embeddings" && error.type!=""}`

## Tool Execution Example

```ts
await client.startToolExecution(
  {
    toolName: "weather",
    includeContent: true,
  },
  async (recorder) => {
    recorder.setResult({
      arguments: { city: "Paris" },
      result: { temp_c: 18 },
    });
  }
);
```

## Provider Helpers

- OpenAI: `docs/providers/openai.md`
- Anthropic: `docs/providers/anthropic.md`
- Gemini: `docs/providers/gemini.md`

## Framework Handlers

Use module subpath exports for framework callback integrations:

- LangChain: `@grafana/sigil-sdk-js/langchain`
- LangGraph: `@grafana/sigil-sdk-js/langgraph`
- OpenAI Agents: `@grafana/sigil-sdk-js/openai-agents`
- LlamaIndex: `@grafana/sigil-sdk-js/llamaindex`
- Google ADK: `@grafana/sigil-sdk-js/google-adk`
- Vercel AI SDK: `@grafana/sigil-sdk-js/vercel-ai-sdk`
- Strands Agents: `@grafana/sigil-sdk-js/strands`
- LangChain guide: `docs/frameworks/langchain.md`
- LangGraph guide: `docs/frameworks/langgraph.md`
- OpenAI Agents guide: `docs/frameworks/openai-agents.md`
- LlamaIndex guide: `docs/frameworks/llamaindex.md`
- Google ADK guide: `docs/frameworks/google-adk.md`
- Vercel AI SDK guide: `docs/frameworks/vercel-ai-sdk.md`
- Strands Agents guide: `docs/frameworks/strands.md`

```ts
import { SigilClient } from "@grafana/sigil-sdk-js";
import { withSigilLangChainCallbacks } from "@grafana/sigil-sdk-js/langchain";
import { withSigilLangGraphCallbacks } from "@grafana/sigil-sdk-js/langgraph";
import { withSigilOpenAIAgentsHooks } from "@grafana/sigil-sdk-js/openai-agents";
import { withSigilLlamaIndexCallbacks } from "@grafana/sigil-sdk-js/llamaindex";
import { withSigilGoogleAdkPlugins } from "@grafana/sigil-sdk-js/google-adk";
import { createSigilVercelAiSdk } from "@grafana/sigil-sdk-js/vercel-ai-sdk";
import { withSigilStrandsHooks } from "@grafana/sigil-sdk-js/strands";
import { Runner } from "@openai/agents";
import { CallbackManager } from "llamaindex";

const client = new SigilClient();
const langChainConfig = withSigilLangChainCallbacks(undefined, client, { providerResolver: "auto" });
const langGraphConfig = withSigilLangGraphCallbacks(undefined, client, { providerResolver: "auto" });
const runner = new Runner();
const openAIAgentsHooks = withSigilOpenAIAgentsHooks(runner, client, { providerResolver: "auto" });
const callbackManager = new CallbackManager();
const llamaIndexConfig = withSigilLlamaIndexCallbacks({ callbackManager }, client, { providerResolver: "auto" });
const googleAdkRunnerConfig = withSigilGoogleAdkPlugins(undefined, client, { providerResolver: "auto" });
const vercelAiSdk = createSigilVercelAiSdk(client, { agentName: "vercel-agent" });
const strandsConfig = withSigilStrandsHooks(undefined, client, { conversationId: "chat-123" });
```

Framework handlers use the `SigilClient` instance you pass in. If that client is configured with
`generationSanitizer`, the same redaction policy applies automatically to generations recorded
through LangChain, LangGraph, OpenAI Agents, LlamaIndex, Google ADK, and Vercel AI SDK integrations.
The same redaction policy also applies to Strands Agents generations.

Each framework handler injects:

- `sigil.framework.name` (`langchain`, `langgraph`, `openai-agents`, `llamaindex`, `google-adk`, `vercel-ai-sdk`, or `strands`)
- `sigil.framework.source` (`handler` for existing callback handlers, `framework` for Vercel AI SDK hooks, `hooks` for Strands)
- `sigil.framework.language` (`javascript` for existing callback handlers, `typescript` for Vercel AI SDK and Strands hooks)
- `metadata["sigil.framework.run_id"]`
- `metadata["sigil.framework.thread_id"]` (when present)
- `metadata["sigil.framework.parent_run_id"]` (when available)
- `metadata["sigil.framework.component_name"]`
- `metadata["sigil.framework.run_type"]`
- `metadata["sigil.framework.tags"]`
- `metadata["sigil.framework.retry_attempt"]` (when available)
- `metadata["sigil.framework.event_id"]` (when available)
- `metadata["sigil.framework.langgraph.node"]` (LangGraph when available)

Conversation mapping is conversation-first:

- `conversation_id` / `session_id` / `group_id` from framework context first
- then `thread_id`
- deterministic fallback `sigil:framework:<framework_name>:<run_id>`

When present in generation metadata, low-cardinality framework keys are copied onto generation span attributes.

For LangGraph persistence, pass `configurable.thread_id` and reuse it across invocations:

```ts
const threadConfig = {
  ...withSigilLangGraphCallbacks(undefined, client, { providerResolver: "auto" }),
  configurable: { thread_id: 'customer-42' },
};
await graph.invoke({ prompt: 'Remember my timezone is UTC+1.', answer: '' }, threadConfig);
await graph.invoke({ prompt: 'What timezone did I give you?', answer: '' }, threadConfig);
```

## Behavior

- Generation modes are explicit: `SYNC` and `STREAM`.
- Generation export supports HTTP, gRPC, and `none` (instrumentation-only).
- Traces/metrics use `config.tracer`/`config.meter` when provided, otherwise OTEL globals.
- Exports are asynchronous with bounded queueing and retry/backoff.
- `flush()` drains queued generations; `shutdown()` flushes and closes generation exporters.
- Empty tool names produce a no-op tool recorder.
- Generation/tool spans always include SDK identity attributes:
  - `sigil.sdk.name=sdk-js`
- Normalized generation metadata always includes the same SDK identity key; conflicting caller values are overwritten.
- Raw provider artifacts are opt-in (`rawArtifacts: true`).

## Instrumentation-only mode (no generation send)

Set `generationExport.protocol` to `"none"` to keep generation/tool instrumentation and spans while disabling generation transport.

```ts
const client = new SigilClient({
  generationExport: {
    protocol: "none",
  },
});
```

## SDK metrics

The SDK emits these OTel histograms through your configured OTEL meter provider:

- `gen_ai.client.operation.duration`
- `gen_ai.client.token.usage`
- `gen_ai.client.time_to_first_token`
- `gen_ai.client.tool_calls_per_operation`

## Generation export auth modes

Auth is configured for `generationExport`.

- `mode: "none"`
- `mode: "tenant"` (requires `tenantId`, injects `X-Scope-OrgID`)
- `mode: "bearer"` (requires `bearerToken`, injects `Authorization: Bearer <token>`)
- `mode: "basic"` (requires `basicPassword` + `basicUser` or `tenantId`, injects `Authorization: Basic <base64(user:password)>`; also injects `X-Scope-OrgID` when `tenantId` is set — for multi-tenant deployments only, not needed for Grafana Cloud)

Invalid mode/field combinations throw during client config resolution.

If explicit headers already contain `Authorization` or `X-Scope-OrgID`, explicit headers take precedence.

```ts
const client = new SigilClient({
  generationExport: {
    protocol: "http",
    endpoint: "http://localhost:8080",
    auth: { mode: "tenant", tenantId: "prod-tenant" },
  },
  api: {
    endpoint: "http://localhost:8080",
  },
});
```

### Grafana Cloud auth (basic)

For Grafana Cloud, use `basic` auth mode. The username is your Grafana Cloud instance/tenant ID and the password is your Grafana Cloud API key. See the [Grafana Cloud AI Observability getting started docs](https://grafana.com/docs/grafana-cloud/machine-learning/ai-observability/get-started/grafana-cloud/) for full setup steps; for this SDK endpoint, copy the **API URL** from **Observability → AI Observability → Configuration**. It looks like `https://sigil-prod-<region>.grafana.net`.

```ts
const client = new SigilClient({
  generationExport: {
    protocol: "http",
    endpoint: "https://sigil-prod-<region>.grafana.net",
    auth: {
      mode: "basic",
      tenantId: process.env.SIGIL_AUTH_TENANT_ID,
      basicPassword: process.env.SIGIL_AUTH_TOKEN,
    },
  },
});
```

If your deployment requires a distinct username, set `basicUser` explicitly:

```ts
auth: {
  mode: "basic",
  tenantId: process.env.SIGIL_AUTH_TENANT_ID,
  basicUser: process.env.SIGIL_AUTH_TENANT_ID,
  basicPassword: process.env.SIGIL_AUTH_TOKEN,
},
```

## Wiring custom env vars

The SDK only auto-loads `SIGIL_*` env vars (`SIGIL_ENDPOINT`, `SIGIL_PROTOCOL`, `SIGIL_AUTH_MODE`, `SIGIL_AUTH_TOKEN`, etc.) when you call `new SigilClient()`. For any other env var (for example one your secret manager exposes under a different name), read it in your app and pass the value into the config:

```ts
const generationBearerToken = (process.env.MY_APP_SIGIL_TOKEN ?? "").trim();

const client = new SigilClient({
  generationExport: {
    protocol: "http",
    endpoint: "http://localhost:8080",
    auth:
      generationBearerToken.length > 0
        ? { mode: "bearer", bearerToken: generationBearerToken }
        : { mode: "tenant", tenantId: "dev-tenant" },
  },
  api: {
    endpoint: "http://localhost:8080",
  },
});
```

Common topology:

- Grafana Cloud: generation `basic` mode with instance ID and API key.
- Self-hosted direct to Sigil: generation `tenant` mode.
- Traces/metrics via OTEL Collector/Alloy: configure exporters in your app OTEL SDK setup.
- Enterprise proxy: generation `bearer` mode to proxy; proxy authenticates and forwards tenant header upstream.

## Conversation Ratings

Use the SDK helper to submit user-facing ratings:

```ts
const result = await client.submitConversationRating("conv-123", {
  ratingId: "rat-123",
  rating: "CONVERSATION_RATING_VALUE_BAD",
  comment: "Answer ignored user context",
  metadata: { channel: "assistant-ui" },
  source: "sdk-js",
});

console.log(result.rating.rating, result.summary.hasBadRating);
```

`submitConversationRating` sends requests to `api.endpoint` (default `http://localhost:8080`) and uses the same generation-export auth headers (`tenant` or `bearer`) already configured on the SDK client.
