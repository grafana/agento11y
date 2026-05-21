package mapper

import (
	"encoding/json"
	"log"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/grafana/sigil-sdk/go/sigil"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/agents/claudecode/state"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/agents/claudecode/transcript"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/redact"
)

const (
	agentName       = "claude-code"
	maxToolInputLen = 4096
)

// Options controls how transcript lines are mapped to generations.
type Options struct {
	SessionID string            // authoritative session ID from the hook input
	Logger    *log.Logger       // debug logger (nil = silent)
	ExtraTags map[string]string // user-supplied tags merged into every generation; built-in keys always win
}

func (o Options) logf(format string, args ...any) {
	if o.Logger != nil {
		o.Logger.Printf(format, args...)
	}
}

type userContext struct {
	prompt      string
	toolResults []sigil.Message
}

// Coalesce merges consecutive assistant lines sharing the same RequestID
// into a single line with merged content blocks and the final line's metadata.
// It returns only the safe prefix ending at the last complete assistant turn.
// Trailing prompts, tool results, and incomplete assistant fragments are left
// out so the next hook invocation re-reads them from the unchanged offset.
func Coalesce(lines []transcript.Line) ([]transcript.Line, int64) {
	var (
		result         []transcript.Line
		pending        []transcript.Line
		lastSafeLen    int
		lastSafeOffset int64
	)

	markSafe := func(offset int64) {
		lastSafeLen = len(result)
		lastSafeOffset = offset
	}

	appendAssistantIfComplete := func(line transcript.Line) {
		var msg transcript.AssistantMessage
		if err := json.Unmarshal(line.Message, &msg); err != nil || msg.StopReason == "" {
			return
		}
		result = append(result, line)
		markSafe(line.EndOffset)
	}

	flush := func() {
		if len(pending) == 0 {
			return
		}
		last := pending[len(pending)-1]
		var msg transcript.AssistantMessage
		if err := json.Unmarshal(last.Message, &msg); err == nil && msg.StopReason != "" {
			result = append(result, mergeAssistantGroup(pending))
			markSafe(last.EndOffset)
		}
		// Incomplete group (no terminal stop_reason): excluded,
		// offset not advanced; will be re-read next invocation.
		pending = nil
	}

	for _, line := range lines {
		if line.Type == "assistant" {
			if line.RequestID == "" {
				flush()
				appendAssistantIfComplete(line)
				continue
			}
			if len(pending) > 0 && pending[0].RequestID != line.RequestID {
				flush()
			}
			pending = append(pending, line)
			continue
		}

		flush()
		result = append(result, line)
	}
	flush()

	return result[:lastSafeLen], lastSafeOffset
}

func mergeAssistantGroup(lines []transcript.Line) transcript.Line {
	if len(lines) == 1 {
		return lines[0]
	}
	final := lines[len(lines)-1]

	var allBlocks []transcript.ContentBlock
	for _, l := range lines {
		var msg transcript.AssistantMessage
		if err := json.Unmarshal(l.Message, &msg); err != nil {
			continue
		}
		allBlocks = append(allBlocks, msg.Content...)
	}

	var finalMsg transcript.AssistantMessage
	if err := json.Unmarshal(final.Message, &finalMsg); err != nil {
		return final
	}
	finalMsg.Content = allBlocks
	merged, err := json.Marshal(finalMsg)
	if err != nil {
		return final
	}
	final.Message = merged
	return final
}

// agentCall holds the metadata captured from an Agent tool_use block.
type agentCall struct {
	parentGenID  string           // generation that spawned this call
	parentGen    sigil.Generation // copy for inheriting fields
	subagentType string           // lowercased subagent_type from tool input; empty falls back to "subagent"
}

