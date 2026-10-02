package kiro

import (
	"context"
	"fmt"
	"log"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/tags"
	"github.com/grafana/agento11y/plugins/agento11y/internal/autotag"
	"github.com/grafana/agento11y/plugins/agento11y/internal/dotenv"
	"github.com/grafana/agento11y/plugins/agento11y/internal/emit"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/grafana/agento11y/plugins/agento11y/internal/mapperutil"
	"github.com/grafana/agento11y/plugins/agento11y/internal/redact"
	"github.com/grafana/agento11y/plugins/agento11y/internal/useragent"
)

func mapTurn(sessionID string, t turn, mode agento11y.ContentCaptureMode) (agento11y.GenerationStart, agento11y.Generation) {
	clamp(&t, mode)
	metadata := map[string]any{
		"kiro.capture_source":           "hooks",
		"kiro.assistant_text_available": false,
		"kiro.usage_available":          false,
		"kiro.model_reported":           false,
		"kiro.tool_timing_available":    false,
		"kiro.tool_count":               len(t.Tools),
	}
	version := envconfig.ResolveAgentVersion("")
	tagMap := tags.Build(tags.BuiltinInputs{Entrypoint: "kiro", Cwd: t.CWD})
	start := agento11y.GenerationStart{
		ID: t.ID, ConversationID: sessionID, AgentName: envconfig.ResolveAgentName("kiro"),
		Model: agento11y.ModelRef{Provider: "kiro", Name: "unknown"},
		Mode:  agento11y.GenerationModeSync, OperationName: "generateText",
		ContentCapture: mode, StartedAt: t.StartedAt, Metadata: metadata,
		AgentVersion: version, EffectiveVersion: version, Tags: mapperutil.Clone(tagMap),
	}
	gen := agento11y.Generation{
		ID: start.ID, ConversationID: sessionID, AgentName: start.AgentName,
		Model: start.Model, Mode: start.Mode, OperationName: start.OperationName,
		StartedAt: t.StartedAt, CompletedAt: t.CompletedAt, Metadata: metadata,
		AgentVersion: version, EffectiveVersion: version, Tags: mapperutil.Clone(tagMap),
	}
	if envconfig.ResolveRedactInput(nil) {
		t.Prompt = redact.New().Prompt(t.Prompt)
	}
	if t.Prompt != "" {
		gen.Input = append(gen.Input, agento11y.UserTextMessage(t.Prompt))
	}
	for i, rec := range t.Tools {
		id := fmt.Sprintf("%s-tool-%d", t.ID, i+1)
		gen.Output = append(gen.Output, agento11y.Message{Role: agento11y.RoleAssistant, Parts: []agento11y.Part{agento11y.ToolCallPart(agento11y.ToolCall{ID: id, Name: rec.Name, InputJSON: mapperutil.BoundJSON(rec.Input, mapperutil.MaxToolInputBytes)})}})
		if mapperutil.NormalizePayloadContentMode(mode) != agento11y.ContentCaptureModeMetadataOnly {
			gen.Input = append(gen.Input, agento11y.Message{Role: agento11y.RoleTool, Parts: []agento11y.Part{agento11y.ToolResultPart(agento11y.ToolResult{ToolCallID: id, Name: rec.Name, ContentJSON: mapperutil.BoundJSON(rec.Response, mapperutil.MaxToolResultBytes)})}})
		}
	}
	return start, gen
}

func sendTurn(ctx context.Context, sessionID string, t turn, mode agento11y.ContentCaptureMode, logger *log.Logger) error {
	envconfig.ApplyLocalAuthPlaceholders()
	if !dotenv.HasCredentials() {
		return fmt.Errorf("capture not configured; run agento11y login")
	}
	providers := emit.SetupOTel(ctx, sessionID, logger)
	if providers != nil {
		defer func() { _ = providers.Shutdown(ctx) }()
	}
	client := emit.NewClient(emit.ClientOptions{
		InstrumentationName: "agento11y.kiro", ContentCapture: mode, Logger: logger,
		Providers: providers, UserAgent: useragent.For("kiro"),
		Tags: autotag.FromEnv(autotag.Inputs{Cwd: t.CWD}, logger),
	})
	defer func() { _ = client.Shutdown(ctx) }()
	start, gen := mapTurn(sessionID, t, mode)
	if err := emit.Record(ctx, client, start, gen, nil, func(genCtx context.Context) {
		for i, rec := range t.Tools {
			_, span := client.StartToolExecution(genCtx, agento11y.ToolExecutionStart{
				ToolName: rec.Name, ToolCallID: fmt.Sprintf("%s-tool-%d", t.ID, i+1), ToolType: "function",
				ConversationID: sessionID, AgentName: start.AgentName, AgentVersion: start.AgentVersion, RequestModel: start.Model.Name,
				RequestProvider: start.Model.Provider, StartedAt: rec.At, ContentCapture: mode,
			})
			// Hooks report completion, not duration: represent this as a point event.
			span.SetResult(toolResult(rec, mode))
			span.End()
		}
	}); err != nil {
		return err
	}
	return client.Flush(ctx)
}

// toolResult bounds span content independently of the generation mapper. Hooks
// retain full redacted payloads, so neither export path can use them directly.
func toolResult(rec tool, mode agento11y.ContentCaptureMode) agento11y.ToolExecutionEnd {
	result := agento11y.ToolExecutionEnd{CompletedAt: rec.At}
	if mapperutil.NormalizePayloadContentMode(mode) == agento11y.ContentCaptureModeFull {
		result.Arguments = mapperutil.BoundText(string(rec.Input), mapperutil.MaxToolInputBytes)
		result.Result = mapperutil.BoundText(string(rec.Response), mapperutil.MaxToolResultBytes)
	}
	return result
}
