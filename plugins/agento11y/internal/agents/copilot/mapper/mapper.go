package mapper

import (
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/grafana/agento11y/go/sigil"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/copilot/fragment"
	"github.com/grafana/agento11y/plugins/agento11y/internal/gitbranch"
	"github.com/grafana/agento11y/plugins/agento11y/internal/mapperutil"
	"github.com/grafana/agento11y/plugins/agento11y/internal/redact"
	"github.com/grafana/agento11y/plugins/agento11y/internal/timeutil"
)

var errGitHubCopilot = errors.New("copilot_error")

const AgentName = "copilot"

type Inputs struct {
	Fragment       *fragment.Fragment
	Session        *fragment.Session
	ContentCapture sigil.ContentCaptureMode
	UserIDOverride string
	Now            time.Time
}

type Mapped struct {
	Start      sigil.GenerationStart
	Generation sigil.Generation
	CallError  error
}

func Map(in Inputs) Mapped {
	frag := in.Fragment
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	startMode := mapperutil.NormalizeStartContentMode(in.ContentCapture)
	payloadMode := mapperutil.NormalizePayloadContentMode(in.ContentCapture)
	completedAt := timeutil.ParseTimestamp(frag.CompletedAt, timeutil.ParseTimestamp(frag.LastEventAt, now))
	startedAt := timeutil.ParseTimestamp(frag.StartedAt, completedAt)

	model := sigil.ModelRef{
		Provider: strings.TrimSpace(frag.Provider),
		Name:     strings.TrimSpace(frag.Model),
	}
	providerReported := model.Provider != ""
	providerInferred := false
	if model.Provider == "" {
		if inferred := inferProvider(model.Name); inferred != "" {
			model.Provider = inferred
			providerInferred = true
		}
	}
	metadata := map[string]any{
		"copilot.assistant_text_available": strings.TrimSpace(frag.AssistantText) != "",
		"copilot.turn_id":                  frag.TurnID,
		"copilot.tool_count":               len(frag.Tools),
		"copilot.error_count":              len(frag.Errors),
		"copilot.subagent_count":           len(frag.Subagents),
		"copilot.model_reported":           model.Name != "",
		"copilot.provider_reported":        providerReported,
		"copilot.transcript_path_present":  strings.TrimSpace(frag.TranscriptPath) != "",
	}
	if model.Provider == "" {
		model.Provider = AgentName
	}
	if model.Name == "" {
		model.Name = "unknown"
	}
	if providerInferred {
		metadata["copilot.provider_inferred"] = true
	}
	if frag.AgentVersion != "" {
		metadata["copilot.agent_version"] = frag.AgentVersion
	}
	if frag.ReasoningEffort != "" {
		metadata["copilot.reasoning_effort"] = frag.ReasoningEffort
	}
	if frag.NativeTurnID != "" {
		metadata["copilot.native_turn_id"] = frag.NativeTurnID
	}
	if frag.InteractionID != "" {
		metadata["copilot.interaction_id"] = frag.InteractionID
	}
	if frag.RequestID != "" {
		metadata["copilot.request_id"] = frag.RequestID
	}
	if frag.MessageID != "" {
		metadata["copilot.message_id"] = frag.MessageID
	}
	if frag.StopReason != "" {
		metadata["copilot.stop_reason"] = frag.StopReason
	}
	if surface := frag.Surface; surface != "" {
		metadata["copilot.hook_surface"] = surface
	} else if in.Session != nil && in.Session.Surface != "" {
		metadata["copilot.hook_surface"] = in.Session.Surface
	}
	if in.Session != nil {
		if in.Session.Source != "" {
			metadata["copilot.session_source"] = in.Session.Source
		}
		if in.Session.TranscriptPath != "" {
			metadata["copilot.session_transcript_path_present"] = true
		}
	}

	fatalError, callErr := mapCallError(frag.Errors)
	if fatalError != nil {
		metadata["error.type"] = fatalError.Name
		metadata["error.category"] = fatalError.Context
	}
	if len(frag.Errors) > 0 {
		metadata["copilot.errors"] = buildErrorMetadata(frag.Errors)
	}
	if len(frag.Subagents) > 0 {
		metadata["copilot.subagents"] = buildSubagentMetadata(frag.Subagents)
	}

	tools := buildToolDefinitions(frag.Tools)
	tags := buildTags(frag, in.Session)
	input, output := buildMessages(frag, payloadMode)
	stopReason := strings.TrimSpace(frag.StopReason)
	if stopReason == "" {
		stopReason = "completed"
	}
	if callErr != nil {
		stopReason = "error"
	}

	start := sigil.GenerationStart{
		ID:             GenerationID(frag.SessionID, frag.TurnID),
		ConversationID: frag.SessionID,
		UserID:         strings.TrimSpace(in.UserIDOverride),
		AgentName:      AgentName,
		AgentVersion:   strings.TrimSpace(frag.AgentVersion),
		Mode:           sigil.GenerationModeSync,
		OperationName:  "generateText",
		Model:          model,
		Tools:          tools,
		Tags:           mapperutil.Clone(tags),
		Metadata:       mapperutil.Clone(metadata),
		StartedAt:      startedAt,
		ContentCapture: startMode,
	}

	gen := sigil.Generation{
		ID:             GenerationID(frag.SessionID, frag.TurnID),
		ConversationID: frag.SessionID,
		UserID:         strings.TrimSpace(in.UserIDOverride),
		AgentName:      AgentName,
		AgentVersion:   strings.TrimSpace(frag.AgentVersion),
		Mode:           sigil.GenerationModeSync,
		OperationName:  "generateText",
		Model:          model,
		ResponseID:     strings.TrimSpace(frag.RequestID),
		ResponseModel:  model.Name,
		Input:          input,
		Output:         output,
		Tools:          tools,
		Usage: sigil.TokenUsage{
			InputTokens:           derefInt64(frag.TokenUsage.InputTokens),
			OutputTokens:          derefInt64(frag.TokenUsage.OutputTokens),
			CacheReadInputTokens:  derefInt64(frag.TokenUsage.CacheReadInputTokens),
			CacheWriteInputTokens: derefInt64(frag.TokenUsage.CacheWriteInputTokens),
			ReasoningTokens:       derefInt64(frag.TokenUsage.ReasoningTokens),
			TotalTokens:           derefInt64(frag.TokenUsage.InputTokens) + derefInt64(frag.TokenUsage.OutputTokens),
		},
		StopReason:  stopReason,
		StartedAt:   startedAt,
		CompletedAt: completedAt,
		Tags:        mapperutil.Clone(tags),
		Metadata:    mapperutil.Clone(metadata),
	}

	return Mapped{Start: start, Generation: gen, CallError: callErr}
}

