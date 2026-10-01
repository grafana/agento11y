package kiro

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/execpath"
	"github.com/grafana/agento11y/plugins/agento11y/internal/fragmentstore"
)

func TestInstallLifecycle(t *testing.T) {
	t.Chdir(t.TempDir())
	old := execpath.Executable
	execpath.Executable = func() (string, error) { return "/path with spaces/agento11y", nil }
	t.Cleanup(func() { execpath.Executable = old })
	if changed, err := Install(); err != nil || !changed {
		t.Fatalf("first install: %v %v", changed, err)
	}
	if changed, err := Install(); err != nil || changed {
		t.Fatalf("second install: %v %v", changed, err)
	}
	if ok, _, err := Status(context.Background()); err != nil || !ok {
		t.Fatalf("status: %v %v", ok, err)
	}
	path, _ := configPath()
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "'/path with spaces/agento11y' kiro hook") {
		t.Fatal("unquoted executable")
	}
	other := filepath.Join(filepath.Dir(path), "personal.json")
	if err := os.WriteFile(other, []byte("personal"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Uninstall(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal("uninstall removed personal hooks")
	}
	if err := os.WriteFile(path, []byte(`{"version":"v1","hooks":[],"custom":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(); err == nil {
		t.Fatal("overwrote customized hooks")
	}
	if err := Uninstall(); err == nil {
		t.Fatal("removed customized hooks")
	}
}

func setupHook(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("USER_PROMPT", "")
	old := exportTurn
	t.Cleanup(func() { exportTurn = old })
}
func fire(t *testing.T, raw string) {
	t.Helper()
	var stdout bytes.Buffer
	if err := Hook(context.Background(), strings.NewReader(raw), &stdout, log.New(io.Discard, "", 0)); err != nil {
		t.Fatal(err)
	}
	if stdout.Len() != 0 {
		t.Fatal("hook wrote agent context")
	}
}

func TestTurnCaptureRetryAndDuplicateStop(t *testing.T) {
	setupHook(t)
	t.Setenv("AGENTO11Y_CONTENT_CAPTURE_MODE", "full")
	var sent []turn
	fail := true
	exportTurn = func(_ context.Context, id string, tr turn, _ agento11y.ContentCaptureMode, _ *log.Logger) error {
		if id != "s" {
			t.Fatal(id)
		}
		sent = append(sent, tr)
		if fail {
			return errors.New("offline")
		}
		return nil
	}
	fire(t, `{"hook_event_name":"UserPromptSubmit","session_id":"s","prompt":"hello"}`)
	fire(t, `{"hook_event_name":"postToolUse","session_id":"s","tool_name":"@postgres/query","tool_input":{"sql":"SELECT 1"},"tool_response":{"rows":[1]}}`)
	fire(t, `{"hook_event_name":"Stop","session_id":"s"}`)
	if len(sent) != 1 || len(sent[0].Tools) != 1 || sent[0].Prompt != "hello" {
		t.Fatalf("bad turn: %+v", sent)
	}
	// A new prompt must not overwrite an earlier failed export.
	fire(t, `{"hook_event_name":"UserPromptSubmit","session_id":"s","prompt":"next"}`)
	fail = false
	fire(t, `{"hook_event_name":"Stop","session_id":"s"}`)
	if len(sent) != 3 || sent[0].ID != sent[1].ID || sent[1].ID == sent[2].ID {
		t.Fatalf("unstable retry IDs: %+v", sent)
	}
	fire(t, `{"hook_event_name":"Stop","session_id":"s"}`)
	if len(sent) != 3 {
		t.Fatal("duplicate Stop exported twice")
	}
	if _, err := os.Stat(statePath("s")); !os.IsNotExist(err) {
		t.Fatal("completed state retained")
	}
}

func TestCapturePrivacy(t *testing.T) {
	for _, mode := range []string{"full", "no_tool_content", "metadata_only"} {
		t.Run(mode, func(t *testing.T) {
			setupHook(t)
			t.Setenv("AGENTO11Y_CONTENT_CAPTURE_MODE", mode)
			t.Setenv("AGENTO11Y_REDACT_INPUT_MESSAGES", "true")
			exportTurn = func(_ context.Context, _ string, tr turn, m agento11y.ContentCaptureMode, _ *log.Logger) error {
				_, gen := mapTurn("s", tr, m)
				raw, _ := json.Marshal(gen)
				if strings.Contains(string(raw), "sk-ant-api03-") {
					t.Fatal("secret exported")
				}
				if mode == "metadata_only" && tr.Prompt != "" {
					t.Fatal("prompt persisted in metadata mode")
				}
				if mode != "full" && len(tr.Tools[0].Input) > 0 {
					t.Fatal("tool body persisted outside full mode")
				}
				return nil
			}
			fire(t, `{"hook_event_name":"userPromptSubmit","session_id":"s","prompt":"hello"}`)
			fire(t, `{"hook_event_name":"PostToolUse","session_id":"s","tool_name":"shell","tool_input":{"command":"echo private"},"tool_response":"private"}`)
			s, _, err := fragmentstore.ReadJSON[session](statePath("s"))
			if err != nil {
				t.Fatal(err)
			}
			if mode != "full" && strings.Contains(string(s.Turns[0].Tools[0].Input), "private") {
				t.Fatal("raw tool content on disk")
			}
			fire(t, `{"hook_event_name":"stop","session_id":"s"}`)
		})
	}
}

func TestMalformedHooksAndTightenedCapture(t *testing.T) {
	setupHook(t)
	t.Setenv("AGENTO11Y_CONTENT_CAPTURE_MODE", "full")
	for _, raw := range []string{"", "{", "null", `{}`, `{"hook_event_name":"Stop"}`, `{"hook_event_name":"unknown","session_id":"s"}`} {
		fire(t, raw)
	}
	fire(t, `{"hook_event_name":"UserPromptSubmit","session_id":"../../s","prompt":"private"}`)
	t.Setenv("AGENTO11Y_CONTENT_CAPTURE_MODE", "metadata_only")
	exportTurn = func(_ context.Context, _ string, tr turn, _ agento11y.ContentCaptureMode, _ *log.Logger) error {
		if tr.Prompt != "" {
			t.Fatal("tightened mode leaked prompt")
		}
		return nil
	}
	fire(t, `{"hook_event_name":"SessionEnd","session_id":"../../s"}`)
}

func TestHTTPExport(t *testing.T) {
	bodies := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/generations:export" {
			t.Errorf("path %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		bodies <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[{"generation_id":"kiro-test","accepted":true}]}`)
	}))
	defer server.Close()
	t.Setenv("AGENTO11Y_ENDPOINT", server.URL)
	t.Setenv("AGENTO11Y_AUTH_TENANT_ID", "test")
	t.Setenv("AGENTO11Y_AUTH_TOKEN", "test")
	t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	now := time.Now().UTC()
	err := sendTurn(context.Background(), "s", turn{ID: "kiro-test", Prompt: "hello", StartedAt: now, CompletedAt: now}, agento11y.ContentCaptureModeFull, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	body := <-bodies
	if !bytes.Contains(body, []byte("kiro-test")) || !bytes.Contains(body, []byte("kiro.usage_available")) {
		t.Fatalf("missing generation: %s", body)
	}
}

