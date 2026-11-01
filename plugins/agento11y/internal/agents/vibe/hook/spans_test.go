package hook

import (
	"context"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/grafana/agento11y/go/agento11y"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/vibe/toolevents"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
)

// TestEmitToolSpans asserts that each assistant tool call becomes one
// execute_tool span, that a post_tool event drives real duration and an
// error status, and that a call with no event still produces a span with
// synthetic (zero-duration) timing.
func TestEmitToolSpans(t *testing.T) {
	envconfig.PinAliasEnvBlank(t)
	t.Setenv("HOME", t.TempDir())
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	logger := log.New(io.Discard, "", 0)
	client := agento11y.NewClient(agento11y.Config{
		ContentCapture:   agento11y.ContentCaptureModeFull,
		Tracer:           tp.Tracer("test"),
		Logger:           logger,
		GenerationExport: agento11y.GenerationExportConfig{Protocol: agento11y.GenerationExportProtocolNone},
	})
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })

	completed := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	gen := agento11y.Generation{
		ID:             "vibe-x",
		ConversationID: "sess",
		AgentName:      "mistral-vibe",
		Model:          agento11y.ModelRef{Provider: "mistral", Name: "mistral-medium-3.5"},
		CompletedAt:    completed,
		Output: []agento11y.Message{{
			Role: agento11y.RoleAssistant,
			Parts: []agento11y.Part{
				agento11y.ToolCallPart(agento11y.ToolCall{ID: "tc1", Name: "bash", InputJSON: []byte(`{"command":"ls"}`)}),
				agento11y.ToolCallPart(agento11y.ToolCall{ID: "tc2", Name: "read", InputJSON: []byte(`{"file":"x"}`)}),
				agento11y.ToolCallPart(agento11y.ToolCall{ID: "tc3", Name: "grep", InputJSON: []byte(`{"pattern":"y"}`)}),
			},
		}},
		Input: []agento11y.Message{{
			Role:  agento11y.RoleTool,
			Parts: []agento11y.Part{agento11y.ToolResultPart(agento11y.ToolResult{ToolCallID: "tc1", Name: "bash", Content: "ok"})},
		}},
	}
	events := map[string]toolevents.Event{
		"tc1": {ToolCallID: "tc1", Status: "success", DurationMs: 1500, CompletedAt: completed},
		"tc2": {ToolCallID: "tc2", Status: "failure", Error: "boom", DurationMs: 200, CompletedAt: completed},
		// tc3 intentionally has no post_tool event -> synthetic timing.
	}

	genCtx, rec := client.StartGeneration(context.Background(), agento11y.GenerationStart{
		ID:             gen.ID,
		ConversationID: gen.ConversationID,
		AgentName:      gen.AgentName,
		Model:          gen.Model,
		ContentCapture: agento11y.ContentCaptureModeFull,
	})
	rec.SetResult(gen, nil)
	emitToolSpans(genCtx, client, gen, agento11y.ContentCaptureModeFull, events, logger)
	rec.End()

	_ = client.Shutdown(context.Background())
	_ = tp.Shutdown(context.Background())

	toolSpans := map[string]sdktrace.ReadOnlySpan{}
	for _, s := range recorder.Ended() {
		if name, ok := strings.CutPrefix(s.Name(), "execute_tool "); ok {
			toolSpans[name] = s
		}
	}
	if len(toolSpans) != 3 {
		t.Fatalf("got %d execute_tool spans, want 3 (bash, read, grep)", len(toolSpans))
	}

	bash, ok := toolSpans["bash"]
	if !ok {
		t.Fatal("missing execute_tool bash span")
	}
	if d := bash.EndTime().Sub(bash.StartTime()); d != 1500*time.Millisecond {
		t.Errorf("bash span duration = %v, want 1.5s from post_tool duration_ms", d)
	}
	if bash.Status().Code == codes.Error {
		t.Error("bash span marked error despite success status")
	}

	read, ok := toolSpans["read"]
	if !ok {
		t.Fatal("missing execute_tool read span")
	}
	if read.Status().Code != codes.Error {
		t.Errorf("read span status = %v, want error from post_tool failure", read.Status().Code)
	}

	grep, ok := toolSpans["grep"]
	if !ok {
		t.Fatal("missing execute_tool grep span")
	}
	if d := grep.EndTime().Sub(grep.StartTime()); d != 0 {
		t.Errorf("grep span duration = %v, want 0 (synthetic timing without a post_tool event)", d)
	}
}

