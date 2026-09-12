package claudeevals

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/emit"
	pluginotel "github.com/grafana/agento11y/plugins/agento11y/internal/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Import IDs must survive retries: an already accepted generation must never
// end up linked to a fresh, unrelated trace on the next upload attempt.
type spanIdentityKey struct{}
type importIDs struct{}

func spanID(key string) trace.SpanID {
	h := sha256.Sum256([]byte("span:" + key))
	return trace.SpanID(h[:8])
}
func traceID(key string) trace.TraceID {
	h := sha256.Sum256([]byte("trace:" + key))
	return trace.TraceID(h[:16])
}
func (importIDs) NewIDs(ctx context.Context) (trace.TraceID, trace.SpanID) {
	if key, ok := ctx.Value(spanIdentityKey{}).(string); ok {
		return traceID(key), spanID(key)
	}
	var t trace.TraceID
	var s trace.SpanID
	_, _ = rand.Read(t[:])
	_, _ = rand.Read(s[:])
	return t, s
}
func (importIDs) NewSpanID(ctx context.Context, _ trace.TraceID) trace.SpanID {
	if key, ok := ctx.Value(spanIdentityKey{}).(string); ok {
		return spanID(key)
	}
	var s trace.SpanID
	_, _ = rand.Read(s[:])
	return s
}

// Telemetry reuses the coding-agent SDK/OTLP configuration. Setup and flush
// failures are fatal for a trajectory import, not silent loss of tracing.
type Telemetry struct {
	client    *agento11y.Client
	providers *pluginotel.Providers
	tracer    trace.Tracer
}

func NewTelemetry(ctx context.Context) (*Telemetry, error) {
	providers, err := pluginotel.SetupWithOptions(ctx, "", pluginotel.Options{IDGenerator: importIDs{}})
	if err != nil {
		return nil, err
	}
	if providers == nil {
		return nil, errors.New("trajectory import requires AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT")
	}
	return &Telemetry{
		providers: providers, tracer: providers.Tracer("agento11y.claude-evals"),
		client: emit.NewClient(emit.ClientOptions{InstrumentationName: "agento11y.claude-evals", Providers: providers, ContentCapture: agento11y.ContentCaptureModeMetadataOnly}),
	}, nil
}

func (t *Telemetry) Shutdown(ctx context.Context) error {
	if t == nil {
		return nil
	}
	return errors.Join(t.client.Shutdown(ctx), t.providers.Shutdown(ctx))
}

func (t *Telemetry) export(ctx context.Context, trajectory *Trajectory, includeContent bool) error {
	if t == nil {
		return errors.New("trajectory import requires an initialized OTLP exporter")
	}
	g := trajectory.Generation
	mode := agento11y.ContentCaptureModeMetadataOnly
	if includeContent {
		mode = agento11y.ContentCaptureModeFull
	}
	// Remove any ambient parent span; this trace represents the recorded eval,
	// not the exporter process. Preserve cancellation/deadlines.
	ctx = trace.ContextWithSpanContext(ctx, trace.SpanContext{})
	ctx = context.WithValue(ctx, spanIdentityKey{}, g.ID)
	genCtx, rec := t.client.StartGeneration(ctx, agento11y.GenerationStart{
		ID: g.ID, ConversationID: g.ConversationID, ConversationTitle: g.ConversationTitle,
		AgentName: g.AgentName, AgentVersion: g.AgentVersion, OperationName: g.OperationName,
		Model: g.Model, Mode: g.Mode, StartedAt: g.StartedAt, Tags: g.Tags, Metadata: g.Metadata, ContentCapture: mode,
	})
	actual := trace.SpanContextFromContext(genCtx)
	if actual.TraceID().String() != g.TraceID || actual.SpanID().String() != g.SpanID {
		rec.SetCallError(errors.New("trace identity mismatch"))
		rec.End()
		return errors.New("trace identity mismatch; refusing to attach an unrelated trace")
	}
	rootSpan := trace.SpanFromContext(genCtx)
	rootSpan.SetAttributes(correlation(g)...)
	toolContexts := map[string]context.Context{}
	// Open tools first so streamed subagent/model events can nest underneath
	// their actual parent_tool_use_id even if their result arrived much later.
	for _, activity := range trajectory.Activities {
		if activity.Kind != "execute_tool" {
			continue
		}
		parent := genCtx
		if parentCtx, ok := toolContexts[activity.ParentToolID]; ok {
			parent = parentCtx
		}
		parent = context.WithValue(parent, spanIdentityKey{}, g.ID+":"+activity.Key)
		toolCtx, tool := t.client.StartToolExecution(parent, agento11y.ToolExecutionStart{
			ToolName: activity.Name, ToolCallID: activity.Key, ToolType: "function",
			ConversationID: g.ConversationID, AgentName: g.AgentName, AgentVersion: g.AgentVersion,
			RequestModel: activity.Model, RequestProvider: g.Model.Provider, StartedAt: activity.StartedAt, ContentCapture: mode,
		})
		toolContexts[activity.Key] = toolCtx
		trace.SpanFromContext(toolCtx).SetAttributes(correlation(g)...)
		end := agento11y.ToolExecutionEnd{CompletedAt: activity.CompletedAt}
		if includeContent {
			end.Arguments = jsonValue(activity.Arguments)
			end.Result = jsonValue(activity.Result)
		}
		tool.SetResult(end)
		if activity.Error {
			tool.SetExecError(errors.New("tool returned an error"))
		}
		tool.End()
		if err := tool.Err(); err != nil {
			rec.SetCallError(err)
			rec.End()
			return err
		}
	}
	for _, activity := range trajectory.Activities {
		if activity.Kind != "chat" {
			continue
		}
		parent := genCtx
		if parentCtx, ok := toolContexts[activity.ParentToolID]; ok {
			parent = parentCtx
		}
		parent = context.WithValue(parent, spanIdentityKey{}, g.ID+":"+activity.Key)
		_, span := t.tracer.Start(parent, "chat "+activity.Model, trace.WithTimestamp(activity.StartedAt), trace.WithSpanKind(trace.SpanKindClient))
		span.SetAttributes(correlation(g)...)
		span.SetAttributes(attribute.String("gen_ai.operation.name", "chat"), attribute.String("gen_ai.provider.name", g.Model.Provider), attribute.String("gen_ai.request.model", activity.Model), attribute.Bool("agento11y.import.per_call_usage_unavailable", true))
		span.End(trace.WithTimestamp(activity.CompletedAt))
	}
	rec.SetResult(g, nil)
	if g.CallError != "" {
		rec.SetCallError(errors.New(g.CallError))
		rootSpan.SetStatus(codes.Error, "Claude agent invocation failed")
	}
	rec.End()
	if err := rec.Err(); err != nil {
		return err
	}
	if err := t.client.Flush(ctx); err != nil {
		return fmt.Errorf("flush conversation: %w", err)
	}
	if err := t.providers.ForceFlush(); err != nil {
		return fmt.Errorf("flush trace: %w", err)
	}
	return nil
}

func correlation(g agento11y.Generation) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("gen_ai.conversation.id", g.ConversationID),
		attribute.String("experiment_id", g.Tags["experiment_id"]),
		attribute.String("test_case_id", g.Tags["test_case_id"]),
		attribute.String("trial_id", g.Tags["trial_id"]),
		attribute.String("agento11y.import.timing_source", "transcript_bounds"),
	}
}

func jsonValue(raw json.RawMessage) any {
	var v any
	if len(raw) != 0 {
		_ = json.Unmarshal(raw, &v)
	}
	return v
}
