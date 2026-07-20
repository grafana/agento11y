# Grafana Sigil Go Provider Helper: Anthropic

This module maps Anthropic Messages SDK request/response payloads into the
typed Sigil `Generation` model.

## Embeddings support

This helper currently supports Anthropic Messages APIs only. Native Anthropic embeddings endpoints are not available in the official SDK/API surface used in this repository.

Use the exported support gate when you need a deterministic capability check:

```go
if err := anthropic.CheckEmbeddingsSupport(); err != nil {
	return err
}
```

## Scope
- One-liner wrappers:
  - `Message(ctx, agento11yClient, provider, req, opts...)`
  - `MessageStream(ctx, agento11yClient, provider, req, opts...)`
- Embedding capability gate:
  - `CheckEmbeddingsSupport()`
- Request/response mapper:
  - `FromRequestResponse(req, resp, opts...)`
- Stream mapper:
  - `FromStream(req, summary, opts...)`
- Typed artifacts:
  - `request`
  - `response`
  - `tools`
  - `provider_event` (stream events)

## SDK
- Official SDK: `github.com/anthropics/anthropic-sdk-go`

## Wrapper (one-liner)
```go
resp, err := anthropic.Message(ctx, agento11yClient, providerClient, req,
	anthropic.WithConversationID("conv-1"),
	anthropic.WithConversationTitle("Weather follow-up"),
	anthropic.WithAgentName("assistant-anthropic"),
	anthropic.WithAgentVersion("1.0.0"),
)
if err != nil {
	return err
}
_ = resp.Content[0].Text
```

## Defer Pattern (full control)
```go
ctx, rec := agento11yClient.StartGeneration(ctx, agento11y.GenerationStart{
	ConversationID: "conv-9b2f",
	AgentName:      "assistant-anthropic",
	AgentVersion:   "1.0.0",
	Model:          agento11y.ModelRef{Provider: "anthropic", Name: "claude-sonnet-4-5"},
})
defer rec.End()

resp, err := anthropicClient.Beta.Messages.New(ctx, req)
if err != nil {
	rec.SetCallError(err)
	return err
}

rec.SetResult(anthropic.FromRequestResponse(req, resp))
```

## Streaming Defer Pattern
```go
ctx, rec := agento11yClient.StartStreamingGeneration(ctx, agento11y.GenerationStart{
	Model: agento11y.ModelRef{Provider: "anthropic", Name: "claude-sonnet-4-5"},
})
defer rec.End()

stream := anthropicClient.Beta.Messages.NewStreaming(ctx, req)
defer stream.Close()

summary := anthropic.StreamSummary{}
for stream.Next() {
	event := stream.Current()
	summary.Events = append(summary.Events, event)
	// process event here
}
if err := stream.Err(); err != nil {
	rec.SetCallError(err)
	return err
}

rec.SetResult(anthropic.FromStream(req, summary))
```

## Live SDK examples
Real end-to-end examples using the actual Anthropic SDK (no fake provider calls) are in:
- `sdk_example_test.go`

Run them with:
```bash
SIGIL_RUN_LIVE_EXAMPLES=1 ANTHROPIC_API_KEY=... go test -run Example_withSigil -v
```

## Provider metadata mapping

In addition to normalized usage fields, Anthropic server-tool counters are mapped into Sigil metadata when present:

- `agento11y.gen_ai.usage.server_tool_use.web_search_requests`
- `agento11y.gen_ai.usage.server_tool_use.web_fetch_requests`
- `agento11y.gen_ai.usage.server_tool_use.total_requests`

Anthropic tool `defer_loading` is mapped to Sigil `Generation.Tools[].Deferred`.

## Tool result correlation

- Anthropic `tool_result` and server-tool result blocks preserve `tool_use_id` in normalized `tool_result.tool_call_id`.
