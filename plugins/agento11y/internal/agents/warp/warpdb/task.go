package warpdb

import (
	"encoding/json"
	"fmt"
	"math"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// The decoder below reads the subset of Warp's warp.multi_agent.v1.Task
// message that history import needs. Field numbers follow the public schema at
// https://github.com/warpdotdev/warp-proto-apis (apis/multi_agent/v1). The
// generated bindings are AGPL-licensed, so this package decodes the wire format
// directly instead of importing them. Unknown fields and fields with an
// unexpected wire type are skipped, so additive upstream changes stay readable.

// Task is one decoded agent_tasks.task value.
type Task struct {
	ID           string
	Description  string
	ParentTaskID string
	Messages     []Message
}

// MessageKind identifies which Message.message oneof variant was set.
type MessageKind int

// The message variants the history mapper consumes. Every other variant
// decodes as KindOther.
const (
	KindOther MessageKind = iota
	KindUserQuery
	KindAgentOutput
	KindReasoning
	KindToolCall
	KindToolResult
	KindModelUsed
	KindRequestMetadata
)

// Message is one decoded task message. Only the fields for Kind are set.
type Message struct {
	ID        string
	TaskID    string
	RequestID string
	Timestamp time.Time
	Kind      MessageKind

	// Text holds the user query, agent output, or reasoning text.
	Text       string
	Model      ModelUsed
	ToolCall   ToolCall
	ToolResult ToolResult
	Request    RequestMetadata
}

// ModelUsed names the model that served a request.
type ModelUsed struct {
	ID          string
	DisplayName string
	Fallback    bool
}

// ToolCall is one agent tool invocation. Input is a JSON object; it is "{}"
// for tools whose arguments this decoder does not read.
type ToolCall struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// ToolResult is one tool outcome. Content is set only for tools whose result
// this decoder reads.
type ToolResult struct {
	CallID  string
	Name    string
	Content string
	IsError bool
}

// Outcome is the terminal state of an agent request.
type Outcome int

// Outcome values match Message.RequestMetadata.Outcome.
const (
	OutcomeUnspecified Outcome = 0
	OutcomeCompleted   Outcome = 1
	OutcomeCanceled    Outcome = 2
	OutcomeErrored     Outcome = 3
)

// RequestMetadata is the per-request timing and billing summary Warp appends
// to a request's messages. Older Warp versions did not write it.
type RequestMetadata struct {
	StartedAt    time.Time
	EndedAt      time.Time
	FirstTokenAt time.Time
	Usage        []ModelUsage
	Incomplete   bool
	Outcome      Outcome
}

// ModelUsage is the usage charged to one model within a request, summed across
// usage categories and key sources (Warp-managed, BYOK, custom endpoint).
type ModelUsage struct {
	Model      string
	Input      int64 // non-cached input tokens
	Output     int64
	CacheRead  int64
	CacheWrite int64
	CostCents  float64
}

// Message.message oneof field numbers.
const (
	msgFieldID              = 1
	msgFieldUserQuery       = 2
	msgFieldAgentOutput     = 3
	msgFieldToolCall        = 4
	msgFieldToolCallResult  = 5
	msgFieldTaskID          = 11
	msgFieldRequestID       = 13
	msgFieldTimestamp       = 14
	msgFieldAgentReasoning  = 15
	msgFieldModelUsed       = 25
	msgFieldRequestMetadata = 30
)

// toolCallNames maps Message.ToolCall.tool oneof field numbers to tool names.
var toolCallNames = map[protowire.Number]string{
	2: "run_shell_command", 3: "search_codebase", 4: "server", 5: "read_files",
	6: "apply_file_diffs", 7: "suggest_plan", 8: "suggest_create_plan", 9: "grep",
	10: "file_glob", 11: "read_mcp_resource", 12: "call_mcp_tool",
	13: "write_to_long_running_shell_command", 14: "suggest_new_conversation",
	15: "file_glob_v2", 16: "suggest_prompt", 17: "open_code_review", 18: "init_project",
	19: "subagent", 20: "read_documents", 21: "edit_documents", 22: "create_documents",
	23: "read_shell_command_output", 24: "use_computer", 25: "insert_review_comments",
	26: "read_skill", 27: "request_computer_use", 28: "fetch_conversation",
	30: "send_message_to_agent", 31: "transfer_shell_command_control_to_user",
	32: "ask_user_question", 34: "upload_file_artifact", 35: "run_agents",
	36: "wait_for_events", 37: "start_recording", 38: "stop_recording",
}

// toolResultNames maps Message.ToolCallResult.result oneof field numbers to
// tool names. Field 14 is the cancel variant.
var toolResultNames = map[protowire.Number]string{
	2: "run_shell_command", 3: "search_codebase", 4: "server", 5: "read_files",
	6: "apply_file_diffs", 7: "suggest_plan", 8: "suggest_create_plan", 9: "grep",
	10: "file_glob", 15: "read_mcp_resource", 16: "call_mcp_tool",
	17: "write_to_long_running_shell_command", 18: "suggest_new_conversation",
	19: "file_glob_v2", 20: "suggest_prompt", 21: "open_code_review", 22: "init_project",
	23: "subagent", 24: "read_documents", 25: "edit_documents", 26: "create_documents",
	27: "read_shell_command_output", 28: "use_computer", 29: "insert_review_comments",
	30: "read_skill", 31: "request_computer_use", 32: "fetch_conversation",
	34: "send_message_to_agent", 35: "transfer_shell_command_control_to_user",
	36: "ask_user_question", 38: "upload_file_artifact", 39: "run_agents",
	40: "wait_for_events", 41: "start_recording", 42: "stop_recording",
}

const toolResultCancel = 14

// field is one decoded wire field. Bytes is set for length-delimited fields and
// Uint for varint and fixed-width fields.
type field struct {
	Num   protowire.Number
	Type  protowire.Type
	Bytes []byte
	Uint  uint64
}

func (f field) str() string { return string(f.Bytes) }

func (f field) isBytes() bool { return f.Type == protowire.BytesType }

func (f field) isVarint() bool { return f.Type == protowire.VarintType }

func (f field) float32() float64 {
	if f.Type != protowire.Fixed32Type {
		return 0
	}
	return float64(math.Float32frombits(uint32(f.Uint)))
}

func eachField(b []byte, fn func(field) error) error {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return protowire.ParseError(n)
		}
		b = b[n:]
		f := field{Num: num, Type: typ}
		switch typ {
		case protowire.VarintType:
			f.Uint, n = protowire.ConsumeVarint(b)
		case protowire.Fixed32Type:
			var v uint32
			v, n = protowire.ConsumeFixed32(b)
			f.Uint = uint64(v)
		case protowire.Fixed64Type:
			f.Uint, n = protowire.ConsumeFixed64(b)
		case protowire.BytesType:
			f.Bytes, n = protowire.ConsumeBytes(b)
		default:
			n = protowire.ConsumeFieldValue(num, typ, b)
		}
		if n < 0 {
			return protowire.ParseError(n)
		}
		b = b[n:]
		if err := fn(f); err != nil {
			return err
		}
	}
	return nil
}

