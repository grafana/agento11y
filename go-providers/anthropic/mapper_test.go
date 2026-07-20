package anthropic

import (
	"encoding/json"
	"testing"

	asdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/grafana/agento11y/go/agento11y"
)

func TestFromRequestResponse(t *testing.T) {
	req := testRequest()
	resp := &asdk.BetaMessage{
		ID:         "msg_1",
		Model:      asdk.Model("claude-sonnet-4-5"),
		StopReason: asdk.BetaStopReasonEndTurn,
		Content: []asdk.BetaContentBlockUnion{
			{Type: "text", Text: "It's 18C and sunny."},
			{Type: "thinking", Thinking: "answer done"},
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

	generation, err := FromRequestResponse(req, resp,
		WithConversationID("conv-9b2f"),
		WithConversationTitle("Paris weather"),
		WithAgentName("agent-anthropic"),
		WithAgentVersion("v-anthropic"),
		WithTag("tenant", "t-123"),
	)
	if err != nil {
		t.Fatalf("from request/response: %v", err)
	}

	if generation.Model.Provider != "anthropic" {
		t.Fatalf("expected provider anthropic, got %q", generation.Model.Provider)
	}
	if generation.Model.Name != "claude-sonnet-4-5" {
		t.Fatalf("expected model claude-sonnet-4-5, got %q", generation.Model.Name)
	}
	if generation.ConversationID != "conv-9b2f" {
		t.Fatalf("expected conversation id conv-9b2f, got %q", generation.ConversationID)
	}
	if generation.ConversationTitle != "Paris weather" {
		t.Fatalf("expected conversation title Paris weather, got %q", generation.ConversationTitle)
	}
	if generation.AgentName != "agent-anthropic" {
		t.Fatalf("expected agent-anthropic, got %q", generation.AgentName)
	}
	if generation.AgentVersion != "v-anthropic" {
		t.Fatalf("expected v-anthropic, got %q", generation.AgentVersion)
	}
	if generation.ResponseID != "msg_1" {
		t.Fatalf("expected response id msg_1, got %q", generation.ResponseID)
	}
	if generation.ResponseModel != "claude-sonnet-4-5" {
		t.Fatalf("expected response model claude-sonnet-4-5, got %q", generation.ResponseModel)
	}
	if generation.SystemPrompt != "Be precise." {
		t.Fatalf("unexpected system prompt: %q", generation.SystemPrompt)
	}
	if generation.Usage.TotalTokens != 162 {
		t.Fatalf("expected total tokens 162, got %d", generation.Usage.TotalTokens)
	}
	if generation.Usage.CacheReadInputTokens != 30 {
		t.Fatalf("expected cache read input tokens 30, got %d", generation.Usage.CacheReadInputTokens)
	}
	if generation.Usage.CacheWriteInputTokens != 10 {
		t.Fatalf("expected cache write tokens 10, got %d", generation.Usage.CacheWriteInputTokens)
	}
	if generation.MaxTokens == nil || *generation.MaxTokens != 512 {
		t.Fatalf("expected max tokens 512, got %v", generation.MaxTokens)
	}
	if generation.Temperature == nil || *generation.Temperature != 0.3 {
		t.Fatalf("expected temperature 0.3, got %v", generation.Temperature)
	}
	if generation.TopP == nil || *generation.TopP != 0.8 {
		t.Fatalf("expected top_p 0.8, got %v", generation.TopP)
	}
	if generation.ToolChoice == nil || *generation.ToolChoice != `{"name":"weather","type":"tool"}` {
		t.Fatalf("unexpected tool choice %v", generation.ToolChoice)
	}
	if generation.ThinkingEnabled == nil || !*generation.ThinkingEnabled {
		t.Fatalf("expected thinking enabled true, got %v", generation.ThinkingEnabled)
	}
	if generation.Metadata == nil {
		t.Fatalf("expected metadata map")
	}
	if generation.Metadata["agento11y.gen_ai.request.thinking.budget_tokens"] != int64(1024) {
		t.Fatalf("expected thinking budget metadata 1024, got %v", generation.Metadata["agento11y.gen_ai.request.thinking.budget_tokens"])
	}
	if generation.Metadata["agento11y.gen_ai.usage.server_tool_use.web_search_requests"] != int64(2) {
		t.Fatalf("expected server tool web_search_requests=2, got %v", generation.Metadata["agento11y.gen_ai.usage.server_tool_use.web_search_requests"])
	}
	if generation.Metadata["agento11y.gen_ai.usage.server_tool_use.web_fetch_requests"] != int64(1) {
		t.Fatalf("expected server tool web_fetch_requests=1, got %v", generation.Metadata["agento11y.gen_ai.usage.server_tool_use.web_fetch_requests"])
	}
	if generation.Metadata["agento11y.gen_ai.usage.server_tool_use.total_requests"] != int64(3) {
		t.Fatalf("expected server tool total_requests=3, got %v", generation.Metadata["agento11y.gen_ai.usage.server_tool_use.total_requests"])
	}
	if generation.Tags["tenant"] != "t-123" {
		t.Fatalf("expected tenant tag")
	}
	if len(generation.Artifacts) != 0 {
		t.Fatalf("expected 0 artifacts by default, got %d", len(generation.Artifacts))
	}
	if len(generation.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(generation.Tools))
	}
	if !generation.Tools[0].Deferred {
		t.Fatalf("expected mapped tool deferred=true")
	}

	hasToolRole := false
	for _, message := range generation.Input {
		if message.Role == agento11y.RoleTool {
			hasToolRole = true
			if len(message.Parts) != 1 || message.Parts[0].ToolResult == nil {
				t.Fatalf("expected single tool_result part, got %#v", message.Parts)
			}
			if message.Parts[0].ToolResult.ToolCallID != "toolu_1" {
				t.Fatalf("expected Anthropic tool_result tool_call_id toolu_1, got %q", message.Parts[0].ToolResult.ToolCallID)
			}
		}
	}
	if !hasToolRole {
		t.Fatalf("expected mapped tool_result message with tool role")
	}
}

func TestFromRequestResponseMapsImageInput(t *testing.T) {
	req := testRequest()
	req.Messages[0].Content = append(req.Messages[0].Content, asdk.NewBetaImageBlock(asdk.BetaBase64ImageSourceParam{
		Data:      "abc123",
		MediaType: asdk.BetaBase64ImageSourceMediaTypeImagePNG,
	}))
	resp := &asdk.BetaMessage{
		ID:         "msg_image",
		Model:      asdk.Model("claude-sonnet-4-5"),
		StopReason: asdk.BetaStopReasonEndTurn,
		Content: []asdk.BetaContentBlockUnion{
			{Type: "text", Text: "I can see it."},
		},
	}

	generation, err := FromRequestResponse(req, resp)
	if err != nil {
		t.Fatalf("from request/response: %v", err)
	}

	if len(generation.Input) == 0 || len(generation.Input[0].Parts) != 2 {
		t.Fatalf("expected text and media input parts, got %#v", generation.Input)
	}
	media := generation.Input[0].Parts[1]
	if media.Kind != agento11y.PartKindMedia {
		t.Fatalf("expected media part, got %q", media.Kind)
	}
	if media.Media == nil {
		t.Fatal("expected media payload")
	}
	if media.Media.Kind != "image" {
		t.Fatalf("expected image media kind, got %q", media.Media.Kind)
	}
	if media.Media.URL != "data:image/png;base64,abc123" {
		t.Fatalf("unexpected media URL %q", media.Media.URL)
	}
	if media.Media.MIMEType != "image/png" {
		t.Fatalf("unexpected media MIME type %q", media.Media.MIMEType)
	}
	if media.Metadata.ProviderType != "image" {
		t.Fatalf("unexpected provider type %q", media.Metadata.ProviderType)
	}
}

func TestFromRequestResponseInfersDataURLImageMIMEType(t *testing.T) {
	const dataURL = "data:image/gif;base64,R0lGODlh"

	req := testRequest()
	req.Messages[0].Content = append(req.Messages[0].Content, asdk.NewBetaImageBlock(asdk.BetaURLImageSourceParam{
		URL: dataURL,
	}))
	resp := &asdk.BetaMessage{
		ID:         "msg_image_url",
		Model:      asdk.Model("claude-sonnet-4-5"),
		StopReason: asdk.BetaStopReasonEndTurn,
		Content: []asdk.BetaContentBlockUnion{
			{Type: "text", Text: "I can see it."},
		},
	}

	generation, err := FromRequestResponse(req, resp)
	if err != nil {
		t.Fatalf("from request/response: %v", err)
	}

	if len(generation.Input) == 0 || len(generation.Input[0].Parts) != 2 {
		t.Fatalf("expected text and media input parts, got %#v", generation.Input)
	}
	media := generation.Input[0].Parts[1]
	if media.Kind != agento11y.PartKindMedia {
		t.Fatalf("expected media part, got %q", media.Kind)
	}
	if media.Media == nil {
		t.Fatal("expected media payload")
	}
	if media.Media.URL != dataURL {
		t.Fatalf("unexpected media URL %q", media.Media.URL)
	}
	if media.Media.MIMEType != "image/gif" {
		t.Fatalf("unexpected media MIME type %q", media.Media.MIMEType)
	}
}

func TestFromStream(t *testing.T) {
	req := testRequest()
	summary := StreamSummary{
		Events: []asdk.BetaRawMessageStreamEventUnion{
			{
				Type: "message_start",
				Message: asdk.BetaMessage{
					ID:    "msg_stream_1",
					Model: asdk.Model("claude-sonnet-4-5"),
				},
			},
			{
				Type:  "content_block_start",
				Index: 0,
				ContentBlock: asdk.BetaRawContentBlockStartEventContentBlockUnion{
					Type:     "thinking",
					Thinking: "look up tool",
				},
			},
			{
				Type:  "content_block_start",
				Index: 1,
				ContentBlock: asdk.BetaRawContentBlockStartEventContentBlockUnion{
					Type:  "tool_use",
					ID:    "toolu_2",
					Name:  "weather",
					Input: map[string]any{"city": "Paris"},
				},
			},
			{
				Type:  "content_block_start",
				Index: 2,
				ContentBlock: asdk.BetaRawContentBlockStartEventContentBlockUnion{
					Type: "text",
					Text: "It's 18C and sunny.",
				},
			},
			{
				Type: "message_delta",
				Delta: asdk.BetaRawMessageStreamEventUnionDelta{
					StopReason: asdk.BetaStopReasonEndTurn,
				},
				Usage: asdk.BetaMessageDeltaUsage{
					InputTokens:              80,
					OutputTokens:             25,
					CacheReadInputTokens:     8,
					CacheCreationInputTokens: 4,
					ServerToolUse: asdk.BetaServerToolUsage{
						WebSearchRequests: 1,
						WebFetchRequests:  2,
					},
				},
			},
		},
	}

	generation, err := FromStream(req, summary,
		WithConversationID("conv-stream"),
		WithAgentName("agent-anthropic-stream"),
		WithAgentVersion("v-anthropic-stream"),
	)
	if err != nil {
		t.Fatalf("from stream: %v", err)
	}

	if generation.ConversationID != "conv-stream" {
		t.Fatalf("expected conv-stream, got %q", generation.ConversationID)
	}
	if generation.AgentName != "agent-anthropic-stream" {
		t.Fatalf("expected agent-anthropic-stream, got %q", generation.AgentName)
	}
	if generation.AgentVersion != "v-anthropic-stream" {
		t.Fatalf("expected v-anthropic-stream, got %q", generation.AgentVersion)
	}
	if generation.ResponseID != "msg_stream_1" {
		t.Fatalf("expected response id msg_stream_1, got %q", generation.ResponseID)
	}
	if generation.StopReason != "end_turn" {
		t.Fatalf("expected end_turn stop reason, got %q", generation.StopReason)
	}
	if generation.ResponseModel != "claude-sonnet-4-5" {
		t.Fatalf("expected response model claude-sonnet-4-5, got %q", generation.ResponseModel)
	}
	if generation.Usage.TotalTokens != 105 {
		t.Fatalf("expected total tokens 105, got %d", generation.Usage.TotalTokens)
	}
	if generation.MaxTokens == nil || *generation.MaxTokens != 512 {
		t.Fatalf("expected max tokens 512, got %v", generation.MaxTokens)
	}
	if generation.Temperature == nil || *generation.Temperature != 0.3 {
		t.Fatalf("expected temperature 0.3, got %v", generation.Temperature)
	}
	if generation.TopP == nil || *generation.TopP != 0.8 {
		t.Fatalf("expected top_p 0.8, got %v", generation.TopP)
	}
	if generation.ToolChoice == nil || *generation.ToolChoice != `{"name":"weather","type":"tool"}` {
		t.Fatalf("unexpected tool choice %v", generation.ToolChoice)
	}
	if generation.ThinkingEnabled == nil || !*generation.ThinkingEnabled {
		t.Fatalf("expected thinking enabled true, got %v", generation.ThinkingEnabled)
	}
	if generation.Metadata == nil {
		t.Fatalf("expected metadata map")
	}
	if generation.Metadata["agento11y.gen_ai.request.thinking.budget_tokens"] != int64(1024) {
		t.Fatalf("expected thinking budget metadata 1024, got %v", generation.Metadata["agento11y.gen_ai.request.thinking.budget_tokens"])
	}
	if generation.Metadata["agento11y.gen_ai.usage.server_tool_use.web_search_requests"] != int64(1) {
		t.Fatalf("expected server tool web_search_requests=1, got %v", generation.Metadata["agento11y.gen_ai.usage.server_tool_use.web_search_requests"])
	}
	if generation.Metadata["agento11y.gen_ai.usage.server_tool_use.web_fetch_requests"] != int64(2) {
		t.Fatalf("expected server tool web_fetch_requests=2, got %v", generation.Metadata["agento11y.gen_ai.usage.server_tool_use.web_fetch_requests"])
	}
	if generation.Metadata["agento11y.gen_ai.usage.server_tool_use.total_requests"] != int64(3) {
		t.Fatalf("expected server tool total_requests=3, got %v", generation.Metadata["agento11y.gen_ai.usage.server_tool_use.total_requests"])
	}
	if len(generation.Artifacts) != 0 {
		t.Fatalf("expected 0 artifacts by default, got %d", len(generation.Artifacts))
	}
	if len(generation.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(generation.Tools))
	}
	if !generation.Tools[0].Deferred {
		t.Fatalf("expected mapped tool deferred=true")
	}
}

