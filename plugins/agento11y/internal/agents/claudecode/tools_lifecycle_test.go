package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/state"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/subagent"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/transcript"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

const rawSession = "lifecycle-session"
const rawCallAt = "2025-06-01T12:00:01Z"
const rawResultAt = "2025-06-01T12:00:04Z"

func toolHookEnv(t *testing.T) string {
	t.Helper()
	envconfig.PinAliasEnvBlank(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	old := transcriptSettleWindow
	transcriptSettleWindow = 0
	t.Cleanup(func() { transcriptSettleWindow = old })
	server := httptest.NewServer(http.HandlerFunc(acceptGenerationExport))
	t.Cleanup(server.Close)
	setHookExportEnv(t, server.URL)
	return filepath.Join(t.TempDir(), rawSession+".jsonl")
}
func putToolTranscript(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}
func toolCallLine(request, id, skill string) string {
	return buildHookAssistantLineAt(rawSession, request, "tool_use", rawCallAt, []any{map[string]any{"type": "tool_use", "id": id, "name": "Skill", "input": map[string]any{"skill": skill, "args": "private input"}}}, 5)
}
func toolResultLine(id string) string {
	return buildHookToolResultJSONLAt(rawSession, id, "private output", rawResultAt)
}
func stopTools(t *testing.T, path string) string {
	t.Helper()
	return runHookForTest(t, hookInput{HookEventName: "Stop", SessionID: rawSession, TranscriptPath: path})
}

func TestToolSharedReadRecovery(t *testing.T) {
	for _, tc := range []struct {
		name, fault, childID string
		persistent           bool
	}{
		{name: "parent", fault: "parent", childID: "child"},
		{name: "child", fault: "child", childID: "child"},
		{name: "persistent child first", fault: "child", childID: "a-child", persistent: true},
		{name: "persistent child last", fault: "child", childID: "z-child", persistent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := toolHookEnv(t)
			rec := installGlobalSpanRecorder(t)
			root := filepath.Join(strings.TrimSuffix(path, ".jsonl"), "subagents")
			child := filepath.Join(root, "agent-"+tc.childID+".jsonl")
			readable := filepath.Join(root, "agent-readable.jsonl")
			readableText := buildHookAssistantJSONLAt(rawSession, "readable", "end_turn", "done", 5, rawResultAt) + "\n"
			putToolTranscript(t, readable, readableText)
			text := toolCallLine("parent", "parent-call", "parent") + "\n" + toolResultLine("parent-call") + "\n"
			putToolTranscript(t, path, text)
			putToolTranscript(t, child, toolCallLine("child", "child-call", "child")+"\n"+toolResultLine("child-call")+"\n"+buildHookAssistantJSONLAt(rawSession, "child-close", "end_turn", "done", 5, rawResultAt)+"\n")
			before := state.Session{Title: "prior title"}
			if err := state.Save(rawSession, before); err != nil {
				t.Fatal(err)
			}
			original := readLiveComplete
			fail := true
			reads := map[string][]int64{}
			readLiveComplete = func(p string, offset int64) ([]transcript.Line, int64, transcript.Titles, error) {
				reads[p] = append(reads[p], offset)
				if fail && ((tc.fault == "parent" && p == path) || (tc.fault == "child" && p == child)) {
					return nil, offset, transcript.Titles{}, errors.New("transient fixture failure")
				}
				return original(p, offset)
			}
			t.Cleanup(func() { readLiveComplete = original })
			stopTools(t, path)
			if got := state.Load(rawSession); !reflect.DeepEqual(got, before) {
				t.Fatalf("failed read changed state: %+v", got)
			}
			if len(rec.Ended()) != 0 {
				t.Fatal("exported before staging all reads")
			}
			if tc.persistent {
				stopTools(t, path)
				if got := state.Load(rawSession); !reflect.DeepEqual(got, before) || len(rec.Ended()) != 0 {
					t.Fatal("persistent Stop failure changed state or exported")
				}
			} else {
				fail = false
			}
			reads = map[string][]int64{}
			event := "Stop"
			if tc.persistent {
				event = " SessionEnd "
			}
			logs := runHookForTest(t, hookInput{HookEventName: event, SessionID: rawSession, TranscriptPath: path})
			if len(reads[path]) != 1 || len(reads[child]) != 1 || len(reads[readable]) != 1 {
				t.Fatalf("initial consumers did not share reads: %v", reads)
			}
			if got := state.Load(rawSession).Offset; got != int64(len(toolCallLine("parent", "parent-call", "parent"))+1) {
				t.Fatalf("recovery offset=%d", got)
			}
			tools := spansByName(rec.Ended(), "execute_tool")
			gens := spansByName(rec.Ended(), "generateText")
			wantTools, wantGens := 2, 4
			if tc.persistent {
				wantTools, wantGens = 1, 2
				st := state.Load(rawSession)
				if st.SubagentOffsets[tc.childID] != 0 || st.Tools.Offsets[toolPath(child)] != 0 {
					t.Fatal("failed child advanced its checkpoints")
				}
				if st.SubagentOffsets["readable"] != int64(len(readableText)) {
					t.Fatal("readable child did not advance its checkpoint")
				}
				if len(st.Tools.Pending) != 0 || len(st.Tools.Spawns) != 0 || len(st.Tools.CompletedChildren) != 0 {
					t.Fatal("terminal partial export retained unresolved tools")
				}
				if !strings.Contains(logs, "read subagent transcript "+child+": transient fixture failure") {
					t.Fatalf("failed child was not logged: %s", logs)
				}
				runHookForTest(t, hookInput{HookEventName: event, SessionID: rawSession, TranscriptPath: path})
				if len(rec.Ended()) != len(tools)+len(gens) {
					t.Fatal("terminal partial export duplicated spans")
				}
			}
			if len(tools) != wantTools || len(gens) != wantGens {
				t.Fatalf("tools=%d generations=%d, want %d/%d logs=%s", len(tools), len(gens), wantTools, wantGens, logs)
			}
			for _, s := range tools {
				found := false
				for _, g := range gens {
					if s.Parent().SpanID() == g.SpanContext().SpanID() {
						found = true
					}
				}
				if !found {
					t.Fatal("recovered tool lost generation context")
				}
			}
		})
	}
}