func TestEmitToolSpansSkillEvidence(t *testing.T) {
	for _, mode := range []agento11y.ContentCaptureMode{
		agento11y.ContentCaptureModeFull,
		agento11y.ContentCaptureModeNoToolContent,
		agento11y.ContentCaptureModeMetadataOnly,
		agento11y.ContentCaptureModeFullWithMetadataSpans,
	} {
		for _, tc := range []struct {
			name, callName, callID, eventKey, eventID, eventTool, stored, status, wantSkill string
			missing, blankEventTool                                                         bool
		}{
			{name: "success", stored: "review", wantSkill: "review"},
			{name: "failure retains attempted skill", stored: "review", status: "failure", wantSkill: "review"},
			{name: "cancellation retains attempted skill", stored: "review", status: "cancelled", wantSkill: "review"},
			{name: "stored identity is normalized again", stored: "  team:review  ", wantSkill: "team:review"},
			{name: "missing post evidence", missing: true},
			{name: "synthetic slash transcript missing post", callID: "synthetic-slash-review", missing: true},
			{name: "event map key mismatch", eventKey: "other", stored: "review"},
			{name: "event call ID mismatch", eventID: "other", stored: "review"},
			{name: "event tool mismatch", eventTool: "bash", stored: "review"},
			{name: "legacy event lacks tool name", stored: "review", blankEventTool: true},
			{name: "event tool case mismatch", eventTool: "Skill", stored: "review"},
			{name: "event tool whitespace mismatch", eventTool: " skill ", stored: "review"},
			{name: "call tool mismatch", callName: "bash", stored: "review"},
			{name: "call tool case mismatch", callName: "Skill", stored: "review"},
			{name: "unsafe stored path", stored: "/private/skills/review/SKILL.md"},
			{name: "unsafe stored URL", stored: "https://example.invalid/review"},
			{name: "unsafe stored control", stored: "review\n"},
			{name: "unsafe stored secret", stored: "ghp_" + strings.Repeat("a", 36)},
			{name: "oversized stored name", stored: strings.Repeat("a", 257)},
			{name: "empty stored name"},
		} {
			t.Run(mode.String()+"/"+tc.name, func(t *testing.T) {
				envconfig.PinAliasEnvBlank(t)
				t.Setenv("HOME", t.TempDir())
				recorder := tracetest.NewSpanRecorder()
				tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
				logger := log.New(io.Discard, "", 0)
				client := agento11y.NewClient(agento11y.Config{
					ContentCapture: mode, Tracer: tp.Tracer("test"), Logger: logger,
					GenerationExport: agento11y.GenerationExportConfig{Protocol: agento11y.GenerationExportProtocolNone},
				})
				t.Cleanup(func() {
					_ = client.Shutdown(context.Background())
					_ = tp.Shutdown(context.Background())
				})
				callName, callID := tc.callName, tc.callID
				if callName == "" {
					callName = "skill"
				}
				if callID == "" {
					callID = "call-review"
				}
				key, eventID, eventTool, status := tc.eventKey, tc.eventID, tc.eventTool, tc.status
				if key == "" {
					key = callID
				}
				if eventID == "" {
					eventID = callID
				}
				if eventTool == "" && !tc.blankEventTool {
					eventTool = "skill"
				}
				if status == "" {
					status = "success"
				}
				completed := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
				event := toolevents.Event{ToolCallID: eventID, ToolName: eventTool, SkillName: tc.stored,
					Status: status, DurationMs: 250, CompletedAt: completed.Add(-time.Second)}
				events := map[string]toolevents.Event{
					"orphan": {ToolCallID: "orphan", ToolName: "skill", SkillName: "orphan"},
				}
				if !tc.missing {
					events[key] = event
				}
				gen := agento11y.Generation{
					ID: "vibe-skill", ConversationID: "session", AgentName: "mistral-vibe",
					Model: agento11y.ModelRef{Provider: "mistral", Name: "test-model"}, CompletedAt: completed,
					Output: []agento11y.Message{{Role: agento11y.RoleAssistant, Parts: []agento11y.Part{
						agento11y.ToolCallPart(agento11y.ToolCall{ID: callID, Name: callName,
							InputJSON: []byte(`{"name":"review","private":"private-skill-input"}`)}),
					}}},
					Input: []agento11y.Message{
						{Role: agento11y.RoleUser, Parts: []agento11y.Part{agento11y.TextPart("/review")}},
						{Role: agento11y.RoleTool, Parts: []agento11y.Part{agento11y.ToolResultPart(agento11y.ToolResult{
							ToolCallID: callID, Name: callName, Content: "private-skill-result"})}},
					},
				}
				ctx, rec := client.StartGeneration(context.Background(), agento11y.GenerationStart{
					ID: gen.ID, ConversationID: gen.ConversationID, AgentName: gen.AgentName, Model: gen.Model, ContentCapture: mode,
				})
				rec.SetResult(gen, nil)
				emitToolSpans(ctx, client, gen, mode, events, logger)
				rec.End()
				require.NoError(t, rec.Err())
				require.NoError(t, client.Shutdown(context.Background()))
				require.NoError(t, tp.Shutdown(context.Background()))
				spans := recorder.Ended()
				require.Len(t, spans, 2, "one generation plus exactly one tool execution")
				var tool, parent sdktrace.ReadOnlySpan
				for _, span := range spans {
					if strings.HasPrefix(span.Name(), "execute_tool ") {
						tool = span
					} else {
						parent = span
					}
				}
				require.NotNil(t, tool)
				require.NotNil(t, parent)
				require.Equal(t, parent.SpanContext().TraceID(), tool.SpanContext().TraceID())
				require.Equal(t, parent.SpanContext().SpanID(), tool.Parent().SpanID())
				require.NotEqual(t, parent.SpanContext().SpanID(), tool.SpanContext().SpanID())
				attrs := map[string]string{}
				for _, a := range tool.Attributes() {
					attrs[string(a.Key)] = a.Value.AsString()
				}
				wantName := "execute_tool " + callName
				if tc.wantSkill != "" {
					wantName += " " + tc.wantSkill
					require.Equal(t, tc.wantSkill, attrs["gen_ai.skill.name"])
				} else {
					require.NotContains(t, attrs, "gen_ai.skill.name")
				}
				require.Equal(t, wantName, tool.Name())
				require.Equal(t, "execute_tool", attrs["gen_ai.operation.name"])
				require.Equal(t, callName, attrs["gen_ai.tool.name"])
				require.Equal(t, callID, attrs["gen_ai.tool.call.id"])
				require.Equal(t, "function", attrs["gen_ai.tool.type"])
				for _, key := range []string{"agento11y.skill.name", "gen_ai.skill.description", "gen_ai.skill.source.uri", "gen_ai.skill.resource.name"} {
					require.NotContains(t, attrs, key)
				}
				wantEnd, wantDuration := completed, time.Duration(0)
				if !tc.missing && key == callID {
					wantEnd, wantDuration = event.CompletedAt, 250*time.Millisecond
				}
				require.Equal(t, wantEnd, tool.EndTime())
				require.Equal(t, wantEnd.Add(-wantDuration), tool.StartTime())
				require.Equal(t, status == "failure" || status == "cancelled", tool.Status().Code == codes.Error)
				if mode == agento11y.ContentCaptureModeFull {
					require.Contains(t, attrs["gen_ai.tool.call.arguments"], "private-skill-input")
					require.JSONEq(t, `"private-skill-result"`, attrs["gen_ai.tool.call.result"])
				} else {
					require.NotContains(t, attrs, "gen_ai.tool.call.arguments")
					require.NotContains(t, attrs, "gen_ai.tool.call.result")
					for _, span := range spans {
						for _, a := range span.Attributes() {
							require.NotContains(t, a.Value.Emit(), "private-skill-input")
							require.NotContains(t, a.Value.Emit(), "private-skill-result")
						}
					}
				}
			})
		}
	}
}
