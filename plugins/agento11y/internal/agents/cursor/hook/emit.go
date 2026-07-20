package hook

import (
	"context"
	"log"

	"github.com/grafana/agento11y/go/agento11y"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/config"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/fragment"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/mapper"
	"github.com/grafana/agento11y/plugins/agento11y/internal/emit"
	"github.com/grafana/agento11y/plugins/agento11y/internal/otel"
	"github.com/grafana/agento11y/plugins/agento11y/internal/useragent"
)

// otelInstrumentationName is the OTel instrumentation scope name attached
// to every span and metric this agent emits. Renamed from "sigil-cursor"
// when the three agent plugins consolidated into one binary; dashboards
// that previously filtered on "sigil-cursor" need to update to
// "agento11y.cursor".
const otelInstrumentationName = "agento11y.cursor"

// setupOTelIfConfigured builds OTel providers when an OTLP endpoint is set
// (SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT or OTEL_EXPORTER_OTLP_ENDPOINT). The OTel
// SDK reads transport env vars (endpoint, headers, insecure, protocol)
// natively; the plugin only provides convenience auth-header injection from
// SIGIL_AUTH_*.
func setupOTelIfConfigured(ctx context.Context, instanceID string, logger *log.Logger) *otel.Providers {
	return emit.SetupOTel(ctx, instanceID, logger)
}

// buildClient constructs the agento11y client. Endpoint, tenant ID, and token
// come from the SDK's automatic SIGIL_* env resolution (config.ApplyEnv has
// already injected dotenv values into the OS env). The plugin only owns the
// pieces the SDK can't infer: HTTP protocol, the `/api/v1/generations:export`
// path suffix, basic-auth mode, and the OTel tracer/meter wiring. cursor does
// not pass a logger, so the SDK client stays silent.
func buildClient(cfg config.Config, providers *otel.Providers) *agento11y.Client {
	return emit.NewClient(emit.ClientOptions{
		InstrumentationName: otelInstrumentationName,
		ContentCapture:      cfg.ContentCapture,
		Providers:           providers,
		UserAgent:           useragent.For("cursor"),
	})
}

// emitGeneration pushes one mapped Generation through the SDK: starts the
// generation span, sets the result, sets a call error if the stop status was
// "error", emits per-tool execute_tool spans, then ends the recorder.
//
// Flushing/shutdown is the caller's responsibility — sessionEnd batches
// multiple generations through one client.
func emitGeneration(ctx context.Context, client *agento11y.Client, frag *fragment.Fragment, mapped mapper.Mapped, logger *log.Logger) error {
	return emit.Record(ctx, client, mapped.Start, mapped.Generation, mapped.CallError, func(genCtx context.Context) {
		emitToolSpans(genCtx, client, frag, mapped.Generation, logger)
	})
}

// emitToolSpans creates one execute_tool span per tool invocation in the
// fragment. Each span is anchored at the tool's own postToolUse timestamp so
// spans interleave on the generation timeline in wall-clock order (CALL→TOOL
// →CALL→TOOL) rather than collapsing onto the generation's completed_at.
//
// Tool argument/result content is forwarded as-is. Capture-mode clamping
// happens at the fragment-write boundary (postToolUse drops bytes for any
// mode other than `full`), so by the time we emit, t.ToolInput/Output are
// already empty in metadata_only / no_tool_content. The SDK additionally
// honors Generation.ContentCapture when serializing the span.
func emitToolSpans(ctx context.Context, client *agento11y.Client, frag *fragment.Fragment, gen agento11y.Generation, logger *log.Logger) {
	for i := range frag.Tools {
		t := &frag.Tools[i]
		if t.ToolName == "" {
			continue
		}
		startedAt, completedAt := emit.ToolSpanWindow(t.CompletedAt, t.DurationMs, gen.CompletedAt)
		_, toolRec := client.StartToolExecution(ctx, agento11y.ToolExecutionStart{
			ToolName:        t.ToolName,
			ToolCallID:      t.ToolUseID,
			ToolType:        "function",
			ConversationID:  gen.ConversationID,
			AgentName:       gen.AgentName,
			AgentVersion:    gen.AgentVersion,
			RequestModel:    gen.Model.Name,
			RequestProvider: gen.Model.Provider,
			StartedAt:       startedAt,
		})

		end := agento11y.ToolExecutionEnd{CompletedAt: completedAt}
		if len(t.ToolInput) > 0 {
			end.Arguments = string(t.ToolInput)
		}
		if len(t.ToolOutput) > 0 {
			end.Result = string(t.ToolOutput)
		}
		if t.Status == "error" {
			toolRec.SetExecError(emit.ToolError(t.ErrorMessage))
		}
		toolRec.SetResult(end)
		toolRec.End()
		if err := toolRec.Err(); err != nil {
			logger.Printf("tool span enqueue: %v", err)
		}
	}
}
