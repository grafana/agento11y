package claudecode

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
	"reflect"
	"strings"
	"testing"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/state"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/transcript"
	"github.com/grafana/agento11y/plugins/agento11y/internal/xdg"
)

func TestHookTerminalFailureCleanup(t *testing.T) {
	for _, fault := range []string{
		"success-control", "parent-read", "missing-parent", "child-read", "discovery",
		"invalid-protocol", "spawns-only", "pending-only", "completed-only", "missing-endpoint", "missing-tenant", "missing-token",
		"otel-opt-in", "otel-endpoint", "otel-setup", "client-setup", "record", "flush",
		"missing-transcript-path",
	} {
		t.Run(fault, func(t *testing.T) {
			path := toolHookEnv(t)
			rec := installGlobalSpanRecorder(t)
			childRoot := strings.TrimSuffix(path, ".jsonl")
			child := filepath.Join(childRoot, "subagents", "agent-child.jsonl")
			call := buildHookAssistantLineAt(rawSession, "request", "tool_use", rawCallAt, []any{
				map[string]any{"type": "tool_use", "id": "skill-call", "name": "Skill", "input": map[string]any{"skill": "review", "args": "private input"}},
				map[string]any{"type": "tool_use", "id": "spawn-call", "name": "Agent", "input": map[string]any{"run_in_background": true, "subagent_type": "Explorer"}},
			}, 5) + "\n"
			putToolTranscript(t, path, call)
			stopTools(t, path)
			before := state.Load(rawSession)
			if len(before.Tools.Pending) != 2 || len(before.Tools.Spawns) != 1 || before.Offset != int64(len(call)) {
				t.Fatal("bad pending/spawn fixture")
			}
			before.Title, before.AITitle, before.CustomTitle, before.Model = "prior title", "AI title", "custom title", "prior model"
			before.SubagentOffsets = map[string]int64{"prior-child": 123}
			before.SubagentParents = map[string]string{"prior-child": "prior-generation"}
			before.SubagentNames = map[string]string{"prior-child": "explorer"}
			before.Tools.CompletedChildren = map[string]bool{"prior-child": true}
			switch fault {
			case "spawns-only":
				before.Tools.Pending, before.Tools.CompletedChildren = nil, nil
			case "pending-only":
				before.Tools.Spawns, before.Tools.CompletedChildren = nil, nil
			case "completed-only":
				before.Tools.Pending, before.Tools.Spawns = nil, nil
			}
			if err := state.Save(rawSession, before); err != nil {
				t.Fatal(err)
			}

			if fault != "discovery" {
				putToolTranscript(t, child, buildHookAssistantJSONLAt(rawSession, "child", "end_turn", "private child output", 5, rawResultAt)+"\n")
			}
			putToolTranscript(t, path, call+buildHookAssistantJSONLAt(rawSession, "new-parent", "end_turn", "private output", 5, rawResultAt)+"\n")
			original := readLiveComplete
			t.Cleanup(func() { readLiveComplete = original })
			input := hookInput{HookEventName: " SessionEnd ", SessionID: rawSession, TranscriptPath: path}
			ctx := context.Background()
			wantLog := ""
			switch fault {
			case "parent-read", "child-read":
				target := path
				if fault == "child-read" {
					target = child
				}
				readLiveComplete = func(p string, offset int64) ([]transcript.Line, int64, transcript.Titles, error) {
					if p == target {
						return nil, offset, transcript.Titles{}, errors.New("fixture failed read")
					}
					return original(p, offset)
				}
				wantLog = "fixture failed read"
			case "missing-parent":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				wantLog = "read transcript:"
			case "discovery":
				// A NUL in the directory path makes Stat fail on Unix and Windows.
				input.TranscriptPath = filepath.Join(childRoot+"\x00", "session.jsonl")
				wantLog = "list subagent transcripts:"
			case "invalid-protocol", "spawns-only", "pending-only", "completed-only":
				t.Setenv("AGENTO11Y_PROTOCOL", "invalid")
				wantLog = "unsupported AGENTO11Y_PROTOCOL"
			case "missing-endpoint", "missing-tenant", "missing-token":
				// HTTPS loopback gets no local-auth placeholders, so missing credentials must stop export.
				t.Setenv("SIGIL_ENDPOINT", "https://127.0.0.1:1")
				suffix := map[string]string{"missing-endpoint": "ENDPOINT", "missing-tenant": "AUTH_TENANT_ID", "missing-token": "AUTH_TOKEN"}[fault]
				t.Setenv("SIGIL_"+suffix, "")
				t.Setenv("AGENTO11Y_"+suffix, "")
				wantLog = "not exporting: missing AGENTO11Y_" + suffix
			case "otel-opt-in", "otel-endpoint", "otel-setup", "client-setup", "record", "flush":
				t.Setenv("AGENTO11Y_PROTOCOL", "otel")
				t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "true")
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/healthz" {
						_, _ = io.WriteString(w, `{"capabilities":[]}`)
						return
					}
					if fault == "flush" {
						http.Error(w, "fixture rejected export", http.StatusBadRequest)
						return
					}
					w.Header().Set("Content-Type", "application/x-protobuf")
				}))
				t.Cleanup(server.Close)
				t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", server.URL)
				switch fault {
				case "otel-opt-in":
					t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "false")
					wantLog = "not exporting:"
				case "otel-endpoint":
					t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", "invalid")
					wantLog = "not exporting:"
				case "otel-setup":
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					cancel()
					wantLog = "otel setup:"
				case "client-setup":
					t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", server.URL+"/otlp")
					wantLog = "client setup:"
				case "record":
					data, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					putToolTranscript(t, path, call+strings.ReplaceAll(string(data[len(call):]), `"model":"claude-opus-4"`, `"model":""`))
					wantLog = "enqueue:"
				case "flush":
					wantLog = "flush:"
				}
			case "missing-transcript-path":
				input.TranscriptPath = ""
				wantLog = "stdin:"
			}
			payload, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			spansBefore := len(rec.Ended())
			var logs bytes.Buffer
			if err := Hook(ctx, bytes.NewReader(payload), io.Discard, log.New(&logs, "", 0)); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(logs.String(), wantLog) {
				t.Fatalf("did not reach %q: %s", wantLog, logs.String())
			}
			got := state.Load(rawSession)
			if len(got.Tools.Pending) != 0 || len(got.Tools.Spawns) != 0 || len(got.Tools.CompletedChildren) != 0 {
				t.Errorf("SessionEnd retained pending=%d spawns=%d completed=%d", len(got.Tools.Pending), len(got.Tools.Spawns), len(got.Tools.CompletedChildren))
			}
			if fault != "success-control" && fault != "child-read" {
				before.Tools.Pending, before.Tools.Spawns, before.Tools.CompletedChildren = nil, nil, nil
				if !reflect.DeepEqual(withoutRecordedMetrics(got), before) {
					t.Error("failed terminal path changed state beyond tool cleanup")
				}
				if fault != "record" && fault != "flush" && len(rec.Ended()) != spansBefore {
					t.Error("failed pre-export path exported")
				}
			} else {
				if got.Offset <= before.Offset {
					t.Error("terminal export did not save the parent checkpoint")
				}
				if fault == "child-read" {
					if got.SubagentOffsets["child"] != 0 {
						t.Error("failed child read advanced its checkpoint")
					}
				} else if got.SubagentOffsets["child"] == 0 {
					t.Error("successful terminal event did not save the child checkpoint")
				}
			}
			encoded, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(encoded, []byte("private input")) || bytes.Contains(encoded, []byte("private output")) || bytes.Contains(encoded, []byte("private child output")) {
				t.Error("persisted raw payload")
			}
		})
	}
}

