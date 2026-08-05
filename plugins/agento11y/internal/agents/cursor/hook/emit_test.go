package hook

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/grafana/agento11y/go/agento11y"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/config"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/fragment"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/mapper"
	"github.com/grafana/agento11y/plugins/agento11y/internal/emit"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
)

// buildClient passes the cursor User-Agent token to the emit package; this guards the
// wiring end to end through a real export request.
func TestEmitGenerationSendsCursorUserAgent(t *testing.T) {
	var gotUA string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		var request struct {
			Generations []struct {
				ID string `json:"id"`
			} `json:"generations"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		results := make([]map[string]any, 0, len(request.Generations))
		for _, g := range request.Generations {
			results = append(results, map[string]any{"generation_id": g.ID, "accepted": true})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
	}))
	defer server.Close()
	t.Setenv("SIGIL_ENDPOINT", server.URL)
	t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
	t.Setenv("SIGIL_AUTH_TOKEN", "token")

	frag := &fragment.Fragment{ConversationID: "conv", GenerationID: "gen-1", Model: "gpt-5"}
	mapped := mapper.MapFragment(mapper.Inputs{
		Fragment:       frag,
		Stop:           &mapper.StopInput{Status: "completed"},
		ContentCapture: agento11y.ContentCaptureModeMetadataOnly,
		Now:            time.Now(),
	})

	ctx := context.Background()
	client := buildClient(Payload{}, nil, config.Config{ContentCapture: agento11y.ContentCaptureModeMetadataOnly}, nil, log.New(io.Discard, "", 0))
	if err := emitGeneration(ctx, client, frag, mapped, nil); err != nil {
		t.Fatalf("emitGeneration: %v", err)
	}
	if err := client.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	_ = client.Shutdown(ctx)

	if !strings.HasPrefix(gotUA, "agento11y-plugin-cursor/") {
		t.Fatalf("User-Agent = %q, want agento11y-plugin-cursor/ prefix", gotUA)
	}
}

// TestBuildClientAttachesAutoTags covers the opt-in switch on a live client
// path: with AGENTO11Y_AUTO_CODING_AGENT_TAGS on, the values the launcher resolves reach
// the export as client tags. Cursor is the one agent that knows a workspace
// root and a user identity, so this also pins where they come from: cursor
// sends both on sessionStart, and the stop and sessionEnd payloads that emit
// carry neither, so those read them back from the saved session.
func TestBuildClientAttachesAutoTags(t *testing.T) {
	var gotTags map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		var request struct {
			Generations []struct {
				ID   string            `json:"id"`
				Tags map[string]string `json:"tags"`
			} `json:"generations"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		results := make([]map[string]any, 0, len(request.Generations))
		for _, g := range request.Generations {
			gotTags = g.Tags
			results = append(results, map[string]any{"generation_id": g.ID, "accepted": true})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
	}))
	defer server.Close()

	envconfig.PinAliasEnvBlank(t)
	t.Setenv("SIGIL_ENDPOINT", server.URL)
	t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
	t.Setenv("SIGIL_AUTH_TOKEN", "token")
	t.Setenv("AGENTO11Y_AUTO_CODING_AGENT_TAGS", "true")

	workspace := t.TempDir()
	writeGitFixture(t, workspace, "git@github.com:grafana/agento11y.git", "feature/auto-tags")

	tests := []struct {
		name    string
		payload Payload
		session *fragment.Session
	}{
		{
			name:    "payload carries the identity",
			payload: Payload{WorkspaceRoots: []string{workspace}, UserEmail: "alice@example.com"},
		},
		{
			name:    "stop payload omits it, session supplies it",
			payload: Payload{ConversationID: "conv"},
			session: &fragment.Session{
				ConversationID: "conv",
				WorkspaceRoots: []string{workspace},
				UserEmail:      "alice@example.com",
			},
		},
		{
			name:    "session without identity falls back to the payload",
			payload: Payload{ConversationID: "conv", WorkspaceRoots: []string{workspace}, UserEmail: "alice@example.com"},
			session: &fragment.Session{ConversationID: "conv"},
		},
	}

	want := map[string]string{
		"user":       "alice@example.com",
		"repo":       "grafana/agento11y",
		"git.branch": "feature/auto-tags",
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotTags = nil

			frag := &fragment.Fragment{ConversationID: "conv", GenerationID: "gen-1", Model: "gpt-5"}
			mapped := mapper.MapFragment(mapper.Inputs{
				Fragment:       frag,
				Stop:           &mapper.StopInput{Status: "completed"},
				ContentCapture: agento11y.ContentCaptureModeMetadataOnly,
				Now:            time.Now(),
			})

			ctx := context.Background()
			client := buildClient(tc.payload, tc.session, config.Config{ContentCapture: agento11y.ContentCaptureModeMetadataOnly}, nil, log.New(io.Discard, "", 0))
			if err := emitGeneration(ctx, client, frag, mapped, nil); err != nil {
				t.Fatalf("emitGeneration: %v", err)
			}
			if err := client.Flush(ctx); err != nil {
				t.Fatalf("Flush: %v", err)
			}
			_ = client.Shutdown(ctx)

			for key, value := range want {
				if gotTags[key] != value {
					t.Errorf("exported tag %s = %q, want %q (all tags: %v)", key, gotTags[key], value, gotTags)
				}
			}
		})
	}
}

