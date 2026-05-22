package hook

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grafana/sigil-sdk/go/sigil"

	"github.com/grafana/sigil-sdk/plugins/sigil/internal/agents/copilot/config"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/agents/copilot/fragment"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/agents/copilot/transcript"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/envconfig"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHookSequenceExportsOnStop(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("SIGIL_CONTENT_CAPTURE_MODE", "full")
	logger := log.New(io.Discard, "", 0)
	var gotPath, gotAuth, gotBody string
	var requestCount atomic.Int64
	server := newAcceptedGenerationServer(t, func(body string) {
		requestCount.Add(1)
		gotBody = body
	})
	defer server.Close()
	gotPath = "/api/v1/generations:export"
	gotAuth = "Basic " + base64.StdEncoding.EncodeToString([]byte("tenant:token"))
	t.Setenv("SIGIL_ENDPOINT", server.URL)
	t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
	t.Setenv("SIGIL_AUTH_TOKEN", "token")
	cfg := config.Config{ContentCapture: sigil.ContentCaptureModeFull}

	SessionStart(Payload{HookEventNameJSON: "SessionStart", SessionIDJSON: "sess", Timestamp: []byte(`"2026-05-18T12:00:00Z"`), SourceValue: "new"}, cfg, logger)
	UserPromptSubmit(Payload{HookEventNameJSON: "UserPromptSubmit", SessionIDJSON: "sess", Timestamp: []byte(`"2026-05-18T12:00:01Z"`), Prompt: "hello glc_abcdefghijklmnopqrstuvwxyz"}, cfg, logger)
	PreToolUse(context.Background(), io.Discard, Payload{HookEventNameJSON: "PreToolUse", SessionIDJSON: "sess", Timestamp: []byte(`"2026-05-18T12:00:02Z"`), ToolNameJSON: "bash", ToolInputJSON: []byte(`{"cmd":"echo hi"}`)}, cfg, logger)
	PostToolUse(Payload{HookEventNameJSON: "PostToolUse", SessionIDJSON: "sess", Timestamp: []byte(`"2026-05-18T12:00:03Z"`), ToolNameJSON: "bash", ToolResultJSON: []byte(`{"text_result_for_llm":"ok"}`)}, cfg, logger, false)
	Stop(Payload{HookEventNameJSON: "Stop", SessionIDJSON: "sess", Timestamp: []byte(`"2026-05-18T12:00:04Z"`), StopReasonJSON: "end_turn"}, cfg, logger)

	if requestCount.Load() == 0 {
		t.Fatal("expected export request")
	}
	if gotPath != "/api/v1/generations:export" {
		t.Fatalf("path = %q, want /api/v1/generations:export", gotPath)
	}
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("tenant:token"))
	if gotAuth != wantAuth {
		t.Fatalf("auth = %q, want %q", gotAuth, wantAuth)
	}
	if strings.Contains(gotBody, "glc_abcdefghijklmnopqrstuvwxyz") {
		t.Fatalf("export leaked unredacted secret: %s", gotBody)
	}
	if got := fragment.LoadSessionTolerant("sess", logger); got == nil || got.ActiveTurnID != "" {
		t.Fatalf("expected cleared active turn, got %+v", got)
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
			t.Setenv("SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT", "")
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
			logger := log.New(io.Discard, "", 0)
			var gotAuth string
			var requestCount atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requestCount.Add(1)
				gotAuth = r.Header.Get("Authorization")
				body, err := io.ReadAll(r.Body)
				if err != nil {
					http.Error(w, "read body", http.StatusBadRequest)
					return
				}
				var request map[string]any
				if err := json.Unmarshal(body, &request); err != nil {
					http.Error(w, "invalid json", http.StatusBadRequest)
					return
				}
				generations, _ := request["generations"].([]any)
				results := make([]map[string]any, 0, len(generations))
				for _, raw := range generations {
					generation, _ := raw.(map[string]any)
					id, _ := generation["id"].(string)
					results = append(results, map[string]any{"generation_id": id, "accepted": true})
				}
				w.Header().Set("Content-Type", "application/json")
				require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"results": results}))
			}))
			defer server.Close()
			t.Setenv("SIGIL_ENDPOINT", server.URL)
			t.Setenv("SIGIL_AUTH_TENANT_ID", tc.tenantID)
			t.Setenv("SIGIL_AUTH_TOKEN", tc.token)
			cfg := config.Config{ContentCapture: sigil.ContentCaptureModeFull}

			UserPromptSubmit(Payload{HookEventNameJSON: "UserPromptSubmit", SessionIDJSON: "sess", Timestamp: []byte(`"2026-05-18T12:00:01Z"`), Prompt: "hello"}, cfg, logger)
			Stop(Payload{HookEventNameJSON: "Stop", SessionIDJSON: "sess", Timestamp: []byte(`"2026-05-18T12:00:04Z"`), StopReasonJSON: "end_turn"}, cfg, logger)

			assert.Equal(t, int64(1), requestCount.Load())
			wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("local:local"))
			assert.Equal(t, wantAuth, gotAuth)
			assert.Nil(t, fragment.LoadTolerant("sess", "turn-000001", logger))
		})
	}
}

