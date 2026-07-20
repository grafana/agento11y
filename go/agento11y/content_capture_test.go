package agento11y

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func TestStripContent(t *testing.T) {
	makeGen := func() Generation {
		return Generation{
			ID:                "gen-1",
			ConversationID:    "conv-1",
			AgentName:         "test-agent",
			AgentVersion:      "1.0",
			ConversationTitle: "Weather chat",
			Mode:              GenerationModeSync,
			Model:             ModelRef{Provider: "anthropic", Name: "claude-sonnet-4-5"},
			SystemPrompt:      "You are helpful.",
			Input: []Message{
				{Role: RoleUser, Parts: []Part{
					{Kind: PartKindText, Text: "What is the weather?"},
					{Kind: PartKindMedia, Media: &Media{Kind: "image", URL: "data:image/png;base64,abc123", MIMEType: "image/png", Name: "weather-map.png"}},
				}},
				{Role: RoleTool, Parts: []Part{{Kind: PartKindToolResult, ToolResult: &ToolResult{
					ToolCallID:  "call_1",
					Name:        "weather",
					Content:     "sunny 18C",
					ContentJSON: json.RawMessage(`{"temp":18}`),
				}}}},
			},
			Output: []Message{
				{Role: RoleAssistant, Parts: []Part{
					{Kind: PartKindThinking, Thinking: "let me think about weather"},
					{Kind: PartKindToolCall, ToolCall: &ToolCall{ID: "call_1", Name: "weather", InputJSON: json.RawMessage(`{"city":"Paris"}`)}},
					{Kind: PartKindText, Text: "It's 18C and sunny in Paris."},
				}},
			},
			Tools: []ToolDefinition{
				{Name: "weather", Description: "Get weather info", Type: "function", InputSchema: json.RawMessage(`{"type":"object"}`)},
			},
			Usage:      TokenUsage{InputTokens: 120, OutputTokens: 42},
			StopReason: "end_turn",
			CallError:  "rate limit exceeded: prompt too long for model",
			Artifacts:  []Artifact{{Kind: "request", Payload: []byte("raw")}},
			Metadata:   map[string]any{"agento11y.sdk.name": "sdk-go", "call_error": "rate limit exceeded: prompt too long for model", "agento11y.conversation.title": "Weather chat"},
		}
	}

	t.Run("strips sensitive content", func(t *testing.T) {
		gen := makeGen()
		stripContent(&gen, "rate_limit")

		if gen.SystemPrompt != "" {
			t.Fatal("SystemPrompt not stripped")
		}
		if gen.Input[0].Parts[0].Text != "" {
			t.Fatal("Input text not stripped")
		}
		if gen.Output[0].Parts[0].Thinking != "" {
			t.Fatal("Thinking not stripped")
		}
		if gen.Output[0].Parts[1].ToolCall.InputJSON != nil {
			t.Fatal("ToolCall.InputJSON not stripped")
		}
		if gen.Output[0].Parts[2].Text != "" {
			t.Fatal("Output text not stripped")
		}
		if gen.Input[0].Parts[1].Media.URL != "" {
			t.Fatal("Media URL not stripped")
		}
		if gen.Input[1].Parts[0].ToolResult.Content != "" {
			t.Fatal("ToolResult.Content not stripped")
		}
		if gen.Input[1].Parts[0].ToolResult.ContentJSON != nil {
			t.Fatal("ToolResult.ContentJSON not stripped")
		}
		if gen.Tools[0].Description != "" {
			t.Fatal("Tool description not stripped")
		}
		if gen.Tools[0].InputSchema != nil {
			t.Fatal("Tool InputSchema not stripped")
		}
		if gen.Artifacts != nil {
			t.Fatal("Artifacts not stripped")
		}
		if gen.ConversationTitle != "" {
			t.Fatal("ConversationTitle not stripped")
		}
		if _, ok := gen.Metadata["agento11y.conversation.title"]; ok {
			t.Fatal("Metadata[agento11y.conversation.title] should be deleted")
		}
	})

	t.Run("preserves message structure", func(t *testing.T) {
		gen := makeGen()
		stripContent(&gen, "rate_limit")

		if len(gen.Input) != 2 {
			t.Fatalf("Input messages lost: got %d", len(gen.Input))
		}
		if len(gen.Output) != 1 {
			t.Fatalf("Output messages lost: got %d", len(gen.Output))
		}
		if len(gen.Output[0].Parts) != 3 {
			t.Fatalf("Output parts lost: got %d", len(gen.Output[0].Parts))
		}
		if gen.Input[0].Role != RoleUser {
			t.Fatal("Input role changed")
		}
		if gen.Input[0].Parts[1].Kind != PartKindMedia {
			t.Fatal("Media part kind changed")
		}
		if gen.Input[0].Parts[1].Media.Kind != "image" || gen.Input[0].Parts[1].Media.MIMEType != "image/png" {
			t.Fatal("Media metadata lost")
		}
		if gen.Output[0].Parts[0].Kind != PartKindThinking {
			t.Fatal("Part kind changed")
		}
		if gen.Output[0].Parts[1].ToolCall.Name != "weather" {
			t.Fatal("Tool call name lost")
		}
		if gen.Output[0].Parts[1].ToolCall.ID != "call_1" {
			t.Fatal("Tool call ID lost")
		}
		if gen.Input[1].Parts[0].ToolResult.ToolCallID != "call_1" {
			t.Fatal("Tool result call ID lost")
		}
		if gen.Input[1].Parts[0].ToolResult.Name != "weather" {
			t.Fatal("Tool result name lost")
		}
	})

	t.Run("preserves operational metadata", func(t *testing.T) {
		gen := makeGen()
		stripContent(&gen, "rate_limit")

		if gen.Tools[0].Name != "weather" {
			t.Fatal("Tool name lost")
		}
		if gen.Usage.InputTokens != 120 || gen.Usage.OutputTokens != 42 {
			t.Fatal("Usage lost")
		}
		if gen.StopReason != "end_turn" {
			t.Fatal("StopReason lost")
		}
		if gen.Model.Name != "claude-sonnet-4-5" {
			t.Fatal("Model lost")
		}
		if gen.Metadata["agento11y.sdk.name"] != "sdk-go" {
			t.Fatal("Metadata lost")
		}
	})

	t.Run("replaces CallError with category", func(t *testing.T) {
		gen := makeGen()
		stripContent(&gen, "rate_limit")

		if gen.CallError != "rate_limit" {
			t.Fatalf("CallError = %q, want %q", gen.CallError, "rate_limit")
		}
		if _, ok := gen.Metadata["call_error"]; ok {
			t.Fatal("Metadata[call_error] should be deleted")
		}
	})

	t.Run("falls back to sdk_error without category", func(t *testing.T) {
		gen := makeGen()
		stripContent(&gen, "")

		if gen.CallError != "sdk_error" {
			t.Fatalf("CallError = %q, want %q", gen.CallError, "sdk_error")
		}
	})
}

