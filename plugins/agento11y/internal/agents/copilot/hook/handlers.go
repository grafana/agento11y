package hook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/grafana/agento11y/go/sigil"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/copilot/config"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/copilot/fragment"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/copilot/mapper"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/copilot/transcript"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/guard"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/grafana/agento11y/plugins/agento11y/internal/otel"
	"github.com/grafana/agento11y/plugins/agento11y/internal/redact"
	"github.com/grafana/agento11y/plugins/agento11y/internal/sigilemit"
	"github.com/grafana/agento11y/plugins/agento11y/internal/timeutil"
	"github.com/grafana/agento11y/plugins/agento11y/internal/useragent"
)

const (
	otelInstrumentationName = "sigil.copilot"
	stopExportTimeout       = 20 * time.Second
	transcriptRetryWindow   = 1500 * time.Millisecond
	transcriptRetryInterval = 100 * time.Millisecond
)

var (
	loadFragment   = fragment.LoadTolerant
	deleteFragment = fragment.Delete
)

func SessionStart(p Payload, cfg config.Config, logger *log.Logger) {
	sessionID := p.SessionID()
	if sessionID == "" {
		return
	}
	if err := fragment.UpdateSession(sessionID, logger, func(s *fragment.Session) bool {
		if p.CWD != "" {
			s.Cwd = p.CWD
		}
		if src := p.Source(); src != "" {
			s.Source = src
		}
		if surface := p.Surface(); surface != "" {
			s.Surface = surface
		}
		if cfg.ContentCapture != sigil.ContentCaptureModeMetadataOnly {
			if initialPrompt := p.InitialPrompt(); initialPrompt != "" {
				s.InitialPrompt = initialPrompt
			}
		}
		if transcriptPath := p.TranscriptPath(); transcriptPath != "" {
			s.TranscriptPath = transcriptPath
		}
		fragment.TouchSession(s, p.ResolvedTimestamp())
		return true
	}); err != nil {
		logger.Printf("sessionStart: save session: %v", err)
	}
}

func SessionEnd(p Payload, logger *log.Logger) {
	sessionID := p.SessionID()
	if sessionID == "" {
		return
	}
	session := fragment.LoadSessionTolerant(sessionID, logger)
	if session != nil && session.ActiveTurnID != "" {
		logger.Printf("sessionEnd: active turn %s still present; deferring cleanup", session.ActiveTurnID)
		return
	}
	for _, turnID := range fragment.ListTurnIDs(sessionID, logger) {
		if err := fragment.Delete(sessionID, turnID); err != nil {
			logger.Printf("sessionEnd: delete turn %s: %v", turnID, err)
		}
	}
	if err := fragment.DeleteSession(sessionID); err != nil {
		logger.Printf("sessionEnd: delete session: %v", err)
	}
}

func UserPromptSubmit(p Payload, cfg config.Config, logger *log.Logger) {
	sessionID := p.SessionID()
	if sessionID == "" {
		logger.Print("userPromptSubmit: missing session_id")
		return
	}
	ts := p.ResolvedTimestamp()
	turnID, session, err := fragment.StartNextTurn(sessionID, logger, ts)
	if err != nil {
		logger.Printf("userPromptSubmit: start turn: %v", err)
		return
	}
	if err := updateCommon(sessionID, turnID, session, p, logger, func(f *fragment.Fragment) {
		prompt := p.Prompt
		if prompt == "" {
			prompt = session.InitialPrompt
		}
		f.PromptHash = transcript.PromptHash(prompt)
		if cfg.ContentCapture != sigil.ContentCaptureModeMetadataOnly {
			f.Prompt = prompt
			f.InitialPrompt = session.InitialPrompt
		}
	}); err != nil {
		logger.Printf("userPromptSubmit: update turn: %v", err)
	}
}