// writeGitFixture creates the two files gitbranch reads: the origin remote in
// the config and the checked out branch in HEAD. No `git` binary needed.
func writeGitFixture(t *testing.T, root, remote, branch string) {
	t.Helper()
	gitDir := filepath.Join(root, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "config"), []byte("[remote \"origin\"]\n\turl = "+remote+"\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/"+branch+"\n"), 0o644); err != nil {
		t.Fatalf("write HEAD: %v", err)
	}
}

// Tool arguments and results reach the OTel span on a path the generation
// export never touches, so the mapper's redaction does not cover them. The
// span carries the same bytes and needs its own pass.
func TestEmitToolSpans_RedactsContent(t *testing.T) {
	const token = "glc_abcdefghijklmnopqrstuvwxyz"
	const apiKey = "kR7fQ2wLmZ9xTb4vNc1JhY6s"

	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	logger := log.New(io.Discard, "", 0)
	client := agento11y.NewClient(agento11y.Config{
		ContentCapture: agento11y.ContentCaptureModeFull,
		Tracer:         tp.Tracer("test"),
		Logger:         logger,
	})
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })

	frag := &fragment.Fragment{
		ConversationID: "conv",
		GenerationID:   "gen-1",
		Tools: []fragment.ToolRecord{{
			ToolName:    "Bash",
			ToolUseID:   "tu-1",
			ToolInput:   json.RawMessage(`{"command":"deploy.sh","api_key":"` + apiKey + `"}`),
			ToolOutput:  json.RawMessage(`{"stdout":"authenticated with ` + token + `"}`),
			Status:      "completed",
			CompletedAt: "2026-04-28T12:00:05Z",
		}},
	}
	gen := agento11y.Generation{
		ID:             "gen-1",
		ConversationID: "conv",
		AgentName:      mapper.AgentName,
		Model:          agento11y.ModelRef{Provider: "openai", Name: "gpt-5-cursor"},
		CompletedAt:    time.Date(2026, 4, 28, 12, 0, 30, 0, time.UTC),
	}

	emitToolSpans(context.Background(), client, frag, gen, logger)
	_ = client.Shutdown(context.Background())
	_ = tp.Shutdown(context.Background())

	var attrs map[string]string
	for _, s := range recorder.Ended() {
		if !strings.HasPrefix(s.Name(), "execute_tool ") {
			continue
		}
		attrs = map[string]string{}
		for _, kv := range s.Attributes() {
			attrs[string(kv.Key)] = kv.Value.AsString()
		}
	}
	if attrs == nil {
		t.Fatal("no execute_tool span recorded")
	}

	cases := []struct {
		attr     string
		wantMark string
	}{
		{"gen_ai.tool.call.arguments", "[REDACTED:json-secret-field]"},
		{"gen_ai.tool.call.result", "[REDACTED:grafana-cloud-token]"},
	}
	for _, tc := range cases {
		t.Run(tc.attr, func(t *testing.T) {
			got, ok := attrs[tc.attr]
			if !ok {
				t.Fatalf("span is missing %s; recorded attributes: %v", tc.attr, attrs)
			}
			for _, secret := range []string{token, apiKey} {
				if strings.Contains(got, secret) {
					t.Errorf("%s leaks %q: %s", tc.attr, secret, got)
				}
			}
			if !strings.Contains(got, tc.wantMark) {
				t.Errorf("%s = %s; want it to contain %s", tc.attr, got, tc.wantMark)
			}
		})
	}
	if args := attrs["gen_ai.tool.call.arguments"]; !strings.Contains(args, "deploy.sh") {
		t.Errorf("non-secret argument dropped: %s", args)
	}
}

// Two tool records with different completedAt timestamps must produce
// distinct, non-overlapping windows so the UI can show the real
// CALL→TOOL→CALL→TOOL interleaving instead of stacking spans at the end.
// The window math lives in emit.ToolSpanWindow; this guards cursor's
// reliance on it.
func TestToolSpanWindow_PreservesInterleaving(t *testing.T) {
	genEnd := time.Date(2026, 4, 28, 12, 0, 30, 0, time.UTC)
	dur := func(ms float64) *float64 { return &ms }

	_, firstEnd := emit.ToolSpanWindow("2026-04-28T12:00:05Z", dur(1000), genEnd)
	secondStart, _ := emit.ToolSpanWindow("2026-04-28T12:00:20Z", dur(1000), genEnd)
	if !firstEnd.Before(secondStart) {
		t.Errorf("first.completedAt (%s) should precede second.startedAt (%s)", firstEnd, secondStart)
	}
}