func TestContentCaptureModeResolution(t *testing.T) {
	cases := []struct {
		name     string
		fallback ContentCaptureMode
		override ContentCaptureMode
		want     ContentCaptureMode
	}{
		{"Full fallback, Default override", ContentCaptureModeFull, ContentCaptureModeDefault, ContentCaptureModeFull},
		{"Full fallback, MetadataOnly override", ContentCaptureModeFull, ContentCaptureModeMetadataOnly, ContentCaptureModeMetadataOnly},
		{"MetadataOnly fallback, Default override", ContentCaptureModeMetadataOnly, ContentCaptureModeDefault, ContentCaptureModeMetadataOnly},
		{"MetadataOnly fallback, Full override", ContentCaptureModeMetadataOnly, ContentCaptureModeFull, ContentCaptureModeFull},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveContentCaptureMode(tc.override, tc.fallback)
			if got != tc.want {
				t.Fatalf("expected %v, got %v", tc.want, got)
			}
		})
	}
}

func TestGenerationContentCapture(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 4, 8, 12, 0, 0, 0, time.UTC) }

	cases := []struct {
		name            string
		clientMode      ContentCaptureMode
		genMode         ContentCaptureMode
		wantStripped    bool
		wantMarker      string
		wantTitleOnSpan bool
	}{
		{"client default, gen default — no_tool_content", ContentCaptureModeDefault, ContentCaptureModeDefault, false, contentCaptureModeValueNoToolContent, true},
		{"client MetadataOnly, gen default — stripped", ContentCaptureModeMetadataOnly, ContentCaptureModeDefault, true, contentCaptureModeValueMetaOnly, false},
		{"client Full, gen MetadataOnly — stripped", ContentCaptureModeFull, ContentCaptureModeMetadataOnly, true, contentCaptureModeValueMetaOnly, false},
		{"client MetadataOnly, gen Full — full", ContentCaptureModeMetadataOnly, ContentCaptureModeFull, false, contentCaptureModeValueFull, true},
		{"client Full, gen default — full", ContentCaptureModeFull, ContentCaptureModeDefault, false, contentCaptureModeValueFull, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, spanRecorder, tp := newTestClient(t, Config{
				ContentCapture: tc.clientMode,
				Now:            now,
			})

			_, rec := client.StartGeneration(context.Background(), GenerationStart{
				Model:             ModelRef{Provider: "anthropic", Name: "claude-sonnet-4-5"},
				ContentCapture:    tc.genMode,
				ConversationTitle: "Sensitive topic",
			})
			rec.SetResult(Generation{
				SystemPrompt: "You are helpful.",
				Input:        []Message{UserTextMessage("Hello")},
				Output:       []Message{AssistantTextMessage("Hi there")},
				Usage:        TokenUsage{InputTokens: 10, OutputTokens: 5},
			}, nil)
			rec.End()

			gen := rec.lastGeneration

			if gen.Metadata[metadataKeyContentCaptureMode] != tc.wantMarker {
				t.Fatalf("marker: got %v, want %v", gen.Metadata[metadataKeyContentCaptureMode], tc.wantMarker)
			}

			if err := rec.Err(); err != nil {
				t.Fatalf("unexpected recorder error: %v", err)
			}

			stripped := gen.Input[0].Parts[0].Text == ""
			if stripped != tc.wantStripped {
				t.Fatalf("content stripped: got %v, want %v", stripped, tc.wantStripped)
			}

			// Structure always preserved
			if len(gen.Input) != 1 || gen.Input[0].Role != RoleUser {
				t.Fatal("Input structure lost")
			}
			if gen.Usage.InputTokens != 10 {
				t.Fatal("Usage lost")
			}

			// Verify conversation title on span
			_ = tp.ForceFlush(context.Background())
			for _, s := range spanRecorder.Ended() {
				if !strings.HasPrefix(s.Name(), "generate") {
					continue
				}
				attrs := spanAttributeMap(s)
				titleVal, hasTitle := attrs[spanAttrConversationTitle]
				if tc.wantTitleOnSpan && (!hasTitle || titleVal.AsString() != "Sensitive topic") {
					t.Fatalf("expected conversation title on span, got present=%v value=%q", hasTitle, titleVal.AsString())
				}
				if !tc.wantTitleOnSpan && hasTitle {
					t.Fatalf("expected conversation title absent from span, got %q", titleVal.AsString())
				}
			}
		})
	}
}

