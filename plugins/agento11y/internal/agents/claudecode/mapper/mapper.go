package mapper

import (
	"encoding/json"
	"log"
	"maps"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/state"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/transcript"
	"github.com/grafana/agento11y/plugins/agento11y/internal/mapperutil"
	"github.com/grafana/agento11y/plugins/agento11y/internal/redact"
	"github.com/grafana/agento11y/plugins/agento11y/internal/timeutil"
)

const (
	agentName       = "claude-code"
	maxToolInputLen = 4096
	// maxTitleLen caps the conversation title derived from the first user prompt.
	maxTitleLen = 100
)

// Options controls how transcript lines are mapped to generations.
type Options struct {
	SessionID string            // authoritative session ID from the hook input
	Logger    *log.Logger       // debug logger (nil = silent)
	ExtraTags map[string]string // user-supplied tags merged into every generation; built-in keys always win
	// SuppressSyntheticSubagentToolCallIDs skips the summary-only generation
	// for an Agent tool result whose full subagent transcript is mapped
	// separately. Live capture leaves it nil, so it keeps recording summaries
	// when the parent transcript is the only record of the subagent run.
	SuppressSyntheticSubagentToolCallIDs map[string]bool
}

func (o Options) logf(format string, args ...any) {
	if o.Logger != nil {
		o.Logger.Printf(format, args...)
	}
}

type userContext struct {
	prompt      string
	toolResults []agento11y.Message
}

// Coalesce merges consecutive assistant lines sharing the same RequestID
// into a single line with merged content blocks and the final line's metadata.
// It returns only the safe prefix ending at the last complete assistant turn.
// Trailing prompts, tool results, and incomplete assistant fragments are left
// out so the next hook invocation re-reads them from the unchanged offset.
//
// A group with no terminal stop_reason still counts as complete when one of
// its tool_use blocks has a matching tool_result later in the same batch: the
// tool only ran because the assistant message was finished. Claude Code wrote
// stop_reason null on every assistant line from version 2.0.62 to 2.1.63, and
// still does so in subagent transcripts, so without this rule those turns are
// dropped.
func Coalesce(lines []transcript.Line) ([]transcript.Line, int64) {
	return coalesce(lines, false)
}

// CoalesceSession is Coalesce for a session that is no longer being written,
// which is what the history importer reads. Every group is complete, including
// a trailing one with no stop_reason and no answered tool call, so the last
// assistant turn of a session is kept rather than left for a next read that
// will never happen.
func CoalesceSession(lines []transcript.Line) []transcript.Line {
	out, _ := coalesce(lines, true)
	return out
}

