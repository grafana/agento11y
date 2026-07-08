package sigil_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	sigilv1 "github.com/grafana/sigil-sdk/go/proto/sigil/v1"
	sigil "github.com/grafana/sigil-sdk/go/sigil"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

func TestConformance_FullGenerationRoundtrip(t *testing.T) {
	startedAt := time.Date(2026, time.March, 12, 11, 0, 0, 0, time.UTC)
	completedAt := startedAt.Add(3 * time.Second)

	requestArtifact, err := sigil.NewJSONArtifact(sigil.ArtifactKindRequest, "request", map[string]any{
		"messages": 1,
		"tools":    1,
	})
	if err != nil {
		t.Fatalf("build request artifact: %v", err)
	}
	requestArtifact.RecordID = "rec-request-1"
	requestArtifact.URI = "sigil://artifact/request-1"

	responseArtifact, err := sigil.NewJSONArtifact(sigil.ArtifactKindResponse, "response", map[string]any{
		"response_id": "msg_1",
		"status":      "ok",
	})
	if err != nil {
		t.Fatalf("build response artifact: %v", err)
	}
	responseArtifact.RecordID = "rec-response-1"
	responseArtifact.URI = "sigil://artifact/response-1"

	env := newConformanceEnv(t, withConformanceConfig(func(cfg *sigil.Config) {
		cfg.Now = func() time.Time { return completedAt }
	}))

	_, recorder := env.Client.StartGeneration(context.Background(), sigil.GenerationStart{
		ID:                "gen-full-roundtrip",
		ConversationID:    "conv-full-roundtrip",
		ConversationTitle: "Weather follow-up",
		UserID:            "user-42",
		AgentName:         "assistant-anthropic",
		AgentVersion:      "1.0.0",
		Model: sigil.ModelRef{
			Provider: "anthropic",
			Name:     "claude-sonnet-4-5",
		},
		SystemPrompt: "Answer with a brief explanation and cite the tool result.",
		Tools: []sigil.ToolDefinition{
			{
				Name:        "weather.lookup",
				Description: "Look up historical weather by city and date",
				Type:        "function",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"},"date":{"type":"string"}},"required":["city","date"]}`),
				Deferred:    true,
			},
		},
		MaxTokens:       int64Ptr(1024),
		Temperature:     float64Ptr(0.7),
		TopP:            float64Ptr(0.9),
		ToolChoice:      stringPtr("required"),
		ThinkingEnabled: boolPtr(true),
		Tags: map[string]string{
			"env":       "prod",
			"seed_only": "seed",
			"shared":    "seed",
		},
		Metadata: map[string]any{
			spanAttrRequestThinkingBudget: int64(2048),
			"request_only":                "seed-value",
			"shared":                      "seed",
			"nested":                      map[string]any{"phase": "seed"},
		},
		StartedAt: startedAt,
	})
	recorder.SetResult(sigil.Generation{
		ResponseID:    "msg_1",
		ResponseModel: "claude-sonnet-4-5-20260312",
		Input: []sigil.Message{
			{
				Role: sigil.RoleUser,
				Name: "customer",
				Parts: []sigil.Part{
					sigil.TextPart("Summarize yesterday's Paris weather and explain the spikes."),
				},
			},
		},
		Output: []sigil.Message{
			{
				Role: sigil.RoleAssistant,
				Name: "assistant",
				Parts: []sigil.Part{
					{
						Kind:     sigil.PartKindThinking,
						Thinking: "Need the weather tool output before the final answer.",
						Metadata: sigil.PartMetadata{ProviderType: "thinking"},
					},
					{
						Kind: sigil.PartKindToolCall,
						ToolCall: &sigil.ToolCall{
							ID:        "call-weather-1",
							Name:      "weather.lookup",
							InputJSON: json.RawMessage(`{"city":"Paris","date":"2026-03-11"}`),
						},
						Metadata: sigil.PartMetadata{ProviderType: "tool_use"},
					},
				},
			},
			{
				Role: sigil.RoleTool,
				Name: "weather.lookup",
				Parts: []sigil.Part{
					{
						Kind: sigil.PartKindToolResult,
						ToolResult: &sigil.ToolResult{
							ToolCallID:  "call-weather-1",
							Name:        "weather.lookup",
							Content:     "22C with a late-afternoon drop",
							ContentJSON: json.RawMessage(`{"high_c":22,"trend":"late drop"}`),
						},
					},
				},
			},
			{
				Role: sigil.RoleAssistant,
				Name: "assistant",
				Parts: []sigil.Part{
					sigil.TextPart("Paris peaked at 22C before a late drop as cloud cover moved in."),
				},
			},
		},
		Usage: sigil.TokenUsage{
			InputTokens:           120,
			OutputTokens:          42,
			TotalTokens:           162,
			CacheReadInputTokens:  30,
			CacheWriteInputTokens: 4,
			ReasoningTokens:       9,
		},
		StopReason: "end_turn",
		Tags: map[string]string{
			"shared":      "result",
			"result_only": "assistant",
		},
		Metadata: map[string]any{
			"shared":      "result",
			"result_only": "assistant",
			"nested":      map[string]any{"phase": "result"},
			"quality":     true,
		},
		Artifacts: []sigil.Artifact{requestArtifact, responseArtifact},
	}, nil)
	recorder.End()
	if err := recorder.Err(); err != nil {
		t.Fatalf("record full generation roundtrip: %v", err)
	}

	metrics := env.CollectMetrics(t)
	env.Shutdown(t)

	span := findSpan(t, env.Spans.Ended(), conformanceOperationName)
	if got := span.Name(); got != "generateText claude-sonnet-4-5" {
		t.Fatalf("unexpected span name: got %q want %q", got, "generateText claude-sonnet-4-5")
	}
	if got := span.SpanKind(); got != trace.SpanKindClient {
		t.Fatalf("unexpected span kind: got %v want %v", got, trace.SpanKindClient)
	}
	if got := span.Status().Code; got != codes.Ok {
		t.Fatalf("unexpected span status: got %v want %v", got, codes.Ok)
	}

	attrs := spanAttrs(span)
	requireSpanAttr(t, attrs, spanAttrGenerationID, "gen-full-roundtrip")
	requireSpanAttr(t, attrs, spanAttrConversationID, "conv-full-roundtrip")
	requireSpanAttr(t, attrs, spanAttrConversationTitle, "Weather follow-up")
	requireSpanAttr(t, attrs, spanAttrUserID, "user-42")
	requireSpanAttr(t, attrs, spanAttrAgentName, "assistant-anthropic")
	requireSpanAttr(t, attrs, spanAttrAgentVersion, "1.0.0")
	requireSpanAttr(t, attrs, spanAttrProviderName, "anthropic")
	requireSpanAttr(t, attrs, spanAttrRequestModel, "claude-sonnet-4-5")
	requireSpanAttr(t, attrs, spanAttrResponseID, "msg_1")
	requireSpanAttr(t, attrs, spanAttrResponseModel, "claude-sonnet-4-5-20260312")
	requireSpanAttr(t, attrs, sdkMetadataKeyName, "sdk-go")
	requireSpanInt64Attr(t, attrs, spanAttrRequestMaxTokens, 1024)
	requireSpanFloat64Attr(t, attrs, spanAttrRequestTemperature, 0.7)
	requireSpanFloat64Attr(t, attrs, spanAttrRequestTopP, 0.9)
	requireSpanAttr(t, attrs, spanAttrRequestToolChoice, "required")
	requireSpanBoolAttr(t, attrs, spanAttrRequestThinkingEnabled, true)
	requireSpanInt64Attr(t, attrs, spanAttrRequestThinkingBudget, 2048)
	requireSpanStringSliceAttr(t, attrs, spanAttrFinishReasons, []string{"end_turn"})
	requireSpanInt64Attr(t, attrs, spanAttrInputTokens, 120)
	requireSpanInt64Attr(t, attrs, spanAttrOutputTokens, 42)
	requireSpanInt64Attr(t, attrs, spanAttrCacheReadTokens, 30)
	requireSpanInt64Attr(t, attrs, spanAttrCacheWriteTokens, 4)
	requireSpanInt64Attr(t, attrs, spanAttrReasoningTokens, 9)

	duration := findHistogram[float64](t, metrics, metricOperationDuration)
	durationPoint := findHistogramPoint(t, duration, map[string]string{
		spanAttrOperationName: conformanceOperationName,
		spanAttrProviderName:  "anthropic",
		spanAttrRequestModel:  "claude-sonnet-4-5",
		spanAttrAgentName:     "assistant-anthropic",
		spanAttrAgentVersion:  "1.0.0",
		spanAttrErrorType:     "",
		spanAttrErrorCategory: "",
	})
	if durationPoint.Count != 1 {
		t.Fatalf("unexpected %s count: got %d want %d", metricOperationDuration, durationPoint.Count, 1)
	}
	if durationPoint.Sum != 3 {
		t.Fatalf("unexpected %s sum: got %v want %v", metricOperationDuration, durationPoint.Sum, 3.0)
	}

	tokenUsage := findHistogram[int64](t, metrics, metricTokenUsage)
	for tokenType, want := range map[string]int64{
		metricTokenTypeInput:      120,
		metricTokenTypeOutput:     42,
		metricTokenTypeCacheRead:  30,
		metricTokenTypeCacheWrite: 4,
		metricTokenTypeReasoning:  9,
	} {
		point := findHistogramPoint(t, tokenUsage, map[string]string{
			spanAttrOperationName: conformanceOperationName,
			spanAttrProviderName:  "anthropic",
			spanAttrRequestModel:  "claude-sonnet-4-5",
			spanAttrAgentName:     "assistant-anthropic",
			spanAttrAgentVersion:  "1.0.0",
			metricAttrTokenType:   tokenType,
		})
		if point.Count != 1 {
			t.Fatalf("unexpected %s count for token type %q: got %d want %d", metricTokenUsage, tokenType, point.Count, 1)
		}
		if point.Sum != want {
			t.Fatalf("unexpected %s sum for token type %q: got %d want %d", metricTokenUsage, tokenType, point.Sum, want)
		}
	}

	toolCalls := findHistogram[int64](t, metrics, metricToolCallsPerOperation)
	toolPoint := findHistogramPoint(t, toolCalls, map[string]string{
		spanAttrProviderName: "anthropic",
		spanAttrRequestModel: "claude-sonnet-4-5",
		spanAttrAgentName:    "assistant-anthropic",
		spanAttrAgentVersion: "1.0.0",
	})
	if toolPoint.Count != 1 {
		t.Fatalf("unexpected %s count: got %d want %d", metricToolCallsPerOperation, toolPoint.Count, 1)
	}
	if toolPoint.Sum != 1 {
		t.Fatalf("unexpected %s sum: got %d want %d", metricToolCallsPerOperation, toolPoint.Sum, 1)
	}
	requireNoHistogram(t, metrics, metricTimeToFirstToken)

	generation := env.Ingest.SingleGeneration(t)
	if got := generation.GetId(); got != "gen-full-roundtrip" {
		t.Fatalf("unexpected proto generation id: got %q want %q", got, "gen-full-roundtrip")
	}
	if got := generation.GetConversationId(); got != "conv-full-roundtrip" {
		t.Fatalf("unexpected proto conversation id: got %q want %q", got, "conv-full-roundtrip")
	}
	if got := generation.GetOperationName(); got != conformanceOperationName {
		t.Fatalf("unexpected proto operation name: got %q want %q", got, conformanceOperationName)
	}
	if got := generation.GetMode(); got != sigilv1.GenerationMode_GENERATION_MODE_SYNC {
		t.Fatalf("unexpected proto mode: got %s want %s", got, sigilv1.GenerationMode_GENERATION_MODE_SYNC)
	}
	if got := generation.GetTraceId(); got != span.SpanContext().TraceID().String() {
		t.Fatalf("unexpected proto trace_id: got %q want %q", got, span.SpanContext().TraceID().String())
	}
	if got := generation.GetSpanId(); got != span.SpanContext().SpanID().String() {
		t.Fatalf("unexpected proto span_id: got %q want %q", got, span.SpanContext().SpanID().String())
	}
	if got := generation.GetAgentName(); got != "assistant-anthropic" {
		t.Fatalf("unexpected proto agent_name: got %q want %q", got, "assistant-anthropic")
	}
	if got := generation.GetAgentVersion(); got != "1.0.0" {
		t.Fatalf("unexpected proto agent_version: got %q want %q", got, "1.0.0")
	}
	if got := generation.GetModel().GetProvider(); got != "anthropic" {
		t.Fatalf("unexpected proto model provider: got %q want %q", got, "anthropic")
	}
	if got := generation.GetModel().GetName(); got != "claude-sonnet-4-5" {
		t.Fatalf("unexpected proto model name: got %q want %q", got, "claude-sonnet-4-5")
	}
	if got := generation.GetResponseId(); got != "msg_1" {
		t.Fatalf("unexpected proto response_id: got %q want %q", got, "msg_1")
	}
	if got := generation.GetResponseModel(); got != "claude-sonnet-4-5-20260312" {
		t.Fatalf("unexpected proto response_model: got %q want %q", got, "claude-sonnet-4-5-20260312")
	}
	if got := generation.GetSystemPrompt(); got != "Answer with a brief explanation and cite the tool result." {
		t.Fatalf("unexpected proto system_prompt: got %q", got)
	}
	if got := generation.GetStopReason(); got != "end_turn" {
		t.Fatalf("unexpected proto stop_reason: got %q want %q", got, "end_turn")
	}
	if got := generation.GetMaxTokens(); got != 1024 {
		t.Fatalf("unexpected proto max_tokens: got %d want %d", got, 1024)
	}
	if got := generation.GetTemperature(); got != 0.7 {
		t.Fatalf("unexpected proto temperature: got %v want %v", got, 0.7)
	}
	if got := generation.GetTopP(); got != 0.9 {
		t.Fatalf("unexpected proto top_p: got %v want %v", got, 0.9)
	}
	if got := generation.GetToolChoice(); got != "required" {
		t.Fatalf("unexpected proto tool_choice: got %q want %q", got, "required")
	}
	if got := generation.GetThinkingEnabled(); !got {
		t.Fatalf("unexpected proto thinking_enabled: got %t want %t", got, true)
	}
	if got := generation.GetCallError(); got != "" {
		t.Fatalf("expected empty proto call_error, got %q", got)
	}

	if got := generation.GetStartedAt().AsTime(); !got.Equal(startedAt) {
		t.Fatalf("unexpected proto started_at: got %s want %s", got, startedAt)
	}
	if got := generation.GetCompletedAt().AsTime(); !got.Equal(completedAt) {
		t.Fatalf("unexpected proto completed_at: got %s want %s", got, completedAt)
	}

	if len(generation.GetInput()) != 1 {
		t.Fatalf("expected 1 proto input message, got %d", len(generation.GetInput()))
	}
	if input := generation.GetInput()[0]; input.GetRole() != sigilv1.MessageRole_MESSAGE_ROLE_USER || input.GetName() != "customer" || len(input.GetParts()) != 1 || input.GetParts()[0].GetText() != "Summarize yesterday's Paris weather and explain the spikes." {
		t.Fatalf("unexpected proto input message: %#v", input)
	}

	if len(generation.GetOutput()) != 3 {
		t.Fatalf("expected 3 proto output messages, got %d", len(generation.GetOutput()))
	}
	firstOutput := generation.GetOutput()[0]
	if firstOutput.GetRole() != sigilv1.MessageRole_MESSAGE_ROLE_ASSISTANT || firstOutput.GetName() != "assistant" || len(firstOutput.GetParts()) != 2 {
		t.Fatalf("unexpected first proto output message: %#v", firstOutput)
	}
	if got := firstOutput.GetParts()[0].GetThinking(); got != "Need the weather tool output before the final answer." {
		t.Fatalf("unexpected proto thinking part: got %q", got)
	}
	if got := firstOutput.GetParts()[0].GetMetadata().GetProviderType(); got != "thinking" {
		t.Fatalf("unexpected proto thinking provider_type: got %q want %q", got, "thinking")
	}
	if got := firstOutput.GetParts()[1].GetToolCall().GetId(); got != "call-weather-1" {
		t.Fatalf("unexpected proto tool call id: got %q want %q", got, "call-weather-1")
	}
	if got := firstOutput.GetParts()[1].GetToolCall().GetName(); got != "weather.lookup" {
		t.Fatalf("unexpected proto tool call name: got %q want %q", got, "weather.lookup")
	}
	if !bytes.Equal(firstOutput.GetParts()[1].GetToolCall().GetInputJson(), []byte(`{"city":"Paris","date":"2026-03-11"}`)) {
		t.Fatalf("unexpected proto tool call input json: %s", firstOutput.GetParts()[1].GetToolCall().GetInputJson())
	}
	if got := firstOutput.GetParts()[1].GetMetadata().GetProviderType(); got != "tool_use" {
		t.Fatalf("unexpected proto tool call provider_type: got %q want %q", got, "tool_use")
	}

	secondOutput := generation.GetOutput()[1]
	if secondOutput.GetRole() != sigilv1.MessageRole_MESSAGE_ROLE_TOOL || secondOutput.GetName() != "weather.lookup" || len(secondOutput.GetParts()) != 1 {
		t.Fatalf("unexpected second proto output message: %#v", secondOutput)
	}
	if got := secondOutput.GetParts()[0].GetToolResult().GetToolCallId(); got != "call-weather-1" {
		t.Fatalf("unexpected proto tool result tool_call_id: got %q want %q", got, "call-weather-1")
	}
	if got := secondOutput.GetParts()[0].GetToolResult().GetName(); got != "weather.lookup" {
		t.Fatalf("unexpected proto tool result name: got %q want %q", got, "weather.lookup")
	}
	if got := secondOutput.GetParts()[0].GetToolResult().GetContent(); got != "22C with a late-afternoon drop" {
		t.Fatalf("unexpected proto tool result content: got %q", got)
	}
	if !bytes.Equal(secondOutput.GetParts()[0].GetToolResult().GetContentJson(), []byte(`{"high_c":22,"trend":"late drop"}`)) {
		t.Fatalf("unexpected proto tool result content json: %s", secondOutput.GetParts()[0].GetToolResult().GetContentJson())
	}
	if secondOutput.GetParts()[0].GetToolResult().GetIsError() {
		t.Fatalf("expected successful proto tool result")
	}

	thirdOutput := generation.GetOutput()[2]
	if thirdOutput.GetRole() != sigilv1.MessageRole_MESSAGE_ROLE_ASSISTANT || thirdOutput.GetName() != "assistant" || len(thirdOutput.GetParts()) != 1 {
		t.Fatalf("unexpected third proto output message: %#v", thirdOutput)
	}
	if got := thirdOutput.GetParts()[0].GetText(); got != "Paris peaked at 22C before a late drop as cloud cover moved in." {
		t.Fatalf("unexpected proto output text: got %q", got)
	}

	if len(generation.GetTools()) != 1 {
		t.Fatalf("expected 1 proto tool definition, got %d", len(generation.GetTools()))
	}
	tool := generation.GetTools()[0]
	if tool.GetName() != "weather.lookup" || tool.GetDescription() != "Look up historical weather by city and date" || tool.GetType() != "function" || !tool.GetDeferred() {
		t.Fatalf("unexpected proto tool definition: %#v", tool)
	}
	if !bytes.Equal(tool.GetInputSchemaJson(), []byte(`{"type":"object","properties":{"city":{"type":"string"},"date":{"type":"string"}},"required":["city","date"]}`)) {
		t.Fatalf("unexpected proto tool input schema: %s", tool.GetInputSchemaJson())
	}

	usage := generation.GetUsage()
	if usage.GetInputTokens() != 120 || usage.GetOutputTokens() != 42 || usage.GetTotalTokens() != 162 || usage.GetCacheReadInputTokens() != 30 || usage.GetCacheWriteInputTokens() != 4 || usage.GetReasoningTokens() != 9 {
		t.Fatalf("unexpected proto usage: %#v", usage)
	}

	if len(generation.GetTags()) != 4 {
		t.Fatalf("expected 4 proto tags, got %d", len(generation.GetTags()))
	}
	if got := generation.GetTags()["env"]; got != "prod" {
		t.Fatalf("unexpected proto tag env: got %q want %q", got, "prod")
	}
	if got := generation.GetTags()["seed_only"]; got != "seed" {
		t.Fatalf("unexpected proto tag seed_only: got %q want %q", got, "seed")
	}
	if got := generation.GetTags()["shared"]; got != "result" {
		t.Fatalf("unexpected proto tag shared: got %q want %q", got, "result")
	}
	if got := generation.GetTags()["result_only"]; got != "assistant" {
		t.Fatalf("unexpected proto tag result_only: got %q want %q", got, "assistant")
	}

	metadata := generation.GetMetadata().AsMap()
	if got := metadata[sdkMetadataKeyName]; got != "sdk-go" {
		t.Fatalf("unexpected proto metadata %q: got %#v want %#v", sdkMetadataKeyName, got, "sdk-go")
	}
	if got := metadata[metadataKeyConversation]; got != "Weather follow-up" {
		t.Fatalf("unexpected proto metadata %q: got %#v want %#v", metadataKeyConversation, got, "Weather follow-up")
	}
	if got := metadata[metadataKeyCanonicalUserID]; got != "user-42" {
		t.Fatalf("unexpected proto metadata %q: got %#v want %#v", metadataKeyCanonicalUserID, got, "user-42")
	}
	if got := metadata[spanAttrRequestThinkingBudget]; got != float64(2048) {
		t.Fatalf("unexpected proto metadata %q: got %#v want %#v", spanAttrRequestThinkingBudget, got, float64(2048))
	}
	if got := metadata["request_only"]; got != "seed-value" {
		t.Fatalf("unexpected proto metadata request_only: got %#v want %#v", got, "seed-value")
	}
	if got := metadata["shared"]; got != "result" {
		t.Fatalf("unexpected proto metadata shared: got %#v want %#v", got, "result")
	}
	if got := metadata["result_only"]; got != "assistant" {
		t.Fatalf("unexpected proto metadata result_only: got %#v want %#v", got, "assistant")
	}
	if got := metadata["quality"]; got != true {
		t.Fatalf("unexpected proto metadata quality: got %#v want %#v", got, true)
	}
	nested, ok := metadata["nested"].(map[string]any)
	if !ok {
		t.Fatalf("expected nested proto metadata map, got %#v", metadata["nested"])
	}
	if got := nested["phase"]; got != "result" {
		t.Fatalf("unexpected proto nested metadata phase: got %#v want %#v", got, "result")
	}

	if len(generation.GetRawArtifacts()) != 2 {
		t.Fatalf("expected 2 proto artifacts, got %d", len(generation.GetRawArtifacts()))
	}
	if artifact := generation.GetRawArtifacts()[0]; artifact.GetKind() != sigilv1.ArtifactKind_ARTIFACT_KIND_REQUEST || artifact.GetName() != "request" || artifact.GetContentType() != "application/json" || artifact.GetRecordId() != "rec-request-1" || artifact.GetUri() != "sigil://artifact/request-1" || !bytes.Equal(artifact.GetPayload(), requestArtifact.Payload) {
		t.Fatalf("unexpected request artifact: %#v", artifact)
	}
	if artifact := generation.GetRawArtifacts()[1]; artifact.GetKind() != sigilv1.ArtifactKind_ARTIFACT_KIND_RESPONSE || artifact.GetName() != "response" || artifact.GetContentType() != "application/json" || artifact.GetRecordId() != "rec-response-1" || artifact.GetUri() != "sigil://artifact/response-1" || !bytes.Equal(artifact.GetPayload(), responseArtifact.Payload) {
		t.Fatalf("unexpected response artifact: %#v", artifact)
	}
}