func buildTags(frag *fragment.Fragment, session *fragment.Session) map[string]string {
	tags := map[string]string{
		"entrypoint": "copilot",
	}
	cwd := frag.Cwd
	if cwd == "" && session != nil {
		cwd = session.Cwd
	}
	if cwd != "" {
		tags["cwd"] = cwd
	}
	if branch := gitbranch.Resolve(cwd); branch != "" {
		tags["git.branch"] = branch
	}
	if frag.Source != "" {
		tags["hook.source"] = frag.Source
	} else if session != nil && session.Source != "" {
		tags["hook.source"] = session.Source
	}
	if frag.Surface != "" {
		tags["hook.surface"] = frag.Surface
	} else if session != nil && session.Surface != "" {
		tags["hook.surface"] = session.Surface
	}
	if len(frag.Subagents) > 0 {
		tags["copilot.subagent_activity"] = "true"
	}
	return tags
}

func buildErrorMetadata(errors []fragment.ErrorRecord) []map[string]any {
	out := make([]map[string]any, 0, len(errors))
	for _, item := range errors {
		out = append(out, map[string]any{
			"context":     item.Context,
			"name":        item.Name,
			"recoverable": item.Recoverable,
			"timestamp":   item.Timestamp,
		})
	}
	return out
}

func buildSubagentMetadata(subagents []fragment.SubagentRecord) []map[string]any {
	out := make([]map[string]any, 0, len(subagents))
	for _, item := range subagents {
		out = append(out, map[string]any{
			"agent_name":              item.AgentName,
			"agent_display_name":      item.AgentDisplayName,
			"started_at":              item.StartedAt,
			"completed_at":            item.CompletedAt,
			"stop_reason":             item.StopReason,
			"transcript_path_present": strings.TrimSpace(item.TranscriptPath) != "",
		})
	}
	return out
}