func TestToolPartialTailAndSessionEnd(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete later", true: "discard unresolved"}[terminal], func(t *testing.T) {
			path := toolHookEnv(t)
			rec := installGlobalSpanRecorder(t)
			call := toolCallLine("request", "call", "review") + "\n"
			result := toolResultLine("call")
			putToolTranscript(t, path, call+result)
			stopTools(t, path)
			st := state.Load(rawSession)
			if st.Offset != int64(len(call)) || st.Tools.Offsets[toolPath(path)] != int64(len(call)) || len(st.Tools.Pending) != 1 {
				t.Fatalf("unsafe partial checkpoint: %+v", st)
			}
			if len(spansByName(rec.Ended(), "execute_tool")) != 0 {
				t.Fatal("unterminated result emitted")
			}
			if terminal {
				runHookForTest(t, hookInput{HookEventName: "SessionEnd", SessionID: rawSession, TranscriptPath: path})
				if got := state.Load(rawSession); len(got.Tools.Pending) != 0 || len(got.Tools.Spawns) != 0 {
					t.Fatal("terminal unresolved metadata retained")
				}
			} else {
				putToolTranscript(t, path, call+result+"\n")
				stopTools(t, path)
				stopTools(t, path)
				if len(spansByName(rec.Ended(), "execute_tool")) != 1 {
					t.Fatal("completed tail lost or duplicated")
				}
			}
		})
	}
}

func TestToolParentWithoutSampledTracing(t *testing.T) {
	for _, mode := range []string{"noop", "unsampled"} {
		t.Run(mode, func(t *testing.T) {
			path := toolHookEnv(t)
			prior := otel.GetTracerProvider()
			if mode == "noop" {
				otel.SetTracerProvider(noop.NewTracerProvider())
			} else {
				provider := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.NeverSample()))
				otel.SetTracerProvider(provider)
				t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
			}
			t.Cleanup(func() { otel.SetTracerProvider(prior) })
			call := toolCallLine("request", "call", "review") + "\n"
			putToolTranscript(t, path, call)
			stopTools(t, path)
			st := state.Load(rawSession)
			if len(st.Tools.Pending) != 1 {
				t.Fatalf("pending=%+v", st.Tools.Pending)
			}
			for _, p := range st.Tools.Pending {
				if !p.Parent.Ready || p.Parent.ID == "" || p.Parent.Model == "" {
					t.Fatalf("tracing controls parent readiness: %+v", p.Parent)
				}
				sc := trace.SpanContextFromContext(toolParentContext(context.Background(), p.Parent))
				if mode == "noop" && sc.IsValid() {
					t.Fatal("invented no-op context")
				}
				if mode == "unsampled" && (!sc.IsValid() || sc.IsSampled()) {
					t.Fatal("lost genuine unsampled context")
				}
			}
			putToolTranscript(t, path, call+toolResultLine("call")+"\n")
			stopTools(t, path)
			if len(state.Load(rawSession).Tools.Pending) != 0 {
				t.Fatal("result-only hook requires sampled tracing")
			}
		})
	}
}

func TestToolSameGenerationIDDistinctLifecycles(t *testing.T) {
	path := toolHookEnv(t)
	rec := installGlobalSpanRecorder(t)
	// The intervening assistant gives the two request-ID lifecycles distinct source positions.
	text := strings.Join([]string{toolCallLine("reused", "first", "first"), toolResultLine("first"), buildHookAssistantJSONLAt(rawSession, "between", "end_turn", "next", 5, rawResultAt), toolCallLine("reused", "second", "second"), toolResultLine("second")}, "\n") + "\n"
	putToolTranscript(t, path, text)
	stopTools(t, path)
	stopTools(t, path)
	gens := spansByName(rec.Ended(), "generateText")
	tools := spansByName(rec.Ended(), "execute_tool")
	if len(gens) != 3 || len(tools) != 2 {
		t.Fatalf("gens=%d tools=%d", len(gens), len(tools))
	}
	if spanAttr(gens[0], "agento11y.generation.id") != spanAttr(gens[2], "agento11y.generation.id") {
		t.Fatal("fixture must reuse generation ID")
	}
	for _, s := range tools {
		index := 0
		if spanAttr(s, "gen_ai.tool.call.id") == "second" {
			index = 2
		}
		if s.Parent().SpanID() != gens[index].SpanContext().SpanID() {
			t.Fatalf("call %s associated by generation ID", spanAttr(s, "gen_ai.tool.call.id"))
		}
	}
}

