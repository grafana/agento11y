package claudecode

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/state"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// slashUserLine builds a user transcript line with the identity fields Claude
// Code writes for a slash command record and its expanded body.
func slashUserLine(sessionID, uuid, parentUUID, ts string, meta bool, content any) string {
	line := map[string]any{
		"type": "user", "sessionId": sessionID, "timestamp": ts, "version": "1.0.0",
		"uuid": uuid, "parentUuid": parentUUID,
		"message": map[string]any{"role": "user", "content": content},
	}
	if meta {
		line["isMeta"] = true
	}
	data, _ := json.Marshal(line)
	return string(data)
}

func slashRecord(name string) string {
	return "<command-message>" + strings.TrimPrefix(name, "/") + "</command-message>\n<command-name>" + name +
		"</command-name>\n<command-args>private-args-sentinel</command-args>"
}

// withAttribution adds Claude Code's attributionSkill to an assistant line.
func withAttribution(t *testing.T, line, skill string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatal(err)
	}
	m["attributionSkill"] = skill
	data, _ := json.Marshal(m)
	return string(data)
}

// TestHook_SlashSkillReportedLikeNativeSkill checks that a skill the user
// invokes as a slash command is exported exactly like a Skill tool call: one
// "execute_tool Skill <name>" span with gen_ai.skill.name, a child of the turn
// that answers it, and the same load_skill duration metric. Local commands,
// commands Claude Code does not attribute to the skill, and invalid names
// produce nothing, and no tool content leaves in any capture mode.
func TestHook_SlashSkillReportedLikeNativeSkill(t *testing.T) {
	const (
		sessionID = "slash-skill-session"
		promptAt  = "2025-06-01T12:00:00Z"
		expandAt  = "2025-06-01T12:00:00.5Z"
		replyAt   = "2025-06-01T12:00:03Z"
	)
	secret := "glc_" + strings.Repeat("a", 24)
	head := []string{
		slashUserLine(sessionID, "cmd-1", "", promptAt, false, slashRecord("/openspec-explore")),
	}
	tail := []string{
		slashUserLine(sessionID, "meta-1", "cmd-1", expandAt, true, []any{map[string]any{
			"type": "text", "text": "Base directory for this skill: /private/skills/openspec-explore\n\nprivate-body-sentinel",
		}}),
		withAttribution(t, buildHookAssistantJSONLAt(sessionID, "req_a", "end_turn", "answer", 5, replyAt), "openspec-explore"),
		// A local command writes a record but is never expanded.
		slashUserLine(sessionID, "cmd-2", "", "2025-06-01T12:00:04Z", false, slashRecord("/model")),
		slashUserLine(sessionID, "out-2", "cmd-2", "2025-06-01T12:00:04Z", false, "<local-command-stdout>Set model</local-command-stdout>"),
		// Expanded, but Claude Code attributes the answer to no skill.
		slashUserLine(sessionID, "cmd-3", "", "2025-06-01T12:00:05Z", false, slashRecord("/not-a-skill")),
		slashUserLine(sessionID, "meta-3", "cmd-3", "2025-06-01T12:00:05.1Z", true, "expanded prompt"),
		buildHookAssistantJSONLAt(sessionID, "req_b", "end_turn", "answer", 5, "2025-06-01T12:00:06Z"),
		// A secret-like name is never a skill name.
		slashUserLine(sessionID, "cmd-4", "", "2025-06-01T12:00:07Z", false, slashRecord("/"+secret)),
		slashUserLine(sessionID, "meta-4", "cmd-4", "2025-06-01T12:00:07.1Z", true, "expanded prompt"),
		withAttribution(t, buildHookAssistantJSONLAt(sessionID, "req_c", "end_turn", "answer", 5, "2025-06-01T12:00:08Z"), secret),
	}

	for _, mode := range []agento11y.ContentCaptureMode{
		agento11y.ContentCaptureModeMetadataOnly,
		agento11y.ContentCaptureModeNoToolContent,
		agento11y.ContentCaptureModeFull,
		agento11y.ContentCaptureModeFullWithMetadataSpans,
	} {
		for _, split := range []bool{false, true} {
			name := mode.String()
			if split {
				name += "/split"
			}
			t.Run(name, func(t *testing.T) {
				batches := [][]string{append(append([]string{}, head...), tail...)}
				if split {
					// The hook first sees only the command record; the expansion and
					// the answer arrive with the next hook.
					batches = [][]string{head, tail}
				}
				tools, gens, metrics := runSlashHook(t, mode, sessionID, batches)
				if len(tools) != 1 {
					names := make([]string, 0, len(tools))
					for _, s := range tools {
						names = append(names, s.Name)
					}
					t.Fatalf("tool spans = %v, want exactly one slash skill span", names)
				}
				tool := tools[0]
				attrs := map[string]string{}
				for _, a := range tool.Attributes {
					attrs[a.Key] = a.Value.GetStringValue()
				}
				if tool.Name != "execute_tool Skill openspec-explore" || attrs["gen_ai.skill.name"] != "openspec-explore" ||
					attrs["gen_ai.tool.name"] != "Skill" || attrs["gen_ai.tool.type"] != "function" || attrs["gen_ai.conversation.id"] != sessionID {
					t.Errorf("slash skill span = %q %v, want the native Skill span shape", tool.Name, attrs)
				}
				parent := gens[hex.EncodeToString(tool.ParentSpanId)]
				if parent == nil || !bytes.Equal(parent.TraceId, tool.TraceId) {
					t.Errorf("slash skill span is not a child of a generation span")
				}
				if got, want := tool.EndTimeUnixNano-tool.StartTimeUnixNano, uint64(500_000_000); got != want {
					t.Errorf("slash skill span duration = %dns, want %dns (record to expansion)", got, want)
				}
				if attrs["gen_ai.tool.call.arguments"] != "" || attrs["gen_ai.tool.call.result"] != "" {
					t.Errorf("slash skill span carries tool content")
				}
				for _, a := range tool.Attributes {
					v := a.Value.GetStringValue()
					for _, sentinel := range []string{"private-args-sentinel", "private-body-sentinel", "/private/skills", secret} {
						if strings.Contains(v, sentinel) {
							t.Errorf("attribute %s leaked %q", a.Key, sentinel)
						}
					}
				}

				found := false
				for _, metric := range metrics {
					if metric.Name != "gen_ai.client.operation.duration" {
						continue
					}
					for _, point := range metric.GetHistogram().GetDataPoints() {
						m := map[string]string{}
						for _, a := range point.Attributes {
							m[a.Key] = a.Value.GetStringValue()
						}
						if m["gen_ai.operation.name"] != "execute_tool" {
							continue
						}
						found = true
						if m["gen_ai.skill.name"] != "openspec-explore" || m["agento11y.tool.execution.kind"] != "load_skill" || m["gen_ai.tool.name"] != "Skill" {
							t.Errorf("tool metric attributes = %v, want the native load_skill labels", m)
						}
					}
				}
				if !found {
					t.Error("no load_skill duration metric for the slash skill")
				}

				if pending := state.Load(sessionID).Tools.Pending; len(pending) != 0 {
					t.Errorf("slash candidates left in state: %v", pending)
				}
			})
		}
	}
}

