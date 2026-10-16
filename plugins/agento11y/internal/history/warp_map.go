package history

import (
	"sort"
	"strings"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/warp/warpdb"
	"github.com/grafana/agento11y/plugins/agento11y/internal/mapperutil"
)

// warpRequest is one Warp agent request: the client round trip that sends a
// user query or tool results and receives the model's output. Child tasks share
// request IDs with their parent, so a request gathers messages across tasks.
type warpRequest struct {
	requestID string
	start     time.Time
	messages  []warpdb.Message
}

// warpRequests groups a conversation's messages by request ID, ordered by the
// request's earliest message timestamp.
func warpRequests(tasks []warpdb.Task) []warpRequest {
	index := map[string]int{}
	seenMessage := map[string]bool{}
	var requests []warpRequest
	for _, task := range warpTasksParentFirst(tasks) {
		for _, msg := range task.Messages {
			if msg.RequestID == "" {
				continue
			}
			if msg.ID != "" {
				if seenMessage[msg.ID] {
					continue
				}
				seenMessage[msg.ID] = true
			}
			i, ok := index[msg.RequestID]
			if !ok {
				i = len(requests)
				index[msg.RequestID] = i
				requests = append(requests, warpRequest{requestID: msg.RequestID})
			}
			req := &requests[i]
			req.messages = append(req.messages, msg)
			if !msg.Timestamp.IsZero() && (req.start.IsZero() || msg.Timestamp.Before(req.start)) {
				req.start = msg.Timestamp
			}
		}
	}
	// A request without timestamps keeps its position after the one before it.
	var previous time.Time
	for i := range requests {
		if requests[i].start.IsZero() {
			requests[i].start = previous
		}
		previous = requests[i].start
	}
	sort.SliceStable(requests, func(i, j int) bool { return requests[i].start.Before(requests[j].start) })
	return requests
}

// warpTasksParentFirst orders tasks so a parent precedes its children, keeping
// row order among siblings.
func warpTasksParentFirst(tasks []warpdb.Task) []warpdb.Task {
	parents := make(map[string]string, len(tasks))
	for _, task := range tasks {
		parents[task.ID] = task.ParentTaskID
	}
	depths := make(map[string]int, len(tasks))
	for _, task := range tasks {
		seen := map[string]bool{task.ID: true}
		for parent := task.ParentTaskID; parent != "" && !seen[parent]; parent = parents[parent] {
			if _, known := parents[parent]; !known {
				break
			}
			seen[parent] = true
			depths[task.ID]++
		}
	}
	ordered := append([]warpdb.Task(nil), tasks...)
	sort.SliceStable(ordered, func(i, j int) bool { return depths[ordered[i].ID] < depths[ordered[j].ID] })
	return ordered
}

// warpConversation is the conversation-wide context every request maps with.
type warpConversation struct {
	// title is the root task's description, which Warp shows as the title.
	title string
	// modelIDs maps a lowercased model display name to its model ID. Billing
	// records name models by display name, ModelUsed records by ID.
	modelIDs map[string]string
	// callNames maps a tool call ID to the call's tool name. A result usually
	// arrives in the request after its call, so the lookup spans requests.
	callNames map[string]string
}

func newWarpConversation(tasks []warpdb.Task) warpConversation {
	conv := warpConversation{modelIDs: map[string]string{}, callNames: map[string]string{}}
	for _, task := range warpTasksParentFirst(tasks) {
		if conv.title == "" {
			conv.title = strings.TrimSpace(task.Description)
		}
		for _, msg := range task.Messages {
			switch {
			case msg.Kind == warpdb.KindModelUsed && msg.Model.ID != "" && msg.Model.DisplayName != "":
				conv.modelIDs[strings.ToLower(msg.Model.DisplayName)] = msg.Model.ID
			case msg.Kind == warpdb.KindToolCall && msg.ToolCall.ID != "" && msg.ToolCall.Name != "":
				conv.callNames[msg.ToolCall.ID] = msg.ToolCall.Name
			}
		}
	}
	return conv
}

// billedModel returns the model ID for a billing record's model name, or the
// name itself when no ModelUsed record in the conversation matches it.
func (c warpConversation) billedModel(name string) string {
	if id := c.modelIDs[strings.ToLower(strings.TrimSpace(name))]; id != "" {
		return id
	}
	return name
}

