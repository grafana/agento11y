package mapper

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/grafana/sigil-sdk/go/sigil"

	"github.com/grafana/sigil-sdk/plugins/sigil/internal/agents/codex/codexlog"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/agents/codex/fragment"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/gitbranch"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/redact"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/timeutil"
)

const (
	AgentName         = "codex"
	SubagentAgentName = AgentName + "/subagent"
)

type Inputs struct {
	Fragment       *fragment.Fragment
	SubagentLink   *fragment.SubagentLink
	TokenSnapshot  *codexlog.TokenSnapshot
	ContentCapture sigil.ContentCaptureMode
	Now            time.Time
}

type Mapped struct {
	Start      sigil.GenerationStart
	Generation sigil.Generation
}

func Map(in Inputs) Mapped {
	frag := in.Fragment
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	completedAt := timeutil.ParseTimestamp(frag.CompletedAt, timeutil.ParseTimestamp(frag.LastEventAt, now))
	startedAt := timeutil.ParseTimestamp(frag.StartedAt, completedAt)
	mode := in.ContentCapture
	if mode == sigil.ContentCaptureModeDefault {
		mode = sigil.ContentCaptureModeMetadataOnly
	}

	modelName := strings.TrimSpace(frag.Model)
	if modelName == "" {
		modelName = "unknown"
	}
	model := sigil.ModelRef{Provider: inferProviderFromModel(modelName), Name: modelName}
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
	input, output := buildMessages(frag, mode)
	conversationID, agentName, parentIDs, metadata := linkFields(frag, in.SubagentLink, tags)
	usage, metadata := usageFields(in.TokenSnapshot, metadata)

	start := sigil.GenerationStart{
		ID:                  id,
		ConversationID:      conversationID,
		AgentName:           agentName,
		Mode:                sigil.GenerationModeSync,
		OperationName:       "generateText",
		Model:               model,
		Tools:               tools,
		ParentGenerationIDs: parentIDs,
		Tags:                cloneStringMap(tags),
		Metadata:            cloneAnyMap(metadata),
		StartedAt:           startedAt,
		ContentCapture:      mode,
	}
	gen := sigil.Generation{
		ID:                  id,
		ConversationID:      conversationID,
		AgentName:           agentName,
		Mode:                sigil.GenerationModeSync,
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
		Tags:                cloneStringMap(tags),
		Metadata:            cloneAnyMap(metadata),
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

func usageFields(snapshot *codexlog.TokenSnapshot, metadata map[string]any) (sigil.TokenUsage, map[string]any) {
	if snapshot == nil || !hasPositiveCodexUsage(snapshot.TurnUsage) {
		return sigil.TokenUsage{}, metadata
	}
	usage := sigil.TokenUsage{
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

func buildMessages(frag *fragment.Fragment, mode sigil.ContentCaptureMode) (input, output []sigil.Message) {
	if mode == sigil.ContentCaptureModeDefault {
		mode = sigil.ContentCaptureModeMetadataOnly
	}
	red := redact.New()
	cleanText := func(s string) string {
		if mode == sigil.ContentCaptureModeFull || mode == sigil.ContentCaptureModeNoToolContent {
			return red.Redact(s)
		}
		return ""
	}

	if mode != sigil.ContentCaptureModeMetadataOnly && strings.TrimSpace(frag.Prompt) != "" {
		input = append(input, sigil.UserTextMessage(cleanText(frag.Prompt)))
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
		if mode == sigil.ContentCaptureModeFull && len(t.ToolResponse) > 0 {
			result.ContentJSON = red.RedactJSON(t.ToolResponse)
		}
		input = append(input, sigil.Message{Role: sigil.RoleTool, Parts: []sigil.Part{sigil.ToolResultPart(result)}})
	}
	if mode != sigil.ContentCaptureModeMetadataOnly && strings.TrimSpace(frag.LastAssistantMessage) != "" {
		output = append(output, sigil.AssistantTextMessage(cleanText(frag.LastAssistantMessage)))
	}
	return input, output
}

func buildToolDefinitions(tools []fragment.ToolRecord) []sigil.ToolDefinition {
	seen := map[string]struct{}{}
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		if t.ToolName == "" {
			continue
		}
		if _, ok := seen[t.ToolName]; ok {
			continue
		}
		seen[t.ToolName] = struct{}{}
		names = append(names, t.ToolName)
	}
	sort.Strings(names)
	out := make([]sigil.ToolDefinition, 0, len(names))
	for _, name := range names {
		out = append(out, sigil.ToolDefinition{Name: name, Type: "function"})
	}
	return out
}

func GenerationID(sessionID, turnID string) string {
	sum := sha256.Sum256([]byte(sessionID + "\x00" + turnID))
	return "codex-" + hex.EncodeToString(sum[:])[:24]
}

func inferProviderFromModel(model string) string {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "claude"):
		return "anthropic"
	case strings.HasPrefix(m, "gpt"), strings.HasPrefix(m, "o1"), strings.HasPrefix(m, "o3"), strings.HasPrefix(m, "o4"):
		return "openai"
	case strings.Contains(m, "gemini"):
		return "google"
	}
	return ""
}