func TestToolExecutionContentCaptureInheritance(t *testing.T) {
	cases := []struct {
		name               string
		clientDefault      ContentCaptureMode
		parentGenOverride  ContentCaptureMode
		useParentCtx       bool
		toolOverride       ContentCaptureMode
		toolIncludeContent bool
		wantContent        bool
		wantTitleOnSpan    bool
	}{
		{
			name:               "parent MetadataOnly, tool inherits — suppressed",
			clientDefault:      ContentCaptureModeMetadataOnly,
			useParentCtx:       true,
			toolIncludeContent: true,
			wantContent:        false,
			wantTitleOnSpan:    false,
		},
		{
			name:               "parent MetadataOnly, tool explicit Full — included",
			clientDefault:      ContentCaptureModeMetadataOnly,
			useParentCtx:       true,
			toolOverride:       ContentCaptureModeFull,
			toolIncludeContent: true,
			wantContent:        true,
			wantTitleOnSpan:    true,
		},
		{
			name:               "parent Full (override client MetadataOnly), tool inherits — included",
			clientDefault:      ContentCaptureModeMetadataOnly,
			parentGenOverride:  ContentCaptureModeFull,
			useParentCtx:       true,
			toolIncludeContent: true,
			wantContent:        true,
			wantTitleOnSpan:    true,
		},
		{
			name:               "no parent gen, client MetadataOnly — suppressed (fail-closed)",
			clientDefault:      ContentCaptureModeMetadataOnly,
			useParentCtx:       false,
			toolIncludeContent: true,
			wantContent:        false,
			wantTitleOnSpan:    false,
		},
		{
			name:               "no parent gen, client Full, legacy true — included",
			clientDefault:      ContentCaptureModeFull,
			useParentCtx:       false,
			toolIncludeContent: true,
			wantContent:        true,
			wantTitleOnSpan:    true,
		},
		{
			name:               "no parent gen, client Full, legacy false — included",
			clientDefault:      ContentCaptureModeFull,
			useParentCtx:       false,
			toolIncludeContent: false,
			wantContent:        true,
			wantTitleOnSpan:    true,
		},
		{
			name:               "parent Full, tool explicit MetadataOnly — suppressed",
			clientDefault:      ContentCaptureModeFull,
			useParentCtx:       true,
			toolOverride:       ContentCaptureModeMetadataOnly,
			toolIncludeContent: true,
			wantContent:        false,
			wantTitleOnSpan:    false,
		},
		// Backward compat: client Default (→ NoToolContent), legacy controls.
		{
			name:               "no parent gen, client Default, legacy false — suppressed",
			clientDefault:      ContentCaptureModeDefault,
			useParentCtx:       false,
			toolIncludeContent: false,
			wantContent:        false,
			wantTitleOnSpan:    true,
		},
		{
			name:               "no parent gen, client Default, legacy true — included",
			clientDefault:      ContentCaptureModeDefault,
			useParentCtx:       false,
			toolIncludeContent: true,
			wantContent:        true,
			wantTitleOnSpan:    true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, recorder, tp := newTestClient(t, Config{
				ContentCapture: tc.clientDefault,
				Now:            func() time.Time { return time.Date(2026, 4, 8, 12, 0, 0, 0, time.UTC) },
			})

			ctx := context.Background()
			var genRec *GenerationRecorder

			if tc.useParentCtx {
				ctx, genRec = client.StartGeneration(ctx, GenerationStart{
					Model:             ModelRef{Provider: "anthropic", Name: "claude-sonnet-4-5"},
					ContentCapture:    tc.parentGenOverride,
					ConversationTitle: "Sensitive topic",
				})
			}

			_, toolRec := client.StartToolExecution(ctx, ToolExecutionStart{
				ToolName:          "test_tool",
				ContentCapture:    tc.toolOverride,
				IncludeContent:    tc.toolIncludeContent,
				ConversationTitle: "Sensitive topic",
			})
			toolRec.SetResult(ToolExecutionEnd{
				Arguments: "args",
				Result:    "result",
			})
			toolRec.End()

			if genRec != nil {
				genRec.SetResult(Generation{Usage: TokenUsage{InputTokens: 1}}, nil)
				genRec.End()
			}

			_ = tp.ForceFlush(context.Background())

			spans := recorder.Ended()
			var toolSpan sdktrace.ReadOnlySpan
			for _, s := range spans {
				if strings.HasPrefix(s.Name(), "execute_tool") {
					toolSpan = s
					break
				}
			}
			if toolSpan == nil {
				t.Fatal("tool execution span not found")
			}

			attrs := spanAttributeMap(toolSpan)
			_, hasArgs := attrs[spanAttrToolCallArguments]
			if hasArgs != tc.wantContent {
				t.Fatalf("tool arguments: got present=%v, want present=%v", hasArgs, tc.wantContent)
			}

			if _, ok := attrs[spanAttrToolName]; !ok {
				t.Fatal("tool name should always be present")
			}

			titleVal, hasTitle := attrs[spanAttrConversationTitle]
			if tc.wantTitleOnSpan && (!hasTitle || titleVal.AsString() != "Sensitive topic") {
				t.Fatalf("expected conversation title on tool span, got present=%v value=%q", hasTitle, titleVal.AsString())
			}
			if !tc.wantTitleOnSpan && hasTitle {
				t.Fatalf("expected conversation title absent from tool span, got %q", titleVal.AsString())
			}
		})
	}
}

