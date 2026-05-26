package anthropic

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	asdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/grafana/sigil-sdk/go/sigil"
)

// StreamSummary captures Anthropic stream events and an optional final message.
type StreamSummary struct {
	Events       []asdk.BetaRawMessageStreamEventUnion
	FinalMessage *asdk.BetaMessage
	FirstChunkAt time.Time
}

// FromStream maps Anthropic streaming output to sigil.Generation.
func FromStream(req asdk.BetaMessageNewParams, summary StreamSummary, opts ...Option) (sigil.Generation, error) {
	if summary.FinalMessage != nil {
		generation, err := FromRequestResponse(req, summary.FinalMessage, opts...)
		if err != nil {
			return sigil.Generation{}, err
		}
		return appendStreamEventsArtifact(generation, summary.Events, opts)
	}

	if len(summary.Events) == 0 {
		return sigil.Generation{}, errors.New("stream summary has no events and no final message")
	}

	options := applyOptions(opts)
	maxTokens, temperature, topP, toolChoice, thinkingEnabled, thinkingBudget := mapRequestControls(req)

	usage := sigil.TokenUsage{}
	stopReason := ""
	modelName := req.Model
	responseID := ""
	serverToolUsage := asdk.BetaServerToolUsage{}

	blocks := newStreamBlockAccumulator()

	for _, event := range summary.Events {
		switch event.Type {
		case "message_start":
			if event.Message.ID != "" {
				responseID = event.Message.ID
			}
			if event.Message.Model != "" {
				modelName = event.Message.Model
			}
		case "content_block_start":
			blocks.startBlock(int(event.Index), event.ContentBlock)
		case "content_block_delta":
			blocks.applyDelta(int(event.Index), event.Delta)
		case "message_delta":
			usage = mapDeltaUsage(event.Usage)
			serverToolUsage = event.Usage.ServerToolUse
			if event.Delta.StopReason != "" {
				stopReason = string(event.Delta.StopReason)
			}
		}
	}

	assistantParts, toolParts := blocks.build()
	metadata := mergeThinkingBudgetMetadata(options.metadata, thinkingBudget)
	metadata = mergeServerToolUsageMetadata(metadata, serverToolUsage)

	input := mapRequestMessages(req.Messages)
	output := make([]sigil.Message, 0, 2)
	if len(assistantParts) > 0 {
		output = append(output, sigil.Message{
			Role:  sigil.RoleAssistant,
			Parts: assistantParts,
		})
	}
	if len(toolParts) > 0 {
		output = append(output, sigil.Message{
			Role:  sigil.RoleTool,
			Parts: toolParts,
		})
	}

	artifacts := make([]sigil.Artifact, 0, 4)
	if options.includeRequestArtifact {
		artifact, err := sigil.NewJSONArtifact(sigil.ArtifactKindRequest, "anthropic.request", req)
		if err != nil {
			return sigil.Generation{}, err
		}
		artifacts = append(artifacts, artifact)
	}
	if options.includeToolsArtifact && len(req.Tools) > 0 {
		artifact, err := sigil.NewJSONArtifact(sigil.ArtifactKindTools, "anthropic.tools", req.Tools)
		if err != nil {
			return sigil.Generation{}, err
		}
		artifacts = append(artifacts, artifact)
	}
	if options.includeEventsArtifact {
		artifact, err := sigil.NewJSONArtifact(sigil.ArtifactKindProviderEvent, "anthropic.stream_events", summary.Events)
		if err != nil {
			return sigil.Generation{}, err
		}
		artifacts = append(artifacts, artifact)
	}

	generation := sigil.Generation{
		ConversationID:    options.conversationID,
		ConversationTitle: options.conversationTitle,
		AgentName:         options.agentName,
		AgentVersion:      options.agentVersion,
		Model:             sigil.ModelRef{Provider: options.providerName, Name: req.Model},
		ResponseID:        responseID,
		ResponseModel:     modelName,
		SystemPrompt:      mapSystemPrompt(req.System),
		Input:             input,
		Output:            output,
		Tools:             mapTools(req.Tools),
		MaxTokens:         maxTokens,
		Temperature:       temperature,
		TopP:              topP,
		ToolChoice:        toolChoice,
		ThinkingEnabled:   thinkingEnabled,
		Usage:             usage,
		StopReason:        stopReason,
		Tags:              cloneStringMap(options.tags),
		Metadata:          metadata,
		Artifacts:         artifacts,
	}

	if err := generation.Validate(); err != nil {
		return sigil.Generation{}, err
	}

	return generation, nil
}

func appendStreamEventsArtifact(generation sigil.Generation, events []asdk.BetaRawMessageStreamEventUnion, opts []Option) (sigil.Generation, error) {
	if len(events) == 0 {
		return generation, nil
	}

	options := applyOptions(opts)
	if !options.includeEventsArtifact {
		return generation, nil
	}

	artifact, err := sigil.NewJSONArtifact(sigil.ArtifactKindProviderEvent, "anthropic.stream_events", events)
	if err != nil {
		return sigil.Generation{}, err
	}

	generation.Artifacts = append(generation.Artifacts, artifact)
	return generation, nil
}