func TestSessionEndRemovesStrandedTurnFiles(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	logger := log.New(io.Discard, "", 0)
	if _, _, err := fragment.StartNextTurn("sess", logger, "2026-05-18T12:00:00Z"); err != nil {
		t.Fatalf("StartNextTurn: %v", err)
	}
	if err := fragment.Update("sess", "turn-000001", logger, func(f *fragment.Fragment) bool {
		f.Prompt = "hello"
		return true
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := fragment.ClearActiveTurn("sess", "turn-000001", logger); err != nil {
		t.Fatalf("ClearActiveTurn: %v", err)
	}
	SessionEnd(Payload{HookEventNameJSON: "SessionEnd", SessionIDJSON: "sess"}, logger)
	if got := fragment.LoadTolerant("sess", "turn-000001", logger); got != nil {
		t.Fatalf("expected fragment deleted, got %+v", got)
	}
	if got := fragment.LoadSessionTolerant("sess", logger); got != nil {
		t.Fatalf("expected session deleted, got %+v", got)
	}
}

func TestSessionEndKeepsActiveTurnUntilStop(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	logger := log.New(io.Discard, "", 0)
	if _, _, err := fragment.StartNextTurn("sess", logger, "2026-05-18T12:00:00Z"); err != nil {
		t.Fatalf("StartNextTurn: %v", err)
	}
	if err := fragment.Update("sess", "turn-000001", logger, func(f *fragment.Fragment) bool {
		f.Prompt = "hello"
		return true
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	SessionEnd(Payload{HookEventNameJSON: "SessionEnd", SessionIDJSON: "sess"}, logger)
	if got := fragment.LoadTolerant("sess", "turn-000001", logger); got == nil {
		t.Fatal("expected active fragment to remain")
	}
	if got := fragment.LoadSessionTolerant("sess", logger); got == nil || got.ActiveTurnID != "turn-000001" {
		t.Fatalf("expected active session to remain, got %+v", got)
	}
}

func TestErrorOccurredStoresMetadataOnlyOutsideFullMode(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	logger := log.New(io.Discard, "", 0)
	cfg := config.Config{ContentCapture: sigil.ContentCaptureModeMetadataOnly}
	UserPromptSubmit(Payload{HookEventNameJSON: "UserPromptSubmit", SessionIDJSON: "sess", Timestamp: []byte(`"2026-05-18T12:00:01Z"`), Prompt: "hello"}, cfg, logger)
	ErrorOccurred(Payload{
		HookEventNameJSON: "ErrorOccurred",
		SessionIDJSON:     "sess",
		Timestamp:         []byte(`"2026-05-18T12:00:02Z"`),
		ErrorContextJSON:  "model_call",
		ErrorJSON:         []byte(`{"name":"Boom","message":"Bearer secret"}`),
	}, cfg, logger)
	frag := fragment.LoadTolerant("sess", "turn-000001", logger)
	if frag == nil || len(frag.Errors) != 1 {
		t.Fatalf("expected stored error, got %+v", frag)
	}
	if frag.Errors[0].Message != "" {
		t.Fatalf("error message should be empty outside full mode, got %q", frag.Errors[0].Message)
	}
}

func TestStopEnrichesExportFromTranscript(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("SIGIL_CONTENT_CAPTURE_MODE", "full")
	logger := log.New(io.Discard, "", 0)

	transcriptPath := filepath.Join(t.TempDir(), "events.jsonl")
	transcript := "" +
		"{\"type\":\"session.start\",\"data\":{\"sessionId\":\"sess\",\"copilotVersion\":\"1.0.48\"}}\n" +
		"{\"type\":\"session.model_change\",\"data\":{\"newModel\":\"gpt-5.4\",\"reasoningEffort\":\"medium\"}}\n" +
		"{\"type\":\"user.message\",\"data\":{\"content\":\"hello\",\"interactionId\":\"int-1\"}}\n" +
		"{\"type\":\"assistant.message\",\"data\":{\"messageId\":\"msg-1\",\"model\":\"gpt-5.4\",\"content\":\"assistant answer\",\"interactionId\":\"int-1\",\"turnId\":\"4\",\"outputTokens\":12,\"requestId\":\"req-1\"}}\n"
	if err := os.WriteFile(transcriptPath, []byte(transcript), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	var gotBody string
	server := newAcceptedGenerationServer(t, func(body string) {
		gotBody = body
	})
	defer server.Close()

	t.Setenv("SIGIL_ENDPOINT", server.URL)
	t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
	t.Setenv("SIGIL_AUTH_TOKEN", "token")
	cfg := config.Config{ContentCapture: sigil.ContentCaptureModeFull}

	UserPromptSubmit(Payload{HookEventNameJSON: "UserPromptSubmit", SessionIDJSON: "sess", Timestamp: []byte(`"2026-05-18T12:00:01Z"`), Prompt: "hello"}, cfg, logger)
	Stop(Payload{HookEventNameJSON: "Stop", SessionIDJSON: "sess", Timestamp: []byte(`"2026-05-18T12:00:04Z"`), StopReasonJSON: "end_turn", TranscriptPathJSON: transcriptPath}, cfg, logger)

	for _, want := range []string{
		`"agent_version":"1.0.48"`,
		`"output_tokens":"12"`,
		`"assistant answer"`,
		`"copilot.native_turn_id":"4"`,
		`"copilot.request_id":"req-1"`,
		`"copilot.message_id":"msg-1"`,
	} {
		if !strings.Contains(gotBody, want) {
			t.Fatalf("export body missing %q: %s", want, gotBody)
		}
	}
}

func TestStopRetainsActiveTurnWhenExportFlushFails(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("SIGIL_CONTENT_CAPTURE_MODE", "full")
	logger := log.New(io.Discard, "", 0)

	t.Setenv("SIGIL_ENDPOINT", "://bad-endpoint")
	t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
	t.Setenv("SIGIL_AUTH_TOKEN", "token")
	cfg := config.Config{ContentCapture: sigil.ContentCaptureModeFull}

	UserPromptSubmit(Payload{HookEventNameJSON: "UserPromptSubmit", SessionIDJSON: "sess", Timestamp: []byte(`"2026-05-18T12:00:01Z"`), Prompt: "hello"}, cfg, logger)
	Stop(Payload{HookEventNameJSON: "Stop", SessionIDJSON: "sess", Timestamp: []byte(`"2026-05-18T12:00:04Z"`), StopReasonJSON: "end_turn"}, cfg, logger)

	session := fragment.LoadSessionTolerant("sess", logger)
	if session == nil || session.ActiveTurnID != "turn-000001" {
		t.Fatalf("expected active turn retained after export failure, got %+v", session)
	}
	if got := fragment.LoadTolerant("sess", "turn-000001", logger); got == nil {
		t.Fatal("expected fragment retained after export failure")
	}
}

func TestStopClearsActiveTurnWhenFragmentLoadFails(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("SIGIL_CONTENT_CAPTURE_MODE", "full")
	// Exercise the no-credentials path explicitly; otherwise the test inherits
	// SIGIL_* vars from the developer's shell and hits a real Sigil instance.
	t.Setenv("SIGIL_ENDPOINT", "")
	t.Setenv("SIGIL_AUTH_TENANT_ID", "")
	t.Setenv("SIGIL_AUTH_TOKEN", "")
	logger := log.New(io.Discard, "", 0)
	cfg := config.Config{ContentCapture: sigil.ContentCaptureModeFull}

	UserPromptSubmit(Payload{HookEventNameJSON: "UserPromptSubmit", SessionIDJSON: "sess", Timestamp: []byte(`"2026-05-18T12:00:01Z"`), Prompt: "hello"}, cfg, logger)
	path := fragment.FragmentFilePath("sess", "turn-000001")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatalf("corrupt fragment: %v", err)
	}

	Stop(Payload{HookEventNameJSON: "Stop", SessionIDJSON: "sess", Timestamp: []byte(`"2026-05-18T12:00:04Z"`), StopReasonJSON: "end_turn"}, cfg, logger)

	session := fragment.LoadSessionTolerant("sess", logger)
	if session == nil {
		t.Fatal("expected session to remain")
	}
	if session.ActiveTurnID != "" {
		t.Fatalf("expected active turn cleared after fragment load failure, got %+v", session)
	}
}

func TestStopClearsActiveTurnWhenDeleteFailsAfterSuccessfulExport(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("SIGIL_CONTENT_CAPTURE_MODE", "full")
	logger := log.New(io.Discard, "", 0)

	server := newAcceptedGenerationServer(t)
	defer server.Close()
	t.Setenv("SIGIL_ENDPOINT", server.URL)
	t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
	t.Setenv("SIGIL_AUTH_TOKEN", "token")
	cfg := config.Config{ContentCapture: sigil.ContentCaptureModeFull}

	origDelete := deleteFragment
	deleteFragment = func(sessionID, turnID string) error {
		return errors.New("delete failed")
	}
	defer func() { deleteFragment = origDelete }()

	UserPromptSubmit(Payload{HookEventNameJSON: "UserPromptSubmit", SessionIDJSON: "sess", Timestamp: []byte(`"2026-05-18T12:00:01Z"`), Prompt: "hello"}, cfg, logger)
	Stop(Payload{HookEventNameJSON: "Stop", SessionIDJSON: "sess", Timestamp: []byte(`"2026-05-18T12:00:04Z"`), StopReasonJSON: "end_turn"}, cfg, logger)

	session := fragment.LoadSessionTolerant("sess", logger)
	if session == nil {
		t.Fatal("expected session to remain")
	}
	if session.ActiveTurnID != "" {
		t.Fatalf("expected active turn cleared after delete failure, got %+v", session)
	}
	if got := fragment.LoadTolerant("sess", "turn-000001", logger); got == nil {
		t.Fatal("expected fragment to remain when delete fails")
	}
}

func TestStopUsesPromptHashForMetadataOnlyTranscriptEnrichment(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("SIGIL_CONTENT_CAPTURE_MODE", "metadata_only")
	logger := log.New(io.Discard, "", 0)

	transcriptPath := filepath.Join(t.TempDir(), "events.jsonl")
	transcript := "" +
		"{\"type\":\"session.start\",\"data\":{\"sessionId\":\"sess\",\"copilotVersion\":\"1.0.49\"}}\n" +
		"{\"type\":\"user.message\",\"data\":{\"content\":\"first prompt\",\"interactionId\":\"int-1\"}}\n" +
		"{\"type\":\"assistant.message\",\"data\":{\"messageId\":\"msg-1\",\"model\":\"claude-sonnet-4.6\",\"content\":\"first answer\",\"interactionId\":\"int-1\",\"turnId\":\"0\",\"outputTokens\":621,\"requestId\":\"req-1\"}}\n" +
		"{\"type\":\"user.message\",\"data\":{\"content\":\"second prompt\",\"interactionId\":\"int-2\"}}\n" +
		"{\"type\":\"assistant.message\",\"data\":{\"messageId\":\"msg-2\",\"model\":\"gpt-4.1\",\"content\":\"second answer\",\"interactionId\":\"int-2\",\"turnId\":\"0\",\"outputTokens\":123,\"requestId\":\"req-2\"}}\n"
	if err := os.WriteFile(transcriptPath, []byte(transcript), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	var gotBody string
	server := newAcceptedGenerationServer(t, func(body string) {
		gotBody = body
	})
	defer server.Close()

	t.Setenv("SIGIL_ENDPOINT", server.URL)
	t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
	t.Setenv("SIGIL_AUTH_TOKEN", "token")
	cfg := config.Config{ContentCapture: sigil.ContentCaptureModeMetadataOnly}

	UserPromptSubmit(Payload{HookEventNameJSON: "UserPromptSubmit", SessionIDJSON: "sess", Timestamp: []byte(`"2026-05-18T12:00:01Z"`), Prompt: "second prompt"}, cfg, logger)
	frag := fragment.LoadTolerant("sess", "turn-000001", logger)
	if frag == nil {
		t.Fatal("expected fragment")
	}
	if frag.Prompt != "" {
		t.Fatalf("metadata_only persisted raw prompt: %q", frag.Prompt)
	}
	if frag.PromptHash == "" || frag.PromptHash == "second prompt" {
		t.Fatalf("metadata_only prompt hash = %q", frag.PromptHash)
	}

	Stop(Payload{HookEventNameJSON: "Stop", SessionIDJSON: "sess", Timestamp: []byte(`"2026-05-18T12:00:04Z"`), StopReasonJSON: "end_turn", TranscriptPathJSON: transcriptPath}, cfg, logger)

	for _, want := range []string{
		`"name":"gpt-4.1"`,
		`"response_id":"req-2"`,
		`"response_model":"gpt-4.1"`,
		`"output_tokens":"123"`,
		`"copilot.request_id":"req-2"`,
	} {
		if !strings.Contains(gotBody, want) {
			t.Fatalf("export body missing %q: %s", want, gotBody)
		}
	}
	for _, leaked := range []string{"second prompt", `"response_model":"claude-sonnet-4.6"`, `"output_tokens":"621"`} {
		if strings.Contains(gotBody, leaked) {
			t.Fatalf("export body contained %q: %s", leaked, gotBody)
		}
	}
}

func newAcceptedGenerationServer(t *testing.T, capture ...func(string)) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		if len(capture) > 0 && capture[0] != nil {
			capture[0](string(body))
		}

		var request map[string]any
		if err := json.Unmarshal(body, &request); err != nil {
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
			http.Error(w, "encode response", http.StatusInternalServerError)
		}
	}))
}

func TestStopWaitsForCurrentCLITranscriptTurnInsteadOfReusingPreviousTurn(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("SIGIL_CONTENT_CAPTURE_MODE", "full")
	logger := log.New(io.Discard, "", 0)

	transcriptPath := filepath.Join(t.TempDir(), "events.jsonl")
	initialTranscript := "" +
		"{\"type\":\"session.start\",\"data\":{\"sessionId\":\"sess\",\"copilotVersion\":\"1.0.49\"}}\n" +
		"{\"type\":\"session.model_change\",\"data\":{\"newModel\":\"auto\"}}\n" +
		"{\"type\":\"user.message\",\"data\":{\"content\":\"first prompt\",\"interactionId\":\"int-1\"}}\n" +
		"{\"type\":\"assistant.message\",\"data\":{\"messageId\":\"msg-1\",\"model\":\"claude-sonnet-4.6\",\"content\":\"first answer\",\"interactionId\":\"int-1\",\"turnId\":\"0\",\"outputTokens\":621,\"requestId\":\"req-1\"}}\n" +
		"{\"type\":\"user.message\",\"data\":{\"content\":\"second prompt\",\"interactionId\":\"int-2\"}}\n"
	if err := os.WriteFile(transcriptPath, []byte(initialTranscript), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	go func() {
		time.Sleep(250 * time.Millisecond)
		f, err := os.OpenFile(transcriptPath, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			return
		}
		defer func() { _ = f.Close() }()
		_, _ = f.WriteString("{\"type\":\"assistant.message\",\"data\":{\"messageId\":\"msg-2\",\"model\":\"gpt-4.1\",\"content\":\"second answer\",\"interactionId\":\"int-2\",\"turnId\":\"0\",\"outputTokens\":123,\"requestId\":\"req-2\"}}\n")
	}()

	var gotBody string
	server := newAcceptedGenerationServer(t, func(body string) {
		gotBody = body
	})
	defer server.Close()

	t.Setenv("SIGIL_ENDPOINT", server.URL)
	t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
	t.Setenv("SIGIL_AUTH_TOKEN", "token")
	cfg := config.Config{ContentCapture: sigil.ContentCaptureModeFull}

	UserPromptSubmit(Payload{HookEventNameJSON: "UserPromptSubmit", SessionIDJSON: "sess", Timestamp: []byte(`"2026-05-18T12:00:01Z"`), Prompt: "second prompt"}, cfg, logger)
	Stop(Payload{HookEventNameJSON: "Stop", SessionIDJSON: "sess", Timestamp: []byte(`"2026-05-18T12:00:04Z"`), StopReasonJSON: "end_turn", TranscriptPathJSON: transcriptPath}, cfg, logger)

	for _, want := range []string{
		`"name":"gpt-4.1"`,
		`"response_model":"gpt-4.1"`,
		`"output_tokens":"123"`,
		`"copilot.request_id":"req-2"`,
	} {
		if !strings.Contains(gotBody, want) {
			t.Fatalf("export body missing %q: %s", want, gotBody)
		}
	}
	if strings.Contains(gotBody, `"response_model":"claude-sonnet-4.6"`) {
		t.Fatalf("export body reused previous turn model: %s", gotBody)
	}
	if strings.Contains(gotBody, `"output_tokens":"621"`) {
		t.Fatalf("export body reused previous turn output tokens: %s", gotBody)
	}
}

func TestPreToolUseGuardBehavior(t *testing.T) {
	tests := []struct {
		name string
		// guards is the GuardsConfig the test passes through cfg.Guards.
		guards envconfig.GuardsConfig
		// hookResponse is the JSON the fake Sigil hook server returns.
		// If empty, an allow is returned.
		hookResponse string
		// useClosedServer points cfg.Endpoint at a closed listener so
		// the request fails at transport.
		useClosedServer bool
		// clearCreds blanks SIGIL_ENDPOINT/SIGIL_AUTH_TENANT_ID/SIGIL_AUTH_TOKEN.
		clearCreds          bool
		wantServerCalled    bool
		wantStdoutContains  string
		wantStdoutEmpty     bool
		wantToolRecordCount int
	}{
		{
			name:                "disabled stays stdout-empty and records pending tool",
			guards:              envconfig.GuardsConfig{Enabled: false, TimeoutMs: 1500, FailOpen: true},
			wantStdoutEmpty:     true,
			wantToolRecordCount: 1,
		},
		{
			name:                "allow stays stdout-empty and records pending tool",
			guards:              envconfig.GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: true},
			hookResponse:        `{"action":"allow"}`,
			wantServerCalled:    true,
			wantStdoutEmpty:     true,
			wantToolRecordCount: 1,
		},
		{
			name:                "deny writes copilot deny JSON and skips record",
			guards:              envconfig.GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: true},
			hookResponse:        `{"action":"deny","reason":"blocked tool"}`,
			wantServerCalled:    true,
			wantStdoutContains:  `"permissionDecision":"deny"`,
			wantToolRecordCount: 0,
		},
		{
			name:                "fail-open transport error allows tool and records it",
			guards:              envconfig.GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: true},
			useClosedServer:     true,
			wantStdoutEmpty:     true,
			wantToolRecordCount: 1,
		},
		{
			name:                "fail-closed transport error denies tool and skips record",
			guards:              envconfig.GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: false},
			useClosedServer:     true,
			wantStdoutContains:  `"permissionDecision":"deny"`,
			wantToolRecordCount: 0,
		},
		{
			name:                "fail-open missing credentials allows tool and records it",
			guards:              envconfig.GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: true},
			clearCreds:          true,
			wantStdoutEmpty:     true,
			wantToolRecordCount: 1,
		},
		{
			name:                "fail-closed missing credentials denies tool and skips record",
			guards:              envconfig.GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: false},
			clearCreds:          true,
			wantStdoutContains:  `"permissionDecision":"deny"`,
			wantToolRecordCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			logger := log.New(io.Discard, "", 0)

			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				body := tt.hookResponse
				if body == "" {
					body = `{"action":"allow"}`
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(body))
			}))
			defer server.Close()

			closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			closed.Close()

			endpoint := server.URL
			if tt.useClosedServer {
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

			cfg := config.Config{
				ContentCapture: sigil.ContentCaptureModeFull,
				Guards:         tt.guards,
			}
			UserPromptSubmit(Payload{HookEventNameJSON: "UserPromptSubmit", SessionIDJSON: "sess", Timestamp: []byte(`"2026-05-18T12:00:01Z"`), Prompt: "hello"}, cfg, logger)

			var stdout bytes.Buffer
			PreToolUse(context.Background(), &stdout, Payload{
				HookEventNameJSON: "PreToolUse",
				SessionIDJSON:     "sess",
				Timestamp:         []byte(`"2026-05-18T12:00:02Z"`),
				ToolNameJSON:      "bash",
				ToolInputJSON:     []byte(`{"cmd":"echo hi"}`),
			}, cfg, logger)

			if tt.wantServerCalled && calls.Load() == 0 {
				t.Errorf("expected sigil hook server call, got 0")
			}
			if !tt.wantServerCalled && !tt.useClosedServer && calls.Load() != 0 {
				t.Errorf("expected no sigil hook server call, got %d", calls.Load())
			}
			if tt.wantStdoutEmpty && stdout.Len() != 0 {
				t.Errorf("stdout not empty: %q", stdout.String())
			}
			if tt.wantStdoutContains != "" && !strings.Contains(stdout.String(), tt.wantStdoutContains) {
				t.Errorf("stdout = %q, want substring %q", stdout.String(), tt.wantStdoutContains)
			}

			frag := fragment.LoadTolerant("sess", "turn-000001", logger)
			if frag == nil {
				t.Fatalf("expected fragment to exist after UserPromptSubmit")
			}
			if len(frag.Tools) != tt.wantToolRecordCount {
				t.Fatalf("len(frag.Tools) = %d, want %d", len(frag.Tools), tt.wantToolRecordCount)
			}
		})
	}
}