func TestToolScopedDuplicateIDs(t *testing.T) {
	for _, scope := range []string{"siblings", "nested", "sidechain", "raw sessions", "adjacent sidechain", "adjacent raw sessions", "adjacent sidechain distinct messages"} {
		t.Run(scope, func(t *testing.T) {
			path := toolHookEnv(t)
			rec := installGlobalSpanRecorder(t)
			reader := sdkmetric.NewManualReader()
			provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
			prior := otel.GetMeterProvider()
			otel.SetMeterProvider(provider)
			t.Cleanup(func() { otel.SetMeterProvider(prior); _ = provider.Shutdown(context.Background()) })
			adjacent := strings.HasPrefix(scope, "adjacent ")
			inline := strings.Contains(scope, "sidechain") || strings.Contains(scope, "raw sessions")
			text := toolCallLine("same-request", "same-call", "parent") + "\n" + toolResultLine("same-call") + "\n"
			if inline {
				request := "side-request"
				if adjacent {
					request = "same-request"
				}
				left := toolCallLine("same-request", "same-call", "parent")
				right := toolCallLine(request, "same-call", "child")
				result := toolResultLine("same-call")
				if strings.Contains(scope, "sidechain") {
					right = strings.ReplaceAll(right, `"sessionId":`, `"isSidechain":true,"agentId":"side","sessionId":`)
					result = strings.ReplaceAll(result, `"sessionId":`, `"isSidechain":true,"agentId":"side","sessionId":`)
				} else {
					right = strings.ReplaceAll(right, rawSession, "another-raw-session")
					result = strings.ReplaceAll(result, rawSession, "another-raw-session")
				}
				if strings.HasSuffix(scope, "distinct messages") {
					left = strings.Replace(left, `"message":{`, `"message":{"id":"msg_left",`, 1)
					right = strings.Replace(right, `"message":{`, `"message":{"id":"msg_right",`, 1)
				}
				if adjacent {
					text = left + "\n" + right + "\n" + toolResultLine("same-call") + "\n" + result + "\n"
				} else {
					text += right + "\n" + result + "\n"
				}
			} else {
				root := filepath.Join(strings.TrimSuffix(path, ".jsonl"), "subagents")
				children := []string{filepath.Join(root, "agent-one.jsonl"), filepath.Join(root, "agent-two.jsonl")}
				if scope == "nested" {
					children[1] = filepath.Join(root, "agent-one", "subagents", "agent-two.jsonl")
				}
				for i, child := range children {
					content := toolCallLine("same-request", "same-call", []string{"one", "two"}[i]) + "\n" + toolResultLine("same-call") + "\n" + buildHookAssistantJSONLAt(rawSession, "closing", "end_turn", "done", 5, rawResultAt) + "\n"
					putToolTranscript(t, child, content)
				}
			}
			putToolTranscript(t, path, text)
			stopTools(t, path)
			stopTools(t, path)
			tools := spansByName(rec.Ended(), "execute_tool")
			want := 3
			if inline {
				want = 2
			}
			if len(tools) != want {
				t.Fatalf("tools=%d want %d", len(tools), want)
			}
			parents := map[trace.SpanID]bool{}
			skills := map[string]bool{}
			for _, s := range tools {
				parents[s.Parent().SpanID()] = true
				skills[spanAttr(s, "gen_ai.skill.name")] = true
				if s.EndTime().Sub(s.StartTime()) != 3*time.Second {
					t.Fatal("wrong scoped raw interval")
				}
			}
			wantParents := want
			if adjacent {
				wantParents = 1
			}
			if len(parents) != wantParents || len(skills) != want {
				t.Fatalf("scopes merged: parents=%v skills=%v", parents, skills)
			}
			var metrics metricdata.ResourceMetrics
			if err := reader.Collect(context.Background(), &metrics); err != nil {
				t.Fatal(err)
			}
			metricSkills := map[string]bool{}
			for _, scope := range metrics.ScopeMetrics {
				for _, metric := range scope.Metrics {
					if metric.Name != "gen_ai.client.operation.duration" {
						continue
					}
					for _, point := range metric.Data.(metricdata.Histogram[float64]).DataPoints {
						operation, _ := point.Attributes.Value("gen_ai.operation.name")
						if operation.AsString() != "execute_tool" {
							continue
						}
						skill, _ := point.Attributes.Value("gen_ai.skill.name")
						metricSkills[skill.AsString()] = true
						if point.Count != 1 || point.Sum != 3 {
							t.Fatalf("skill %s: count=%d sum=%v", skill.AsString(), point.Count, point.Sum)
						}
					}
				}
			}
			if !reflect.DeepEqual(metricSkills, skills) {
				t.Fatalf("metric skills=%v, want %v", metricSkills, skills)
			}
		})
	}
}

