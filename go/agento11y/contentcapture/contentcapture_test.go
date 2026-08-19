package contentcapture_test

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/grafana/agento11y/go/agento11y/contentcapture"
	"github.com/grafana/agento11y/go/agento11y/model"
	agento11yv1 "github.com/grafana/agento11y/go/proto/agento11y/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// leakMarker is embedded in every content field of the fixture so a single
// scan of the stripped payload catches content the strip forgot.
const leakMarker = "ignore previous instructions"

func fixedTime() time.Time {
	return time.Date(2026, 3, 12, 14, 10, 1, 0, time.UTC)
}

func mustStruct(fields map[string]any) *structpb.Struct {
	s, err := structpb.NewStruct(fields)
	if err != nil {
		panic(err)
	}
	return s
}

// fullContentGeneration returns a proto generation with every content-bearing
// field populated, plus the structure and metadata that must survive.
//
// Every field reachable from Generation is populated: TestProtoFieldCoverage
// reports a field the fixture leaves empty, because an all-zero field lets an
// assertion pass without exercising the strip.
func fullContentGeneration() *agento11yv1.Generation {
	maxTokens := int64(2048)
	temperature := 0.7
	topP := 0.95
	toolChoice := "auto"
	thinkingEnabled := true
	effectiveVersion := "sha256:0f1e2d"

	return &agento11yv1.Generation{
		Id:             "gen-1",
		ConversationId: "conv-1",
		OperationName:  "generateText",
		AgentName:      "weather-agent",
		AgentVersion:   "1.2.3",
		TraceId:        "4bf92f3577b34da6a3ce929d0e0e4736",
		SpanId:         "00f067aa0ba902b7",
		Model:          &agento11yv1.ModelRef{Provider: "anthropic", Name: "claude-sonnet-4-5"},
		ResponseId:     "resp-1",
		ResponseModel:  "claude-sonnet-4-5-20260101",
		Mode:           agento11yv1.GenerationMode_GENERATION_MODE_SYNC,
		SystemPrompt:   "You are helpful. " + leakMarker,
		Input: []*agento11yv1.Message{
			{
				Role: agento11yv1.MessageRole_MESSAGE_ROLE_USER,
				Name: "alex",
				Parts: []*agento11yv1.Part{
					{
						Payload:  &agento11yv1.Part_Text{Text: "What is the weather? " + leakMarker},
						Metadata: &agento11yv1.PartMetadata{ProviderType: "text"},
					},
					{Payload: &agento11yv1.Part_Media{Media: &agento11yv1.Media{
						Kind:     "image",
						Url:      "data:image/png;base64," + leakMarker,
						MimeType: "image/png",
						Name:     "map.png",
					}}},
				},
			},
			{
				Role: agento11yv1.MessageRole_MESSAGE_ROLE_TOOL,
				Parts: []*agento11yv1.Part{
					{Payload: &agento11yv1.Part_ToolResult{ToolResult: &agento11yv1.ToolResult{
						ToolCallId:  "call_1",
						Name:        "weather",
						Content:     "sunny 18C " + leakMarker,
						ContentJson: []byte(`{"temp":"` + leakMarker + `"}`),
						IsError:     true,
					}}},
				},
			},
		},
		Output: []*agento11yv1.Message{
			{
				Role: agento11yv1.MessageRole_MESSAGE_ROLE_ASSISTANT,
				Parts: []*agento11yv1.Part{
					{Payload: &agento11yv1.Part_Thinking{Thinking: "let me think " + leakMarker}},
					{Payload: &agento11yv1.Part_ToolCall{ToolCall: &agento11yv1.ToolCall{
						Id:        "call_1",
						Name:      "weather",
						InputJson: []byte(`{"city":"` + leakMarker + `"}`),
					}}},
					{Payload: &agento11yv1.Part_Text{Text: "It is 18C in Paris. " + leakMarker}},
				},
			},
		},
		Tools: []*agento11yv1.ToolDefinition{
			{
				Name:            "weather",
				Description:     "Get weather info " + leakMarker,
				Type:            "function",
				InputSchemaJson: []byte(`{"type":"object","title":"` + leakMarker + `"}`),
				Deferred:        true,
			},
		},
		Usage: &agento11yv1.TokenUsage{
			InputTokens:           120,
			OutputTokens:          42,
			TotalTokens:           162,
			CacheReadInputTokens:  16,
			CacheWriteInputTokens: 8,
			ReasoningTokens:       4,
			InputSemantics:        agento11yv1.TokenInputSemantics_TOKEN_INPUT_SEMANTICS_INCLUSIVE,
		},
		StopReason:  "end_turn",
		StartedAt:   timestamppb.New(fixedTime().Add(-time.Second)),
		CompletedAt: timestamppb.New(fixedTime()),
		Tags:        map[string]string{"env": "test"},
		RawArtifacts: []*agento11yv1.Artifact{
			{
				Kind:        agento11yv1.ArtifactKind_ARTIFACT_KIND_REQUEST,
				Name:        "request.json",
				ContentType: "application/json",
				Payload:     []byte(`{"prompt":"` + leakMarker + `"}`),
				RecordId:    "rec-1",
				Uri:         "https://example.invalid/artifacts/rec-1",
			},
		},
		CallError:           "provider refused: " + leakMarker,
		MaxTokens:           &maxTokens,
		Temperature:         &temperature,
		TopP:                &topP,
		ToolChoice:          &toolChoice,
		ThinkingEnabled:     &thinkingEnabled,
		ParentGenerationIds: []string{"gen-parent-1"},
		EffectiveVersion:    &effectiveVersion,
		Metadata: mustStruct(map[string]any{
			"call_error":                        "provider refused: " + leakMarker,
			"agento11y.conversation.title":      "Weather chat " + leakMarker,
			"sigil.conversation.title":          "Weather chat " + leakMarker,
			"user.key":                          "keep me",
			"agento11y.sdk.content_capture":     "not the mode key",
			model.MetadataKeyContentCaptureMode: model.ContentCaptureModeMetadataOnly,
		}),
	}
}