func TestConformance_WorkflowStepRoundtrip(t *testing.T) {
	now := time.Date(2026, time.March, 12, 12, 0, 0, 0, time.UTC)
	env := newConformanceEnv(t, withConformanceConfig(func(cfg *sigil.Config) {
		cfg.Now = func() time.Time { return now }
		cfg.Tags = map[string]string{
			"client": "tag",
			"shared": "client",
		}
	}))

	if err := env.Client.EnqueueWorkflowStep(sigil.WorkflowStep{
		ID:                  "wfs-roundtrip",
		ConversationID:      "conv-workflow",
		StepName:            "route",
		Framework:           "custom",
		InputState:          map[string]any{"prompt": "hello", "count": 1},
		OutputState:         map[string]any{"route": "answer"},
		Tags:                map[string]string{"shared": "step"},
		LinkedGenerationIDs: []string{"gen-roundtrip"},
		ParentStepIDs:       []string{"wfs-root"},
		AgentName:           "agent-workflow",
		AgentVersion:        "v1",
		TraceID:             "0123456789abcdef0123456789abcdef",
		SpanID:              "0123456789abcdef",
		Metadata:            map[string]any{"run_id": "run-1"},
	}); err != nil {
		t.Fatalf("enqueue workflow step: %v", err)
	}

	env.Shutdown(t)

	step := env.Ingest.SingleWorkflowStep(t)
	if got := step.GetId(); got != "wfs-roundtrip" {
		t.Fatalf("workflow step id = %q, want wfs-roundtrip", got)
	}
	if got := step.GetConversationId(); got != "conv-workflow" {
		t.Fatalf("conversation id = %q, want conv-workflow", got)
	}
	if got := step.GetStepName(); got != "route" {
		t.Fatalf("step name = %q, want route", got)
	}
	if got := step.GetStartedAt().AsTime(); !got.Equal(now) {
		t.Fatalf("started_at = %s, want %s", got, now)
	}
	if got := step.GetCompletedAt().AsTime(); !got.Equal(now) {
		t.Fatalf("completed_at = %s, want %s", got, now)
	}
	if got := step.GetTags()["client"]; got != "tag" {
		t.Fatalf("client tag = %q, want tag", got)
	}
	if got := step.GetTags()["shared"]; got != "step" {
		t.Fatalf("shared tag = %q, want step", got)
	}
	if got := step.GetInputState().GetFields()["prompt"].GetStringValue(); got != "hello" {
		t.Fatalf("input_state.prompt = %q, want hello", got)
	}
	if got := step.GetOutputState().GetFields()["route"].GetStringValue(); got != "answer" {
		t.Fatalf("output_state.route = %q, want answer", got)
	}
	if got := step.GetMetadata().GetFields()["run_id"].GetStringValue(); got != "run-1" {
		t.Fatalf("metadata.run_id = %q, want run-1", got)
	}
	if got := step.GetLinkedGenerationIds(); !slices.Equal(got, []string{"gen-roundtrip"}) {
		t.Fatalf("linked_generation_ids = %#v, want gen-roundtrip", got)
	}
	if got := step.GetParentStepIds(); !slices.Equal(got, []string{"wfs-root"}) {
		t.Fatalf("parent_step_ids = %#v, want wfs-root", got)
	}
}

