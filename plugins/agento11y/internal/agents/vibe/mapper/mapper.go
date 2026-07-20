// Package mapper turns a slice of new vibe transcript lines plus the
// session's meta.json into a sigil.Generation ready for export.
//
// One mapper.Map call produces exactly one generation per
// post_agent_turn hook fire. Sigil groups records by ConversationID
// (= vibe session_id), so multi-turn sessions get one generation per
// turn under the same conversation.
package mapper

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/grafana/agento11y/go/sigil"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/vibe/meta"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/vibe/state"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/vibe/transcript"
	"github.com/grafana/agento11y/plugins/agento11y/internal/gitbranch"
	"github.com/grafana/agento11y/plugins/agento11y/internal/redact"
)

// AgentName is the Sigil identity attached to every generation emitted
// by the vibe agent adapter. Stable across versions.
const AgentName = "mistral-vibe"

// Inputs are the parameters Map needs to build one generation.
//
// PriorState is the state snapshot from before this turn; its session-wide
// totals (tokens, cost, tool-call counters) are the baseline for the
// per-turn deltas. PriorStateFound reports whether that snapshot came from a
// real state file: when false mid-session (a state loss), the delta logic
// must not attribute the whole cumulative total to one turn.
//
// ParentGenerationID, when set, is the parent session's most recent
// generation ID resolved by the caller. It produces a real
// ParentGenerationIDs edge and reparents the conversation onto the parent
// session, the same shape codex uses for subagent links.
type Inputs struct {
	SessionID          string
	CWD                string
	ParentSessionID    string
	ParentGenerationID string
	Lines              []transcript.Line
	Meta               meta.Meta
	PriorState         state.Session
	PriorStateFound    bool
	ContentCapture     sigil.ContentCaptureMode
	Now                time.Time
}

// Mapped bundles the GenerationStart and the corresponding Generation so
// the caller can call client.StartGeneration / rec.SetResult against the
// same input.
type Mapped struct {
	Start      sigil.GenerationStart
	Generation sigil.Generation
}

// Map converts the new transcript slice plus meta.json into a single
// generation. The caller chooses the turn sequence (typically the
// number of post_agent_turn calls observed for this session) so the
// generation ID stays deterministic across re-runs.
func Map(in Inputs, turnSeq int) Mapped {
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	mode := in.ContentCapture
	if mode == sigil.ContentCaptureModeDefault {
		mode = sigil.ContentCaptureModeMetadataOnly
	}

	provider, apiName := in.Meta.ActiveModelRef()
	displayName := strings.TrimSpace(in.Meta.Config.ActiveModel)
	if displayName == "" {
		displayName = apiName
	}
	if displayName == "" {
		displayName = "unknown"
	}
	model := sigil.ModelRef{Provider: provider, Name: displayName}

	id := GenerationID(in.SessionID, turnSeq)

	// A resolved parent generation reparents this turn onto the parent
	// session and records a real generation edge; without it we only have a
	// session-level hint to expose as a tag/metadata string. Same shape as
	// the codex subagent link.
	conversationID := in.SessionID
	var parentIDs []string
	if in.ParentGenerationID != "" && in.ParentSessionID != "" {
		conversationID = in.ParentSessionID
		parentIDs = []string{in.ParentGenerationID}
	}

	tags := map[string]string{
		"entrypoint":    "vibe",
		"vibe.turn_seq": strconv.Itoa(turnSeq),
	}
	if in.CWD != "" {
		tags["cwd"] = in.CWD
	}
	if branch := gitbranch.Resolve(in.CWD); branch != "" {
		tags["git.branch"] = branch
	}
	if in.ParentSessionID != "" {
		tags["vibe.parent_session_id"] = in.ParentSessionID
	}
	// When reparented onto the parent session, conversation_id is the
	// parent's; record the child's own session id so subagent generations
	// stay tied back to the child in observability.
	if len(parentIDs) > 0 {
		tags["vibe.child_session_id"] = in.SessionID
	}

	metadata := buildMetadata(in)

	completedAt := now.UTC()
	startedAt := completedAt
	if d := in.Meta.Stats.LastTurnDuration; d > 0 {
		startedAt = completedAt.Add(-time.Duration(d * float64(time.Second)))
	}

	usage := turnUsage(in.Meta.Stats, in.PriorState, in.PriorStateFound)
	tools := buildToolDefinitions(in.Meta.ToolsAvailable)
	systemPrompt := ""
	if mode != sigil.ContentCaptureModeMetadataOnly {
		systemPrompt = strings.TrimSpace(in.Meta.SystemPrompt.Content)
	}

	// On a mid-session state loss the handler reads the whole transcript
	// from offset zero and usage falls back to the last turn (see
	// turnUsage). Map only the latest turn's messages so the single emitted
	// generation does not bundle the entire history under last-turn usage.
	lines := linesForMapping(in.Lines, in.Meta.Stats, in.PriorStateFound)
	input, output, thinkingEnabled := buildMessages(lines, mode)

	start := sigil.GenerationStart{
		ID:                  id,
		ConversationID:      conversationID,
		ConversationTitle:   in.Meta.Title,
		AgentName:           AgentName,
		Mode:                sigil.GenerationModeSync,
		OperationName:       "generateText",
		Model:               model,
		SystemPrompt:        systemPrompt,
		Tools:               tools,
		ParentGenerationIDs: parentIDs,
		ThinkingEnabled:     thinkingEnabled,
		Tags:                cloneStringMap(tags),
		Metadata:            cloneAnyMap(metadata),
		StartedAt:           startedAt,
		ContentCapture:      mode,
	}
	gen := sigil.Generation{
		ID:                  id,
		ConversationID:      conversationID,
		ConversationTitle:   in.Meta.Title,
		AgentName:           AgentName,
		Mode:                sigil.GenerationModeSync,
		OperationName:       "generateText",
		Model:               model,
		ResponseModel:       apiName,
		SystemPrompt:        systemPrompt,
		Input:               input,
		Output:              output,
		Tools:               tools,
		ParentGenerationIDs: parentIDs,
		ThinkingEnabled:     thinkingEnabled,
		Usage:               usage,
		StopReason:          "completed",
		StartedAt:           startedAt,
		CompletedAt:         completedAt,
		Tags:                cloneStringMap(tags),
		Metadata:            cloneAnyMap(metadata),
	}
	return Mapped{Start: start, Generation: gen}
}

