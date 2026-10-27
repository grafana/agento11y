package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"os"
	"path/filepath"
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

func dropCoveredSidechain(lines []transcript.Line, files []string, path string) []transcript.Line {
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, toolPath(file))
	}
	return subagent.DropCoveredSidechain(lines, paths, toolPath(path))
}

func toolScope(path string, line transcript.Line) string {
	path = toolPath(path)
	if line.IsSidechain && (!subagent.IsTranscript(path) || line.AgentID != "" && line.AgentID != subagent.AgentIDFromPath(path)) {
		return path + "#sidechain:" + line.AgentID
	}
	return path
}

func toolKey(session, scope, id string) string {
	key, _ := json.Marshal([]string{session, scope, id})
	return string(key)
}

func initToolState(st *state.Session, parent string, children []string) {
	if st.Tools.Version != toolStateVersion {
		// Upgrade starts at the old checkpoints. Historical tools must not be reconstructed.
		st.Tools = state.ToolState{
			Version:       toolStateVersion,
			Offsets:       map[string]int64{toolPath(parent): st.Offset},
			Pending:       map[string]*state.PendingTool{},
			LegacyOffsets: maps.Clone(st.SubagentOffsets),
		}
	}
	if st.Tools.Offsets == nil {
		st.Tools.Offsets = map[string]int64{}
	}
	for _, path := range children {
		key := toolPath(path)
		if _, initialized := st.Tools.Offsets[key]; !initialized {
			st.Tools.Offsets[key] = st.Tools.LegacyOffsets[subagent.FileKey(path)]
		}
	}
	if st.Tools.Pending == nil {
		st.Tools.Pending = map[string]*state.PendingTool{}
	}
	if st.Tools.Background == nil {
		st.Tools.Background = map[string]bool{}
	}
}

func collectTools(st *state.Session, path string, files []string, logger *log.Logger) error {
	path = toolPath(path)
	lines, offset, err := transcript.ReadComplete(path, st.Tools.Offsets[path])
	if err != nil {
		if logger != nil {
			logger.Printf("read tools %s: %v", path, err)
		}
		return err
	}
	st.Tools.Offsets[path] = offset
	lines = dropCoveredSidechain(lines, files, path)
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
				if block.Name == "Agent" {
					var input struct {
						Background bool `json:"run_in_background"`
					}
					if json.Unmarshal(block.Input, &input) == nil && input.Background {
						st.Tools.Background[toolKey(line.SessionID, scope, block.ID)] = true
					}
				}
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
				result := state.ToolResult{ToolSource: source, IsError: block.IsError, VerifiedName: verifiedSkillResult(line.ToolUseResult, blocks, block.ToolUseID)}
				if p.Result != nil {
					if p.Result.VerifiedName != result.VerifiedName || p.Result.IsError != result.IsError {
						p.Result.Conflict = true
					}
					p.Result.IsError = p.Result.IsError || result.IsError
				} else {
					p.Result = &result
				}
			}
		}
	}
	return nil
}

func verifiedSkillResult(raw json.RawMessage, blocks []transcript.UserContentBlock, id string) string {
	if !uniqueResultFields(raw) {
		return ""
	}
	var result struct {
		Success     json.RawMessage `json:"success"`
		CommandName string          `json:"commandName"`
		ToolUseID   string          `json:"tool_use_id"`
		CamelID     string          `json:"toolUseID"`
		CamelId     string          `json:"toolUseId"`
		IsError     bool            `json:"is_error"`
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return ""
	}
	// The host's aliases are exact keys; Go's struct decoder also accepts case variants.
	for _, field := range []struct {
		key string
		out any
	}{
		{"success", &result.Success}, {"commandName", &result.CommandName},
		{"tool_use_id", &result.ToolUseID}, {"toolUseID", &result.CamelID},
		{"toolUseId", &result.CamelId}, {"is_error", &result.IsError},
	} {
		if value, ok := fields[field.key]; ok && json.Unmarshal(value, field.out) != nil {
			return ""
		}
	}
	if string(result.Success) != "true" || result.IsError {
		return ""
	}
	explicit := ""
	for _, value := range []string{result.ToolUseID, result.CamelID, result.CamelId} {
		if value == "" {
			continue
		}
		if explicit != "" && explicit != value {
			return ""
		}
		explicit = value
	}
	count := 0
	for _, block := range blocks {
		if block.Type != "tool_result" {
			continue
		}
		if explicit == "" || block.ToolUseID == explicit {
			count++
			if block.ToolUseID != id || block.IsError {
				return ""
			}
		}
	}
	if count != 1 || explicit != "" && explicit != id {
		return ""
	}
	input, _ := json.Marshal(map[string]string{"skill": result.CommandName})
	name, ok := skilldetect.Detect("Skill", input)
	if !ok {
		return ""
	}
	return name
}