func TestResolveToolContentCaptureMode(t *testing.T) {
	cases := []struct {
		name          string
		toolMode      ContentCaptureMode
		ctxMode       ContentCaptureMode
		ctxSet        bool
		clientDefault ContentCaptureMode
		want          ContentCaptureMode
	}{
		{"client Full, no ctx", ContentCaptureModeDefault, ContentCaptureModeFull, false, ContentCaptureModeFull, ContentCaptureModeFull},
		{"client Default, no ctx — resolves to NoToolContent", ContentCaptureModeDefault, ContentCaptureModeDefault, false, ContentCaptureModeDefault, ContentCaptureModeNoToolContent},
		{"ctx MetadataOnly overrides client Full", ContentCaptureModeDefault, ContentCaptureModeMetadataOnly, true, ContentCaptureModeFull, ContentCaptureModeMetadataOnly},
		{"ctx Full overrides client MetadataOnly", ContentCaptureModeDefault, ContentCaptureModeFull, true, ContentCaptureModeMetadataOnly, ContentCaptureModeFull},
		{"ctx NoToolContent overrides client Full", ContentCaptureModeDefault, ContentCaptureModeNoToolContent, true, ContentCaptureModeFull, ContentCaptureModeNoToolContent},
		{"no ctx, client MetadataOnly", ContentCaptureModeDefault, ContentCaptureModeFull, false, ContentCaptureModeMetadataOnly, ContentCaptureModeMetadataOnly},
		{"explicit Full overrides ctx MetadataOnly", ContentCaptureModeFull, ContentCaptureModeMetadataOnly, true, ContentCaptureModeFull, ContentCaptureModeFull},
		{"explicit MetadataOnly overrides everything", ContentCaptureModeMetadataOnly, ContentCaptureModeFull, true, ContentCaptureModeFull, ContentCaptureModeMetadataOnly},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveToolContentCaptureMode(tc.toolMode, tc.ctxMode, tc.ctxSet, tc.clientDefault)
			if got != tc.want {
				t.Fatalf("expected %v, got %v", tc.want, got)
			}
		})
	}
}