// turnUsage is the per-turn delta of vibe's session-token totals against
// the prior state snapshot. We use the delta (not stats.last_turn_*)
// because last_turn_* covers only the final LLM call inside a multi-step
// tool loop; the delta covers the whole turn.
//
// Two failure modes are handled instead of blindly trusting the delta:
//   - No prior snapshot (priorFound=false) on turn >1 means state was lost
//     mid-session. Subtracting from zero would bill the entire cumulative
//     session to this one turn, so fall back to the last-turn figures.
//   - A negative delta means the totals regressed (session reset or a
//     stale/out-of-order snapshot); the delta is meaningless, so fall back
//     to the last-turn figures rather than emit a silently under-counted
//     turn.
func turnUsage(stats meta.Stats, prior state.Session, priorFound bool) sigil.TokenUsage {
	if !priorFound {
		if stats.Steps > 1 {
			return lastTurnUsage(stats)
		}
		return tokenUsage(stats.SessionPromptTokens, stats.SessionCompletionTokens)
	}
	in := stats.SessionPromptTokens - prior.SessionPromptTokens
	out := stats.SessionCompletionTokens - prior.SessionCompletionTokens
	if in < 0 || out < 0 {
		return lastTurnUsage(stats)
	}
	return tokenUsage(in, out)
}

func lastTurnUsage(stats meta.Stats) sigil.TokenUsage {
	return tokenUsage(stats.LastTurnPromptTokens, stats.LastTurnCompletionTokens)
}

func tokenUsage(in, out int64) sigil.TokenUsage {
	if in < 0 {
		in = 0
	}
	if out < 0 {
		out = 0
	}
	return sigil.TokenUsage{
		InputTokens:  in,
		OutputTokens: out,
		TotalTokens:  in + out,
	}
}

