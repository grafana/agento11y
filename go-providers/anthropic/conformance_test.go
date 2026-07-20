package anthropic

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	asdk "github.com/anthropics/anthropic-sdk-go"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/go/agento11y/testkit"
)

const anthropicSpanErrorCategory = "error.category"

func TestConformance_AnthropicSyncMapping(t *testing.T) {
	env := testkit.NewEnv(t)

	req := testRequest()
	resp := &asdk.BetaMessage{
		ID:         "msg_conformance_sync",
		Model:      asdk.Model("claude-sonnet-4-5"),
		StopReason: asdk.BetaStopReasonToolUse,
		Content: []asdk.BetaContentBlockUnion{
			mustUnmarshalBetaContentBlockUnion(t, `{"type":"thinking","thinking":"need weather tool","signature":"sig"}`),
			mustUnmarshalBetaContentBlockUnion(t, `{"type":"text","text":"Checking weather."}`),
			mustUnmarshalBetaContentBlockUnion(t, `{"type":"tool_use","id":"toolu_sync","name":"weather","input":{"city":"Paris"}}`),
		},
		Usage: asdk.BetaUsage{
			InputTokens:              120,
			OutputTokens:             42,
			CacheReadInputTokens:     30,
			CacheCreationInputTokens: 10,
			ServerToolUse: asdk.BetaServerToolUsage{
				WebSearchRequests: 2,
				WebFetchRequests:  1,
			},
		},
	}
	start := agento11y.GenerationStart{
		ConversationID:    "conv-anthropic-sync",
		ConversationTitle: "Anthropic sync",
		AgentName:         "agent-anthropic",
		AgentVersion:      "v-anthropic",
		Model:             agento11y.ModelRef{Provider: "anthropic", Name: req.Model},
	}

	generation, err := FromRequestResponse(
		req,
		resp,
		WithConversationID(start.ConversationID),
		WithConversationTitle(start.ConversationTitle),
		WithAgentName(start.AgentName),
		WithAgentVersion(start.AgentVersion),
		WithTag("tenant", "t-anthropic"),
	)
	testkit.RecordGeneration(t, env, start, generation, err)
	env.Shutdown(t)

	exported := env.SingleGenerationJSON(t)

	if got := testkit.StringValue(t, exported, "mode"); got != "GENERATION_MODE_SYNC" {
		t.Fatalf("unexpected mode: got %q want %q\n%s", got, "GENERATION_MODE_SYNC", testkit.DebugJSON(exported))
	}
	if got := testkit.StringValue(t, exported, "stop_reason"); got != "tool_use" {
		t.Fatalf("unexpected stop_reason: got %q want %q", got, "tool_use")
	}
	if got := testkit.StringValue(t, exported, "output", 0, "parts", 0, "thinking"); got != "need weather tool" {
		t.Fatalf("unexpected thinking part: got %q want %q", got, "need weather tool")
	}
	if got := testkit.StringValue(t, exported, "output", 0, "parts", 1, "text"); got != "Checking weather." {
		t.Fatalf("unexpected text part: got %q want %q", got, "Checking weather.")
	}
	if got := testkit.StringValue(t, exported, "output", 0, "parts", 2, "tool_call", "name"); got != "weather" {
		t.Fatalf("unexpected tool_call.name: got %q want %q", got, "weather")
	}
	if got := testkit.StringValue(t, exported, "input", 2, "role"); got != "MESSAGE_ROLE_TOOL" {
		t.Fatalf("unexpected tool input role: got %q want %q", got, "MESSAGE_ROLE_TOOL")
	}
	if got := testkit.StringValue(t, exported, "usage", "cache_read_input_tokens"); got != "30" {
		t.Fatalf("unexpected usage.cache_read_input_tokens: got %q want %q", got, "30")
	}
	if got := testkit.StringValue(t, exported, "usage", "cache_write_input_tokens"); got != "10" {
		t.Fatalf("unexpected usage.cache_write_input_tokens: got %q want %q", got, "10")
	}
	if got := testkit.FloatValue(t, exported, "metadata", "agento11y.gen_ai.usage.server_tool_use.total_requests"); got != 3 {
		t.Fatalf("unexpected server tool total requests: got %v want %v", got, float64(3))
	}
}

