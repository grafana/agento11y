package hook

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/grafana/agento11y/go/agento11y"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/vibe/mapper"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/vibe/meta"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/vibe/state"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/vibe/toolevents"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/vibe/transcript"
	"github.com/grafana/agento11y/plugins/agento11y/internal/autotag"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/grafana/agento11y/plugins/agento11y/internal/otel"
	"github.com/grafana/agento11y/plugins/agento11y/internal/useragent"
)

// exportTimeout caps how long PostAgent will wait for the SDK to
// drain. Vibe blocks on the hook command, so a hung export would stall
// the user's session.
const exportTimeout = 20 * time.Second

// otelInstrumentationName scopes the tracer/meter vibe's tool-execution
// spans and metrics are emitted under.
const otelInstrumentationName = "agento11y.vibe"

// PostAgent handles a vibe post_agent hook event end to end:
// read state, read the new transcript slice, read meta.json, map to a
// agento11y.Generation, persist the advanced offset and session snapshot, then
// export. State is saved before the export so a post-export save failure
// cannot strand the offset; a failed export rolls the state back so the turn
// replays on the next fire.
//
// The handler never returns an error. Every failure is logged and
// swallowed so an agento11y outage cannot interrupt the user's vibe session.
// The caller (vibe.Hook) writes nothing to stdout and always exits 0.
func PostAgent(ctx context.Context, p Payload, logger *log.Logger) {
	if p.SessionID == "" {
		logger.Print("post_agent: missing session_id")
		return
	}
	if p.TranscriptPath == "" {
		logger.Print("post_agent: missing transcript_path")
		return
	}

	prior, priorFound := state.Load(p.SessionID)
	lines, newOffset, err := transcript.Read(p.TranscriptPath, prior.Offset)
	if err != nil {
		logger.Printf("post_agent: read transcript %s: %v", p.TranscriptPath, err)
		return
	}
	if len(lines) == 0 {
		logger.Printf("post_agent: no new lines at offset=%d", prior.Offset)
		return
	}

	m, err := meta.Load(p.TranscriptPath)
	if err != nil {
		logger.Printf("post_agent: read meta: %v", err)
		return
	}

	// Resolve credentials. A user running the hook directly without
	// agento11y creds (e.g. during testing) shouldn't have their session
	// crash; we just log and bail without advancing the offset.
	envconfig.ApplyLocalAuthPlaceholders()
	endpoint := envconfig.Getenv("ENDPOINT")
	tenantID := envconfig.Getenv("AUTH_TENANT_ID")
	authToken := envconfig.Getenv("AUTH_TOKEN")
	missing := envconfig.MissingEnvVars(
		[]string{"AGENTO11Y_ENDPOINT", "AGENTO11Y_AUTH_TENANT_ID", "AGENTO11Y_AUTH_TOKEN"},
		map[string]string{
			"AGENTO11Y_ENDPOINT":       endpoint,
			"AGENTO11Y_AUTH_TENANT_ID": tenantID,
			"AGENTO11Y_AUTH_TOKEN":     authToken,
		},
	)
	if len(missing) > 0 {
		logger.Printf("post_agent: not exporting: missing %s", strings.Join(missing, ", "))
		return
	}

	contentMode := envconfig.ResolveContentMode(logger)
	skipPromptRedaction := !envconfig.ResolveRedactInput(logger)

	// vibe persists the post_agent count as stats.steps. Using it
	// (rather than a counter in state) keeps the generation ID stable
	// even when a user re-runs the hook against an old transcript while
	// state was lost.
	turnSeq := m.Stats.Steps
	if turnSeq <= 0 {
		// Fall back to one-greater than the prior export so reruns of
		// the very first turn after state loss still progress.
		turnSeq = 1
	}

	parentSessionID, parentGenID := resolveParentLineage(p, m)

	mapped := mapper.Map(mapper.Inputs{
		SessionID:           p.SessionID,
		CWD:                 p.CWD,
		ParentSessionID:     parentSessionID,
		ParentGenerationID:  parentGenID,
		Lines:               lines,
		Meta:                m,
		PriorState:          prior,
		PriorStateFound:     priorFound,
		ContentCapture:      contentMode,
		SkipPromptRedaction: skipPromptRedaction,
		AgentName:           envconfig.ResolveAgentName(mapper.AgentName),
	}, turnSeq)

	// A meta.json without session_cost carries no new baseline, so keep the
	// prior one. Snapshotting 0 would make a later turn that does report a
	// cost delta against zero and bill the whole session total to it.
	sessionCost := prior.SessionCost
	if m.Stats.SessionCost != nil {
		sessionCost = *m.Stats.SessionCost
	}

	// Persist the advanced offset and session snapshot BEFORE exporting. A
	// save failure aborts the turn (it replays on the next fire); a save here
	// also means a successful export can never be followed by a lost offset,
	// which would re-read and double-export this turn. A failed export rolls
	// the state back below.
	next := state.Session{
		Offset:                  newOffset,
		SessionPromptTokens:     m.Stats.SessionPromptTokens,
		SessionCompletionTokens: m.Stats.SessionCompletionTokens,
		SessionCost:             sessionCost,
		ToolCallsRejected:       m.Stats.ToolCallsRejected,
		ToolCallsHookDenied:     m.Stats.ToolCallsHookDenied,
		ToolCallsFailed:         m.Stats.ToolCallsFailed,
		LastGenerationID:        mapped.Generation.ID,
		Title:                   m.Title,
	}
	if err := state.Save(p.SessionID, next); err != nil {
		logger.Printf("post_agent: save state: %v", err)
		return
	}

	exportCtx, cancel := context.WithTimeout(ctx, exportTimeout)
	defer cancel()

	// Tool-execution spans only leave the process through an OTel exporter,
	// which the user configures via SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT. When
	// unset, setupOTelIfConfigured returns nil and the spans are no-ops, the
	// same as the generation export running without OTel.
	providers := setupOTelIfConfigured(exportCtx, p.SessionID, logger)
	if providers != nil {
		defer func() {
			if err := providers.Shutdown(exportCtx); err != nil {
				logger.Printf("post_agent: otel shutdown: %v", err)
			}
		}()
	}
	client := buildClient(contentMode, providers, endpoint, tenantID, authToken, p.CWD, logger)

	// Per-tool timing/status recorded by post_tool fires this turn. Empty
	// when the user has not enabled the post_tool hook, in which case the
	// spans fall back to synthetic timing off the generation timestamp.
	toolEvents := toolevents.Load(p.SessionID)

	logger.Printf("post_agent: export id=%s session=%s turn=%d", mapped.Generation.ID, p.SessionID, turnSeq)
	if err := emit(exportCtx, client, mapped, toolEvents, logger); err != nil {
		logger.Printf("post_agent: emit: %v", err)
		_ = client.Shutdown(exportCtx)
		restoreState(p.SessionID, prior, priorFound, logger)
		return
	}
	if err := client.Flush(exportCtx); err != nil {
		logger.Printf("post_agent: flush: %v", err)
		_ = client.Shutdown(exportCtx)
		restoreState(p.SessionID, prior, priorFound, logger)
		return
	}
	if providers != nil {
		if err := providers.ForceFlush(); err != nil {
			logger.Printf("post_agent: otel flush: %v", err)
		}
	}
	_ = client.Shutdown(exportCtx)
	// The turn is exported; its tool events have been consumed into spans.
	toolevents.Clear(p.SessionID)
	logger.Printf("post_agent: done session=%s offset=%d", p.SessionID, newOffset)
}