// streamBlock tracks a single content block being assembled from streaming events.
type streamBlock struct {
	index        int
	blockType    string
	providerType string

	// text/thinking accumulation
	text     strings.Builder
	thinking strings.Builder

	// tool_use fields from content_block_start
	toolID   string
	toolName string
	toolJSON strings.Builder // partial_json deltas

	// tool_result fields from content_block_start
	toolResultID  string
	isError       bool
	resultContent any
}

// streamBlockAccumulator collects content blocks in index order across
// content_block_start and content_block_delta events.
type streamBlockAccumulator struct {
	blocks   map[int]*streamBlock
	maxIndex int
}

func newStreamBlockAccumulator() *streamBlockAccumulator {
	return &streamBlockAccumulator{
		blocks:   make(map[int]*streamBlock),
		maxIndex: -1,
	}
}

func (a *streamBlockAccumulator) startBlock(index int, cb asdk.BetaRawContentBlockStartEventContentBlockUnion) {
	b := &streamBlock{
		index:        index,
		blockType:    cb.Type,
		providerType: cb.Type,
	}

	switch cb.Type {
	case "text":
		b.text.WriteString(cb.Text)
	case "thinking", "redacted_thinking":
		if cb.Type == "thinking" {
			b.thinking.WriteString(cb.Thinking)
		} else {
			b.thinking.WriteString(cb.Data)
		}
	case "tool_use", "server_tool_use", "mcp_tool_use":
		b.toolID = cb.ID
		b.toolName = cb.Name
		b.providerType = providerTypeForToolUse(cb.Type, cb.Name)
		if cb.Input != nil {
			if raw, err := json.Marshal(cb.Input); err == nil && string(raw) != "{}" {
				b.toolJSON.Write(raw)
			}
		}
	default:
		if isToolResultType(cb.Type) {
			b.toolResultID = cb.ToolUseID
			b.isError = cb.IsError
			b.resultContent = cb.Content
		}
	}

	a.blocks[index] = b
	if index > a.maxIndex {
		a.maxIndex = index
	}
}

func (a *streamBlockAccumulator) applyDelta(index int, delta asdk.BetaRawMessageStreamEventUnionDelta) {
	b, ok := a.blocks[index]
	if !ok {
		b = &streamBlock{index: index}
		a.blocks[index] = b
		if index > a.maxIndex {
			a.maxIndex = index
		}
	}

	switch {
	case delta.Text != "":
		if b.blockType == "" {
			b.blockType = "text"
			b.providerType = "text"
		}
		b.text.WriteString(delta.Text)
	case delta.Thinking != "":
		if b.blockType == "" {
			b.blockType = "thinking"
			b.providerType = "thinking"
		}
		b.thinking.WriteString(delta.Thinking)
	case delta.PartialJSON != "":
		if b.blockType == "" {
			b.blockType = "tool_use"
			b.providerType = "tool_use"
		}
		b.toolJSON.WriteString(delta.PartialJSON)
	}
}

// build produces the final assistant and tool parts in block-index order.
func (a *streamBlockAccumulator) build() (assistantParts, toolParts []sigil.Part) {
	for i := 0; i <= a.maxIndex; i++ {
		b, ok := a.blocks[i]
		if !ok {
			continue
		}
		part, isTool, ok := b.toPart()
		if !ok {
			continue
		}
		if isTool {
			toolParts = append(toolParts, part)
		} else {
			assistantParts = append(assistantParts, part)
		}
	}
	return
}

func (b *streamBlock) toPart() (sigil.Part, bool, bool) {
	switch b.blockType {
	case "text":
		text := b.text.String()
		if text == "" {
			return sigil.Part{}, false, false
		}
		return sigil.TextPart(text), false, true
	case "thinking", "redacted_thinking":
		content := b.thinking.String()
		if content == "" {
			return sigil.Part{}, false, false
		}
		part := sigil.ThinkingPart(content)
		part.Metadata.ProviderType = b.providerType
		return part, false, true
	case "tool_use", "server_tool_use", "mcp_tool_use":
		var inputJSON []byte
		if accumulated := b.toolJSON.String(); accumulated != "" {
			inputJSON = []byte(accumulated)
		}
		part := sigil.ToolCallPart(sigil.ToolCall{
			ID:        b.toolID,
			Name:      b.toolName,
			InputJSON: inputJSON,
		})
		part.Metadata.ProviderType = b.providerType
		return part, false, true
	default:
		if isToolResultType(b.blockType) {
			contentJSON, _ := marshalAny(b.resultContent)
			part := sigil.ToolResultPart(sigil.ToolResult{
				ToolCallID:  b.toolResultID,
				IsError:     b.isError,
				ContentJSON: contentJSON,
			})
			part.Metadata.ProviderType = b.providerType
			return part, true, true
		}
		return sigil.Part{}, false, false
	}
}

func isToolResultType(t string) bool {
	switch t {
	case "tool_result",
		"web_search_tool_result",
		"web_fetch_tool_result",
		"code_execution_tool_result",
		"bash_code_execution_tool_result",
		"text_editor_code_execution_tool_result",
		"tool_search_tool_result",
		toolSearchRegexToolResultType,
		toolSearchBM25ToolResultType,
		"mcp_tool_result":
		return true
	}
	return false
}
