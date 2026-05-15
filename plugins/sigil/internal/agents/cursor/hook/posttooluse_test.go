package hook

import (
	"bytes"
	"encoding/json"
	"log"
	"testing"

	"github.com/grafana/sigil-sdk/go/sigil"

	"github.com/grafana/sigil-sdk/plugins/sigil/internal/agents/cursor/config"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/agents/cursor/fragment"
)

// In metadata_only / no_tool_content mode tool input/output gets stripped at
// emit time, so the handler must drop the bytes before they hit disk —
// otherwise a fragment file (mode 0600 still, but on-disk) would carry
// content the user opted out of capturing.
func TestPostToolUse_DropsContentInMetadataOnly(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	logger := log.New(&bytes.Buffer{}, "", 0)
	cfg := config.Config{ContentCapture: sigil.ContentCaptureModeMetadataOnly}

	PostToolUse(Payload{
		HookEventName:  "postToolUse",
		ConversationID: "conv",
		GenerationID:   "gen",
		Timestamp:      "2026-04-28T12:00:00Z",
		ToolName:       "Read",
		ToolUseID:      "t1",
		ToolInput:      json.RawMessage(`{"path":"/etc/secrets"}`),
		ToolOutput:     json.RawMessage(`"big secret content"`),
		Status:         "completed",
	}, cfg, logger, false)

	got := fragment.LoadTolerant("conv", "gen", logger)
	if got == nil || len(got.Tools) != 1 {
		t.Fatalf("expected 1 tool record; got %+v", got)
	}
	tool := got.Tools[0]
	if len(tool.ToolInput) > 0 {
		t.Errorf("ToolInput leaked into fragment in metadata_only mode: %s", tool.ToolInput)
	}
	if len(tool.ToolOutput) > 0 {
		t.Errorf("ToolOutput leaked into fragment in metadata_only mode: %s", tool.ToolOutput)
	}
	if tool.ToolName != "Read" {
		t.Errorf("ToolName = %q; want Read", tool.ToolName)
	}
	if tool.Status != "completed" {
		t.Errorf("Status = %q; want completed", tool.Status)
	}
}

func TestPostToolUse_KeepsContentInFullMode(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	logger := log.New(&bytes.Buffer{}, "", 0)
	cfg := config.Config{ContentCapture: sigil.ContentCaptureModeFull}

	PostToolUse(Payload{
		HookEventName:  "postToolUse",
		ConversationID: "conv",
		GenerationID:   "gen",
		ToolName:       "Read",
		ToolUseID:      "t1",
		ToolInput:      json.RawMessage(`{"path":"x"}`),
		ToolOutput:     json.RawMessage(`"contents"`),
	}, cfg, logger, false)

	got := fragment.LoadTolerant("conv", "gen", logger)
	if got == nil || len(got.Tools) != 1 {
		t.Fatalf("expected 1 tool record; got %+v", got)
	}
	tool := got.Tools[0]
	if string(tool.ToolInput) != `{"path":"x"}` {
		t.Errorf("ToolInput = %s; want {\"path\":\"x\"}", tool.ToolInput)
	}
	if string(tool.ToolOutput) != `"contents"` {
		t.Errorf("ToolOutput = %s; want \"contents\"", tool.ToolOutput)
	}
}

func TestPostToolUseFailure_RecordsErrorStatusAndMessage(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	logger := log.New(&bytes.Buffer{}, "", 0)
	cfg := config.Config{ContentCapture: sigil.ContentCaptureModeMetadataOnly}

	cases := []struct {
		name     string
		errorRaw json.RawMessage
		want     string
	}{
		{"string error", json.RawMessage(`"boom"`), "boom"},
		{"object with message", json.RawMessage(`{"message":"timeout","code":"E1"}`), "timeout"},
		{"empty error", json.RawMessage(``), ""},
		{"object without message", json.RawMessage(`{"code":"E1"}`), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			PostToolUse(Payload{
				HookEventName:  "postToolUseFailure",
				ConversationID: "conv",
				GenerationID:   "gen",
				ToolName:       "Bash",
				Error:          tc.errorRaw,
			}, cfg, logger, true)

			got := fragment.LoadTolerant("conv", "gen", logger)
			if got == nil || len(got.Tools) != 1 {
				t.Fatalf("expected 1 tool record; got %+v", got)
			}
			tool := got.Tools[0]
			if tool.Status != "error" {
				t.Errorf("Status = %q; want error", tool.Status)
			}
			if tool.ErrorMessage != tc.want {
				t.Errorf("ErrorMessage = %q; want %q", tool.ErrorMessage, tc.want)
			}
		})
	}
}

func TestPostToolUse_MissingIDsSilent(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	cfg := config.Config{ContentCapture: sigil.ContentCaptureModeMetadataOnly}
	PostToolUse(Payload{HookEventName: "postToolUse"}, cfg, logger, false)
	if !bytes.Contains(buf.Bytes(), []byte("missing")) {
		t.Errorf("expected 'missing' log; got %q", buf.String())
	}
}
