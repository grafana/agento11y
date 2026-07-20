package agento11y

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
)

// ExecuteToolCallsOptions configures [Client.ExecuteToolCalls].
//
// Tags is reserved for forward compatibility and is not applied to tool spans in this release.
type ExecuteToolCallsOptions struct {
	ConversationID    string
	ConversationTitle string
	AgentName         string
	AgentVersion      string
	ContentCapture    ContentCaptureMode
	RequestModel      string
	RequestProvider   string
	ToolType          string
	Tags              map[string]string
}

// ToolCallExecutor runs one tool invocation for [Client.ExecuteToolCalls].
// args is the raw JSON tool arguments from the assistant message (or "{}" when empty).
type ToolCallExecutor func(ctx context.Context, toolName string, args json.RawMessage) (result any, err error)

func buildToolResultMessageSuccess(toolName, callID string, result any) Message {
	tr := ToolResult{
		ToolCallID: callID,
		Name:       toolName,
	}
	if result == nil {
		return Message{Role: RoleTool, Name: toolName, Parts: []Part{ToolResultPart(tr)}}
	}
	switch v := result.(type) {
	case string:
		tr.Content = v
	case []byte:
		tr.ContentJSON = json.RawMessage(v)
	case json.RawMessage:
		tr.ContentJSON = v
	default:
		data, err := json.Marshal(v)
		if err != nil {
			tr.Content = err.Error()
			return Message{Role: RoleTool, Name: toolName, Parts: []Part{ToolResultPart(tr)}}
		}
		tr.ContentJSON = data
	}
	return Message{Role: RoleTool, Name: toolName, Parts: []Part{ToolResultPart(tr)}}
}

func buildToolResultMessageError(toolName, callID string, execErr error) Message {
	msg := ""
	if execErr != nil {
		msg = execErr.Error()
	}
	return Message{
		Role: RoleTool,
		Name: toolName,
		Parts: []Part{
			ToolResultPart(ToolResult{
				ToolCallID: callID,
				Name:       toolName,
				Content:    msg,
				IsError:    true,
			}),
		},
	}
}

// ExecuteToolCalls walks assistant tool-call parts in messages, runs executor for each under
// execute_tool spans, and returns tool-role messages with tool_result parts for the next model turn.
//
// The returned context is the last tool execution context (or the input ctx when there are no tool calls).
func (c *Client) ExecuteToolCalls(ctx context.Context, messages []Message, executor ToolCallExecutor, opts ExecuteToolCallsOptions) (context.Context, []Message) {
	if c == nil || executor == nil {
		return ctx, nil
	}
	out := make([]Message, 0)
	toolType := strings.TrimSpace(opts.ToolType)
	if toolType == "" {
		toolType = "function"
	}

	lastCtx := ctx
	for _, msg := range messages {
		for _, part := range msg.Parts {
			if part.Kind != PartKindToolCall || part.ToolCall == nil {
				continue
			}
			tc := part.ToolCall
			name := strings.TrimSpace(tc.Name)
			if name == "" {
				continue
			}
			callID := strings.TrimSpace(tc.ID)
			args := tc.InputJSON
			if len(bytes.TrimSpace(args)) == 0 {
				args = json.RawMessage([]byte("{}"))
			}

			toolCtx, rec := c.StartToolExecution(ctx, ToolExecutionStart{
				ToolName:          name,
				ToolCallID:        callID,
				ToolType:          toolType,
				ConversationID:    opts.ConversationID,
				ConversationTitle: opts.ConversationTitle,
				AgentName:         opts.AgentName,
				AgentVersion:      opts.AgentVersion,
				RequestModel:      opts.RequestModel,
				RequestProvider:   opts.RequestProvider,
				ContentCapture:    opts.ContentCapture,
			})

			var argsObj any
			if err := json.Unmarshal(args, &argsObj); err != nil {
				argsObj = string(args)
			}
			if argsObj == nil {
				argsObj = map[string]any{}
			}

			result, execErr := executor(toolCtx, name, args)
			if execErr != nil {
				rec.SetExecError(execErr)
				out = append(out, buildToolResultMessageError(name, callID, execErr))
			} else {
				rec.SetResult(ToolExecutionEnd{
					Arguments: argsObj,
					Result:    result,
				})
				out = append(out, buildToolResultMessageSuccess(name, callID, result))
			}
			rec.End()
			lastCtx = toolCtx
		}
	}
	return lastCtx, out
}
