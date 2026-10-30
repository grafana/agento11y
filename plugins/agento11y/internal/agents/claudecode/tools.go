package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/mapper"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/skilldetect"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/state"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/subagent"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/transcript"
	"github.com/grafana/agento11y/plugins/agento11y/internal/redact"
	"go.opentelemetry.io/otel/trace"
)

const toolStateVersion = 1

func toolPath(path string) string {
	path, _ = filepath.Abs(path)
	prefix, suffix := path, ""
	for {
		if resolved, err := filepath.EvalSymlinks(prefix); err == nil {
			return filepath.Join(resolved, suffix)
		}
		parent := filepath.Dir(prefix)
		if parent == prefix {
			return filepath.Clean(path)
		}
		suffix = filepath.Join(filepath.Base(prefix), suffix)
		prefix = parent
	}
}

func toolScope(path string, line transcript.Line) string {
	path = toolPath(path)
	if line.IsSidechain && (!subagent.IsTranscript(path) || line.AgentID != "" && line.AgentID != subagent.AgentIDFromPath(path)) {
		return path + "#sidechain:" + line.AgentID
	}
	return path
}

func toolKey(session, scope, id string) string {
	b, _ := json.Marshal([]string{session, scope, id})
	return string(b)
}

func initToolState(st *state.Session, parent string, children []string) {
	legacy := st.Tools.Version != toolStateVersion
	if legacy {
		st.Tools = state.ToolState{
			Version:       toolStateVersion,
			Offsets:       map[string]int64{toolPath(parent): st.Offset},
			LegacyOffsets: maps.Clone(st.SubagentOffsets),
		}
	}
	if st.Tools.Offsets == nil {
		st.Tools.Offsets = map[string]int64{}
	}
	if st.Tools.LegacySpawnOffsets == nil {
		st.Tools.LegacySpawnOffsets = map[string]int64{}
	}
	if legacy && st.Offset > 0 {
		st.Tools.LegacySpawnOffsets[toolPath(parent)] = st.Tools.Offsets[toolPath(parent)]
	}
	// Map membership distinguishes an initialized zero from a newly discovered file.
	for _, path := range append([]string{parent}, children...) {
		key := toolPath(path)
		if _, ok := st.Tools.Offsets[key]; !ok {
			if key == toolPath(parent) {
				st.Tools.Offsets[key] = st.Offset
			} else {
				st.Tools.Offsets[key] = st.Tools.LegacyOffsets[subagent.FileKey(path)]
				if st.Tools.Offsets[key] > 0 {
					st.Tools.LegacySpawnOffsets[key] = st.Tools.Offsets[key]
				}
			}
		}
	}
	if st.Tools.Pending == nil {
		st.Tools.Pending = map[string]*state.PendingTool{}
	}
	if st.Tools.Spawns == nil {
		st.Tools.Spawns = map[string]*state.ToolSpawn{}
	}
}

func toolReadOffset(st *state.Session, path string, generationOffset int64) int64 {
	path = toolPath(path)
	if _, pending := st.Tools.LegacySpawnOffsets[path]; pending {
		return 0
	}
	return min(generationOffset, st.Tools.Offsets[path])
}

// Legacy checkpoints can precede a background acknowledgement. Recover only
// spawn classification from history, not tool observations or payloads.
func observeLegacyToolSpawns(st *state.Session, path string, raw []transcript.Line, offset int64, files []string) {
	for _, line := range uncoveredToolLines(raw, files, path) {
		if line.StartOffset >= offset {
			continue
		}
		scope := toolScope(path, line)
		switch line.Type {
		case "assistant":
			var msg transcript.AssistantMessage
			if json.Unmarshal(line.Message, &msg) != nil {
				continue
			}
			for _, block := range msg.Content {
				if block.Type != "tool_use" || block.Name != "Agent" || block.ID == "" {
					continue
				}
				var input struct {
					Background bool `json:"run_in_background"`
				}
				_ = json.Unmarshal(block.Input, &input)
				st.Tools.Spawns[toolKey(line.SessionID, scope, block.ID)] = &state.ToolSpawn{Path: path, Background: input.Background}
			}
		case "user":
			var msg transcript.UserMessage
			if json.Unmarshal(line.Message, &msg) != nil {
				continue
			}
			_, blocks, _ := transcript.ParseUserContent(msg.Content)
			for _, block := range blocks {
				if block.Type != "tool_result" {
					continue
				}
				if spawn := st.Tools.Spawns[toolKey(line.SessionID, scope, block.ToolUseID)]; spawn != nil {
					spawn.ResultEnd = line.EndOffset
				}
			}
		}
	}
}

