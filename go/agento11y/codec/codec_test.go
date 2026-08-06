package codec_test

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/grafana/agento11y/go/agento11y/codec"
	"github.com/grafana/agento11y/go/agento11y/model"
	agento11yv1 "github.com/grafana/agento11y/go/proto/agento11y/v1"
)

func TestToProtoMode(t *testing.T) {
	cases := []struct {
		name string
		mode model.GenerationMode
		want agento11yv1.GenerationMode
	}{
		{name: "sync", mode: model.GenerationModeSync, want: agento11yv1.GenerationMode_GENERATION_MODE_SYNC},
		{name: "stream", mode: model.GenerationModeStream, want: agento11yv1.GenerationMode_GENERATION_MODE_STREAM},
		{name: "empty", mode: model.GenerationMode(""), want: agento11yv1.GenerationMode_GENERATION_MODE_UNSPECIFIED},
		{name: "unknown", mode: model.GenerationMode("UNKNOWN"), want: agento11yv1.GenerationMode_GENERATION_MODE_UNSPECIFIED},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := codec.ToProto(model.Generation{Mode: tc.mode})
			if err != nil {
				t.Fatalf("ToProto(%q): %v", tc.mode, err)
			}
			if got.GetMode() != tc.want {
				t.Errorf("mode %q -> %v, want %v", tc.mode, got.GetMode(), tc.want)
			}
		})
	}
}

func TestToProtoRolesAndParts(t *testing.T) {
	g := model.Generation{
		Input: []model.Message{{
			Role: model.RoleUser,
			Parts: []model.Part{
				{Kind: model.PartKindText, Text: "hi"},
				{Kind: model.PartKindMedia, Media: &model.Media{
					Kind:     "image",
					URL:      "data:image/png;base64,abc123",
					MIMEType: "image/png",
					Name:     "prompt.png",
				}},
			},
		}},
		Output: []model.Message{{
			Role: model.RoleAssistant,
			Parts: []model.Part{
				{Kind: model.PartKindThinking, Thinking: "let me think"},
				{Kind: model.PartKindToolCall, ToolCall: &model.ToolCall{
					ID:        "tc-1",
					Name:      "search",
					InputJSON: []byte(`{"q":"foo"}`),
				}},
			},
		}, {
			Role: model.RoleTool,
			Parts: []model.Part{
				{Kind: model.PartKindToolResult, ToolResult: &model.ToolResult{
					ToolCallID:  "tc-1",
					Name:        "search",
					ContentJSON: []byte(`{"hit":1}`),
					IsError:     false,
				}},
			},
		}},
	}

	got, err := codec.ToProto(g)
	if err != nil {
		t.Fatalf("ToProto: %v", err)
	}

	if got.GetInput()[0].GetRole() != agento11yv1.MessageRole_MESSAGE_ROLE_USER {
		t.Errorf("expected USER role, got %v", got.GetInput()[0].GetRole())
	}
	if got.GetOutput()[0].GetRole() != agento11yv1.MessageRole_MESSAGE_ROLE_ASSISTANT {
		t.Errorf("expected ASSISTANT role, got %v", got.GetOutput()[0].GetRole())
	}
	if got.GetOutput()[1].GetRole() != agento11yv1.MessageRole_MESSAGE_ROLE_TOOL {
		t.Errorf("expected TOOL role, got %v", got.GetOutput()[1].GetRole())
	}

	textPart := got.GetInput()[0].GetParts()[0]
	if textPart.GetText() != "hi" {
		t.Errorf("expected text part %q, got %q", "hi", textPart.GetText())
	}
	mediaPart := got.GetInput()[0].GetParts()[1]
	if media := mediaPart.GetMedia(); media == nil || media.GetKind() != "image" || media.GetUrl() != "data:image/png;base64,abc123" || media.GetMimeType() != "image/png" || media.GetName() != "prompt.png" {
		t.Errorf("media mismatch: %+v", media)
	}
	thinkPart := got.GetOutput()[0].GetParts()[0]
	if thinkPart.GetThinking() != "let me think" {
		t.Errorf("expected thinking %q, got %q", "let me think", thinkPart.GetThinking())
	}
	toolCallPart := got.GetOutput()[0].GetParts()[1]
	if call := toolCallPart.GetToolCall(); call == nil || call.GetId() != "tc-1" || call.GetName() != "search" || string(call.GetInputJson()) != `{"q":"foo"}` {
		t.Errorf("tool call mismatch: %+v", call)
	}
	toolResultPart := got.GetOutput()[1].GetParts()[0]
	if res := toolResultPart.GetToolResult(); res == nil || res.GetToolCallId() != "tc-1" || string(res.GetContentJson()) != `{"hit":1}` {
		t.Errorf("tool result mismatch: %+v", res)
	}
}