func TestContentCaptureModeString(t *testing.T) {
	cases := []struct {
		mode ContentCaptureMode
		want string
	}{
		{ContentCaptureModeFull, "full"},
		{ContentCaptureModeNoToolContent, "no_tool_content"},
		{ContentCaptureModeMetadataOnly, "metadata_only"},
		{ContentCaptureModeFullWithMetadataSpans, "full_with_metadata_spans"},
		{ContentCaptureModeDefault, "default"},
	}
	for _, tc := range cases {
		if got := tc.mode.String(); got != tc.want {
			t.Fatalf("%d.String() = %q, want %q", tc.mode, got, tc.want)
		}
	}
}

func TestContentCaptureModeTextMarshaling(t *testing.T) {
	cases := []struct {
		text string
		want ContentCaptureMode
	}{
		{"full", ContentCaptureModeFull},
		{"metadata_only", ContentCaptureModeMetadataOnly},
		{"full_with_metadata_spans", ContentCaptureModeFullWithMetadataSpans},
		{"default", ContentCaptureModeDefault},
		{"", ContentCaptureModeDefault},
	}

	for _, tc := range cases {
		var m ContentCaptureMode
		if err := m.UnmarshalText([]byte(tc.text)); err != nil {
			t.Fatalf("unmarshal %q: %v", tc.text, err)
		}
		if m != tc.want {
			t.Fatalf("unmarshal %q: got %v, want %v", tc.text, m, tc.want)
		}
	}

	var m ContentCaptureMode
	if err := m.UnmarshalText([]byte("invalid")); err == nil {
		t.Fatal("expected error for invalid mode")
	}
}

