package claudeevals

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/transcript"
)

// Trajectory records one actual agent invocation, not a fabricated single LLM
// call. Claude's assistant events contain partial token counts; only the final
// result has authoritative invocation usage. Child spans retain model/tool
// activity without attributing that aggregate usage to each call again.
type Trajectory struct {
	Generation agento11y.Generation `json:"generation"`
	Activities []Activity           `json:"activities"`
	SHA256     string               `json:"sha256"`
}

type Activity struct {
	Key          string          `json:"key"`
	Kind         string          `json:"kind"`
	Name         string          `json:"name"`
	Model        string          `json:"model,omitempty"`
	ParentToolID string          `json:"parent_tool_id,omitempty"`
	StartedAt    time.Time       `json:"started_at"`
	CompletedAt  time.Time       `json:"completed_at"`
	Arguments    json.RawMessage `json:"arguments,omitempty"`
	Result       json.RawMessage `json:"result,omitempty"`
	Error        bool            `json:"error,omitempty"`
}

type streamUsage struct {
	Input      *int64 `json:"input_tokens"`
	Output     *int64 `json:"output_tokens"`
	CacheRead  *int64 `json:"cache_read_input_tokens"`
	CacheWrite *int64 `json:"cache_creation_input_tokens"`
}

func (u streamUsage) tokens() (agento11y.TokenUsage, error) {
	if u.Input == nil || u.Output == nil {
		return agento11y.TokenUsage{}, errors.New("terminal result is missing authoritative token usage")
	}
	value := func(p *int64) int64 {
		if p == nil {
			return 0
		}
		return *p
	}
	for _, n := range []*int64{u.Input, u.Output, u.CacheRead, u.CacheWrite} {
		if value(n) < 0 || value(n) > 1_000_000_000 {
			return agento11y.TokenUsage{}, errors.New("invalid token count in terminal result")
		}
	}
	return (agento11y.TokenUsage{
		InputTokens: *u.Input + value(u.CacheRead) + value(u.CacheWrite), OutputTokens: *u.Output,
		CacheReadInputTokens: value(u.CacheRead), CacheWriteInputTokens: value(u.CacheWrite),
		InputSemantics: agento11y.TokenInputSemanticsInclusive,
	}).Normalize(), nil
}