func uniqueResultFields(raw json.RawMessage) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return false
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return false
		}
		key, ok := token.(string)
		if !ok || seen[key] {
			return false
		}
		seen[key] = true
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return false
		}
	}
	_, err := decoder.Token()
	return err == nil
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
						if block.Type == "tool_result" && st.Tools.Background[toolKey(line.SessionID, toolScope(path, line), block.ToolUseID)] {
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

func associateTools(st *state.Session, path string, sources map[int]transcript.Line, gens []agento11y.Generation) [][]*state.PendingTool {
	parents := make([][]*state.PendingTool, len(gens))
	for i, gen := range gens {
		line, ok := sources[i]
		if !ok {
			continue
		}
		for _, msg := range gen.Output {
			for _, part := range msg.Parts {
				if part.ToolCall == nil {
					continue
				}
				key := toolKey(line.SessionID, toolScope(path, line), part.ToolCall.ID)
				p := st.Tools.Pending[key]
				if p == nil {
					continue
				}
				if p.Parent.ID != "" && p.Parent.ID != gen.ID {
					p.Ambiguous = true
					continue
				}
				if p.Parent.Ready {
					continue
				}
				parents[i] = append(parents[i], p)
				p.Parent = state.ToolParent{
					ID:             gen.ID,
					ConversationID: gen.ConversationID,
					AgentName:      gen.AgentName,
					AgentVersion:   gen.AgentVersion,
					Model:          gen.Model.Name,
					Provider:       gen.Model.Provider,
				}
			}
		}
	}
	return parents
}

func captureToolParent(parents []*state.PendingTool, sc trace.SpanContext) {
	for _, p := range parents {
		if p.Parent.Ready {
			continue
		}
		p.Parent.Ready = true
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
	defer f.Close()
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
	if line, err := readToolSource(p.Path, *p.Call); err == nil && toolSourceMatches(p, line, p.Call.At) {
		var msg transcript.AssistantMessage
		if json.Unmarshal(line.Message, &msg) == nil {
			for _, block := range msg.Content {
				if block.Type == "tool_use" && block.ID == p.ID && block.Name == p.Name {
					args = string(mapper.ToolInputJSON(block.Input, r))
					break
				}
			}
		}
	}
	if line, err := readToolSource(p.Path, p.Result.ToolSource); err == nil && toolSourceMatches(p, line, p.Result.At) {
		var msg transcript.UserMessage
		if json.Unmarshal(line.Message, &msg) == nil {
			_, blocks, _ := transcript.ParseUserContent(msg.Content)
			for _, block := range blocks {
				if block.Type == "tool_result" && block.ToolUseID == p.ID {
					result = mapper.ToolResultContent(block, r)
					break
				}
			}
		}
	}
	return args, result
}

// Transcript timing includes approval time, not just the tool body.
func toolSpanWindow(callAt, resultAt time.Time) (startedAt, completedAt time.Time) {
	if resultAt.Before(callAt) {
		return callAt, callAt
	}
	return callAt, resultAt
}

func emitPendingTools(ctx context.Context, client *agento11y.Client, st *state.Session, mode agento11y.ContentCaptureMode, r *redact.Redactor) {
	for key, p := range st.Tools.Pending {
		if p.Call == nil || p.Result == nil || p.Parent.ID == "" || !p.Parent.Ready || p.Ambiguous || p.Call.At.IsZero() || p.Result.At.IsZero() {
			continue
		}
		startedAt, completedAt := toolSpanWindow(p.Call.At, p.Result.At)
		start := agento11y.ToolExecutionStart{
			ToolName:        p.Name,
			ToolCallID:      p.ID,
			ToolType:        "function",
			ConversationID:  p.Parent.ConversationID,
			AgentName:       p.Parent.AgentName,
			AgentVersion:    p.Parent.AgentVersion,
			RequestModel:    p.Parent.Model,
			RequestProvider: p.Parent.Provider,
			StartedAt:       startedAt,
		}
		if p.Name == "Skill" && p.SkillName != "" {
			start.ExecutionKind = agento11y.ToolExecutionKindLoadSkill
			start.SkillName = p.SkillName
		}
		end := agento11y.ToolExecutionEnd{CompletedAt: completedAt}
		if p.Name == "Skill" && p.SkillName != "" && p.Result.VerifiedName == p.SkillName && !p.Result.IsError && !p.Result.Conflict {
			end.VerifiedSkillName = p.SkillName
		}
		if mode != agento11y.ContentCaptureModeMetadataOnly {
			args, result := toolContent(p, r)
			if args != "" {
				end.Arguments = args
			}
			if result != "" {
				end.Result = result
			}
		}
		_, rec := client.StartToolExecution(toolParentContext(ctx, p.Parent), start)
		if p.Result.IsError {
			rec.SetExecError(fmt.Errorf("tool returned error"))
		}
		rec.SetResult(end)
		rec.End()
		delete(st.Tools.Pending, key)
	}
}

func collectSessionTools(st *state.Session, parent string, files []string, logger *log.Logger) error {
	if err := collectTools(st, parent, files, logger); err != nil {
		return err
	}
	for _, path := range files {
		if toolPath(path) != toolPath(parent) {
			if err := collectTools(st, path, files, logger); err != nil {
				return err
			}
		}
	}
	return nil
}
