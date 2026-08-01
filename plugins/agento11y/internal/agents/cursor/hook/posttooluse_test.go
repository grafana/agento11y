package hook

import (
	"bytes"
	"encoding/json"
	"log"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/config"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/fragment"
)

// In metadata_only / no_tool_content mode tool input/output gets stripped at
// emit time, so the handler must drop the bytes before they hit disk —
// otherwise a fragment file (mode 0600 still, but on-disk) would carry
// content the user opted out of capturing.
func TestPostToolUse_DropsContentInMetadataOnly(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	logger := log.New(&bytes.Buffer{}, "", 0)
	cfg := config.Config{ContentCapture: agento11y.ContentCaptureModeMetadataOnly}

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
	cfg := config.Config{ContentCapture: agento11y.ContentCaptureModeFull}

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

// The failure message is the one content field the SDK still forwards to the
// span status and error event under no_tool_content, so it is gated on `full`
// mode and redacted here rather than at emit.
func TestPostToolUseFailure_RecordsErrorStatusAndMessage(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	logger := log.New(&bytes.Buffer{}, "", 0)

	cases := []struct {
		name     string
		mode     agento11y.ContentCaptureMode
		errorRaw json.RawMessage
		want     string
	}{
		{"string error", agento11y.ContentCaptureModeFull, json.RawMessage(`"boom"`), "boom"},
		{"object with message", agento11y.ContentCaptureModeFull, json.RawMessage(`{"message":"timeout","code":"E1"}`), "timeout"},
		{"empty error", agento11y.ContentCaptureModeFull, json.RawMessage(``), ""},
		{"object without message", agento11y.ContentCaptureModeFull, json.RawMessage(`{"code":"E1"}`), ""},
		{
			"redacts tier 1 token",
			agento11y.ContentCaptureModeFull,
			json.RawMessage(`"deploy.sh exited 1: authenticated with glc_abcdefghijklmnopqrstuvwxyz"`),
			"deploy.sh exited 1: authenticated with [REDACTED:grafana-cloud-token]",
		},
		{
			"redacts tier 2 env assignment",
			agento11y.ContentCaptureModeFull,
			json.RawMessage(`{"message":"env dump: API_KEY=kR7fQ2wLmZ9xTb4vNc1JhY6s"}`),
			"env dump:[REDACTED:env-secret-value]",
		},
		{"dropped in no_tool_content", agento11y.ContentCaptureModeNoToolContent, json.RawMessage(`"boom"`), ""},
		{"dropped in metadata_only", agento11y.ContentCaptureModeMetadataOnly, json.RawMessage(`"boom"`), ""},
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
			}, config.Config{ContentCapture: tc.mode}, logger, true)

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
	cfg := config.Config{ContentCapture: agento11y.ContentCaptureModeMetadataOnly}
	PostToolUse(Payload{HookEventName: "postToolUse"}, cfg, logger, false)
	if !bytes.Contains(buf.Bytes(), []byte("missing")) {
		t.Errorf("expected 'missing' log; got %q", buf.String())
	}
}
