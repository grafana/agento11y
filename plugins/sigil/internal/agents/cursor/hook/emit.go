package hook

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/grafana/sigil-sdk/go/sigil"

	"github.com/grafana/sigil-sdk/plugins/sigil/internal/agents/cursor/config"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/agents/cursor/fragment"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/agents/cursor/mapper"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/otel"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/useragent"
)

// otelInstrumentationName is the OTel instrumentation scope name attached
// to every span and metric this agent emits. Renamed from "sigil-cursor"
// when the three agent plugins consolidated into one binary; dashboards
// that previously filtered on "sigil-cursor" need to update to
// "sigil.cursor".
const otelInstrumentationName = "sigil.cursor"

// setupOTelIfConfigured builds OTel providers when an OTLP endpoint is set
// (SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT or OTEL_EXPORTER_OTLP_ENDPOINT). The OTel
// SDK reads transport env vars (endpoint, headers, insecure, protocol)
// natively; the plugin only provides convenience auth-header injection from
// SIGIL_AUTH_*.
func setupOTelIfConfigured(ctx context.Context, instanceID string, logger *log.Logger) *otel.Providers {
	endpoint := otel.EndpointFromEnv()
	if endpoint == "" {
		return nil
	}
	providers, err := otel.Setup(ctx, instanceID)
	if err != nil {
		logger.Printf("otel: setup: %v", err)
		return nil
	}
	if providers != nil {
		logger.Printf("otel: endpoint=%s", endpoint)
	}
	return providers
}

// buildClient constructs the Sigil client. Endpoint, tenant ID, and token
// come from the SDK's automatic SIGIL_* env resolution (config.ApplyEnv has
// already injected dotenv values into the OS env). The plugin only owns the
// pieces the SDK can't infer: HTTP protocol, the `/api/v1/generations:export`
// path suffix, basic-auth mode, and the OTel tracer/meter wiring.
func exportConfig() sigil.GenerationExportConfig {
	return sigil.GenerationExportConfig{
		Protocol: sigil.GenerationExportProtocolHTTP,
		Endpoint: strings.TrimRight(os.Getenv("SIGIL_ENDPOINT"), "/") + "/api/v1/generations:export",
		Headers:  map[string]string{"User-Agent": useragent.For("cursor")},
		Auth:     sigil.AuthConfig{Mode: sigil.ExportAuthModeBasic},
	}
}

func buildClient(cfg config.Config, providers *otel.Providers) *sigil.Client {
	c := sigil.Config{
		ContentCapture:   cfg.ContentCapture,
		GenerationExport: exportConfig(),
	}
	if providers != nil {
		c.Tracer = providers.Tracer(otelInstrumentationName)
		c.Meter = providers.Meter(otelInstrumentationName)
	}
	return sigil.NewClient(c)
}

// emitGeneration pushes one mapped Generation through the SDK: starts the
// generation span, sets the result, sets a call error if the stop status was
// "error", emits per-tool execute_tool spans, then ends the recorder.
//
// Flushing/shutdown is the caller's responsibility — sessionEnd batches
// multiple generations through one client.
func emitGeneration(ctx context.Context, client *sigil.Client, frag *fragment.Fragment, mapped mapper.Mapped, logger *log.Logger) error {
	genCtx, rec := client.StartGeneration(ctx, mapped.Start)
	rec.SetResult(mapped.Generation, nil)
	if mapped.CallError != nil {
		rec.SetCallError(mapped.CallError)
	}
	emitToolSpans(genCtx, client, frag, mapped.Generation, logger)
	rec.End()
	if err := rec.Err(); err != nil {
		return fmt.Errorf("recorder: %w", err)
	}
	return nil
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
func emitToolSpans(ctx context.Context, client *sigil.Client, frag *fragment.Fragment, gen sigil.Generation, logger *log.Logger) {
	for i := range frag.Tools {
		t := &frag.Tools[i]
		if t.ToolName == "" {
			continue
		}
		startedAt, completedAt := toolSpanWindow(*t, gen.CompletedAt)
		_, toolRec := client.StartToolExecution(ctx, sigil.ToolExecutionStart{
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

		end := sigil.ToolExecutionEnd{CompletedAt: completedAt}
		if len(t.ToolInput) > 0 {
			end.Arguments = string(t.ToolInput)
		}
		if len(t.ToolOutput) > 0 {
			end.Result = string(t.ToolOutput)
		}
		if t.Status == "error" {
			toolRec.SetExecError(toolErrorOr(t.ErrorMessage))
		}
		toolRec.SetResult(end)
		toolRec.End()
		if err := toolRec.Err(); err != nil {
			logger.Printf("tool span enqueue: %v", err)
		}
	}
}

// toolSpanWindow returns the (startedAt, completedAt) wall-clock window for a
// tool span. completedAt comes from the tool's own postToolUse timestamp so
// spans land in real order on the timeline; startedAt subtracts the reported
// duration when available. Both fall back to genCompletedAt when the
// per-tool timestamp is missing or unparseable.
func toolSpanWindow(t fragment.ToolRecord, genCompletedAt time.Time) (startedAt, completedAt time.Time) {
	completedAt = parseToolTimestamp(t.CompletedAt, genCompletedAt)
	startedAt = completedAt
	if t.DurationMs != nil && !completedAt.IsZero() {
		startedAt = completedAt.Add(-time.Duration(*t.DurationMs) * time.Millisecond)
	}
	return startedAt, completedAt
}

// parseToolTimestamp parses an ISO-8601 timestamp recorded in a ToolRecord,
// falling back to def when empty or unparseable.
func parseToolTimestamp(s string, def time.Time) time.Time {
	if s == "" {
		return def
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return def
}

// toolErrorOr wraps an error message string into an error value, with a
// generic sentinel when the message is empty.
func toolErrorOr(msg string) error {
	if msg == "" {
		return errToolError
	}
	return errors.New(msg)
}

var errToolError = errors.New("tool returned error")
