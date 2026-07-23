# Grafana Agent Observability Go Provider Helper: OpenAI

This module maps official OpenAI Go SDK request/response payloads into typed agento11y `Generation` records for both Chat Completions and Responses APIs.

## Scope

- One-liner wrappers:
  - `ChatCompletionsNew(ctx, agento11yClient, provider, req, opts...)`
  - `ChatCompletionsNewStreaming(ctx, agento11yClient, provider, req, opts...)`
  - `ResponsesNew(ctx, agento11yClient, provider, req, opts...)`
  - `ResponsesNewStreaming(ctx, agento11yClient, provider, req, opts...)`
  - `EmbeddingsNew(ctx, agento11yClient, provider, req, opts...)`
- Mapper functions:
  - `ChatCompletionsFromRequestResponse(req, resp, opts...)`
  - `ChatCompletionsFromStream(req, summary, opts...)`
  - `ResponsesFromRequestResponse(req, resp, opts...)`
  - `ResponsesFromStream(req, summary, opts...)`
  - `EmbeddingsFromResponse(req, resp)`

## Integration styles

- Strict wrappers: use `ChatCompletionsNew*` / `ResponsesNew*` for one-call instrumentation.
- Manual instrumentation: use `agento11y.Client.StartGeneration` or `StartStreamingGeneration` and map strict OpenAI request/response payloads with `ChatCompletionsFrom*` or `ResponsesFrom*`.

## SDK

- Official SDK: `github.com/openai/openai-go/v3`

## Chat Completions Wrapper

```go
resp, err := openai.ChatCompletionsNew(ctx, agento11yClient, providerClient, req,
	openai.WithConversationID("conv-1"),
	openai.WithConversationTitle("Weather follow-up"),
	openai.WithAgentName("assistant-openai"),
	openai.WithAgentVersion("1.0.0"),
)
if err != nil {
	return err
}
_ = resp.Choices[0].Message.Content
```

## Responses Wrapper

```go
resp, err := openai.ResponsesNew(ctx, agento11yClient, providerClient, req,
	openai.WithConversationID("conv-1"),
	openai.WithConversationTitle("Weather follow-up"),
	openai.WithAgentName("assistant-openai"),
	openai.WithAgentVersion("1.0.0"),
)
if err != nil {
	return err
}
_ = resp.ID
```

## Embeddings Wrapper

```go
embedResp, err := openai.EmbeddingsNew(ctx, agento11yClient, providerClient, osdk.EmbeddingNewParams{
	Model: osdk.EmbeddingModel("text-embedding-3-small"),
	Input: osdk.EmbeddingNewParamsInputUnion{
		OfArrayOfStrings: []string{"hello", "world"},
	},
})
if err != nil {
	return err
}
_ = embedResp.Model
```

## Defer Pattern (explicit control)

```go
ctx, rec := agento11yClient.StartGeneration(ctx, agento11y.GenerationStart{
	ConversationID: "conv-9b2f",
	AgentName:      "assistant-openai",
	AgentVersion:   "1.0.0",
	Model:          agento11y.ModelRef{Provider: "openai", Name: "gpt-5"},
})
defer rec.End()

resp, err := openaiClient.Chat.Completions.New(ctx, req)
if err != nil {
	rec.SetCallError(err)
	return err
}

rec.SetResult(openai.ChatCompletionsFromRequestResponse(req, resp))
```

## Streaming Defer Pattern

```go
ctx, rec := agento11yClient.StartStreamingGeneration(ctx, agento11y.GenerationStart{
	Model: agento11y.ModelRef{Provider: "openai", Name: "gpt-5"},
})
defer rec.End()

stream := openaiClient.Responses.NewStreaming(ctx, req)
defer stream.Close()

summary := openai.ResponsesStreamSummary{}
for stream.Next() {
	summary.Events = append(summary.Events, stream.Current())
	// process event here
}
if err := stream.Err(); err != nil {
	rec.SetCallError(err)
	return err
}

rec.SetResult(openai.ResponsesFromStream(req, summary))
```

## Raw artifact policy

- Default: raw request/response/provider-event artifacts are OFF.
- Opt-in with `WithRawArtifacts()`.

## Tool result correlation

- Chat Completions tool messages and Responses function-call outputs preserve upstream call IDs in normalized `tool_result.tool_call_id`.
- Legacy Chat Completions `function` role messages do not expose a call ID; the mapper falls back to normalized `tool_result.name`.

## Live SDK examples

Real end-to-end examples using the actual OpenAI SDK (no fake provider calls) are in `sdk_example_test.go`.

```bash
AGENTO11Y_RUN_LIVE_EXAMPLES=1 OPENAI_API_KEY=... go test -run Example_withAgento11y -v
```
