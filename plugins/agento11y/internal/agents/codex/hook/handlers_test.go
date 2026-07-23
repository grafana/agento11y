package hook

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grafana/agento11y/go/agento11y"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/codex/config"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/codex/fragment"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/codex/mapper"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/grafana/agento11y/plugins/agento11y/internal/redact"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionStartWithoutTurnIDSeedsLaterTurn(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	logger := log.New(io.Discard, "", 0)
	cfg := config.Config{ContentCapture: agento11y.ContentCaptureModeMetadataOnly}

	SessionStart(Payload{
		HookEventName:  "SessionStart",
		SessionID:      "sess",
		CWD:            "/repo",
		Model:          "gpt-5.5",
		Source:         "startup",
		TranscriptPath: "/tmp/transcript.jsonl",
		Timestamp:      "2026-05-11T10:00:00Z",
	}, cfg, logger)

	UserPromptSubmit(Payload{
		HookEventName: "UserPromptSubmit",
		SessionID:     "sess",
		TurnID:        "turn",
		Timestamp:     "2026-05-11T10:00:01Z",
	}, cfg, logger)

	got := fragment.LoadTolerant("sess", "turn", logger)
	if got == nil {
		t.Fatal("expected turn fragment")
	}
	if got.Cwd != "/repo" || got.Model != "gpt-5.5" || got.Source != "startup" || got.TranscriptPath != "/tmp/transcript.jsonl" {
		t.Fatalf("session defaults not inherited: %+v", got)
	}
}

func TestSessionStartRecordsSubagentLink(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	logger := log.New(io.Discard, "", 0)
	cfg := config.Config{ContentCapture: agento11y.ContentCaptureModeMetadataOnly}
	transcriptPath := writeHookTranscript(t, `{"type":"session_meta","payload":{"id":"child","thread_source":"subagent","agent_role":"reviewer","agent_nickname":"Dalton","source":{"subagent":{"thread_spawn":{"parent_thread_id":"parent","depth":1}}}}}`)

	SessionStart(Payload{
		HookEventName:  "SessionStart",
		SessionID:      "child",
		Source:         "startup",
		TranscriptPath: transcriptPath,
	}, cfg, logger)

	got := fragment.LoadSubagentLinkTolerant("child", logger)
	if got == nil {
		t.Fatal("expected subagent link")
	}
	if got.ParentSessionID != "parent" || got.AgentRole != "reviewer" || got.AgentNickname != "Dalton" || got.AgentDepth != 1 || got.Source != "transcript.session_meta" {
		t.Fatalf("unexpected link: %+v", got)
	}
}

func TestRedactSpanContent(t *testing.T) {
	secret := "glc_abcdefghijklmnopqrstuvwxyz"
	raw, _ := json.Marshal(map[string]string{"token": secret})
	got := redactSpanContent(redact.New(), raw)
	if strings.Contains(got, secret) {
		t.Fatalf("unredacted secret in span content: %s", got)
	}
	if !strings.Contains(got, "[REDACTED:") {
		t.Fatalf("missing redaction marker: %s", got)
	}
}

func TestRedactSpanContentRedactsSensitiveJSONKeys(t *testing.T) {
	secret := "npm_" + strings.Repeat("A", 36)
	raw := json.RawMessage(`{"Authorization":"Bearer short","clientSecret":"short-secret","token":"` + secret + `","safe":"visible"}`)
	got := redactSpanContent(redact.New(), raw)
	for _, secret := range []string{"Bearer short", "short-secret", secret} {
		if strings.Contains(got, secret) {
			t.Fatalf("secret %q leaked in span content: %s", secret, got)
		}
	}
	if !strings.Contains(got, "[REDACTED:json-secret-field]") {
		t.Fatalf("missing sensitive key redaction marker: %s", got)
	}
	if !strings.Contains(got, "visible") {
		t.Fatalf("safe value should remain visible: %s", got)
	}
}

func TestPostToolUseLeavesStatusUnknownWhenCodexDoesNotProvideOne(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	logger := log.New(io.Discard, "", 0)
	cfg := config.Config{ContentCapture: agento11y.ContentCaptureModeMetadataOnly}

	PostToolUse(Payload{
		HookEventName: "PostToolUse",
		SessionID:     "sess",
		TurnID:        "turn",
		ToolName:      "Bash",
		ToolUseID:     "tool-1",
		ToolResponse:  json.RawMessage(`{"output":"ok"}`),
	}, cfg, logger)

	got := fragment.LoadTolerant("sess", "turn", logger)
	if got == nil || len(got.Tools) != 1 {
		t.Fatalf("expected one tool record, got %+v", got)
	}
	if got.Tools[0].Status != "" {
		t.Fatalf("Status = %q, want unknown/empty", got.Tools[0].Status)
	}
}

func TestNormalizeStatusIgnoresFalsyErrorValues(t *testing.T) {
	for _, raw := range []string{`null`, `false`, `0`, `""`, `[]`, `{}`} {
		t.Run(raw, func(t *testing.T) {
			got := normalizeStatus(Payload{Error: json.RawMessage(raw)}, nil)
			if got != "" {
				t.Fatalf("Status = %q, want unknown/empty", got)
			}
		})
	}
}