func coalesce(lines []transcript.Line, wholeSession bool) ([]transcript.Line, int64) {
	var (
		result         = make([]transcript.Line, 0, len(lines))
		pending        []transcript.Line
		lastSafeLen    int
		lastSafeOffset int64
		answered       = toolResultIDs(lines)
	)

	markSafe := func(offset int64) {
		lastSafeLen = len(result)
		lastSafeOffset = offset
	}

	appendAssistant := func(line transcript.Line) {
		var msg transcript.AssistantMessage
		err := json.Unmarshal(line.Message, &msg)
		if err != nil {
			return
		}
		if msg.StopReason == "" && !wholeSession {
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
		switch {
		case json.Unmarshal(last.Message, &msg) == nil && msg.StopReason != "":
			result = append(result, mergeAssistantGroup(pending, ""))
			markSafe(last.EndOffset)
		case groupAnswered(pending, answered):
			result = append(result, mergeAssistantGroup(pending, "tool_use"))
			markSafe(last.EndOffset)
		case wholeSession:
			result = append(result, mergeAssistantGroup(pending, ""))
			markSafe(last.EndOffset)
		}
		// Incomplete group in a session still being written (no terminal
		// stop_reason, and no answered tool_use): excluded, offset not advanced;
		// will be re-read next invocation.
		pending = nil
	}

	for _, line := range lines {
		if line.Type == "assistant" {
			if line.RequestID == "" {
				flush()
				appendAssistant(line)
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

	if wholeSession {
		// Nothing is held back: there is no next read to pick up a trailing
		// group, and the trailing user lines carry the tool results the mapper
		// turns into subagent generations.
		return result, lastSafeOffset
	}
	return result[:lastSafeLen], lastSafeOffset
}

// mergeAssistantGroup merges one request's assistant fragments into a single
// line. inferredStopReason fills a missing stop_reason; an empty value leaves
// the field as the source wrote it.
func mergeAssistantGroup(lines []transcript.Line, inferredStopReason string) transcript.Line {
	if len(lines) == 1 {
		if inferredStopReason == "" {
			return lines[0]
		}
		return assistantLineWithStopReason(lines[0], inferredStopReason)
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
	if finalMsg.StopReason == "" {
		finalMsg.StopReason = inferredStopReason
	}
	merged, err := json.Marshal(finalMsg)
	if err != nil {
		return final
	}
	final.Message = merged
	return final
}

func assistantLineWithStopReason(line transcript.Line, stopReason string) transcript.Line {
	var msg transcript.AssistantMessage
	if err := json.Unmarshal(line.Message, &msg); err != nil || msg.StopReason != "" {
		return line
	}
	msg.StopReason = stopReason
	merged, err := json.Marshal(msg)
	if err != nil {
		return line
	}
	line.Message = merged
	return line
}

// groupAnswered reports whether an assistant group ended in a tool call the
// batch already answers. answered holds every tool_use ID a tool_result refers
// to; a tool_use ID is unique within a session, so a match means this turn's
// call ran, which it can only do once the assistant message is complete.
func groupAnswered(group []transcript.Line, answered map[string]bool) bool {
	if len(answered) == 0 {
		return false
	}
	for _, line := range group {
		if line.Type != "assistant" {
			continue
		}
		var msg transcript.AssistantMessage
		if err := json.Unmarshal(line.Message, &msg); err != nil || msg.StopReason != "" {
			continue
		}
		for _, block := range msg.Content {
			if block.Type == "tool_use" && answered[block.ID] {
				return true
			}
		}
	}
	return false
}

// toolResultIDs collects every tool_use ID answered by a tool_result in lines.
func toolResultIDs(lines []transcript.Line) map[string]bool {
	ids := map[string]bool{}
	for _, line := range lines {
		if line.Type != "user" {
			continue
		}
		var msg transcript.UserMessage
		if err := json.Unmarshal(line.Message, &msg); err != nil {
			continue
		}
		_, blocks, err := transcript.ParseUserContent(msg.Content)
		if err != nil {
			continue
		}
		for _, block := range blocks {
			if block.Type == "tool_result" && block.ToolUseID != "" {
				ids[block.ToolUseID] = true
			}
		}
	}
	return ids
}

// agentCall holds the metadata captured from an Agent tool_use block.
type agentCall struct {
	parentGenID  string               // generation that spawned this call
	parentGen    agento11y.Generation // copy for inheriting fields
	subagentType string               // lowercased subagent_type from tool input; empty falls back to "subagent"
}

// Process walks transcript lines and produces Generation records, plus the
// timestamp of every tool_result line keyed by tool call ID. The caller uses
// those timestamps as the end of each execute_tool span; without them a tool
// span has no width, because a tool call and its result live on separate lines.
// A tool call ID is present in that map only when its result line carried a
// parseable timestamp, so a present key always means a usable end time.
// Process updates st.Title with the conversation title if discovered.
//
// Claude Code subagents do not produce their own lines in the main transcript —
// the only evidence of their execution is the Agent tool_use (spawn) and the
// matching tool_result (output). Process synthesises a generation for each
// completed Agent call so that the agento11y dependency graph can display the
// DAG.
func Process(lines []transcript.Line, st *state.Session, opts Options, r *redact.Redactor) ([]agento11y.Generation, map[string]time.Time) {
	var (
		gens []agento11y.Generation
		uctx userContext
		// toolResultAt records when each tool call's result arrived.
		toolResultAt = map[string]time.Time{}
		// agentCalls indexes Agent tool_use call IDs to the generation that
		// emitted them, so we can synthesise subagent generations when the
		// matching tool_result arrives.
		agentCalls = make(map[string]agentCall)
		// prevAt holds the timestamp of the previous processed line per chain,
		// keyed by line.IsSidechain. A sidechain line belongs to a subagent turn,
		// so the two chains must not hand each other a request time. Claude Code
		// writes sidechain lines to separate subagent transcripts, so on the
		// current format only the main-chain key is ever set.
		prevAt = map[bool]time.Time{}
	)

	for _, line := range lines {
		switch line.Type {
		case "user":
			processUserLine(line, &uctx, st, r, opts, toolResultAt)
			// Synthesise subagent generations from Agent tool results.
			gens = append(gens, synthesiseSubagentGens(line, &uctx, agentCalls, opts)...)

		case "assistant":
			if gen, ok := processAssistantLine(line, &uctx, st, opts, r, prevAt[line.IsSidechain]); ok {
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

		// Advance the cursor only after the line is handled: an assistant line
		// must not seed its own start, or every window collapses to zero.
		if at := timeutil.ParseTimestamp(line.Timestamp, time.Time{}); !at.IsZero() {
			prevAt[line.IsSidechain] = at
		}
	}

	title := conversationTitle(st, opts.SessionID, r)
	for i := range gens {
		gens[i].ConversationTitle = title
	}

	return gens, toolResultAt
}

// clampSpanStart returns the start of a generation window whose end is fixed at
// completedAt. derivedStart comes from another transcript line, so it is used
// only when it is present and not after completedAt; anything else collapses the
// window to completedAt. Transcript timestamps are optional and not strictly
// monotonic, and a window that inverts reaches the trace store as the unsigned
// wrap of a negative duration instead of a latency.
func clampSpanStart(derivedStart, completedAt time.Time) time.Time {
	if derivedStart.IsZero() || derivedStart.After(completedAt) {
		return completedAt
	}
	return derivedStart
}

// conversationTitle returns a truncated version of the session title derived
// from the first user prompt. Falls back to the session ID when no title is
// available (e.g. transcript with no user lines processed yet).
func conversationTitle(st *state.Session, sessionID string, r *redact.Redactor) string {
	if st == nil || st.Title == "" {
		return sessionID
	}
	t := strings.TrimSpace(st.Title)
	if r != nil {
		t = r.RedactLightweight(t)
	}
	if t == "" {
		return sessionID
	}
	if len(t) > maxTitleLen {
		t = t[:maxTitleLen]
		// Truncate to valid UTF-8 boundary
		for !utf8.ValidString(t) {
			t = t[:len(t)-1]
		}
	}
	return t
}

// synthesiseSubagentGens creates a generation for each Agent tool result in
// the user line, using the Agent tool_use input for metadata (model,
// description) and the tool_result content as output.
func synthesiseSubagentGens(line transcript.Line, uctx *userContext, calls map[string]agentCall, opts Options) []agento11y.Generation {
	var gens []agento11y.Generation
	for _, msg := range uctx.toolResults {
		for _, part := range msg.Parts {
			if part.ToolResult == nil {
				continue
			}
			ac, ok := calls[part.ToolResult.ToolCallID]
			if !ok {
				continue
			}
			if opts.SuppressSyntheticSubagentToolCallIDs[part.ToolResult.ToolCallID] {
				continue
			}

			completedAt := timeutil.ParseTimestamp(line.Timestamp, time.Time{})

			// The subagent ran between the spawning turn's completion and the
			// tool_result that carries its output.
			startedAt := clampSpanStart(ac.parentGen.CompletedAt, completedAt)

			suffix := ac.subagentType
			if suffix == "" {
				suffix = "subagent"
			}

			gen := agento11y.Generation{
				ID:                  subagentGenID(opts.SessionID, part.ToolResult.ToolCallID),
				ConversationID:      opts.SessionID,
				ConversationTitle:   opts.SessionID,
				ParentGenerationIDs: []string{ac.parentGenID},
				AgentName:           agentName + "/" + suffix,
				AgentVersion:        ac.parentGen.AgentVersion,
				EffectiveVersion:    ac.parentGen.EffectiveVersion,
				Mode:                agento11y.GenerationModeSync,
				OperationName:       "generateText",
				Model:               ac.parentGen.Model,
				StopReason:          "end_turn",
				StartedAt:           startedAt,
				CompletedAt:         completedAt,
				Tags:                buildTags(line, true, opts.ExtraTags),
			}

			// Use the tool result content as the output.
			outputText := part.ToolResult.Content
			if outputText != "" {
				gen.Output = []agento11y.Message{{
					Role:  agento11y.RoleAssistant,
					Parts: []agento11y.Part{{Kind: agento11y.PartKindText, Text: outputText}},
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

// processUserLine folds one user line into uctx: a plain prompt, or the tool
// results that answer the previous assistant turn. Every tool result with a
// parseable line timestamp also records it in toolResultAt, so the caller can
// give the execute_tool span a real end time.
func processUserLine(line transcript.Line, uctx *userContext, st *state.Session, r *redact.Redactor, opts Options, toolResultAt map[string]time.Time) {
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

	// Every tool_result block on this line shares the line's timestamp.
	resultAt := timeutil.ParseTimestamp(line.Timestamp, time.Time{})

	var toolParts []agento11y.Part
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			uctx.prompt = b.Text
			uctx.toolResults = nil
			if st.Title == "" {
				st.Title = b.Text
			}
		}
		if b.Type == "tool_result" {
			if b.ToolUseID != "" && !resultAt.IsZero() {
				toolResultAt[b.ToolUseID] = resultAt
			}
			content := b.Content()
			if r != nil {
				content = r.Redact(content)
			}
			toolParts = append(toolParts, agento11y.Part{
				Kind: agento11y.PartKindToolResult,
				ToolResult: &agento11y.ToolResult{
					ToolCallID: b.ToolUseID,
					Content:    content,
					IsError:    b.IsError,
				},
			})
		}
	}
	if len(toolParts) > 0 {
		uctx.toolResults = []agento11y.Message{{
			Role:  agento11y.RoleTool,
			Parts: toolParts,
		}}
	}
}

// processAssistantLine maps one coalesced assistant line to a generation.
// requestAt is the timestamp of the previous line on the same chain, zero when
// the batch opens on an assistant line.
func processAssistantLine(line transcript.Line, uctx *userContext, _ *state.Session, opts Options, r *redact.Redactor, requestAt time.Time) (agento11y.Generation, bool) {
	var msg transcript.AssistantMessage
	if err := json.Unmarshal(line.Message, &msg); err != nil {
		opts.logf("unmarshal assistant message: %v", err)
		return agento11y.Generation{}, false
	}

	// Zero-token assistant lines are Claude Code's client-side socket-error
	// recovery markers ("API Error: The socket connection was closed..."),
	// not real LLM turns.
	if msg.Usage.OutputTokens <= 0 {
		return agento11y.Generation{}, false
	}

	isSidechain := line.IsSidechain

	completedAt := timeutil.ParseTimestamp(line.Timestamp, time.Time{})

	// The transcript records no request-start time. The previous line on this
	// chain (the user prompt, or the tool_result that unblocked the turn) is the
	// closest real one.
	startedAt := clampSpanStart(requestAt, completedAt)

	usage := agento11y.TokenUsage{
		InputTokens:           msg.Usage.InputTokens,
		OutputTokens:          msg.Usage.OutputTokens,
		CacheReadInputTokens:  msg.Usage.CacheReadInputTokens,
		CacheWriteInputTokens: msg.Usage.CacheCreationInputTokens,
	}
	usage.TotalTokens = usage.InputTokens + usage.OutputTokens

	gen := agento11y.Generation{
		ID:                generationID(line),
		ConversationID:    opts.SessionID,
		ConversationTitle: opts.SessionID,
		AgentName:         lineAgentName(line),
		AgentVersion:      line.Version,
		EffectiveVersion:  line.Version,
		Mode:              agento11y.GenerationModeSync,
		OperationName:     "generateText",
		Model: agento11y.ModelRef{
			Provider: "anthropic",
			Name:     msg.Model,
		},
		ResponseID:    strings.TrimSpace(line.RequestID),
		ResponseModel: msg.Model,
		Usage:         usage,
		StopReason:    msg.StopReason,
		StartedAt:     startedAt,
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
	maps.Copy(tags, extras)
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

// lineAgentName names the agent that produced a turn. A sidechain line ran
// inside a subagent, so its type is appended ("claude-code/general-purpose")
// and the turn is attributable to that subagent rather than the main thread.
func lineAgentName(line transcript.Line) string {
	if !line.IsSidechain {
		return agentName
	}
	suffix := strings.ToLower(strings.TrimSpace(line.AttributionAgent))
	if suffix == "" {
		return agentName
	}
	return agentName + "/" + suffix
}

func buildToolDefs(names map[string]bool) []agento11y.ToolDefinition {
	keys := make([]string, 0, len(names))
	for name := range names {
		keys = append(keys, name)
	}
	return mapperutil.SortedToolDefinitions(keys)
}

func buildInput(uctx *userContext, r *redact.Redactor) []agento11y.Message {
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
	return []agento11y.Message{{
		Role: agento11y.RoleUser,
		Parts: []agento11y.Part{{
			Kind: agento11y.PartKindText,
			Text: text,
		}},
	}}
}

func buildOutput(blocks []transcript.ContentBlock, r *redact.Redactor) []agento11y.Message {
	var parts []agento11y.Part

	for _, block := range blocks {
		switch block.Type {
		case "text":
			text := block.Text
			if r != nil {
				text = r.RedactLightweight(text)
			}
			parts = append(parts, agento11y.Part{
				Kind: agento11y.PartKindText,
				Text: text,
			})

		case "thinking":
			// Omit content (can be 50KB+), just note presence
			parts = append(parts, agento11y.Part{
				Kind:     agento11y.PartKindThinking,
				Thinking: "[thinking block omitted]",
			})

		case "tool_use":
			inputJSON := truncateJSON(block.Input, maxToolInputLen, r)
			parts = append(parts, agento11y.Part{
				Kind: agento11y.PartKindToolCall,
				ToolCall: &agento11y.ToolCall{
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

	return []agento11y.Message{{
		Role:  agento11y.RoleAssistant,
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