func PreToolUse(ctx context.Context, stdout io.Writer, p Payload, cfg config.Config, logger *log.Logger) {
	sessionID := p.SessionID()
	if sessionID == "" {
		logger.Print("preToolUse: missing session_id")
		return
	}
	toolArgs := p.ToolInput()
	if cfg.Guards.Enabled {
		// Best-effort: pull last-known model/provider off the active turn
		// fragment. Copilot's preToolUse payload doesn't carry model, but
		// once enrichFromTranscript has run for a prior turn we may have
		// it cached. The guard helper falls back to "unknown" when blank,
		// which keeps the request well-formed for Sigil.
		var provider, modelName string
		if session := fragment.LoadSessionTolerant(sessionID, logger); session != nil && session.ActiveTurnID != "" {
			if frag := loadFragment(sessionID, session.ActiveTurnID, logger); frag != nil {
				provider = frag.Provider
				modelName = frag.Model
			}
		}
		res := guard.EvaluateToolCall(ctx, cfg.Guards, guard.ToolCallInput{
			AgentName: mapper.AgentName,
			ToolName:  p.ToolName(),
			// Copilot delivers tool args as a JSON-encoded string; the Sigil
			// server only transforms tool-call input that is a JSON object, so
			// decode the wrapper before evaluating or redaction never happens.
			ToolInputJSON: decodeStringEncodedToolInput(toolArgs),
			ModelProvider: provider,
			ModelName:     modelName,
		}, logger)
		if res.Blocked() {
			// The two Copilot surfaces read opposite response shapes: the
			// CLI honors only the flat top-level permissionDecision
			// (verified against CLI 1.0.54, which silently ignores the
			// nested envelope), while Copilot Chat in VS Code honors only
			// the nested hookSpecificOutput envelope and ignores the flat
			// fields. Each host skips the shape it does not know, so one
			// combined object blocks on both.
			writeDeny(stdout, res.Reason)
			return
		}
		if len(res.UpdatedInputJSON) > 0 {
			// Transform (redaction) verdicts are applied on the copilot-cli
			// surface only. The CLI substitutes tool arguments from the flat
			// {"modifiedArgs": <object>} response (verified against CLI
			// 1.0.54, which ignores the nested hookSpecificOutput envelope
			// for argument rewrites). Copilot Chat in VS Code parses only
			// hookSpecificOutput.updatedInput, and that path is unverified
			// live, so nothing is written there. A transform the host
			// silently drops would report redaction that never happened.
			if p.Surface() == surfaceCopilotCLI {
				writeModifiedArgs(stdout, res.UpdatedInputJSON)
				// Record the redacted arguments: they are what the tool
				// actually runs with, and the originals may hold the very
				// content the Transform rule strips.
				toolArgs = res.UpdatedInputJSON
			} else {
				logger.Printf("preToolUse: dropping guard transform for tool %q: surface %q is not verified to apply modified arguments", p.ToolName(), p.Surface())
			}
		}
	}
	turnID, session, err := fragment.EnsureActiveTurn(sessionID, logger, p.ResolvedTimestamp())
	if err != nil {
		logger.Printf("preToolUse: ensure turn: %v", err)
		return
	}
	if err := updateCommon(sessionID, turnID, session, p, logger, func(f *fragment.Fragment) {
		f.NextToolIndex++
		input := toolArgs
		if cfg.ContentCapture != sigil.ContentCaptureModeFull {
			input = nil
		}
		f.Tools = append(f.Tools, fragment.ToolRecord{
			ToolName:   p.ToolName(),
			ToolUseID:  fmt.Sprintf("%s-tool-%03d", turnID, f.NextToolIndex),
			ToolInput:  input,
			Cwd:        p.CWD,
			StartedAt:  p.ResolvedTimestamp(),
			Status:     "pending",
			DurationMs: p.DurationMs(),
		})
	}); err != nil {
		logger.Printf("preToolUse: update turn: %v", err)
	}
}

// surfaceCopilotCLI is the surface the dispatcher stamps onto the payload
// when the Copilot CLI (rather than Copilot Chat in VS Code) fired the hook;
// see copilot/surface.go.
const surfaceCopilotCLI = "copilot-cli"

// decodeStringEncodedToolInput unwraps tool arguments that the Copilot CLI
// delivers as a JSON-encoded string (e.g. `"{\"command\":\"…\"}"`) into the
// underlying JSON object. The Sigil server only applies Transform rules to
// tool-call input that is a JSON object: given a JSON string it returns no
// transform at all, so Copilot's arguments are never redacted without this.
// Returns raw unchanged when it is already an object, or a string that does
// not wrap a JSON object.
func decodeStringEncodedToolInput(raw json.RawMessage) json.RawMessage {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return raw
	}
	inner := json.RawMessage(s)
	var obj map[string]any
	if err := json.Unmarshal(inner, &obj); err != nil || obj == nil {
		return raw
	}
	return inner
}