func TestConformance_AnthropicStreamMapping(t *testing.T) {
	env := testkit.NewEnv(t)

	req := testRequest()
	summary := StreamSummary{
		FirstChunkAt: time.Unix(1_741_780_100, 0).UTC(),
		Events: []asdk.BetaRawMessageStreamEventUnion{
			{
				Type: "message_start",
				Message: asdk.BetaMessage{
					ID:    "msg_conformance_stream",
					Model: asdk.Model("claude-sonnet-4-5"),
				},
			},
			{
				Type:         "content_block_start",
				Index:        0,
				ContentBlock: mustUnmarshalBetaRawContentBlockStartEventContentBlockUnion(t, `{"type":"thinking","thinking":""}`),
			},
			{
				Type:  "content_block_delta",
				Index: 0,
				Delta: asdk.BetaRawMessageStreamEventUnionDelta{Thinking: "need weather"},
			},
			{
				Type:         "content_block_start",
				Index:        1,
				ContentBlock: mustUnmarshalBetaRawContentBlockStartEventContentBlockUnion(t, `{"type":"text","text":""}`),
			},
			{
				Type:  "content_block_delta",
				Index: 1,
				Delta: asdk.BetaRawMessageStreamEventUnionDelta{Text: "Checking "},
			},
			{
				Type:  "content_block_delta",
				Index: 1,
				Delta: asdk.BetaRawMessageStreamEventUnionDelta{Text: "weather"},
			},
			{
				Type:         "content_block_start",
				Index:        2,
				ContentBlock: mustUnmarshalBetaRawContentBlockStartEventContentBlockUnion(t, `{"type":"tool_use","id":"toolu_stream","name":"weather","input":{}}`),
			},
			{
				Type:  "content_block_delta",
				Index: 2,
				Delta: asdk.BetaRawMessageStreamEventUnionDelta{PartialJSON: `{"city":"Paris"}`},
			},
			{
				Type: "message_delta",
				Delta: asdk.BetaRawMessageStreamEventUnionDelta{
					StopReason: asdk.BetaStopReasonToolUse,
				},
				Usage: asdk.BetaMessageDeltaUsage{
					InputTokens:  80,
					OutputTokens: 25,
				},
			},
		},
	}
	start := agento11y.GenerationStart{
		ConversationID: "conv-anthropic-stream",
		AgentName:      "agent-anthropic-stream",
		AgentVersion:   "v-anthropic-stream",
		Model:          agento11y.ModelRef{Provider: "anthropic", Name: req.Model},
	}

	generation, err := FromStream(
		req,
		summary,
		WithConversationID(start.ConversationID),
		WithAgentName(start.AgentName),
		WithAgentVersion(start.AgentVersion),
	)
	testkit.RecordStreamingGeneration(t, env, start, summary.FirstChunkAt, generation, err)
	env.Shutdown(t)

	exported := env.SingleGenerationJSON(t)

	if got := testkit.StringValue(t, exported, "mode"); got != "GENERATION_MODE_STREAM" {
		t.Fatalf("unexpected mode: got %q want %q\n%s", got, "GENERATION_MODE_STREAM", testkit.DebugJSON(exported))
	}
	if got := testkit.StringValue(t, exported, "response_id"); got != "msg_conformance_stream" {
		t.Fatalf("unexpected response_id: got %q want %q", got, "msg_conformance_stream")
	}
	if got := testkit.StringValue(t, exported, "stop_reason"); got != "tool_use" {
		t.Fatalf("unexpected stop_reason: got %q want %q", got, "tool_use")
	}
	if got := testkit.StringValue(t, exported, "output", 0, "parts", 0, "thinking"); got != "need weather" {
		t.Fatalf("unexpected streamed thinking part: got %q want %q", got, "need weather")
	}
	if got := testkit.StringValue(t, exported, "output", 0, "parts", 1, "text"); got != "Checking weather" {
		t.Fatalf("unexpected streamed text part: got %q want %q", got, "Checking weather")
	}
	if got := testkit.StringValue(t, exported, "output", 0, "parts", 2, "tool_call", "name"); got != "weather" {
		t.Fatalf("unexpected streamed tool_call.name: got %q want %q", got, "weather")
	}
	if got := testkit.StringValue(t, exported, "usage", "total_tokens"); got != "105" {
		t.Fatalf("unexpected streamed usage.total_tokens: got %q want %q", got, "105")
	}
}