func TestLaunchVersionAndArguments(t *testing.T) {
	for _, version := range []string{"kiro-cli 2.9.0", "unknown", "kiro-cli 3.1.0"} {
		t.Run(version, func(t *testing.T) {
			t.Chdir(t.TempDir())
			oldLook, oldExec, oldVersion := lookPath, execFn, versionOutput
			t.Cleanup(func() { lookPath, execFn, versionOutput = oldLook, oldExec, oldVersion })
			lookPath = func(name string) (string, error) {
				if name != "kiro-cli" {
					t.Fatal(name)
				}
				return "/bin/kiro-cli", nil
			}
			versionOutput = func(context.Context, string) ([]byte, error) { return []byte(version), nil }
			launched := false
			execFn = func(bin string, args, env []string) error {
				launched = true
				if bin != "/bin/kiro-cli" || strings.Join(args, "|") != "/bin/kiro-cli|chat|--agent|custom" {
					t.Fatalf("args %v", args)
				}
				return nil
			}
			err := Launch(context.Background(), []string{"chat", "--agent", "custom"}, nil, nil, io.Discard, io.Discard, log.New(io.Discard, "", 0), "dev")
			want := strings.Contains(version, "3.1.0")
			if launched != want || (err == nil) != want {
				t.Fatalf("launched=%v err=%v", launched, err)
			}
			path, _ := configPath()
			if !want {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("unsupported CLI modified hooks")
				}
			}
		})
	}
}

func TestFullWithMetadataSpansAndRedaction(t *testing.T) {
	setupHook(t)
	t.Setenv("AGENTO11Y_CONTENT_CAPTURE_MODE", "full_with_metadata_spans")
	t.Setenv("AGENTO11Y_REDACT_INPUT_MESSAGES", "true")
	exportTurn = func(_ context.Context, _ string, tr turn, mode agento11y.ContentCaptureMode, _ *log.Logger) error {
		start, gen := mapTurn("s", tr, mode)
		if start.ContentCapture != agento11y.ContentCaptureModeFullWithMetadataSpans {
			t.Fatal("lost span policy")
		}
		raw, _ := json.Marshal(gen)
		if bytes.Contains(raw, []byte("super-secret-password")) {
			t.Fatal("tool secret leaked")
		}
		if !bytes.Contains(raw, []byte("visible-command")) {
			t.Fatal("full generation content missing")
		}
		return nil
	}
	fire(t, `{"hook_event_name":"UserPromptSubmit","session_id":"s","prompt":"hello"}`)
	fire(t, `{"hook_event_name":"PostToolUse","session_id":"s","tool_name":"shell","tool_input":{"command":"visible-command","password":"super-secret-password"},"tool_response":{"password":"super-secret-password"}}`)
	raw, err := os.ReadFile(statePath("s"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("super-secret-password")) {
		t.Fatal("unredacted secret persisted")
	}
	fire(t, `{"hook_event_name":"Stop","session_id":"s"}`)
}