func TestConformance_ConversationTitleSemantics(t *testing.T) {
	testCases := []struct {
		name          string
		startTitle    string
		contextTitle  string
		metadataTitle string
		wantTitle     string
	}{
		{
			name:          "explicit wins",
			startTitle:    "Explicit",
			contextTitle:  "Context",
			metadataTitle: "Meta",
			wantTitle:     "Explicit",
		},
		{
			name:         "context fallback",
			contextTitle: "Context",
			wantTitle:    "Context",
		},
		{
			name:          "metadata fallback",
			metadataTitle: "Meta",
			wantTitle:     "Meta",
		},
		{
			name:       "whitespace omitted",
			startTitle: "  ",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			env := newConformanceEnv(t)

			ctx := context.Background()
			if tc.contextTitle != "" {
				ctx = sigil.WithConversationTitle(ctx, tc.contextTitle)
			}

			start := sigil.GenerationStart{
				Model:             conformanceModel,
				ConversationTitle: tc.startTitle,
			}
			if tc.metadataTitle != "" {
				start.Metadata = map[string]any{
					metadataKeyConversation: tc.metadataTitle,
				}
			}

			recordGeneration(t, ctx, env, start, sigil.Generation{})

			span := findSpan(t, env.Spans.Ended(), conformanceOperationName)
			attrs := spanAttrs(span)
			if tc.wantTitle == "" {
				requireSpanAttrAbsent(t, attrs, spanAttrConversationTitle)
			} else {
				requireSpanAttr(t, attrs, spanAttrConversationTitle, tc.wantTitle)
			}

			requireSyncGenerationMetrics(t, env)
			env.Shutdown(t)

			generation := env.Ingest.SingleGeneration(t)
			if tc.wantTitle == "" {
				requireProtoMetadataAbsent(t, generation, metadataKeyConversation)
			} else {
				requireProtoMetadata(t, generation, metadataKeyConversation, tc.wantTitle)
			}
		})
	}
}

func TestConformance_UserIDSemantics(t *testing.T) {
	testCases := []struct {
		name           string
		startUserID    string
		contextUserID  string
		canonicalUser  string
		legacyUser     string
		wantResolvedID string
	}{
		{
			name:           "explicit wins",
			startUserID:    "explicit",
			contextUserID:  "ctx",
			canonicalUser:  "meta-canonical",
			legacyUser:     "meta-legacy",
			wantResolvedID: "explicit",
		},
		{
			name:           "context fallback",
			contextUserID:  "ctx",
			wantResolvedID: "ctx",
		},
		{
			name:           "canonical metadata",
			canonicalUser:  "canonical",
			wantResolvedID: "canonical",
		},
		{
			name:           "legacy metadata",
			legacyUser:     "legacy",
			wantResolvedID: "legacy",
		},
		{
			name:           "canonical beats legacy",
			canonicalUser:  "canonical",
			legacyUser:     "legacy",
			wantResolvedID: "canonical",
		},
		{
			name:           "whitespace trimmed",
			startUserID:    "  padded  ",
			wantResolvedID: "padded",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			env := newConformanceEnv(t)

			ctx := context.Background()
			if tc.contextUserID != "" {
				ctx = sigil.WithUserID(ctx, tc.contextUserID)
			}

			start := sigil.GenerationStart{
				Model:  conformanceModel,
				UserID: tc.startUserID,
			}
			if tc.canonicalUser != "" || tc.legacyUser != "" {
				start.Metadata = map[string]any{}
				if tc.canonicalUser != "" {
					start.Metadata[metadataKeyCanonicalUserID] = tc.canonicalUser
				}
				if tc.legacyUser != "" {
					start.Metadata[metadataKeyLegacyUserID] = tc.legacyUser
				}
			}

			recordGeneration(t, ctx, env, start, sigil.Generation{})

			span := findSpan(t, env.Spans.Ended(), conformanceOperationName)
			attrs := spanAttrs(span)
			requireSpanAttr(t, attrs, spanAttrUserID, tc.wantResolvedID)

			requireSyncGenerationMetrics(t, env)
			env.Shutdown(t)

			generation := env.Ingest.SingleGeneration(t)
			requireProtoMetadata(t, generation, metadataKeyCanonicalUserID, tc.wantResolvedID)
		})
	}
}