func TestConformance_AnthropicErrorMapping(t *testing.T) {
	env := testkit.NewEnv(t)

	callErr := &asdk.Error{
		StatusCode: http.StatusTooManyRequests,
		Request:    &http.Request{Method: http.MethodPost, URL: mustAnthropicURL(t, "https://api.anthropic.com/v1/messages")},
		Response:   &http.Response{StatusCode: http.StatusTooManyRequests, Status: "429 Too Many Requests"},
	}
	testkit.RecordCallError(t, env, agento11y.GenerationStart{
		Model: agento11y.ModelRef{Provider: "anthropic", Name: "claude-sonnet-4-5"},
	}, callErr)

	span := testkit.FindSpan(t, env.Spans.Ended(), "generateText claude-sonnet-4-5")
	attrs := testkit.SpanAttributes(span)
	if got := attrs[anthropicSpanErrorCategory].AsString(); got != "rate_limit" {
		t.Fatalf("unexpected error.category: got %q want %q", got, "rate_limit")
	}

	env.Shutdown(t)
	exported := env.SingleGenerationJSON(t)
	callError := testkit.StringValue(t, exported, "call_error")
	if !strings.Contains(callError, "429") {
		t.Fatalf("expected call_error to include status code, got %q", callError)
	}
}

func mustAnthropicURL(t testing.TB, raw string) *url.URL {
	t.Helper()

	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse url %q: %v", raw, err)
	}
	return parsed
}

func TestConformance_MessageSyncNormalization(t *testing.T) {
	req := testRequest()
	resp := &asdk.BetaMessage{
		ID:         "msg_1",
		Model:      asdk.Model("claude-sonnet-4-5"),
		StopReason: asdk.BetaStopReasonEndTurn,
		Content: []asdk.BetaContentBlockUnion{
			{Type: "text", Text: "It's 18C and sunny."},
			{Type: "thinking", Thinking: "answer done"},
			mustUnmarshalBetaContentBlockUnion(t, `{"type":"tool_use","id":"toolu_2","name":"weather","input":{"city":"Paris"}}`),
		},
		Usage: asdk.BetaUsage{
			InputTokens:              120,
			OutputTokens:             42,
			CacheReadInputTokens:     30,
			CacheCreationInputTokens: 10,
		},
	}

	generation, err := FromRequestResponse(req, resp,
		WithConversationID("conv-anthropic-sync"),
		WithConversationTitle("Paris weather"),
		WithAgentName("agent-anthropic"),
		WithAgentVersion("v-anthropic"),
		WithTag("tenant", "t-123"),
		WithRawArtifacts(),
	)
	if err != nil {
		t.Fatalf("anthropic sync mapping: %v", err)
	}

	if generation.Model.Provider != "anthropic" || generation.Model.Name != "claude-sonnet-4-5" {
		t.Fatalf("unexpected model mapping: %#v", generation.Model)
	}
	if generation.ConversationID != "conv-anthropic-sync" || generation.ConversationTitle != "Paris weather" {
		t.Fatalf("unexpected conversation mapping: %#v", generation)
	}
	if generation.AgentName != "agent-anthropic" || generation.AgentVersion != "v-anthropic" {
		t.Fatalf("unexpected agent mapping: name=%q version=%q", generation.AgentName, generation.AgentVersion)
	}
	if generation.ResponseID != "msg_1" || generation.ResponseModel != "claude-sonnet-4-5" {
		t.Fatalf("unexpected response mapping: id=%q model=%q", generation.ResponseID, generation.ResponseModel)
	}
	if generation.StopReason != "end_turn" {
		t.Fatalf("unexpected stop reason: %q", generation.StopReason)
	}
	if generation.Usage.TotalTokens != 162 || generation.Usage.CacheReadInputTokens != 30 || generation.Usage.CacheWriteInputTokens != 10 {
		t.Fatalf("unexpected usage mapping: %#v", generation.Usage)
	}
	if generation.ThinkingEnabled == nil || !*generation.ThinkingEnabled {
		t.Fatalf("expected thinking enabled true, got %v", generation.ThinkingEnabled)
	}
	if len(generation.Output) != 1 || len(generation.Output[0].Parts) != 3 {
		t.Fatalf("expected text + thinking + tool call output, got %#v", generation.Output)
	}
	if generation.Output[0].Parts[0].Kind != agento11y.PartKindText || generation.Output[0].Parts[0].Text != "It's 18C and sunny." {
		t.Fatalf("unexpected text output: %#v", generation.Output[0].Parts[0])
	}
	if generation.Output[0].Parts[1].Kind != agento11y.PartKindThinking || generation.Output[0].Parts[1].Thinking != "answer done" {
		t.Fatalf("unexpected thinking output: %#v", generation.Output[0].Parts[1])
	}
	if generation.Output[0].Parts[2].Kind != agento11y.PartKindToolCall {
		t.Fatalf("expected tool call output, got %#v", generation.Output[0].Parts[2])
	}
	if generation.Output[0].Parts[2].ToolCall.ID != "toolu_2" || generation.Output[0].Parts[2].ToolCall.Name != "weather" {
		t.Fatalf("unexpected tool call mapping: %#v", generation.Output[0].Parts[2].ToolCall)
	}
	if generation.Tags["tenant"] != "t-123" {
		t.Fatalf("expected tenant tag")
	}
	requireAnthropicArtifactKinds(t, generation.Artifacts,
		agento11y.ArtifactKindRequest,
		agento11y.ArtifactKindResponse,
		agento11y.ArtifactKindTools,
	)
}

