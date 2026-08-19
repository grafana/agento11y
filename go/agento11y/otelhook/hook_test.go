package otelhook_test

import (
	"context"
	"math"
	"slices"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"github.com/grafana/agento11y/go/agento11y/contentcapture"
	"github.com/grafana/agento11y/go/agento11y/otelhook"
	"github.com/grafana/agento11y/go/otelgenai"
)

func attributeMap(attrs []attribute.KeyValue) map[string]string {
	out := make(map[string]string, len(attrs))
	for _, kv := range attrs {
		out[string(kv.Key)] = kv.Value.Emit()
	}
	return out
}

func TestOnEnd(t *testing.T) {
	t.Parallel()

	toolChoice := "auto"
	thinking := true

	cases := []struct {
		name    string
		vendor  any
		capture otelgenai.CaptureMode
		want    map[string]string
		absent  []string
	}{
		{
			name: "full generation",
			vendor: otelhook.Generation{
				ID:                      "gen-1",
				UserID:                  "user-1",
				Tags:                    map[string]string{"team": "sigil", " env ": " dev "},
				Metadata:                map[string]any{"model_version": "2026-06"},
				ParentGenerationIDs:     []string{"gen-0"},
				EffectiveVersion:        "v3",
				ToolChoice:              &toolChoice,
				ThinkingEnabled:         &thinking,
				TotalTokens:             162,
				InclusiveTokenSemantics: true,
			},
			want: map[string]string{
				"agento11y.generation.id":                    "gen-1",
				"user.id":                                    "user-1",
				"agento11y.generation.tags":                  `{" env ":" dev ","team":"sigil"}`,
				"agento11y.tag.team":                         "sigil",
				"agento11y.tag.env":                          "dev",
				"agento11y.generation.metadata":              `{"model_version":"2026-06"}`,
				"agento11y.generation.parent_generation_ids": `["gen-0"]`,
				// The digest of "v3", the rule the proto path applies too.
				"agento11y.agent.effective_version":         "sha256:e0d2747b9ab7abb6eb65e0373fa1b428a28bd6d8a2380106dcc080f58005ee14",
				"agento11y.gen_ai.request.tool_choice":      "auto",
				"agento11y.gen_ai.request.thinking.enabled": "true",
				"agento11y.gen_ai.usage.total_tokens":       "162",
				"gen_ai.token.semantics":                    "inclusive",
			},
		},
		{
			name:   "id only",
			vendor: otelhook.Generation{ID: "gen-2"},
			want:   map[string]string{"agento11y.generation.id": "gen-2"},
			absent: []string{
				"user.id",
				"agento11y.generation.tags",
				"agento11y.generation.metadata",
				"gen_ai.token.semantics",
			},
		},
		{
			name:   "no vendor payload",
			vendor: nil,
			absent: []string{"agento11y.generation.id"},
		},
		{
			name:   "foreign vendor payload",
			vendor: "not-a-generation",
			absent: []string{"agento11y.generation.id"},
		},
		{
			name:    "content capture off drops the title and the artifacts",
			capture: otelgenai.CaptureNoContent,
			vendor: otelhook.Generation{
				ID:                "gen-3",
				ConversationTitle: "TITLE-SECRET",
				Metadata: map[string]any{
					"agento11y.conversation.title": "TITLE-SECRET",
					"sigil.conversation.title":     "TITLE-SECRET",
					"call_error":                   "openai: 401 unauthorized (api_key=sk-SECRET)",
					"model_version":                "2026-06",
				},
				Artifacts: []otelhook.Artifact{{Kind: "request", Payload: []byte("SECRET-BODY")}},
			},
			want: map[string]string{
				"agento11y.generation.metadata": `{"model_version":"2026-06"}`,
			},
			absent: []string{
				"agento11y.conversation.title",
				"agento11y.generation.raw_artifacts",
			},
		},
		{
			name:    "content capture on puts the title in its own attribute",
			capture: otelgenai.CaptureSpanOnly,
			vendor: otelhook.Generation{
				ID:                "gen-4",
				ConversationTitle: "Fix the flake",
				Metadata: map[string]any{
					"agento11y.conversation.title": "Fix the flake",
					"call_error":                   "openai: 401 unauthorized (api_key=sk-SECRET)",
					"model_version":                "2026-06",
				},
				Artifacts: []otelhook.Artifact{{Kind: "request", ContentType: "application/json", Payload: []byte(`{"a":1}`)}},
			},
			want: map[string]string{
				"agento11y.conversation.title":       "Fix the flake",
				"agento11y.generation.metadata":      `{"model_version":"2026-06"}`,
				"agento11y.generation.raw_artifacts": `[{"kind":"request","content_type":"application/json","payload":"eyJhIjoxfQ=="}]`,
			},
		},
		{
			name:    "the title mirror alone still reports a title",
			capture: otelgenai.CaptureSpanOnly,
			vendor: otelhook.Generation{
				ID:       "gen-5",
				Metadata: map[string]any{"sigil.conversation.title": "Fix the flake"},
			},
			want:   map[string]string{"agento11y.conversation.title": "Fix the flake"},
			absent: []string{"agento11y.generation.metadata"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := attributeMap(otelhook.New().OnEnd(context.Background(), &otelgenai.Invocation{Vendor: tc.vendor}, tc.capture))
			for key, want := range tc.want {
				if got[key] != want {
					t.Errorf("%s = %q, want %q", key, got[key], want)
				}
			}
			for _, key := range tc.absent {
				if _, ok := got[key]; ok {
					t.Errorf("%s is present, want it absent", key)
				}
			}
		})
	}
}

