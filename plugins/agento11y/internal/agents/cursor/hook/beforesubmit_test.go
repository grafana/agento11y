package hook

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/config"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/fragment"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
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

			BeforeSubmit(context.Background(), io.Discard, Payload{
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

// A denied message must not become the conversation title because the mapper
// repeats the title on every later generation.
func TestBeforeSubmit_DenyCapturesNothing(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	envconfig.PinAliasEnvBlank(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"action":"deny","reason":"secret in prompt"}`))
	}))
	defer server.Close()

	t.Setenv("SIGIL_GUARDS_ENABLED", "true")
	t.Setenv("SIGIL_ENDPOINT", server.URL)
	t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
	t.Setenv("SIGIL_AUTH_TOKEN", "token")

	logger := log.New(&bytes.Buffer{}, "", 0)
	cfg := config.Config{ContentCapture: agento11y.ContentCaptureModeFull}

	var stdout bytes.Buffer
	BeforeSubmit(context.Background(), &stdout, Payload{
		HookEventName:  "beforeSubmitPrompt",
		ConversationID: "conv",
		GenerationID:   "gen",
		Prompt:         "my token is glc_secret",
	}, cfg, logger)

	if !strings.Contains(stdout.String(), `"continue":false`) {
		t.Fatalf("stdout = %q; want the stop envelope", stdout.String())
	}
	if sess := fragment.LoadSession("conv", logger); sess != nil && sess.ConversationTitle != "" {
		t.Errorf("ConversationTitle = %q; a denied message must not become the title", sess.ConversationTitle)
	}
	if got := fragment.LoadTolerant("conv", "gen", logger); got != nil && got.UserPrompt != "" {
		t.Errorf("UserPrompt = %q; a denied message must not reach the fragment", got.UserPrompt)
	}
}

func TestBeforeSubmit_MissingConversationIDSilent(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	cfg := config.Config{ContentCapture: agento11y.ContentCaptureModeFull}
	BeforeSubmit(context.Background(), io.Discard, Payload{HookEventName: "beforeSubmitPrompt"}, cfg, logger)
	if !bytes.Contains(buf.Bytes(), []byte("skipping")) {
		t.Errorf("expected 'skipping' log; got %q", buf.String())
	}
}

func TestBeforeSubmit_StampsTitleWithoutGenerationID(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	logger := log.New(&bytes.Buffer{}, "", 0)
	cfg := config.Config{ContentCapture: agento11y.ContentCaptureModeFull}

	BeforeSubmit(context.Background(), io.Discard, Payload{
		HookEventName:  "beforeSubmitPrompt",
		ConversationID: "conv",
		Prompt:         "list go files",
	}, cfg, logger)

	sess := fragment.LoadSession("conv", logger)
	if sess == nil {
		t.Fatal("expected a session so the title survives until stop")
	}
	if sess.ConversationTitle != "list go files" {
		t.Errorf("ConversationTitle = %q; want list go files", sess.ConversationTitle)
	}
}

func TestBeforeSubmit_FirstPromptWinsTitle(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	logger := log.New(&bytes.Buffer{}, "", 0)
	cfg := config.Config{ContentCapture: agento11y.ContentCaptureModeFull}

	BeforeSubmit(context.Background(), io.Discard, Payload{
		HookEventName:  "beforeSubmitPrompt",
		ConversationID: "conv",
		GenerationID:   "gen-1",
		Prompt:         "first prompt",
	}, cfg, logger)
	BeforeSubmit(context.Background(), io.Discard, Payload{
		HookEventName:  "beforeSubmitPrompt",
		ConversationID: "conv",
		GenerationID:   "gen-2",
		Prompt:         "second prompt",
	}, cfg, logger)

	sess := fragment.LoadSession("conv", logger)
	if sess == nil {
		t.Fatal("expected a session")
	}
	if sess.ConversationTitle != "first prompt" {
		t.Errorf("ConversationTitle = %q; want first prompt", sess.ConversationTitle)
	}
}

func TestBeforeSubmit_TitleWriteKeepsSessionMetadata(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	logger := log.New(&bytes.Buffer{}, "", 0)
	cfg := config.Config{ContentCapture: agento11y.ContentCaptureModeFull}

	SessionStart(Payload{
		HookEventName:  "sessionStart",
		ConversationID: "conv",
		WorkspaceRoots: []string{"/repo"},
		UserEmail:      "dev@example.com",
		CursorVersion:  "2.0",
		Timestamp:      "2026-04-28T12:00:00Z",
	}, logger)
	BeforeSubmit(context.Background(), io.Discard, Payload{
		HookEventName:  "beforeSubmitPrompt",
		ConversationID: "conv",
		Prompt:         "list go files",
	}, cfg, logger)

	sess := fragment.LoadSession("conv", logger)
	if sess == nil {
		t.Fatal("expected a session")
	}
	if sess.ConversationTitle != "list go files" {
		t.Errorf("ConversationTitle = %q; want list go files", sess.ConversationTitle)
	}
	if sess.UserEmail != "dev@example.com" {
		t.Errorf("UserEmail = %q; want sessionStart metadata kept", sess.UserEmail)
	}
	if len(sess.WorkspaceRoots) != 1 || sess.WorkspaceRoots[0] != "/repo" {
		t.Errorf("WorkspaceRoots = %v; want [/repo]", sess.WorkspaceRoots)
	}
}
