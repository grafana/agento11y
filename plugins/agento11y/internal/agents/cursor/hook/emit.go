package hook

import (
	"context"
	"log"

	"github.com/grafana/agento11y/go/agento11y"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/config"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/fragment"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/mapper"
	"github.com/grafana/agento11y/plugins/agento11y/internal/autotag"
	"github.com/grafana/agento11y/plugins/agento11y/internal/emit"
	"github.com/grafana/agento11y/plugins/agento11y/internal/otel"
	"github.com/grafana/agento11y/plugins/agento11y/internal/redact"
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
// pieces the SDK can't infer: the export protocol, the
// `/api/v1/generations:export` path suffix, basic-auth mode, and the OTel
// tracer/meter wiring.
//
// The hook hands its own logger to the SDK. That logger writes to the plugin
// log file; without it the SDK falls back to log.Default(), which writes to
// the stderr the host agent reads.
//
// The auto-tag inputs are the first workspace root and the signed-in user's
// email, the same values mapper.MapFragment reads for git.branch and the
// generation user id. Cursor sends both on sessionStart, and the stop and
// sessionEnd payloads that emit telemetry carry neither, so the session
// sessionStart saved is the primary source and the live payload is the
// fallback for a conversation whose session file was never written.
func buildClient(p Payload, session *fragment.Session, cfg config.Config, providers *otel.Providers, logger *log.Logger) *agento11y.Client {
	return emit.NewClient(emit.ClientOptions{
		InstrumentationName: otelInstrumentationName,
		ContentCapture:      cfg.ContentCapture,
		Logger:              logger,
		Providers:           providers,
		UserAgent:           useragent.For("cursor"),
		Tags: autotag.FromEnv(autotag.Inputs{
			Cwd:    autoTagWorkspaceRoot(p, session),
			UserID: autoTagUserEmail(p, session),
		}, logger),
	})
}

func autoTagWorkspaceRoot(p Payload, session *fragment.Session) string {
	if session != nil {
		if root := firstWorkspaceRoot(session.WorkspaceRoots); root != "" {
			return root
		}
	}
	return firstWorkspaceRoot(p.WorkspaceRoots)
}

func autoTagUserEmail(p Payload, session *fragment.Session) string {
	if session != nil && session.UserEmail != "" {
		return session.UserEmail
	}
	return p.UserEmail
}

func firstWorkspaceRoot(roots []string) string {
	for _, root := range roots {
		if root != "" {
			return root
		}
	}
	return ""
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
// Tool argument/result content goes through the shared redactor before it
// reaches the span, mirroring codex. This is a second boundary: the mapper
// redacts the generation export, and the span carries its own copy of the
// same bytes. t.ErrorMessage needs no pass here — postToolUse already
// redacted it, the same split codex uses. Capture-mode clamping happens at
// the fragment-write boundary (postToolUse drops bytes for any mode other
// than `full`), so by the time we emit, t.ToolInput/Output and t.ErrorMessage
// are already empty in metadata_only / no_tool_content. The SDK additionally
// honors Generation.ContentCapture when serializing the span.
func emitToolSpans(ctx context.Context, client *agento11y.Client, frag *fragment.Fragment, gen agento11y.Generation, logger *log.Logger) {
	red := redact.New()
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
		end.Arguments = red.ToolPayloadText(t.ToolInput)
		end.Result = red.ToolPayloadText(t.ToolOutput)
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