func TestConformance_AgentIdentitySemantics(t *testing.T) {
	testCases := []struct {
		name             string
		startAgentName   string
		startVersion     string
		contextAgentName string
		contextVersion   string
		resultAgentName  string
		resultVersion    string
		wantAgentName    string
		wantVersion      string
	}{
		{
			name:           "explicit fields",
			startAgentName: "agent-explicit",
			startVersion:   "v1.2.3",
			wantAgentName:  "agent-explicit",
			wantVersion:    "v1.2.3",
		},
		{
			name:             "context fallback",
			contextAgentName: "agent-context",
			contextVersion:   "v-context",
			wantAgentName:    "agent-context",
			wantVersion:      "v-context",
		},
		{
			name:            "result-time override",
			startAgentName:  "agent-seed",
			startVersion:    "v-seed",
			resultAgentName: "agent-result",
			resultVersion:   "v-result",
			wantAgentName:   "agent-result",
			wantVersion:     "v-result",
		},
		{
			name: "empty field omission",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			env := newConformanceEnv(t)

			ctx := context.Background()
			if tc.contextAgentName != "" {
				ctx = sigil.WithAgentName(ctx, tc.contextAgentName)
			}
			if tc.contextVersion != "" {
				ctx = sigil.WithAgentVersion(ctx, tc.contextVersion)
			}

			start := sigil.GenerationStart{
				Model:        conformanceModel,
				AgentName:    tc.startAgentName,
				AgentVersion: tc.startVersion,
			}
			result := sigil.Generation{
				AgentName:    tc.resultAgentName,
				AgentVersion: tc.resultVersion,
			}

			recordGeneration(t, ctx, env, start, result)

			span := findSpan(t, env.Spans.Ended(), conformanceOperationName)
			attrs := spanAttrs(span)
			if tc.wantAgentName == "" {
				requireSpanAttrAbsent(t, attrs, spanAttrAgentName)
			} else {
				requireSpanAttr(t, attrs, spanAttrAgentName, tc.wantAgentName)
			}
			if tc.wantVersion == "" {
				requireSpanAttrAbsent(t, attrs, spanAttrAgentVersion)
			} else {
				requireSpanAttr(t, attrs, spanAttrAgentVersion, tc.wantVersion)
			}

			requireSyncGenerationMetrics(t, env)
			env.Shutdown(t)

			generation := env.Ingest.SingleGeneration(t)
			if tc.wantAgentName == "" {
				if got := generation.GetAgentName(); got != "" {
					t.Fatalf("expected empty proto agent_name, got %q", got)
				}
			} else if got := generation.GetAgentName(); got != tc.wantAgentName {
				t.Fatalf("unexpected proto agent_name: got %q want %q", got, tc.wantAgentName)
			}

			if tc.wantVersion == "" {
				if got := generation.GetAgentVersion(); got != "" {
					t.Fatalf("expected empty proto agent_version, got %q", got)
				}
			} else if got := generation.GetAgentVersion(); got != tc.wantVersion {
				t.Fatalf("unexpected proto agent_version: got %q want %q", got, tc.wantVersion)
			}
		})
	}
}

// echo -n "1.2.3" | shasum -a 256
const effectiveVersionDigest1_2_3 = "sha256:c47f5b18b8a430e698b9fe15e51f6119984e78334bcf3f45e210d30c37ef2f9e"

func TestConformance_EffectiveVersionSemantics(t *testing.T) {
	testCases := []struct {
		name             string
		effectiveVersion string
		want             string // empty means proto field must be unset
	}{
		{
			name: "unset leaves proto field absent",
		},
		{
			name:             "whitespace-only leaves proto field absent",
			effectiveVersion: "   ",
		},
		{
			name:             "raw 1.2.3 hashes to pinned digest",
			effectiveVersion: "1.2.3",
			want:             effectiveVersionDigest1_2_3,
		},
		{
			name:             "surrounding whitespace is trimmed before hashing",
			effectiveVersion: "  1.2.3\t\n",
			want:             effectiveVersionDigest1_2_3,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			env := newConformanceEnv(t)

			start := sigil.GenerationStart{
				Model:            conformanceModel,
				EffectiveVersion: tc.effectiveVersion,
			}
			recordGeneration(t, context.Background(), env, start, sigil.Generation{})

			env.Shutdown(t)
			generation := env.Ingest.SingleGeneration(t)

			if tc.want == "" {
				if generation.EffectiveVersion != nil {
					t.Fatalf("expected unset proto effective_version, got %q", generation.GetEffectiveVersion())
				}
				return
			}
			if got := generation.GetEffectiveVersion(); got != tc.want {
				t.Fatalf("unexpected effective_version: got %q want %q", got, tc.want)
			}
		})
	}
}

func TestConformance_EffectiveVersionResultOverridesStart(t *testing.T) {
	// echo -n "result-only" | shasum -a 256
	const resultOnlyDigest = "sha256:f61f2b041f07a7e4a58a926df31279f4c11ebd1f716147d8ee8cbfad6a69f30e"

	testCases := []struct {
		name        string
		startValue  string
		resultValue string
		want        string
	}{
		{
			name:       "start falls through when result is empty",
			startValue: "1.2.3",
			want:       effectiveVersionDigest1_2_3,
		},
		{
			name:        "start falls through when result is whitespace-only",
			startValue:  "1.2.3",
			resultValue: "   ",
			want:        effectiveVersionDigest1_2_3,
		},
		{
			name:        "result wins over start",
			startValue:  "ignored",
			resultValue: "result-only",
			want:        resultOnlyDigest,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			env := newConformanceEnv(t)

			start := sigil.GenerationStart{
				Model:            conformanceModel,
				EffectiveVersion: tc.startValue,
			}
			result := sigil.Generation{EffectiveVersion: tc.resultValue}
			recordGeneration(t, context.Background(), env, start, result)

			env.Shutdown(t)
			generation := env.Ingest.SingleGeneration(t)

			if got := generation.GetEffectiveVersion(); got != tc.want {
				t.Fatalf("unexpected effective_version: got %q want %q", got, tc.want)
			}
		})
	}
}

func TestConformance_StreamingMode(t *testing.T) {
	env := newConformanceEnv(t)

	recordGeneration(t, context.Background(), env, sigil.GenerationStart{
		ConversationID: "conv-sync",
		Model:          conformanceModel,
		StartedAt:      time.Date(2026, 3, 12, 14, 0, 0, 0, time.UTC),
	}, sigil.Generation{
		Input:       []sigil.Message{sigil.UserTextMessage("hello")},
		Output:      []sigil.Message{sigil.AssistantTextMessage("hi")},
		CompletedAt: time.Date(2026, 3, 12, 14, 0, 1, 0, time.UTC),
	})

	streamStartedAt := time.Date(2026, 3, 12, 14, 1, 0, 0, time.UTC)
	_, recorder := env.Client.StartStreamingGeneration(context.Background(), sigil.GenerationStart{
		ConversationID: "conv-stream",
		AgentName:      "agent-stream",
		Model:          conformanceModel,
		StartedAt:      streamStartedAt,
	})
	recorder.SetFirstTokenAt(streamStartedAt.Add(250 * time.Millisecond))
	recorder.SetResult(sigil.Generation{
		Input:  []sigil.Message{sigil.UserTextMessage("say hello")},
		Output: []sigil.Message{sigil.AssistantTextMessage("Hello world")},
		Usage: sigil.TokenUsage{
			InputTokens:  3,
			OutputTokens: 2,
			TotalTokens:  5,
		},
		CompletedAt: streamStartedAt.Add(1500 * time.Millisecond),
	}, nil)
	recorder.End()
	if err := recorder.Err(); err != nil {
		t.Fatalf("record streaming generation: %v", err)
	}

	metrics := env.CollectMetrics(t)
	ttft := findHistogram[float64](t, metrics, metricTimeToFirstToken)
	if len(ttft.DataPoints) != 1 {
		t.Fatalf("expected exactly 1 %s datapoint, got %d", metricTimeToFirstToken, len(ttft.DataPoints))
	}
	requireHistogramPointWithAttrs(t, ttft, map[string]string{
		spanAttrProviderName: conformanceModel.Provider,
		spanAttrRequestModel: conformanceModel.Name,
		spanAttrAgentName:    "agent-stream",
	})

	expectedDurationBuckets := []float64{
		0.01, 0.02, 0.04, 0.08, 0.16, 0.32, 0.64, 1.28,
		2.56, 5.12, 10.24, 20.48, 40.96, 81.92,
	}
	expectedTokenUsageBuckets := []float64{
		1, 4, 16, 64, 256, 1024, 4096, 16384,
		65536, 262144, 1048576, 4194304, 16777216, 67108864,
	}
	duration := findHistogram[float64](t, metrics, metricOperationDuration)
	if len(duration.DataPoints) == 0 {
		t.Fatalf("expected %s datapoints", metricOperationDuration)
	}
	if got := duration.DataPoints[0].Bounds; !slices.Equal(got, expectedDurationBuckets) {
		t.Fatalf("%s bucket boundaries mismatch:\nexpected %v\n     got %v", metricOperationDuration, expectedDurationBuckets, got)
	}
	if got := ttft.DataPoints[0].Bounds; !slices.Equal(got, expectedDurationBuckets) {
		t.Fatalf("%s bucket boundaries mismatch:\nexpected %v\n     got %v", metricTimeToFirstToken, expectedDurationBuckets, got)
	}
	tokenUsage := findHistogram[int64](t, metrics, metricTokenUsage)
	if len(tokenUsage.DataPoints) == 0 {
		t.Fatalf("expected %s datapoints", metricTokenUsage)
	}
	if got := tokenUsage.DataPoints[0].Bounds; !slices.Equal(got, expectedTokenUsageBuckets) {
		t.Fatalf("%s bucket boundaries mismatch:\nexpected %v\n     got %v", metricTokenUsage, expectedTokenUsageBuckets, got)
	}

	env.Shutdown(t)

	streamGeneration := findGenerationByConversationID(t, env.Ingest.Requests(), "conv-stream")
	if got := streamGeneration.GetMode(); got != sigilv1.GenerationMode_GENERATION_MODE_STREAM {
		t.Fatalf("unexpected proto mode: got %v want %v", got, sigilv1.GenerationMode_GENERATION_MODE_STREAM)
	}
	if got := streamGeneration.GetOperationName(); got != conformanceStreamOperation {
		t.Fatalf("unexpected proto operation: got %q want %q", got, conformanceStreamOperation)
	}
	if len(streamGeneration.GetOutput()) != 1 || len(streamGeneration.GetOutput()[0].GetParts()) != 1 {
		t.Fatalf("expected a single streamed assistant output, got %#v", streamGeneration.GetOutput())
	}
	if got := streamGeneration.GetOutput()[0].GetParts()[0].GetText(); got != "Hello world" {
		t.Fatalf("unexpected streamed assistant text: got %q want %q", got, "Hello world")
	}

	span := findSpan(t, env.Spans.Ended(), conformanceStreamOperation)
	if got := span.Name(); got != conformanceStreamOperation+" "+conformanceModel.Name {
		t.Fatalf("unexpected streaming span name: %q", got)
	}
	attrs := spanAttrs(span)
	requireSpanAttr(t, attrs, spanAttrOperationName, conformanceStreamOperation)
}

