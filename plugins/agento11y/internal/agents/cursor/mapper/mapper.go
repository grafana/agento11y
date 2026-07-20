// Package mapper turns the on-disk Fragment + Session + StopPayload into a
// Sigil Generation suitable for emission via the Go SDK. Unlike the
// claudecode mapper there is no redactor — content passes through verbatim
// in `full` mode.
package mapper

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/grafana/agento11y/go/agento11y"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/fragment"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/tags"
	"github.com/grafana/agento11y/plugins/agento11y/internal/gitbranch"
	"github.com/grafana/agento11y/plugins/agento11y/internal/mapperutil"
	"github.com/grafana/agento11y/plugins/agento11y/internal/timeutil"
)

var errCursorStop = errors.New("cursor_stop_error")

// AgentName is the value reported as `agent_name` on every emitted generation.
const AgentName = "cursor"

// StopStatus is the normalized stop reason ("completed" / "aborted" / "error").
type StopStatus string

const (
	StopStatusCompleted StopStatus = "completed"
	StopStatusAborted   StopStatus = "aborted"
	StopStatusError     StopStatus = "error"
)

// StopInput carries the fields handleStop / sessionEnd extract from Cursor's
// stop payload and pass into the mapper. The fragment may also carry a
// pendingStopStatus (set by handleStop on flush failure) that sessionEnd
// passes through here.
type StopInput struct {
	Status string
	// Error is the raw `error` field from Cursor's payload (string OR
	// {message, code}). Forwarded verbatim — extractCallError parses it.
	Error []byte
}

// Inputs collects everything mapFragment needs.
type Inputs struct {
	Fragment       *fragment.Fragment
	Session        *fragment.Session
	Stop           *StopInput
	ContentCapture agento11y.ContentCaptureMode
	UserIDOverride string
	Now            time.Time
}

// Mapped is the mapper's output: the start seed, the full Generation for the
// recorder's SetResult, and the resolved stop status / call error.
type Mapped struct {
	Start      agento11y.GenerationStart
	Generation agento11y.Generation
	StopStatus StopStatus
	CallError  error
}

// MapFragment builds the Sigil Generation for a finished turn.
func MapFragment(in Inputs) Mapped {
	frag := in.Fragment
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}

	completedAt := timeutil.ParseTimestamp(frag.LastEventAt, now)
	startedAt := timeutil.ParseTimestamp(frag.StartedAt, completedAt)

	// Provider/model fallback: SDK validation requires both to be non-empty.
	provider := frag.Provider
	if provider == "" {
		provider = mapperutil.InferProvider(frag.Model)
	}
	if provider == "" {
		provider = "cursor"
	}
	modelName := frag.Model
	if modelName == "" {
		modelName = "unknown"
	}
	model := agento11y.ModelRef{Provider: provider, Name: modelName}

	stopStatus := resolveStopStatus(in.Stop)

	var workspaceRoot string
	if in.Session != nil && len(in.Session.WorkspaceRoots) > 0 {
		workspaceRoot = in.Session.WorkspaceRoots[0]
	}
	var cursorVersion, userEmail, conversationTitle string
	var isBackgroundAgent bool
	if in.Session != nil {
		cursorVersion = in.Session.CursorVersion
		userEmail = in.Session.UserEmail
		isBackgroundAgent = in.Session.IsBackgroundAgent
		conversationTitle = in.Session.ConversationTitle
	}

	tagMap := tags.Build(tags.BuiltinInputs{
		WorkspaceRoot:     workspaceRoot,
		Cwd:               firstToolCwd(frag.Tools),
		GitBranch:         gitbranch.Resolve(workspaceRoot),
		IsBackgroundAgent: isBackgroundAgent,
	})

	uid := resolveUserID(in.UserIDOverride, userEmail)

	toolDefs := buildToolDefinitions(frag.Tools)

	var thinkingEnabled *bool
	if frag.ThinkingPresent {
		v := true
		thinkingEnabled = &v
	}

	start := agento11y.GenerationStart{
		ID:                frag.GenerationID,
		ConversationID:    frag.ConversationID,
		ConversationTitle: conversationTitle,
		UserID:            uid,
		AgentName:         AgentName,
		AgentVersion:      cursorVersion,
		EffectiveVersion:  cursorVersion,
		Mode:              agento11y.GenerationModeSync,
		OperationName:     "generateText",
		Model:             model,
		Tools:             toolDefs,
		ThinkingEnabled:   thinkingEnabled,
		Tags:              tagMap,
		StartedAt:         startedAt,
		ContentCapture:    in.ContentCapture,
	}

	input, output := buildMessages(frag, in.ContentCapture)

	gen := agento11y.Generation{
		ID:                frag.GenerationID,
		ConversationID:    frag.ConversationID,
		ConversationTitle: conversationTitle,
		UserID:            uid,
		AgentName:         AgentName,
		AgentVersion:      cursorVersion,
		EffectiveVersion:  cursorVersion,
		Mode:              agento11y.GenerationModeSync,
		OperationName:     "generateText",
		Model:             model,
		ResponseModel:     modelName,
		Input:             input,
		Output:            output,
		Tools:             toolDefs,
		ThinkingEnabled:   thinkingEnabled,
		Usage:             mapTokenUsage(frag.TokenUsage),
		StopReason:        string(stopStatus),
		StartedAt:         startedAt,
		CompletedAt:       completedAt,
		Tags:              tagMap,
	}

	mapped := Mapped{
		Start:      start,
		Generation: gen,
		StopStatus: stopStatus,
	}
	if stopStatus == StopStatusError {
		mapped.CallError = extractCallError(in.Stop)
	}
	return mapped
}

