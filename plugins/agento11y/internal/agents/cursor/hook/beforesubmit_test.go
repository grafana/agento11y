package hook

import (
	"bytes"
	"log"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/config"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/fragment"
)

// In metadata_only mode the user prompt gets stripped at emit time, so the
// handler must drop the bytes before they hit disk — fragment file is mode
// 0600 but on-disk persistence of opted-out content is itself the leak.
func TestBeforeSubmit_GatesUserPromptByMode(t *testing.T) {
	cases := []struct {
		name string
		mode agento11y.ContentCaptureMode
		want string
	}{
		{"metadata_only drops prompt", agento11y.ContentCaptureModeMetadataOnly, ""},
		{"default drops prompt", agento11y.ContentCaptureModeDefault, ""},
		{"full keeps prompt", agento11y.ContentCaptureModeFull, "hello"},
		{"no_tool_content keeps prompt", agento11y.ContentCaptureModeNoToolContent, "hello"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			logger := log.New(&bytes.Buffer{}, "", 0)
			cfg := config.Config{ContentCapture: tc.mode}

			BeforeSubmit(Payload{
				HookEventName:  "beforeSubmitPrompt",
				ConversationID: "conv",
				GenerationID:   "gen",
				Timestamp:      "2026-04-28T12:00:00Z",
				Prompt:         "hello",
			}, cfg, logger)

			got := fragment.LoadTolerant("conv", "gen", logger)
			if got == nil {
				t.Fatalf("fragment not written")
			}
			if got.UserPrompt != tc.want {
				t.Errorf("UserPrompt = %q; want %q", got.UserPrompt, tc.want)
			}
			// Touch must always run so downstream handlers see activity.
			if got.LastEventAt == "" {
				t.Errorf("LastEventAt empty; Touch should fire regardless of mode")
			}
		})
	}
}

func TestBeforeSubmit_MissingIDsSilent(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	cfg := config.Config{ContentCapture: agento11y.ContentCaptureModeFull}
	BeforeSubmit(Payload{HookEventName: "beforeSubmitPrompt"}, cfg, logger)
	if !bytes.Contains(buf.Bytes(), []byte("skipping")) {
		t.Errorf("expected 'skipping' log; got %q", buf.String())
	}
}