func TestConformance_ToolExecution(t *testing.T) {
	env := newConformanceEnv(t)

	ctx := sigil.WithConversationID(context.Background(), "conv-tool")
	ctx = sigil.WithConversationTitle(ctx, "Weather lookup")
	ctx = sigil.WithAgentName(ctx, "agent-tools")
	ctx = sigil.WithAgentVersion(ctx, "2026.03.12")

	generationStartedAt := time.Date(2026, 3, 12, 14, 2, 0, 0, time.UTC)
	callCtx, generationRecorder := env.Client.StartGeneration(ctx, sigil.GenerationStart{
		Model:     conformanceModel,
		StartedAt: generationStartedAt,
	})
	_, toolRecorder := env.Client.StartToolExecution(callCtx, sigil.ToolExecutionStart{
		ToolName:        "weather",
		ToolCallID:      "call-weather",
		ToolType:        "function",
		ToolDescription: "Get weather",
		RequestModel:    conformanceModel.Name,
		RequestProvider: conformanceModel.Provider,
		IncludeContent:  true,
		StartedAt:       generationStartedAt.Add(100 * time.Millisecond),
	})
	toolRecorder.SetResult(sigil.ToolExecutionEnd{
		Arguments:   map[string]any{"city": "Paris"},
		Result:      map[string]any{"temp_c": 18},
		CompletedAt: generationStartedAt.Add(600 * time.Millisecond),
	})
	toolRecorder.End()
	if err := toolRecorder.Err(); err != nil {
		t.Fatalf("record tool execution: %v", err)
	}

	generationRecorder.SetResult(sigil.Generation{
		Input:       []sigil.Message{sigil.UserTextMessage("weather in Paris")},
		Output:      []sigil.Message{sigil.AssistantTextMessage("Paris is 18C")},
		CompletedAt: generationStartedAt.Add(time.Second),
	}, nil)
	generationRecorder.End()
	if err := generationRecorder.Err(); err != nil {
		t.Fatalf("record parent generation: %v", err)
	}

	metrics := env.CollectMetrics(t)
	duration := findHistogram[float64](t, metrics, metricOperationDuration)
	requireHistogramPointWithAttrs(t, duration, map[string]string{
		spanAttrOperationName: conformanceToolOperation,
		spanAttrProviderName:  conformanceModel.Provider,
		spanAttrRequestModel:  conformanceModel.Name,
		spanAttrToolName:      "weather",
		spanAttrAgentName:     "agent-tools",
	})

	env.Shutdown(t)

	span := findSpan(t, env.Spans.Ended(), conformanceToolOperation)
	if got := span.SpanKind(); got != trace.SpanKindInternal {
		t.Fatalf("unexpected tool span kind: got %v want %v", got, trace.SpanKindInternal)
	}

	attrs := spanAttrs(span)
	requireSpanAttr(t, attrs, spanAttrOperationName, conformanceToolOperation)
	requireSpanAttr(t, attrs, spanAttrToolName, "weather")
	requireSpanAttr(t, attrs, spanAttrToolCallID, "call-weather")
	requireSpanAttr(t, attrs, spanAttrToolType, "function")
	requireSpanAttr(t, attrs, spanAttrToolDescription, "Get weather")
	requireSpanAttr(t, attrs, spanAttrConversationID, "conv-tool")
	requireSpanAttr(t, attrs, spanAttrConversationTitle, "Weather lookup")
	requireSpanAttr(t, attrs, spanAttrAgentName, "agent-tools")
	requireSpanAttr(t, attrs, spanAttrAgentVersion, "2026.03.12")
	requireSpanAttr(t, attrs, spanAttrProviderName, conformanceModel.Provider)
	requireSpanAttr(t, attrs, spanAttrRequestModel, conformanceModel.Name)
	requireSpanAttr(t, attrs, metadataKeySDKName, sdkNameGo)
	requireSpanAttrPresent(t, attrs, spanAttrToolCallArguments)
	requireSpanAttrPresent(t, attrs, spanAttrToolCallResult)
}

func TestConformance_Embedding(t *testing.T) {
	env := newConformanceEnv(t)

	_, recorder := env.Client.StartEmbedding(context.Background(), sigil.EmbeddingStart{
		Model:          sigil.ModelRef{Provider: "openai", Name: "text-embedding-3-small"},
		AgentName:      "agent-embed",
		Dimensions:     int64Ptr(256),
		EncodingFormat: "float",
		StartedAt:      time.Date(2026, 3, 12, 14, 3, 0, 0, time.UTC),
	})
	recorder.SetResult(sigil.EmbeddingResult{
		InputCount:    2,
		InputTokens:   120,
		ResponseModel: "text-embedding-3-small",
		Dimensions:    int64Ptr(256),
	})
	recorder.End()
	if err := recorder.Err(); err != nil {
		t.Fatalf("record embedding: %v", err)
	}

	metrics := env.CollectMetrics(t)
	duration := findHistogram[float64](t, metrics, metricOperationDuration)
	requireHistogramPointWithAttrs(t, duration, map[string]string{
		spanAttrOperationName: conformanceEmbeddingOperation,
		spanAttrProviderName:  "openai",
		spanAttrRequestModel:  "text-embedding-3-small",
		spanAttrAgentName:     "agent-embed",
	})
	tokenUsage := findHistogram[int64](t, metrics, metricTokenUsage)
	requireHistogramPointWithAttrs(t, tokenUsage, map[string]string{
		spanAttrOperationName: conformanceEmbeddingOperation,
		spanAttrProviderName:  "openai",
		spanAttrRequestModel:  "text-embedding-3-small",
		spanAttrAgentName:     "agent-embed",
		metricAttrTokenType:   metricTokenTypeInput,
	})
	requireNoHistogram(t, metrics, metricTimeToFirstToken)
	requireNoHistogram(t, metrics, metricToolCallsPerOperation)

	env.Shutdown(t)

	if got := env.Ingest.GenerationCount(); got != 0 {
		t.Fatalf("expected no generation exports for embeddings, got %d", got)
	}

	span := findSpan(t, env.Spans.Ended(), conformanceEmbeddingOperation)
	if got := span.SpanKind(); got != trace.SpanKindClient {
		t.Fatalf("unexpected embedding span kind: got %v want %v", got, trace.SpanKindClient)
	}

	attrs := spanAttrs(span)
	requireSpanAttr(t, attrs, spanAttrOperationName, conformanceEmbeddingOperation)
	requireSpanAttr(t, attrs, spanAttrProviderName, "openai")
	requireSpanAttr(t, attrs, spanAttrRequestModel, "text-embedding-3-small")
	requireSpanAttr(t, attrs, metadataKeySDKName, sdkNameGo)
	if got := attrs[spanAttrEmbeddingInputCount].AsInt64(); got != 2 {
		t.Fatalf("unexpected embedding input count: got %d want 2", got)
	}
	if got := attrs[spanAttrEmbeddingDimCount].AsInt64(); got != 256 {
		t.Fatalf("unexpected embedding dimension count: got %d want 256", got)
	}
}