// preToolUseModifiedArgs is the flat preToolUse response the Copilot CLI
// reads to substitute tool arguments. It is unrelated to the nested
// hookSpecificOutput envelope used for deny verdicts.
type preToolUseModifiedArgs struct {
	ModifiedArgs json.RawMessage `json:"modifiedArgs"`
}

// preToolUseDeny carries the deny verdict in both Copilot response shapes:
// the flat fields for the CLI and the nested envelope for Copilot Chat in
// VS Code.
type preToolUseDeny struct {
	PermissionDecision       string                 `json:"permissionDecision"`
	PermissionDecisionReason string                 `json:"permissionDecisionReason"`
	HookSpecificOutput       preToolUseDenyEnvelope `json:"hookSpecificOutput"`
}

type preToolUseDenyEnvelope struct {
	HookEventName            string `json:"hookEventName"`
	PermissionDecision       string `json:"permissionDecision"`
	PermissionDecisionReason string `json:"permissionDecisionReason"`
}

// writeDeny writes the combined PreToolUse deny JSON. The Copilot CLI reads
// the flat permissionDecision fields and ignores the nested envelope
// (verified against CLI 1.0.54); Copilot Chat in VS Code reads the nested
// hookSpecificOutput envelope and ignores the flat fields. The hookEventName
// must be the PascalCase "PreToolUse"; VS Code drops the envelope on an
// exact-match failure.
func writeDeny(stdout io.Writer, reason string) {
	if stdout == nil {
		return
	}
	if strings.TrimSpace(reason) == "" {
		reason = "tool call denied by Sigil guard"
	}
	_ = json.NewEncoder(stdout).Encode(preToolUseDeny{
		PermissionDecision:       "deny",
		PermissionDecisionReason: reason,
		HookSpecificOutput: preToolUseDenyEnvelope{
			HookEventName:            "PreToolUse",
			PermissionDecision:       "deny",
			PermissionDecisionReason: reason,
		},
	})
}

// writeModifiedArgs writes the flat modifiedArgs JSON that makes the Copilot
// CLI run the tool with the substituted arguments. modifiedArgs must be a
// JSON object, which guard.EvaluateToolCall already guarantees for
// UpdatedInputJSON; the CLI ignores a JSON-encoded string even though it
// sends toolArgs in that form. No permissionDecision is included: the CLI applies
// modifiedArgs without one, and adding "allow" would also skip the user's
// permission prompt for the tool call.
func writeModifiedArgs(stdout io.Writer, modifiedArgs json.RawMessage) {
	if stdout == nil || len(modifiedArgs) == 0 {
		return
	}
	_ = json.NewEncoder(stdout).Encode(preToolUseModifiedArgs{ModifiedArgs: modifiedArgs})
}

func PostToolUse(p Payload, cfg config.Config, logger *log.Logger, failed bool) {
	sessionID := p.SessionID()
	if sessionID == "" {
		logger.Print("postToolUse: missing session_id")
		return
	}
	turnID, session, err := fragment.EnsureActiveTurn(sessionID, logger, p.ResolvedTimestamp())
	if err != nil {
		logger.Printf("postToolUse: ensure turn: %v", err)
		return
	}
	if err := updateCommon(sessionID, turnID, session, p, logger, func(f *fragment.Fragment) {
		status := "completed"
		if failed {
			status = "error"
		}
		response := p.ToolResult()
		if cfg.ContentCapture != sigil.ContentCaptureModeFull {
			response = nil
		}
		errorMessage := ""
		if failed && cfg.ContentCapture == sigil.ContentCaptureModeFull {
			errorMessage = redactor().RedactJSONForText(p.Error())
		}
		idx := findPendingToolIndex(f.Tools, p.ToolName())
		if idx < 0 {
			f.NextToolIndex++
			f.Tools = append(f.Tools, fragment.ToolRecord{
				ToolName:     p.ToolName(),
				ToolUseID:    fmt.Sprintf("%s-tool-%03d", turnID, f.NextToolIndex),
				Cwd:          p.CWD,
				StartedAt:    p.ResolvedTimestamp(),
				Status:       status,
				ToolResponse: response,
				ErrorMessage: errorMessage,
				CompletedAt:  p.ResolvedTimestamp(),
				DurationMs:   p.DurationMs(),
			})
			return
		}
		f.Tools[idx].Status = status
		f.Tools[idx].CompletedAt = p.ResolvedTimestamp()
		f.Tools[idx].DurationMs = p.DurationMs()
		f.Tools[idx].Cwd = firstNonEmpty(p.CWD, f.Tools[idx].Cwd)
		if len(response) > 0 {
			f.Tools[idx].ToolResponse = response
		}
		if errorMessage != "" {
			f.Tools[idx].ErrorMessage = errorMessage
		}
	}); err != nil {
		logger.Printf("postToolUse: update turn: %v", err)
	}
}

