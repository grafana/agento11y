package hook

import (
	"bytes"
	"log"
	"testing"

	"github.com/grafana/agento11y/go/sigil"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/config"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/fragment"
)

func ptrInt64(v int64) *int64 { return &v }

// Assistant text is treated like UserPrompt: dropped from the on-disk
// fragment in metadata_only / default, kept in full / no_tool_content.
// Model, provider, and token counts are metadata and stay in every mode.
func TestAfterAgentResponse_GatesAssistantTextByMode(t *testing.T) {
	cases := []struct {
		name        string
		mode        sigil.ContentCaptureMode
		wantSegment bool
	}{
		{"metadata_only drops text", sigil.ContentCaptureModeMetadataOnly, false},
		{"default drops text", sigil.ContentCaptureModeDefault, false},
		{"full keeps text", sigil.ContentCaptureModeFull, true},
		{"no_tool_content keeps text", sigil.ContentCaptureModeNoToolContent, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			logger := log.New(&bytes.Buffer{}, "", 0)
			cfg := config.Config{ContentCapture: tc.mode}

			AfterAgentResponse(Payload{
				HookEventName:  "afterAgentResponse",
				ConversationID: "conv",
				GenerationID:   "gen",
				Timestamp:      "2026-04-28T12:00:00Z",
				Text:           "hello world",
				Model:          "claude-opus-4-7",
				Provider:       "anthropic",
				InputTokens:    ptrInt64(10),
				OutputTokens:   ptrInt64(20),
			}, cfg, logger)

			got := fragment.LoadTolerant("conv", "gen", logger)
			if got == nil {
				t.Fatalf("fragment not written")
			}

			if tc.wantSegment {
				if len(got.Assistant) != 1 || got.Assistant[0].Text != "hello world" {
					t.Errorf("Assistant = %+v; want one segment with %q", got.Assistant, "hello world")
				}
			} else {
				if len(got.Assistant) != 0 {
					t.Errorf("Assistant text leaked into fragment in %s mode: %+v", tc.mode, got.Assistant)
				}
			}

			// Metadata always preserved.
			if got.Model != "claude-opus-4-7" {
				t.Errorf("Model = %q; want claude-opus-4-7", got.Model)
			}
			if got.Provider != "anthropic" {
				t.Errorf("Provider = %q; want anthropic", got.Provider)
			}
			if got.TokenUsage == nil ||
				got.TokenUsage.InputTokens == nil || *got.TokenUsage.InputTokens != 10 ||
				got.TokenUsage.OutputTokens == nil || *got.TokenUsage.OutputTokens != 20 {
				t.Errorf("TokenUsage = %+v; want input=10 output=20", got.TokenUsage)
			}
		})
	}
}

func TestAfterAgentResponse_MissingIDsSilent(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	cfg := config.Config{ContentCapture: sigil.ContentCaptureModeFull}
	AfterAgentResponse(Payload{HookEventName: "afterAgentResponse"}, cfg, logger)
	if !bytes.Contains(buf.Bytes(), []byte("missing")) {
		t.Errorf("expected 'missing' log; got %q", buf.String())
	}
}