func TestToolScopedSpawnCompletion(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		parentBackground, adjacent bool
	}{
		{"inline background", false, false},
		{"parent background", true, false},
		{"coalesced inline background", false, true},
		{"coalesced parent background", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parentBackground := tc.parentBackground
			path := toolHookEnv(t)
			rec := installGlobalSpanRecorder(t)
			root := filepath.Join(strings.TrimSuffix(path, ".jsonl"), "subagents")
			spawn := func(request, child, name string, background, side bool) string {
				call := buildHookAssistantLineAt(rawSession, request, "tool_use", rawCallAt, []any{map[string]any{"type": "tool_use", "id": "same-spawn", "name": "Agent", "input": map[string]any{"subagent_type": name, "run_in_background": background}}}, 5)
				ack := buildHookToolResultJSONLAt(rawSession, "same-spawn", "agentId: "+child, rawResultAt)
				text := call + "\n" + ack + "\n"
				if side {
					text = strings.ReplaceAll(text, `"sessionId":`, `"isSidechain":true,"agentId":"inline","sessionId":`)
				}
				return text
			}
			text := spawn("parent-spawn", "left", "left-type", parentBackground, false) + spawn("inline-spawn", "right", "right-type", !parentBackground, true)
			if tc.adjacent {
				left := strings.Split(strings.TrimSpace(spawn("shared", "left", "left-type", parentBackground, false)), "\n")
				right := strings.Split(strings.TrimSpace(spawn("shared", "right", "right-type", !parentBackground, true)), "\n")
				text = strings.Join([]string{left[0], right[0], left[1], right[1]}, "\n") + "\n"
			}
			putToolTranscript(t, path, text)
			for _, id := range []string{"left", "right"} {
				call := strings.Replace(toolCallLine("child-"+id, "same-child-call", id), `"stop_reason":"tool_use"`, `"stop_reason":""`, 1)
				putToolTranscript(t, filepath.Join(root, "agent-"+id+".jsonl"), call+"\n"+toolResultLine("same-child-call")+"\n")
			}
			stopTools(t, path)
			st := state.Load(rawSession)
			gens := spansByName(rec.Ended(), "generateText")
			wantGens := 3
			if tc.adjacent {
				wantGens = 2
			}
			if len(gens) != wantGens {
				t.Fatalf("one blocking child should finish, got %d generations", len(gens))
			}
			for i, id := range []string{"left", "right"} {
				if tc.adjacent {
					i = 0
				}
				if st.SubagentParents[id] != spanAttr(gens[i], "agento11y.generation.id") || st.SubagentNames[id] != AgentName+"/"+id+"-type" {
					t.Fatalf("scoped spawn lost parent/name for %s: %+v", id, st)
				}
				background := parentBackground
				if id == "right" {
					background = !parentBackground
				}
				if (st.SubagentOffsets[id] == 0) != background {
					t.Fatalf("scoped completion for %s: background=%t offset=%d", id, background, st.SubagentOffsets[id])
				}
			}
		})
	}
}

func TestToolBackgroundLateAcknowledgement(t *testing.T) {
	path := toolHookEnv(t)
	rec := installGlobalSpanRecorder(t)
	child := filepath.Join(strings.TrimSuffix(path, ".jsonl"), "subagents", "agent-child.jsonl")
	call := buildHookAssistantLineAt(rawSession, "spawn", "tool_use", rawCallAt, []any{map[string]any{"type": "tool_use", "id": "spawn-call", "name": "Agent", "input": map[string]any{"run_in_background": true, "subagent_type": "Explorer"}}}, 5) + "\n"
	putToolTranscript(t, path, call)
	stopTools(t, path)
	parentID := spanAttr(spansByName(rec.Ended(), "generateText")[0], "agento11y.generation.id")
	// A result can arrive before the child's final assistant fragment.
	first := strings.Replace(toolCallLine("child-first", "first-call", "first"), `"stop_reason":"tool_use"`, `"stop_reason":""`, 1) + "\n" + toolResultLine("first-call") + "\n"
	putToolTranscript(t, child, first)
	ack := buildHookToolResultJSONLAt(rawSession, "spawn-call", "agentId: child\nTranscript dir: "+filepath.Dir(child), rawResultAt) + "\n"
	putToolTranscript(t, path, call+ack)
	stopTools(t, path)
	st := state.Load(rawSession)
	if st.SubagentOffsets["child"] != 0 {
		t.Fatalf("late background ack finalized growing child: %+v", st)
	}
	if st.SubagentParents["child"] != parentID || st.SubagentNames["child"] != AgentName+"/explorer" {
		t.Fatalf("lost late parent/name: %+v", st)
	}
	if len(st.Tools.Spawns) != 1 {
		t.Fatal("background status pruned before mapping passed ack")
	}
	second := toolCallLine("child-second", "child-call", "review") + "\n"
	putToolTranscript(t, child, first+second)
	stopTools(t, path)
	for _, g := range spansByName(rec.Ended(), "generateText")[1:] {
		if spanAttr(g, "gen_ai.agent.name") != AgentName+"/explorer" {
			t.Fatalf("wrong child name: %s", spanAttr(g, "gen_ai.agent.name"))
		}
	}
	putToolTranscript(t, path, call+ack+buildHookAssistantJSONLAt(rawSession, "parent-close", "end_turn", "done", 5, rawResultAt)+"\n")
	stopTools(t, path)
	if len(state.Load(rawSession).Tools.Spawns) != 0 {
		t.Fatal("completed background acknowledgement accumulated")
	}
}

