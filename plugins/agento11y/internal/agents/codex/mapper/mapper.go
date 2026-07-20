package mapper

import (
	"strconv"
	"strings"
	"time"

	"github.com/grafana/agento11y/go/agento11y"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/codex/codexlog"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/codex/fragment"
	"github.com/grafana/agento11y/plugins/agento11y/internal/gitbranch"
	"github.com/grafana/agento11y/plugins/agento11y/internal/mapperutil"
	"github.com/grafana/agento11y/plugins/agento11y/internal/redact"
	"github.com/grafana/agento11y/plugins/agento11y/internal/timeutil"
)

const (
	AgentName         = "codex"
	SubagentAgentName = AgentName + "/subagent"
)

type Inputs struct {
	Fragment       *fragment.Fragment
	SubagentLink   *fragment.SubagentLink
	TokenSnapshot  *codexlog.TokenSnapshot
	ContentCapture agento11y.ContentCaptureMode
	Now            time.Time
}

type Mapped struct {
	Start      agento11y.GenerationStart
	Generation agento11y.Generation
}

func Map(in Inputs) Mapped {
	frag := in.Fragment
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	completedAt := timeutil.ParseTimestamp(frag.CompletedAt, timeutil.ParseTimestamp(frag.LastEventAt, now))
	startedAt := timeutil.ParseTimestamp(frag.StartedAt, completedAt)
	startMode := mapperutil.NormalizeStartContentMode(in.ContentCapture)
	payloadMode := mapperutil.NormalizePayloadContentMode(in.ContentCapture)

	modelName := strings.TrimSpace(frag.Model)
	if modelName == "" {
		modelName = "unknown"
	}
	model := agento11y.ModelRef{Provider: mapperutil.InferProvider(modelName), Name: modelName}
	if model.Provider == "" {
		model.Provider = "codex"
	}

	id := GenerationID(frag.SessionID, frag.TurnID)
	tags := map[string]string{"entrypoint": "codex"}
	if frag.Cwd != "" {
		tags["cwd"] = frag.Cwd
	}
	if branch := gitbranch.Resolve(frag.Cwd); branch != "" {
		tags["git.branch"] = branch
	}
	if frag.Source != "" {
		tags["hook.source"] = frag.Source
	}
	tags["codex.stop_hook_active"] = strconv.FormatBool(frag.StopHookActive)

	tools := buildToolDefinitions(frag.Tools)
	input, output := buildMessages(frag, payloadMode)
	conversationID, agentName, parentIDs, metadata := linkFields(frag, in.SubagentLink, tags)
	usage, metadata := usageFields(in.TokenSnapshot, metadata)

	start := agento11y.GenerationStart{
		ID:                  id,
		ConversationID:      conversationID,
		AgentName:           agentName,
		Mode:                agento11y.GenerationModeSync,
		OperationName:       "generateText",
		Model:               model,
		Tools:               tools,
		ParentGenerationIDs: parentIDs,
		Tags:                mapperutil.Clone(tags),
		Metadata:            mapperutil.Clone(metadata),
		StartedAt:           startedAt,
		ContentCapture:      startMode,
	}
	gen := agento11y.Generation{
		ID:                  id,
		ConversationID:      conversationID,
		AgentName:           agentName,
		Mode:                agento11y.GenerationModeSync,
		OperationName:       "generateText",
		Model:               model,
		ResponseModel:       modelName,
		Input:               input,
		Output:              output,
		Tools:               tools,
		ParentGenerationIDs: parentIDs,
		Usage:               usage,
		StopReason:          "completed",
		StartedAt:           startedAt,
		CompletedAt:         completedAt,
		Tags:                mapperutil.Clone(tags),
		Metadata:            mapperutil.Clone(metadata),
	}
	return Mapped{Start: start, Generation: gen}
}

func linkFields(frag *fragment.Fragment, link *fragment.SubagentLink, tags map[string]string) (conversationID, agentName string, parentIDs []string, metadata map[string]any) {
	conversationID = frag.SessionID
	agentName = AgentName
	if link == nil || link.ParentSessionID == "" {
		return conversationID, agentName, nil, nil
	}

	agentName = SubagentAgentName
	tags["subagent"] = "true"
	tags["codex.thread_source"] = "subagent"
	tags["codex.link_source"] = "partial"
	if link.AgentRole != "" {
		tags["codex.agent_role"] = link.AgentRole
	}

	childSessionID := link.ChildSessionID
	if childSessionID == "" {
		childSessionID = frag.SessionID
	}
	metadata = map[string]any{
		"codex.child_session_id":  childSessionID,
		"codex.parent_session_id": link.ParentSessionID,
	}
	if link.AgentNickname != "" {
		metadata["codex.agent_nickname"] = link.AgentNickname
	}
	if link.AgentDepth != 0 {
		metadata["codex.agent_depth"] = link.AgentDepth
	}
	if link.Source != "" {
		metadata["codex.link_state_source"] = link.Source
	}
	if link.ParentTurnID != "" {
		metadata["codex.parent_turn_id"] = link.ParentTurnID
	}
	if link.SpawnCallID != "" {
		metadata["codex.spawn_call_id"] = link.SpawnCallID
	}

	if link.ParentGenerationID == "" {
		return conversationID, agentName, nil, metadata
	}
	conversationID = link.ParentSessionID
	parentIDs = []string{link.ParentGenerationID}
	tags["codex.link_source"] = "transcript"
	return conversationID, agentName, parentIDs, metadata
}