func TestWorkflowStepToProtoMapsAllFields(t *testing.T) {
	startedAt := time.Date(2026, 2, 11, 12, 0, 0, 0, time.UTC)
	completedAt := startedAt.Add(time.Second)

	got, err := codec.WorkflowStepToProto(model.WorkflowStep{
		ID:                  "wfs-route",
		ConversationID:      "conv-workflow",
		StepName:            "route",
		Framework:           "custom",
		StartedAt:           startedAt,
		CompletedAt:         completedAt,
		InputState:          map[string]any{"prompt": "hello", "count": 1},
		OutputState:         map[string]any{"route": "answer"},
		Error:               "boom",
		Tags:                map[string]string{"env": "test"},
		LinkedGenerationIDs: []string{"gen-1", "gen-2"},
		ParentStepIDs:       []string{"wfs-root"},
		AgentName:           "agent-workflow",
		AgentVersion:        "v1",
		TraceID:             "trace-1",
		SpanID:              "span-1",
		Metadata:            map[string]any{"run_id": "run-1"},
	})
	if err != nil {
		t.Fatalf("WorkflowStepToProto: %v", err)
	}

	if got.GetId() != "wfs-route" {
		t.Fatalf("expected id wfs-route, got %q", got.GetId())
	}
	if got.GetConversationId() != "conv-workflow" {
		t.Fatalf("expected conversation id conv-workflow, got %q", got.GetConversationId())
	}
	if got.GetStepName() != "route" {
		t.Fatalf("expected step name route, got %q", got.GetStepName())
	}
	if got.GetFramework() != "custom" {
		t.Fatalf("expected framework custom, got %q", got.GetFramework())
	}
	if got.GetStartedAt().AsTime() != startedAt {
		t.Fatalf("expected startedAt %s, got %s", startedAt, got.GetStartedAt().AsTime())
	}
	if got.GetCompletedAt().AsTime() != completedAt {
		t.Fatalf("expected completedAt %s, got %s", completedAt, got.GetCompletedAt().AsTime())
	}
	if got.GetInputState().GetFields()["prompt"].GetStringValue() != "hello" {
		t.Fatalf("expected input_state.prompt=hello, got %#v", got.GetInputState())
	}
	if got.GetInputState().GetFields()["count"].GetNumberValue() != 1 {
		t.Fatalf("expected input_state.count=1, got %#v", got.GetInputState())
	}
	if got.GetOutputState().GetFields()["route"].GetStringValue() != "answer" {
		t.Fatalf("expected output_state.route=answer, got %#v", got.GetOutputState())
	}
	if got.GetError() != "boom" {
		t.Fatalf("expected error boom, got %q", got.GetError())
	}
	if got.GetTags()["env"] != "test" {
		t.Fatalf("expected env tag, got %#v", got.GetTags())
	}
	if strings.Join(got.GetLinkedGenerationIds(), ",") != "gen-1,gen-2" {
		t.Fatalf("unexpected linked generation ids: %#v", got.GetLinkedGenerationIds())
	}
	if strings.Join(got.GetParentStepIds(), ",") != "wfs-root" {
		t.Fatalf("unexpected parent step ids: %#v", got.GetParentStepIds())
	}
	if got.GetAgentName() != "agent-workflow" || got.GetAgentVersion() != "v1" {
		t.Fatalf("unexpected agent fields: %q %q", got.GetAgentName(), got.GetAgentVersion())
	}
	if got.GetTraceId() != "trace-1" || got.GetSpanId() != "span-1" {
		t.Fatalf("unexpected trace fields: %q %q", got.GetTraceId(), got.GetSpanId())
	}
	if got.GetMetadata().GetFields()["run_id"].GetStringValue() != "run-1" {
		t.Fatalf("expected metadata.run_id=run-1, got %#v", got.GetMetadata())
	}
}

func TestToProtoArtifacts(t *testing.T) {
	g := model.Generation{
		Artifacts: []model.Artifact{
			{Kind: model.ArtifactKindRequest, Name: "req", ContentType: "application/json", Payload: []byte(`{}`)},
			{Kind: model.ArtifactKindResponse, Name: "resp"},
			{Kind: model.ArtifactKindTools, Name: "tools"},
			{Kind: model.ArtifactKindProviderEvent, Name: "ev"},
		},
	}
	got, err := codec.ToProto(g)
	if err != nil {
		t.Fatalf("ToProto: %v", err)
	}
	arts := got.GetRawArtifacts()
	if len(arts) != 4 {
		t.Fatalf("expected 4 artifacts, got %d", len(arts))
	}
	wantKinds := []agento11yv1.ArtifactKind{
		agento11yv1.ArtifactKind_ARTIFACT_KIND_REQUEST,
		agento11yv1.ArtifactKind_ARTIFACT_KIND_RESPONSE,
		agento11yv1.ArtifactKind_ARTIFACT_KIND_TOOLS,
		agento11yv1.ArtifactKind_ARTIFACT_KIND_PROVIDER_EVENT,
	}
	for i, want := range wantKinds {
		if arts[i].GetKind() != want {
			t.Errorf("artifact[%d] kind = %v, want %v", i, arts[i].GetKind(), want)
		}
	}
	if string(arts[0].GetPayload()) != "{}" {
		t.Errorf("artifact payload = %q, want %q", arts[0].GetPayload(), "{}")
	}
}

