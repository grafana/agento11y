package hook

import (
	"context"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/grafana/agento11y/go/sigil"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/vibe/toolevents"
)

// TestEmitToolSpans asserts that each assistant tool call becomes one
// execute_tool span, that an after_tool event drives real duration and an
// error status, and that a call with no event still produces a span with
// synthetic (zero-duration) timing.
func TestEmitToolSpans(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	logger := log.New(io.Discard, "", 0)
	client := sigil.NewClient(sigil.Config{
		ContentCapture: sigil.ContentCaptureModeFull,
		Tracer:         tp.Tracer("test"),
		Logger:         logger,
	})
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })

	completed := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	gen := sigil.Generation{
		ID:             "vibe-x",
		ConversationID: "sess",
		AgentName:      "mistral-vibe",
		Model:          sigil.ModelRef{Provider: "mistral", Name: "mistral-medium-3.5"},
		CompletedAt:    completed,
		Output: []sigil.Message{{
			Role: sigil.RoleAssistant,
			Parts: []sigil.Part{
				sigil.ToolCallPart(sigil.ToolCall{ID: "tc1", Name: "bash", InputJSON: []byte(`{"command":"ls"}`)}),
				sigil.ToolCallPart(sigil.ToolCall{ID: "tc2", Name: "read", InputJSON: []byte(`{"file":"x"}`)}),
				sigil.ToolCallPart(sigil.ToolCall{ID: "tc3", Name: "grep", InputJSON: []byte(`{"pattern":"y"}`)}),
			},
		}},
		Input: []sigil.Message{{
			Role:  sigil.RoleTool,
			Parts: []sigil.Part{sigil.ToolResultPart(sigil.ToolResult{ToolCallID: "tc1", Name: "bash", Content: "ok"})},
		}},
	}
	events := map[string]toolevents.Event{
		"tc1": {ToolCallID: "tc1", Status: "success", DurationMs: 1500, CompletedAt: completed},
		"tc2": {ToolCallID: "tc2", Status: "failure", Error: "boom", DurationMs: 200, CompletedAt: completed},
		// tc3 intentionally has no after_tool event -> synthetic timing.
	}

	genCtx, rec := client.StartGeneration(context.Background(), sigil.GenerationStart{
		ID:             gen.ID,
		ConversationID: gen.ConversationID,
		AgentName:      gen.AgentName,
		Model:          gen.Model,
		ContentCapture: sigil.ContentCaptureModeFull,
	})
	rec.SetResult(gen, nil)
	emitToolSpans(genCtx, client, gen, sigil.ContentCaptureModeFull, events, logger)
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
		t.Errorf("bash span duration = %v, want 1.5s from after_tool duration_ms", d)
	}
	if bash.Status().Code == codes.Error {
		t.Error("bash span marked error despite success status")
	}

	read, ok := toolSpans["read"]
	if !ok {
		t.Fatal("missing execute_tool read span")
	}
	if read.Status().Code != codes.Error {
		t.Errorf("read span status = %v, want error from after_tool failure", read.Status().Code)
	}

	grep, ok := toolSpans["grep"]
	if !ok {
		t.Fatal("missing execute_tool grep span")
	}
	if d := grep.EndTime().Sub(grep.StartTime()); d != 0 {
		t.Errorf("grep span duration = %v, want 0 (synthetic timing without an after_tool event)", d)
	}
}