// runSlashHook runs the hook once per batch, each batch appending lines to the
// transcript, and returns the exported tool spans, generation spans by span
// ID, and metrics.
func runSlashHook(t *testing.T, mode agento11y.ContentCaptureMode, sessionID string, batches [][]string) ([]*tracepb.Span, map[string]*tracepb.Span, []*metricpb.Metric) {
	t.Helper()
	envconfig.PinAliasEnvBlank(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("AGENTO11Y_CONTENT_CAPTURE_MODE", mode.String())
	var mu sync.Mutex
	var metrics []*metricpb.Metric
	var spans []*tracepb.Span
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/api/v1/generations:export":
			r.Body = io.NopCloser(bytes.NewReader(body))
			acceptGenerationExport(w, r)
		case "/v1/metrics":
			var request colmetricpb.ExportMetricsServiceRequest
			if err := proto.Unmarshal(body, &request); err != nil {
				t.Error(err)
			}
			for _, resource := range request.ResourceMetrics {
				for _, scope := range resource.ScopeMetrics {
					metrics = append(metrics, scope.Metrics...)
				}
			}
			w.Header().Set("Content-Type", "application/x-protobuf")
		case "/v1/traces":
			var request coltracepb.ExportTraceServiceRequest
			if err := proto.Unmarshal(body, &request); err != nil {
				t.Error(err)
			}
			for _, resource := range request.ResourceSpans {
				for _, scope := range resource.ScopeSpans {
					spans = append(spans, scope.Spans...)
				}
			}
			w.Header().Set("Content-Type", "application/x-protobuf")
		}
	}))
	t.Cleanup(server.Close)
	setHookExportEnv(t, server.URL)
	t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", server.URL)

	path := filepath.Join(t.TempDir(), "session.jsonl")
	input := hookInput{HookEventName: "Stop", SessionID: sessionID, TranscriptPath: path}
	var written []string
	for _, batch := range batches {
		written = append(written, batch...)
		if err := os.WriteFile(path, []byte(strings.Join(written, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runHookForTest(t, input)
	}

	mu.Lock()
	defer mu.Unlock()
	var tools []*tracepb.Span
	gens := map[string]*tracepb.Span{}
	for _, span := range spans {
		switch {
		case strings.HasPrefix(span.Name, "execute_tool"):
			tools = append(tools, span)
		case strings.HasPrefix(span.Name, "generateText"):
			gens[hex.EncodeToString(span.SpanId)] = span
		}
	}
	return tools, gens, metrics
}

// TestHook_SlashSkillCases covers the record shapes and outcomes that the
// combined scenario above does not: IDE context ahead of the record, legacy
// namespaced commands, an interrupted answer, plain prompt commands, a local
// command whose output arrives with a later hook, and sidechain records.
func TestHook_SlashSkillCases(t *testing.T) {
	const session = "slash-cases"
	skillBody := []any{map[string]any{"type": "text", "text": "Base directory for this skill: /private/skills/x\n\nbody"}}
	answer := func(req, ts, skill string) string {
		line := buildHookAssistantJSONLAt(session, req, "end_turn", "answer", 5, ts)
		if skill == "" {
			return line
		}
		return withAttribution(t, line, skill)
	}
	tests := []struct {
		name    string
		batches [][]string
		want    []string // expected tool span names
	}{
		{
			name: "IDE context block ahead of the record",
			batches: [][]string{{
				slashUserLine(session, "c1", "", "2025-06-01T12:00:00Z", false, []any{
					map[string]any{"type": "text", "text": "<ide_opened_file>The user opened /private/app/main.go</ide_opened_file>"},
					map[string]any{"type": "text", "text": slashRecord("/openspec-explore")},
				}),
				slashUserLine(session, "m1", "c1", "2025-06-01T12:00:00.2Z", true, skillBody),
				answer("req_a", "2025-06-01T12:00:02Z", "openspec-explore"),
			}},
			want: []string{"execute_tool Skill openspec-explore"},
		},
		{
			name: "legacy namespaced command attributed by Claude Code",
			batches: [][]string{{
				slashUserLine(session, "c1", "", "2025-06-01T12:00:00Z", false, slashRecord("/opsx:propose")),
				slashUserLine(session, "m1", "c1", "2025-06-01T12:00:00.2Z", true, "Propose a change."),
				answer("req_a", "2025-06-01T12:00:02Z", "opsx:propose"),
			}},
			want: []string{"execute_tool Skill opsx:propose"},
		},
		{
			name: "interrupted answer still reports the skill load",
			batches: [][]string{{
				slashUserLine(session, "c1", "", "2025-06-01T12:00:00Z", false, slashRecord("/openspec-explore")),
				slashUserLine(session, "m1", "c1", "2025-06-01T12:00:00.2Z", true, skillBody),
				slashUserLine(session, "i1", "m1", "2025-06-01T12:00:01Z", false, "[Request interrupted by user]"),
				slashUserLine(session, "p2", "i1", "2025-06-01T12:00:05Z", false, "continue"),
				answer("req_b", "2025-06-01T12:00:06Z", ""),
			}},
			want: []string{"execute_tool Skill openspec-explore"},
		},
		{
			name: "plain prompt command without attribution",
			batches: [][]string{{
				slashUserLine(session, "c1", "", "2025-06-01T12:00:00Z", false, slashRecord("/my-prompt")),
				slashUserLine(session, "m1", "c1", "2025-06-01T12:00:00.2Z", true, "expanded prompt"),
				answer("req_a", "2025-06-01T12:00:02Z", ""),
			}},
		},
		{
			name: "local command output arrives with a later hook",
			batches: [][]string{
				{slashUserLine(session, "c1", "", "2025-06-01T12:00:00Z", false, slashRecord("/model"))},
				{
					slashUserLine(session, "o1", "c1", "2025-06-01T12:00:00.1Z", false, "<local-command-stdout>Set model</local-command-stdout>"),
					slashUserLine(session, "p2", "o1", "2025-06-01T12:00:05Z", false, "hello"),
					answer("req_a", "2025-06-01T12:00:06Z", ""),
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tools, gens, _ := runSlashHook(t, agento11y.ContentCaptureModeMetadataOnly, session, tt.batches)
			var got []string
			for _, s := range tools {
				got = append(got, s.Name)
				if gens[hex.EncodeToString(s.ParentSpanId)] == nil {
					t.Errorf("%s: not a child of a generation span", s.Name)
				}
			}
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("tool spans = %v, want %v", got, tt.want)
			}
			if pending := state.Load(session).Tools.Pending; len(pending) != 0 {
				t.Errorf("slash candidates left in state: %v", pending)
			}
		})
	}
}