func TestConformance_MessageStreamNormalization(t *testing.T) {
	req := testRequest()
	summary := StreamSummary{
		Events: []asdk.BetaRawMessageStreamEventUnion{
			{
				Type: "message_start",
				Message: asdk.BetaMessage{
					ID:    "msg_delta_1",
					Model: asdk.Model("claude-sonnet-4-5"),
				},
			},
			{
				Type:  "content_block_start",
				Index: 0,
				ContentBlock: asdk.BetaRawContentBlockStartEventContentBlockUnion{
					Type: "thinking",
				},
			},
			{
				Type:  "content_block_delta",
				Index: 0,
				Delta: asdk.BetaRawMessageStreamEventUnionDelta{Thinking: "let me "},
			},
			{
				Type:  "content_block_delta",
				Index: 0,
				Delta: asdk.BetaRawMessageStreamEventUnionDelta{Thinking: "think about this"},
			},
			{
				Type:  "content_block_start",
				Index: 1,
				ContentBlock: asdk.BetaRawContentBlockStartEventContentBlockUnion{
					Type: "text",
				},
			},
			{
				Type:  "content_block_delta",
				Index: 1,
				Delta: asdk.BetaRawMessageStreamEventUnionDelta{Text: "Hello, "},
			},
			{
				Type:  "content_block_delta",
				Index: 1,
				Delta: asdk.BetaRawMessageStreamEventUnionDelta{Text: "world!"},
			},
			{
				Type:  "content_block_start",
				Index: 2,
				ContentBlock: asdk.BetaRawContentBlockStartEventContentBlockUnion{
					Type:  "tool_use",
					ID:    "toolu_1",
					Name:  "weather",
					Input: map[string]any{},
				},
			},
			{
				Type:  "content_block_delta",
				Index: 2,
				Delta: asdk.BetaRawMessageStreamEventUnionDelta{PartialJSON: `{"city"`},
			},
			{
				Type:  "content_block_delta",
				Index: 2,
				Delta: asdk.BetaRawMessageStreamEventUnionDelta{PartialJSON: `:"Berlin"}`},
			},
			{
				Type: "message_delta",
				Delta: asdk.BetaRawMessageStreamEventUnionDelta{
					StopReason: asdk.BetaStopReasonToolUse,
				},
				Usage: asdk.BetaMessageDeltaUsage{
					InputTokens:  100,
					OutputTokens: 50,
				},
			},
		},
	}

	generation, err := FromStream(req, summary,
		WithConversationID("conv-anthropic-stream"),
		WithAgentName("agent-anthropic-stream"),
		WithAgentVersion("v-anthropic-stream"),
		WithRawArtifacts(),
	)
	if err != nil {
		t.Fatalf("anthropic stream mapping: %v", err)
	}

	if generation.ConversationID != "conv-anthropic-stream" || generation.AgentName != "agent-anthropic-stream" || generation.AgentVersion != "v-anthropic-stream" {
		t.Fatalf("unexpected identity mapping: %#v", generation)
	}
	if generation.ResponseID != "msg_delta_1" || generation.ResponseModel != "claude-sonnet-4-5" {
		t.Fatalf("unexpected response mapping: id=%q model=%q", generation.ResponseID, generation.ResponseModel)
	}
	if generation.StopReason != "tool_use" {
		t.Fatalf("unexpected stop reason: %q", generation.StopReason)
	}
	if generation.Usage.TotalTokens != 150 {
		t.Fatalf("unexpected usage mapping: %#v", generation.Usage)
	}
	if len(generation.Output) != 1 || len(generation.Output[0].Parts) != 3 {
		t.Fatalf("expected thinking + text + tool call output, got %#v", generation.Output)
	}
	if generation.Output[0].Parts[0].Kind != agento11y.PartKindThinking || generation.Output[0].Parts[0].Thinking != "let me think about this" {
		t.Fatalf("unexpected thinking output: %#v", generation.Output[0].Parts[0])
	}
	if generation.Output[0].Parts[1].Kind != agento11y.PartKindText || generation.Output[0].Parts[1].Text != "Hello, world!" {
		t.Fatalf("unexpected text output: %#v", generation.Output[0].Parts[1])
	}
	if generation.Output[0].Parts[2].Kind != agento11y.PartKindToolCall {
		t.Fatalf("expected tool call output, got %#v", generation.Output[0].Parts[2])
	}
	if string(generation.Output[0].Parts[2].ToolCall.InputJSON) != `{"city":"Berlin"}` {
		t.Fatalf("unexpected streamed tool input: %q", string(generation.Output[0].Parts[2].ToolCall.InputJSON))
	}
	requireAnthropicArtifactKinds(t, generation.Artifacts,
		agento11y.ArtifactKindRequest,
		agento11y.ArtifactKindTools,
		agento11y.ArtifactKindProviderEvent,
	)
}