func TestShouldPreferTranscriptSnapshot(t *testing.T) {
	twelve := int64(12)
	oneThousand := int64(1334)

	tests := []struct {
		name    string
		current transcript.Snapshot
		have    bool
		next    transcript.Snapshot
		want    bool
	}{
		{
			name: "first snapshot wins when nothing cached",
			have: false,
			next: transcript.Snapshot{MessageID: "msg-1"},
			want: true,
		},
		{
			name: "prefer snapshot with assistant text over empty content",
			have: true,
			current: transcript.Snapshot{
				MessageID:     "msg-1",
				AssistantText: "",
				OutputTokens:  &twelve,
				NativeTurnID:  "3",
				InteractionID: "int-1",
				RequestID:     "req-1",
			},
			next: transcript.Snapshot{
				MessageID:     "msg-2",
				AssistantText: "final answer",
				OutputTokens:  &oneThousand,
				NativeTurnID:  "4",
				InteractionID: "int-1",
				RequestID:     "req-2",
			},
			want: true,
		},
		{
			name: "prefer higher token count when both snapshots lack text",
			have: true,
			current: transcript.Snapshot{
				MessageID:    "msg-1",
				OutputTokens: &twelve,
				NativeTurnID: "3",
			},
			next: transcript.Snapshot{
				MessageID:    "msg-2",
				OutputTokens: &oneThousand,
				NativeTurnID: "4",
			},
			want: true,
		},
		{
			name: "keep current snapshot when next is not better",
			have: true,
			current: transcript.Snapshot{
				MessageID:     "msg-2",
				AssistantText: "final answer",
				OutputTokens:  &oneThousand,
				NativeTurnID:  "4",
			},
			next: transcript.Snapshot{
				MessageID:     "msg-1",
				AssistantText: "",
				OutputTokens:  &twelve,
				NativeTurnID:  "3",
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldPreferTranscriptSnapshot(tt.current, tt.have, tt.next)
			if got != tt.want {
				t.Fatalf("shouldPreferTranscriptSnapshot() = %t, want %t", got, tt.want)
			}
		})
	}
}