func warpGeneration(sess SessionPreview, req warpRequest, source SourceRef, conv warpConversation, parentGenerationID string) HistoricalGeneration {
	var (
		input, output []agento11y.Message
		toolNames     []string
		models        []warpdb.ModelUsed
		meta          *warpdb.RequestMetadata
		first, last   time.Time
	)
	responding := false
	for _, msg := range req.messages {
		if !msg.Timestamp.IsZero() {
			if first.IsZero() || msg.Timestamp.Before(first) {
				first = msg.Timestamp
			}
			if msg.Timestamp.After(last) {
				last = msg.Timestamp
			}
		}
		switch msg.Kind {
		case warpdb.KindUserQuery:
			if strings.TrimSpace(msg.Text) != "" {
				input = append(input, warpText(agento11y.RoleUser, msg.Text))
			}
		case warpdb.KindToolResult:
			result := warpToolResult(msg.ToolResult, conv.callNames)
			if responding {
				output = append(output, result)
			} else {
				input = append(input, result)
			}
		case warpdb.KindModelUsed:
			responding = true
			if msg.Model.ID != "" {
				models = append(models, msg.Model)
			}
		case warpdb.KindReasoning:
			responding = true
			if strings.TrimSpace(msg.Text) != "" {
				output = append(output, agento11y.Message{
					Role:  agento11y.RoleAssistant,
					Parts: []agento11y.Part{{Kind: agento11y.PartKindThinking, Thinking: msg.Text}},
				})
			}
		case warpdb.KindAgentOutput:
			responding = true
			if strings.TrimSpace(msg.Text) != "" {
				output = append(output, warpText(agento11y.RoleAssistant, msg.Text))
			}
		case warpdb.KindToolCall:
			responding = true
			call := msg.ToolCall
			if call.Name == "" {
				continue
			}
			toolNames = append(toolNames, call.Name)
			output = append(output, agento11y.Message{
				Role: agento11y.RoleAssistant,
				Parts: []agento11y.Part{{
					Kind:     agento11y.PartKindToolCall,
					ToolCall: &agento11y.ToolCall{ID: call.ID, Name: call.Name, InputJSON: call.Input},
				}},
			})
		case warpdb.KindRequestMetadata:
			m := msg.Request
			meta = &m
		default:
		}
	}

	model, responseModel := warpModels(models, meta, conv)
	usage, cost, hasUsage := warpUsage(meta)
	gen := agento11y.Generation{
		ID:             source.GenerationID(),
		ConversationID: sess.SessionID,
		AgentName:      string(AgentWarp),
		Mode:           agento11y.GenerationModeStream,
		OperationName:  "streamText",
		Model:          agento11y.ModelRef{Provider: mapperutil.InferProvider(model), Name: model},
		ResponseModel:  responseModel,
		Input:          input,
		Output:         output,
		Tools:          mapperutil.SortedToolDefinitions(toolNames),
		Usage:          usage,
		StartedAt:      first,
		CompletedAt:    last,
		Tags:           warpTags(sess.Workspace),
		Metadata:       warpMetadata(cost, hasUsage, models),
	}
	if conv.title != "" {
		gen.ConversationTitle = conv.title
	}
	if meta != nil {
		if !meta.StartedAt.IsZero() {
			gen.StartedAt = meta.StartedAt
		}
		if !meta.EndedAt.IsZero() {
			gen.CompletedAt = meta.EndedAt
		}
		switch meta.Outcome {
		case warpdb.OutcomeCanceled:
			gen.StopReason = "cancelled"
		case warpdb.OutcomeErrored:
			gen.CallError = "errored"
		default:
		}
	}
	if parentGenerationID != "" {
		gen.ParentGenerationIDs = []string{parentGenerationID}
	}
	quality := QualityReport{
		ApproxStartedAt:   gen.StartedAt.IsZero(),
		ApproxCompletedAt: gen.CompletedAt.IsZero(),
		ApproxUsage:       !hasUsage,
		MissingModel:      strings.TrimSpace(gen.Model.Name) == "",
	}
	return HistoricalGeneration{Source: source, Gen: gen, Quality: quality}
}

func warpText(role agento11y.Role, text string) agento11y.Message {
	return agento11y.Message{Role: role, Parts: []agento11y.Part{{Kind: agento11y.PartKindText, Text: text}}}
}

func warpToolResult(result warpdb.ToolResult, callNames map[string]string) agento11y.Message {
	name := callNames[result.CallID]
	if name == "" {
		name = result.Name
	}
	return agento11y.Message{
		Role: agento11y.RoleTool,
		Parts: []agento11y.Part{{
			Kind: agento11y.PartKindToolResult,
			ToolResult: &agento11y.ToolResult{
				ToolCallID: result.CallID,
				Name:       name,
				Content:    result.Content,
				IsError:    result.IsError,
			},
		}},
	}
}

// warpModels returns the request model and the model that produced the final
// response. Billing names the model when no ModelUsed message was recorded.
func warpModels(models []warpdb.ModelUsed, meta *warpdb.RequestMetadata, conv warpConversation) (model, responseModel string) {
	if len(models) > 0 {
		return models[0].ID, models[len(models)-1].ID
	}
	if meta != nil {
		for _, u := range meta.Usage {
			if u.Model != "" {
				billed := conv.billedModel(u.Model)
				return billed, billed
			}
		}
	}
	return "", ""
}

func warpUsage(meta *warpdb.RequestMetadata) (agento11y.TokenUsage, float64, bool) {
	if meta == nil || len(meta.Usage) == 0 {
		return agento11y.TokenUsage{}, 0, false
	}
	var input, output, cacheRead, cacheWrite int64
	cents := meta.PlatformCents
	for _, u := range meta.Usage {
		input += u.Input
		output += u.Output
		cacheRead += u.CacheRead
		cacheWrite += u.CacheWrite
		cents += u.CostCents
	}
	// Warp reports input exclusive of both cache buckets; the SDK's inclusive
	// contract covers them.
	inclusiveInput := input + cacheRead + cacheWrite
	return agento11y.TokenUsage{
		InputTokens:           inclusiveInput,
		OutputTokens:          output,
		TotalTokens:           inclusiveInput + output,
		CacheReadInputTokens:  cacheRead,
		CacheWriteInputTokens: cacheWrite,
		InputSemantics:        agento11y.TokenInputSemanticsInclusive,
	}, cents / 100, true
}

func warpTags(workspace string) map[string]string {
	if workspace == "" {
		return nil
	}
	return map[string]string{"cwd": workspace}
}

func warpMetadata(costUSD float64, hasUsage bool, models []warpdb.ModelUsed) map[string]any {
	metadata := map[string]any{}
	if hasUsage {
		metadata["cost_usd"] = costUSD
	}
	for _, m := range models {
		if m.Fallback {
			metadata["warp.model_fallback"] = true
			break
		}
	}
	if len(metadata) == 0 {
		return nil
	}
	return metadata
}