func afterOffset(lines []transcript.Line, offset int64) []transcript.Line {
	out := make([]transcript.Line, 0, len(lines))
	for _, line := range lines {
		if line.StartOffset >= offset {
			out = append(out, line)
		}
	}
	return out
}

func uncoveredToolLines(lines []transcript.Line, files []string, path string) []transcript.Line {
	paths := make([]string, len(files))
	for i, file := range files {
		paths[i] = toolPath(file)
	}
	return subagent.DropCoveredSidechain(lines, paths, toolPath(path))
}

// observeTools consumes the accepted read also used by the mapper. Its cursor
// can pass an unfinished assistant turn without advancing the generation cursor.
func observeTools(st *state.Session, path string, raw []transcript.Line, pos int64, files []string) {
	path = toolPath(path)
	if offset, ok := st.Tools.LegacySpawnOffsets[path]; ok {
		observeLegacyToolSpawns(st, path, raw, offset, files)
		delete(st.Tools.LegacySpawnOffsets, path)
	}
	lines := uncoveredToolLines(afterOffset(raw, st.Tools.Offsets[path]), files, path)
	for _, line := range lines {
		scope := toolScope(path, line)
		at, _ := time.Parse(time.RFC3339Nano, line.Timestamp)
		source := state.ToolSource{Start: line.StartOffset, End: line.EndOffset, At: at}
		pending := func(id string) *state.PendingTool {
			key := toolKey(line.SessionID, scope, id)
			p := st.Tools.Pending[key]
			if p == nil {
				p = &state.PendingTool{SessionID: line.SessionID, Scope: scope, Path: path, ID: id}
				st.Tools.Pending[key] = p
			}
			return p
		}
		switch line.Type {
		case "assistant":
			var msg transcript.AssistantMessage
			if json.Unmarshal(line.Message, &msg) != nil {
				continue
			}
			for _, block := range msg.Content {
				if block.Type != "tool_use" || block.ID == "" {
					continue
				}
				p := pending(block.ID)
				skill, _ := skilldetect.Detect(block.Name, block.Input)
				if p.Call != nil {
					if p.Name != block.Name || p.SkillName != skill {
						p.Ambiguous = true
					}
					continue
				}
				p.Name = block.Name
				p.SkillName = skill
				p.Call = &source
				if block.Name == "Agent" {
					var input struct {
						Background bool `json:"run_in_background"`
					}
					_ = json.Unmarshal(block.Input, &input)
					st.Tools.Spawns[toolKey(line.SessionID, scope, block.ID)] = &state.ToolSpawn{Path: path, Background: input.Background}
				}
			}
		case "user":
			var msg transcript.UserMessage
			if json.Unmarshal(line.Message, &msg) != nil {
				continue
			}
			_, blocks, err := transcript.ParseUserContent(msg.Content)
			if err != nil {
				continue
			}
			for _, block := range blocks {
				if block.Type != "tool_result" || block.ToolUseID == "" {
					continue
				}
				p := pending(block.ToolUseID)
				if p.Result == nil {
					p.Result = &state.ToolResult{ToolSource: source, IsError: block.IsError}
					if p.Name == "Agent" {
						// Directory-only acknowledgements can identify a known child too.
						p.Result.ChildIDs = toolResultChildIDs(block, files)
					}
				} else {
					p.Result.IsError = p.Result.IsError || block.IsError
				}
				if spawn := st.Tools.Spawns[toolKey(line.SessionID, scope, block.ToolUseID)]; spawn != nil {
					spawn.ResultEnd = line.EndOffset
				}
			}
		}
	}
	st.Tools.Offsets[path] = pos
}

func toolResultChildIDs(block transcript.UserContentBlock, files []string) []string {
	content, _ := json.Marshal([]transcript.UserContentBlock{block})
	message, _ := json.Marshal(transcript.UserMessage{Content: content})
	return slices.Sorted(maps.Keys(subagent.CompletedKeys([]transcript.Line{{Type: "user", Message: message}}, files)))
}