func TestToProtoMetadata(t *testing.T) {
	g := model.Generation{
		Metadata: map[string]any{
			"feature":     "test",
			"latency_ms":  float64(123),
			"tags":        []any{"a", "b"},
			"nested_bool": true,
		},
	}
	got, err := codec.ToProto(g)
	if err != nil {
		t.Fatalf("ToProto: %v", err)
	}
	md := got.GetMetadata()
	if md == nil {
		t.Fatal("expected metadata struct, got nil")
	}
	if v := md.GetFields()["feature"].GetStringValue(); v != "test" {
		t.Errorf("feature = %q, want %q", v, "test")
	}
	if v := md.GetFields()["latency_ms"].GetNumberValue(); v != 123 {
		t.Errorf("latency_ms = %v, want 123", v)
	}
	if v := md.GetFields()["nested_bool"].GetBoolValue(); !v {
		t.Errorf("nested_bool = %v, want true", v)
	}
	if v := md.GetFields()["tags"].GetListValue().GetValues(); len(v) != 2 {
		t.Errorf("tags list length = %d, want 2", len(v))
	}
}

func TestToProtoTimestamps(t *testing.T) {
	started := time.Date(2026, 5, 22, 10, 0, 0, 0, time.UTC)
	completed := started.Add(2 * time.Second)
	g := model.Generation{
		StartedAt:   started,
		CompletedAt: completed,
	}
	got, err := codec.ToProto(g)
	if err != nil {
		t.Fatalf("ToProto: %v", err)
	}
	if !got.GetStartedAt().AsTime().Equal(started) {
		t.Errorf("started_at = %v, want %v", got.GetStartedAt().AsTime(), started)
	}
	if !got.GetCompletedAt().AsTime().Equal(completed) {
		t.Errorf("completed_at = %v, want %v", got.GetCompletedAt().AsTime(), completed)
	}

	// Zero timestamps should not be set.
	empty, err := codec.ToProto(model.Generation{})
	if err != nil {
		t.Fatalf("ToProto empty: %v", err)
	}
	if empty.GetStartedAt() != nil {
		t.Errorf("zero StartedAt should map to nil, got %v", empty.GetStartedAt())
	}
}

func TestToProtoEffectiveVersionIsHashed(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  func() string
	}{
		{
			name:  "non-empty input is sha256 hashed",
			input: "v1.2.3+abcdef",
			want: func() string {
				sum := sha256.Sum256([]byte("v1.2.3+abcdef"))
				return "sha256:" + hex.EncodeToString(sum[:])
			},
		},
		{
			name:  "leading and trailing whitespace is trimmed before hashing",
			input: "   v1.2.3+abcdef   ",
			want: func() string {
				sum := sha256.Sum256([]byte("v1.2.3+abcdef"))
				return "sha256:" + hex.EncodeToString(sum[:])
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := codec.ToProto(model.Generation{EffectiveVersion: tc.input})
			if err != nil {
				t.Fatalf("ToProto: %v", err)
			}
			if got.EffectiveVersion == nil {
				t.Fatal("expected effective_version, got nil")
			}
			want := tc.want()
			if *got.EffectiveVersion != want {
				t.Errorf("effective_version = %q, want %q", *got.EffectiveVersion, want)
			}
			if !strings.HasPrefix(*got.EffectiveVersion, "sha256:") {
				t.Errorf("effective_version must be prefixed with sha256:, got %q", *got.EffectiveVersion)
			}
		})
	}
}

func TestToProtoEffectiveVersionEmptyOrWhitespace(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{name: "empty", input: ""},
		{name: "whitespace", input: "   "},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := codec.ToProto(model.Generation{EffectiveVersion: tc.input})
			if err != nil {
				t.Fatalf("ToProto: %v", err)
			}
			if got.EffectiveVersion != nil {
				t.Errorf("effective_version with input %q should be nil, got %q", tc.input, *got.EffectiveVersion)
			}
		})
	}
}

func TestToProtoUsage(t *testing.T) {
	g := model.Generation{
		Usage: model.TokenUsage{
			InputTokens:           10,
			OutputTokens:          5,
			TotalTokens:           15,
			CacheReadInputTokens:  3,
			CacheWriteInputTokens: 2,
			ReasoningTokens:       1,
		},
	}
	got, err := codec.ToProto(g)
	if err != nil {
		t.Fatalf("ToProto: %v", err)
	}
	u := got.GetUsage()
	if u.GetInputTokens() != 10 || u.GetOutputTokens() != 5 || u.GetTotalTokens() != 15 {
		t.Errorf("usage tokens mismatch: %+v", u)
	}
	if u.GetCacheReadInputTokens() != 3 || u.GetCacheWriteInputTokens() != 2 || u.GetReasoningTokens() != 1 {
		t.Errorf("usage cache/reasoning tokens mismatch: %+v", u)
	}
}