func TestToolAgentResultChildIDs(t *testing.T) {
	secret := "glc_" + strings.Repeat("a", 24)
	for _, tc := range []struct {
		name, result               string
		known, delayed, background bool
	}{
		{name: "known", result: "agentId: child", known: true},
		{name: "unknown", result: "agentId: " + secret},
		{name: "mixed", result: "agentId: child\nagentId: " + secret, known: true},
		{name: "directory", known: true},
		{name: "late foreground", result: "agentId: child", known: true, delayed: true},
		{name: "late background", result: "agentId: child", known: true, delayed: true, background: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := toolHookEnv(t)
			t.Setenv("AGENTO11Y_CONTENT_CAPTURE_MODE", "metadata_only")
			recorder := installGlobalSpanRecorder(t)
			child := filepath.Join(strings.TrimSuffix(path, ".jsonl"), "subagents", "agent-child.jsonl")
			stopReason := "end_turn"
			if tc.delayed && !tc.background {
				stopReason = ""
			}
			childText := buildHookAssistantJSONLAt(rawSession, "child-request", stopReason, "done", 5, rawResultAt) + "\n"
			if tc.known && !tc.delayed {
				putToolTranscript(t, child, childText)
			}
			result := tc.result
			if tc.name == "directory" {
				result = "Transcript dir: " + filepath.Dir(child)
			}
			call := buildHookAssistantLineAt(rawSession, "spawn", "tool_use", rawCallAt, []any{map[string]any{
				"type": "tool_use", "id": "spawn-call", "name": "Agent", "input": map[string]any{"subagent_type": "explorer", "run_in_background": tc.background},
			}}, 5) + "\n"
			putToolTranscript(t, path, call)
			stopTools(t, path)
			parentID := spanAttr(spansByName(recorder.Ended(), "generateText")[0], "agento11y.generation.id")
			putToolTranscript(t, path, call+buildHookToolResultJSONLAt(rawSession, "spawn-call", result, rawResultAt)+"\n"+buildHookAssistantJSONLAt(rawSession, "parent-close", "end_turn", "done", 5, rawResultAt)+"\n")
			stopTools(t, path)
			if tc.delayed {
				putToolTranscript(t, child, childText)
			}
			stopTools(t, path)
			st := state.Load(rawSession)
			persisted, err := json.Marshal(st)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(persisted), secret) {
				t.Fatal("Agent result content persisted as a child ID")
			}
			if tc.known {
				if len(st.SubagentParents) != 1 || st.SubagentParents["child"] != parentID || st.SubagentNames["child"] != AgentName+"/explorer" {
					t.Fatalf("known child association lost: %+v", st)
				}
				if len(st.Tools.Pending) != 0 || len(st.Tools.Spawns) != 0 || len(st.Tools.CompletedChildren) != 0 {
					t.Fatalf("resolved child association retained pending state: %+v", st.Tools)
				}
				if tc.delayed {
					children := 0
					for _, span := range spansByName(recorder.Ended(), "generateText") {
						if spanAttr(span, "gen_ai.response.id") == "child-request" {
							children++
							if spanAttr(span, "gen_ai.agent.name") != AgentName+"/explorer" {
								t.Fatal("late child lost its agent type")
							}
						}
					}
					if children != 1 {
						t.Fatalf("late child generations=%d, want 1", children)
					}
				}
			} else if len(st.SubagentParents) != 0 || len(st.SubagentNames) != 0 {
				t.Fatalf("unknown child association retained: %+v", st)
			}
			if tools := spansByName(recorder.Ended(), "execute_tool"); len(tools) != 1 {
				t.Fatalf("Agent observations=%d, want 1", len(tools))
			}
			runHookForTest(t, hookInput{HookEventName: "SessionEnd", SessionID: rawSession, TranscriptPath: path})
			if got := state.Load(rawSession); len(got.Tools.Pending) != 0 || len(got.Tools.Spawns) != 0 || len(got.Tools.CompletedChildren) != 0 {
				t.Fatalf("SessionEnd retained tool association state: %+v", got.Tools)
			}
		})
	}
}