func TestFromStream_DeltaAccumulation(t *testing.T) {
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
			// Block 0: thinking via deltas
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
			// Block 1: text via deltas (real streaming behavior)
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
			// Block 2: tool_use with partial_json deltas
			// Real Anthropic streams send "input": {} on start, which
			// deserializes as a non-nil empty map.
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
		WithConversationID("conv-delta"),
		WithAgentName("agent-delta"),
	)
	if err != nil {
		t.Fatalf("from stream: %v", err)
	}

	if generation.ResponseID != "msg_delta_1" {
		t.Fatalf("expected response id msg_delta_1, got %q", generation.ResponseID)
	}
	if generation.StopReason != "tool_use" {
		t.Fatalf("expected tool_use stop reason, got %q", generation.StopReason)
	}
	if len(generation.Output) != 1 {
		t.Fatalf("expected 1 output message, got %d", len(generation.Output))
	}

	output := generation.Output[0]
	if output.Role != agento11y.RoleAssistant {
		t.Fatalf("expected assistant role, got %q", output.Role)
	}
	if len(output.Parts) != 3 {
		t.Fatalf("expected 3 parts (thinking + text + tool_use), got %d", len(output.Parts))
	}

	// Thinking part (accumulated from deltas)
	if output.Parts[0].Kind != agento11y.PartKindThinking {
		t.Fatalf("expected thinking part, got %q", output.Parts[0].Kind)
	}
	if output.Parts[0].Thinking != "let me think about this" {
		t.Fatalf("expected accumulated thinking, got %q", output.Parts[0].Thinking)
	}

	// Text part (accumulated from deltas)
	if output.Parts[1].Kind != agento11y.PartKindText {
		t.Fatalf("expected text part, got %q", output.Parts[1].Kind)
	}
	if output.Parts[1].Text != "Hello, world!" {
		t.Fatalf("expected accumulated text 'Hello, world!', got %q", output.Parts[1].Text)
	}

	// Tool use part (accumulated from partial_json deltas)
	if output.Parts[2].Kind != agento11y.PartKindToolCall {
		t.Fatalf("expected tool_call part, got %q", output.Parts[2].Kind)
	}
	if output.Parts[2].ToolCall.Name != "weather" {
		t.Fatalf("expected tool name weather, got %q", output.Parts[2].ToolCall.Name)
	}
	if output.Parts[2].ToolCall.ID != "toolu_1" {
		t.Fatalf("expected tool id toolu_1, got %q", output.Parts[2].ToolCall.ID)
	}
	if string(output.Parts[2].ToolCall.InputJSON) != `{"city":"Berlin"}` {
		t.Fatalf("expected tool input JSON, got %q", string(output.Parts[2].ToolCall.InputJSON))
	}
}

