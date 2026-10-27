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

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/mapper"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/state"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/transcript"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/trace"
)

func emitToolFixture(t *testing.T, client *agento11y.Client, gen agento11y.Generation, results map[string]*agento11y.ToolResult, resultAt map[string]time.Time, mode agento11y.ContentCaptureMode) {
	t.Helper()
	var calls []any
	for _, msg := range gen.Output {
		for _, part := range msg.Parts {
			if part.ToolCall == nil {
				continue
			}
			tc := part.ToolCall
			input := tc.InputJSON
			if len(input) == 0 {
				input = json.RawMessage(`{}`)
			}
			calls = append(calls, map[string]any{"type": "tool_use", "id": tc.ID, "name": tc.Name, "input": input})
		}
	}
	stamp := func(at time.Time) string {
		if at.IsZero() {
			return ""
		}
		return at.Format(time.RFC3339Nano)
	}
	text := buildHookAssistantLineAt("fixture", "request", "tool_use", stamp(gen.CompletedAt), calls, 7) + "\n"
	for id, result := range results {
		content := result.Content
		if content == "" && len(result.ContentJSON) > 0 {
			content = string(result.ContentJSON)
		}
		line, err := json.Marshal(map[string]any{"type": "user", "sessionId": "fixture", "timestamp": stamp(resultAt[id]), "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": content, "is_error": result.IsError}}}})
		if err != nil {
			t.Fatal(err)
		}
		text += string(line) + "\n"
	}
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	st := state.Session{}
	initToolState(&st, path, nil)
	if err := collectTools(&st, path, nil, nil); err != nil {
		t.Fatal(err)
	}
	lines, _, err := transcript.ReadComplete(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	coalesced, _ := mapper.Coalesce(lines)
	sources := map[int]transcript.Line{}
	gens, _ := mapper.Process(coalesced, &st, mapper.Options{SessionID: "fixture", GenerationSources: sources}, nil)
	associateTools(&st, path, sources, gens)
	for _, p := range st.Tools.Pending {
		p.Parent.Ready = true
	}
	emitPendingTools(context.Background(), client, &st, mode, nil)
}