// Process walks transcript lines and produces Generation records.
// It updates st.Title with the conversation title if discovered.
//
// Claude Code subagents do not produce their own transcript lines — the only
// evidence of their execution is the Agent tool_use (spawn) and the matching
// tool_result (output). Process synthesises a generation for each completed
// Agent call so that the Sigil dependency graph can display the DAG.
func Process(lines []transcript.Line, st *state.Session, opts Options, r *redact.Redactor) []sigil.Generation {
	var (
		gens []sigil.Generation
		uctx userContext
		// agentCalls indexes Agent tool_use call IDs to the generation that
		// emitted them, so we can synthesise subagent generations when the
		// matching tool_result arrives.
		agentCalls = make(map[string]agentCall)
	)

	for _, line := range lines {
		switch line.Type {
		case "user":
			processUserLine(line, &uctx, st, r, opts)
			// Synthesise subagent generations from Agent tool results.
			gens = append(gens, synthesiseSubagentGens(line, &uctx, agentCalls, opts)...)

		case "assistant":
			if gen, ok := processAssistantLine(line, &uctx, st, opts, r); ok {
				// Index Agent tool calls from this generation's output.
				for _, msg := range gen.Output {
					for _, part := range msg.Parts {
						if part.ToolCall != nil && part.ToolCall.Name == "Agent" {
							var parsed struct {
								SubagentType string `json:"subagent_type"`
							}
							_ = json.Unmarshal(part.ToolCall.InputJSON, &parsed)
							agentCalls[part.ToolCall.ID] = agentCall{
								parentGenID:  gen.ID,
								parentGen:    gen,
								subagentType: strings.ToLower(parsed.SubagentType),
							}
						}
					}
				}
				gens = append(gens, gen)
			}
		}
	}

	return gens
}

// synthesiseSubagentGens creates a generation for each Agent tool result in
// the user line, using the Agent tool_use input for metadata (model,
// description) and the tool_result content as output.
func synthesiseSubagentGens(line transcript.Line, uctx *userContext, calls map[string]agentCall, opts Options) []sigil.Generation {
	var gens []sigil.Generation
	for _, msg := range uctx.toolResults {
		for _, part := range msg.Parts {
			if part.ToolResult == nil {
				continue
			}
			ac, ok := calls[part.ToolResult.ToolCallID]
			if !ok {
				continue
			}

			completedAt, _ := time.Parse(time.RFC3339Nano, line.Timestamp)

			suffix := ac.subagentType
			if suffix == "" {
				suffix = "subagent"
			}

			gen := sigil.Generation{
				ID:                  subagentGenID(opts.SessionID, part.ToolResult.ToolCallID),
				ConversationID:      opts.SessionID,
				ConversationTitle:   opts.SessionID,
				ParentGenerationIDs: []string{ac.parentGenID},
				AgentName:           agentName + "/" + suffix,
				AgentVersion:        ac.parentGen.AgentVersion,
				EffectiveVersion:    ac.parentGen.EffectiveVersion,
				Mode:                sigil.GenerationModeSync,
				OperationName:       "generateText",
				Model:               ac.parentGen.Model,
				StopReason:          "end_turn",
				StartedAt:           ac.parentGen.CompletedAt,
				CompletedAt:         completedAt,
				Tags:                buildTags(line, true, opts.ExtraTags),
			}

			// Use the tool result content as the output.
			outputText := part.ToolResult.Content
			if outputText != "" {
				gen.Output = []sigil.Message{{
					Role:  sigil.RoleAssistant,
					Parts: []sigil.Part{{Kind: sigil.PartKindText, Text: outputText}},
				}}
			}

			gens = append(gens, gen)
		}
	}
	return gens
}

// subagentGenID produces a deterministic generation ID for a synthesised
// subagent generation, namespaced by session and the Agent tool call ID.
func subagentGenID(sessionID, toolCallID string) string {
	return uuid.NewSHA1(uuid.NameSpaceDNS, []byte(sessionID+":subagent:"+toolCallID)).String()
}

