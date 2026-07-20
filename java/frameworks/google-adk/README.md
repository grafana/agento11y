# Grafana Sigil Java Framework Helper: Google ADK

This module maps Google ADK callback/interceptor lifecycles to Sigil generation and tool recording lifecycles.

## Scope

- Conversation-first mapping (`conversation_id`/`session_id`/`group_id` first)
- Optional lineage metadata (`run_id`, `thread_id`, `parent_run_id`, `event_id`)
- SYNC and STREAM lifecycle support
- Tool lifecycle support
- Explicit embeddings unsupported contract via `Agento11yGoogleAdkAdapter.checkEmbeddingsSupport()`

## Install

From `sdks/java`:

```bash
./gradlew :frameworks:google-adk:build
```

## Quickstart

```java
Agento11yClient client = new Agento11yClient(new Agento11yClientConfig());
Agento11yGoogleAdkAdapter.Callbacks callbacks = Agento11yGoogleAdkAdapter.createCallbacks(
    client,
    new Agento11yGoogleAdkAdapter.Options()
        .setAgentName("planner")
        .setAgentVersion("1.0.0")
        .setCaptureInputs(true)
        .setCaptureOutputs(true)
);
```

`createCallbacks(...)` is the one-liner path for runner wiring. `Agento11yGoogleAdkAdapter` remains available for advanced/manual integration.

## Run lifecycle snippet

```java
callbacks.onRunStart(new Agento11yGoogleAdkAdapter.RunStartEvent()
    .setRunId("run-1")
    .setSessionId("session-42")
    .setModelName("gpt-5")
    .setRunType("chat")
    .addPrompt("Summarize release status"));

callbacks.onRunEnd("run-1", new Agento11yGoogleAdkAdapter.RunEndEvent()
    .setResponseModel("gpt-5")
    .setStopReason("stop"));
```

## Streaming snippet

```java
callbacks.onRunStart(new Agento11yGoogleAdkAdapter.RunStartEvent()
    .setRunId("run-stream")
    .setSessionId("session-42")
    .setModelName("gemini-2.5-pro")
    .setStream(true)
    .addPrompt("Stream migration status"));
callbacks.onRunToken("run-stream", "step ");
callbacks.onRunToken("run-stream", "complete");
callbacks.onRunEnd("run-stream", new Agento11yGoogleAdkAdapter.RunEndEvent());
```

## Conversation mapping

Precedence:

1. `conversationId`
2. `sessionId`
3. `groupId`
4. `threadId`
5. fallback `agento11y:framework:google-adk:<run_id>`

## Tool lifecycle snippet

```java
callbacks.onToolStart(new Agento11yGoogleAdkAdapter.ToolStartEvent()
    .setRunId("tool-1")
    .setSessionId("session-42")
    .setToolName("lookup_customer")
    .setArguments(Map.of("customer_id", "42")));
callbacks.onToolEnd("tool-1", new Agento11yGoogleAdkAdapter.ToolEndEvent().setResult(Map.of("status", "ok")));
```

## Troubleshooting

- Missing conversation grouping: pass stable ADK session/conversation IDs.
- Provider inferred as `custom`: set explicit provider in options.
- Always call `client.shutdown()` at process teardown.