func TestConformance_FullWithMetadataSpansMode(t *testing.T) {
	t.Run("generation proto full, span title absent", func(t *testing.T) {
		env := newConformanceEnv(t, withConformanceConfig(func(cfg *sigil.Config) {
			cfg.ContentCapture = sigil.ContentCaptureModeFullWithMetadataSpans
		}))

		requestArtifact, err := sigil.NewJSONArtifact(sigil.ArtifactKindRequest, "request", map[string]any{"messages": 1})
		if err != nil {
			t.Fatalf("build request artifact: %v", err)
		}

		_, recorder := env.Client.StartGeneration(context.Background(), sigil.GenerationStart{
			ConversationID:    "conv-fwms",
			ConversationTitle: "Sensitive conversation",
			Model:             conformanceModel,
			SystemPrompt:      "Be helpful.",
			StartedAt:         time.Date(2026, 3, 12, 14, 10, 0, 0, time.UTC),
		})
		recorder.SetResult(sigil.Generation{
			Input:  []sigil.Message{sigil.UserTextMessage("hello world")},
			Output: []sigil.Message{sigil.AssistantTextMessage("hi back")},
			Usage: sigil.TokenUsage{
				InputTokens:  3,
				OutputTokens: 2,
				TotalTokens:  5,
			},
			Artifacts:   []sigil.Artifact{requestArtifact},
			CompletedAt: time.Date(2026, 3, 12, 14, 10, 1, 0, time.UTC),
		}, nil)
		recorder.End()
		if err := recorder.Err(); err != nil {
			t.Fatalf("record generation: %v", err)
		}

		env.Shutdown(t)

		span := findSpan(t, env.Spans.Ended(), conformanceOperationName)
		attrs := spanAttrs(span)
		requireSpanAttrAbsent(t, attrs, spanAttrConversationTitle)

		generation := env.Ingest.SingleGeneration(t)
		if got := generation.GetSystemPrompt(); got != "Be helpful." {
			t.Fatalf("unexpected proto system_prompt: got %q", got)
		}
		if len(generation.GetInput()) != 1 || generation.GetInput()[0].GetParts()[0].GetText() != "hello world" {
			t.Fatalf("expected proto input text preserved, got %#v", generation.GetInput())
		}
		if len(generation.GetOutput()) != 1 || generation.GetOutput()[0].GetParts()[0].GetText() != "hi back" {
			t.Fatalf("expected proto output text preserved, got %#v", generation.GetOutput())
		}
		if len(generation.GetRawArtifacts()) != 1 {
			t.Fatalf("expected 1 proto raw_artifact, got %d", len(generation.GetRawArtifacts()))
		}
		requireProtoMetadata(t, generation, metadataKeyConversation, "Sensitive conversation")
	})

	t.Run("tool execution proto full, span content absent", func(t *testing.T) {
		env := newConformanceEnv(t, withConformanceConfig(func(cfg *sigil.Config) {
			cfg.ContentCapture = sigil.ContentCaptureModeFullWithMetadataSpans
		}))

		ctx := sigil.WithConversationID(context.Background(), "conv-fwms-tool")
		ctx = sigil.WithConversationTitle(ctx, "Tool sensitive title")

		generationStartedAt := time.Date(2026, 3, 12, 14, 11, 0, 0, time.UTC)
		callCtx, generationRecorder := env.Client.StartGeneration(ctx, sigil.GenerationStart{
			Model:     conformanceModel,
			StartedAt: generationStartedAt,
		})
		_, toolRecorder := env.Client.StartToolExecution(callCtx, sigil.ToolExecutionStart{
			ToolName:        "weather",
			ToolCallID:      "call-weather",
			ToolType:        "function",
			ToolDescription: "Get weather",
			RequestModel:    conformanceModel.Name,
			RequestProvider: conformanceModel.Provider,
			IncludeContent:  true,
			StartedAt:       generationStartedAt.Add(100 * time.Millisecond),
		})
		toolRecorder.SetResult(sigil.ToolExecutionEnd{
			Arguments:   map[string]any{"city": "Paris"},
			Result:      map[string]any{"temp_c": 18},
			CompletedAt: generationStartedAt.Add(600 * time.Millisecond),
		})
		toolRecorder.End()
		if err := toolRecorder.Err(); err != nil {
			t.Fatalf("record tool execution: %v", err)
		}

		generationRecorder.SetResult(sigil.Generation{
			Input: []sigil.Message{sigil.UserTextMessage("weather please")},
			Output: []sigil.Message{
				{
					Role: sigil.RoleAssistant,
					Parts: []sigil.Part{
						{
							Kind: sigil.PartKindToolCall,
							ToolCall: &sigil.ToolCall{
								ID:        "call-weather",
								Name:      "weather",
								InputJSON: json.RawMessage(`{"city":"Paris"}`),
							},
						},
					},
				},
				{
					Role: sigil.RoleTool,
					Parts: []sigil.Part{
						{
							Kind: sigil.PartKindToolResult,
							ToolResult: &sigil.ToolResult{
								ToolCallID: "call-weather",
								Name:       "weather",
								Content:    "18C",
							},
						},
					},
				},
			},
			CompletedAt: generationStartedAt.Add(time.Second),
		}, nil)
		generationRecorder.End()
		if err := generationRecorder.Err(); err != nil {
			t.Fatalf("record parent generation: %v", err)
		}

		env.Shutdown(t)

		toolSpan := findSpan(t, env.Spans.Ended(), conformanceToolOperation)
		toolAttrs := spanAttrs(toolSpan)
		requireSpanAttrAbsent(t, toolAttrs, spanAttrToolCallArguments)
		requireSpanAttrAbsent(t, toolAttrs, spanAttrToolCallResult)
		requireSpanAttrAbsent(t, toolAttrs, spanAttrConversationTitle)
		requireSpanAttrAbsent(t, toolAttrs, spanAttrToolDescription)

		generation := env.Ingest.SingleGeneration(t)
		firstOutput := generation.GetOutput()[0]
		if got := firstOutput.GetParts()[0].GetToolCall().GetInputJson(); !bytes.Equal(got, []byte(`{"city":"Paris"}`)) {
			t.Fatalf("expected proto tool call input json preserved, got %s", got)
		}
		secondOutput := generation.GetOutput()[1]
		if got := secondOutput.GetParts()[0].GetToolResult().GetContent(); got != "18C" {
			t.Fatalf("expected proto tool result content preserved, got %q", got)
		}
	})

	t.Run("embedding input_texts absent from span", func(t *testing.T) {
		env := newConformanceEnv(t, withConformanceConfig(func(cfg *sigil.Config) {
			cfg.ContentCapture = sigil.ContentCaptureModeFullWithMetadataSpans
			cfg.EmbeddingCapture = sigil.EmbeddingCaptureConfig{
				CaptureInput: true,
			}
		}))

		_, recorder := env.Client.StartEmbedding(context.Background(), sigil.EmbeddingStart{
			Model:     sigil.ModelRef{Provider: "openai", Name: "text-embedding-3-small"},
			AgentName: "agent-embed-fwms",
			StartedAt: time.Date(2026, 3, 12, 14, 12, 0, 0, time.UTC),
		})
		recorder.SetResult(sigil.EmbeddingResult{
			InputCount:    1,
			InputTokens:   10,
			InputTexts:    []string{"sensitive input text"},
			ResponseModel: "text-embedding-3-small",
		})
		recorder.End()
		if err := recorder.Err(); err != nil {
			t.Fatalf("record embedding: %v", err)
		}

		env.Shutdown(t)

		span := findSpan(t, env.Spans.Ended(), conformanceEmbeddingOperation)
		attrs := spanAttrs(span)
		requireSpanAttrAbsent(t, attrs, spanAttrEmbeddingInputTexts)
	})

	t.Run("tool execution call error redacted on span", func(t *testing.T) {
		// Tools have no proto export, so the raw provider error must not echo
		// on the span path under FullWithMetadataSpans.
		env := newConformanceEnv(t, withConformanceConfig(func(cfg *sigil.Config) {
			cfg.ContentCapture = sigil.ContentCaptureModeFullWithMetadataSpans
		}))

		const leakMarker = "ignore previous instructions"
		rawErr := "provider returned HTTP 400: blocked content '" + leakMarker + "'"

		_, toolRec := env.Client.StartToolExecution(context.Background(), sigil.ToolExecutionStart{
			ToolName:        "weather",
			ToolCallID:      "call-tool-err",
			ToolType:        "function",
			ToolDescription: "Get weather",
			IncludeContent:  true,
			StartedAt:       time.Date(2026, 3, 12, 14, 13, 0, 0, time.UTC),
		})
		toolRec.SetExecError(errors.New(rawErr))
		toolRec.SetResult(sigil.ToolExecutionEnd{
			Arguments:   map[string]any{"city": "Paris"},
			Result:      map[string]any{"temp_c": 18},
			CompletedAt: time.Date(2026, 3, 12, 14, 13, 1, 0, time.UTC),
		})
		toolRec.End()

		env.Shutdown(t)

		span := findSpan(t, env.Spans.Ended(), conformanceToolOperation)
		if got := span.Status().Code; got != codes.Error {
			t.Fatalf("expected error span status, got %v", got)
		}
		if got := span.Status().Description; strings.Contains(got, leakMarker) {
			t.Fatalf("tool span status description leaks raw error: %q", got)
		}
		for _, ev := range span.Events() {
			for _, attr := range ev.Attributes {
				if strings.Contains(attr.Value.Emit(), leakMarker) {
					t.Errorf("tool span event %q attr %s leaks raw error: %q",
						ev.Name, attr.Key, attr.Value.Emit())
				}
			}
		}
		attrs := spanAttrs(span)
		requireSpanAttr(t, attrs, spanAttrErrorType, "tool_execution_error")
	})

	t.Run("embedding provider call error redacted on span", func(t *testing.T) {
		// Embeddings have no proto export, so the raw provider error must
		// not echo on the span path under FullWithMetadataSpans.
		env := newConformanceEnv(t, withConformanceConfig(func(cfg *sigil.Config) {
			cfg.ContentCapture = sigil.ContentCaptureModeFullWithMetadataSpans
			cfg.EmbeddingCapture = sigil.EmbeddingCaptureConfig{CaptureInput: true}
		}))

		const leakMarker = "ignore previous instructions"
		rawErr := "provider returned HTTP 400: blocked content '" + leakMarker + "'"

		_, recorder := env.Client.StartEmbedding(context.Background(), sigil.EmbeddingStart{
			Model:     sigil.ModelRef{Provider: "openai", Name: "text-embedding-3-small"},
			AgentName: "agent-embed-fwms-error",
			StartedAt: time.Date(2026, 3, 12, 14, 12, 30, 0, time.UTC),
		})
		recorder.SetCallError(errors.New(rawErr))
		recorder.SetResult(sigil.EmbeddingResult{
			InputCount: 1,
			InputTexts: []string{"sensitive input text"},
		})
		recorder.End()
		if err := recorder.Err(); err != nil {
			t.Fatalf("record embedding: %v", err)
		}

		env.Shutdown(t)

		span := findSpan(t, env.Spans.Ended(), conformanceEmbeddingOperation)
		if got := span.Status().Code; got != codes.Error {
			t.Fatalf("expected error span status, got %v", got)
		}
		if got := span.Status().Description; strings.Contains(got, leakMarker) {
			t.Fatalf("embedding span status description leaks raw error: %q", got)
		}
		for _, ev := range span.Events() {
			for _, attr := range ev.Attributes {
				if strings.Contains(attr.Value.Emit(), leakMarker) {
					t.Errorf("embedding span event %q attr %s leaks raw error: %q",
						ev.Name, attr.Key, attr.Value.Emit())
				}
			}
		}
		attrs := spanAttrs(span)
		requireSpanAttr(t, attrs, spanAttrErrorType, "provider_call_error")
	})

	t.Run("rating comment preserved", func(t *testing.T) {
		env := newConformanceEnv(t, withConformanceConfig(func(cfg *sigil.Config) {
			cfg.ContentCapture = sigil.ContentCaptureModeFullWithMetadataSpans
		}))

		_, err := env.Client.SubmitConversationRating(context.Background(), "conv-fwms-rating", sigil.ConversationRatingInput{
			RatingID: "rat-fwms",
			Rating:   sigil.ConversationRatingValueGood,
			Comment:  "user-supplied free text",
		})
		if err != nil {
			t.Fatalf("submit conversation rating: %v", err)
		}

		request := env.Rating.SingleRequest(t)
		var payload sigil.ConversationRatingInput
		if err := json.Unmarshal(request.Body, &payload); err != nil {
			t.Fatalf("decode rating request body: %v", err)
		}
		if payload.Comment != "user-supplied free text" {
			t.Fatalf("expected rating comment preserved, got %q", payload.Comment)
		}
	})

	t.Run("rating comment stripped under MetadataOnly", func(t *testing.T) {
		env := newConformanceEnv(t, withConformanceConfig(func(cfg *sigil.Config) {
			cfg.ContentCapture = sigil.ContentCaptureModeMetadataOnly
		}))

		_, err := env.Client.SubmitConversationRating(context.Background(), "conv-meta-rating", sigil.ConversationRatingInput{
			RatingID: "rat-meta",
			Rating:   sigil.ConversationRatingValueGood,
			Comment:  "user-supplied free text",
		})
		if err != nil {
			t.Fatalf("submit conversation rating: %v", err)
		}

		request := env.Rating.SingleRequest(t)
		var payload sigil.ConversationRatingInput
		if err := json.Unmarshal(request.Body, &payload); err != nil {
			t.Fatalf("decode rating request body: %v", err)
		}
		if payload.Comment != "" {
			t.Fatalf("expected rating comment stripped, got %q", payload.Comment)
		}
	})

	t.Run("provider call error redacted on span, raw in proto", func(t *testing.T) {
		env := newConformanceEnv(t, withConformanceConfig(func(cfg *sigil.Config) {
			cfg.ContentCapture = sigil.ContentCaptureModeFullWithMetadataSpans
		}))

		// Sentinel substring guaranteed not to appear in any error category
		// classifier output — if it shows up on the span we know the raw
		// provider error leaked.
		const leakMarker = "ignore previous instructions"
		rawErr := "provider returned HTTP 400: blocked content '" + leakMarker + "'"

		_, recorder := env.Client.StartGeneration(context.Background(), sigil.GenerationStart{
			ConversationID: "conv-fwms-error",
			AgentName:      "agent-fwms-error",
			Model:          conformanceModel,
			StartedAt:      time.Date(2026, 3, 12, 14, 15, 0, 0, time.UTC),
		})
		recorder.SetCallError(errors.New(rawErr))
		recorder.SetResult(sigil.Generation{
			Input:       []sigil.Message{sigil.UserTextMessage("x")},
			Output:      []sigil.Message{sigil.AssistantTextMessage("y")},
			CompletedAt: time.Date(2026, 3, 12, 14, 15, 1, 0, time.UTC),
		}, nil)
		recorder.End()
		if err := recorder.Err(); err != nil {
			t.Fatalf("record generation: %v", err)
		}

		env.Shutdown(t)

		// Proto export: raw provider error preserved (this is the
		// FullWithMetadataSpans contract — gRPC destination is trusted).
		generation := env.Ingest.SingleGeneration(t)
		if got := generation.GetCallError(); got != rawErr {
			t.Fatalf("proto call_error: got %q want %q", got, rawErr)
		}
		requireProtoMetadata(t, generation, "call_error", rawErr)

		// Span: raw error must not appear in status description or any
		// exception event attribute (e.g. exception.message,
		// exception.stacktrace).
		span := findSpan(t, env.Spans.Ended(), conformanceOperationName)
		if got := span.Status().Code; got != codes.Error {
			t.Fatalf("expected error span status, got %v", got)
		}
		if got := span.Status().Description; strings.Contains(got, leakMarker) {
			t.Fatalf("span status description leaks raw error: %q", got)
		}
		for _, ev := range span.Events() {
			for _, attr := range ev.Attributes {
				if strings.Contains(attr.Value.Emit(), leakMarker) {
					t.Errorf("span event %q attr %s leaks raw error: %q",
						ev.Name, attr.Key, attr.Value.Emit())
				}
			}
		}
		attrs := spanAttrs(span)
		requireSpanAttr(t, attrs, spanAttrErrorType, "provider_call_error")
	})
}