func TestFromStream_DeltaWithoutContentBlockStart(t *testing.T) {
	req := testRequest()
	summary := StreamSummary{
		Events: []asdk.BetaRawMessageStreamEventUnion{
			{
				Type: "message_start",
				Message: asdk.BetaMessage{
					ID:    "msg_fallback_1",
					Model: asdk.Model("claude-sonnet-4-5"),
				},
			},
			// No content_block_start for index 0 — text deltas arrive directly
			{
				Type:  "content_block_delta",
				Index: 0,
				Delta: asdk.BetaRawMessageStreamEventUnionDelta{Text: "orphan "},
			},
			{
				Type:  "content_block_delta",
				Index: 0,
				Delta: asdk.BetaRawMessageStreamEventUnionDelta{Text: "text"},
			},
			// No content_block_start for index 1 — thinking deltas only
			{
				Type:  "content_block_delta",
				Index: 1,
				Delta: asdk.BetaRawMessageStreamEventUnionDelta{Thinking: "hmm"},
			},
			{
				Type: "message_delta",
				Delta: asdk.BetaRawMessageStreamEventUnionDelta{
					StopReason: asdk.BetaStopReasonEndTurn,
				},
				Usage: asdk.BetaMessageDeltaUsage{
					InputTokens:  10,
					OutputTokens: 5,
				},
			},
		},
	}

	generation, err := FromStream(req, summary,
		WithConversationID("conv-fallback"),
		WithAgentName("agent-fallback"),
	)
	if err != nil {
		t.Fatalf("from stream: %v", err)
	}

	if len(generation.Output) == 0 {
		t.Fatal("expected output messages but got none")
	}

	output := generation.Output[0]
	if len(output.Parts) != 2 {
		t.Fatalf("expected 2 parts (text + thinking), got %d", len(output.Parts))
	}

	if output.Parts[0].Kind != agento11y.PartKindText {
		t.Fatalf("expected text part at index 0, got %q", output.Parts[0].Kind)
	}
	if output.Parts[0].Text != "orphan text" {
		t.Fatalf("expected 'orphan text', got %q", output.Parts[0].Text)
	}

	if output.Parts[1].Kind != agento11y.PartKindThinking {
		t.Fatalf("expected thinking part at index 1, got %q", output.Parts[1].Kind)
	}
	if output.Parts[1].Thinking != "hmm" {
		t.Fatalf("expected 'hmm', got %q", output.Parts[1].Thinking)
	}
}

