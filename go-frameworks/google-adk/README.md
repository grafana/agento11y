# Grafana Sigil Go Framework Helper: Google ADK

This module maps Google ADK callback/interceptor lifecycles to Sigil generation and tool recorder lifecycles.

## Scope

- Conversation-first run mapping (`conversation_id` / `session_id` / `group_id` first)
- Optional lineage metadata (`run_id`, `thread_id`, `parent_run_id`, `event_id`)
- SYNC/STREAM run lifecycle support with TTFT capture
- Tool lifecycle support

## Embeddings support

This helper currently supports Google ADK run and tool lifecycles only. The
Google ADK lifecycle surface used in this repository does not expose a
dedicated embeddings callback, so embedding conformance is explicitly
unsupported until that surface exists.

Use the capability gate before assuming framework-level embedding coverage:

```go
if err := googleadk.CheckEmbeddingsSupport(); err != nil {
	// Embedding conformance is not available through the current ADK lifecycle.
}
```

## Install

```bash
go get github.com/grafana/agento11y/go-frameworks/google-adk
```

## Quickstart

```go
client := sigil.NewClient(cfg)
captureInputs := true
captureOutputs := true
callbacks := googleadk.NewCallbacks(client, googleadk.Options{
	AgentName:      "planner",
	AgentVersion:   "1.0.0",
	CaptureInputs:  &captureInputs,
	CaptureOutputs: &captureOutputs,
})
```

`NewCallbacks(...)` is the one-liner path for wiring ADK lifecycle hooks once in runner setup.
`NewSigilAdapter(...)` remains available for advanced/manual integration.

Capture defaults:

- `CaptureInputs=nil` defaults to `true`.
- `CaptureOutputs=nil` defaults to `true`.

## Run lifecycle mapping

```go
_ = callbacks.OnRunStart(ctx, googleadk.RunStartEvent{
	RunID:       "run-1",
	SessionID:   "session-42",
	ModelName:   "gpt-5",
	RunType:     "chat",
	Prompts:     []string{"Summarize release status"},
	Stream:      false,
	Metadata:    map[string]any{"team": "infra"},
})

_ = callbacks.OnRunEnd("run-1", googleadk.RunEndEvent{
	RunID:          "run-1",
	OutputMessages: []sigil.Message{sigil.AssistantTextMessage("Release is healthy")},
	ResponseModel:  "gpt-5",
	StopReason:     "stop",
})
```

## Streaming snippet

```go
_ = callbacks.OnRunStart(ctx, googleadk.RunStartEvent{
	RunID:     "run-stream",
	SessionID: "session-42",
	ModelName: "gemini-2.5-pro",
	Stream:    true,
	Prompts:   []string{"Stream migration status"},
})
callbacks.OnRunToken("run-stream", "step ")
callbacks.OnRunToken("run-stream", "complete")
_ = callbacks.OnRunEnd("run-stream", googleadk.RunEndEvent{RunID: "run-stream"})
```

## Conversation mapping

Precedence:

1. `ConversationID`
2. `SessionID`
3. `GroupID`
4. `ThreadID`
5. fallback `sigil:framework:google-adk:<run_id>`

## Tool lifecycle snippet

```go
_ = callbacks.OnToolStart(ctx, googleadk.ToolStartEvent{
	RunID:           "tool-1",
	SessionID:       "session-42",
	ToolCallID:      "call_lookup_customer",
	ToolName:        "lookup_customer",
	ToolType:        "function",
	ToolDescription: "Lookup customer profile",
	Arguments:       map[string]any{"customer_id": "42"},
})
_ = callbacks.OnToolEnd("tool-1", googleadk.ToolEndEvent{Result: map[string]any{"status": "ok"}})
```

`ToolCallID` and `ToolType` are optional, but passing them lets the framework tool span line up with the assistant `tool_call` message part and emit `gen_ai.tool.call.id` / `gen_ai.tool.type`.

## Troubleshooting

- Missing conversation grouping: pass stable session/conversation IDs from ADK context.
- Provider inferred as `custom`: set `Options.Provider` or `Options.ProviderResolver`.
- Always call `client.Shutdown(ctx)` in process teardown.