func ErrorOccurred(p Payload, cfg config.Config, logger *log.Logger) {
	sessionID := p.SessionID()
	if sessionID == "" {
		logger.Print("errorOccurred: missing session_id")
		return
	}
	turnID, session, err := fragment.EnsureActiveTurn(sessionID, logger, p.ResolvedTimestamp())
	if err != nil {
		logger.Printf("errorOccurred: ensure turn: %v", err)
		return
	}
	if err := updateCommon(sessionID, turnID, session, p, logger, func(f *fragment.Fragment) {
		item := fragment.ErrorRecord{
			Context:     p.ErrorContext(),
			Name:        extractErrorName(p.Error()),
			Recoverable: p.Recoverable != nil && *p.Recoverable,
			Timestamp:   p.ResolvedTimestamp(),
		}
		if cfg.ContentCapture == sigil.ContentCaptureModeFull {
			item.Message = redactor().RedactJSONForText(p.Error())
		}
		f.Errors = append(f.Errors, item)
	}); err != nil {
		logger.Printf("errorOccurred: update turn: %v", err)
	}
}

func SubagentStart(p Payload, cfg config.Config, logger *log.Logger) {
	sessionID := p.SessionID()
	if sessionID == "" {
		logger.Print("subagentStart: missing session_id")
		return
	}
	turnID, session, err := fragment.EnsureActiveTurn(sessionID, logger, p.ResolvedTimestamp())
	if err != nil {
		logger.Printf("subagentStart: ensure turn: %v", err)
		return
	}
	if err := updateCommon(sessionID, turnID, session, p, logger, func(f *fragment.Fragment) {
		record := fragment.SubagentRecord{
			AgentName:        p.AgentName(),
			AgentDisplayName: p.AgentDisplayName(),
			StartedAt:        p.ResolvedTimestamp(),
		}
		if cfg.ContentCapture != sigil.ContentCaptureModeMetadataOnly {
			record.AgentDescription = p.AgentDescription()
		}
		if transcriptPath := p.TranscriptPath(); transcriptPath != "" {
			record.TranscriptPath = transcriptPath
		}
		f.Subagents = append(f.Subagents, record)
	}); err != nil {
		logger.Printf("subagentStart: update turn: %v", err)
	}
}

func SubagentStop(p Payload, logger *log.Logger) {
	sessionID := p.SessionID()
	if sessionID == "" {
		logger.Print("subagentStop: missing session_id")
		return
	}
	session := fragment.LoadSessionTolerant(sessionID, logger)
	if session == nil || session.ActiveTurnID == "" {
		return
	}
	if err := fragment.Update(sessionID, session.ActiveTurnID, logger, func(f *fragment.Fragment) bool {
		idx := findPendingSubagentIndex(f.Subagents, p.AgentName(), p.AgentDisplayName())
		if idx < 0 {
			f.Subagents = append(f.Subagents, fragment.SubagentRecord{
				AgentName:        p.AgentName(),
				AgentDisplayName: p.AgentDisplayName(),
				TranscriptPath:   p.TranscriptPath(),
				CompletedAt:      p.ResolvedTimestamp(),
				StopReason:       firstNonEmpty(p.StopReason(), p.Reason()),
			})
			return true
		}
		f.Subagents[idx].CompletedAt = p.ResolvedTimestamp()
		f.Subagents[idx].StopReason = firstNonEmpty(p.StopReason(), p.Reason())
		if transcriptPath := p.TranscriptPath(); transcriptPath != "" {
			f.Subagents[idx].TranscriptPath = transcriptPath
		}
		return true
	}); err != nil {
		logger.Printf("subagentStop: update turn: %v", err)
	}
}