func TestFromStreamWithFinalMessageAppendsEventsArtifact(t *testing.T) {
	req := testRequest()
	finalMessage := &asdk.BetaMessage{
		ID:         "msg_final",
		Model:      asdk.Model("claude-sonnet-4-5"),
		StopReason: asdk.BetaStopReasonEndTurn,
		Content: []asdk.BetaContentBlockUnion{
			{Type: "text", Text: "final output"},
		},
		Usage: asdk.BetaUsage{
			InputTokens:  10,
			OutputTokens: 4,
		},
	}

	generation, err := FromStream(req, StreamSummary{
		Events: []asdk.BetaRawMessageStreamEventUnion{
			{Type: "message_stop"},
		},
		FinalMessage: finalMessage,
	})
	if err != nil {
		t.Fatalf("from stream with final message: %v", err)
	}

	if len(generation.Artifacts) != 0 {
		t.Fatalf("expected 0 artifacts by default, got %d", len(generation.Artifacts))
	}
}

func TestFromStreamWithRawArtifacts(t *testing.T) {
	req := testRequest()
	finalMessage := &asdk.BetaMessage{
		ID:         "msg_final",
		Model:      asdk.Model("claude-sonnet-4-5"),
		StopReason: asdk.BetaStopReasonEndTurn,
		Content: []asdk.BetaContentBlockUnion{
			{Type: "text", Text: "final output"},
		},
		Usage: asdk.BetaUsage{
			InputTokens:  10,
			OutputTokens: 4,
		},
	}

	generation, err := FromStream(req, StreamSummary{
		Events: []asdk.BetaRawMessageStreamEventUnion{
			{Type: "message_stop"},
		},
		FinalMessage: finalMessage,
	}, WithRawArtifacts())
	if err != nil {
		t.Fatalf("from stream with final message: %v", err)
	}

	if len(generation.Artifacts) != 4 {
		t.Fatalf("expected 4 artifacts with raw artifact opt-in, got %d", len(generation.Artifacts))
	}
}