func TestStripGeneration_ClearsContentKeepsStructure(t *testing.T) {
	g := fullContentGeneration()
	contentcapture.StripGeneration(g, "")

	if g.GetSystemPrompt() != "" {
		t.Errorf("system_prompt not cleared: %q", g.GetSystemPrompt())
	}
	if g.GetRawArtifacts() != nil {
		t.Errorf("raw_artifacts not cleared: %v", g.GetRawArtifacts())
	}

	if got := len(g.GetInput()); got != 2 {
		t.Fatalf("input message count = %d, want 2", got)
	}
	if got := len(g.GetOutput()); got != 1 {
		t.Fatalf("output message count = %d, want 1", got)
	}
	if got := len(g.GetInput()[0].GetParts()); got != 2 {
		t.Fatalf("input[0] part count = %d, want 2", got)
	}
	if got := len(g.GetOutput()[0].GetParts()); got != 3 {
		t.Fatalf("output[0] part count = %d, want 3", got)
	}

	userText := g.GetInput()[0].GetParts()[0]
	if userText.GetText() != "" {
		t.Errorf("input text not cleared: %q", userText.GetText())
	}
	if _, ok := userText.GetPayload().(*agento11yv1.Part_Text); !ok {
		t.Errorf("input text part lost its payload kind: %T", userText.GetPayload())
	}

	media := g.GetInput()[0].GetParts()[1].GetMedia()
	if media.GetUrl() != "" {
		t.Errorf("media url not cleared: %q", media.GetUrl())
	}
	if media.GetMimeType() != "image/png" || media.GetKind() != "image" || media.GetName() != "map.png" {
		t.Errorf("media reference fields changed: %v", media)
	}

	result := g.GetInput()[1].GetParts()[0].GetToolResult()
	if result.GetContent() != "" || result.GetContentJson() != nil {
		t.Errorf("tool result content not cleared: %v", result)
	}
	if result.GetToolCallId() != "call_1" || result.GetName() != "weather" || !result.GetIsError() {
		t.Errorf("tool result structure changed: %v", result)
	}

	if thinking := g.GetOutput()[0].GetParts()[0]; thinking.GetThinking() != "" {
		t.Errorf("thinking not cleared: %q", thinking.GetThinking())
	}
	call := g.GetOutput()[0].GetParts()[1].GetToolCall()
	if call.GetInputJson() != nil {
		t.Errorf("tool call input_json not cleared: %s", call.GetInputJson())
	}
	if call.GetName() != "weather" || call.GetId() != "call_1" {
		t.Errorf("tool call structure changed: %v", call)
	}

	tool := g.GetTools()[0]
	if tool.GetDescription() != "" || tool.GetInputSchemaJson() != nil {
		t.Errorf("tool definition content not cleared: %v", tool)
	}
	if tool.GetName() != "weather" || tool.GetType() != "function" {
		t.Errorf("tool definition structure changed: %v", tool)
	}

	if got := g.GetUsage().GetTotalTokens(); got != 162 {
		t.Errorf("usage.total_tokens = %d, want 162", got)
	}
	if g.GetStopReason() != "end_turn" {
		t.Errorf("stop_reason = %q, want end_turn", g.GetStopReason())
	}
	if !g.GetCompletedAt().AsTime().Equal(fixedTime()) {
		t.Errorf("completed_at = %v, want %v", g.GetCompletedAt().AsTime(), fixedTime())
	}

	// One scan over the encoded payload catches any content field the
	// assertions above do not name.
	raw, err := proto.Marshal(g)
	if err != nil {
		t.Fatalf("marshal stripped generation: %v", err)
	}
	if bytes.Contains(raw, []byte(leakMarker)) {
		t.Errorf("stripped generation still contains the leak marker: %s", raw)
	}
}