// buildMetadata assembles the generation metadata: the parent-session hint
// string, the per-turn USD cost, and any tool-call failures this turn
// produced. Returns nil when there is nothing to attach so the export omits
// an empty metadata object.
func buildMetadata(in Inputs) map[string]any {
	metadata := map[string]any{}
	if in.ParentSessionID != "" {
		metadata["vibe.parent_session_id"] = in.ParentSessionID
	}
	// Reparented turns lose the child session id from conversation_id; keep
	// it here so the child session is still recoverable.
	if in.ParentGenerationID != "" && in.ParentSessionID != "" {
		metadata["vibe.child_session_id"] = in.SessionID
	}
	if cost := turnCost(in.Meta.Stats, in.PriorState, in.PriorStateFound); cost > 0 {
		metadata["vibe.cost_usd"] = cost
	}
	addToolFailureCounts(metadata, in.Meta.Stats, in.PriorState, in.PriorStateFound)
	if len(metadata) == 0 {
		return nil
	}
	return metadata
}

// turnCost is the per-turn USD cost: the delta of vibe's session_cost
// against the prior snapshot. Mirrors turnUsage's state-loss handling so a
// lost snapshot mid-session reports nothing rather than billing the whole
// session to one turn; on the first turn the full session_cost applies.
func turnCost(stats meta.Stats, prior state.Session, priorFound bool) float64 {
	if !priorFound {
		if stats.Steps > 1 {
			return 0
		}
		return stats.SessionCost
	}
	if delta := stats.SessionCost - prior.SessionCost; delta > 0 {
		return delta
	}
	return 0
}

// addToolFailureCounts records the per-turn count of rejected, hook-denied,
// and failed tool calls (the delta of the session-wide counters). Without a
// prior snapshot the delta is unknowable, so nothing is recorded.
func addToolFailureCounts(metadata map[string]any, stats meta.Stats, prior state.Session, priorFound bool) {
	if !priorFound {
		return
	}
	add := func(key string, now, was int64) {
		if d := now - was; d > 0 {
			metadata[key] = d
		}
	}
	add("vibe.tool_calls_rejected", stats.ToolCallsRejected, prior.ToolCallsRejected)
	add("vibe.tool_calls_hook_denied", stats.ToolCallsHookDenied, prior.ToolCallsHookDenied)
	add("vibe.tool_calls_failed", stats.ToolCallsFailed, prior.ToolCallsFailed)
}

// linesForMapping selects which transcript lines become the generation's
// messages. Normally that is every new line. But when state was lost
// mid-session (no prior snapshot yet stats.steps > 1) the handler reads the
// whole transcript from offset zero and usage falls back to the last turn;
// mapping every line would bundle the entire history into one generation
// labelled with last-turn usage. In that case map only the latest turn so
// messages and usage describe the same turn.
func linesForMapping(lines []transcript.Line, stats meta.Stats, priorFound bool) []transcript.Line {
	if priorFound || stats.Steps <= 1 {
		return lines
	}
	return latestTurnLines(lines)
}

// latestTurnLines returns the trailing slice starting at the last
// non-injected user message, i.e. the most recent turn. Falls back to all
// lines when no user message is found.
func latestTurnLines(lines []transcript.Line) []transcript.Line {
	for i, l := range slices.Backward(lines) {
		if l.Role == "user" && !l.Injected && strings.TrimSpace(l.Content) != "" {
			return lines[i:]
		}
	}
	return lines
}