func TestFromRequestResponseMapsThinkingDisabled(t *testing.T) {
	req := testRequest()
	disabled := asdk.NewBetaThinkingConfigDisabledParam()
	req.Thinking = asdk.BetaThinkingConfigParamUnion{
		OfDisabled: &disabled,
	}

	resp := &asdk.BetaMessage{
		ID:         "msg_1",
		Model:      asdk.Model("claude-sonnet-4-5"),
		StopReason: asdk.BetaStopReasonEndTurn,
		Content: []asdk.BetaContentBlockUnion{
			{Type: "text", Text: "done"},
		},
	}

	generation, err := FromRequestResponse(req, resp)
	if err != nil {
		t.Fatalf("from request/response: %v", err)
	}

	if generation.ThinkingEnabled == nil || *generation.ThinkingEnabled {
		t.Fatalf("expected thinking enabled false, got %v", generation.ThinkingEnabled)
	}
}

func TestFromRequestResponseMapsToolDeferredDefaultFalse(t *testing.T) {
	req := testRequest()
	req.Tools = []asdk.BetaToolUnionParam{
		asdk.BetaToolUnionParamOfTool(asdk.BetaToolInputSchemaParam{
			Type: "object",
			Properties: map[string]any{
				"city": map[string]any{
					"type": "string",
				},
			},
			Required: []string{"city"},
		}, "weather"),
	}

	resp := &asdk.BetaMessage{
		ID:         "msg_1",
		Model:      asdk.Model("claude-sonnet-4-5"),
		StopReason: asdk.BetaStopReasonEndTurn,
		Content: []asdk.BetaContentBlockUnion{
			{Type: "text", Text: "done"},
		},
	}

	generation, err := FromRequestResponse(req, resp)
	if err != nil {
		t.Fatalf("from request/response: %v", err)
	}
	if len(generation.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(generation.Tools))
	}
	if generation.Tools[0].Deferred {
		t.Fatalf("expected mapped tool deferred=false when defer_loading is unset")
	}
}

func TestFromRequestResponsePreservesWhitespaceInTextAndSystemPrompt(t *testing.T) {
	req := asdk.BetaMessageNewParams{
		MaxTokens: 1,
		Model:     asdk.Model("claude-sonnet-4-5"),
		System: []asdk.BetaTextBlockParam{
			{Text: "  first system  ", Type: "text"},
			{Text: "  second system  ", Type: "text"},
		},
		Messages: []asdk.BetaMessageParam{
			{
				Role: asdk.BetaMessageParamRoleUser,
				Content: []asdk.BetaContentBlockParamUnion{
					asdk.NewBetaTextBlock("  user content with literal \\\\n\\\\n  "),
				},
			},
		},
	}

	resp := &asdk.BetaMessage{
		ID:         "msg_whitespace",
		Model:      asdk.Model("claude-sonnet-4-5"),
		StopReason: asdk.BetaStopReasonEndTurn,
		Content: []asdk.BetaContentBlockUnion{
			{Type: "text", Text: "\n  assistant content  \n"},
		},
	}

	generation, err := FromRequestResponse(req, resp)
	if err != nil {
		t.Fatalf("from request/response: %v", err)
	}

	if generation.SystemPrompt != "  first system  \n\n  second system  " {
		t.Fatalf("unexpected system prompt %q", generation.SystemPrompt)
	}
	if len(generation.Input) != 1 || len(generation.Input[0].Parts) != 1 {
		t.Fatalf("expected one input text part, got %#v", generation.Input)
	}
	if generation.Input[0].Parts[0].Text != "  user content with literal \\\\n\\\\n  " {
		t.Fatalf("unexpected input text %q", generation.Input[0].Parts[0].Text)
	}
	if len(generation.Output) != 1 || len(generation.Output[0].Parts) != 1 {
		t.Fatalf("expected one output text part, got %#v", generation.Output)
	}
	if generation.Output[0].Parts[0].Text != "\n  assistant content  \n" {
		t.Fatalf("unexpected output text %q", generation.Output[0].Parts[0].Text)
	}
}