func Stop(p Payload, cfg config.Config, logger *log.Logger) {
	sessionID := p.SessionID()
	if sessionID == "" {
		logger.Print("stop: missing session_id")
		return
	}
	session := fragment.LoadSessionTolerant(sessionID, logger)
	if session == nil || session.ActiveTurnID == "" {
		logger.Print("stop: no active turn")
		return
	}
	turnID := session.ActiveTurnID
	clearActiveTurn := false
	if err := updateCommon(sessionID, turnID, session, p, logger, func(f *fragment.Fragment) {
		f.CompletedAt = p.ResolvedTimestamp()
		f.StopReason = firstNonEmpty(p.StopReason(), p.Reason(), "end_turn")
	}); err != nil {
		logger.Printf("stop: update turn: %v", err)
		clearActiveTurn = true
		_ = fragment.ClearActiveTurn(sessionID, turnID, logger)
		return
	}
	defer func() {
		if !clearActiveTurn {
			return
		}
		if err := fragment.ClearActiveTurn(sessionID, turnID, logger); err != nil {
			logger.Printf("stop: clear active turn: %v", err)
		}
	}()

	envconfig.ApplyLocalAuthPlaceholders()
	if !config.HasCredentials() {
		logger.Print("stop: missing SIGIL_ENDPOINT/SIGIL_AUTH_TENANT_ID/SIGIL_AUTH_TOKEN; discarding fragment")
		clearActiveTurn = true
		if err := fragment.Delete(sessionID, turnID); err != nil {
			logger.Printf("stop: delete fragment: %v", err)
		}
		return
	}

	frag := loadFragment(sessionID, turnID, logger)
	if frag == nil {
		logger.Print("stop: no fragment")
		clearActiveTurn = true
		return
	}
	enrichFromTranscript(frag, logger)

	ctx, cancel := context.WithTimeout(context.Background(), stopExportTimeout)
	defer cancel()
	providers := sigilemit.SetupOTel(ctx, sessionID, logger)
	if providers != nil {
		defer func() {
			if err := providers.Shutdown(ctx); err != nil {
				logger.Printf("otel: shutdown: %v", err)
			}
		}()
	}
	client := buildClient(cfg, providers, logger)
	defer func() {
		_ = client.Shutdown(ctx)
	}()
	mapped := mapper.Map(mapper.Inputs{
		Fragment:       frag,
		Session:        session,
		ContentCapture: cfg.ContentCapture,
		UserIDOverride: envconfig.Getenv("USER_ID"),
	})
	logger.Printf(
		"stop: mapped model=%s provider=%s response_id=%s output_tokens=%d assistant_text=%t tool_count=%d",
		mapped.Generation.Model.Name,
		mapped.Generation.Model.Provider,
		mapped.Generation.ResponseID,
		mapped.Generation.Usage.OutputTokens,
		strings.TrimSpace(frag.AssistantText) != "",
		len(frag.Tools),
	)
	logger.Printf("stop: export id=%s conversation=%s turn=%s", mapped.Generation.ID, mapped.Generation.ConversationID, frag.TurnID)
	if err := emitGeneration(ctx, client, frag, mapped, logger); err != nil {
		logger.Printf("stop: emit: %v", err)
		return
	}
	if err := client.Flush(ctx); err != nil {
		logger.Printf("stop: sigil flush: %v", err)
		return
	}
	if providers != nil {
		if err := providers.ForceFlush(); err != nil {
			logger.Printf("stop: otel flush: %v", err)
		}
	}
	clearActiveTurn = true
	if err := deleteFragment(sessionID, turnID); err != nil {
		logger.Printf("stop: delete fragment: %v", err)
		return
	}
	logger.Printf("stop: emitted session=%s turn=%s", sessionID, turnID)
}

func updateCommon(sessionID, turnID string, session *fragment.Session, p Payload, logger *log.Logger, mutate func(f *fragment.Fragment)) error {
	return fragment.Update(sessionID, turnID, logger, func(f *fragment.Fragment) bool {
		applySessionDefaults(f, session)
		if p.CWD != "" {
			f.Cwd = p.CWD
		}
		if src := p.Source(); src != "" {
			f.Source = src
		}
		if surface := p.Surface(); surface != "" {
			f.Surface = surface
		}
		if transcriptPath := p.TranscriptPath(); transcriptPath != "" {
			f.TranscriptPath = transcriptPath
		}
		if p.Model != "" {
			f.Model = p.Model
		}
		if provider := p.Provider(); provider != "" {
			f.Provider = provider
		}
		applyPayloadUsage(f, p)
		fragment.Touch(f, p.ResolvedTimestamp())
		if mutate != nil {
			mutate(f)
		}
		return true
	})
}