// buildMessages converts transcript lines into Sigil input/output messages.
// Order convention matches the codex mapper: user prompts and tool
// results land in Input; assistant text and tool calls land in Output.
//
// Assistant reasoning_content becomes a thinking part (content modes only);
// thinkingEnabled is set whenever any assistant line carried reasoning, even
// in MetadataOnly mode where the text itself is dropped, so the fact that
// the model reasoned is still observable.
//
// In MetadataOnly mode all message text, reasoning, and tool
// arguments/results are dropped but the structural shape (assistant called
// tool X, tool returned, assistant replied) is preserved.
func buildMessages(lines []transcript.Line, mode sigil.ContentCaptureMode) (input, output []sigil.Message, thinkingEnabled *bool) {
	red := redact.New()
	contentMode := mode == sigil.ContentCaptureModeFull || mode == sigil.ContentCaptureModeNoToolContent
	cleanText := func(s string) string {
		if contentMode {
			return red.Redact(s)
		}
		return ""
	}
	anyReasoning := false
	for _, l := range lines {
		switch l.Role {
		case "user":
			if l.Injected {
				continue
			}
			if strings.TrimSpace(l.Content) == "" {
				continue
			}
			if mode == sigil.ContentCaptureModeMetadataOnly {
				continue
			}
			input = append(input, sigil.UserTextMessage(cleanText(l.Content)))
		case "assistant":
			// Assistant turns come in two shapes: a tool-call message
			// (no Content, ToolCalls populated) or a final text message
			// (Content populated). Either may also carry reasoning_content,
			// emitted as a leading thinking part in content modes.
			if strings.TrimSpace(l.ReasoningContent) != "" {
				anyReasoning = true
			}
			var parts []sigil.Part
			if contentMode {
				if r := strings.TrimSpace(l.ReasoningContent); r != "" {
					parts = append(parts, sigil.ThinkingPart(red.Redact(l.ReasoningContent)))
				}
			}
			if len(l.ToolCalls) > 0 {
				for _, tc := range l.ToolCalls {
					call := sigil.ToolCall{ID: tc.ID, Name: tc.Function.Name}
					if mode == sigil.ContentCaptureModeFull && tc.Function.Arguments != "" {
						// vibe encodes function.arguments as a JSON-encoded
						// string already, so the raw bytes are valid JSON and
						// go straight into InputJSON without re-encoding.
						call.InputJSON = []byte(tc.Function.Arguments)
					}
					parts = append(parts, sigil.ToolCallPart(call))
				}
				output = append(output, sigil.Message{Role: sigil.RoleAssistant, Parts: parts})
				continue
			}
			if strings.TrimSpace(l.Content) == "" {
				// A reasoning-only assistant line still carries its thinking
				// part in content modes; otherwise there is nothing to emit.
				if len(parts) > 0 {
					output = append(output, sigil.Message{Role: sigil.RoleAssistant, Parts: parts})
				}
				continue
			}
			if mode == sigil.ContentCaptureModeMetadataOnly {
				continue
			}
			parts = append(parts, sigil.TextPart(cleanText(l.Content)))
			output = append(output, sigil.Message{Role: sigil.RoleAssistant, Parts: parts})
		case "tool":
			if mode == sigil.ContentCaptureModeMetadataOnly {
				// Still preserve the structural tool-result so the
				// conversation does not look like an unfinished call.
				input = append(input, sigil.Message{
					Role: sigil.RoleTool,
					Parts: []sigil.Part{sigil.ToolResultPart(sigil.ToolResult{
						ToolCallID: l.ToolCallID,
						Name:       l.Name,
					})},
				})
				continue
			}
			result := sigil.ToolResult{
				ToolCallID: l.ToolCallID,
				Name:       l.Name,
			}
			if mode == sigil.ContentCaptureModeFull {
				result.Content = red.Redact(l.Content)
			}
			input = append(input, sigil.Message{
				Role:  sigil.RoleTool,
				Parts: []sigil.Part{sigil.ToolResultPart(result)},
			})
		}
	}
	if anyReasoning {
		t := true
		thinkingEnabled = &t
	}
	return input, output, thinkingEnabled
}

// buildToolDefinitions copies meta.json's tools_available[] to the
// Sigil ToolDefinition shape. Parameters land in InputSchema verbatim.
func buildToolDefinitions(tools []meta.ToolDef) []sigil.ToolDefinition {
	if len(tools) == 0 {
		return nil
	}
	out := make([]sigil.ToolDefinition, 0, len(tools))
	for _, t := range tools {
		if t.Function.Name == "" {
			continue
		}
		def := sigil.ToolDefinition{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Type:        t.Type,
		}
		if def.Type == "" {
			def.Type = "function"
		}
		if len(t.Function.Parameters) > 0 {
			def.InputSchema = append([]byte(nil), t.Function.Parameters...)
		}
		out = append(out, def)
	}
	return out
}

// GenerationID is a stable per-turn ID derived from session_id and the
// turn sequence so reruns of the same hook against the same transcript
// produce the same ID (and Sigil dedupes them on the server side).
func GenerationID(sessionID string, turnSeq int) string {
	sum := sha256.Sum256([]byte(sessionID + "\x00" + strconv.Itoa(turnSeq)))
	return "vibe-" + hex.EncodeToString(sum[:])[:24]
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	maps.Copy(out, in)
	return out
}

func cloneAnyMap(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	maps.Copy(out, in)
	return out
}