func TestConformance_ValidationAndErrorSemantics(t *testing.T) {
	t.Run("invalid generation", func(t *testing.T) {
		env := newConformanceEnv(t)

		_, recorder := env.Client.StartGeneration(context.Background(), sigil.GenerationStart{
			ConversationID: "conv-invalid",
			StartedAt:      time.Date(2026, 3, 12, 14, 4, 0, 0, time.UTC),
		})
		recorder.SetResult(sigil.Generation{
			Input:       []sigil.Message{sigil.UserTextMessage("hello")},
			Output:      []sigil.Message{sigil.AssistantTextMessage("hi")},
			CompletedAt: time.Date(2026, 3, 12, 14, 4, 1, 0, time.UTC),
		}, nil)
		recorder.End()

		if err := recorder.Err(); !errors.Is(err, sigil.ErrValidationFailed) {
			t.Fatalf("expected ErrValidationFailed, got %v", err)
		}
		if got := env.Ingest.GenerationCount(); got != 0 {
			t.Fatalf("expected no exports for invalid generation, got %d", got)
		}

		span := findSpan(t, env.Spans.Ended(), conformanceOperationName)
		if got := span.Status().Code; got != codes.Error {
			t.Fatalf("expected error span status, got %v", got)
		}
		attrs := spanAttrs(span)
		requireSpanAttr(t, attrs, spanAttrErrorType, "validation_error")
	})

	t.Run("provider call error", func(t *testing.T) {
		env := newConformanceEnv(t)

		_, recorder := env.Client.StartGeneration(context.Background(), sigil.GenerationStart{
			ConversationID: "conv-rate-limit",
			AgentName:      "agent-error",
			Model:          conformanceModel,
			StartedAt:      time.Date(2026, 3, 12, 14, 5, 0, 0, time.UTC),
		})
		recorder.SetCallError(errors.New("provider returned HTTP 429 rate limit"))
		recorder.SetResult(sigil.Generation{
			Input:       []sigil.Message{sigil.UserTextMessage("retry later")},
			Output:      []sigil.Message{sigil.AssistantTextMessage("rate limited")},
			CompletedAt: time.Date(2026, 3, 12, 14, 5, 1, 0, time.UTC),
		}, nil)
		recorder.End()
		if err := recorder.Err(); err != nil {
			t.Fatalf("expected no local error for provider call failure, got %v", err)
		}

		metrics := env.CollectMetrics(t)
		duration := findHistogram[float64](t, metrics, metricOperationDuration)
		requireHistogramPointWithAttrs(t, duration, map[string]string{
			spanAttrOperationName: conformanceOperationName,
			spanAttrProviderName:  conformanceModel.Provider,
			spanAttrRequestModel:  conformanceModel.Name,
			spanAttrAgentName:     "agent-error",
			spanAttrErrorType:     "provider_call_error",
			spanAttrErrorCategory: "rate_limit",
		})

		env.Shutdown(t)

		span := findSpan(t, env.Spans.Ended(), conformanceOperationName)
		if got := span.Status().Code; got != codes.Error {
			t.Fatalf("expected error span status, got %v", got)
		}
		attrs := spanAttrs(span)
		requireSpanAttr(t, attrs, spanAttrErrorType, "provider_call_error")
		requireSpanAttr(t, attrs, spanAttrErrorCategory, "rate_limit")

		generation := env.Ingest.SingleGeneration(t)
		if got := generation.GetCallError(); got != "provider returned HTTP 429 rate limit" {
			t.Fatalf("unexpected proto call error: got %q", got)
		}
		requireProtoMetadata(t, generation, "call_error", "provider returned HTTP 429 rate limit")
	})
}

func TestConformance_RatingHelper(t *testing.T) {
	env := newConformanceEnv(t, withConformanceConfig(func(cfg *sigil.Config) {
		cfg.GenerationExport.Headers = map[string]string{"X-Custom": "test"}
	}))

	response, err := env.Client.SubmitConversationRating(context.Background(), "conv-rated", sigil.ConversationRatingInput{
		RatingID: "rat-1",
		Rating:   sigil.ConversationRatingValueGood,
		Comment:  "looks good",
		Metadata: map[string]any{"channel": "assistant"},
	})
	if err != nil {
		t.Fatalf("submit conversation rating: %v", err)
	}

	requests := env.Rating.Requests()
	if len(requests) != 1 {
		t.Fatalf("expected exactly 1 rating request, got %d", len(requests))
	}

	request := requests[0]
	if request.Method != http.MethodPost {
		t.Fatalf("unexpected request method: got %s want %s", request.Method, http.MethodPost)
	}
	if request.Path != "/api/v1/conversations/conv-rated/ratings" {
		t.Fatalf("unexpected rating request path: %s", request.Path)
	}
	if got := request.Headers.Get("X-Custom"); got != "test" {
		t.Fatalf("expected X-Custom header, got %q", got)
	}

	var payload sigil.ConversationRatingInput
	if err := json.Unmarshal(request.Body, &payload); err != nil {
		t.Fatalf("decode rating request body: %v", err)
	}
	if payload.RatingID != "rat-1" {
		t.Fatalf("unexpected rating id: %q", payload.RatingID)
	}
	if payload.Rating != sigil.ConversationRatingValueGood {
		t.Fatalf("unexpected rating value: %q", payload.Rating)
	}
	if payload.Comment != "looks good" {
		t.Fatalf("unexpected comment: %q", payload.Comment)
	}
	if got := payload.Metadata["channel"]; got != "assistant" {
		t.Fatalf("unexpected metadata: %#v", payload.Metadata)
	}
	if response == nil || response.Rating.RatingID != "rat-1" {
		t.Fatalf("unexpected rating response: %#v", response)
	}
}

func TestConformance_ShutdownFlushesPendingGeneration(t *testing.T) {
	env := newConformanceEnv(t, withConformanceConfig(func(cfg *sigil.Config) {
		cfg.GenerationExport.BatchSize = 10
	}))

	recordGeneration(t, context.Background(), env, sigil.GenerationStart{
		ConversationID: "conv-shutdown",
		Model:          conformanceModel,
		StartedAt:      time.Date(2026, 3, 12, 14, 6, 0, 0, time.UTC),
	}, sigil.Generation{
		Input:       []sigil.Message{sigil.UserTextMessage("hello")},
		Output:      []sigil.Message{sigil.AssistantTextMessage("hi")},
		CompletedAt: time.Date(2026, 3, 12, 14, 6, 1, 0, time.UTC),
	})

	if got := env.Ingest.GenerationCount(); got != 0 {
		t.Fatalf("expected no exports before shutdown flush, got %d", got)
	}

	env.Shutdown(t)

	if got := env.Ingest.GenerationCount(); got != 1 {
		t.Fatalf("expected exactly 1 exported generation after shutdown, got %d", got)
	}
	generation := env.Ingest.SingleGeneration(t)
	if got := generation.GetConversationId(); got != "conv-shutdown" {
		t.Fatalf("unexpected shutdown-flushed conversation id: %q", got)
	}
}

func recordGeneration(t *testing.T, ctx context.Context, env *conformanceEnv, start sigil.GenerationStart, result sigil.Generation) {
	t.Helper()

	_, recorder := env.Client.StartGeneration(ctx, start)
	recorder.SetResult(result, nil)
	recorder.End()
	if err := recorder.Err(); err != nil {
		t.Fatalf("record generation: %v", err)
	}
}

func requireSyncGenerationMetrics(t *testing.T, env *conformanceEnv) {
	t.Helper()

	metrics := env.CollectMetrics(t)
	duration := findHistogram[float64](t, metrics, metricOperationDuration)
	if len(duration.DataPoints) == 0 {
		t.Fatalf("expected %s datapoints for conformance generation", metricOperationDuration)
	}
	requireNoHistogram(t, metrics, metricTimeToFirstToken)
}

func findGenerationByConversationID(t *testing.T, requests []*sigilv1.ExportGenerationsRequest, conversationID string) *sigilv1.Generation {
	t.Helper()

	for _, req := range requests {
		for _, generation := range req.GetGenerations() {
			if generation.GetConversationId() == conversationID {
				return generation
			}
		}
	}

	t.Fatalf("expected generation for conversation %q", conversationID)
	return nil
}

func int64Ptr(value int64) *int64 {
	return &value
}

func float64Ptr(value float64) *float64 {
	return &value
}

func stringPtr(value string) *string {
	return &value
}

func boolPtr(value bool) *bool {
	return &value
}

// modeExpect encodes the contract that every SDK is expected to honor for a
// given content capture mode: which fields stay in the proto, what marker is
// stamped, and what the OTel span sees.
type modeExpect struct {
	name                 string
	mode                 sigil.ContentCaptureMode
	marker               string
	protoContentStripped bool // system_prompt, message text/thinking, tool args/results, tools.description/schema all stripped
	spanTitlePresent     bool // generation span carries sigil.conversation.title
	protoCallErrorRaw    bool // proto.call_error is the raw provider message vs the error category
	spanRawError         bool // span echoes the raw provider message via exception events / status
}

// DEFAULT is intentionally absent here — it's the resolver fall-through, not a
// stable wire mode. The four entries below are the actual on-the-wire modes.
var contentCaptureModeMatrix = []modeExpect{
	{
		name:                 "full",
		mode:                 sigil.ContentCaptureModeFull,
		marker:               "full",
		protoContentStripped: false,
		spanTitlePresent:     true,
		protoCallErrorRaw:    true,
		spanRawError:         true,
	},
	{
		// NO_TOOL_CONTENT is generation-content-full; only tool spans gate
		// arguments/results via legacy include_content.
		name:                 "no_tool_content",
		mode:                 sigil.ContentCaptureModeNoToolContent,
		marker:               "no_tool_content",
		protoContentStripped: false,
		spanTitlePresent:     true,
		protoCallErrorRaw:    true,
		spanRawError:         true,
	},
	{
		name:                 "metadata_only",
		mode:                 sigil.ContentCaptureModeMetadataOnly,
		marker:               "metadata_only",
		protoContentStripped: true,
		spanTitlePresent:     false,
		protoCallErrorRaw:    false, // replaced with error category
		spanRawError:         false,
	},
	{
		name:                 "full_with_metadata_spans",
		mode:                 sigil.ContentCaptureModeFullWithMetadataSpans,
		marker:               "full_with_metadata_spans",
		protoContentStripped: false, // proto path keeps full content
		spanTitlePresent:     false, // but the span drops the title
		protoCallErrorRaw:    true,
		spanRawError:         false,
	},
}

