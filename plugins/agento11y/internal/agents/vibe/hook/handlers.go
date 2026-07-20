package hook

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/grafana/agento11y/go/sigil"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/vibe/mapper"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/vibe/meta"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/vibe/state"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/vibe/toolevents"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/vibe/transcript"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/grafana/agento11y/plugins/agento11y/internal/otel"
	"github.com/grafana/agento11y/plugins/agento11y/internal/useragent"
)

// exportTimeout caps how long PostAgentTurn will wait for the SDK to
// drain. Vibe blocks on the hook command, so a hung export would stall
// the user's session.
const exportTimeout = 20 * time.Second

// otelInstrumentationName scopes the tracer/meter vibe's tool-execution
// spans and metrics are emitted under.
const otelInstrumentationName = "sigil.vibe"

// PostAgentTurn handles a vibe post_agent_turn hook event end to end:
// read state, read the new transcript slice, read meta.json, map to a
// sigil.Generation, persist the advanced offset and session snapshot, then
// export. State is saved before the export so a post-export save failure
// cannot strand the offset; a failed export rolls the state back so the turn
// replays on the next fire.
//
// The handler never returns an error. Every failure is logged and
// swallowed so a Sigil outage cannot interrupt the user's vibe session.
// The caller (vibe.Hook) writes nothing to stdout and always exits 0.
func PostAgentTurn(ctx context.Context, p Payload, logger *log.Logger) {
	if p.SessionID == "" {
		logger.Print("post_agent_turn: missing session_id")
		return
	}
	if p.TranscriptPath == "" {
		logger.Print("post_agent_turn: missing transcript_path")
		return
	}

	prior, priorFound := state.Load(p.SessionID)
	lines, newOffset, err := transcript.Read(p.TranscriptPath, prior.Offset)
	if err != nil {
		logger.Printf("post_agent_turn: read transcript %s: %v", p.TranscriptPath, err)
		return
	}
	if len(lines) == 0 {
		logger.Printf("post_agent_turn: no new lines at offset=%d", prior.Offset)
		return
	}

	m, err := meta.Load(p.TranscriptPath)
	if err != nil {
		logger.Printf("post_agent_turn: read meta: %v", err)
		return
	}

	// Resolve credentials. A user running the hook directly without
	// Sigil creds (e.g. during testing) shouldn't have their session
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
		logger.Printf("post_agent_turn: not exporting: missing %s", strings.Join(missing, ", "))
		return
	}

	contentMode := envconfig.ResolveContentMode(logger)

	// vibe persists the post_agent_turn count as stats.steps. Using it
	// (rather than a counter in state) keeps the generation ID stable
	// even when a user re-runs the hook against an old transcript while
	// state was lost.
	turnSeq := m.Stats.Steps
	if turnSeq <= 0 {
		// Fall back to one-greater than the prior export so reruns of
		// the very first turn after state loss still progress.
		turnSeq = 1
	}

	// The parent session id can arrive on the thin hook payload or, for
	// subagent sessions, only in meta.json on disk. Prefer the payload and
	// fall back to meta so subagent linkage is not silently skipped.
	parentSessionID := p.ParentSessionID
	if parentSessionID == "" {
		parentSessionID = m.ParentSessionID
	}

	// Resolve a real parent edge when this is a subagent session: the
	// parent session's last export ID is persisted in its own state file.
	// We only get a session-level parent_session_id from vibe, so this
	// links to the parent's most recent generation rather than the exact
	// turn that spawned the child.
	var parentGenID string
	if parentSessionID != "" {
		if parentState, ok := state.Load(parentSessionID); ok {
			parentGenID = parentState.LastGenerationID
		}
	}

	mapped := mapper.Map(mapper.Inputs{
		SessionID:          p.SessionID,
		CWD:                p.CWD,
		ParentSessionID:    parentSessionID,
		ParentGenerationID: parentGenID,
		Lines:              lines,
		Meta:               m,
		PriorState:         prior,
		PriorStateFound:    priorFound,
		ContentCapture:     contentMode,
	}, turnSeq)

	// Persist the advanced offset and session snapshot BEFORE exporting. A
	// save failure aborts the turn (it replays on the next fire); a save here
	// also means a successful export can never be followed by a lost offset,
	// which would re-read and double-export this turn. A failed export rolls
	// the state back below.
	next := state.Session{
		Offset:                  newOffset,
		SessionPromptTokens:     m.Stats.SessionPromptTokens,
		SessionCompletionTokens: m.Stats.SessionCompletionTokens,
		SessionCost:             m.Stats.SessionCost,
		ToolCallsRejected:       m.Stats.ToolCallsRejected,
		ToolCallsHookDenied:     m.Stats.ToolCallsHookDenied,
		ToolCallsFailed:         m.Stats.ToolCallsFailed,
		LastGenerationID:        mapped.Generation.ID,
		Title:                   m.Title,
	}
	if err := state.Save(p.SessionID, next); err != nil {
		logger.Printf("post_agent_turn: save state: %v", err)
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
				logger.Printf("post_agent_turn: otel shutdown: %v", err)
			}
		}()
	}
	client := buildClient(contentMode, providers, endpoint, tenantID, authToken, logger)

	// Per-tool timing/status recorded by after_tool fires this turn. Empty
	// when the user has not enabled the after_tool hook, in which case the
	// spans fall back to synthetic timing off the generation timestamp.
	toolEvents := toolevents.Load(p.SessionID)

	logger.Printf("post_agent_turn: export id=%s session=%s turn=%d", mapped.Generation.ID, p.SessionID, turnSeq)
	if err := emit(exportCtx, client, mapped, toolEvents, logger); err != nil {
		logger.Printf("post_agent_turn: emit: %v", err)
		_ = client.Shutdown(exportCtx)
		restoreState(p.SessionID, prior, priorFound, logger)
		return
	}
	if err := client.Flush(exportCtx); err != nil {
		logger.Printf("post_agent_turn: flush: %v", err)
		_ = client.Shutdown(exportCtx)
		restoreState(p.SessionID, prior, priorFound, logger)
		return
	}
	if providers != nil {
		if err := providers.ForceFlush(); err != nil {
			logger.Printf("post_agent_turn: otel flush: %v", err)
		}
	}
	_ = client.Shutdown(exportCtx)
	// The turn is exported; its tool events have been consumed into spans.
	toolevents.Clear(p.SessionID)
	logger.Printf("post_agent_turn: done session=%s offset=%d", p.SessionID, newOffset)
}