// TestOnEndKeepsContentUnderClassifiedKeys pins the rule a reducing forwarder
// depends on: content only sits under keys
// contentcapture.IsTraceContentAttribute reports.
func TestOnEndKeepsContentUnderClassifiedKeys(t *testing.T) {
	t.Parallel()

	const secret = "CONTENT-SECRET"
	generation := otelhook.Generation{
		ID:                "gen-1",
		ConversationTitle: secret,
		Tags:              map[string]string{"env": "dev"},
		Metadata: map[string]any{
			"agento11y.conversation.title": secret,
			"sigil.conversation.title":     secret,
			"call_error":                   secret,
			"model_version":                "2026-06",
		},
	}

	attrs := otelhook.New().OnEnd(context.Background(), &otelgenai.Invocation{Vendor: generation}, otelgenai.CaptureSpanOnly)
	for _, kv := range attrs {
		if contentcapture.IsTraceContentAttribute(string(kv.Key)) {
			continue
		}
		if value := kv.Value.Emit(); strings.Contains(value, secret) {
			t.Errorf("%s = %q carries content under a key a forwarder does not strip", kv.Key, value)
		}
	}
	if got := attributeMap(attrs)["agento11y.conversation.title"]; got != secret {
		t.Errorf("agento11y.conversation.title = %q, want %q", got, secret)
	}
}

func TestOnEndNilInvocation(t *testing.T) {
	t.Parallel()

	if got := otelhook.New().OnEnd(context.Background(), nil, otelgenai.CaptureSpanOnly); got != nil {
		t.Fatalf("OnEnd(nil) = %v, want nil", got)
	}
}

// TestOnEndReportsDroppedPayloads pins that a payload the hook cannot encode
// reaches the OTel error handler. Dropping the payload silently leaves a span
// that looks healthy while it is missing the attributes that make it a
// generation.
//
// The error handler is global, so these cases cannot run in parallel.
func TestOnEndReportsDroppedPayloads(t *testing.T) {
	cases := []struct {
		name   string
		vendor any
		want   string
	}{
		{
			name: "metadata that does not marshal",
			vendor: otelhook.Generation{
				ID:       "gen_1",
				Metadata: map[string]any{"score": math.NaN()},
			},
			want: "dropping generation metadata",
		},
		{
			name:   "a vendor payload of another type",
			vendor: struct{ Other string }{Other: "not a generation"},
			want:   "want otelhook.Generation",
		},
		{
			name:   "a nil generation pointer",
			vendor: (*otelhook.Generation)(nil),
			want:   "nil *otelhook.Generation",
		},
		{
			name:   "a generation with no id",
			vendor: otelhook.Generation{},
			want:   "has no id",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var reported []string
			previous := otel.GetErrorHandler()
			otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
				reported = append(reported, err.Error())
			}))
			t.Cleanup(func() { otel.SetErrorHandler(previous) })

			otelhook.New().OnEnd(context.Background(), &otelgenai.Invocation{Vendor: tc.vendor}, otelgenai.CaptureSpanOnly)

			if !slices.ContainsFunc(reported, func(msg string) bool { return strings.Contains(msg, tc.want) }) {
				t.Errorf("reported %v, want one mentioning %q", reported, tc.want)
			}
		})
	}
}

// TestOnEndStaysQuietForForeignInvocations pins the other half of the error
// reporting: the hook can share a handler with instrumentation that has no
// vendor payload at all, and reporting those invocations would put one line
// per span in the host application's error handler.
func TestOnEndStaysQuietForForeignInvocations(t *testing.T) {
	var reported []string
	previous := otel.GetErrorHandler()
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		reported = append(reported, err.Error())
	}))
	t.Cleanup(func() { otel.SetErrorHandler(previous) })

	otelhook.New().OnEnd(context.Background(), &otelgenai.Invocation{}, otelgenai.CaptureSpanOnly)

	if len(reported) != 0 {
		t.Errorf("reported %v for an invocation with no vendor payload, want silence", reported)
	}
}