func TestStripGeneration_CallError(t *testing.T) {
	tests := []struct {
		name      string
		callError string
		category  string
		want      string
	}{
		{
			name:      "category replaces raw error",
			callError: "429 too many requests for prompt " + leakMarker,
			category:  "rate_limit",
			want:      "rate_limit",
		},
		{
			name:      "no category falls back to sdk_error",
			callError: "429 too many requests for prompt " + leakMarker,
			category:  "",
			want:      contentcapture.StrippedCallError,
		},
		{
			name:      "empty error stays empty",
			callError: "",
			category:  "rate_limit",
			want:      "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := &agento11yv1.Generation{CallError: tc.callError}
			contentcapture.StripGeneration(g, tc.category)
			if got := g.GetCallError(); got != tc.want {
				t.Errorf("call_error = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStripGeneration_Metadata(t *testing.T) {
	g := fullContentGeneration()
	contentcapture.StripGeneration(g, "rate_limit")

	fields := g.GetMetadata().GetFields()
	for _, key := range []string{
		contentcapture.MetadataKeyCallError,
		contentcapture.ConversationTitleKey,
		contentcapture.LegacyConversationTitleKey,
	} {
		if _, present := fields[key]; present {
			t.Errorf("metadata key %q survived the strip", key)
		}
	}

	if got := fields["user.key"].GetStringValue(); got != "keep me" {
		t.Errorf("metadata user.key = %q, want %q", got, "keep me")
	}
	if got := fields["agento11y.sdk.content_capture"].GetStringValue(); got != "not the mode key" {
		t.Errorf("unrelated metadata key was rewritten: %q", got)
	}
}

// TestStripGeneration_DoesNotStampContentCaptureMode pins the split with the
// caller: the strip itself never writes the mode marker.
func TestStripGeneration_DoesNotStampContentCaptureMode(t *testing.T) {
	const modeKey = "agento11y.sdk.content_capture_mode"

	t.Run("absent stays absent", func(t *testing.T) {
		g := &agento11yv1.Generation{
			SystemPrompt: leakMarker,
			Metadata:     mustStruct(map[string]any{"user.key": "keep me"}),
		}
		contentcapture.StripGeneration(g, "")
		if _, present := g.GetMetadata().GetFields()[modeKey]; present {
			t.Errorf("%s was stamped by the strip", modeKey)
		}
	})

	t.Run("incoming full stamp is left alone", func(t *testing.T) {
		g := &agento11yv1.Generation{
			SystemPrompt: leakMarker,
			Metadata:     mustStruct(map[string]any{modeKey: "full"}),
		}
		contentcapture.StripGeneration(g, "")
		if got := g.GetMetadata().GetFields()[modeKey].GetStringValue(); got != "full" {
			t.Errorf("%s = %q, want the caller's value to be untouched", modeKey, got)
		}
	})

	t.Run("metadata holding only content keys is cleared", func(t *testing.T) {
		g := &agento11yv1.Generation{
			Metadata: mustStruct(map[string]any{contentcapture.ConversationTitleKey: leakMarker}),
		}
		contentcapture.StripGeneration(g, "")
		if g.GetMetadata() != nil {
			t.Errorf("emptied metadata struct survived: %v", g.GetMetadata())
		}
	})
}

// TestStripGeneration_NormalizesEmptyMetadata covers an input only a forwarder
// sees: another exporter can send "metadata": {}, which decodes to a set but
// empty Struct. The strip normalizes it to unset so both implementations still
// encode to the same bytes.
func TestStripGeneration_NormalizesEmptyMetadata(t *testing.T) {
	tests := []struct {
		name     string
		metadata *structpb.Struct
	}{
		{name: "empty struct", metadata: &structpb.Struct{}},
		{name: "struct with an empty field map", metadata: &structpb.Struct{Fields: map[string]*structpb.Value{}}},
		{name: "unset", metadata: nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := &agento11yv1.Generation{Id: "g1", Metadata: tc.metadata}
			contentcapture.StripGeneration(g, "")
			if g.GetMetadata() != nil {
				t.Errorf("metadata = %v, want unset", g.GetMetadata())
			}
		})
	}
}

func TestIsTraceContentAttribute(t *testing.T) {
	tests := []struct {
		key  string
		want bool
	}{
		{key: "gen_ai.tool.call.arguments", want: true},
		{key: "gen_ai.tool.call.result", want: true},
		{key: "gen_ai.tool.description", want: true},
		{key: "gen_ai.embeddings.input_texts", want: true},
		{key: "agento11y.conversation.title", want: true},
		{key: "sigil.conversation.title", want: true},
		// The semconv content documents and the raw artifacts appear on a
		// generation span under the otel export protocol.
		{key: "gen_ai.system_instructions", want: true},
		{key: "gen_ai.input.messages", want: true},
		{key: "gen_ai.output.messages", want: true},
		{key: "gen_ai.tool.definitions", want: true},
		{key: "gen_ai.retrieval.query.text", want: true},
		{key: "gen_ai.retrieval.documents", want: true},
		{key: "agento11y.generation.raw_artifacts", want: true},
		{key: "gen_ai.tool.name", want: false},
		{key: "gen_ai.usage.input_tokens", want: false},
		{key: contentcapture.ErrorCategoryAttributeKey, want: false},
		{key: contentcapture.MetadataKeyCallError, want: false},
		{key: "agento11y.generation.id", want: false},
		{key: "", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.key, func(t *testing.T) {
			if got := contentcapture.IsTraceContentAttribute(tc.key); got != tc.want {
				t.Errorf("IsTraceContentAttribute(%q) = %t, want %t", tc.key, got, tc.want)
			}
		})
	}
}

// TestSharedDeclarations pins the wire spellings. Renaming any of these breaks
// a reader: an OTel consumer reads the event and attribute names, and Cloud
// reads the replacement error value.
func TestSharedDeclarations(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{name: "ExceptionEventName", got: contentcapture.ExceptionEventName, want: "exception"},
		{name: "ErrorCategoryAttributeKey", got: contentcapture.ErrorCategoryAttributeKey, want: "error.category"},
		{name: "StrippedCallError", got: contentcapture.StrippedCallError, want: "sdk_error"},
		{name: "MetadataKeyCallError", got: contentcapture.MetadataKeyCallError, want: "call_error"},
		{name: "ConversationTitleKey", got: contentcapture.ConversationTitleKey, want: "agento11y.conversation.title"},
		{name: "LegacyConversationTitleKey", got: contentcapture.LegacyConversationTitleKey, want: "sigil.conversation.title"},
		{name: "EmbeddingInputTextsAttributeKey", got: contentcapture.EmbeddingInputTextsAttributeKey, want: "gen_ai.embeddings.input_texts"},
		{name: "ToolDescriptionAttributeKey", got: contentcapture.ToolDescriptionAttributeKey, want: "gen_ai.tool.description"},
		{name: "ToolCallArgumentsAttributeKey", got: contentcapture.ToolCallArgumentsAttributeKey, want: "gen_ai.tool.call.arguments"},
		{name: "ToolCallResultAttributeKey", got: contentcapture.ToolCallResultAttributeKey, want: "gen_ai.tool.call.result"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
			}
		})
	}
}