func TestCallContentCaptureResolver(t *testing.T) {
	cases := []struct {
		name     string
		resolver func(context.Context, map[string]any) ContentCaptureMode
		metadata map[string]any
		want     ContentCaptureMode
	}{
		{
			name:     "nil resolver returns Default",
			resolver: nil,
			want:     ContentCaptureModeDefault,
		},
		{
			name: "resolver returns MetadataOnly",
			resolver: func(_ context.Context, _ map[string]any) ContentCaptureMode {
				return ContentCaptureModeMetadataOnly
			},
			want: ContentCaptureModeMetadataOnly,
		},
		{
			name: "resolver returns Full",
			resolver: func(_ context.Context, _ map[string]any) ContentCaptureMode {
				return ContentCaptureModeFull
			},
			want: ContentCaptureModeFull,
		},
		{
			name: "resolver reads tenant_id from metadata",
			resolver: func(_ context.Context, meta map[string]any) ContentCaptureMode {
				if tid, _ := meta["tenant_id"].(string); tid == "opted-out" {
					return ContentCaptureModeMetadataOnly
				}
				return ContentCaptureModeFull
			},
			metadata: map[string]any{"tenant_id": "opted-out"},
			want:     ContentCaptureModeMetadataOnly,
		},
		{
			name: "resolver receives nil metadata without panic",
			resolver: func(_ context.Context, meta map[string]any) ContentCaptureMode {
				if meta == nil {
					return ContentCaptureModeMetadataOnly
				}
				return ContentCaptureModeFull
			},
			metadata: nil,
			want:     ContentCaptureModeMetadataOnly,
		},
		{
			name: "resolver panic recovers to MetadataOnly",
			resolver: func(_ context.Context, _ map[string]any) ContentCaptureMode {
				panic("resolver bug")
			},
			want: ContentCaptureModeMetadataOnly,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := callContentCaptureResolver(context.Background(), tc.resolver, tc.metadata)
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestGenerationContentCaptureWithResolver(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 4, 9, 12, 0, 0, 0, time.UTC) }

	cases := []struct {
		name         string
		clientMode   ContentCaptureMode
		resolver     func(context.Context, map[string]any) ContentCaptureMode
		genMode      ContentCaptureMode
		wantStripped bool
		wantMarker   string
	}{
		{
			name:       "resolver MetadataOnly overrides client Full",
			clientMode: ContentCaptureModeFull,
			resolver: func(_ context.Context, _ map[string]any) ContentCaptureMode {
				return ContentCaptureModeMetadataOnly
			},
			genMode:      ContentCaptureModeDefault,
			wantStripped: true,
			wantMarker:   contentCaptureModeValueMetaOnly,
		},
		{
			name:       "per-generation Full overrides resolver MetadataOnly",
			clientMode: ContentCaptureModeDefault,
			resolver: func(_ context.Context, _ map[string]any) ContentCaptureMode {
				return ContentCaptureModeMetadataOnly
			},
			genMode:      ContentCaptureModeFull,
			wantStripped: false,
			wantMarker:   contentCaptureModeValueFull,
		},
		{
			name:       "resolver Default defers to client",
			clientMode: ContentCaptureModeMetadataOnly,
			resolver: func(_ context.Context, _ map[string]any) ContentCaptureMode {
				return ContentCaptureModeDefault
			},
			genMode:      ContentCaptureModeDefault,
			wantStripped: true,
			wantMarker:   contentCaptureModeValueMetaOnly,
		},
		{
			name:       "resolver Full overrides client MetadataOnly",
			clientMode: ContentCaptureModeMetadataOnly,
			resolver: func(_ context.Context, _ map[string]any) ContentCaptureMode {
				return ContentCaptureModeFull
			},
			genMode:      ContentCaptureModeDefault,
			wantStripped: false,
			wantMarker:   contentCaptureModeValueFull,
		},
		{
			name:       "panicking resolver fails closed, client Full → MetadataOnly",
			clientMode: ContentCaptureModeFull,
			resolver: func(_ context.Context, _ map[string]any) ContentCaptureMode {
				panic("oops")
			},
			genMode:      ContentCaptureModeDefault,
			wantStripped: true,
			wantMarker:   contentCaptureModeValueMetaOnly,
		},
	}

	tp := sdktrace.NewTracerProvider()
	defer func() { _ = tp.Shutdown(context.Background()) }()

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := NewClient(Config{
				ContentCapture:         tc.clientMode,
				ContentCaptureResolver: tc.resolver,
				Tracer:                 tp.Tracer("test"),
				Now:                    now,
				testDisableWorker:      true,
			})

			ctx, rec := client.StartGeneration(context.Background(), GenerationStart{
				ContentCapture: tc.genMode,
				Model:          ModelRef{Provider: "test", Name: "test-model"},
				Metadata:       map[string]any{"tenant_id": "t1"},
			})
			_ = ctx
			rec.SetResult(Generation{
				Input:  []Message{UserTextMessage("hello")},
				Output: []Message{AssistantTextMessage("world")},
				Usage:  TokenUsage{InputTokens: 10, OutputTokens: 5},
			}, nil)
			rec.End()

			gen := rec.lastGeneration
			if gen.Metadata[metadataKeyContentCaptureMode] != tc.wantMarker {
				t.Fatalf("marker: got %v, want %v", gen.Metadata[metadataKeyContentCaptureMode], tc.wantMarker)
			}
			inputStripped := len(gen.Input) > 0 && gen.Input[0].Parts[0].Text == ""
			if inputStripped != tc.wantStripped {
				t.Fatalf("stripped: got %v, want %v", inputStripped, tc.wantStripped)
			}
		})
	}
}