func TestHookSessionEndFailureWithoutPendingDoesNotSave(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "legacy"}[legacy], func(t *testing.T) {
			path := toolHookEnv(t)
			statePath := filepath.Join(xdg.AppStateRoot(), "claude-code", state.SanitizeSessionID(rawSession)+".state")
			var before []byte
			if legacy {
				if err := state.Save(rawSession, state.Session{Offset: 123, Title: "prior title", SubagentOffsets: map[string]int64{"child": 456}}); err != nil {
					t.Fatal(err)
				}
				var err error
				before, err = os.ReadFile(statePath)
				if err != nil {
					t.Fatal(err)
				}
			}
			original := readLiveComplete
			readLiveComplete = func(_ string, offset int64) ([]transcript.Line, int64, transcript.Titles, error) {
				return nil, offset, transcript.Titles{}, errors.New("fixture failed read")
			}
			t.Cleanup(func() { readLiveComplete = original })
			runHookForTest(t, hookInput{HookEventName: "SessionEnd", SessionID: rawSession, TranscriptPath: path})
			after, err := os.ReadFile(statePath)
			if legacy {
				if err != nil || !bytes.Equal(before, after) {
					t.Fatal("failed terminal read changed legacy state or initialized tool checkpoints")
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed terminal read created state: %v", err)
			}
		})
	}
}