// Vibe provides only a parent session ID, so the edge targets that session's
// most recently exported generation.
func resolveParentLineage(p Payload, m meta.Meta) (sessionID, generationID string) {
	sessionID = p.ParentSessionID
	if sessionID == "" {
		sessionID = m.ParentSessionID
	}
	if sessionID == "" {
		return "", ""
	}
	if parentState, ok := state.Load(sessionID); ok {
		generationID = parentState.LastGenerationID
	}
	return sessionID, generationID
}

// restoreState rolls back the pre-export state write after a failed export so
// the turn replays on the next fire instead of being skipped. When there was
// no prior state, the advanced snapshot is removed entirely so the session
// looks untouched.
func restoreState(sessionID string, prior state.Session, priorFound bool, logger *log.Logger) {
	if priorFound {
		if err := state.Save(sessionID, prior); err != nil {
			logger.Printf("post_agent: restore state: %v", err)
		}
		return
	}
	if err := state.Delete(sessionID); err != nil {
		logger.Printf("post_agent: delete state: %v", err)
	}
}

func emit(ctx context.Context, client *agento11y.Client, mapped mapper.Mapped, toolEvents map[string]toolevents.Event, logger *log.Logger) error {
	genCtx, rec := client.StartGeneration(ctx, mapped.Start)
	rec.SetResult(mapped.Generation, nil)
	emitToolSpans(genCtx, client, mapped.Generation, mapped.Start.ContentCapture, toolEvents, logger)
	rec.End()
	if err := rec.Err(); err != nil {
		return fmt.Errorf("recorder: %w", err)
	}
	return nil
}

