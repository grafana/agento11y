package copilot

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/copilot/fragment"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/stretchr/testify/require"
)

func TestHookUnknownEventDoesNotCrash(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	logger := log.New(io.Discard, "", 0)
	if err := Hook(context.Background(), strings.NewReader(`{"hook_event_name":"Unknown","session_id":"sess"}`), io.Discard, logger); err != nil {
		t.Fatalf("Hook: %v", err)
	}
}

func TestHookUserPromptCreatesTurn(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("SIGIL_CONTENT_CAPTURE_MODE", "full")
	logger := log.New(io.Discard, "", 0)
	if err := Hook(context.Background(), strings.NewReader(`{"hook_event_name":"UserPromptSubmit","session_id":"sess","prompt":"hello","timestamp":"2026-05-18T12:00:00Z"}`), io.Discard, logger); err != nil {
		t.Fatalf("Hook: %v", err)
	}
	got := fragment.LoadTolerant("sess", "turn-000001", logger)
	if got == nil || got.Prompt != "hello" {
		t.Fatalf("expected fragment with prompt, got %+v", got)
	}
}

func TestHookPreToolUseDeniedPropagatesStdout(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("SIGIL_GUARDS_ENABLED", "true")
	t.Setenv("SIGIL_GUARDS_FAIL_OPEN", "true")
	t.Setenv("SIGIL_GUARDS_TIMEOUT_MS", "1500")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"action":"deny","reason":"blocked"}`))
	}))
	defer server.Close()
	t.Setenv("SIGIL_ENDPOINT", server.URL)
	t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
	t.Setenv("SIGIL_AUTH_TOKEN", "token")

	var stdout bytes.Buffer
	payload := `{"hook_event_name":"preToolUse","session_id":"sess","tool_name":"bash","toolArgs":{"cmd":"rm -rf /"}}`
	logger := log.New(io.Discard, "", 0)
	if err := Hook(context.Background(), strings.NewReader(payload), &stdout, logger); err != nil {
		t.Fatalf("Hook: %v", err)
	}
	if !strings.Contains(stdout.String(), `"permissionDecision":"deny"`) {
		t.Fatalf("stdout = %q, want deny decision", stdout.String())
	}
	if !strings.Contains(stdout.String(), `A Grafana Agent Observability policy`) ||
		!strings.Contains(stdout.String(), `Reason: blocked`) {
		t.Fatalf("stdout = %q, want wrapped deny reason", stdout.String())
	}
}

func TestHookPreToolUseAllowKeepsStdoutEmpty(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("SIGIL_GUARDS_ENABLED", "true")
	t.Setenv("SIGIL_GUARDS_FAIL_OPEN", "true")
	t.Setenv("SIGIL_GUARDS_TIMEOUT_MS", "1500")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"action":"allow"}`))
	}))
	defer server.Close()
	t.Setenv("SIGIL_ENDPOINT", server.URL)
	t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
	t.Setenv("SIGIL_AUTH_TOKEN", "token")

	var stdout bytes.Buffer
	payload := `{"hook_event_name":"preToolUse","session_id":"sess","tool_name":"bash","toolArgs":{"cmd":"echo hi"}}`
	logger := log.New(io.Discard, "", 0)
	if err := Hook(context.Background(), strings.NewReader(payload), &stdout, logger); err != nil {
		t.Fatalf("Hook: %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
}

func TestHookSkillClassification(t *testing.T) {
	for _, mode := range []agento11y.ContentCaptureMode{
		agento11y.ContentCaptureModeFull, agento11y.ContentCaptureModeNoToolContent,
		agento11y.ContentCaptureModeFullWithMetadataSpans, agento11y.ContentCaptureModeMetadataOnly,
	} {
		for _, surface := range []string{"copilot-cli", "vscode", "unknown"} {
			for _, event := range []string{"postToolUse", "PostToolUse", "postToolUseFailure", "PostToolUseFailure"} {
				t.Run(mode.String()+"/"+surface+"/"+event, func(t *testing.T) {
					envconfig.PinAliasEnvBlank(t)
					t.Setenv("XDG_STATE_HOME", t.TempDir())
					t.Setenv("AGENTO11Y_CONTENT_CAPTURE_MODE", mode.String())
					t.Setenv("AGENTO11Y_COPILOT_HOOK_SURFACE", surface)
					logger := log.New(io.Discard, "", 0)
					var stdout bytes.Buffer
					pre := `{"hook_event_name":"PreToolUse","session_id":"sess","tool_name":"skill","tool_input":{"skill":"original"}}`
					require.NoError(t, Hook(context.Background(), strings.NewReader(pre), &stdout, logger))
					before := fragment.LoadTolerant("sess", "turn-000001", logger)
					require.NotNil(t, before)
					require.Len(t, before.Tools, 1)
					require.Empty(t, before.Tools[0].SkillName, "pre arguments are not effective-input evidence")

					input := any(map[string]string{"skill": "review", "private": "private-input"})
					if strings.HasPrefix(event, "Post") {
						input = `{"skill":"review","private":"private-input"}`
					}
					raw, err := json.Marshal(map[string]any{
						"hookEventName": event, "sessionId": "sess", "toolName": "skill",
						"toolArgs": input, "toolResult": map[string]string{"text_result_for_llm": "private-result"},
						"SurfaceMarker": "copilot-cli", "surface": "copilot-cli",
					})
					require.NoError(t, err)
					require.NoError(t, Hook(context.Background(), bytes.NewReader(raw), &stdout, logger))
					require.Empty(t, stdout.String())
					got := fragment.LoadTolerant("sess", "turn-000001", logger)
					require.NotNil(t, got)
					require.Equal(t, surface, got.Surface)
					require.Len(t, got.Tools, 1)
					tool := got.Tools[0]
					require.Equal(t, before.Tools[0].ToolUseID, tool.ToolUseID)
					require.Equal(t, "skill", tool.ToolName)
					wantSkill, wantStatus := "", "completed"
					if strings.HasSuffix(event, "Failure") {
						wantStatus = "error"
					} else if surface == "copilot-cli" {
						wantSkill = "review"
					}
					require.Equal(t, wantSkill, tool.SkillName)
					require.Equal(t, wantStatus, tool.Status)
					require.False(t, tool.SkillAttributionAmbiguous)
					if mode != agento11y.ContentCaptureModeFull {
						require.Empty(t, tool.ToolInput)
						require.Empty(t, tool.ToolResponse)
					}
				})
			}
		}
	}
}

func TestHookUsesEventFromEnvFallback(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("SIGIL_CONTENT_CAPTURE_MODE", "full")
	t.Setenv("SIGIL_COPILOT_HOOK_EVENT", "userPromptSubmitted")
	logger := log.New(io.Discard, "", 0)
	if err := Hook(context.Background(), strings.NewReader(`{"sessionId":"sess","prompt":"hello","timestamp":1747579200000}`), io.Discard, logger); err != nil {
		t.Fatalf("Hook: %v", err)
	}
	got := fragment.LoadTolerant("sess", "turn-000001", logger)
	if got == nil || got.Prompt != "hello" {
		t.Fatalf("expected fragment with prompt from env-dispatched event, got %+v", got)
	}
}