func TestFromStreamPreservesWhitespaceOnlyParts(t *testing.T) {
	req := asdk.BetaMessageNewParams{
		MaxTokens: 1,
		Model:     asdk.Model("claude-sonnet-4-5"),
	}

	summary := StreamSummary{
		Events: []asdk.BetaRawMessageStreamEventUnion{
			{
				Type: "message_start",
				Message: asdk.BetaMessage{
					ID:    "msg_stream_whitespace",
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
				Delta: asdk.BetaRawMessageStreamEventUnionDelta{Thinking: "   "},
			},
			{
				Type:  "content_block_start",
				Index: 1,
				ContentBlock: asdk.BetaRawContentBlockStartEventContentBlockUnion{
					Type: "text",
					Text: "  ",
				},
			},
			{
				Type: "message_delta",
				Delta: asdk.BetaRawMessageStreamEventUnionDelta{
					StopReason: asdk.BetaStopReasonEndTurn,
				},
				Usage: asdk.BetaMessageDeltaUsage{
					InputTokens:  1,
					OutputTokens: 1,
				},
			},
		},
	}

	generation, err := FromStream(req, summary)
	if err != nil {
		t.Fatalf("from stream: %v", err)
	}
	if len(generation.Output) != 1 || len(generation.Output[0].Parts) != 2 {
		t.Fatalf("expected two output parts, got %#v", generation.Output)
	}
	if generation.Output[0].Parts[0].Thinking != "   " {
		t.Fatalf("unexpected thinking %q", generation.Output[0].Parts[0].Thinking)
	}
	if generation.Output[0].Parts[1].Text != "  " {
		t.Fatalf("unexpected text %q", generation.Output[0].Parts[1].Text)
	}
}

func TestMapSystemPromptPreservesEmptySegments(t *testing.T) {
	got := mapSystemPrompt([]asdk.BetaTextBlockParam{
		{Text: "", Type: "text"},
		{Text: "second", Type: "text"},
	})
	if got != "\n\nsecond" {
		t.Fatalf("expected preserved empty segment separator, got %q", got)
	}
}

func TestFromRequestResponsePreservesToolSearchVariantToolResultTypes(t *testing.T) {
	req := testRequest()
	resp := &asdk.BetaMessage{
		ID:         "msg_variant_types",
		Model:      asdk.Model("claude-sonnet-4-5"),
		StopReason: asdk.BetaStopReasonEndTurn,
		Content: []asdk.BetaContentBlockUnion{
			mustUnmarshalBetaContentBlockUnion(t, `{"type":"tool_search_tool_regex_tool_result","tool_use_id":"toolu_regex","content":{"type":"tool_search_tool_search_result","tool_references":[]}}`),
			mustUnmarshalBetaContentBlockUnion(t, `{"type":"tool_search_tool_bm25_tool_result","tool_use_id":"toolu_bm25","content":{"type":"tool_search_tool_search_result","tool_references":[]}}`),
		},
	}

	generation, err := FromRequestResponse(req, resp)
	if err != nil {
		t.Fatalf("from request/response: %v", err)
	}

	if len(generation.Output) != 1 {
		t.Fatalf("expected 1 output message (tool), got %d", len(generation.Output))
	}
	if generation.Output[0].Role != agento11y.RoleTool {
		t.Fatalf("expected tool role, got %q", generation.Output[0].Role)
	}
	if len(generation.Output[0].Parts) != 2 {
		t.Fatalf("expected 2 tool result parts, got %d", len(generation.Output[0].Parts))
	}

	regexPart := generation.Output[0].Parts[0]
	if regexPart.Metadata.ProviderType != toolSearchRegexToolResultType {
		t.Fatalf("expected regex provider type %q, got %q", toolSearchRegexToolResultType, regexPart.Metadata.ProviderType)
	}
	if regexPart.ToolResult.ToolCallID != "toolu_regex" {
		t.Fatalf("expected regex tool call id toolu_regex, got %q", regexPart.ToolResult.ToolCallID)
	}

	bm25Part := generation.Output[0].Parts[1]
	if bm25Part.Metadata.ProviderType != toolSearchBM25ToolResultType {
		t.Fatalf("expected bm25 provider type %q, got %q", toolSearchBM25ToolResultType, bm25Part.Metadata.ProviderType)
	}
	if bm25Part.ToolResult.ToolCallID != "toolu_bm25" {
		t.Fatalf("expected bm25 tool call id toolu_bm25, got %q", bm25Part.ToolResult.ToolCallID)
	}
}

func TestFromStreamPreservesToolSearchVariantToolResultTypes(t *testing.T) {
	req := asdk.BetaMessageNewParams{
		MaxTokens: 1,
		Model:     asdk.Model("claude-sonnet-4-5"),
	}

	summary := StreamSummary{
		Events: []asdk.BetaRawMessageStreamEventUnion{
			{
				Type: "message_start",
				Message: asdk.BetaMessage{
					ID:    "msg_stream_variants",
					Model: asdk.Model("claude-sonnet-4-5"),
				},
			},
			{
				Type:         "content_block_start",
				Index:        0,
				ContentBlock: mustUnmarshalBetaRawContentBlockStartEventContentBlockUnion(t, `{"type":"tool_search_tool_regex_tool_result","tool_use_id":"toolu_regex","content":{"type":"tool_search_tool_search_result","tool_references":[]}}`),
			},
			{
				Type:         "content_block_start",
				Index:        1,
				ContentBlock: mustUnmarshalBetaRawContentBlockStartEventContentBlockUnion(t, `{"type":"tool_search_tool_bm25_tool_result","tool_use_id":"toolu_bm25","content":{"type":"tool_search_tool_search_result","tool_references":[]}}`),
			},
			{
				Type: "message_delta",
				Delta: asdk.BetaRawMessageStreamEventUnionDelta{
					StopReason: asdk.BetaStopReasonEndTurn,
				},
				Usage: asdk.BetaMessageDeltaUsage{
					InputTokens:  10,
					OutputTokens: 5,
				},
			},
		},
	}

	generation, err := FromStream(req, summary)
	if err != nil {
		t.Fatalf("from stream: %v", err)
	}

	if len(generation.Output) != 1 {
		t.Fatalf("expected 1 output message (tool), got %d", len(generation.Output))
	}
	if generation.Output[0].Role != agento11y.RoleTool {
		t.Fatalf("expected tool role, got %q", generation.Output[0].Role)
	}
	if len(generation.Output[0].Parts) != 2 {
		t.Fatalf("expected 2 tool result parts, got %d", len(generation.Output[0].Parts))
	}

	regexPart := generation.Output[0].Parts[0]
	if regexPart.Metadata.ProviderType != toolSearchRegexToolResultType {
		t.Fatalf("expected regex provider type %q, got %q", toolSearchRegexToolResultType, regexPart.Metadata.ProviderType)
	}
	if regexPart.ToolResult.ToolCallID != "toolu_regex" {
		t.Fatalf("expected regex tool call id toolu_regex, got %q", regexPart.ToolResult.ToolCallID)
	}

	bm25Part := generation.Output[0].Parts[1]
	if bm25Part.Metadata.ProviderType != toolSearchBM25ToolResultType {
		t.Fatalf("expected bm25 provider type %q, got %q", toolSearchBM25ToolResultType, bm25Part.Metadata.ProviderType)
	}
	if bm25Part.ToolResult.ToolCallID != "toolu_bm25" {
		t.Fatalf("expected bm25 tool call id toolu_bm25, got %q", bm25Part.ToolResult.ToolCallID)
	}
}

func TestFromRequestResponsePreservesToolSearchVariantToolUseTypes(t *testing.T) {
	req := testRequest()
	resp := &asdk.BetaMessage{
		ID:         "msg_variant_tool_use_types",
		Model:      asdk.Model("claude-sonnet-4-5"),
		StopReason: asdk.BetaStopReasonToolUse,
		Content: []asdk.BetaContentBlockUnion{
			mustUnmarshalBetaContentBlockUnion(t, `{"type":"server_tool_use","id":"toolu_regex","name":"tool_search_tool_regex","input":{"query":"error"}}`),
			mustUnmarshalBetaContentBlockUnion(t, `{"type":"server_tool_use","id":"toolu_bm25","name":"tool_search_tool_bm25","input":{"query":"latency"}}`),
		},
	}

	generation, err := FromRequestResponse(req, resp)
	if err != nil {
		t.Fatalf("from request/response: %v", err)
	}

	if len(generation.Output) != 1 {
		t.Fatalf("expected 1 output assistant message, got %d", len(generation.Output))
	}
	if generation.Output[0].Role != agento11y.RoleAssistant {
		t.Fatalf("expected assistant role, got %q", generation.Output[0].Role)
	}
	if len(generation.Output[0].Parts) != 2 {
		t.Fatalf("expected 2 tool call parts, got %d", len(generation.Output[0].Parts))
	}

	regexPart := generation.Output[0].Parts[0]
	if regexPart.Metadata.ProviderType != toolSearchRegexToolUseType {
		t.Fatalf("expected regex provider type %q, got %q", toolSearchRegexToolUseType, regexPart.Metadata.ProviderType)
	}
	if regexPart.ToolCall.Name != toolSearchRegexToolUseType {
		t.Fatalf("expected regex tool call name %q, got %q", toolSearchRegexToolUseType, regexPart.ToolCall.Name)
	}

	bm25Part := generation.Output[0].Parts[1]
	if bm25Part.Metadata.ProviderType != toolSearchBM25ToolUseType {
		t.Fatalf("expected bm25 provider type %q, got %q", toolSearchBM25ToolUseType, bm25Part.Metadata.ProviderType)
	}
	if bm25Part.ToolCall.Name != toolSearchBM25ToolUseType {
		t.Fatalf("expected bm25 tool call name %q, got %q", toolSearchBM25ToolUseType, bm25Part.ToolCall.Name)
	}
}

func TestFromStreamPreservesToolSearchVariantToolUseTypes(t *testing.T) {
	req := asdk.BetaMessageNewParams{
		MaxTokens: 1,
		Model:     asdk.Model("claude-sonnet-4-5"),
	}

	summary := StreamSummary{
		Events: []asdk.BetaRawMessageStreamEventUnion{
			{
				Type: "message_start",
				Message: asdk.BetaMessage{
					ID:    "msg_stream_tool_use_variants",
					Model: asdk.Model("claude-sonnet-4-5"),
				},
			},
			{
				Type:         "content_block_start",
				Index:        0,
				ContentBlock: mustUnmarshalBetaRawContentBlockStartEventContentBlockUnion(t, `{"type":"server_tool_use","id":"toolu_regex","name":"tool_search_tool_regex","input":{"query":"error"}}`),
			},
			{
				Type:         "content_block_start",
				Index:        1,
				ContentBlock: mustUnmarshalBetaRawContentBlockStartEventContentBlockUnion(t, `{"type":"server_tool_use","id":"toolu_bm25","name":"tool_search_tool_bm25","input":{"query":"latency"}}`),
			},
			{
				Type: "message_delta",
				Delta: asdk.BetaRawMessageStreamEventUnionDelta{
					StopReason: asdk.BetaStopReasonToolUse,
				},
				Usage: asdk.BetaMessageDeltaUsage{
					InputTokens:  10,
					OutputTokens: 5,
				},
			},
		},
	}

	generation, err := FromStream(req, summary)
	if err != nil {
		t.Fatalf("from stream: %v", err)
	}

	if len(generation.Output) != 1 {
		t.Fatalf("expected 1 output assistant message, got %d", len(generation.Output))
	}
	if generation.Output[0].Role != agento11y.RoleAssistant {
		t.Fatalf("expected assistant role, got %q", generation.Output[0].Role)
	}
	if len(generation.Output[0].Parts) != 2 {
		t.Fatalf("expected 2 tool call parts, got %d", len(generation.Output[0].Parts))
	}

	regexPart := generation.Output[0].Parts[0]
	if regexPart.Metadata.ProviderType != toolSearchRegexToolUseType {
		t.Fatalf("expected regex provider type %q, got %q", toolSearchRegexToolUseType, regexPart.Metadata.ProviderType)
	}
	if regexPart.ToolCall.Name != toolSearchRegexToolUseType {
		t.Fatalf("expected regex tool call name %q, got %q", toolSearchRegexToolUseType, regexPart.ToolCall.Name)
	}

	bm25Part := generation.Output[0].Parts[1]
	if bm25Part.Metadata.ProviderType != toolSearchBM25ToolUseType {
		t.Fatalf("expected bm25 provider type %q, got %q", toolSearchBM25ToolUseType, bm25Part.Metadata.ProviderType)
	}
	if bm25Part.ToolCall.Name != toolSearchBM25ToolUseType {
		t.Fatalf("expected bm25 tool call name %q, got %q", toolSearchBM25ToolUseType, bm25Part.ToolCall.Name)
	}
}

func mustUnmarshalBetaContentBlockUnion(t *testing.T, payload string) asdk.BetaContentBlockUnion {
	t.Helper()
	var block asdk.BetaContentBlockUnion
	if err := json.Unmarshal([]byte(payload), &block); err != nil {
		t.Fatalf("unmarshal beta content block union: %v", err)
	}
	return block
}

func mustUnmarshalBetaRawContentBlockStartEventContentBlockUnion(t *testing.T, payload string) asdk.BetaRawContentBlockStartEventContentBlockUnion {
	t.Helper()
	var block asdk.BetaRawContentBlockStartEventContentBlockUnion
	if err := json.Unmarshal([]byte(payload), &block); err != nil {
		t.Fatalf("unmarshal beta raw content block start event union: %v", err)
	}
	return block
}

func testRequest() asdk.BetaMessageNewParams {
	toolResult := asdk.NewBetaToolResultBlock("toolu_1", "", false)
	toolResult.OfToolResult.Content = []asdk.BetaToolResultBlockParamContentUnion{
		{
			OfText: &asdk.BetaTextBlockParam{
				Text: "18C and sunny",
				Type: "text",
			},
		},
	}

	weatherTool := asdk.BetaToolUnionParamOfTool(asdk.BetaToolInputSchemaParam{
		Type: "object",
		Properties: map[string]any{
			"city": map[string]any{
				"type": "string",
			},
		},
		Required: []string{"city"},
	}, "weather")
	weatherTool.OfTool.DeferLoading = param.NewOpt(true)

	return asdk.BetaMessageNewParams{
		MaxTokens:   512,
		Model:       asdk.Model("claude-sonnet-4-5"),
		Temperature: param.NewOpt(0.3),
		TopP:        param.NewOpt(0.8),
		ToolChoice:  asdk.BetaToolChoiceParamOfTool("weather"),
		Thinking:    asdk.BetaThinkingConfigParamOfEnabled(1024),
		System: []asdk.BetaTextBlockParam{
			{
				Text: "Be precise.",
				Type: "text",
			},
		},
		Messages: []asdk.BetaMessageParam{
			{
				Role: asdk.BetaMessageParamRoleUser,
				Content: []asdk.BetaContentBlockParamUnion{
					asdk.NewBetaTextBlock("What's the weather in Paris?"),
				},
			},
			{
				Role: asdk.BetaMessageParamRoleAssistant,
				Content: []asdk.BetaContentBlockParamUnion{
					asdk.NewBetaThinkingBlock("sig", "need to call weather tool"),
					asdk.NewBetaToolUseBlock("toolu_1", map[string]any{"city": "Paris"}, "weather"),
				},
			},
			{
				Role: asdk.BetaMessageParamRoleUser,
				Content: []asdk.BetaContentBlockParamUnion{
					toolResult,
				},
			},
		},
		Tools: []asdk.BetaToolUnionParam{weatherTool},
	}
}

func TestFromRequestResponseSkipsEmptyThinkingBlocks(t *testing.T) {
	// Adaptive thinking (e.g. Claude Sonnet 5) can emit signature-only
	// thinking blocks with empty text. They must be skipped: a thinking Part
	// without a payload fails generation validation and would previously
	// zero out the whole generation.
	req := testRequest()
	req.Messages = append(req.Messages, asdk.BetaMessageParam{
		Role: asdk.BetaMessageParamRoleAssistant,
		Content: []asdk.BetaContentBlockParamUnion{
			{OfThinking: &asdk.BetaThinkingBlockParam{Thinking: "", Signature: "sig-1"}},
			{OfRedactedThinking: &asdk.BetaRedactedThinkingBlockParam{Data: ""}},
			{OfText: &asdk.BetaTextBlockParam{Text: "prior turn", Type: "text"}},
		},
	})

	resp := &asdk.BetaMessage{
		ID:         "msg_1",
		Model:      asdk.Model("claude-sonnet-5"),
		StopReason: asdk.BetaStopReasonEndTurn,
		Content: []asdk.BetaContentBlockUnion{
			{Type: "thinking", Thinking: "", Signature: "sig-2"},
			{Type: "redacted_thinking", Data: ""},
			{Type: "thinking", Thinking: "kept", Signature: "sig-3"},
			{Type: "text", Text: "done"},
		},
	}

	generation, err := FromRequestResponse(req, resp)
	if err != nil {
		t.Fatalf("from request/response: %v", err)
	}

	for _, message := range append(generation.Input, generation.Output...) {
		for _, part := range message.Parts {
			if part.Kind == agento11y.PartKindThinking && part.Thinking == "" {
				t.Fatalf("empty thinking part leaked into generation: %+v", part)
			}
		}
	}

	if len(generation.Output) == 0 {
		t.Fatal("expected output messages")
	}
	parts := generation.Output[0].Parts
	if len(parts) != 2 {
		t.Fatalf("expected 2 output parts (non-empty thinking + text), got %d: %+v", len(parts), parts)
	}
	if parts[0].Thinking != "kept" || parts[1].Text != "done" {
		t.Fatalf("unexpected output parts: %+v", parts)
	}

	if verr := agento11y.ValidateGeneration(generation); verr != nil {
		t.Fatalf("generation failed validation: %v", verr)
	}
}
