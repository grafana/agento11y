package claudecode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/state"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/transcript"
	"go.opentelemetry.io/otel/trace"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"go.opentelemetry.io/otel/codes"
)

func pendingToolFixture(t *testing.T, gen agento11y.Generation, results map[string]*agento11y.ToolResult, times map[string]time.Time) state.Session {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tools.jsonl")
	var calls []any
	var blocks []string
	for _, msg := range gen.Output {
		for _, part := range msg.Parts {
			if part.ToolCall == nil {
				continue
			}
			call := part.ToolCall
			input := call.InputJSON
			if len(input) == 0 {
				input = json.RawMessage(`{}`)
			}
			calls = append(calls, map[string]any{"type": "tool_use", "id": call.ID, "name": call.Name, "input": input})
			result, ok := results[call.ID]
			at, hasTime := times[call.ID]
			if !ok && !hasTime {
				continue
			}
			if !hasTime {
				at = gen.CompletedAt.Add(time.Second)
			}
			content := any("")
			failed := false
			if result != nil {
				failed = result.IsError
				content = result.Content
				if len(result.ContentJSON) > 0 {
					content = result.ContentJSON
				}
			}
			timestamp := ""
			if !at.IsZero() {
				timestamp = at.Format(time.RFC3339Nano)
			}
			b, _ := json.Marshal(map[string]any{"type": "user", "sessionId": "fixture", "timestamp": timestamp, "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": call.ID, "content": content, "is_error": failed}}}})
			blocks = append(blocks, string(b))
		}
	}
	callAt := ""
	if !gen.CompletedAt.IsZero() {
		callAt = gen.CompletedAt.Format(time.RFC3339Nano)
	}
	text := buildHookAssistantLineAt("fixture", "request", "tool_use", callAt, calls, 1) + "\n" + strings.Join(blocks, "\n") + "\n"
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	raw, pos, err := transcript.ReadComplete(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	st := state.Session{}
	initToolState(&st, path, nil)
	observeTools(&st, path, raw, pos, nil)
	for _, p := range st.Tools.Pending {
		p.Parent = state.ToolParent{ID: "fixture", ConversationID: gen.ConversationID, AgentName: gen.AgentName, AgentVersion: gen.AgentVersion, Model: gen.Model.Name, Provider: gen.Model.Provider}
		captureToolParent([]*state.PendingTool{p}, trace.SpanContextFromContext(context.Background()))
	}
	return st
}

func TestHookRawToolLifecycle(t *testing.T) {
	for _, mode := range []agento11y.ContentCaptureMode{agento11y.ContentCaptureModeMetadataOnly, agento11y.ContentCaptureModeNoToolContent, agento11y.ContentCaptureModeFull, agento11y.ContentCaptureModeFullWithMetadataSpans} {
		for _, tc := range []struct {
			name                   string
			late, failed, fragment bool
			callAt, resultAt       string
			want                   int
			duration               time.Duration
		}{
			{name: "late success", late: true, callAt: "2025-06-01T12:00:01Z", resultAt: "2025-06-01T12:00:04Z", want: 1, duration: 3 * time.Second},
			{name: "late failure", late: true, failed: true, callAt: "2025-06-01T12:00:01Z", resultAt: "2025-06-01T12:00:04Z", want: 1, duration: 3 * time.Second},
			{name: "fragment raw interval", fragment: true, callAt: "2025-06-01T12:00:01Z", resultAt: "2025-06-01T12:00:04Z", want: 1, duration: 3 * time.Second},
			{name: "missing call time", resultAt: "2025-06-01T12:00:04Z"},
			{name: "missing result time", callAt: "2025-06-01T12:00:01Z"},
			{name: "reversed present times", callAt: "2025-06-01T12:00:04Z", resultAt: "2025-06-01T12:00:01Z", want: 1},
		} {
			t.Run(mode.String()+"/"+tc.name, func(t *testing.T) {
				envconfig.PinAliasEnvBlank(t)
				t.Setenv("XDG_STATE_HOME", t.TempDir())
				t.Setenv("AGENTO11Y_CONTENT_CAPTURE_MODE", mode.String())
				priorWindow := transcriptSettleWindow
				transcriptSettleWindow = 0
				t.Cleanup(func() { transcriptSettleWindow = priorWindow })
				server := httptest.NewServer(http.HandlerFunc(acceptGenerationExport))
				defer server.Close()
				setHookExportEnv(t, server.URL)
				rec := installGlobalSpanRecorder(t)
				const session = "raw-tools"
				path := filepath.Join(t.TempDir(), session+".jsonl")
				call := buildHookAssistantLineAt(session, "request", "tool_use", tc.callAt, []any{map[string]any{"type": "tool_use", "id": "call", "name": "Skill", "input": map[string]any{"skill": "review", "args": "private arguments"}}}, 5)
				if tc.fragment {
					call = buildHookAssistantLineAt(session, "request", "", tc.callAt, []any{map[string]any{"type": "tool_use", "id": "call", "name": "Skill", "input": map[string]any{"skill": "review"}}}, 0) + "\n" + buildHookAssistantJSONLAt(session, "request", "tool_use", "later fragment", 5, "2025-06-01T12:00:02Z")
				}
				result := buildHookToolResultJSONLAt(session, "call", "private result", tc.resultAt)
				if tc.failed {
					result = strings.Replace(result, `"tool_use_id":"call"`, `"tool_use_id":"call","is_error":true`, 1)
				}
				text := buildHookUserJSONLAt(session, "load review", "2025-06-01T12:00:00Z") + "\n" + call + "\n"
				if !tc.late {
					text += result + "\n"
				}
				if err := os.WriteFile(path, []byte(text), 0600); err != nil {
					t.Fatal(err)
				}
				input := hookInput{HookEventName: "Stop", SessionID: session, TranscriptPath: path}
				runHookForTest(t, input)
				persisted, err := json.Marshal(state.Load(session))
				if err != nil {
					t.Fatal(err)
				}
				for _, private := range []string{"private arguments", "private result", "tool_input", "input_json", "instructions"} {
					if strings.Contains(string(persisted), private) {
						t.Fatalf("raw tool content persisted: %s", persisted)
					}
				}
				if tc.late {
					if got := len(spansByName(rec.Ended(), "execute_tool")); got != 0 {
						t.Errorf("call-only emitted %d tools", got)
					}
					f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
					if err != nil {
						t.Fatal(err)
					}
					_, err = f.WriteString(result + "\n")
					_ = f.Close()
					if err != nil {
						t.Fatal(err)
					}
					runHookForTest(t, input)
				}
				runHookForTest(t, input)
				tools := spansByName(rec.Ended(), "execute_tool")
				if len(tools) != tc.want {
					t.Fatalf("tools=%d want %d", len(tools), tc.want)
				}
				if tc.want == 0 {
					return
				}
				s := tools[0]
				if got := s.EndTime().Sub(s.StartTime()); got != tc.duration {
					t.Errorf("duration=%s want %s", got, tc.duration)
				}
				if (s.Status().Code == codes.Error) != tc.failed {
					t.Errorf("status=%s failed=%t", s.Status().Code, tc.failed)
				}
				if spanAttr(s, "gen_ai.skill.name") != "review" {
					t.Error("missing skill identity")
				}
				gens := spansByName(rec.Ended(), "generateText")
				if len(gens) != 1 {
					t.Fatalf("generations=%d, want no replay", len(gens))
				}
				if s.Parent().SpanID() != gens[0].SpanContext().SpanID() {
					t.Error("lost original generation parent")
				}
			})
		}
	}
}