func TestPostToolUseInfersKnownFailureShapes(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	logger := log.New(io.Discard, "", 0)
	cfg := config.Config{ContentCapture: agento11y.ContentCaptureModeMetadataOnly}

	PostToolUse(Payload{
		HookEventName: "PostToolUse",
		SessionID:     "sess",
		TurnID:        "turn",
		ToolName:      "Bash",
		ToolUseID:     "tool-1",
		ToolResponse:  json.RawMessage(`{"metadata":{"exit_code":2}}`),
	}, cfg, logger)

	got := fragment.LoadTolerant("sess", "turn", logger)
	if got == nil || len(got.Tools) != 1 {
		t.Fatalf("expected one tool record, got %+v", got)
	}
	if got.Tools[0].Status != "error" {
		t.Fatalf("Status = %q, want error", got.Tools[0].Status)
	}
}

func TestPostToolUseInfersStatusFromToolOutputFallback(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	logger := log.New(io.Discard, "", 0)
	cfg := config.Config{ContentCapture: agento11y.ContentCaptureModeMetadataOnly}

	PostToolUse(Payload{
		HookEventName: "PostToolUse",
		SessionID:     "sess",
		TurnID:        "turn",
		ToolName:      "Bash",
		ToolUseID:     "tool-1",
		ToolOutput:    json.RawMessage(`{"exitCode":1}`),
	}, cfg, logger)

	got := fragment.LoadTolerant("sess", "turn", logger)
	if got == nil || len(got.Tools) != 1 {
		t.Fatalf("expected one tool record, got %+v", got)
	}
	if got.Tools[0].Status != "error" {
		t.Fatalf("Status = %q, want error", got.Tools[0].Status)
	}
}

func TestPostToolUseDropsErrorMessageOutsideFullMode(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	logger := log.New(io.Discard, "", 0)
	cfg := config.Config{ContentCapture: agento11y.ContentCaptureModeMetadataOnly}

	PostToolUse(Payload{
		HookEventName: "PostToolUse",
		SessionID:     "sess",
		TurnID:        "turn",
		ToolName:      "Bash",
		ToolUseID:     "tool-1",
		Error:         json.RawMessage(`{"message":"password=hunter2"}`),
	}, cfg, logger)

	got := fragment.LoadTolerant("sess", "turn", logger)
	if got == nil || len(got.Tools) != 1 {
		t.Fatalf("expected one tool record, got %+v", got)
	}
	if got.Tools[0].ErrorMessage != "" {
		t.Fatalf("ErrorMessage = %q, want empty outside full mode", got.Tools[0].ErrorMessage)
	}
	if got.Tools[0].Status != "error" {
		t.Fatalf("Status = %q, want error", got.Tools[0].Status)
	}
}

func TestPostToolUseRedactsErrorMessageInFullMode(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	logger := log.New(io.Discard, "", 0)
	cfg := config.Config{ContentCapture: agento11y.ContentCaptureModeFull}

	PostToolUse(Payload{
		HookEventName: "PostToolUse",
		SessionID:     "sess",
		TurnID:        "turn",
		ToolName:      "Bash",
		ToolUseID:     "tool-1",
		Error:         json.RawMessage(`{"message":"password=hunter2"}`),
	}, cfg, logger)

	got := fragment.LoadTolerant("sess", "turn", logger)
	if got == nil || len(got.Tools) != 1 {
		t.Fatalf("expected one tool record, got %+v", got)
	}
	if strings.Contains(got.Tools[0].ErrorMessage, "hunter2") {
		t.Fatalf("ErrorMessage leaked secret: %q", got.Tools[0].ErrorMessage)
	}
	if !strings.Contains(got.Tools[0].ErrorMessage, "[REDACTED:") {
		t.Fatalf("ErrorMessage missing redaction marker: %q", got.Tools[0].ErrorMessage)
	}
}

func TestStopSuccessfulExportDeletesFragmentAndUsesAuth(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT", "")
	logger := log.New(io.Discard, "", 0)
	var gotPath, gotAuth, gotUA string
	var requestCount atomic.Int64
	server := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		writeAcceptedGenerationResponseFromRequest(t, w, r)
	}))
	defer server.Close()
	t.Setenv("SIGIL_ENDPOINT", server.URL)
	t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
	t.Setenv("SIGIL_AUTH_TOKEN", "token")

	if err := fragment.Update("sess", "turn", logger, func(f *fragment.Fragment) bool {
		f.Model = "gpt-5.5"
		f.Prompt = "hello"
		return true
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	Stop(Payload{HookEventName: "Stop", SessionID: "sess", TurnID: "turn"}, config.Config{ContentCapture: agento11y.ContentCaptureModeFull}, logger)

	if got := fragment.LoadTolerant("sess", "turn", logger); got != nil {
		t.Fatalf("expected fragment deleted after successful export, got %+v", got)
	}
	if requestCount.Load() != 1 {
		t.Fatalf("request count = %d, want 1", requestCount.Load())
	}
	if gotPath != "/api/v1/generations:export" {
		t.Fatalf("path = %q", gotPath)
	}
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("tenant:token"))
	if gotAuth != wantAuth {
		t.Fatalf("Authorization = %q, want %q", gotAuth, wantAuth)
	}
	if !strings.HasPrefix(gotUA, "agento11y-plugin-codex/") {
		t.Fatalf("User-Agent = %q, want agento11y-plugin-codex/ prefix", gotUA)
	}
}