// restoreState rolls back the pre-export state write after a failed export so
// the turn replays on the next fire instead of being skipped. When there was
// no prior state, the advanced snapshot is removed entirely so the session
// looks untouched.
func restoreState(sessionID string, prior state.Session, priorFound bool, logger *log.Logger) {
	if priorFound {
		if err := state.Save(sessionID, prior); err != nil {
			logger.Printf("post_agent_turn: restore state: %v", err)
		}
		return
	}
	if err := state.Delete(sessionID); err != nil {
		logger.Printf("post_agent_turn: delete state: %v", err)
	}
}

func emit(ctx context.Context, client *sigil.Client, mapped mapper.Mapped, toolEvents map[string]toolevents.Event, logger *log.Logger) error {
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
// and error status from the after_tool event for that call (when present;
// otherwise the span gets synthetic zero-duration timing off the generation
// completion time, like claude-code's reconstructed spans).
func emitToolSpans(ctx context.Context, client *sigil.Client, gen sigil.Generation, mode sigil.ContentCaptureMode, events map[string]toolevents.Event, logger *log.Logger) {
	results := buildToolResultMap(gen.Input)
	for _, msg := range gen.Output {
		for _, part := range msg.Parts {
			if part.ToolCall == nil {
				continue
			}
			tc := part.ToolCall
			ev, hasEvent := events[tc.ID]
			startedAt, completedAt := toolSpanWindow(ev, hasEvent, gen.CompletedAt)
			_, toolRec := client.StartToolExecution(ctx, sigil.ToolExecutionStart{
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
			end := sigil.ToolExecutionEnd{CompletedAt: completedAt}
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
				logger.Printf("post_agent_turn: tool span: %v", err)
			}
		}
	}
}

// buildToolResultMap indexes the turn's tool-result parts by tool_call_id so
// each tool call's span can carry its result.
func buildToolResultMap(input []sigil.Message) map[string]sigil.ToolResult {
	out := map[string]sigil.ToolResult{}
	for _, msg := range input {
		for _, part := range msg.Parts {
			if part.ToolResult != nil && part.ToolResult.ToolCallID != "" {
				out[part.ToolResult.ToolCallID] = *part.ToolResult
			}
		}
	}
	return out
}

// toolSpanWindow resolves a tool span's [start, end] window. With an
// after_tool event it uses the recorded completion time and duration; without
// one it collapses to a zero-duration span at the generation completion time.
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
		logger.Printf("post_agent_turn: otel setup: %v", err)
		return nil
	}
	return providers
}

func buildClient(mode sigil.ContentCaptureMode, providers *otel.Providers, endpoint, tenantID, authToken string, logger *log.Logger) *sigil.Client {
	cfg := sigil.Config{
		ContentCapture:   mode,
		Logger:           logger,
		GenerationExport: exportConfig(endpoint, tenantID, authToken),
	}
	if providers != nil {
		cfg.Tracer = providers.Tracer(otelInstrumentationName)
		cfg.Meter = providers.Meter(otelInstrumentationName)
	}
	return sigil.NewClient(cfg)
}

func exportConfig(endpoint, tenantID, authToken string) sigil.GenerationExportConfig {
	return sigil.GenerationExportConfig{
		Protocol: sigil.GenerationExportProtocolHTTP,
		Endpoint: strings.TrimRight(endpoint, "/") + "/api/v1/generations:export",
		Headers:  map[string]string{"User-Agent": useragent.For("vibe")},
		Auth: sigil.AuthConfig{
			Mode:          sigil.ExportAuthModeBasic,
			BasicUser:     tenantID,
			BasicPassword: authToken,
			TenantID:      tenantID,
		},
	}
}