func applySessionDefaults(f *fragment.Fragment, session *fragment.Session) {
	if session == nil {
		return
	}
	if f.Cwd == "" {
		f.Cwd = session.Cwd
	}
	if f.Source == "" {
		f.Source = session.Source
	}
	if f.Surface == "" {
		f.Surface = session.Surface
	}
	if f.TranscriptPath == "" {
		f.TranscriptPath = session.TranscriptPath
	}
	if f.InitialPrompt == "" {
		f.InitialPrompt = session.InitialPrompt
	}
}

func applyPayloadUsage(f *fragment.Fragment, p Payload) {
	if v := p.InputTokens(); v != nil {
		f.TokenUsage.InputTokens = v
	}
	if v := p.OutputTokens(); v != nil {
		f.TokenUsage.OutputTokens = v
	}
	if v := p.CacheReadInputTokens(); v != nil {
		f.TokenUsage.CacheReadInputTokens = v
	}
	if v := p.CacheWriteInputTokens(); v != nil {
		f.TokenUsage.CacheWriteInputTokens = v
	}
	if v := p.ReasoningTokens(); v != nil {
		f.TokenUsage.ReasoningTokens = v
	}
}

func enrichFromTranscript(f *fragment.Fragment, logger *log.Logger) {
	if f == nil || strings.TrimSpace(f.TranscriptPath) == "" {
		return
	}
	if strings.TrimSpace(f.Prompt) == "" && strings.TrimSpace(f.PromptHash) == "" {
		logger.Print("stop: transcript enrich: missing turn hint")
		return
	}

	deadline := time.Now().Add(transcriptRetryWindow)
	var (
		best    transcript.Snapshot
		have    bool
		lastErr error
	)
	for {
		snap, ok, err := transcript.ReadAssistantTurn(f.TranscriptPath, transcript.ReadHint{
			UserPrompt:     f.Prompt,
			UserPromptHash: f.PromptHash,
		})
		if err != nil {
			lastErr = err
		} else if ok {
			if shouldPreferTranscriptSnapshot(best, have, snap) {
				best = snap
				have = true
			}
			if strings.TrimSpace(best.AssistantText) != "" {
				break
			}
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(transcriptRetryInterval)
	}
	if !have {
		if lastErr != nil {
			logger.Printf("stop: transcript enrich: %v", lastErr)
		}
		return
	}
	if f.AgentVersion == "" {
		f.AgentVersion = best.CopilotVersion
	}
	if f.Model == "" {
		f.Model = best.Model
	}
	if f.ReasoningEffort == "" {
		f.ReasoningEffort = best.ReasoningEffort
	}
	if f.NativeTurnID == "" {
		f.NativeTurnID = best.NativeTurnID
	}
	if f.InteractionID == "" {
		f.InteractionID = best.InteractionID
	}
	if f.RequestID == "" {
		f.RequestID = best.RequestID
	}
	if f.MessageID == "" {
		f.MessageID = best.MessageID
	}
	if f.AssistantText == "" {
		f.AssistantText = best.AssistantText
	}
	if strings.TrimSpace(f.Prompt) == "" && strings.TrimSpace(best.UserPrompt) != "" {
		f.Prompt = best.UserPrompt
	}
	if f.TokenUsage.InputTokens == nil && best.InputTokens != nil {
		f.TokenUsage.InputTokens = best.InputTokens
	}
	if f.TokenUsage.OutputTokens == nil && best.OutputTokens != nil {
		f.TokenUsage.OutputTokens = best.OutputTokens
	}
}

func shouldPreferTranscriptSnapshot(current transcript.Snapshot, haveCurrent bool, next transcript.Snapshot) bool {
	if !haveCurrent {
		return true
	}
	currentHasText := strings.TrimSpace(current.AssistantText) != ""
	nextHasText := strings.TrimSpace(next.AssistantText) != ""
	if nextHasText != currentHasText {
		return nextHasText
	}
	if nextHasText && currentHasText && strings.TrimSpace(next.MessageID) != "" && next.MessageID != current.MessageID {
		return true
	}
	currentTokens := int64(0)
	if current.OutputTokens != nil {
		currentTokens = *current.OutputTokens
	}
	nextTokens := int64(0)
	if next.OutputTokens != nil {
		nextTokens = *next.OutputTokens
	}
	if nextTokens != currentTokens {
		return nextTokens > currentTokens
	}
	if strings.TrimSpace(next.MessageID) != "" && next.MessageID != current.MessageID {
		return true
	}
	return false
}

// buildClient constructs the Sigil client. copilot leaves endpoint, tenant ID,
// and token to the SDK's automatic SIGIL_* env resolution, so it only needs the
// shared HTTP/basic-auth export defaults plus the OTel wiring.
func buildClient(cfg config.Config, providers *otel.Providers, logger *log.Logger) *sigil.Client {
	return sigilemit.NewClient(sigilemit.ClientOptions{
		InstrumentationName: otelInstrumentationName,
		ContentCapture:      cfg.ContentCapture,
		Logger:              logger,
		Providers:           providers,
		UserAgent:           useragent.For("copilot"),
	})
}

func emitGeneration(ctx context.Context, client *sigil.Client, frag *fragment.Fragment, mapped mapper.Mapped, logger *log.Logger) error {
	return sigilemit.Record(ctx, client, mapped.Start, mapped.Generation, mapped.CallError, func(genCtx context.Context) {
		emitToolSpans(genCtx, client, frag, mapped.Generation, logger)
	})
}

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
			ContentCapture:  mappedContentCapture(gen),
		})

		end := sigil.ToolExecutionEnd{CompletedAt: completedAt}
		if len(t.ToolInput) > 0 {
			end.Arguments = string(t.ToolInput)
		}
		if len(t.ToolResponse) > 0 {
			end.Result = string(t.ToolResponse)
		} else if t.ErrorMessage != "" {
			end.Result = t.ErrorMessage
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

func mappedContentCapture(gen sigil.Generation) sigil.ContentCaptureMode {
	if len(gen.Input) == 0 && len(gen.Output) == 0 {
		return sigil.ContentCaptureModeMetadataOnly
	}
	for _, msg := range gen.Output {
		for _, part := range msg.Parts {
			if part.ToolCall != nil && len(part.ToolCall.InputJSON) > 0 {
				return sigil.ContentCaptureModeFull
			}
		}
	}
	for _, msg := range gen.Input {
		if msg.Role == sigil.RoleUser {
			return sigil.ContentCaptureModeNoToolContent
		}
		for _, part := range msg.Parts {
			if part.ToolResult != nil && (part.ToolResult.Content != "" || len(part.ToolResult.ContentJSON) > 0) {
				return sigil.ContentCaptureModeFull
			}
		}
	}
	return sigil.ContentCaptureModeMetadataOnly
}

// toolSpanWindow differs from sigilemit.ToolSpanWindow: copilot records a
// per-tool StartedAt at preToolUse, but the historical behavior gives a
// reported DurationMs precedence when it is present. StartedAt is only used
// when DurationMs is missing.
func toolSpanWindow(t fragment.ToolRecord, genCompletedAt time.Time) (startedAt, completedAt time.Time) {
	completedAt = timeutil.ParseTimestamp(t.CompletedAt, genCompletedAt)
	startedAt = timeutil.ParseTimestamp(t.StartedAt, completedAt)
	if t.DurationMs != nil && !completedAt.IsZero() {
		startedAt = completedAt.Add(-time.Duration(*t.DurationMs) * time.Millisecond)
	}
	if startedAt.IsZero() {
		startedAt = completedAt
	}
	return startedAt, completedAt
}

// toolErrorOr trims whitespace before the empty check, unlike
// sigilemit.ToolError, so a whitespace-only tool error message collapses to the
// generic sentinel. Kept local to preserve that behavior.
func toolErrorOr(msg string) error {
	if strings.TrimSpace(msg) == "" {
		return errToolError
	}
	return errors.New(msg)
}

var errToolError = errors.New("tool returned error")

func findPendingToolIndex(tools []fragment.ToolRecord, toolName string) int {
	for i, v := range slices.Backward(tools) {
		if v.ToolName == toolName && v.CompletedAt == "" {
			return i
		}
	}
	return -1
}

func findPendingSubagentIndex(items []fragment.SubagentRecord, agentName, displayName string) int {
	for i, v := range slices.Backward(items) {
		if v.CompletedAt != "" {
			continue
		}
		if v.AgentName == agentName || v.AgentDisplayName == displayName {
			return i
		}
	}
	return -1
}

func extractErrorName(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var asObject struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &asObject); err == nil {
		return strings.TrimSpace(asObject.Name)
	}
	return ""
}

func redactor() *redact.Redactor {
	return redact.New()
}