// emitToolSpans emits one execute_tool span per assistant tool call in the
// turn, nested under the generation. The call args come from the generation
// output, the result from the matching tool-result message, and the timing
// and error status from the post_tool event for that call (when present;
// otherwise the span gets synthetic zero-duration timing off the generation
// completion time, like claude-code's reconstructed spans).
func emitToolSpans(ctx context.Context, client *agento11y.Client, gen agento11y.Generation, mode agento11y.ContentCaptureMode, events map[string]toolevents.Event, logger *log.Logger) {
	results := buildToolResultMap(gen.Input)
	for _, msg := range gen.Output {
		for _, part := range msg.Parts {
			if part.ToolCall == nil {
				continue
			}
			tc := part.ToolCall
			ev, hasEvent := events[tc.ID]
			startedAt, completedAt := toolSpanWindow(ev, hasEvent, gen.CompletedAt)
			_, toolRec := client.StartToolExecution(ctx, agento11y.ToolExecutionStart{
				ToolName:        tc.Name,
				ToolCallID:      tc.ID,
				ToolType:        "function",
				ConversationID:  gen.ConversationID,
				AgentName:       gen.AgentName,
				RequestModel:    gen.Model.Name,
				RequestProvider: gen.Model.Provider,
				StartedAt:       startedAt,
				ContentCapture:  mode,
			})
			end := agento11y.ToolExecutionEnd{CompletedAt: completedAt}
			if len(tc.InputJSON) > 0 {
				end.Arguments = string(tc.InputJSON)
			}
			if tr, ok := results[tc.ID]; ok {
				if tr.Content != "" {
					end.Result = tr.Content
				} else if len(tr.ContentJSON) > 0 {
					end.Result = string(tr.ContentJSON)
				}
			}
			if hasEvent && ev.Failed() {
				toolRec.SetExecError(ev.ErrorOr())
			}
			toolRec.SetResult(end)
			toolRec.End()
			if err := toolRec.Err(); err != nil {
				logger.Printf("post_agent: tool span: %v", err)
			}
		}
	}
}

// buildToolResultMap indexes the turn's tool-result parts by tool_call_id so
// each tool call's span can carry its result.
func buildToolResultMap(input []agento11y.Message) map[string]agento11y.ToolResult {
	out := map[string]agento11y.ToolResult{}
	for _, msg := range input {
		for _, part := range msg.Parts {
			if part.ToolResult != nil && part.ToolResult.ToolCallID != "" {
				out[part.ToolResult.ToolCallID] = *part.ToolResult
			}
		}
	}
	return out
}

// toolSpanWindow resolves a tool span's [start, end] window. With a post_tool
// event it uses the recorded completion time and duration; without one it
// collapses to a zero-duration span at the generation completion time.
func toolSpanWindow(ev toolevents.Event, hasEvent bool, genCompletedAt time.Time) (startedAt, completedAt time.Time) {
	completedAt = genCompletedAt
	if hasEvent && !ev.CompletedAt.IsZero() {
		completedAt = ev.CompletedAt
	}
	startedAt = completedAt
	if hasEvent && ev.DurationMs > 0 {
		startedAt = completedAt.Add(-time.Duration(ev.DurationMs * float64(time.Millisecond)))
	}
	return startedAt, completedAt
}

func setupOTelIfConfigured(ctx context.Context, instanceID string, logger *log.Logger) *otel.Providers {
	if otel.EndpointFromEnv() == "" {
		return nil
	}
	providers, err := otel.Setup(ctx, instanceID)
	if err != nil {
		logger.Printf("post_agent: otel setup: %v", err)
		return nil
	}
	return providers
}

// buildClient constructs the agento11y client for one exported turn. cwd is
// the turn's working directory, which auto-tags resolve the repository and
// branch from; vibe payloads carry no user identity, so that falls back to the
// configured AGENTO11Y_USER_ID or the OS account name.
func buildClient(mode agento11y.ContentCaptureMode, providers *otel.Providers, endpoint, tenantID, authToken, cwd string, logger *log.Logger) *agento11y.Client {
	cfg := agento11y.Config{
		ContentCapture:   mode,
		Logger:           logger,
		GenerationExport: exportConfig(endpoint, tenantID, authToken),
		Tags:             autotag.FromEnv(autotag.Inputs{Cwd: cwd}, logger),
	}
	if providers != nil {
		cfg.Tracer = providers.Tracer(otelInstrumentationName)
		cfg.Meter = providers.Meter(otelInstrumentationName)
	}
	return agento11y.NewClient(cfg)
}

func exportConfig(endpoint, tenantID, authToken string) agento11y.GenerationExportConfig {
	return agento11y.GenerationExportConfig{
		Protocol: agento11y.GenerationExportProtocolHTTP,
		Endpoint: strings.TrimRight(endpoint, "/") + "/api/v1/generations:export",
		Headers:  map[string]string{"User-Agent": useragent.For("vibe")},
		Auth: agento11y.AuthConfig{
			Mode:          agento11y.ExportAuthModeBasic,
			BasicUser:     tenantID,
			BasicPassword: authToken,
			TenantID:      tenantID,
		},
	}
}
