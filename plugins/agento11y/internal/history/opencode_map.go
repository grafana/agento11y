package history

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/opencode/sessiondb"
)

type opencodeLineage struct {
	parentSessionID   string
	conversationID    string
	conversationTitle string
	spawnGenerationID string
}

func resolveOpenCodeLineage(ctx context.Context, store *sessiondb.Store, sessionID, sourcePath string) (opencodeLineage, error) {
	lineage := opencodeLineage{conversationID: sessionID}
	row, ok, err := store.Session(ctx, sessionID)
	if err != nil {
		return lineage, err
	}
	if !ok {
		return lineage, fmt.Errorf("opencode session %s no longer exists in %s", sessionID, sourcePath)
	}
	lineage.conversationTitle = openCodeConversationTitle(row.Title)
	if row.ParentID == "" || row.ParentID == sessionID {
		return lineage, nil
	}
	lineage.parentSessionID = row.ParentID

	current := sessionID
	seen := map[string]bool{current: true}
	for {
		currentRow, found, err := store.Session(ctx, current)
		if err != nil {
			return lineage, err
		}
		if !found || currentRow.ParentID == "" {
			lineage.conversationID = current
			break
		}
		if seen[currentRow.ParentID] {
			lineage.conversationID = current
			break
		}
		seen[currentRow.ParentID] = true
		current = currentRow.ParentID
		lineage.conversationID = current
	}

	messageID, found, err := store.SpawningMessageID(ctx, row.ParentID, sessionID)
	if err != nil || !found {
		return lineage, err
	}
	source, found, err := opencodeTerminalSource(ctx, store, row.ParentID, sourcePath, messageID)
	if err != nil || !found {
		return lineage, err
	}
	lineage.spawnGenerationID = source.GenerationID()
	return lineage, nil
}

func opencodeTerminalSource(ctx context.Context, store *sessiondb.Store, sessionID, sourcePath, targetMessageID string) (SourceRef, bool, error) {
	assistantIndex := 0
	for msg, err := range store.Messages(ctx, sessionID) {
		if err != nil {
			return SourceRef{}, false, err
		}
		if msg.Role != "assistant" {
			continue
		}
		turnIndex := assistantIndex
		assistantIndex++
		if !opencodeTerminalAssistant(msg) {
			continue
		}
		if msg.ID == targetMessageID {
			return SourceRef{
				Agent:        AgentOpenCode,
				SessionID:    sessionID,
				SourcePath:   sourcePath,
				TurnIndex:    turnIndex,
				TurnID:       msg.ID,
				TurnIDStable: true,
			}, true, nil
		}
	}
	return SourceRef{}, false, nil
}

func opencodeTerminalAssistant(msg sessiondb.Message) bool {
	return msg.Role == "assistant" && (msg.Time.Completed != nil || msg.Error != nil)
}

func opencodeGeneration(
	sess SessionPreview,
	msg sessiondb.Message,
	userParts, assistantParts []sessiondb.Part,
	source SourceRef,
	lineage opencodeLineage,
	parentGenerationID string,
) HistoricalGeneration {
	usage, fallbackUsage := opencodeMapUsage(msg.Tokens, assistantParts)
	gen := agento11y.Generation{
		ID:             source.GenerationID(),
		ConversationID: lineage.conversationID,
		AgentName:      opencodeAgentName(msg.Mode),
		Mode:           agento11y.GenerationModeStream,
		OperationName:  "streamText",
		Model:          agento11y.ModelRef{Provider: msg.ProviderID, Name: msg.ModelID},
		ResponseModel:  msg.ModelID,
		Input:          opencodeInputMessages(userParts),
		Output:         opencodeOutputMessages(assistantParts),
		Tools:          opencodeToolDefinitions(assistantParts),
		Usage:          usage,
		StopReason:     msg.Finish,
		CallError:      opencodeError(msg.Error),
		Tags:           opencodeTags(msg.Path.CWD, lineage.parentSessionID != ""),
		Metadata:       opencodeMetadata(msg.Cost, sess.SessionID, lineage),
	}
	if lineage.conversationID == sess.SessionID {
		gen.ConversationTitle = lineage.conversationTitle
	}
	if msg.Time.Created > 0 {
		gen.StartedAt = time.UnixMilli(msg.Time.Created).UTC()
	}
	if msg.Time.Completed != nil && *msg.Time.Completed > 0 {
		gen.CompletedAt = time.UnixMilli(*msg.Time.Completed).UTC()
	}
	if parentGenerationID != "" {
		gen.ParentGenerationIDs = []string{parentGenerationID}
	}
	quality := QualityReport{
		ApproxStartedAt:   gen.StartedAt.IsZero(),
		ApproxCompletedAt: gen.CompletedAt.IsZero(),
		ApproxUsage:       fallbackUsage,
		MissingModel:      strings.TrimSpace(gen.Model.Name) == "",
	}
	return HistoricalGeneration{Source: source, Gen: gen, Quality: quality}
}

func opencodeAgentName(mode string) string {
	if mode == "" {
		return string(AgentOpenCode)
	}
	return string(AgentOpenCode) + ":" + mode
}