func mapCallError(records []fragment.ErrorRecord) (*fragment.ErrorRecord, error) {
	for _, v := range slices.Backward(records) {
		item := v
		if item.Recoverable || item.Context == "tool_execution" {
			continue
		}
		msg := strings.TrimSpace(item.Message)
		if msg == "" {
			msg = strings.TrimSpace(item.Name)
		}
		if msg == "" {
			msg = errGitHubCopilot.Error()
		}
		return &item, errors.New(msg)
	}
	return nil, nil
}

func derefInt64(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

func buildMessages(frag *fragment.Fragment, mode sigil.ContentCaptureMode) (input, output []sigil.Message) {
	mode = mapperutil.NormalizePayloadContentMode(mode)
	red := redact.New()
	if mode != sigil.ContentCaptureModeMetadataOnly {
		prompt := strings.TrimSpace(frag.Prompt)
		if prompt == "" {
			prompt = strings.TrimSpace(frag.InitialPrompt)
		}
		if prompt != "" {
			input = append(input, sigil.UserTextMessage(red.Redact(prompt)))
		}
	}

	for i := range frag.Tools {
		t := &frag.Tools[i]
		if t.ToolName == "" {
			continue
		}
		call := sigil.ToolCall{ID: t.ToolUseID, Name: t.ToolName}
		if mode == sigil.ContentCaptureModeFull && len(t.ToolInput) > 0 {
			call.InputJSON = red.RedactJSON(t.ToolInput)
		}
		output = append(output, sigil.Message{Role: sigil.RoleAssistant, Parts: []sigil.Part{sigil.ToolCallPart(call)}})
		if mode == sigil.ContentCaptureModeMetadataOnly {
			continue
		}
		result := sigil.ToolResult{ToolCallID: t.ToolUseID, Name: t.ToolName, IsError: t.Status == "error"}
		if mode == sigil.ContentCaptureModeFull {
			if len(t.ToolResponse) > 0 {
				result.ContentJSON = red.RedactJSON(t.ToolResponse)
			} else if t.ErrorMessage != "" {
				result.Content = red.Redact(t.ErrorMessage)
			}
		}
		input = append(input, sigil.Message{Role: sigil.RoleTool, Parts: []sigil.Part{sigil.ToolResultPart(result)}})
	}
	if mode != sigil.ContentCaptureModeMetadataOnly {
		if assistantText := strings.TrimSpace(frag.AssistantText); assistantText != "" {
			output = append(output, sigil.AssistantTextMessage(red.Redact(assistantText)))
		}
	}
	return input, output
}

func buildToolDefinitions(tools []fragment.ToolRecord) []sigil.ToolDefinition {
	names := make([]string, len(tools))
	for i := range tools {
		names[i] = tools[i].ToolName
	}
	return mapperutil.SortedToolDefinitions(names)
}

func GenerationID(sessionID, turnID string) string {
	return mapperutil.DeterministicID("copilot", sessionID, turnID)
}

// inferProvider is intentionally stricter than mapperutil.InferProvider: it
// requires hyphenated prefixes ("gpt-", "claude-", "gemini-") and trims
// whitespace, so unknown providers fall through to the "copilot" fallback
// rather than being guessed from a loose substring match. Keep it local.
func inferProvider(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	switch {
	case model == "":
		return ""
	case strings.HasPrefix(model, "gpt-"), strings.HasPrefix(model, "o1"), strings.HasPrefix(model, "o3"), strings.HasPrefix(model, "o4"):
		return "openai"
	case strings.HasPrefix(model, "claude-"):
		return "anthropic"
	case strings.HasPrefix(model, "gemini-"):
		return "google"
	default:
		return ""
	}
}