func TestStripGeneration_NilSafety(t *testing.T) {
	contentcapture.StripGeneration(nil, "")

	g := &agento11yv1.Generation{
		Input: []*agento11yv1.Message{
			nil,
			{Parts: []*agento11yv1.Part{
				nil,
				{},
				{Payload: &agento11yv1.Part_ToolCall{}},
				{Payload: &agento11yv1.Part_ToolResult{}},
				{Payload: &agento11yv1.Part_Media{}},
				// Typed-nil oneof wrappers. A nil pointer in a payload
				// interface is not a nil interface, so it reaches its case in
				// stripMessage and the case has to guard the wrapper itself.
				{Payload: (*agento11yv1.Part_Text)(nil)},
				{Payload: (*agento11yv1.Part_Thinking)(nil)},
				{Payload: (*agento11yv1.Part_ToolCall)(nil)},
				{Payload: (*agento11yv1.Part_ToolResult)(nil)},
				{Payload: (*agento11yv1.Part_Media)(nil)},
			}},
		},
		Output: []*agento11yv1.Message{nil},
		Tools:  []*agento11yv1.ToolDefinition{nil, {Name: "weather", Description: leakMarker}},
	}
	contentcapture.StripGeneration(g, "")

	if got := len(g.GetInput()[1].GetParts()); got != 10 {
		t.Errorf("part count = %d, want 10", got)
	}
	if got := g.GetTools()[1].GetDescription(); got != "" {
		t.Errorf("tool description = %q, want empty", got)
	}
	if got := g.GetTools()[1].GetName(); got != "weather" {
		t.Errorf("tool name = %q, want weather", got)
	}
}

