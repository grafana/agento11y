# Grafana Agent Observability TypeScript/JavaScript SDK

`@grafana/agento11y` records normalized LLM generation and tool-execution telemetry using your OpenTelemetry tracer/meter setup.

## Installation

```bash
pnpm add @grafana/agento11y
```

For low-dependency runtimes that only need the core `Agento11yClient` and generation export APIs, use the slim core package:

```bash
pnpm add @grafana/agento11y-core
```

For a Grafana Cloud setup walkthrough (where to find the endpoint URL, instance ID, and API token), refer to the [Grafana Cloud setup guide](https://grafana.com/docs/grafana-cloud/machine-learning/agent-observability/get-started/grafana-cloud/).

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

The snippet below configures the SDK explicitly. As an alternative, set `AGENTO11Y_*` environment variables and call `new Agento11yClient()` with no arguments — refer to the [Grafana Cloud setup guide](https://grafana.com/docs/grafana-cloud/machine-learning/agent-observability/get-started/grafana-cloud/) for the variable names.

```ts
import { Agento11yClient } from "@grafana/agento11y";

const client = new Agento11yClient({
  generationExport: {
    protocol: "http",
    endpoint: "https://agento11y-prod-<region>.grafana.net",
    auth: {
      mode: "basic",
      tenantId: process.env.AGENTO11Y_AUTH_TENANT_ID,
      basicPassword: process.env.AGENTO11Y_AUTH_TOKEN,
    },
  },
  api: {
    endpoint: "https://agento11y-prod-<region>.grafana.net",
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
import { Agento11yClient } from "@grafana/agento11y";

const client = new Agento11yClient({
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
const client = new Agento11yClient({
  contentCaptureResolver: (metadata) => {
    if (metadata?.["tenant"] === "healthcare") {
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

User-provided `metadata` and `tags` are not stripped by any capture mode. SDK-internal metadata keys that carry content (e.g. `call_error`, `agento11y.conversation.title`) are stripped along with the matching content. See [Tags and Metadata](../docs/concepts/tags-and-metadata.md) for where client tags, per-generation tags, metadata, and `userId` each show up (export vs spans vs metrics).

## Pre-Ingest Redaction

Use `generationSanitizer` when you want to redact substrings from normalized generations before
validation, span sync, debug snapshots, and export.

```ts
import {
  Agento11yClient,
  createSecretRedactionSanitizer,
} from "@grafana/agento11y";

const client = new Agento11yClient({
  generationSanitizer: createSecretRedactionSanitizer({
    redactInputMessages: false, // omit to fall back to AGENTO11Y_REDACT_INPUT_MESSAGES, then false
    redactEmailAddresses: true,
  }),
});
```

The built-in sanitizer:

- redacts high-confidence secret formats in assistant text and thinking
- redacts secret formats plus env-style secret values in tool call inputs and tool results
- redacts email addresses by default
- leaves user input unchanged unless input redaction is enabled

To preserve email addresses, opt out explicitly:

```ts
const client = new Agento11yClient({
  generationSanitizer: createSecretRedactionSanitizer({
    redactEmailAddresses: false,
  }),
});
```

### Configuring redaction via environment variables

`createSecretRedactionSanitizer()` reads `AGENTO11Y_REDACT_INPUT_MESSAGES` (accepts
`1/0`, `true/false`, `yes/no`, `on/off`) when `redactInputMessages` is omitted.
Precedence is explicit option > env var > `false`. An unrecognised env value is
warned and falls back to the next layer, so a typo cannot silently flip
redaction.

```ts
import {
  createSecretRedactionSanitizer,
  Agento11yClient,
} from "@grafana/agento11y";

// Omit redactInputMessages so AGENTO11Y_REDACT_INPUT_MESSAGES decides.
const client = new Agento11yClient({
  generationSanitizer: createSecretRedactionSanitizer(),
});
```

## Hooks and Guards

Use hooks when you want Agent Observability guard rules to run before an LLM call. The SDK evaluates the hook on your request path; guard rules configured in Grafana Cloud decide whether to allow, deny, or transform the input.

Hooks are disabled by default. Enable them on the client and call `evaluateHook(...)` before the provider request:

```ts
import { HookDeniedError, Agento11yClient } from "@grafana/agento11y";

const client = new Agento11yClient({
  hooks: { enabled: true, phases: ["preflight"], timeoutMs: 15_000, failOpen: true },
});

let messages = [{ role: "user" as const, content: "Summarize this customer note..." }];
const response = await client.evaluateHook({
  phase: "preflight",
  context: {
    agentName: "support-agent",
    agentVersion: "1.0.0",
    model: { provider: "openai", name: "gpt-5" },
  },
  input: {
    messages,
    systemPrompt: "You are a helpful support agent.",
    conversationPreview: "Summarize this customer note...",
  },
});

if (response.action === "deny") {
  throw new HookDeniedError(response.reason ?? "", response.ruleId, response.evaluations);
}

messages = response.transformedInput?.messages ?? messages;
```

With `failOpen: true`, hook transport errors resolve to allow so an unavailable evaluator does not block production traffic. Set `failOpen: false` for strict paths that should fail closed.

If you use transformed input, pass the transformed messages/system prompt to the provider and record those same values in `startGeneration(...)`. If you use the Vercel AI SDK adapter, see `docs/frameworks/vercel-ai-sdk.md` for automatic preflight hook wiring.

Configure OTEL exporters (traces/metrics) in your application OTEL SDK setup. You can optionally pass `tracer` and `meter` directly to `Agento11yClient`.

Quick OTEL setup pattern before creating the agento11y client:

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
const client = new Agento11yClient({
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

- LangChain: `@grafana/agento11y/langchain`
- LangGraph: `@grafana/agento11y/langgraph`
- OpenAI Agents: `@grafana/agento11y/openai-agents`
- LlamaIndex: `@grafana/agento11y/llamaindex`
- Google ADK: `@grafana/agento11y/google-adk`
- Vercel AI SDK: `@grafana/agento11y/vercel-ai-sdk`
- Strands Agents: `@grafana/agento11y/strands`
- LangChain guide: `docs/frameworks/langchain.md`
- LangGraph guide: `docs/frameworks/langgraph.md`
- OpenAI Agents guide: `docs/frameworks/openai-agents.md`
- LlamaIndex guide: `docs/frameworks/llamaindex.md`
- Google ADK guide: `docs/frameworks/google-adk.md`
- Vercel AI SDK guide: `docs/frameworks/vercel-ai-sdk.md`
- Strands Agents guide: `docs/frameworks/strands.md`

```ts
import { Agento11yClient } from "@grafana/agento11y";
import { withAgento11yLangChainCallbacks } from "@grafana/agento11y/langchain";
import { withAgento11yLangGraphCallbacks } from "@grafana/agento11y/langgraph";
import { withAgento11yOpenAIAgentsHooks } from "@grafana/agento11y/openai-agents";
import { withAgento11yLlamaIndexCallbacks } from "@grafana/agento11y/llamaindex";
import { withAgento11yGoogleAdkPlugins } from "@grafana/agento11y/google-adk";
import { createAgento11yVercelAiSdk } from "@grafana/agento11y/vercel-ai-sdk";
import { withAgento11yStrandsHooks } from "@grafana/agento11y/strands";
import { Runner } from "@openai/agents";
import { CallbackManager } from "llamaindex";

const client = new Agento11yClient();
const langChainConfig = withAgento11yLangChainCallbacks(undefined, client, { providerResolver: "auto" });
const langGraphConfig = withAgento11yLangGraphCallbacks(undefined, client, { providerResolver: "auto" });
const runner = new Runner();
const openAIAgentsHooks = withAgento11yOpenAIAgentsHooks(runner, client, { providerResolver: "auto" });
const callbackManager = new CallbackManager();
const llamaIndexConfig = withAgento11yLlamaIndexCallbacks({ callbackManager }, client, { providerResolver: "auto" });
const googleAdkRunnerConfig = withAgento11yGoogleAdkPlugins(undefined, client, { providerResolver: "auto" });
const vercelAiSdk = createAgento11yVercelAiSdk(client, { agentName: "vercel-agent" });
const strandsConfig = withAgento11yStrandsHooks(undefined, client, { conversationId: "chat-123" });
```

Framework handlers use the `Agento11yClient` instance you pass in. If that client is configured with
`generationSanitizer`, the same redaction policy applies automatically to generations recorded
through LangChain, LangGraph, OpenAI Agents, LlamaIndex, Google ADK, and Vercel AI SDK integrations.
The same redaction policy also applies to Strands Agents generations.

Each framework handler injects:

- `agento11y.framework.name` (`langchain`, `langgraph`, `openai-agents`, `llamaindex`, `google-adk`, `vercel-ai-sdk`, or `strands`)
- `agento11y.framework.source` (`handler` for existing callback handlers, `framework` for Vercel AI SDK hooks, `hooks` for Strands)
- `agento11y.framework.language` (`javascript` for existing callback handlers, `typescript` for Vercel AI SDK and Strands hooks)
- `metadata["agento11y.framework.run_id"]`
- `metadata["agento11y.framework.thread_id"]` (when present)
- `metadata["agento11y.framework.parent_run_id"]` (when available)
- `metadata["agento11y.framework.component_name"]`
- `metadata["agento11y.framework.run_type"]`
- `metadata["agento11y.framework.tags"]`
- `metadata["agento11y.framework.retry_attempt"]` (when available)
- `metadata["agento11y.framework.event_id"]` (when available)
- `metadata["agento11y.framework.langgraph.node"]` (LangGraph when available)

Conversation mapping is conversation-first:

- `conversation_id` / `session_id` / `group_id` from framework context first
- then `thread_id`
- deterministic fallback `agento11y:framework:<framework_name>:<run_id>`

When present in generation metadata, low-cardinality framework keys are copied onto generation span attributes.

For LangGraph persistence, pass `configurable.thread_id` and reuse it across invocations:

```ts
const threadConfig = {
  ...withAgento11yLangGraphCallbacks(undefined, client, { providerResolver: "auto" }),
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
  - `agento11y.sdk.name=sdk-js`
- Normalized generation metadata always includes the same SDK identity key; conflicting caller values are overwritten.
- Raw provider artifacts are opt-in (`rawArtifacts: true`).

## Instrumentation-only mode (no generation send)

Set `generationExport.protocol` to `"none"` to keep generation/tool instrumentation and spans while disabling generation transport.

```ts
const client = new Agento11yClient({
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
const client = new Agento11yClient({
  generationExport: {
    protocol: "http",
    endpoint: "https://agento11y-prod-<region>.grafana.net",
    auth: {
      mode: "basic",
      tenantId: process.env.AGENTO11Y_AUTH_TENANT_ID,
      basicPassword: process.env.AGENTO11Y_AUTH_TOKEN,
    },
  },
  api: {
    endpoint: "https://agento11y-prod-<region>.grafana.net",
  },
});
```

### Grafana Cloud auth (basic)

For Grafana Cloud, use `basic` auth mode. The username is your Grafana Cloud instance/tenant ID and the password is your Grafana Cloud API key. See the [Grafana Cloud Agent Observability getting started docs](https://grafana.com/docs/grafana-cloud/machine-learning/agent-observability/get-started/grafana-cloud/) for full setup steps; for this SDK endpoint, copy the **API URL** from **Observability → Agent Observability → Configuration**. It looks like `https://agento11y-prod-<region>.grafana.net`.

```ts
const client = new Agento11yClient({
  generationExport: {
    protocol: "http",
    endpoint: "https://agento11y-prod-<region>.grafana.net",
    auth: {
      mode: "basic",
      tenantId: process.env.AGENTO11Y_AUTH_TENANT_ID,
      basicPassword: process.env.AGENTO11Y_AUTH_TOKEN,
    },
  },
});
```

If your deployment requires a distinct username, set `basicUser` explicitly:

```ts
auth: {
  mode: "basic",
  tenantId: process.env.AGENTO11Y_AUTH_TENANT_ID,
  basicUser: process.env.AGENTO11Y_AUTH_TENANT_ID,
  basicPassword: process.env.AGENTO11Y_AUTH_TOKEN,
},
```

## Wiring custom env vars

The SDK only auto-loads `AGENTO11Y_*` env vars (`AGENTO11Y_ENDPOINT`, `AGENTO11Y_PROTOCOL`, `AGENTO11Y_AUTH_MODE`, `AGENTO11Y_AUTH_TOKEN`, etc.) when you call `new Agento11yClient()`. For any other env var (for example one your secret manager exposes under a different name), read it in your app and pass the value into the config:

```ts
const generationBearerToken = (process.env.MY_APP_AGENTO11Y_TOKEN ?? "").trim();

const client = new Agento11yClient({
  generationExport: {
    protocol: "http",
    endpoint: "https://agento11y-prod-<region>.grafana.net",
    auth:
      generationBearerToken.length > 0
        ? { mode: "bearer", bearerToken: generationBearerToken }
        : {
            mode: "basic",
            tenantId: process.env.AGENTO11Y_AUTH_TENANT_ID,
            basicPassword: process.env.AGENTO11Y_AUTH_TOKEN,
          },
  },
  api: {
    endpoint: "https://agento11y-prod-<region>.grafana.net",
  },
});
```

Common topology:

- Grafana Cloud: generation `basic` mode with instance ID and API key.
- Self-hosted direct to the ingest API: generation `tenant` mode.
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

`submitConversationRating` sends requests to `api.endpoint`, which should be the Grafana Cloud Agent Observability API URL from Agent Observability configuration, and uses the same generation-export auth headers already configured on the SDK client.