func processUserLine(line transcript.Line, uctx *userContext, st *state.Session, r *redact.Redactor, opts Options) {
	var msg transcript.UserMessage
	if err := json.Unmarshal(line.Message, &msg); err != nil {
		opts.logf("unmarshal user message: %v", err)
		return
	}

	text, blocks, err := transcript.ParseUserContent(msg.Content)
	if err != nil {
		opts.logf("parse user content: %v", err)
		return
	}

	if text != "" {
		uctx.prompt = text
		uctx.toolResults = nil
		if st.Title == "" {
			st.Title = text
		}
		return
	}

	var toolParts []sigil.Part
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			uctx.prompt = b.Text
			uctx.toolResults = nil
			if st.Title == "" {
				st.Title = b.Text
			}
		}
		if b.Type == "tool_result" {
			content := b.Content()
			if r != nil {
				content = r.Redact(content)
			}
			toolParts = append(toolParts, sigil.Part{
				Kind: sigil.PartKindToolResult,
				ToolResult: &sigil.ToolResult{
					ToolCallID: b.ToolUseID,
					Content:    content,
					IsError:    b.IsError,
				},
			})
		}
	}
	if len(toolParts) > 0 {
		uctx.toolResults = []sigil.Message{{
			Role:  sigil.RoleTool,
			Parts: toolParts,
		}}
	}
}

func processAssistantLine(line transcript.Line, uctx *userContext, _ *state.Session, opts Options, r *redact.Redactor) (sigil.Generation, bool) {
	var msg transcript.AssistantMessage
	if err := json.Unmarshal(line.Message, &msg); err != nil {
		opts.logf("unmarshal assistant message: %v", err)
		return sigil.Generation{}, false
	}

	// Zero-token assistant lines are Claude Code's client-side socket-error
	// recovery markers ("API Error: The socket connection was closed..."),
	// not real LLM turns.
	if msg.Usage.OutputTokens <= 0 {
		return sigil.Generation{}, false
	}

	isSidechain := line.IsSidechain

	completedAt, _ := time.Parse(time.RFC3339Nano, line.Timestamp)

	usage := sigil.TokenUsage{
		InputTokens:           msg.Usage.InputTokens,
		OutputTokens:          msg.Usage.OutputTokens,
		CacheReadInputTokens:  msg.Usage.CacheReadInputTokens,
		CacheWriteInputTokens: msg.Usage.CacheCreationInputTokens,
	}
	usage.TotalTokens = usage.InputTokens + usage.OutputTokens

	gen := sigil.Generation{
		ID:                generationID(line),
		ConversationID:    opts.SessionID,
		ConversationTitle: opts.SessionID,
		AgentName:         agentName,
		AgentVersion:      line.Version,
		EffectiveVersion:  line.Version,
		Mode:              sigil.GenerationModeSync,
		OperationName:     "generateText",
		Model: sigil.ModelRef{
			Provider: "anthropic",
			Name:     msg.Model,
		},
		ResponseID:    strings.TrimSpace(line.RequestID),
		ResponseModel: msg.Model,
		Usage:         usage,
		StopReason:    msg.StopReason,
		StartedAt:     completedAt, // no real start time; set equal to avoid zero-value skip in SDK metrics
		CompletedAt:   completedAt,
		Tags:          buildTags(line, isSidechain, opts.ExtraTags),
	}

	toolNames := map[string]bool{}
	hasThinking := false

	for _, block := range msg.Content {
		switch block.Type {
		case "tool_use":
			toolNames[block.Name] = true
		case "thinking":
			hasThinking = true
		}
	}

	if len(toolNames) > 0 {
		gen.Tools = buildToolDefs(toolNames)
	}

	if hasThinking {
		gen.ThinkingEnabled = ptrBool(true)
	}

	gen.Input = buildInput(uctx, r)
	gen.Output = buildOutput(msg.Content, r)

	return gen, true
}