func TestToolDelayedContentValidation(t *testing.T) {
	for _, mutation := range []string{"none", "missing", "session", "scope", "identity", "timestamp", "type", "invalid range"} {
		t.Run(mutation, func(t *testing.T) {
			path := toolHookEnv(t)
			rec := installGlobalSpanRecorder(t)
			t.Setenv("AGENTO11Y_CONTENT_CAPTURE_MODE", "full")
			call := toolCallLine("request", "call", "review") + "\n"
			result := toolResultLine("call") + "\n"
			putToolTranscript(t, path, call+result)
			raw, pos, err := transcript.ReadComplete(path, 0)
			if err != nil {
				t.Fatal(err)
			}
			st := state.Session{}
			initToolState(&st, path, nil)
			observeTools(&st, path, raw, pos, nil)
			for _, p := range st.Tools.Pending {
				p.Parent = state.ToolParent{ID: "parent", Ready: true}
			}
			switch mutation {
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "session":
				putToolTranscript(t, path, strings.ReplaceAll(call+result, rawSession, strings.Repeat("x", len(rawSession))))
			case "scope":
				putToolTranscript(t, path, strings.ReplaceAll(call+result, `"sessionId":`, `"isSidechain":true,"agentId":"other","sessionId":`))
			case "identity":
				putToolTranscript(t, path, strings.Replace(call+result, `"skill":"review"`, `"skill":"otherx"`, 1))
			case "timestamp":
				putToolTranscript(t, path, strings.Replace(call+result, rawCallAt, "2025-06-01T12:00:02Z", 1))
			case "type":
				putToolTranscript(t, path, strings.Replace(call+result, `"type":"assistant"`, `"type":"notassist"`, 1))
			case "invalid range":
				for _, p := range st.Tools.Pending {
					p.Call.Start = -1
				}
			}
			client := agento11y.NewClient(agento11y.Config{ContentCapture: agento11y.ContentCaptureModeFull, GenerationExport: agento11y.GenerationExportConfig{Protocol: agento11y.GenerationExportProtocolNone}})
			emitPendingTools(context.Background(), newRecordClients(client, agento11y.Config{}, nil), &st, agento11y.ContentCaptureModeFull, nil)
			_ = client.Shutdown(context.Background())
			tools := spansByName(rec.Ended(), "execute_tool")
			if len(tools) != 1 {
				t.Fatal("content read failure dropped structural observation")
			}
			args := spanAttr(tools[0], "gen_ai.tool.call.arguments")
			if mutation == "none" {
				if args == "" {
					t.Fatal("missing valid delayed content")
				}
			} else if args != "" {
				t.Fatalf("unvalidated delayed arguments: %s", args)
			}
		})
	}
}

func TestToolCanonicalPathAcrossHooks(t *testing.T) {
	path := toolHookEnv(t)
	rec := installGlobalSpanRecorder(t)
	aliasDir := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(filepath.Dir(path), aliasDir); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(aliasDir, filepath.Base(path))
	call := toolCallLine("request", "call", "review") + "\n"
	putToolTranscript(t, path, call)
	stopTools(t, alias)
	st := state.Load(rawSession)
	if len(st.Tools.Offsets) != 1 || st.Tools.Offsets[toolPath(path)] != int64(len(call)) {
		t.Fatalf("alias created separate observation cursor: %+v", st.Tools)
	}
	putToolTranscript(t, path, call+toolResultLine("call")+"\n")
	stopTools(t, path)
	stopTools(t, alias)
	if len(spansByName(rec.Ended(), "execute_tool")) != 1 || len(spansByName(rec.Ended(), "generateText")) != 1 {
		t.Fatal("path alias lost association or replayed generation")
	}
}

func TestToolRecordValidationReadiness(t *testing.T) {
	for _, protocol := range []string{"http", "otel"} {
		t.Run(protocol, func(t *testing.T) {
			path := toolHookEnv(t)
			rec := installGlobalSpanRecorder(t)
			if protocol == "otel" {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/healthz":
						_, _ = io.WriteString(w, `{"capabilities":["otel_generation_ingest_v1"]}`)
					case "/v1/traces", "/v1/metrics":
						w.Header().Set("Content-Type", "application/x-protobuf")
					default:
						t.Errorf("unexpected request %s", r.URL.Path)
					}
				}))
				t.Cleanup(server.Close)
				t.Setenv("AGENTO11Y_PROTOCOL", "otel")
				t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "true")
				t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", server.URL)
			}
			call := strings.Replace(toolCallLine("invalid-parent", "call", "review"), `"model":"claude-opus-4"`, `"model":""`, 1) + "\n"
			putToolTranscript(t, path, call+toolResultLine("call")+"\n")
			logs := stopTools(t, path)
			if !strings.Contains(logs, "generation.model.name is required") {
				t.Fatalf("fixture did not fail validation: %s", logs)
			}
			st := state.Load(rawSession)
			if protocol == "otel" {
				if !reflect.DeepEqual(withoutRecordedMetrics(st), state.Session{}) {
					t.Fatalf("OTel error changed state: %+v", st)
				}
				return
			}
			if st.Offset != int64(len(call)) || len(st.Tools.Pending) != 1 {
				t.Fatalf("HTTP error changed checkpoint semantics: %+v", st)
			}
			for _, p := range st.Tools.Pending {
				if p.Parent.Ready || p.Parent.TraceID != "" || p.Parent.SpanID != "" {
					t.Fatalf("callback before failed validation marked parent ready: %+v", p.Parent)
				}
			}
			if len(spansByName(rec.Ended(), "execute_tool")) != 0 {
				t.Fatal("failed generation emitted tool")
			}
			runHookForTest(t, hookInput{HookEventName: "SessionEnd", SessionID: rawSession, TranscriptPath: path})
			if len(state.Load(rawSession).Tools.Pending) != 0 {
				t.Fatal("terminal hook retained failed parent")
			}
		})
	}
}