func TestHookSessionEndCleanupSaveFailureLogged(t *testing.T) {
	path := toolHookEnv(t)
	putToolTranscript(t, path, toolCallLine("request", "call", "review")+"\n")
	stopTools(t, path)
	before := state.Load(rawSession)
	// A directory at Save's temporary file path reliably fails on all platforms,
	// unlike permission bits when tests run as root.
	tmp := filepath.Join(xdg.AppStateRoot(), "claude-code", state.SanitizeSessionID(rawSession)+".state.tmp")
	if err := os.Mkdir(tmp, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTO11Y_PROTOCOL", "invalid")
	logs := runHookForTest(t, hookInput{HookEventName: "SessionEnd", SessionID: rawSession, TranscriptPath: path})
	if !strings.Contains(logs, "discard session tools:") {
		t.Fatal("cleanup persistence failure was not logged")
	}
	if !reflect.DeepEqual(state.Load(rawSession), before) {
		t.Fatal("failed cleanup damaged existing state")
	}
	if err := os.Remove(tmp); err != nil {
		t.Fatal(err)
	}
	runHookForTest(t, hookInput{HookEventName: "SessionEnd", SessionID: rawSession, TranscriptPath: path})
	if len(state.Load(rawSession).Tools.Pending) != 0 {
		t.Fatal("cleanup did not recover after storage became writable")
	}
}

func TestHookTerminalFailureLateResult(t *testing.T) {
	for _, event := range []string{"SessionEnd", "Stop"} {
		for _, fail := range []bool{false, true} {
			name := event + map[bool]string{false: "/success", true: "/failed-read"}[fail]
			t.Run(name, func(t *testing.T) {
				path := toolHookEnv(t)
				rec := installGlobalSpanRecorder(t)
				call := toolCallLine("request", "call", "review") + "\n"
				putToolTranscript(t, path, call)
				stopTools(t, path)
				original := readLiveComplete
				t.Cleanup(func() { readLiveComplete = original })
				if fail {
					readLiveComplete = func(_ string, offset int64) ([]transcript.Line, int64, transcript.Titles, error) {
						return nil, offset, transcript.Titles{}, errors.New("terminal read failure")
					}
				}
				runHookForTest(t, hookInput{HookEventName: event, SessionID: rawSession, TranscriptPath: path})
				readLiveComplete = original
				runHookForTest(t, hookInput{HookEventName: "SessionStart", SessionID: rawSession, TranscriptPath: path, Model: "claude-opus-4"})
				putToolTranscript(t, path, call+toolResultLine("call")+"\n")
				stopTools(t, path)
				want := 0
				if event == "Stop" {
					want = 1
				}
				if count := len(spansByName(rec.Ended(), "execute_tool")); count != want {
					t.Errorf("after %s, SessionStart and late result: executions=%d want %d", name, count, want)
				}
			})
		}
	}
}