func TestConformance_AnthropicMapperValidationErrors(t *testing.T) {
	if _, err := FromRequestResponse(testRequest(), nil); err == nil || err.Error() != "response is required" {
		t.Fatalf("expected explicit response error, got %v", err)
	}
	if _, err := FromStream(testRequest(), StreamSummary{}); err == nil || err.Error() != "stream summary has no events and no final message" {
		t.Fatalf("expected explicit stream error, got %v", err)
	}

	_, err := FromRequestResponse(
		testRequest(),
		&asdk.BetaMessage{Model: asdk.Model("claude-sonnet-4-5")},
		WithProviderName(""),
	)
	if err == nil || err.Error() != "generation.model.provider is required" {
		t.Fatalf("expected explicit validation error for invalid provider mapping, got %v", err)
	}
}

func TestConformance_EmbeddingSupportStatus(t *testing.T) {
	err := CheckEmbeddingsSupport()
	if err == nil {
		t.Fatalf("expected Anthropic embeddings to remain unsupported")
	}
	if !errors.Is(err, ErrEmbeddingsUnsupported) {
		t.Fatalf("expected ErrEmbeddingsUnsupported, got %v", err)
	}
}

func requireAnthropicArtifactKinds(t *testing.T, artifacts []agento11y.Artifact, want ...agento11y.ArtifactKind) {
	t.Helper()

	if len(artifacts) != len(want) {
		t.Fatalf("expected %d artifacts, got %d", len(want), len(artifacts))
	}
	for i, kind := range want {
		if artifacts[i].Kind != kind {
			t.Fatalf("artifact %d kind mismatch: got %q want %q", i, artifacts[i].Kind, kind)
		}
	}
}