const contentCaptureLeakMarker = "ignore previous instructions"

// fullContentGeneration returns a generation result with every content-bearing
// field populated, so a single fixture exercises the strip / preserve contract
// across all modes.
func fullContentGeneration() sigil.Generation {
	return sigil.Generation{
		SystemPrompt: "You are helpful.",
		Input: []sigil.Message{
			sigil.UserTextMessage("What is the weather?"),
			{
				Role: sigil.RoleTool,
				Parts: []sigil.Part{
					{
						Kind: sigil.PartKindToolResult,
						ToolResult: &sigil.ToolResult{
							ToolCallID: "call_1",
							Name:       "weather",
							Content:    "sunny 18C",
						},
					},
				},
			},
		},
		Output: []sigil.Message{
			{
				Role: sigil.RoleAssistant,
				Parts: []sigil.Part{
					{Kind: sigil.PartKindThinking, Thinking: "let me think about weather"},
					{
						Kind: sigil.PartKindToolCall,
						ToolCall: &sigil.ToolCall{
							ID:        "call_1",
							Name:      "weather",
							InputJSON: json.RawMessage(`{"city":"Paris"}`),
						},
					},
					{Kind: sigil.PartKindText, Text: "It's 18C and sunny in Paris."},
				},
			},
		},
		Tools: []sigil.ToolDefinition{
			{
				Name:        "weather",
				Description: "Get weather info",
				Type:        "function",
				InputSchema: json.RawMessage(`{"type":"object"}`),
			},
		},
		Usage: sigil.TokenUsage{
			InputTokens:  120,
			OutputTokens: 42,
			TotalTokens:  162,
		},
		StopReason:  "end_turn",
		CompletedAt: time.Date(2026, 3, 12, 14, 10, 1, 0, time.UTC),
	}
}

func TestConformance_ContentCaptureModeMatrix(t *testing.T) {
	// Generation proto + span coverage across every on-the-wire mode. One
	// fixture, one subtest per mode, expectations driven by the matrix above.
	for _, tc := range contentCaptureModeMatrix {
		t.Run(tc.name+"/generation", func(t *testing.T) {
			env := newConformanceEnv(t, withConformanceConfig(func(cfg *sigil.Config) {
				cfg.ContentCapture = tc.mode
			}))

			const title = "Sensitive conversation"
			_, rec := env.Client.StartGeneration(context.Background(), sigil.GenerationStart{
				ConversationID:    "conv-matrix",
				ConversationTitle: title,
				Model:             conformanceModel,
				SystemPrompt:      "You are helpful.",
				StartedAt:         time.Date(2026, 3, 12, 14, 10, 0, 0, time.UTC),
			})
			rec.SetResult(fullContentGeneration(), nil)
			rec.End()
			if err := rec.Err(); err != nil {
				t.Fatalf("record generation: %v", err)
			}

			env.Shutdown(t)

			gen := env.Ingest.SingleGeneration(t)
			requireProtoMetadata(t, gen, metadataKeyContentCaptureMode, tc.marker)

			// Content fields: stripped only under METADATA_ONLY.
			assertProtoContentField(t, "system_prompt", gen.GetSystemPrompt(), "You are helpful.", tc.protoContentStripped)
			assertProtoContentField(t, "input[0].text", gen.GetInput()[0].GetParts()[0].GetText(), "What is the weather?", tc.protoContentStripped)
			assertProtoContentField(t, "output[0].thinking", gen.GetOutput()[0].GetParts()[0].GetThinking(), "let me think about weather", tc.protoContentStripped)
			assertProtoContentField(t, "output[0].tool_call.input_json", string(gen.GetOutput()[0].GetParts()[1].GetToolCall().GetInputJson()), `{"city":"Paris"}`, tc.protoContentStripped)
			assertProtoContentField(t, "output[0].text", gen.GetOutput()[0].GetParts()[2].GetText(), "It's 18C and sunny in Paris.", tc.protoContentStripped)
			assertProtoContentField(t, "input[1].tool_result.content", gen.GetInput()[1].GetParts()[0].GetToolResult().GetContent(), "sunny 18C", tc.protoContentStripped)
			assertProtoContentField(t, "tools[0].description", gen.GetTools()[0].GetDescription(), "Get weather info", tc.protoContentStripped)
			assertProtoContentField(t, "tools[0].input_schema_json", string(gen.GetTools()[0].GetInputSchemaJson()), `{"type":"object"}`, tc.protoContentStripped)

			// Structural fields are always preserved.
			if got := len(gen.GetInput()); got != 2 {
				t.Fatalf("unexpected input length: got %d want 2", got)
			}
			if got := gen.GetOutput()[0].GetParts()[1].GetToolCall().GetName(); got != "weather" {
				t.Fatalf("unexpected tool_call.name: got %q", got)
			}
			if got := gen.GetUsage().GetInputTokens(); got != 120 {
				t.Fatalf("unexpected usage.input_tokens: got %d want 120", got)
			}

			// Conversation title metadata mirror: present iff the proto keeps it.
			if tc.protoContentStripped {
				requireProtoMetadataAbsent(t, gen, metadataKeyConversation)
			} else {
				requireProtoMetadata(t, gen, metadataKeyConversation, title)
			}

			// Span path: title presence is what the mode advertises.
			span := findSpan(t, env.Spans.Ended(), conformanceOperationName)
			attrs := spanAttrs(span)
			if tc.spanTitlePresent {
				requireSpanAttr(t, attrs, spanAttrConversationTitle, title)
			} else {
				requireSpanAttrAbsent(t, attrs, spanAttrConversationTitle)
			}
		})

		t.Run(tc.name+"/call_error", func(t *testing.T) {
			env := newConformanceEnv(t, withConformanceConfig(func(cfg *sigil.Config) {
				cfg.ContentCapture = tc.mode
			}))

			rawErr := "provider returned HTTP 400: blocked content '" + contentCaptureLeakMarker + "'"
			_, rec := env.Client.StartGeneration(context.Background(), sigil.GenerationStart{
				ConversationID: "conv-matrix-error",
				AgentName:      "agent-matrix-error",
				Model:          conformanceModel,
				StartedAt:      time.Date(2026, 3, 12, 14, 15, 0, 0, time.UTC),
			})
			rec.SetCallError(errors.New(rawErr))
			rec.SetResult(sigil.Generation{
				Input:       []sigil.Message{sigil.UserTextMessage("x")},
				Output:      []sigil.Message{sigil.AssistantTextMessage("y")},
				CompletedAt: time.Date(2026, 3, 12, 14, 15, 1, 0, time.UTC),
			}, nil)
			rec.End()
			if err := rec.Err(); err != nil {
				t.Fatalf("record generation: %v", err)
			}

			env.Shutdown(t)

			gen := env.Ingest.SingleGeneration(t)
			if tc.protoCallErrorRaw {
				if got := gen.GetCallError(); got != rawErr {
					t.Fatalf("proto call_error: got %q want raw", got)
				}
				requireProtoMetadata(t, gen, "call_error", rawErr)
			} else {
				if got := gen.GetCallError(); got == rawErr || got == "" {
					t.Fatalf("proto call_error: got %q, want non-empty error category", got)
				}
				requireProtoMetadataAbsent(t, gen, "call_error")
			}

			span := findSpan(t, env.Spans.Ended(), conformanceOperationName)
			if tc.spanRawError {
				if desc := span.Status().Description; !strings.Contains(desc, contentCaptureLeakMarker) {
					t.Fatalf("expected span status to echo raw error, got %q", desc)
				}
			} else {
				if desc := span.Status().Description; strings.Contains(desc, contentCaptureLeakMarker) {
					t.Fatalf("span status description leaks raw error: %q", desc)
				}
				for _, ev := range span.Events() {
					for _, attr := range ev.Attributes {
						if strings.Contains(attr.Value.Emit(), contentCaptureLeakMarker) {
							t.Errorf("span event %q attr %s leaks raw error: %q", ev.Name, attr.Key, attr.Value.Emit())
						}
					}
				}
				requireSpanAttr(t, spanAttrs(span), spanAttrErrorType, "provider_call_error")
			}
		})
	}

	t.Run("streaming_full_with_metadata_spans", func(t *testing.T) {
		// Streaming changes the span operation name from generateText to
		// streamText but the redaction logic is shared with non-streaming.
		// This subtest catches regressions where the two paths drift apart.
		env := newConformanceEnv(t, withConformanceConfig(func(cfg *sigil.Config) {
			cfg.ContentCapture = sigil.ContentCaptureModeFullWithMetadataSpans
		}))

		const title = "Sensitive streaming conversation"
		_, rec := env.Client.StartStreamingGeneration(context.Background(), sigil.GenerationStart{
			ConversationID:    "conv-stream-fwms",
			ConversationTitle: title,
			Model:             conformanceModel,
			SystemPrompt:      "Be helpful.",
			StartedAt:         time.Date(2026, 3, 12, 14, 20, 0, 0, time.UTC),
		})
		rec.SetResult(sigil.Generation{
			SystemPrompt: "Be helpful.",
			Input:        []sigil.Message{sigil.UserTextMessage("hello")},
			Output:       []sigil.Message{sigil.AssistantTextMessage("hi")},
			Usage:        sigil.TokenUsage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
			CompletedAt:  time.Date(2026, 3, 12, 14, 20, 1, 0, time.UTC),
		}, nil)
		rec.End()
		if err := rec.Err(); err != nil {
			t.Fatalf("record streaming generation: %v", err)
		}

		env.Shutdown(t)

		gen := env.Ingest.SingleGeneration(t)
		if got := gen.GetSystemPrompt(); got != "Be helpful." {
			t.Fatalf("streaming proto system_prompt stripped: got %q", got)
		}
		if got := gen.GetInput()[0].GetParts()[0].GetText(); got != "hello" {
			t.Fatalf("streaming proto input text stripped: got %q", got)
		}
		requireProtoMetadata(t, gen, metadataKeyConversation, title)
		requireProtoMetadata(t, gen, metadataKeyContentCaptureMode, "full_with_metadata_spans")

		streamSpan := findSpan(t, env.Spans.Ended(), conformanceStreamOperation)
		requireSpanAttrAbsent(t, spanAttrs(streamSpan), spanAttrConversationTitle)
	})
}

func assertProtoContentField(t *testing.T, name, got, want string, expectStripped bool) {
	t.Helper()
	if expectStripped {
		if got != "" {
			t.Errorf("%s: expected stripped (empty), got %q", name, got)
		}
		return
	}
	if got != want {
		t.Errorf("%s: got %q want %q", name, got, want)
	}
}
