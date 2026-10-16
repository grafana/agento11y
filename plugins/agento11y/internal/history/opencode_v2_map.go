package history

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/opencode/sessiondb"
)

func opencodeV2Generation(sess SessionPreview, msg sessiondb.V2Message, input []agento11y.Message, source SourceRef, lineage opencodeLineage, parent, cwd string) HistoricalGeneration {
	usage, approximate := opencodeV2Usage(msg.Tokens)
	metadata := map[string]any{
		"opencode.input_source":                "persisted_user_delta",
		"opencode.completion_time_approximate": msg.Time.Streamed == nil,
		"opencode.settlement":                  "history",
	}
	if msg.Cost != nil {
		metadata["cost_usd"] = *msg.Cost
	}
	if lineage.parentSessionID != "" {
		metadata["opencode.parent_session_id"] = lineage.parentSessionID
	}
	if lineage.conversationID != sess.SessionID {
		metadata["opencode.child_session_id"] = sess.SessionID
	}
	output, tools := opencodeV2Output(msg.Content)
	gen := agento11y.Generation{
		ID: source.GenerationID(), ConversationID: lineage.conversationID,
		AgentName: opencodeAgentName(msg.Agent), Mode: agento11y.GenerationModeStream,
		OperationName: "streamText", Model: agento11y.ModelRef{Provider: msg.Model.ProviderID, Name: msg.Model.ID},
		ResponseModel: msg.Model.ID, Input: input, Output: output, Tools: tools,
		Usage: usage, StopReason: msg.Finish, Metadata: metadata,
		Tags: opencodeTags(cwd, lineage.parentSessionID != ""),
	}
	if lineage.conversationID == sess.SessionID {
		gen.ConversationTitle = lineage.conversationTitle
	}
	if msg.Error != nil {
		gen.CallError = msg.Error.Type
	}
	if parent != "" {
		gen.ParentGenerationIDs = []string{parent}
	}
	if msg.Time.Created > 0 {
		gen.StartedAt = time.UnixMilli(msg.Time.Created).UTC()
	}
	completed := msg.Time.Streamed
	if completed == nil {
		completed = msg.Time.Completed
	}
	if completed != nil && *completed > 0 {
		gen.CompletedAt = time.UnixMilli(*completed).UTC()
	}
	return HistoricalGeneration{Source: source, Gen: gen, Quality: QualityReport{
		ApproxStartedAt: gen.StartedAt.IsZero(), ApproxCompletedAt: gen.CompletedAt.IsZero() || msg.Time.Streamed == nil,
		ApproxUsage: approximate, MissingModel: strings.TrimSpace(msg.Model.ID) == "",
	}}
}

func opencodeV2Usage(tokens *sessiondb.V2Tokens) (agento11y.TokenUsage, bool) {
	if tokens == nil {
		return agento11y.TokenUsage{}, true
	}
	value := func(n *int64) int64 {
		if n == nil {
			return 0
		}
		return *n
	}
	input := value(tokens.Input) + value(tokens.Cache.Read) + value(tokens.Cache.Write)
	output := value(tokens.Output) + value(tokens.Reasoning)
	return agento11y.TokenUsage{
		InputTokens: input, OutputTokens: output, TotalTokens: input + output,
		CacheReadInputTokens: value(tokens.Cache.Read), CacheWriteInputTokens: value(tokens.Cache.Write),
		ReasoningTokens: value(tokens.Reasoning), InputSemantics: agento11y.TokenInputSemanticsInclusive,
	}, tokens.Input == nil || tokens.Output == nil || tokens.Reasoning == nil || tokens.Cache.Read == nil || tokens.Cache.Write == nil
}

func opencodeV2Output(content []sessiondb.V2Content) ([]agento11y.Message, []agento11y.ToolDefinition) {
	var messages []agento11y.Message
	names := map[string]bool{}
	for _, block := range content {
		switch block.Type {
		case "text", "reasoning":
			if strings.TrimSpace(block.Text) == "" {
				continue
			}
			part := agento11y.Part{Kind: agento11y.PartKindText, Text: block.Text}
			if block.Type == "reasoning" {
				part = agento11y.Part{Kind: agento11y.PartKindThinking, Thinking: block.Text}
			}
			messages = append(messages, agento11y.Message{Role: agento11y.RoleAssistant, Parts: []agento11y.Part{part}})
		case "tool":
			state := block.State
			switch state.Status {
			case "streaming", "running", "completed", "error":
			default:
				continue
			}
			if block.Name != "" {
				names[block.Name] = true
			}
			input := state.Input
			if state.Status == "streaming" {
				var text string
				if json.Unmarshal(input, &text) == nil && json.Valid([]byte(text)) {
					input = json.RawMessage(text)
				}
			}
			messages = append(messages, agento11y.Message{Role: agento11y.RoleAssistant, Parts: []agento11y.Part{{
				Kind: agento11y.PartKindToolCall, ToolCall: &agento11y.ToolCall{ID: block.ID, Name: block.Name, InputJSON: input},
			}}})
			if state.Status != "completed" && state.Status != "error" {
				continue
			}
			var text []string
			for _, item := range state.Content {
				if item.Type == "text" && strings.TrimSpace(item.Text) != "" {
					text = append(text, item.Text)
				}
			}
			result := &agento11y.ToolResult{ToolCallID: block.ID, Name: block.Name, Content: strings.Join(text, "\n"),
				IsError: state.Status == "error" || block.Name == "invalid"}
			if (block.Name == "bash" || block.Name == "shell") && state.Metadata.Exit != nil && *state.Metadata.Exit != 0 {
				result.IsError = true
			}
			if len(text) == 0 && state.Status == "error" && state.Error != nil {
				result.Content = state.Error.Message
			}
			messages = append(messages, agento11y.Message{Role: agento11y.RoleTool, Parts: []agento11y.Part{{Kind: agento11y.PartKindToolResult, ToolResult: result}}})
		}
	}
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	tools := make([]agento11y.ToolDefinition, 0, len(ordered))
	for _, name := range ordered {
		tools = append(tools, agento11y.ToolDefinition{Name: name, Type: "function"})
	}
	return messages, tools
}