type streamEvent struct {
	UUID         string `json:"uuid"`
	Type         string `json:"type"`
	Subtype      string `json:"subtype"`
	SessionID    string `json:"session_id"`
	Timestamp    string `json:"timestamp"`
	ParentToolID string `json:"parent_tool_use_id"`
	Model        string `json:"model"`
	Version      string `json:"claude_code_version"`
	Message      struct {
		ID      string          `json:"id"`
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	Usage      *streamUsage `json:"usage"`
	DurationMS *int64       `json:"duration_ms"`
	StopReason string       `json:"stop_reason"`
	IsError    bool         `json:"is_error"`
}

// readTrajectory only opens out/trace.jsonl below the explicitly authorized
// root. os.Root prevents symlink/traversal escapes. Never traverse a retained
// sandbox's sealed home, execute its code, or upload its init/config record.
func readTrajectory(rootPath, sourcePath string, a attempt, prompt, generationID, title string) (*Trajectory, error) {
	rootPath, err := filepath.EvalSymlinks(rootPath)
	if err != nil {
		return nil, errors.New("cannot resolve --trace-root")
	}
	rootPath, err = filepath.Abs(rootPath)
	if err != nil {
		return nil, err
	}
	if sourcePath == "" {
		return nil, errors.New("tracePath missing; rerun Claude with --keep-temp")
	}
	path := sourcePath
	if !filepath.IsAbs(path) {
		path = filepath.Join(rootPath, path)
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return nil, errors.New("retained trace missing; rerun Claude with --keep-temp")
	}
	rel, err := filepath.Rel(rootPath, path)
	if err != nil || !filepath.IsLocal(rel) || filepath.Base(rel) != "trace.jsonl" || filepath.Base(filepath.Dir(rel)) != "out" {
		return nil, errors.New("tracePath must be an out/trace.jsonl file inside --trace-root")
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	info, err := root.Stat(rel)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxDocumentBytes {
		return nil, errors.New("trace must be a regular file of at most 32 MiB")
	}
	file, err := root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return parseTrajectory(file, a, prompt, generationID, title)
}

func parseTrajectory(reader io.Reader, a attempt, prompt, generationID, title string) (*Trajectory, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maxDocumentBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxDocumentBytes {
		return nil, errors.New("trace exceeds 32 MiB")
	}
	start, err := time.Parse(time.RFC3339Nano, a.StartedAt)
	if err != nil {
		return nil, errors.New("trajectory requires an attempt startedAt timestamp")
	}
	t := &Trajectory{SHA256: fmt.Sprintf("%x", sha256.Sum256(data))}
	g := &t.Generation
	g.ID, g.ConversationTitle = generationID, title
	g.AgentName, g.OperationName, g.Mode = "claude-code-eval", "invoke_agent", agento11y.GenerationModeSync
	g.StartedAt, g.CompletedAt = start, start
	g.Input = []agento11y.Message{agento11y.UserTextMessage(prompt)}
	g.Metadata = map[string]any{"source": "claude-plugin-eval", "granularity": "agent_invocation", "usage_source": "terminal_result", "timing_source": "transcript_bounds", "trace_sha256": t.SHA256}
	previous := map[string]time.Time{"": start}
	models := map[string]bool{}
	calls := map[string]int{}
	modelCalls := map[string]int{}
	seenEvents := map[string]bool{}
	var terminal *streamEvent
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	for scanner.Scan() {
		var event streamEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return nil, errors.New("invalid Claude stream JSONL record")
		}
		if event.SessionID != "" {
			if g.ConversationID != "" && g.ConversationID != event.SessionID {
				return nil, errors.New("trace contains multiple session IDs")
			}
			g.ConversationID = event.SessionID
		}
		switch event.Type {
		case "system":
			if event.Subtype == "init" {
				g.AgentVersion = event.Version
				g.Model = agento11y.ModelRef{Provider: "anthropic", Name: event.Model}
			}
		case "result":
			copy := event
			terminal = &copy // cumulative session totals: never sum result records
		case "assistant", "user":
			at, err := time.Parse(time.RFC3339Nano, event.Timestamp)
			if err != nil {
				return nil, errors.New("conversation event is missing a valid timestamp")
			}
			if at.Before(start) {
				at = start
			}
			if at.After(g.CompletedAt) {
				g.CompletedAt = at
			}
			key := event.UUID
			if key == "" {
				key = string(scanner.Bytes())
			}
			if seenEvents[key] {
				continue
			}
			seenEvents[key] = true
			if event.Type == "assistant" {
				if event.Message.Model == "<synthetic>" {
					continue
				}
				model := event.Message.Model
				if model == "" {
					model = g.Model.Name
				}
				models[model] = true
				modelKey := event.ParentToolID + ":" + event.Message.ID
				index, exists := modelCalls[modelKey]
				if !exists {
					from := previous[event.ParentToolID]
					if from.IsZero() || from.After(at) {
						from = at
					}
					index = len(t.Activities)
					modelCalls[modelKey] = index
					t.Activities = append(t.Activities, Activity{Key: "model:" + modelKey, Kind: "chat", Name: model, Model: model, ParentToolID: event.ParentToolID, StartedAt: from, CompletedAt: at})
				} else {
					t.Activities[index].CompletedAt = at
				}
				var blocks []struct {
					Type     string          `json:"type"`
					Text     string          `json:"text"`
					Thinking string          `json:"thinking"`
					ID       string          `json:"id"`
					Name     string          `json:"name"`
					Input    json.RawMessage `json:"input"`
				}
				if err := json.Unmarshal(event.Message.Content, &blocks); err != nil {
					return nil, errors.New("invalid assistant content")
				}
				msg := agento11y.Message{Role: agento11y.RoleAssistant}
				for _, b := range blocks {
					switch b.Type {
					case "text":
						if b.Text != "" {
							msg.Parts = append(msg.Parts, agento11y.Part{Kind: agento11y.PartKindText, Text: b.Text})
						}
					case "thinking":
						// Claude may emit an empty thinking block with only a signature.
						// There is no reasoning text to export in that event.
						if b.Thinking != "" {
							msg.Parts = append(msg.Parts, agento11y.Part{Kind: agento11y.PartKindThinking, Thinking: b.Thinking})
						}
					case "tool_use":
						if b.ID == "" || b.Name == "" {
							return nil, errors.New("tool call is missing its ID or name")
						}
						if _, exists := calls[b.ID]; exists {
							return nil, errors.New("duplicate tool call ID")
						}
						msg.Parts = append(msg.Parts, agento11y.Part{Kind: agento11y.PartKindToolCall, ToolCall: &agento11y.ToolCall{ID: b.ID, Name: b.Name, InputJSON: b.Input}})
						calls[b.ID] = len(t.Activities)
						t.Activities = append(t.Activities, Activity{Key: b.ID, Kind: "execute_tool", Name: b.Name, Model: model, ParentToolID: event.ParentToolID, StartedAt: at, CompletedAt: at, Arguments: b.Input})
					default:
						return nil, fmt.Errorf("unsupported assistant content type %q; no partial conversation exported", b.Type)
					}
				}
				if len(msg.Parts) > 0 {
					g.Output = append(g.Output, msg)
				}
			} else {
				text, blocks, err := transcript.ParseUserContent(event.Message.Content)
				if err != nil {
					return nil, errors.New("invalid user/tool content")
				}
				if text != "" {
					g.Output = append(g.Output, agento11y.UserTextMessage(text))
				}
				for _, b := range blocks {
					switch b.Type {
					case "text":
						if b.Text != "" {
							g.Output = append(g.Output, agento11y.UserTextMessage(b.Text))
						}
					case "tool_result":
						index, ok := calls[b.ToolUseID]
						if !ok {
							return nil, errors.New("tool result has no matching call")
						}
						tool := &t.Activities[index]
						tool.CompletedAt = at
						if at.Before(tool.StartedAt) {
							tool.CompletedAt = tool.StartedAt
						}
						tool.Result, tool.Error = b.RawContent, b.IsError
						g.Output = append(g.Output, agento11y.Message{Role: agento11y.RoleTool, Parts: []agento11y.Part{{Kind: agento11y.PartKindToolResult, ToolResult: &agento11y.ToolResult{ToolCallID: b.ToolUseID, Name: tool.Name, Content: b.Content(), IsError: b.IsError}}}})
					default:
						return nil, fmt.Errorf("unsupported user content type %q; no partial conversation exported", b.Type)
					}
				}
			}
			if at.After(previous[event.ParentToolID]) {
				previous[event.ParentToolID] = at
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return t.finish(terminal, models)
}

func (t *Trajectory) finish(terminal *streamEvent, models map[string]bool) (*Trajectory, error) {
	g := &t.Generation
	if g.ConversationID == "" || len(g.Output) == 0 {
		return nil, errors.New("trace has no recorded conversation")
	}
	if terminal == nil || terminal.Usage == nil {
		return nil, errors.New("trace lacks a terminal result with final token usage; refusing to guess")
	}
	usage, err := terminal.Usage.tokens()
	if err != nil {
		return nil, err
	}
	g.Usage = usage
	g.StopReason = terminal.StopReason
	if terminal.IsError {
		g.CallError = "Claude agent invocation failed"
	}
	if terminal.DurationMS != nil {
		if *terminal.DurationMS < 0 || *terminal.DurationMS > 86_400_000 {
			return nil, errors.New("invalid terminal duration_ms")
		}
		end := g.StartedAt.Add(time.Duration(*terminal.DurationMS) * time.Millisecond)
		if end.After(g.CompletedAt) {
			g.CompletedAt = end
		}
		g.Metadata["source_duration_ms"] = *terminal.DurationMS
	}
	if len(models) == 1 {
		for model := range models {
			g.Model.Name = model
		}
	}
	if len(models) > 1 {
		g.Model.Name = "mixed"
		g.Metadata["multiple_models"] = true
	}
	if strings.TrimSpace(g.Model.Name) == "" {
		return nil, errors.New("trace is missing model identity")
	}
	return t, nil
}