func opencodeInputMessages(parts []sessiondb.Part) []agento11y.Message {
	var messages []agento11y.Message
	for _, part := range parts {
		if part.Type != "text" || part.Ignored || strings.TrimSpace(part.Text) == "" {
			continue
		}
		messages = append(messages, agento11y.Message{
			Role:  agento11y.RoleUser,
			Parts: []agento11y.Part{{Kind: agento11y.PartKindText, Text: part.Text}},
		})
	}
	return messages
}

func opencodeOutputMessages(parts []sessiondb.Part) []agento11y.Message {
	var messages []agento11y.Message
	for _, part := range parts {
		switch part.Type {
		case "text":
			if strings.TrimSpace(part.Text) == "" {
				continue
			}
			messages = append(messages, agento11y.Message{
				Role:  agento11y.RoleAssistant,
				Parts: []agento11y.Part{{Kind: agento11y.PartKindText, Text: part.Text}},
			})
		case "reasoning":
			if strings.TrimSpace(part.Text) == "" {
				continue
			}
			messages = append(messages, agento11y.Message{
				Role:  agento11y.RoleAssistant,
				Parts: []agento11y.Part{{Kind: agento11y.PartKindThinking, Thinking: part.Text}},
			})
		case "tool":
			if part.State.Status != "completed" && part.State.Status != "error" {
				continue
			}
			messages = append(messages, agento11y.Message{
				Role: agento11y.RoleAssistant,
				Parts: []agento11y.Part{{
					Kind: agento11y.PartKindToolCall,
					ToolCall: &agento11y.ToolCall{
						ID:        part.CallID,
						Name:      part.Tool,
						InputJSON: opencodeToolInput(part.State.Input),
					},
				}},
			})
			result := &agento11y.ToolResult{
				ToolCallID: part.CallID,
				Name:       part.Tool,
				IsError:    part.Tool == "invalid",
			}
			if part.Tool == "bash" && part.State.Metadata.Exit != nil && *part.State.Metadata.Exit != 0 {
				result.IsError = true
			}
			if part.State.Status == "error" {
				result.IsError = true
				result.Content = "unknown error"
				if part.State.Error != nil {
					result.Content = *part.State.Error
				}
			} else {
				result.Content = part.State.Output
			}
			messages = append(messages, agento11y.Message{
				Role:  agento11y.RoleTool,
				Parts: []agento11y.Part{{Kind: agento11y.PartKindToolResult, ToolResult: result}},
			})
		}
	}
	return messages
}

func opencodeToolInput(raw json.RawMessage) json.RawMessage {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return json.RawMessage("{}")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return json.RawMessage("{}")
	}
	return json.RawMessage(compact.Bytes())
}

func opencodeToolDefinitions(parts []sessiondb.Part) []agento11y.ToolDefinition {
	names := map[string]bool{}
	for _, part := range parts {
		if part.Type == "tool" && part.Tool != "" && (part.State.Status == "completed" || part.State.Status == "error") {
			names[part.Tool] = true
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
	return tools
}

func opencodeMapUsage(fallback sessiondb.TokenCounts, parts []sessiondb.Part) (agento11y.TokenUsage, bool) {
	var tokens sessiondb.TokenCounts
	found := false
	for _, part := range parts {
		if part.Type != "step-finish" || !part.Tokens.Observed() {
			continue
		}
		found = true
		tokens.Input += part.Tokens.Input
		tokens.Output += part.Tokens.Output
		tokens.Reasoning += part.Tokens.Reasoning
		tokens.Cache.Read += part.Tokens.Cache.Read
		tokens.Cache.Write += part.Tokens.Cache.Write
	}
	if !found {
		tokens = fallback
	}
	return agento11y.TokenUsage{
		InputTokens:           tokens.Input,
		OutputTokens:          tokens.Output,
		TotalTokens:           tokens.Input + tokens.Output,
		CacheReadInputTokens:  tokens.Cache.Read,
		CacheWriteInputTokens: tokens.Cache.Write,
		ReasoningTokens:       tokens.Reasoning,
	}, !found
}

func opencodeError(messageError *sessiondb.MessageError) string {
	if messageError == nil {
		return ""
	}
	switch messageError.Name {
	case "ProviderAuthError":
		return "provider_auth"
	case "APIError":
		if messageError.Data.StatusCode == nil {
			return "api_error: unknown"
		}
		return fmt.Sprintf("api_error: %d", *messageError.Data.StatusCode)
	case "MessageOutputLengthError":
		return "output_length_exceeded"
	case "MessageAbortedError":
		return "aborted"
	case "UnknownError":
		return "unknown_error"
	default:
		return "unknown_error"
	}
}

func opencodeTags(cwd string, subagent bool) map[string]string {
	tags := map[string]string{}
	if cwd != "" {
		tags["cwd"] = cwd
	}
	if subagent {
		tags["subagent"] = "true"
	}
	if len(tags) == 0 {
		return nil
	}
	return tags
}

func opencodeMetadata(cost float64, sessionID string, lineage opencodeLineage) map[string]any {
	metadata := map[string]any{"cost_usd": cost}
	if lineage.parentSessionID != "" {
		metadata["opencode.parent_session_id"] = lineage.parentSessionID
	}
	if lineage.conversationID != sessionID {
		metadata["opencode.child_session_id"] = sessionID
	}
	return metadata
}