func TestToolHistoricalMigrationLateChild(t *testing.T) {
	path := toolHookEnv(t)
	rec := installGlobalSpanRecorder(t)
	child := filepath.Join(strings.TrimSuffix(path, ".jsonl"), "subagents", "agent-known.jsonl")
	old := toolCallLine("historical", "old-call", "old") + "\n" + toolResultLine("old-call") + "\n" + buildHookAssistantJSONLAt(rawSession, "historical-close", "end_turn", "done", 5, rawResultAt) + "\n"
	putToolTranscript(t, path, old)
	prior := state.Session{Offset: int64(len(old)), SubagentOffsets: map[string]int64{"known": int64(len(old))}}
	if err := state.Save(rawSession, prior); err != nil {
		t.Fatal(err)
	}
	stopTools(t, path)
	if len(rec.Ended()) != 0 {
		t.Fatal("migration replayed parent history")
	}
	// The child checkpoint existed at migration, but its file was temporarily absent.
	putToolTranscript(t, child, old+toolCallLine("new-child", "new-call", "new")+"\n"+toolResultLine("new-call")+"\n"+buildHookAssistantJSONLAt(rawSession, "new-close", "end_turn", "done", 5, rawResultAt)+"\n")
	stopTools(t, path)
	stopTools(t, path)
	tools := spansByName(rec.Ended(), "execute_tool")
	if len(tools) != 1 || spanAttr(tools[0], "gen_ai.skill.name") != "new" {
		t.Fatalf("late child replayed or lost tools: %d", len(tools))
	}
	st := state.Load(rawSession)
	if st.Tools.Offsets[toolPath(child)] <= int64(len(old)) {
		t.Fatal("late child observation did not advance")
	}
}

func TestToolCursorReadMinimum(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		genRecord, obsRecord int
	}{
		{"equal", 1, 1}, {"observation behind", 2, 1}, {"observation ahead", 1, 2}, {"initialized zero", 2, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := toolHookEnv(t)
			a := buildHookAssistantJSONLAt(rawSession, "one", "end_turn", "first", 5, rawCallAt) + "\n"
			b := buildHookAssistantJSONLAt(rawSession, "two", "end_turn", "second", 5, rawResultAt) + "\n"
			offsets := []int64{0, int64(len(a)), int64(len(a + b))}
			child := filepath.Join(strings.TrimSuffix(path, ".jsonl"), "subagents", "agent-child.jsonl")
			putToolTranscript(t, path, a+b)
			putToolTranscript(t, child, a+b)
			st := state.Session{Offset: offsets[tc.genRecord], SubagentOffsets: map[string]int64{"child": offsets[tc.genRecord]}, Tools: state.ToolState{Version: toolStateVersion, Offsets: map[string]int64{toolPath(path): offsets[tc.obsRecord], toolPath(child): offsets[tc.obsRecord]}}}
			if err := state.Save(rawSession, st); err != nil {
				t.Fatal(err)
			}
			original := readLiveComplete
			counts := map[string]int{}
			readLiveComplete = func(p string, offset int64) ([]transcript.Line, int64, transcript.Titles, error) {
				counts[p]++
				if want := min(offsets[tc.genRecord], offsets[tc.obsRecord]); offset != want {
					t.Errorf("read %s from %d want min=%d", p, offset, want)
				}
				return original(p, offset)
			}
			t.Cleanup(func() { readLiveComplete = original })
			rec := installGlobalSpanRecorder(t)
			stopTools(t, path)
			if counts[path] != 1 || counts[child] != 1 {
				t.Fatalf("unshared reads: %v", counts)
			}
			if want := 2 * (2 - tc.genRecord); len(spansByName(rec.Ended(), "generateText")) != want {
				t.Fatalf("mapping ignored own cursor, want %d generations", want)
			}
		})
	}
}

func TestToolMigrationAndCursorOrdering(t *testing.T) {
	for _, tc := range []struct {
		name        string
		gen, obs    int64
		initialized bool
	}{
		{name: "old checkpoint", gen: 100}, {name: "observation behind", gen: 100, obs: 50, initialized: true}, {name: "observation ahead", gen: 50, obs: 100, initialized: true}, {name: "initialized zero", gen: 100, initialized: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent := filepath.Join(t.TempDir(), "session.jsonl")
			child := filepath.Join(t.TempDir(), "subagents", "agent-known.jsonl")
			st := state.Session{Offset: tc.gen, SubagentOffsets: map[string]int64{"known": 75}}
			if tc.initialized {
				st.Tools = state.ToolState{Version: toolStateVersion, Offsets: map[string]int64{toolPath(parent): tc.obs}}
			}
			initToolState(&st, parent, nil)
			want := tc.gen
			if tc.initialized {
				want = tc.obs
			}
			if st.Tools.Offsets[toolPath(parent)] != want {
				t.Fatal("migration reset explicit cursor")
			}
			// A known child may be absent during migration and appear after later hooks.
			initToolState(&st, parent, []string{child})
			childWant := int64(75)
			if tc.initialized {
				childWant = 0
			}
			if st.Tools.Offsets[toolPath(child)] != childWant {
				t.Fatalf("late known child cursor=%d want %d", st.Tools.Offsets[toolPath(child)], childWant)
			}
			st.SubagentOffsets[subagent.FileKey(child)] = 200
			initToolState(&st, parent, []string{child})
			if st.Tools.Offsets[toolPath(child)] != childWant {
				t.Fatal("cursor reinitialized from later generation checkpoint")
			}
			b, err := json.Marshal(st)
			if err != nil {
				t.Fatal(err)
			}
			var loaded state.Session
			if err = json.Unmarshal(b, &loaded); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(st.Tools.Offsets, loaded.Tools.Offsets) || !reflect.DeepEqual(st.Tools.LegacyOffsets, loaded.Tools.LegacyOffsets) {
				t.Fatalf("state round trip changed cursor membership: %s", b)
			}
		})
	}
}