func resolvePendingToolChildren(st *state.Session, files []string) {
	for key, p := range st.Tools.Pending {
		if p.Name != "Agent" || p.Result == nil || len(p.Result.ChildIDs) > 0 {
			continue
		}
		line, err := readToolSource(p.Path, p.Result.ToolSource)
		if err != nil || line.Type != "user" || !toolSourceMatches(p, line, p.Result.At) {
			continue
		}
		var msg transcript.UserMessage
		if json.Unmarshal(line.Message, &msg) != nil {
			continue
		}
		_, blocks, err := transcript.ParseUserContent(msg.Content)
		if err != nil {
			continue
		}
		for _, block := range blocks {
			if block.Type == "tool_result" && block.ToolUseID == p.ID {
				p.Result.ChildIDs = toolResultChildIDs(block, files)
				break
			}
		}
		if spawn := st.Tools.Spawns[key]; spawn != nil && !spawn.Background && p.Emitted && len(p.Result.ChildIDs) > 0 {
			if st.Tools.CompletedChildren == nil {
				st.Tools.CompletedChildren = map[string]bool{}
			}
			for _, id := range p.Result.ChildIDs {
				st.Tools.CompletedChildren[id] = true
			}
		}
	}
}

func blockingToolLines(st *state.Session, path string, lines []transcript.Line) []transcript.Line {
	out := make([]transcript.Line, 0, len(lines))
	for _, line := range lines {
		if line.Type == "user" {
			var msg transcript.UserMessage
			if json.Unmarshal(line.Message, &msg) == nil {
				_, blocks, err := transcript.ParseUserContent(msg.Content)
				if err == nil && len(blocks) > 0 {
					kept := make([]transcript.UserContentBlock, 0, len(blocks))
					for _, block := range blocks {
						spawn := st.Tools.Spawns[toolKey(line.SessionID, toolScope(path, line), block.ToolUseID)]
						if block.Type == "tool_result" && spawn != nil && spawn.Background {
							continue
						}
						kept = append(kept, block)
					}
					msg.Content, _ = json.Marshal(kept)
					line.Message, _ = json.Marshal(msg)
				}
			}
		}
		out = append(out, line)
	}
	return out
}

// Output position, source interval, and scope identify the lifecycle. Generation
// IDs alone are not unique when Claude reuses an assistant request ID.
func eachToolAssociation(st *state.Session, path string, sources map[int]transcript.Line, gens []agento11y.Generation, visit func(int, *state.PendingTool, *agento11y.ToolCall)) {
	for i, gen := range gens {
		line, ok := sources[i]
		if !ok {
			continue
		}
		rawSources := line.AssistantSources
		if len(rawSources) == 0 {
			rawSources = []transcript.Line{line}
		}
		type callKey struct{ id, name string }
		callSources := map[callKey][]transcript.Line{}
		for _, source := range rawSources {
			var msg transcript.AssistantMessage
			if json.Unmarshal(source.Message, &msg) != nil {
				continue
			}
			for _, block := range msg.Content {
				if block.Type == "tool_use" {
					key := callKey{block.ID, block.Name}
					callSources[key] = append(callSources[key], source)
				}
			}
		}
		seen := map[string]bool{}
		for _, msg := range gen.Output {
			for _, part := range msg.Parts {
				if part.ToolCall == nil {
					continue
				}
				call := callKey{part.ToolCall.ID, part.ToolCall.Name}
				queue := callSources[call]
				if len(queue) == 0 {
					continue
				}
				source := queue[0]
				callSources[call] = queue[1:]
				key := toolKey(source.SessionID, toolScope(path, source), part.ToolCall.ID)
				p := st.Tools.Pending[key]
				if seen[key] || p == nil || p.Call == nil || p.Name != part.ToolCall.Name || p.Call.Start != source.StartOffset || p.Call.End != source.EndOffset || p.Parent.Ready {
					continue
				}
				seen[key] = true
				visit(i, p, part.ToolCall)
			}
		}
	}
}

func spawnAgentName(call *agento11y.ToolCall) string {
	// Use mapped arguments so subagent names respect generation redaction and size limits.
	var input struct {
		Type string `json:"subagent_type"`
	}
	_ = json.Unmarshal(call.InputJSON, &input)
	name := strings.ToLower(strings.TrimSpace(input.Type))
	if name == "" {
		return "subagent"
	}
	return name
}