func usageFields(snapshot *codexlog.TokenSnapshot, metadata map[string]any) (agento11y.TokenUsage, map[string]any) {
	if snapshot == nil || !hasPositiveCodexUsage(snapshot.TurnUsage) {
		return agento11y.TokenUsage{}, metadata
	}
	usage := agento11y.TokenUsage{
		InputTokens:          snapshot.TurnUsage.InputTokens,
		OutputTokens:         snapshot.TurnUsage.OutputTokens,
		TotalTokens:          snapshot.TurnUsage.TotalTokens,
		CacheReadInputTokens: snapshot.TurnUsage.CachedInputTokens,
		ReasoningTokens:      snapshot.TurnUsage.ReasoningOutputTokens,
	}
	if metadata == nil {
		metadata = map[string]any{}
	}
	if snapshot.Source != "" {
		metadata["codex.token_usage.source"] = snapshot.Source
	}
	if snapshot.ModelContextWindow > 0 {
		metadata["codex.token_usage.context_window"] = snapshot.ModelContextWindow
	}
	metadata["codex.token_usage.total.input_tokens"] = snapshot.TotalUsage.InputTokens
	metadata["codex.token_usage.total.output_tokens"] = snapshot.TotalUsage.OutputTokens
	metadata["codex.token_usage.total.cached_input_tokens"] = snapshot.TotalUsage.CachedInputTokens
	metadata["codex.token_usage.total.reasoning_output_tokens"] = snapshot.TotalUsage.ReasoningOutputTokens
	metadata["codex.token_usage.total.total_tokens"] = snapshot.TotalUsage.TotalTokens
	return usage, metadata
}

func hasPositiveCodexUsage(u codexlog.TokenUsage) bool {
	return u.InputTokens > 0 ||
		u.CachedInputTokens > 0 ||
		u.OutputTokens > 0 ||
		u.ReasoningOutputTokens > 0 ||
		u.TotalTokens > 0
}

func buildMessages(frag *fragment.Fragment, mode agento11y.ContentCaptureMode) (input, output []agento11y.Message) {
	mode = mapperutil.NormalizePayloadContentMode(mode)
	red := redact.New()
	cleanText := func(s string) string {
		if mode == agento11y.ContentCaptureModeFull || mode == agento11y.ContentCaptureModeNoToolContent {
			return red.Redact(s)
		}
		return ""
	}

	if mode != agento11y.ContentCaptureModeMetadataOnly && strings.TrimSpace(frag.Prompt) != "" {
		input = append(input, agento11y.UserTextMessage(cleanText(frag.Prompt)))
	}
	for i := range frag.Tools {
		t := &frag.Tools[i]
		if t.ToolName == "" {
			continue
		}
		call := agento11y.ToolCall{ID: t.ToolUseID, Name: t.ToolName}
		if mode == agento11y.ContentCaptureModeFull && len(t.ToolInput) > 0 {
			call.InputJSON = red.RedactJSON(t.ToolInput)
		}
		output = append(output, agento11y.Message{Role: agento11y.RoleAssistant, Parts: []agento11y.Part{agento11y.ToolCallPart(call)}})
		if mode == agento11y.ContentCaptureModeMetadataOnly {
			continue
		}
		result := agento11y.ToolResult{ToolCallID: t.ToolUseID, Name: t.ToolName, IsError: t.Status == "error"}
		if mode == agento11y.ContentCaptureModeFull && len(t.ToolResponse) > 0 {
			result.ContentJSON = red.RedactJSON(t.ToolResponse)
		}
		input = append(input, agento11y.Message{Role: agento11y.RoleTool, Parts: []agento11y.Part{agento11y.ToolResultPart(result)}})
	}
	if mode != agento11y.ContentCaptureModeMetadataOnly && strings.TrimSpace(frag.LastAssistantMessage) != "" {
		output = append(output, agento11y.AssistantTextMessage(cleanText(frag.LastAssistantMessage)))
	}
	return input, output
}

func buildToolDefinitions(tools []fragment.ToolRecord) []agento11y.ToolDefinition {
	names := make([]string, len(tools))
	for i := range tools {
		names[i] = tools[i].ToolName
	}
	return mapperutil.SortedToolDefinitions(names)
}

func GenerationID(sessionID, turnID string) string {
	return mapperutil.DeterministicID("codex", sessionID, turnID)
}