func TestToolLegacyBackgroundAcknowledgement(t *testing.T) {
	for _, tc := range []struct {
		name             string
		nextTurn, nested bool
	}{
		{"result only", false, false},
		{"result with next turn", true, false},
		{"nested result only", false, true},
		{"nested result with next turn", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := toolHookEnv(t)
			rec := installGlobalSpanRecorder(t)
			root := filepath.Join(strings.TrimSuffix(path, ".jsonl"), "subagents")
			spawnPath := path
			child := filepath.Join(root, "agent-child.jsonl")
			if tc.nested {
				spawnPath = filepath.Join(root, "agent-outer.jsonl")
				child = filepath.Join(root, "agent-outer", "subagents", "agent-child.jsonl")
				putToolTranscript(t, path, buildHookAssistantJSONLAt(rawSession, "main", "end_turn", "done", 5, rawCallAt)+"\n")
			}
			call := buildHookAssistantLineAt(rawSession, "spawn", "tool_use", rawCallAt, []any{map[string]any{"type": "tool_use", "id": "spawn-call", "name": "Agent", "input": map[string]any{"subagent_type": "explorer", "run_in_background": true}}}, 5) + "\n"
			prefix := buildHookSidechainAssistantLineID(rawSession, "child-request", "child", "explorer", "open-message", "", []any{map[string]any{"type": "tool_use", "id": "child-read", "name": "Read", "input": map[string]any{"file_path": "a.go"}}}, 3) + "\n" + toolResultLine("child-read") + "\n"
			putToolTranscript(t, spawnPath, call)
			putToolTranscript(t, child, prefix)
			stopTools(t, path)
			st := state.Load(rawSession)
			spawnOffset := st.Offset
			if tc.nested {
				spawnOffset = st.SubagentOffsets["outer"]
			}
			if spawnOffset != int64(len(call)) || st.SubagentOffsets["child"] != 0 {
				t.Fatalf("unreachable legacy checkpoint: %+v", st)
			}
			st.Tools = state.ToolState{}
			if err := state.Save(rawSession, st); err != nil {
				t.Fatal(err)
			}
			ack := buildHookToolResultJSONLAt(rawSession, "spawn-call", "agentId: child\nTranscript dir: "+filepath.Dir(child), rawResultAt) + "\n"
			parent := call + ack
			if tc.nextTurn {
				parent += buildHookAssistantJSONLAt(rawSession, "next", "end_turn", "done", 5, rawResultAt) + "\n"
			}
			putToolTranscript(t, spawnPath, parent)
			original := readLiveComplete
			var spawnReads []int64
			readLiveComplete = func(p string, offset int64) ([]transcript.Line, int64, transcript.Titles, error) {
				if p == spawnPath {
					spawnReads = append(spawnReads, offset)
				}
				return original(p, offset)
			}
			t.Cleanup(func() { readLiveComplete = original })
			stopTools(t, path)
			stopTools(t, path)
			if got := state.Load(rawSession).SubagentOffsets["child"]; got != 0 {
				t.Fatalf("background acknowledgement advanced child to %d", got)
			}
			if len(spawnReads) != 2 || spawnReads[0] != 0 || spawnReads[1] < int64(len(call)) {
				t.Fatalf("migration must share one historical read, then resume checkpoints: %v", spawnReads)
			}
			tail := buildHookSidechainAssistantLineID(rawSession, "child-request", "child", "explorer", "open-message", "", []any{map[string]any{"type": "text", "text": "finished response"}}, 660) + "\n"
			putToolTranscript(t, child, prefix+tail)
			runHookForTest(t, hookInput{HookEventName: "SessionEnd", SessionID: rawSession, TranscriptPath: path})
			childCount := 0
			for _, span := range spansByName(rec.Ended(), "generateText") {
				if spanAttr(span, "gen_ai.response.id") != "child-request" {
					continue
				}
				childCount++
				var tokens int64
				for _, attr := range span.Attributes() {
					if attr.Key == "gen_ai.usage.output_tokens" {
						tokens = attr.Value.AsInt64()
					}
				}
				if tokens != 660 {
					t.Errorf("child output tokens=%d, want 660", tokens)
				}
			}
			if childCount != 1 {
				t.Fatalf("child generations=%d, want one complete generation", childCount)
			}
		})
	}
}