// DecodeTask decodes one serialized Task.
func DecodeTask(b []byte) (Task, error) {
	var task Task
	err := eachField(b, func(f field) error {
		if !f.isBytes() {
			return nil
		}
		switch f.Num {
		case 1:
			task.ID = f.str()
		case 2:
			task.Description = f.str()
		case 3:
			return eachField(f.Bytes, func(dep field) error {
				if dep.Num == 1 && dep.isBytes() {
					task.ParentTaskID = dep.str()
				}
				return nil
			})
		case 5:
			msg, err := decodeMessage(f.Bytes)
			if err != nil {
				return err
			}
			task.Messages = append(task.Messages, msg)
		default:
		}
		return nil
	})
	if err != nil {
		return Task{}, fmt.Errorf("decode task: %w", err)
	}
	return task, nil
}

// RequestIDs returns the distinct request IDs in a serialized Task in first-seen
// order. It reads no message content.
func RequestIDs(b []byte) ([]string, error) {
	var ids []string
	seen := map[string]bool{}
	err := eachField(b, func(f field) error {
		if f.Num != 5 || !f.isBytes() {
			return nil
		}
		return eachField(f.Bytes, func(mf field) error {
			if mf.Num == msgFieldRequestID && mf.isBytes() {
				if id := mf.str(); id != "" && !seen[id] {
					seen[id] = true
					ids = append(ids, id)
				}
			}
			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("scan task: %w", err)
	}
	return ids, nil
}

func decodeMessage(b []byte) (Message, error) {
	var msg Message
	err := eachField(b, func(f field) error {
		if !f.isBytes() {
			return nil
		}
		var err error
		switch f.Num {
		case msgFieldID:
			msg.ID = f.str()
		case msgFieldTaskID:
			msg.TaskID = f.str()
		case msgFieldRequestID:
			msg.RequestID = f.str()
		case msgFieldTimestamp:
			msg.Timestamp, err = decodeTimestamp(f.Bytes)
		case msgFieldUserQuery:
			msg.Kind = KindUserQuery
			msg.Text, err = stringField(f.Bytes, 1)
		case msgFieldAgentOutput:
			msg.Kind = KindAgentOutput
			msg.Text, err = stringField(f.Bytes, 1)
		case msgFieldAgentReasoning:
			msg.Kind = KindReasoning
			msg.Text, err = stringField(f.Bytes, 1)
		case msgFieldModelUsed:
			msg.Kind = KindModelUsed
			msg.Model, err = decodeModelUsed(f.Bytes)
		case msgFieldToolCall:
			msg.Kind = KindToolCall
			msg.ToolCall, err = decodeToolCall(f.Bytes)
		case msgFieldToolCallResult:
			msg.Kind = KindToolResult
			msg.ToolResult, err = decodeToolResult(f.Bytes)
		case msgFieldRequestMetadata:
			msg.Kind = KindRequestMetadata
			msg.Request, err = decodeRequestMetadata(f.Bytes)
		default:
		}
		return err
	})
	return msg, err
}

func stringField(b []byte, num protowire.Number) (string, error) {
	var out string
	err := eachField(b, func(f field) error {
		if f.Num == num && f.isBytes() {
			out = f.str()
		}
		return nil
	})
	return out, err
}

func decodeTimestamp(b []byte) (time.Time, error) {
	var seconds, nanos int64
	err := eachField(b, func(f field) error {
		if !f.isVarint() {
			return nil
		}
		switch f.Num {
		case 1:
			seconds = int64(f.Uint)
		case 2:
			nanos = int64(int32(f.Uint))
		default:
		}
		return nil
	})
	if err != nil || (seconds == 0 && nanos == 0) {
		return time.Time{}, err
	}
	return time.Unix(seconds, nanos).UTC(), nil
}

func decodeModelUsed(b []byte) (ModelUsed, error) {
	var m ModelUsed
	err := eachField(b, func(f field) error {
		switch {
		case f.Num == 1 && f.isBytes():
			m.ID = f.str()
		case f.Num == 2 && f.isBytes():
			m.DisplayName = f.str()
		case f.Num == 3 && f.isVarint():
			m.Fallback = f.Uint != 0
		}
		return nil
	})
	return m, err
}

func decodeToolCall(b []byte) (ToolCall, error) {
	call := ToolCall{Input: json.RawMessage("{}")}
	err := eachField(b, func(f field) error {
		if !f.isBytes() {
			return nil
		}
		if f.Num == 1 {
			call.ID = f.str()
			return nil
		}
		name, ok := toolCallNames[f.Num]
		if !ok {
			return nil
		}
		call.Name = name
		input, err := toolCallInput(f.Num, f.Bytes)
		if err != nil {
			return err
		}
		if input != nil {
			raw, err := json.Marshal(input)
			if err != nil {
				return err
			}
			call.Input = raw
		}
		if f.Num == 12 {
			if mcpName, _ := input["name"].(string); mcpName != "" {
				call.Name = mcpName
			}
		}
		return nil
	})
	return call, err
}

// toolCallInput returns a JSON-ready view of the arguments for the common
// tools. It returns nil for tools whose arguments are not decoded.
func toolCallInput(num protowire.Number, b []byte) (map[string]any, error) {
	switch toolCallNames[num] {
	case "run_shell_command":
		command, err := stringField(b, 1)
		return map[string]any{"command": command}, err
	case "search_codebase":
		in := map[string]any{}
		err := eachField(b, func(f field) error {
			if !f.isBytes() {
				return nil
			}
			switch f.Num {
			case 1:
				in["query"] = f.str()
			case 2:
				filters, _ := in["path_filters"].([]string)
				in["path_filters"] = append(filters, f.str())
			case 3:
				in["codebase_path"] = f.str()
			default:
			}
			return nil
		})
		return in, err
	case "read_files":
		var paths []string
		err := eachField(b, func(f field) error {
			if f.Num != 1 || !f.isBytes() {
				return nil
			}
			name, err := stringField(f.Bytes, 1)
			if name != "" {
				paths = append(paths, name)
			}
			return err
		})
		return map[string]any{"files": paths}, err
	case "grep":
		in := map[string]any{}
		err := eachField(b, func(f field) error {
			if !f.isBytes() {
				return nil
			}
			switch f.Num {
			case 1:
				queries, _ := in["queries"].([]string)
				in["queries"] = append(queries, f.str())
			case 2:
				in["path"] = f.str()
			default:
			}
			return nil
		})
		return in, err
	case "file_glob_v2":
		in := map[string]any{}
		err := eachField(b, func(f field) error {
			if !f.isBytes() {
				return nil
			}
			switch f.Num {
			case 1:
				patterns, _ := in["patterns"].([]string)
				in["patterns"] = append(patterns, f.str())
			case 2:
				in["search_dir"] = f.str()
			default:
			}
			return nil
		})
		return in, err
	case "apply_file_diffs":
		var files []string
		err := eachField(b, func(f field) error {
			if f.Num < 2 || f.Num > 5 || !f.isBytes() {
				return nil
			}
			path, err := stringField(f.Bytes, 1)
			if path != "" {
				files = append(files, path)
			}
			return err
		})
		return map[string]any{"files": files}, err
	case "call_mcp_tool":
		in := map[string]any{}
		err := eachField(b, func(f field) error {
			if !f.isBytes() {
				return nil
			}
			switch f.Num {
			case 1:
				in["name"] = f.str()
			case 2:
				var args structpb.Struct
				if err := proto.Unmarshal(f.Bytes, &args); err != nil {
					return fmt.Errorf("decode mcp args: %w", err)
				}
				in["args"] = args.AsMap()
			case 3:
				in["server_id"] = f.str()
			default:
			}
			return nil
		})
		return in, err
	}
	return nil, nil
}

func decodeToolResult(b []byte) (ToolResult, error) {
	var result ToolResult
	err := eachField(b, func(f field) error {
		if !f.isBytes() {
			return nil
		}
		if f.Num == 1 {
			result.CallID = f.str()
			return nil
		}
		if f.Num == toolResultCancel {
			result.IsError = true
			result.Content = "cancelled"
			return nil
		}
		name, ok := toolResultNames[f.Num]
		if !ok {
			return nil
		}
		result.Name = name
		switch name {
		case "run_shell_command":
			return decodeShellResult(f.Bytes, &result)
		case "call_mcp_tool":
			return decodeMCPResult(f.Bytes, &result)
		}
		return nil
	})
	return result, err
}

func decodeShellResult(b []byte, result *ToolResult) error {
	var legacyOutput string
	var legacyExit int32
	finished := false
	err := eachField(b, func(f field) error {
		switch {
		case f.Num == 1 && f.isBytes():
			legacyOutput = f.str()
		case f.Num == 2 && f.isVarint():
			legacyExit = int32(f.Uint)
		case f.Num == 5 && f.isBytes():
			finished = true
			return eachField(f.Bytes, func(cf field) error {
				switch {
				case cf.Num == 1 && cf.isBytes():
					result.Content = cf.str()
				case cf.Num == 2 && cf.isVarint():
					result.IsError = int32(cf.Uint) != 0
				}
				return nil
			})
		case f.Num == 6 && f.isBytes():
			result.IsError = true
			result.Content = "permission denied"
		}
		return nil
	})
	if !finished && result.Content == "" {
		result.Content = legacyOutput
		result.IsError = result.IsError || legacyExit != 0
	}
	return err
}

func decodeMCPResult(b []byte, result *ToolResult) error {
	return eachField(b, func(f field) error {
		if !f.isBytes() {
			return nil
		}
		switch f.Num {
		case 1:
			return eachField(f.Bytes, func(rf field) error {
				if rf.Num != 1 || !rf.isBytes() {
					return nil
				}
				return eachField(rf.Bytes, func(variant field) error {
					if variant.Num != 1 || !variant.isBytes() {
						return nil
					}
					text, err := stringField(variant.Bytes, 1)
					if result.Content != "" && text != "" {
						result.Content += "\n"
					}
					result.Content += text
					return err
				})
			})
		case 2:
			result.IsError = true
			message, err := stringField(f.Bytes, 1)
			result.Content = message
			return err
		default:
		}
		return nil
	})
}

func decodeRequestMetadata(b []byte) (RequestMetadata, error) {
	var meta RequestMetadata
	err := eachField(b, func(f field) error {
		switch {
		case f.Num == 1 && f.isBytes():
			return decodeRequestTiming(f.Bytes, &meta)
		case f.Num == 2 && f.isBytes():
			usage, err := decodeRequestCharges(f.Bytes)
			meta.Usage = usage
			return err
		case f.Num == 3 && f.isVarint():
			meta.Incomplete = f.Uint != 0
		case f.Num == 4 && f.isVarint():
			meta.Outcome = Outcome(f.Uint)
		}
		return nil
	})
	return meta, err
}

func decodeRequestTiming(b []byte, meta *RequestMetadata) error {
	return eachField(b, func(f field) error {
		if !f.isBytes() {
			return nil
		}
		var err error
		switch f.Num {
		case 1:
			err = eachField(f.Bytes, func(span field) error {
				if !span.isBytes() {
					return nil
				}
				var err error
				switch span.Num {
				case 1:
					meta.StartedAt, err = decodeTimestamp(span.Bytes)
				case 2:
					meta.EndedAt, err = decodeTimestamp(span.Bytes)
				default:
				}
				return err
			})
		case 2:
			meta.FirstTokenAt, err = decodeTimestamp(f.Bytes)
		default:
		}
		return err
	})
}

// decodeRequestCharges sums RequestCharges.usage_by_category into one entry per
// model, in first-seen order.
func decodeRequestCharges(b []byte) ([]ModelUsage, error) {
	var usage []ModelUsage
	index := map[string]int{}
	add := func(model string, u ModelUsage) {
		i, ok := index[model]
		if !ok {
			i = len(usage)
			index[model] = i
			usage = append(usage, ModelUsage{Model: model})
		}
		usage[i].Input += u.Input
		usage[i].Output += u.Output
		usage[i].CacheRead += u.CacheRead
		usage[i].CacheWrite += u.CacheWrite
		usage[i].CostCents += u.CostCents
	}
	err := eachField(b, func(category field) error {
		if category.Num != 1 || !category.isBytes() {
			return nil
		}
		charged, err := mapEntryValue(category.Bytes)
		if err != nil || charged == nil {
			return err
		}
		return eachField(charged, func(source field) error {
			if source.Num < 1 || source.Num > 3 || !source.isBytes() {
				return nil
			}
			model, inference, err := mapEntry(source.Bytes)
			if err != nil || inference == nil {
				return err
			}
			u, err := decodeInferenceUsage(inference)
			if err != nil {
				return err
			}
			add(model, u)
			return nil
		})
	})
	return usage, err
}

func mapEntry(b []byte) (key string, value []byte, err error) {
	err = eachField(b, func(f field) error {
		if !f.isBytes() {
			return nil
		}
		switch f.Num {
		case 1:
			key = f.str()
		case 2:
			value = f.Bytes
		default:
		}
		return nil
	})
	return key, value, err
}

func mapEntryValue(b []byte) ([]byte, error) {
	_, value, err := mapEntry(b)
	return value, err
}

func decodeInferenceUsage(b []byte) (ModelUsage, error) {
	var u ModelUsage
	err := eachField(b, func(f field) error {
		switch {
		case f.Num == 1 && f.isBytes():
			return eachField(f.Bytes, func(count field) error {
				if !count.isVarint() {
					return nil
				}
				switch count.Num {
				case 1:
					u.Input = int64(count.Uint)
				case 2:
					u.Output = int64(count.Uint)
				case 3:
					u.CacheRead = int64(count.Uint)
				case 4:
					u.CacheWrite = int64(count.Uint)
				default:
				}
				return nil
			})
		case f.Num == 2 && f.isBytes():
			return eachField(f.Bytes, func(cost field) error {
				if cost.Num >= 1 && cost.Num <= 4 {
					u.CostCents += cost.float32()
				}
				return nil
			})
		case f.Num == 4 || f.Num == 7:
			u.CostCents += f.float32()
		}
		return nil
	})
	return u, err
}