// Markers on the two constant blocks in contentcapture.go.
const (
	contentKeyMarker      = "//contentcapture:content-keys"
	nonContentValueMarker = "//contentcapture:non-content-values"
)

// TestContentKeyBlockMatchesPredicate pins the content-key constant block to
// IsTraceContentAttribute's cases.
//
// Go has no reflection over package constants, so the test reads the source.
// Without it, adding a key to the content block and forgetting the predicate
// compiles, keeps TestSharedDeclarations green (it only pins string values) and
// keeps TestIsTraceContentAttribute green (it lists keys by hand), and a
// forwarder relays the new attribute to Cloud under a reduced mode.
func TestContentKeyBlockMatchesPredicate(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "contentcapture.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse contentcapture.go: %v", err)
	}

	contentKeys := markedStringConsts(t, file, contentKeyMarker)
	nonContentValues := markedStringConsts(t, file, nonContentValueMarker)
	switchCases := traceContentSwitchCases(t, file)

	for _, name := range slices.Sorted(maps.Keys(contentKeys)) {
		if !contentcapture.IsTraceContentAttribute(contentKeys[name]) {
			t.Errorf("%s = %q is declared in the content-key block but IsTraceContentAttribute reports it as metadata: add it to the switch, or move the constant to the %s block if it carries no content",
				name, contentKeys[name], nonContentValueMarker)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(nonContentValues)) {
		if contentcapture.IsTraceContentAttribute(nonContentValues[name]) {
			t.Errorf("%s = %q is declared as a non-content value but IsTraceContentAttribute reports it as content", name, nonContentValues[name])
		}
	}

	if want, got := slices.Sorted(maps.Keys(contentKeys)), slices.Sorted(maps.Keys(switchCases)); !slices.Equal(want, got) {
		t.Errorf("IsTraceContentAttribute cases = %v, want exactly the content-key block %v", got, want)
	}
}

// markedStringConsts returns the name/value pairs of the string constants in
// the const block carrying marker in its doc comment.
func markedStringConsts(t *testing.T, file *ast.File, marker string) map[string]string {
	t.Helper()

	out := map[string]string{}
	found := false
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST || !hasComment(gen.Doc, marker) {
			continue
		}
		found = true
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range value.Names {
				literal, ok := value.Values[i].(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					t.Errorf("%s: %s is not a string literal, so this test can no longer read the block", marker, name.Name)
					continue
				}
				unquoted, err := strconv.Unquote(literal.Value)
				if err != nil {
					t.Errorf("%s: unquote %s: %v", marker, name.Name, err)
					continue
				}
				out[name.Name] = unquoted
			}
		}
	}
	if !found {
		t.Fatalf("no const block in contentcapture.go carries the %s marker: the marker is what pins the block to IsTraceContentAttribute, so restore it rather than deleting this test", marker)
	}

	return out
}

// traceContentSwitchCases returns the constant names IsTraceContentAttribute
// matches on.
func traceContentSwitchCases(t *testing.T, file *ast.File) map[string]bool {
	t.Helper()

	out := map[string]bool{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "IsTraceContentAttribute" {
			continue
		}
		ast.Inspect(fn, func(node ast.Node) bool {
			clause, ok := node.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, expr := range clause.List {
				ident, ok := expr.(*ast.Ident)
				if !ok {
					t.Errorf("IsTraceContentAttribute matches on %T rather than a named constant, so this test can no longer read it", expr)
					continue
				}
				out[ident.Name] = true
			}
			return true
		})
		return out
	}
	t.Fatalf("IsTraceContentAttribute not found in contentcapture.go")

	return nil
}

func hasComment(group *ast.CommentGroup, want string) bool {
	if group == nil {
		return false
	}
	for _, comment := range group.List {
		if strings.TrimSpace(comment.Text) == want {
			return true
		}
	}
	return false
}