func associateTools(st *state.Session, path string, sources map[int]transcript.Line, gens []agento11y.Generation) [][]*state.PendingTool {
	out := make([][]*state.PendingTool, len(gens))
	eachToolAssociation(st, path, sources, gens, func(i int, p *state.PendingTool, call *agento11y.ToolCall) {
		gen := gens[i]
		p.Parent = state.ToolParent{
			ID:             gen.ID,
			ConversationID: gen.ConversationID,
			AgentName:      gen.AgentName,
			AgentVersion:   gen.AgentVersion,
			Model:          gen.Model.Name,
			Provider:       gen.Model.Provider,
		}
		if spawn := st.Tools.Spawns[toolKey(p.SessionID, p.Scope, p.ID)]; spawn != nil {
			spawn.AgentName = spawnAgentName(call)
		}
		out[i] = append(out[i], p)
	})
	return out
}

// Resolve child links before Finalize, without assigning tool parent metadata.
// Unlike ParentMap's legacy index, this lookup also distinguishes sidechains.
func rememberMappedToolChildren(st *state.Session, path string, sources map[int]transcript.Line, gens []agento11y.Generation, base string) {
	eachToolAssociation(st, path, sources, gens, func(i int, p *state.PendingTool, call *agento11y.ToolCall) {
		if call.Name != "Agent" || p.Result == nil {
			return
		}
		rememberChildIDs(st, p.Result.ChildIDs, gens[i].ID, base, spawnAgentName(call))
	})
}

func rememberChildIDs(st *state.Session, ids []string, parent, base, suffix string) {
	if len(ids) == 0 {
		return
	}
	if st.SubagentParents == nil {
		st.SubagentParents = map[string]string{}
	}
	if st.SubagentNames == nil {
		st.SubagentNames = map[string]string{}
	}
	if base == "" {
		base = AgentName
	}
	base = strings.SplitN(base, "/", 2)[0]
	for _, id := range ids {
		st.SubagentParents[id] = parent
		st.SubagentNames[id] = base + "/" + suffix
	}
}

func rememberToolChildren(st *state.Session, base string) {
	for key, p := range st.Tools.Pending {
		spawn := st.Tools.Spawns[key]
		if spawn == nil || p.Result == nil || len(p.Result.ChildIDs) == 0 || p.Parent.ID == "" {
			continue
		}
		rememberChildIDs(st, p.Result.ChildIDs, p.Parent.ID, base, spawn.AgentName)
		if p.Emitted {
			delete(st.Tools.Pending, key)
		}
	}
	if len(st.SubagentParents) == 0 {
		st.SubagentParents = nil
	}
	if len(st.SubagentNames) == 0 {
		st.SubagentNames = nil
	}
}

func captureToolParent(parents []*state.PendingTool, sc trace.SpanContext) {
	for _, p := range parents {
		p.Parent.Ready = true
		if !sc.IsValid() {
			continue
		}
		p.Parent.TraceID = sc.TraceID().String()
		p.Parent.SpanID = sc.SpanID().String()
		p.Parent.Flags = byte(sc.TraceFlags())
		p.Parent.TraceState = sc.TraceState().String()
		p.Parent.Remote = sc.IsRemote()
	}
}

func toolParentContext(ctx context.Context, parent state.ToolParent) context.Context {
	tid, _ := trace.TraceIDFromHex(parent.TraceID)
	sid, _ := trace.SpanIDFromHex(parent.SpanID)
	ts, _ := trace.ParseTraceState(parent.TraceState)
	// An invalid stored context must not inherit an unrelated hook span.
	return trace.ContextWithSpanContext(ctx, trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    tid,
		SpanID:     sid,
		TraceFlags: trace.TraceFlags(parent.Flags),
		TraceState: ts,
		Remote:     parent.Remote,
	}))
}

func readToolSource(path string, source state.ToolSource) (transcript.Line, error) {
	var line transcript.Line
	length := source.End - source.Start
	if source.Start < 0 || length <= 0 || length > 10*1024*1024 {
		return line, fmt.Errorf("invalid tool source range")
	}
	f, err := os.Open(path)
	if err != nil {
		return line, err
	}
	defer func() { _ = f.Close() }()
	raw := make([]byte, int(length))
	if _, err = f.ReadAt(raw, source.Start); err != nil {
		return line, err
	}
	err = json.Unmarshal(raw, &line)
	return line, err
}