func buildTags(line transcript.Line, subagent bool, extras map[string]string) map[string]string {
	if line.GitBranch == "" && line.CWD == "" && line.Entrypoint == "" && !subagent && len(extras) == 0 {
		return nil
	}
	tags := make(map[string]string, 4+len(extras))
	// Extras go in first; built-ins written below overwrite any collisions
	// so user-supplied keys can never shadow git.branch/cwd/entrypoint/subagent.
	for k, v := range extras {
		tags[k] = v
	}
	if line.GitBranch != "" {
		tags["git.branch"] = line.GitBranch
	}
	if line.CWD != "" {
		tags["cwd"] = line.CWD
	}
	if line.Entrypoint != "" {
		tags["entrypoint"] = line.Entrypoint
	}
	if subagent {
		tags["subagent"] = "true"
	}
	return tags
}

func buildToolDefs(names map[string]bool) []sigil.ToolDefinition {
	sorted := slices.Sorted(maps.Keys(names))
	defs := make([]sigil.ToolDefinition, len(sorted))
	for i, name := range sorted {
		defs[i] = sigil.ToolDefinition{Name: name, Type: "function"}
	}
	return defs
}

func buildInput(uctx *userContext, r *redact.Redactor) []sigil.Message {
	if len(uctx.toolResults) > 0 {
		return uctx.toolResults
	}
	if uctx.prompt == "" {
		return nil
	}
	text := uctx.prompt
	if r != nil {
		text = r.RedactLightweight(text)
	}
	return []sigil.Message{{
		Role: sigil.RoleUser,
		Parts: []sigil.Part{{
			Kind: sigil.PartKindText,
			Text: text,
		}},
	}}
}

func buildOutput(blocks []transcript.ContentBlock, r *redact.Redactor) []sigil.Message {
	var parts []sigil.Part

	for _, block := range blocks {
		switch block.Type {
		case "text":
			text := block.Text
			if r != nil {
				text = r.RedactLightweight(text)
			}
			parts = append(parts, sigil.Part{
				Kind: sigil.PartKindText,
				Text: text,
			})

		case "thinking":
			// Omit content (can be 50KB+), just note presence
			parts = append(parts, sigil.Part{
				Kind:     sigil.PartKindThinking,
				Thinking: "[thinking block omitted]",
			})

		case "tool_use":
			inputJSON := truncateJSON(block.Input, maxToolInputLen, r)
			parts = append(parts, sigil.Part{
				Kind: sigil.PartKindToolCall,
				ToolCall: &sigil.ToolCall{
					ID:        block.ID,
					Name:      block.Name,
					InputJSON: inputJSON,
				},
			})
		}
	}

	if len(parts) == 0 {
		return nil
	}

	return []sigil.Message{{
		Role:  sigil.RoleAssistant,
		Parts: parts,
	}}
}

// truncateJSON redacts and truncates tool input JSON.
// Uses Tier 1 only (RedactLightweight) to avoid Tier 2 patterns mangling
// JSON structure. When truncation occurs, the result is wrapped as a JSON
// string (type changes from the original object/array to string).
func truncateJSON(raw json.RawMessage, maxLen int, r *redact.Redactor) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}

	s := string(raw)
	if r != nil {
		s = r.RedactLightweight(s)
	}

	if len(s) <= maxLen {
		return json.RawMessage(s)
	}

	// Truncate to valid UTF-8 boundary
	truncated := s[:maxLen]
	for !utf8.ValidString(truncated) {
		truncated = truncated[:len(truncated)-1]
	}

	quoted, _ := json.Marshal(truncated + " [truncated]")
	return json.RawMessage(quoted)
}

// generationID produces a deterministic UUID v5 from transcript data.
// Uses RequestID when available (shared across streaming fragments),
// falling back to UUID for backward compatibility.
func generationID(line transcript.Line) string {
	key := line.RequestID
	if key == "" {
		key = line.UUID
	}
	name := line.SessionID + ":" + key
	return uuid.NewSHA1(uuid.NameSpaceDNS, []byte(name)).String()
}

func ptrBool(b bool) *bool { return &b }