func TestStopLocalEndpointAllowsMissingCredentials(t *testing.T) {
	for _, tc := range []struct {
		name     string
		tenantID string
		token    string
	}{
		{name: "empty credentials", tenantID: "", token: ""},
		{name: "blank credentials", tenantID: "  ", token: "\t"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			envconfig.PinAliasEnvBlank(t)
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
			logger := log.New(io.Discard, "", 0)
			var gotAuth string
			var requestCount atomic.Int64
			server := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requestCount.Add(1)
				gotAuth = r.Header.Get("Authorization")
				writeAcceptedGenerationResponseFromRequest(t, w, r)
			}))
			defer server.Close()
			t.Setenv("SIGIL_ENDPOINT", server.URL)
			t.Setenv("SIGIL_AUTH_TENANT_ID", tc.tenantID)
			t.Setenv("SIGIL_AUTH_TOKEN", tc.token)

			require.NoError(t, fragment.Update("sess", "turn", logger, func(f *fragment.Fragment) bool {
				f.Model = "gpt-5.5"
				f.Prompt = "hello"
				return true
			}))

			Stop(Payload{HookEventName: "Stop", SessionID: "sess", TurnID: "turn"}, config.Config{ContentCapture: agento11y.ContentCaptureModeFull}, logger)

			assert.Equal(t, int64(1), requestCount.Load())
			wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("local:local"))
			assert.Equal(t, wantAuth, gotAuth)
			assert.Nil(t, fragment.LoadTolerant("sess", "turn", logger))
		})
	}
}