func toolSourceMatches(p *state.PendingTool, line transcript.Line, at time.Time) bool {
	observed, err := time.Parse(time.RFC3339Nano, line.Timestamp)
	return err == nil && observed.Equal(at) && line.SessionID == p.SessionID && toolScope(p.Path, line) == p.Scope
}

func toolContent(p *state.PendingTool, r *redact.Redactor) (string, string) {
	var args, result string
	if line, err := readToolSource(p.Path, *p.Call); err == nil && line.Type == "assistant" && toolSourceMatches(p, line, p.Call.At) {
		var msg transcript.AssistantMessage
		if json.Unmarshal(line.Message, &msg) == nil {
			for _, block := range msg.Content {
				skill, _ := skilldetect.Detect(block.Name, block.Input)
				if block.Type == "tool_use" && block.ID == p.ID && block.Name == p.Name && skill == p.SkillName {
					args = string(mapper.ToolInputJSON(block.Input, r))
					break
				}
			}
		}
	}
	if line, err := readToolSource(p.Path, p.Result.ToolSource); err == nil && line.Type == "user" && toolSourceMatches(p, line, p.Result.At) {
		var msg transcript.UserMessage
		if json.Unmarshal(line.Message, &msg) == nil {
			_, blocks, _ := transcript.ParseUserContent(msg.Content)
			for _, block := range blocks {
				if block.Type == "tool_result" && block.ToolUseID == p.ID && block.IsError == p.Result.IsError {
					result = mapper.ToolResultContent(block, r)
					break
				}
			}
		}
	}
	return args, result
}

// The interval includes approval time. Missing timestamps are rejected before
// this function; reversed, present timestamps retain the zero-clamp policy.
func toolSpanWindow(callAt, resultAt time.Time) (time.Time, time.Time) {
	if resultAt.Before(callAt) {
		return callAt, callAt
	}
	return callAt, resultAt
}

func emitPendingTools(ctx context.Context, clients *recordClients, st *state.Session, mode agento11y.ContentCaptureMode, r *redact.Redactor) {
	// Stable order also makes adjacent tools deterministic for local consumers.
	for _, key := range slices.Sorted(maps.Keys(st.Tools.Pending)) {
		p := st.Tools.Pending[key]
		if p.Emitted || p.Call == nil || p.Result == nil || p.Parent.ID == "" || !p.Parent.Ready || p.Ambiguous || p.Call.At.IsZero() || p.Result.At.IsZero() {
			continue
		}
		startAt, endAt := toolSpanWindow(p.Call.At, p.Result.At)
		start := agento11y.ToolExecutionStart{
			ToolName:        p.Name,
			ToolCallID:      p.ID,
			ToolType:        "function",
			ConversationID:  p.Parent.ConversationID,
			AgentName:       p.Parent.AgentName,
			AgentVersion:    p.Parent.AgentVersion,
			RequestModel:    p.Parent.Model,
			RequestProvider: p.Parent.Provider,
			StartedAt:       startAt,
		}
		if p.Name == "Skill" && p.SkillName != "" {
			start.ExecutionKind = agento11y.ToolExecutionKindLoadSkill
			start.SkillName = p.SkillName
		}
		end := agento11y.ToolExecutionEnd{CompletedAt: endAt}
		if mode != agento11y.ContentCaptureModeMetadataOnly {
			args, result := toolContent(p, r)
			if args != "" {
				end.Arguments = args
			}
			if result != "" {
				end.Result = result
			}
		}
		_, rec := clients.forRecord(toolRecordKey(key)).StartToolExecution(toolParentContext(ctx, p.Parent), start)
		if p.Result.IsError {
			rec.SetExecError(fmt.Errorf("tool returned error"))
		}
		rec.SetResult(end)
		rec.End()
		if p.Name == "Agent" && len(p.Result.ChildIDs) == 0 {
			p.Emitted = true
			continue
		}
		delete(st.Tools.Pending, key)
	}
}

func pruneToolSpawns(st *state.Session, parent string) {
	for key, spawn := range st.Tools.Spawns {
		if pending := st.Tools.Pending[key]; pending != nil && pending.Emitted {
			continue
		}
		offset := st.Offset
		if toolPath(spawn.Path) != toolPath(parent) {
			offset = st.SubagentOffsets[subagent.FileKey(spawn.Path)]
		}
		if spawn.ResultEnd > 0 && offset >= spawn.ResultEnd {
			delete(st.Tools.Spawns, key)
		}
	}
}