// resolveStopStatus normalizes Cursor's stop.status to the subset Sigil uses.
// Unknown values (and missing payloads) → "completed" so we never silently
// drop a turn.
func resolveStopStatus(stop *StopInput) StopStatus {
	if stop == nil {
		return StopStatusCompleted
	}
	switch strings.ToLower(strings.TrimSpace(stop.Status)) {
	case "aborted", "cancelled", "canceled":
		return StopStatusAborted
	case "error", "failed":
		return StopStatusError
	default:
		// "", "completed", "success", "ok", or any unrecognized value.
		return StopStatusCompleted
	}
}

// extractCallError reads Cursor's `error` field (string or {message, code})
// from the StopInput. Returns errCursorStop when nothing parseable is
// available.
func extractCallError(stop *StopInput) error {
	if stop == nil || len(stop.Error) == 0 {
		return errCursorStop
	}
	var asString string
	if err := json.Unmarshal(stop.Error, &asString); err == nil && asString != "" {
		return errors.New(asString)
	}
	var asObj struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(stop.Error, &asObj); err == nil && asObj.Message != "" {
		return errors.New(asObj.Message)
	}
	return errCursorStop
}

// resolveUserID picks the user id for emitted generations: SIGIL_USER_ID
// override wins, falling back to the payload's user_email. Whitespace-only
// values are treated as unset. Cursor's payload carries user_email directly,
// so unlike claude-code there's no ~/.claude.json fallback.
func resolveUserID(override, payloadEmail string) string {
	if v := strings.TrimSpace(override); v != "" {
		return v
	}
	return strings.TrimSpace(payloadEmail)
}

func firstToolCwd(tools []fragment.ToolRecord) string {
	for i := range tools {
		if tools[i].Cwd != "" {
			return tools[i].Cwd
		}
	}
	return ""
}

// buildToolDefinitions deduplicates tool names across the fragment and emits
// a sorted slice for stable output (tests, log diffing, dashboards).
func buildToolDefinitions(tools []fragment.ToolRecord) []agento11y.ToolDefinition {
	names := make([]string, len(tools))
	for i := range tools {
		names[i] = tools[i].ToolName
	}
	return mapperutil.SortedToolDefinitions(names)
}