func TestStopExportsRolloutTokenUsage(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT", "")
	logger := log.New(io.Discard, "", 0)
	var body []byte
	var requestCount atomic.Int64
	server := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		var err error
		body, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		writeAcceptedGenerationResponse(t, w, body)
	}))
	defer server.Close()
	t.Setenv("SIGIL_ENDPOINT", server.URL)
	t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
	t.Setenv("SIGIL_AUTH_TOKEN", "token")

	transcript := writeHookTranscript(t,
		`{"type":"turn_context","payload":{"turn_id":"previous"}}`,
		`{"type":"response_item","payload":{"type":"reasoning"}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"cached_input_tokens":20,"output_tokens":10,"reasoning_output_tokens":3,"total_tokens":110},"last_token_usage":{"input_tokens":100,"cached_input_tokens":20,"output_tokens":10,"reasoning_output_tokens":3,"total_tokens":110},"model_context_window":258400}}}`,
		`{"type":"turn_context","payload":{"turn_id":"turn"}}`,
		`{"type":"response_item","payload":{"type":"reasoning"}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":170,"cached_input_tokens":60,"output_tokens":25,"reasoning_output_tokens":7,"total_tokens":195},"last_token_usage":{"input_tokens":70,"cached_input_tokens":40,"output_tokens":15,"reasoning_output_tokens":4,"total_tokens":85},"model_context_window":258400}}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":260,"cached_input_tokens":140,"output_tokens":40,"reasoning_output_tokens":12,"total_tokens":300},"last_token_usage":{"input_tokens":90,"cached_input_tokens":80,"output_tokens":15,"reasoning_output_tokens":5,"total_tokens":105},"model_context_window":258400}}}`,
	)
	if err := fragment.Update("sess", "turn", logger, func(f *fragment.Fragment) bool {
		f.Model = "gpt-5.5"
		f.TranscriptPath = transcript
		return true
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	Stop(Payload{HookEventName: "Stop", SessionID: "sess", TurnID: "turn"}, config.Config{ContentCapture: agento11y.ContentCaptureModeMetadataOnly}, logger)

	if requestCount.Load() != 1 {
		t.Fatalf("request count = %d, want 1", requestCount.Load())
	}
	var req struct {
		Generations []struct {
			Usage    map[string]any `json:"usage"`
			Metadata map[string]any `json:"metadata"`
		} `json:"generations"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&req); err != nil {
		t.Fatalf("decode export body %s: %v", string(body), err)
	}
	if len(req.Generations) != 1 {
		t.Fatalf("generations len = %d, want 1; body=%s", len(req.Generations), string(body))
	}
	usage := req.Generations[0].Usage
	if jsonInt64(t, usage["input_tokens"]) != 160 ||
		jsonInt64(t, usage["cache_read_input_tokens"]) != 120 ||
		jsonInt64(t, usage["output_tokens"]) != 30 ||
		jsonInt64(t, usage["reasoning_tokens"]) != 9 ||
		jsonInt64(t, usage["total_tokens"]) != 190 {
		t.Fatalf("unexpected usage: %+v", usage)
	}
	metadata := req.Generations[0].Metadata
	if jsonInt64(t, metadata["codex.token_usage.total.total_tokens"]) != 300 ||
		jsonInt64(t, metadata["codex.token_usage.context_window"]) != 258400 ||
		metadata["codex.token_usage.source"] != "turn_context_delta" {
		t.Fatalf("unexpected metadata: %+v", metadata)
	}
}

func TestStopResolvesSubagentParentGeneration(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT", "")
	logger := log.New(io.Discard, "", 0)
	var requestCount atomic.Int64
	server := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		writeAcceptedGenerationResponseFromRequest(t, w, r)
	}))
	defer server.Close()
	t.Setenv("SIGIL_ENDPOINT", server.URL)
	t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
	t.Setenv("SIGIL_AUTH_TOKEN", "token")
	cfg := config.Config{ContentCapture: agento11y.ContentCaptureModeMetadataOnly}

	parentTranscript := writeHookTranscript(t,
		`{"type":"session_meta","payload":{"id":"parent"}}`,
		`{"type":"turn_context","payload":{"turn_id":"parent-turn"}}`,
		`{"type":"response_item","payload":{"type":"function_call","name":"spawn_agent","call_id":"call_1"}}`,
		`{"type":"response_item","payload":{"type":"function_call_output","call_id":"call_1","output":"{\"agent_id\":\"child\",\"nickname\":\"Dalton\"}"}}`,
	)
	childTranscript := writeHookTranscript(t, `{"type":"session_meta","payload":{"id":"child","thread_source":"subagent","agent_role":"reviewer","source":{"subagent":{"thread_spawn":{"parent_thread_id":"parent","depth":1}}}}}`)

	SessionStart(Payload{HookEventName: "SessionStart", SessionID: "parent", TranscriptPath: parentTranscript}, cfg, logger)
	SessionStart(Payload{HookEventName: "SessionStart", SessionID: "child", TranscriptPath: childTranscript}, cfg, logger)
	UserPromptSubmit(Payload{HookEventName: "UserPromptSubmit", SessionID: "child", TurnID: "child-turn"}, cfg, logger)

	Stop(Payload{HookEventName: "Stop", SessionID: "child", TurnID: "child-turn", Timestamp: "2026-05-11T10:00:00Z"}, cfg, logger)

	if requestCount.Load() != 1 {
		t.Fatalf("request count = %d, want 1", requestCount.Load())
	}
	got := fragment.LoadSubagentLinkTolerant("child", logger)
	if got == nil {
		t.Fatal("expected subagent link retained")
	}
	if got.ParentTurnID != "parent-turn" || got.ParentGenerationID != mapper.GenerationID("parent", "parent-turn") || got.SpawnCallID != "call_1" || got.AgentNickname != "Dalton" {
		t.Fatalf("unexpected resolved link: %+v", got)
	}
	if got := fragment.LoadTolerant("child", "child-turn", logger); got != nil {
		t.Fatalf("expected exported child fragment deleted, got %+v", got)
	}
}

func TestStopMissingCredentialsDiscardsFragment(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("SIGIL_ENDPOINT", "")
	t.Setenv("SIGIL_AUTH_TENANT_ID", "")
	t.Setenv("SIGIL_AUTH_TOKEN", "")
	logger := log.New(io.Discard, "", 0)
	if err := fragment.Update("sess", "turn", logger, func(f *fragment.Fragment) bool {
		f.Model = "gpt-5.5"
		return true
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	Stop(Payload{HookEventName: "Stop", SessionID: "sess", TurnID: "turn"}, config.Config{ContentCapture: agento11y.ContentCaptureModeMetadataOnly}, logger)

	if got := fragment.LoadTolerant("sess", "turn", logger); got != nil {
		t.Fatalf("expected fragment discarded without credentials, got %+v", got)
	}
}

func TestStopExportFailureRetainsFragmentAndUsesSDKRetryDefaults(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT", "")
	logger := log.New(io.Discard, "", 0)
	var requestCount atomic.Int64
	server := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestCount.Add(1)
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer server.Close()
	t.Setenv("SIGIL_ENDPOINT", server.URL)
	t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
	t.Setenv("SIGIL_AUTH_TOKEN", "token")

	if err := fragment.Update("sess", "turn", logger, func(f *fragment.Fragment) bool {
		f.Model = "gpt-5.5"
		return true
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	start := time.Now()
	Stop(Payload{HookEventName: "Stop", SessionID: "sess", TurnID: "turn"}, config.Config{ContentCapture: agento11y.ContentCaptureModeMetadataOnly}, logger)

	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Stop took %s on immediate export failure", elapsed)
	}
	got := fragment.LoadTolerant("sess", "turn", logger)
	if got == nil {
		t.Fatal("expected fragment retained after export failure")
	}
	if !got.PendingRetry {
		t.Fatal("expected fragment marked PendingRetry after export failure")
	}
	wantAttempts := int64(agento11y.DefaultConfig().GenerationExport.MaxRetries + 1)
	if requestCount.Load() != wantAttempts {
		t.Fatalf("request count = %d, want %d attempts from SDK defaults", requestCount.Load(), wantAttempts)
	}
}

// TestStopRetriesPendingFragmentsFromPriorTurns verifies that a Stop event
// for a fresh turn drains any prior PendingRetry fragments from the same
// session — the cursor-style sweep mechanism, adapted to codex's per-turn
// model. A transient ingest hiccup at Stop time would otherwise lose that
// turn's telemetry 24h later via CleanupStale.
func TestStopRetriesPendingFragmentsFromPriorTurns(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT", "")
	logger := log.New(io.Discard, "", 0)

	var requestCount atomic.Int64
	server := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		writeAcceptedGenerationResponseFromRequest(t, w, r)
	}))
	defer server.Close()
	t.Setenv("SIGIL_ENDPOINT", server.URL)
	t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
	t.Setenv("SIGIL_AUTH_TOKEN", "token")

	// Plant a prior-turn fragment marked PendingRetry as if a previous Stop
	// had failed at flush time.
	if err := fragment.Update("sess", "old-turn", logger, func(f *fragment.Fragment) bool {
		f.Model = "gpt-5.5"
		f.PendingRetry = true
		f.CompletedAt = "2026-05-15T09:00:00Z"
		return true
	}); err != nil {
		t.Fatalf("seed prior turn: %v", err)
	}

	// New turn lands and Stop succeeds — should drain old-turn alongside.
	if err := fragment.Update("sess", "new-turn", logger, func(f *fragment.Fragment) bool {
		f.Model = "gpt-5.5"
		return true
	}); err != nil {
		t.Fatalf("seed new turn: %v", err)
	}
	Stop(Payload{HookEventName: "Stop", SessionID: "sess", TurnID: "new-turn"}, config.Config{ContentCapture: agento11y.ContentCaptureModeMetadataOnly}, logger)

	if fragment.LoadTolerant("sess", "new-turn", logger) != nil {
		t.Error("new-turn fragment should be deleted after successful Stop")
	}
	if fragment.LoadTolerant("sess", "old-turn", logger) != nil {
		t.Error("old-turn fragment should be deleted after retry sweep")
	}
	if requestCount.Load() == 0 {
		t.Fatal("expected at least one export request")
	}
}

// TestStopFlushFailurePreservesSweptRetries guards the order in which the
// retry sweep deletes fragments. Swept retry fragments must only be deleted
// after client.Flush succeeds; if Flush fails they must remain on disk with
// their PendingRetry flag set, so the next Stop sweeps them again.
func TestStopFlushFailurePreservesSweptRetries(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT", "")
	logger := log.New(io.Discard, "", 0)

	server := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer server.Close()
	t.Setenv("SIGIL_ENDPOINT", server.URL)
	t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
	t.Setenv("SIGIL_AUTH_TOKEN", "token")

	if err := fragment.Update("sess", "old-turn", logger, func(f *fragment.Fragment) bool {
		f.Model = "gpt-5.5"
		f.PendingRetry = true
		f.CompletedAt = "2026-05-15T09:00:00Z"
		return true
	}); err != nil {
		t.Fatalf("seed prior turn: %v", err)
	}
	if err := fragment.Update("sess", "new-turn", logger, func(f *fragment.Fragment) bool {
		f.Model = "gpt-5.5"
		return true
	}); err != nil {
		t.Fatalf("seed new turn: %v", err)
	}

	Stop(Payload{HookEventName: "Stop", SessionID: "sess", TurnID: "new-turn"}, config.Config{ContentCapture: agento11y.ContentCaptureModeMetadataOnly}, logger)

	old := fragment.LoadTolerant("sess", "old-turn", logger)
	if old == nil {
		t.Fatal("old-turn fragment was deleted despite Flush failure; swept retries must survive failed flush")
	}
	if !old.PendingRetry {
		t.Error("old-turn fragment lost its PendingRetry flag after failed Flush")
	}
	newT := fragment.LoadTolerant("sess", "new-turn", logger)
	if newT == nil {
		t.Fatal("new-turn fragment was deleted despite Flush failure")
	}
	if !newT.PendingRetry {
		t.Error("new-turn fragment missing PendingRetry flag after failed Flush")
	}
}

// TestStopRetrySweepPreservesSubagentLinkAndTokenUsage guards against a
// regression where sweepPendingRetries passed zero-value SubagentLink and
// TokenSnapshot to mapper.Map. That made every retried subagent turn
// silently re-export as a plain `codex` turn with no parent generation
// edge and empty token usage, even though both signals were recoverable
// from disk (subagent link file) and the fragment's TranscriptPath.
func TestStopRetrySweepPreservesSubagentLinkAndTokenUsage(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT", "")
	logger := log.New(io.Discard, "", 0)

	var bodies [][]byte
	server := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		bodies = append(bodies, body)
		writeAcceptedGenerationResponse(t, w, body)
	}))
	defer server.Close()
	t.Setenv("SIGIL_ENDPOINT", server.URL)
	t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
	t.Setenv("SIGIL_AUTH_TOKEN", "token")

	// Seed a fully-resolved subagent link on disk. We set ParentGenerationID
	// directly so resolveSubagentLinkForStop short-circuits without needing
	// a parent transcript on disk — the point of this test is the retry
	// path's mapper.Inputs, not spawn-link discovery.
	parentGenerationID := mapper.GenerationID("parent", "parent-turn")
	if err := fragment.UpdateSubagentLink("child", logger, func(link *fragment.SubagentLink) bool {
		link.ParentSessionID = "parent"
		link.ParentTurnID = "parent-turn"
		link.ParentGenerationID = parentGenerationID
		link.AgentRole = "reviewer"
		link.Source = "test"
		return true
	}); err != nil {
		t.Fatalf("seed subagent link: %v", err)
	}

	// Plant a prior-turn fragment marked PendingRetry, with a transcript
	// containing token usage for that turn. The retry sweep must pick up
	// both the transcript-derived token snapshot and the on-disk subagent
	// link when remapping it.
	transcript := writeHookTranscript(t,
		`{"type":"turn_context","payload":{"turn_id":"old-turn"}}`,
		`{"type":"response_item","payload":{"type":"reasoning"}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":17,"cached_input_tokens":6,"output_tokens":5,"reasoning_output_tokens":2,"total_tokens":22},"last_token_usage":{"input_tokens":17,"cached_input_tokens":6,"output_tokens":5,"reasoning_output_tokens":2,"total_tokens":22},"model_context_window":128000}}}`,
	)
	if err := fragment.Update("child", "old-turn", logger, func(f *fragment.Fragment) bool {
		f.Model = "gpt-5.5"
		f.TranscriptPath = transcript
		f.PendingRetry = true
		f.CompletedAt = "2026-05-15T09:00:00Z"
		return true
	}); err != nil {
		t.Fatalf("seed prior turn: %v", err)
	}
	if err := fragment.Update("child", "new-turn", logger, func(f *fragment.Fragment) bool {
		f.Model = "gpt-5.5"
		return true
	}); err != nil {
		t.Fatalf("seed new turn: %v", err)
	}

	Stop(Payload{HookEventName: "Stop", SessionID: "child", TurnID: "new-turn"}, config.Config{ContentCapture: agento11y.ContentCaptureModeMetadataOnly}, logger)

	type exportedGen struct {
		ID                  string         `json:"id"`
		ConversationID      string         `json:"conversation_id"`
		AgentName           string         `json:"agent_name"`
		ParentGenerationIDs []string       `json:"parent_generation_ids"`
		Usage               map[string]any `json:"usage"`
		Metadata            map[string]any `json:"metadata"`
	}
	oldGenID := mapper.GenerationID("child", "old-turn")
	var retried *exportedGen
	for _, body := range bodies {
		var req struct {
			Generations []exportedGen `json:"generations"`
		}
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.UseNumber()
		if err := dec.Decode(&req); err != nil {
			t.Fatalf("decode export body %s: %v", string(body), err)
		}
		for i := range req.Generations {
			if req.Generations[i].ID == oldGenID {
				g := req.Generations[i]
				retried = &g
			}
		}
	}
	if retried == nil {
		t.Fatalf("retry generation %s not exported; bodies=%q", oldGenID, bodies)
	}
	if retried.AgentName != mapper.SubagentAgentName {
		t.Errorf("retry AgentName = %q, want %q", retried.AgentName, mapper.SubagentAgentName)
	}
	if retried.ConversationID != "parent" {
		t.Errorf("retry ConversationID = %q, want %q", retried.ConversationID, "parent")
	}
	if len(retried.ParentGenerationIDs) != 1 || retried.ParentGenerationIDs[0] != parentGenerationID {
		t.Errorf("retry ParentGenerationIDs = %v, want [%s]", retried.ParentGenerationIDs, parentGenerationID)
	}
	if jsonInt64(t, retried.Usage["input_tokens"]) != 17 ||
		jsonInt64(t, retried.Usage["cache_read_input_tokens"]) != 6 ||
		jsonInt64(t, retried.Usage["output_tokens"]) != 5 ||
		jsonInt64(t, retried.Usage["reasoning_tokens"]) != 2 ||
		jsonInt64(t, retried.Usage["total_tokens"]) != 22 {
		t.Errorf("retry lost token usage: %+v", retried.Usage)
	}
	if retried.Metadata["codex.token_usage.source"] != "turn_context_delta" {
		t.Errorf("retry token-usage metadata missing or wrong: %+v", retried.Metadata)
	}
}

func TestPreToolUseGuard(t *testing.T) {
	var calls atomic.Int32
	var responseBody atomic.Value
	responseBody.Store("")
	server := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := responseBody.Load().(string)
		if body == "" {
			body = `{"action":"allow"}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	closed := newTestServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closed.Close()

	tests := []struct {
		name               string
		env                map[string]string
		useClosedEndpoint  bool
		clearCreds         bool
		serverResponds     string
		toolInput          string
		expectServerCall   bool
		wantStdoutContains []string
		wantStdoutExcludes []string
		wantStdoutEmpty    bool
		wantLogContains    string
	}{
		{
			name:            "disabled_by_default_no_env",
			wantStdoutEmpty: true,
		},
		{
			name:            "disabled_explicit_false",
			env:             map[string]string{"SIGIL_GUARDS_ENABLED": "false"},
			wantStdoutEmpty: true,
		},
		{
			name:             "enabled_allow_response",
			env:              map[string]string{"SIGIL_GUARDS_ENABLED": "true"},
			serverResponds:   `{"action":"allow"}`,
			expectServerCall: true,
			wantStdoutEmpty:  true,
		},
		{
			name:               "enabled_deny_response",
			env:                map[string]string{"SIGIL_GUARDS_ENABLED": "true"},
			serverResponds:     `{"action":"deny","reason":"blocked tool"}`,
			expectServerCall:   true,
			wantStdoutContains: []string{`"permissionDecision":"deny"`, "blocked tool", "A Grafana Agent Observability policy"},
		},
		{
			name:               "enabled_deny_empty_reason_fallback",
			env:                map[string]string{"SIGIL_GUARDS_ENABLED": "true"},
			serverResponds:     `{"action":"deny"}`,
			expectServerCall:   true,
			wantStdoutContains: []string{`"permissionDecision":"deny"`, "A Grafana Agent Observability policy blocked"},
		},
		{
			name:              "enabled_fail_open_on_transport_error",
			env:               map[string]string{"SIGIL_GUARDS_ENABLED": "true"},
			useClosedEndpoint: true,
			wantStdoutEmpty:   true,
		},
		{
			name: "enabled_fail_closed_on_transport_error",
			env: map[string]string{
				"SIGIL_GUARDS_ENABLED":   "true",
				"SIGIL_GUARDS_FAIL_OPEN": "false",
			},
			useClosedEndpoint:  true,
			wantStdoutContains: []string{`"permissionDecision":"deny"`, "could not evaluate"},
		},
		{
			name:            "enabled_fail_open_missing_credentials",
			env:             map[string]string{"SIGIL_GUARDS_ENABLED": "true"},
			clearCreds:      true,
			wantStdoutEmpty: true,
		},
		{
			name: "enabled_fail_closed_missing_credentials",
			env: map[string]string{
				"SIGIL_GUARDS_ENABLED":   "true",
				"SIGIL_GUARDS_FAIL_OPEN": "false",
			},
			clearCreds:         true,
			wantStdoutContains: []string{`"permissionDecision":"deny"`, "missing AGENTO11Y_ENDPOINT/AGENTO11Y_AUTH_TENANT_ID/AGENTO11Y_AUTH_TOKEN"},
		},
		{
			name:             "enabled_allow_transform_writes_updated_input",
			env:              map[string]string{"SIGIL_GUARDS_ENABLED": "true"},
			serverResponds:   `{"action":"allow","transformed_input":{"output":[{"role":"assistant","parts":[{"kind":"tool_call","tool_call":{"id":"tu_1","name":"Bash","input_json":{"command":"echo [REDACTED]"}}}]}]}}`,
			expectServerCall: true,
			wantStdoutContains: []string{
				`"hookSpecificOutput"`,
				`"hookEventName":"PreToolUse"`,
				`"permissionDecision":"allow"`,
				`"updatedInput":{"command":"echo [REDACTED]"}`,
			},
		},
		{
			name:             "enabled_allow_transform_for_mcp_tool_without_command",
			env:              map[string]string{"SIGIL_GUARDS_ENABLED": "true"},
			serverResponds:   `{"action":"allow","transformed_input":{"output":[{"role":"assistant","parts":[{"kind":"tool_call","tool_call":{"id":"tu_1","name":"mcp__vault__read","input_json":{"token":"REDACTED"}}}]}]}}`,
			toolInput:        `{"token":"secret"}`,
			expectServerCall: true,
			wantStdoutContains: []string{
				`"permissionDecision":"allow"`,
				`"updatedInput":{"token":"REDACTED"}`,
			},
		},
		{
			name:             "enabled_allow_transform_dropping_command_is_dropped",
			env:              map[string]string{"SIGIL_GUARDS_ENABLED": "true"},
			serverResponds:   `{"action":"allow","transformed_input":{"output":[{"role":"assistant","parts":[{"kind":"tool_call","tool_call":{"id":"tu_1","name":"Bash","input_json":{"args":"echo [REDACTED]"}}}]}]}}`,
			expectServerCall: true,
			wantStdoutEmpty:  true,
			wantLogContains:  "tool-call transform for tu_1 dropped",
		},
		{
			name:             "enabled_allow_transform_with_null_command_is_dropped",
			env:              map[string]string{"SIGIL_GUARDS_ENABLED": "true"},
			serverResponds:   `{"action":"allow","transformed_input":{"output":[{"role":"assistant","parts":[{"kind":"tool_call","tool_call":{"id":"tu_1","name":"Bash","input_json":{"command":null}}}]}]}}`,
			expectServerCall: true,
			wantStdoutEmpty:  true,
			wantLogContains:  "tool-call transform for tu_1 dropped",
		},
		{
			name:               "enabled_deny_with_transform_stays_deny_only",
			env:                map[string]string{"SIGIL_GUARDS_ENABLED": "true"},
			serverResponds:     `{"action":"deny","reason":"blocked tool","transformed_input":{"output":[{"role":"assistant","parts":[{"kind":"tool_call","tool_call":{"id":"tu_1","name":"Bash","input_json":{"command":"echo [REDACTED]"}}}]}]}}`,
			expectServerCall:   true,
			wantStdoutContains: []string{`"permissionDecision":"deny"`, "blocked tool"},
			wantStdoutExcludes: []string{"updatedInput"},
		},
		{
			name:             "enabled_allow_unusable_transform_stays_silent",
			env:              map[string]string{"SIGIL_GUARDS_ENABLED": "true"},
			serverResponds:   `{"action":"allow","transformed_input":{"output":[{"role":"assistant","parts":[{"kind":"tool_call","tool_call":{"id":"tu_other","name":"Bash","input_json":{"command":"echo X"}}}]}]}}`,
			expectServerCall: true,
			wantStdoutEmpty:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("SIGIL_GUARDS_ENABLED", "")
			t.Setenv("SIGIL_GUARDS_FAIL_OPEN", "")
			t.Setenv("SIGIL_GUARDS_TIMEOUT_MS", "")
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			endpoint := server.URL
			if tt.useClosedEndpoint {
				endpoint = closed.URL
			}
			if tt.clearCreds {
				t.Setenv("SIGIL_ENDPOINT", "")
				t.Setenv("SIGIL_AUTH_TENANT_ID", "")
				t.Setenv("SIGIL_AUTH_TOKEN", "")
			} else {
				t.Setenv("SIGIL_ENDPOINT", endpoint)
				t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
				t.Setenv("SIGIL_AUTH_TOKEN", "token")
			}

			calls.Store(0)
			responseBody.Store(tt.serverResponds)

			cfg := config.Load(log.New(io.Discard, "", 0))
			var stdout bytes.Buffer
			var logs bytes.Buffer
			toolInput := tt.toolInput
			if toolInput == "" {
				toolInput = `{"command":"echo hi"}`
			}
			payload := Payload{
				HookEventName: "PreToolUse",
				SessionID:     "sess",
				ToolName:      "Bash",
				ToolUseID:     "tu_1",
				ToolInput:     json.RawMessage(toolInput),
				Model:         "gpt-5",
			}

			PreToolUse(context.Background(), &stdout, payload, cfg, log.New(&logs, "", 0))

			if tt.expectServerCall && calls.Load() == 0 {
				t.Errorf("expected server call, got 0")
			}
			if !tt.expectServerCall && !tt.useClosedEndpoint && !tt.clearCreds && calls.Load() != 0 {
				t.Errorf("expected no server call, got %d", calls.Load())
			}
			if tt.wantStdoutEmpty && stdout.Len() != 0 {
				t.Errorf("stdout not empty: %q", stdout.String())
			}
			for _, want := range tt.wantStdoutContains {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("stdout = %q, want substring %q", stdout.String(), want)
				}
			}
			for _, exclude := range tt.wantStdoutExcludes {
				if strings.Contains(stdout.String(), exclude) {
					t.Errorf("stdout = %q, must not contain %q", stdout.String(), exclude)
				}
			}
			if tt.wantLogContains != "" && !strings.Contains(logs.String(), tt.wantLogContains) {
				t.Errorf("logs missing %q:\n%s", tt.wantLogContains, logs.String())
			}
		})
	}
}

func TestPreToolUseGuardSendsExpectedRequest(t *testing.T) {
	var capturedPath atomic.Value
	var capturedBody atomic.Value
	server := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath.Store(r.URL.Path)
		body, _ := io.ReadAll(r.Body)
		capturedBody.Store(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"action":"allow"}`))
	}))
	defer server.Close()

	t.Setenv("SIGIL_GUARDS_ENABLED", "true")
	t.Setenv("SIGIL_GUARDS_FAIL_OPEN", "")
	t.Setenv("SIGIL_GUARDS_TIMEOUT_MS", "")
	t.Setenv("SIGIL_ENDPOINT", server.URL)
	t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
	t.Setenv("SIGIL_AUTH_TOKEN", "token")

	cfg := config.Load(log.New(io.Discard, "", 0))
	var stdout bytes.Buffer
	payload := Payload{
		HookEventName: "PreToolUse",
		SessionID:     "sess",
		ToolName:      "Bash",
		ToolUseID:     "tu_1",
		ToolInput:     json.RawMessage(`{"command":"rm -rf /"}`),
		Model:         "gpt-5",
	}
	PreToolUse(context.Background(), &stdout, payload, cfg, log.New(io.Discard, "", 0))

	path, _ := capturedPath.Load().(string)
	if path != "/api/v1/hooks:evaluate" {
		t.Errorf("path = %q, want /api/v1/hooks:evaluate", path)
	}
	rawBody, _ := capturedBody.Load().([]byte)
	if len(rawBody) == 0 {
		t.Fatal("captured body is empty")
	}
	var req struct {
		Phase   string `json:"phase"`
		Context struct {
			AgentName string `json:"agent_name"`
		} `json:"context"`
		Input struct {
			Output []struct {
				Role  string `json:"role"`
				Parts []struct {
					Kind     string `json:"kind"`
					ToolCall *struct {
						ID    string          `json:"id"`
						Name  string          `json:"name"`
						Input json.RawMessage `json:"input_json"`
					} `json:"tool_call"`
				} `json:"parts"`
			} `json:"output"`
		} `json:"input"`
	}
	if err := json.Unmarshal(rawBody, &req); err != nil {
		t.Fatalf("decode request body: %v\nbody: %s", err, string(rawBody))
	}
	if req.Phase != "postflight" {
		t.Errorf("phase = %q, want postflight", req.Phase)
	}
	if req.Context.AgentName != mapper.AgentName {
		t.Errorf("agent_name = %q, want %q", req.Context.AgentName, mapper.AgentName)
	}
	if len(req.Input.Output) != 1 || len(req.Input.Output[0].Parts) != 1 {
		t.Fatalf("unexpected output shape: %+v", req.Input.Output)
	}
	tc := req.Input.Output[0].Parts[0].ToolCall
	if tc == nil {
		t.Fatal("missing tool_call part")
	}
	if tc.Name != "Bash" || tc.ID != "tu_1" {
		t.Errorf("tool_call name/id = %q/%q, want Bash/tu_1", tc.Name, tc.ID)
	}
	if !strings.Contains(string(tc.Input), "rm -rf /") {
		t.Errorf("tool_call input = %s, want substring %q", string(tc.Input), "rm -rf /")
	}
}

func newTestServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("loopback listen unavailable in this sandbox: %v", err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.Listener = listener
	server.Start()
	return server
}

func writeHookTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	return path
}

func writeAcceptedGenerationResponseFromRequest(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Errorf("read body: %v", err)
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	writeAcceptedGenerationResponse(t, w, body)
}

func writeAcceptedGenerationResponse(t *testing.T, w http.ResponseWriter, body []byte) {
	t.Helper()
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		t.Errorf("decode export request: %v", err)
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	generations, _ := request["generations"].([]any)
	results := make([]map[string]any, 0, len(generations))
	for _, raw := range generations {
		generation, _ := raw.(map[string]any)
		id, _ := generation["id"].(string)
		results = append(results, map[string]any{
			"generation_id": id,
			"accepted":      true,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"results": results}); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

func jsonInt64(t *testing.T, v any) int64 {
	t.Helper()
	switch value := v.(type) {
	case json.Number:
		n, err := value.Int64()
		if err != nil {
			t.Fatalf("parse json number %q: %v", value, err)
		}
		return n
	case string:
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			t.Fatalf("parse json string %q: %v", value, err)
		}
		return n
	case float64:
		return int64(value)
	default:
		t.Fatalf("expected numeric JSON value, got %T (%#v)", v, v)
	}
	return 0
}
