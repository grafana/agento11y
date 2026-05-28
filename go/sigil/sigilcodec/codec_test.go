package sigilcodec_test

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	sigilv1 "github.com/grafana/sigil-sdk/go/proto/sigil/v1"
	"github.com/grafana/sigil-sdk/go/sigil/sigilcodec"
	"github.com/grafana/sigil-sdk/go/sigil/sigilmodel"
)

func TestToProtoMode(t *testing.T) {
	cases := []struct {
		name string
		mode sigilmodel.GenerationMode
		want sigilv1.GenerationMode
	}{
		{name: "sync", mode: sigilmodel.GenerationModeSync, want: sigilv1.GenerationMode_GENERATION_MODE_SYNC},
		{name: "stream", mode: sigilmodel.GenerationModeStream, want: sigilv1.GenerationMode_GENERATION_MODE_STREAM},
		{name: "empty", mode: sigilmodel.GenerationMode(""), want: sigilv1.GenerationMode_GENERATION_MODE_UNSPECIFIED},
		{name: "unknown", mode: sigilmodel.GenerationMode("UNKNOWN"), want: sigilv1.GenerationMode_GENERATION_MODE_UNSPECIFIED},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := sigilcodec.ToProto(sigilmodel.Generation{Mode: tc.mode})
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
	g := sigilmodel.Generation{
		Input: []sigilmodel.Message{{
			Role: sigilmodel.RoleUser,
			Parts: []sigilmodel.Part{
				{Kind: sigilmodel.PartKindText, Text: "hi"},
			},
		}},
		Output: []sigilmodel.Message{{
			Role: sigilmodel.RoleAssistant,
			Parts: []sigilmodel.Part{
				{Kind: sigilmodel.PartKindThinking, Thinking: "let me think"},
				{Kind: sigilmodel.PartKindToolCall, ToolCall: &sigilmodel.ToolCall{
					ID:        "tc-1",
					Name:      "search",
					InputJSON: []byte(`{"q":"foo"}`),
				}},
			},
		}, {
			Role: sigilmodel.RoleTool,
			Parts: []sigilmodel.Part{
				{Kind: sigilmodel.PartKindToolResult, ToolResult: &sigilmodel.ToolResult{
					ToolCallID:  "tc-1",
					Name:        "search",
					ContentJSON: []byte(`{"hit":1}`),
					IsError:     false,
				}},
			},
		}},
	}

	got, err := sigilcodec.ToProto(g)
	if err != nil {
		t.Fatalf("ToProto: %v", err)
	}

	if got.GetInput()[0].GetRole() != sigilv1.MessageRole_MESSAGE_ROLE_USER {
		t.Errorf("expected USER role, got %v", got.GetInput()[0].GetRole())
	}
	if got.GetOutput()[0].GetRole() != sigilv1.MessageRole_MESSAGE_ROLE_ASSISTANT {
		t.Errorf("expected ASSISTANT role, got %v", got.GetOutput()[0].GetRole())
	}
	if got.GetOutput()[1].GetRole() != sigilv1.MessageRole_MESSAGE_ROLE_TOOL {
		t.Errorf("expected TOOL role, got %v", got.GetOutput()[1].GetRole())
	}

	textPart := got.GetInput()[0].GetParts()[0]
	if textPart.GetText() != "hi" {
		t.Errorf("expected text part %q, got %q", "hi", textPart.GetText())
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

func TestToProtoArtifacts(t *testing.T) {
	g := sigilmodel.Generation{
		Artifacts: []sigilmodel.Artifact{
			{Kind: sigilmodel.ArtifactKindRequest, Name: "req", ContentType: "application/json", Payload: []byte(`{}`)},
			{Kind: sigilmodel.ArtifactKindResponse, Name: "resp"},
			{Kind: sigilmodel.ArtifactKindTools, Name: "tools"},
			{Kind: sigilmodel.ArtifactKindProviderEvent, Name: "ev"},
		},
	}
	got, err := sigilcodec.ToProto(g)
	if err != nil {
		t.Fatalf("ToProto: %v", err)
	}
	arts := got.GetRawArtifacts()
	if len(arts) != 4 {
		t.Fatalf("expected 4 artifacts, got %d", len(arts))
	}
	wantKinds := []sigilv1.ArtifactKind{
		sigilv1.ArtifactKind_ARTIFACT_KIND_REQUEST,
		sigilv1.ArtifactKind_ARTIFACT_KIND_RESPONSE,
		sigilv1.ArtifactKind_ARTIFACT_KIND_TOOLS,
		sigilv1.ArtifactKind_ARTIFACT_KIND_PROVIDER_EVENT,
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
	g := sigilmodel.Generation{
		Metadata: map[string]any{
			"feature":     "test",
			"latency_ms":  float64(123),
			"tags":        []any{"a", "b"},
			"nested_bool": true,
		},
	}
	got, err := sigilcodec.ToProto(g)
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
	g := sigilmodel.Generation{
		StartedAt:   started,
		CompletedAt: completed,
	}
	got, err := sigilcodec.ToProto(g)
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
	empty, err := sigilcodec.ToProto(sigilmodel.Generation{})
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
			got, err := sigilcodec.ToProto(sigilmodel.Generation{EffectiveVersion: tc.input})
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
			got, err := sigilcodec.ToProto(sigilmodel.Generation{EffectiveVersion: tc.input})
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
	g := sigilmodel.Generation{
		Usage: sigilmodel.TokenUsage{
			InputTokens:           10,
			OutputTokens:          5,
			TotalTokens:           15,
			CacheReadInputTokens:  3,
			CacheWriteInputTokens: 2,
			ReasoningTokens:       1,
		},
	}
	got, err := sigilcodec.ToProto(g)
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