func buildMessages(frag *fragment.Fragment, mode agento11y.ContentCaptureMode) (input, output []agento11y.Message) {
	// Normalize so the rest of this function only deals with the three real
	// content-gating modes. envconfig.ResolveContentMode does this at the
	// config layer too, but mappers re-apply it for callers (including tests)
	// that build Inputs directly. NormalizePayloadContentMode also collapses
	// FullWithMetadataSpans to Full because the two modes differ only in OTel
	// span exposure; the gRPC payload buildMessages produces is identical.
	mode = mapperutil.NormalizePayloadContentMode(mode)

	// User prompt → user input message. Dropped in metadata-only mode.
	if mode != agento11y.ContentCaptureModeMetadataOnly && strings.TrimSpace(frag.UserPrompt) != "" {
		input = append(input, agento11y.Message{
			Role: agento11y.RoleUser,
			Parts: []agento11y.Part{
				agento11y.TextPart(frag.UserPrompt),
			},
		})
	}

	// Tool calls + results, interleaved per Sigil's convention:
	// assistant → tool_call, then a tool message → tool_result.
	for i := range frag.Tools {
		t := &frag.Tools[i]
		if t.ToolName == "" {
			continue
		}
		assistantParts := []agento11y.Part{
			{
				Kind: agento11y.PartKindToolCall,
				ToolCall: &agento11y.ToolCall{
					ID:   t.ToolUseID,
					Name: t.ToolName,
					InputJSON: func() []byte {
						if mode == agento11y.ContentCaptureModeFull {
							return t.ToolInput
						}
						return nil
					}(),
				},
			},
		}
		output = append(output, agento11y.Message{
			Role:  agento11y.RoleAssistant,
			Parts: assistantParts,
		})

		// no_tool_content keeps the tool_result skeleton (so consumers see the
		// call completed) but strips content. metadata_only drops the result
		// message entirely. full sends content as-is.
		switch mode {
		case agento11y.ContentCaptureModeFull:
			input = append(input, agento11y.Message{
				Role: agento11y.RoleTool,
				Parts: []agento11y.Part{
					{
						Kind: agento11y.PartKindToolResult,
						ToolResult: &agento11y.ToolResult{
							ToolCallID:  t.ToolUseID,
							Name:        t.ToolName,
							ContentJSON: t.ToolOutput,
							IsError:     t.Status == "error",
						},
					},
				},
			})
		case agento11y.ContentCaptureModeNoToolContent:
			input = append(input, agento11y.Message{
				Role: agento11y.RoleTool,
				Parts: []agento11y.Part{
					{
						Kind: agento11y.PartKindToolResult,
						ToolResult: &agento11y.ToolResult{
							ToolCallID: t.ToolUseID,
							Name:       t.ToolName,
							IsError:    t.Status == "error",
						},
					},
				},
			})
		case agento11y.ContentCaptureModeMetadataOnly:
			// Drop the tool_result entirely.
		default:
			// Default and FullWithMetadataSpans are normalized away above.
		}
	}

	// Assistant text. Concatenate segments in arrival order. Dropped in
	// metadata-only mode.
	if mode != agento11y.ContentCaptureModeMetadataOnly && len(frag.Assistant) > 0 {
		var b strings.Builder
		for _, seg := range frag.Assistant {
			b.WriteString(seg.Text)
		}
		text := b.String()
		if strings.TrimSpace(text) != "" {
			output = append(output, agento11y.Message{
				Role:  agento11y.RoleAssistant,
				Parts: []agento11y.Part{agento11y.TextPart(text)},
			})
		}
	}

	return input, output
}

func mapTokenUsage(t *fragment.TokenCounts) agento11y.TokenUsage {
	if t == nil {
		return agento11y.TokenUsage{}
	}
	var u agento11y.TokenUsage
	if t.InputTokens != nil {
		u.InputTokens = *t.InputTokens
	}
	if t.OutputTokens != nil {
		u.OutputTokens = *t.OutputTokens
	}
	if t.CacheReadTokens != nil {
		u.CacheReadInputTokens = *t.CacheReadTokens
	}
	if t.CacheWriteTokens != nil {
		u.CacheWriteInputTokens = *t.CacheWriteTokens
	}
	if u.InputTokens != 0 || u.OutputTokens != 0 {
		u.TotalTokens = u.InputTokens + u.OutputTokens
	}
	return u
}