func TestPendingSkillVerification(t *testing.T) {
	for _, tc := range []struct {
		name, evidence, duplicate, want string
		failed                          bool
	}{
		{name: "verified", evidence: `{"success":true,"commandName":"review"}`, want: "review"},
		{name: "mismatched attempt", evidence: `{"success":true,"commandName":"other"}`},
		{name: "text is not evidence"},
		{name: "explicit error", evidence: `{"success":true,"commandName":"review"}`, failed: true},
		{name: "conflicting duplicates", evidence: `{"success":true,"commandName":"review"}`, duplicate: `{"success":true,"commandName":"other"}`},
		{name: "contradictory duplicates", evidence: `{"success":true,"commandName":"review"}`, duplicate: `{"success":false,"commandName":"review"}`},
		{name: "identical duplicates", evidence: `{"success":true,"commandName":"review"}`, duplicate: `{"success":true,"commandName":"review"}`, want: "review"},
		{name: "duplicate success fields", evidence: `{"success":false,"success":true,"commandName":"review"}`},
		{name: "case conflicting success", evidence: `{"success":false,"Success":true,"commandName":"review"}`},
		{name: "case conflicting name", evidence: `{"success":true,"commandName":"other","CommandName":"review"}`},
		{name: "case conflicting error", evidence: `{"success":true,"commandName":"review","is_error":true,"IS_ERROR":false}`},
		{name: "case conflicting association", evidence: `{"success":true,"commandName":"review","tool_use_id":"other","TOOL_USE_ID":"one"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := sdkmetric.NewManualReader()
			provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
			defer func() { _ = provider.Shutdown(t.Context()) }()
			recorder := installGlobalSpanRecorder(t)
			client := agento11y.NewClient(agento11y.Config{Meter: provider.Meter("tools-test"), ContentCapture: agento11y.ContentCaptureModeMetadataOnly, GenerationExport: agento11y.GenerationExportConfig{Protocol: agento11y.GenerationExportProtocolNone}})
			defer func() { _ = client.Shutdown(t.Context()) }()
			path := filepath.Join(t.TempDir(), "transcript.jsonl")
			text := buildHookAssistantLineAt("raw", "request", "tool_use", "2025-06-01T12:00:01Z", []any{map[string]any{"type": "tool_use", "id": "one", "name": "Skill", "input": map[string]any{"skill": "review"}}}, 7) + "\n"
			for _, evidence := range []string{tc.evidence, tc.duplicate} {
				if evidence == "" && tc.duplicate != "" {
					continue
				}
				var line map[string]any
				if err := json.Unmarshal([]byte(buildHookToolResultJSONLAt("raw", "one", "success:true review", "2025-06-01T12:00:03Z")), &line); err != nil {
					t.Fatal(err)
				}
				if evidence != "" {
					line["toolUseResult"] = json.RawMessage(evidence)
				}
				if tc.failed {
					line["message"].(map[string]any)["content"].([]any)[0].(map[string]any)["is_error"] = true
				}
				encoded, err := json.Marshal(line)
				if err != nil {
					t.Fatal(err)
				}
				text += string(encoded) + "\n"
				if tc.duplicate == "" {
					break
				}
			}
			if err := os.WriteFile(path, []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
			st := state.Session{}
			initToolState(&st, path, nil)
			if err := collectTools(&st, path, nil, nil); err != nil {
				t.Fatal(err)
			}
			lines, _, err := transcript.ReadComplete(path, 0)
			if err != nil {
				t.Fatal(err)
			}
			coalesced, _ := mapper.Coalesce(lines)
			sources := map[int]transcript.Line{}
			gens, _ := mapper.Process(coalesced, &st, mapper.Options{SessionID: "hook", GenerationSources: sources}, nil)
			parents := associateTools(&st, path, sources, gens)
			for _, pending := range parents {
				captureToolParent(pending, trace.SpanContext{})
			}
			emitPendingTools(t.Context(), client, &st, agento11y.ContentCaptureModeMetadataOnly, nil)
			var data metricdata.ResourceMetrics
			if err := reader.Collect(t.Context(), &data); err != nil {
				t.Fatal(err)
			}
			count := uint64(0)
			for _, scope := range data.ScopeMetrics {
				for _, metric := range scope.Metrics {
					if metric.Name != "gen_ai.execute_tool.duration" {
						continue
					}
					histogram, ok := metric.Data.(metricdata.Histogram[float64])
					if !ok {
						t.Fatalf("histogram type %T", metric.Data)
					}
					for _, point := range histogram.DataPoints {
						count += point.Count
						name, _ := point.Attributes.Value("gen_ai.skill.name")
						if name.AsString() != tc.want {
							t.Errorf("verified name=%q, want %q", name.AsString(), tc.want)
						}
					}
				}
			}
			if count != 1 {
				t.Errorf("metric count=%d, want 1", count)
			}
			spans := spansByName(recorder.Ended(), "execute_tool")
			if len(spans) != 1 || spans[0].Name() != "execute_tool Skill review" {
				t.Errorf("attempted span identity changed: %v", spans)
			}
		})
	}
}

func TestHookToolUnterminatedRecords(t *testing.T) {
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
			const session = "unterminated-tools"
			path := filepath.Join(t.TempDir(), session+".jsonl")
			target := path
			if child {
				target = filepath.Join(filepath.Dir(path), session, "subagents", "agent-child.jsonl")
				if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			input := hookInput{HookEventName: "Stop", SessionID: session, TranscriptPath: path}
			text := buildHookAssistantToolUseJSONLAt("raw", "request", "one", "Bash", "2025-06-01T12:00:01Z")
			put := func() {
				t.Helper()
				if err := os.WriteFile(target, []byte(text), 0600); err != nil {
					t.Fatal(err)
				}
			}
			put()
			runHookForTest(t, input)
			if len(recorder.Ended()) != 0 || state.Load(session).Tools.Offsets[toolPath(target)] != 0 {
				t.Fatal("unterminated assistant record was consumed")
			}
			text += "\n"
			callEnd := int64(len(text))
			text += buildHookToolResultJSONLAt("raw", "one", "done", "2025-06-01T12:00:03Z")
			put()
			runHookForTest(t, input)
			if got := state.Load(session).Tools.Offsets[toolPath(target)]; got != callEnd {
				t.Errorf("checkpoint=%d, want %d before unterminated result", got, callEnd)
			}
			if len(spansByName(recorder.Ended(), "generateText")) != 1 || len(spansByName(recorder.Ended(), "execute_tool")) != 0 {
				t.Fatal("unterminated result finalized the tool")
			}
			text += "\n"
			put()
			runHookForTest(t, input)
			tools := spansByName(recorder.Ended(), "execute_tool")
			if len(tools) != 1 || len(spansByName(recorder.Ended(), "generateText")) != 1 {
				t.Fatalf("completed tail tools=%d or replayed generation", len(tools))
			}
			if tools[0].EndTime().Sub(tools[0].StartTime()) != 2*time.Second {
				t.Errorf("raw duration=%v", tools[0].EndTime().Sub(tools[0].StartTime()))
			}
		})
	}
}

func TestAssociateToolsScopedIDs(t *testing.T) {
	client, recorder := newSpanRecordingClient(t, agento11y.ContentCaptureModeMetadataOnly)
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	mainCall := buildHookAssistantToolUseJSONLAt("raw", "main-request", "same", "Bash", "2025-06-01T12:00:01Z")
	sideCall := buildHookAssistantToolUseJSONLAt("raw", "side-request", "same", "Read", "2025-06-01T12:00:02Z")
	sideResult := buildHookToolResultJSONLAt("raw", "same", "side result", "2025-06-01T12:00:06Z")
	side := func(raw string) string {
		var line map[string]any
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.Fatal(err)
		}
		line["isSidechain"] = true
		line["agentId"] = "a"
		line["attributionAgent"] = "general-purpose"
		encoded, err := json.Marshal(line)
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
	text := strings.Join([]string{mainCall, side(sideCall), buildHookToolResultJSONLAt("raw", "same", "main result", "2025-06-01T12:00:04Z"), side(sideResult)}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	st := state.Session{}
	initToolState(&st, path, nil)
	if err := collectTools(&st, path, nil, nil); err != nil {
		t.Fatal(err)
	}
	lines, _, err := transcript.ReadComplete(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	coalesced, _ := mapper.Coalesce(lines)
	sources := map[int]transcript.Line{}
	gens, _ := mapper.Process(coalesced, &st, mapper.Options{SessionID: "hook", GenerationSources: sources}, nil)
	toolParents := associateTools(&st, path, sources, gens)
	if len(gens) != 2 || len(st.Tools.Pending) != 2 {
		t.Fatalf("generations=%d pending=%d, want 2/2", len(gens), len(st.Tools.Pending))
	}
	for i, gen := range gens {
		genCtx, rec := client.StartGeneration(t.Context(), generationStart(gen))
		rec.SetResult(gen, nil)
		rec.End()
		if err := rec.Err(); err != nil {
			t.Fatal(err)
		}
		captureToolParent(toolParents[i], trace.SpanContextFromContext(genCtx))
	}
	emitPendingTools(t.Context(), client, &st, agento11y.ContentCaptureModeMetadataOnly, nil)
	tools := spansByName(recorder.Ended(), "execute_tool")
	if len(tools) != 2 {
		t.Fatalf("tools=%d, want 2", len(tools))
	}
	parents := spansByName(recorder.Ended(), "generateText")
	for _, tool := range tools {
		index := 0
		duration := 3 * time.Second
		if spanAttr(tool, "gen_ai.tool.name") == "Read" {
			index = 1
			duration = 4 * time.Second
		}
		if tool.Parent().SpanID() != parents[index].SpanContext().SpanID() || tool.EndTime().Sub(tool.StartTime()) != duration {
			t.Errorf("tool %s joined another scope: parent=%s duration=%v", tool.Name(), tool.Parent().SpanID(), tool.EndTime().Sub(tool.StartTime()))
		}
	}
}

func TestHookChildDistinctRawToolTimes(t *testing.T) {
	envconfig.PinAliasEnvBlank(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(acceptGenerationExport))
	defer server.Close()
	setHookExportEnv(t, server.URL)
	recorder := installGlobalSpanRecorder(t)
	const session = "child-raw-windows"
	path := filepath.Join(t.TempDir(), session+".jsonl")
	child := filepath.Join(filepath.Dir(path), session, "subagents", "agent-child.jsonl")
	if err := os.MkdirAll(filepath.Dir(child), 0700); err != nil {
		t.Fatal(err)
	}
	parentText := buildHookAssistantJSONLAt("raw-session", "shared-request", "end_turn", "parent", 5, "2025-06-01T12:00:00Z") + "\n"
	childText := buildHookAssistantToolUseJSONLAt("raw-session", "shared-request", "one", "Bash", "2025-06-01T12:00:01Z") + "\n" +
		buildHookAssistantToolUseJSONLAt("raw-session", "shared-request", "two", "Read", "2025-06-01T12:00:02Z") + "\n" +
		buildHookToolResultJSONLAt("raw-session", "one", "first", "2025-06-01T12:00:03Z") + "\n" +
		buildHookToolResultJSONLAt("raw-session", "two", "second", "2025-06-01T12:00:04Z") + "\n" +
		buildHookAssistantJSONLAt("raw-session", "reply", "end_turn", "child done", 5, "2025-06-01T12:00:05Z") + "\n"
	for file, text := range map[string]string{path: parentText, child: childText} {
		if err := os.WriteFile(file, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	runHookForTest(t, hookInput{HookEventName: "Stop", SessionID: session, TranscriptPath: path})
	gens := spansByName(recorder.Ended(), "generateText")
	tools := spansByName(recorder.Ended(), "execute_tool")
	if len(gens) != 3 || len(tools) != 2 {
		t.Fatalf("generations=%d tools=%d, want 3/2", len(gens), len(tools))
	}
	if spanAttr(gens[0], "agento11y.generation.id") == spanAttr(gens[1], "agento11y.generation.id") {
		t.Error("fixture did not exercise Unique")
	}
	for _, s := range tools {
		id := spanAttr(s, "gen_ai.tool.call.id")
		startText, endText := "2025-06-01T12:00:01Z", "2025-06-01T12:00:03Z"
		if id == "two" {
			startText = "2025-06-01T12:00:02Z"
			endText = "2025-06-01T12:00:04Z"
		}
		start, _ := time.Parse(time.RFC3339Nano, startText)
		end, _ := time.Parse(time.RFC3339Nano, endText)
		if !s.StartTime().Equal(start) || !s.EndTime().Equal(end) {
			t.Errorf("%s raw window = %s to %s, want %s to %s", id, s.StartTime(), s.EndTime(), start, end)
		}
		if s.Parent().SpanID() != gens[1].SpanContext().SpanID() || s.SpanContext().TraceID() != gens[1].SpanContext().TraceID() {
			t.Errorf("%s lost finalized child generation context", id)
		}
		if spanAttr(s, "gen_ai.conversation.id") != session || spanAttr(s, "gen_ai.agent.name") != "claude-code/subagent" {
			t.Errorf("%s lost Finalize identity", id)
		}
	}
}

func TestHookBackgroundAcknowledgementAfterSpawn(t *testing.T) {
	envconfig.PinAliasEnvBlank(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	oldWindow := transcriptSettleWindow
	transcriptSettleWindow = 0
	t.Cleanup(func() { transcriptSettleWindow = oldWindow })
	server := httptest.NewServer(http.HandlerFunc(acceptGenerationExport))
	defer server.Close()
	setHookExportEnv(t, server.URL)
	recorder := installGlobalSpanRecorder(t)
	const session = "background-late-ack"
	path := filepath.Join(t.TempDir(), session+".jsonl")
	child := filepath.Join(filepath.Dir(path), session, "subagents", "agent-child.jsonl")
	if err := os.MkdirAll(filepath.Dir(child), 0700); err != nil {
		t.Fatal(err)
	}
	put := func(path, text string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	parentText := buildHookAssistantLineAt(session, "spawn-request", "tool_use", "2025-06-01T12:00:00Z", []any{map[string]any{"type": "tool_use", "id": "spawn", "name": "Agent", "input": map[string]any{"run_in_background": true, "subagent_type": "general-purpose"}}}, 7) + "\n"
	fragment := func(terminal bool) string {
		stop := ""
		var content []any
		if terminal {
			stop = "tool_use"
		} else {
			content = []any{map[string]any{"type": "tool_use", "id": "child-tool", "name": "Bash", "input": map[string]any{"command": "true"}}}
		}
		var line map[string]any
		if err := json.Unmarshal([]byte(buildHookAssistantLineAt(session, "child-request", stop, "2025-06-01T12:00:01Z", content, 7)), &line); err != nil {
			t.Fatal(err)
		}
		line["message"].(map[string]any)["id"] = "same-message"
		encoded, err := json.Marshal(line)
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded) + "\n"
	}
	childText := fragment(false) + buildHookToolResultJSONLAt(session, "child-tool", "done", "2025-06-01T12:00:03Z") + "\n"
	put(path, parentText)
	put(child, childText)
	input := hookInput{HookEventName: "Stop", SessionID: session, TranscriptPath: path}
	runHookForTest(t, input)
	if len(spansByName(recorder.Ended(), "execute_tool")) != 0 {
		t.Fatal("open child or unfinished spawn emitted tool spans")
	}
	parentText += buildHookToolResultJSONLAt(session, "spawn", "agentId: child", "2025-06-01T12:00:04Z") + "\n" + buildHookAssistantJSONLAt(session, "reply", "end_turn", "spawned", 5, "2025-06-01T12:00:05Z") + "\n"
	put(path, parentText)
	runHookForTest(t, input)
	for _, s := range spansByName(recorder.Ended(), "execute_tool") {
		if spanAttr(s, "gen_ai.tool.call.id") == "child-tool" {
			t.Error("spawn acknowledgement finalized the background child")
		}
	}
	if got := state.Load(session).SubagentOffsets["child"]; got != 0 {
		t.Errorf("background child checkpoint advanced to %d before terminal fragment", got)
	}
	childText += fragment(true)
	put(child, childText)
	runHookForTest(t, input)
	count := 0
	for _, s := range spansByName(recorder.Ended(), "execute_tool") {
		if spanAttr(s, "gen_ai.tool.call.id") == "child-tool" {
			count++
			if s.EndTime().Sub(s.StartTime()) != 2*time.Second {
				t.Errorf("child window=%v", s.EndTime().Sub(s.StartTime()))
			}
		}
	}
	if count != 1 {
		t.Errorf("completed child emitted %d tool spans, want 1", count)
	}
}

func TestCaptureToolParentPreservesReadyContext(t *testing.T) {
	tid, _ := trace.TraceIDFromHex("0123456789abcdef0123456789abcdef")
	firstID, _ := trace.SpanIDFromHex("0123456789abcdef")
	secondID, _ := trace.SpanIDFromHex("fedcba9876543210")
	first := trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: firstID, TraceFlags: trace.FlagsSampled})
	second := trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: secondID})
	st := state.Session{Tools: state.ToolState{Pending: map[string]*state.PendingTool{"first": {Parent: state.ToolParent{ID: "reused-request"}}}}}
	st.Tools.Pending["second"] = &state.PendingTool{Parent: state.ToolParent{ID: "reused-request"}}
	captureToolParent([]*state.PendingTool{st.Tools.Pending["first"]}, first)
	captureToolParent([]*state.PendingTool{st.Tools.Pending["second"]}, second)
	captureToolParent([]*state.PendingTool{st.Tools.Pending["first"]}, second)
	for key, want := range map[string]trace.SpanContext{"first": first, "second": second} {
		got := trace.SpanContextFromContext(toolParentContext(t.Context(), st.Tools.Pending[key].Parent))
		if got.SpanID() != want.SpanID() || got.TraceFlags() != want.TraceFlags() {
			t.Errorf("%s context replaced: %v", key, got)
		}
	}
}

func TestToolCheckpointLateKnownChild(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	child := filepath.Join(filepath.Dir(path), "session", "subagents", "agent-child.jsonl")
	st := state.Session{Offset: 100, SubagentOffsets: map[string]int64{"child": 200}}
	initToolState(&st, path, nil)
	st.Offset = 300
	st.SubagentOffsets["child"] = 400
	initToolState(&st, path, []string{child})
	if st.Tools.Offsets[toolPath(path)] != 100 || st.Tools.Offsets[toolPath(child)] != 200 {
		t.Errorf("late known child used mapping checkpoints: %v", st.Tools.Offsets)
	}
}

func TestHookToolParentFailure(t *testing.T) {
	envconfig.PinAliasEnvBlank(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(acceptGenerationExport))
	defer server.Close()
	setHookExportEnv(t, server.URL)
	recorder := installGlobalSpanRecorder(t)
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	call := strings.ReplaceAll(buildHookAssistantToolUseJSONLAt("raw", "request", "one", "Bash", "2025-06-01T12:00:01Z"), `"model":"claude-opus-4"`, `"model":""`)
	text := call + "\n" + buildHookToolResultJSONLAt("raw", "one", "done", "2025-06-01T12:00:03Z") + "\n"
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	input := hookInput{HookEventName: "Stop", SessionID: "hook", TranscriptPath: path}
	logs := runHookForTest(t, input)
	if !strings.Contains(logs, "generation.model.name is required") {
		t.Fatalf("missing recorder failure: %s", logs)
	}
	pending := state.Load("hook").Tools.Pending
	if len(pending) != 1 {
		t.Fatalf("pending=%+v, want unresolvable call", pending)
	}
	for _, p := range pending {
		if p.Parent.Ready {
			t.Error("callback made failed parent ready")
		}
	}
	if tools := spansByName(recorder.Ended(), "execute_tool"); len(tools) != 0 {
		t.Errorf("failed parent emitted %d tools", len(tools))
	}
	input.HookEventName = "SessionEnd"
	runHookForTest(t, input)
	if len(state.Load("hook").Tools.Pending) != 0 {
		t.Error("SessionEnd retained unresolvable call")
	}
}

func TestHookToolLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name                                                            string
		early, child, upgrade, missingTime, missingResultTime, inverted bool
	}{
		{name: "late results only"}, {name: "completion before coalesced parent", early: true},
		{name: "child raw timestamps", child: true}, {name: "upgrade no replay", upgrade: true},
		{name: "child upgrade no replay", child: true, upgrade: true},
		{name: "missing result timestamp", missingResultTime: true},
		{name: "missing call timestamp", missingTime: true}, {name: "inverted timestamps", inverted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			envconfig.PinAliasEnvBlank(t)
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			oldWindow := transcriptSettleWindow
			transcriptSettleWindow = 0
			t.Cleanup(func() { transcriptSettleWindow = oldWindow })
			server := httptest.NewServer(http.HandlerFunc(acceptGenerationExport))
			defer server.Close()
			setHookExportEnv(t, server.URL)
			recorder := installGlobalSpanRecorder(t)
			const session = "tool-lifecycle"
			path := filepath.Join(t.TempDir(), session+".jsonl")
			target := path
			if tc.child || tc.early {
				target = filepath.Join(filepath.Dir(path), session, "subagents", "agent-child.jsonl")
				if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			callAt := "2025-06-01T12:00:01Z"
			if tc.missingTime {
				callAt = ""
			}
			stop := "tool_use"
			if tc.early {
				stop = ""
			}
			text := buildHookAssistantLineAt("raw-session", "req-a", stop, callAt, []any{
				map[string]any{"type": "tool_use", "id": "one", "name": "Bash", "input": map[string]any{"command": "private argument"}},
				map[string]any{"type": "tool_use", "id": "two", "name": "Read", "input": map[string]any{"file_path": "private path"}},
				map[string]any{"type": "tool_use", "id": "unfinished", "name": "Read", "input": map[string]any{}},
			}, 7) + "\n"
			put := func() {
				t.Helper()
				if err := os.WriteFile(target, []byte(text), 0600); err != nil {
					t.Fatal(err)
				}
			}
			hook := func(event string) {
				t.Helper()
				runHookForTest(t, hookInput{HookEventName: event, SessionID: session, TranscriptPath: path})
			}
			put()
			if tc.upgrade {
				prior := state.Session{Offset: int64(len(text))}
				if tc.child {
					prior.Offset = 0
					prior.SubagentOffsets = map[string]int64{"child": int64(len(text))}
				}
				if err := state.Save(session, prior); err != nil {
					t.Fatal(err)
				}
			}
			hook("Stop")
			if got := len(spansByName(recorder.Ended(), "execute_tool")); got != 0 {
				t.Errorf("before results: got %d tool spans, want 0", got)
			}
			firstGens := len(spansByName(recorder.Ended(), "generateText"))
			resultOne := "2025-06-01T12:00:03Z"
			if tc.inverted {
				resultOne = "2025-06-01T12:00:00Z"
			}
			resultTwo := "2025-06-01T12:00:04Z"
			if tc.missingResultTime {
				resultOne = ""
				resultTwo = ""
			}
			text += buildHookToolResultJSONLAt("raw-session", "one", "private result", resultOne) + "\n"
			text += buildHookToolResultJSONLAt("raw-session", "two", "other result", resultTwo) + "\n"
			put()
			hook("Stop")
			want := 2
			if tc.early || tc.upgrade || tc.missingTime || tc.missingResultTime {
				want = 0
			}
			if got := len(spansByName(recorder.Ended(), "execute_tool")); got != want {
				t.Errorf("results-only hook: got %d tool spans, want %d", got, want)
			}
			if got := len(spansByName(recorder.Ended(), "generateText")); got != firstGens {
				t.Errorf("results-only hook replayed generations: %d -> %d", firstGens, got)
			}
			if tc.early {
				text += buildHookAssistantLineAt("raw-session", "req-a", "tool_use", "2025-06-01T12:00:05Z", nil, 7) + "\n"
			}
			if tc.child {
				text += buildHookAssistantJSONLAt("raw-session", "req-b", "end_turn", "done", 5, "2025-06-01T12:00:05Z") + "\n"
			}
			put()
			hook("Stop")
			if !tc.upgrade {
				pending := state.Load(session).Tools.Pending
				if len(pending) == 0 {
					t.Error("unfinished calls were not retained before SessionEnd")
				}
			}
			hook("SessionEnd")
			hook("SessionEnd")
			if pending := state.Load(session).Tools.Pending; len(pending) != 0 {
				t.Errorf("SessionEnd retained unfinished calls: %+v", pending)
			}
			tools := spansByName(recorder.Ended(), "execute_tool")
			want = 2
			if tc.upgrade || tc.missingTime || tc.missingResultTime {
				want = 0
			}
			if len(tools) != want {
				t.Errorf("final: got %d tool spans, want %d", len(tools), want)
			}
			gens := spansByName(recorder.Ended(), "generateText")
			for _, s := range tools {
				id := spanAttr(s, "gen_ai.tool.call.id")
				end := "2025-06-01T12:00:03Z"
				if id == "two" {
					end = "2025-06-01T12:00:04Z"
				}
				if tc.inverted && id == "one" {
					end = "2025-06-01T12:00:01Z"
				}
				start, _ := time.Parse(time.RFC3339Nano, "2025-06-01T12:00:01Z")
				finish, _ := time.Parse(time.RFC3339Nano, end)
				if !s.StartTime().Equal(start) || !s.EndTime().Equal(finish) {
					t.Errorf("%s raw window = %s to %s, want %s to %s", id, s.StartTime(), s.EndTime(), start, finish)
				}
				if len(gens) == 0 || s.Parent().SpanID() != gens[0].SpanContext().SpanID() {
					t.Errorf("%s lost parent context", id)
				}
			}
			data, err := os.ReadFile(filepath.Join(os.Getenv("XDG_STATE_HOME"), "agento11y", "claude-code", state.SanitizeSessionID(session)+".state"))
			if err == nil && (strings.Contains(string(data), "private argument") || strings.Contains(string(data), "private result") || strings.Contains(string(data), "private path")) {
				t.Errorf("state retained content: %s", data)
			}
		})
	}
}
