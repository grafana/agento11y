package claudecode

import (
	"bytes"
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

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/state"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
)

func TestHookToolParentsWithRepeatedRequestIDs(t *testing.T) {
	for _, repeated := range []bool{false, true} {
		name := "distinct requests"
		if repeated {
			name = "repeated request"
		}
		t.Run(name, func(t *testing.T) {
			envconfig.PinAliasEnvBlank(t)
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			server := httptest.NewServer(http.HandlerFunc(acceptGenerationExport))
			defer server.Close()
			setHookExportEnv(t, server.URL)
			recorder := installGlobalSpanRecorder(t)
			path := filepath.Join(t.TempDir(), "transcript.jsonl")
			assistant := func(request, message, tool, at string) string {
				var line map[string]any
				if err := json.Unmarshal([]byte(buildHookAssistantToolUseJSONLAt("raw", request, tool, "Bash", at)), &line); err != nil {
					t.Fatal(err)
				}
				line["message"].(map[string]any)["id"] = message
				encoded, err := json.Marshal(line)
				if err != nil {
					t.Fatal(err)
				}
				return string(encoded)
			}
			secondRequest := "request-2"
			if repeated {
				secondRequest = "request-1"
			}
			text := strings.Join([]string{
				assistant("request-1", "msg-1", "one", "2025-06-01T12:00:01Z"),
				buildHookToolResultJSONLAt("raw", "one", "done", "2025-06-01T12:00:02Z"),
				assistant(secondRequest, "msg-2", "two", "2025-06-01T12:00:03Z"),
				buildHookToolResultJSONLAt("raw", "two", "done", "2025-06-01T12:00:04Z"),
				buildHookAssistantJSONLAt("raw", "final", "end_turn", "done", 5, "2025-06-01T12:00:05Z"),
			}, "\n") + "\n"
			if err := os.WriteFile(path, []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
			runHookForTest(t, hookInput{HookEventName: "Stop", SessionID: "repeated-requests", TranscriptPath: path})
			parents := spansByName(recorder.Ended(), "generateText")
			tools := spansByName(recorder.Ended(), "execute_tool")
			if len(parents) != 3 || len(tools) != 2 {
				t.Fatalf("generations=%d tools=%d, want 3/2", len(parents), len(tools))
			}
			for _, tool := range tools {
				index := 0
				if spanAttr(tool, "gen_ai.tool.call.id") == "two" {
					index = 1
				}
				if tool.Parent().SpanID() != parents[index].SpanContext().SpanID() {
					t.Errorf("tool %s has parent %s, want %s", tool.Name(), tool.Parent().SpanID(), parents[index].SpanContext().SpanID())
				}
			}
		})
	}
}

type toolCollectionFaultWriter struct {
	write func(string)
}

func (w toolCollectionFaultWriter) Write(p []byte) (int, error) {
	w.write(string(p))
	return len(p), nil
}

func TestHookToolCollectionReadFailureRecovery(t *testing.T) {
	for _, child := range []bool{false, true} {
		name := "parent"
		if child {
			name = "child"
		}
		t.Run(name, func(t *testing.T) {
			envconfig.PinAliasEnvBlank(t)
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			server := httptest.NewServer(http.HandlerFunc(acceptGenerationExport))
			defer server.Close()
			setHookExportEnv(t, server.URL)
			recorder := installGlobalSpanRecorder(t)
			const session = "collection-recovery"
			path := filepath.Join(t.TempDir(), session+".jsonl")
			target := path
			if child {
				target = filepath.Join(filepath.Dir(path), session, "subagents", "agent-child.jsonl")
				if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
					t.Fatal(err)
				}
			}
			text := buildHookAssistantToolUseJSONLAt("raw", "request", "one", "Bash", "2025-06-01T12:00:01Z") + "\n" +
				buildHookToolResultJSONLAt("raw", "one", "done", "2025-06-01T12:00:02Z") + "\n" +
				buildHookAssistantJSONLAt("raw", "final", "end_turn", "done", 5, "2025-06-01T12:00:03Z") + "\n"
			if err := os.WriteFile(path, []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
			if child {
				if err := os.WriteFile(target, []byte(text), 0600); err != nil {
					t.Fatal(err)
				}
			}
			injected, restored := false, false
			restore := func() {
				t.Helper()
				var err error
				if child {
					err = os.Chmod(target, 0600)
				} else {
					err = os.Rename(target+".hold", target)
				}
				if err != nil {
					t.Fatal(err)
				}
				restored = true
			}
			t.Cleanup(func() {
				if injected && !restored {
					restore()
				}
			})
			logger := log.New(toolCollectionFaultWriter{write: func(line string) {
				if !injected && strings.Contains(line, "coalesced to") {
					var err error
					if child {
						err = os.Chmod(target, 0000)
					} else {
						err = os.Rename(target, target+".hold")
					}
					if err != nil {
						t.Fatal(err)
					}
					injected = true
				}
				if injected && !restored && strings.Contains(line, "read tools ") {
					restore()
				}
			}}, "", 0)
			input := hookInput{HookEventName: "Stop", SessionID: session, TranscriptPath: path}
			payload, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			if err := Hook(context.Background(), bytes.NewReader(payload), io.Discard, logger); err != nil {
				t.Fatal(err)
			}
			if !injected || !restored {
				t.Fatal("collector reopen failure was not injected")
			}
			if len(recorder.Ended()) != 0 || state.Load(session).Offset != 0 {
				t.Error("generation advanced without tool observations")
			}
			runHookForTest(t, input)
			generations, tools := len(spansByName(recorder.Ended(), "generateText")), len(spansByName(recorder.Ended(), "execute_tool"))
			wantGenerations, wantTools := 2, 1
			if child {
				wantGenerations, wantTools = 4, 2
			}
			if generations != wantGenerations || tools != wantTools {
				t.Fatalf("recovered generations/tools=%d/%d, want %d/%d", generations, tools, wantGenerations, wantTools)
			}
			runHookForTest(t, input)
			if len(spansByName(recorder.Ended(), "generateText")) != generations || len(spansByName(recorder.Ended(), "execute_tool")) != tools {
				t.Error("recovery replayed telemetry")
			}
		})
	}
}
