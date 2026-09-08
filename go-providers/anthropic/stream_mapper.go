package anthropic

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	asdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/grafana/agento11y/go/agento11y"
)

// StreamSummary captures Anthropic stream events and an optional final message.
type StreamSummary struct {
	Events       []asdk.BetaRawMessageStreamEventUnion
	FinalMessage *asdk.BetaMessage
	FirstChunkAt time.Time
}

// FromStream maps Anthropic streaming output to agento11y.Generation.
func FromStream(req asdk.BetaMessageNewParams, summary StreamSummary, opts ...Option) (agento11y.Generation, error) {
	if summary.FinalMessage != nil {
		generation, err := FromRequestResponse(req, summary.FinalMessage, opts...)
		if err != nil {
			return agento11y.Generation{}, err
		}
		if usage, reported, serverToolUsage := mapStreamUsage(summary.Events); reported {
			generation.Usage = usage
			generation.Metadata = mergeServerToolUsageMetadata(generation.Metadata, serverToolUsage)
		}
		return appendStreamEventsArtifact(generation, summary.Events, opts)
	}

	if len(summary.Events) == 0 {
		return agento11y.Generation{}, errors.New("stream summary has no events and no final message")
	}
	if !hasMessageStreamEvent(summary.Events) {
		return agento11y.Generation{}, errors.New("stream summary has no message events and no final message")
	}

	options := applyOptions(opts)
	controls := mapRequestControls(req)

	usage, _, serverToolUsage := mapStreamUsage(summary.Events)
	stopReason := ""
	modelName := req.Model
	responseID := ""

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
			if event.Delta.StopReason != "" {
				stopReason = string(event.Delta.StopReason)
			}
		}
	}

	output := blocks.buildMessages(stopReason)
	metadata := mergeThinkingBudgetMetadata(options.metadata, controls.thinkingBudget)
	metadata = mergeServerToolUsageMetadata(metadata, serverToolUsage)

	input := mapRequestMessages(req.Messages)
	artifacts := make([]agento11y.Artifact, 0, 4)
	if options.includeRequestArtifact {
		artifact, err := agento11y.NewJSONArtifact(agento11y.ArtifactKindRequest, "anthropic.request", req)
		if err != nil {
			return agento11y.Generation{}, err
		}
		artifacts = append(artifacts, artifact)
	}
	if options.includeToolsArtifact && len(req.Tools) > 0 {
		artifact, err := agento11y.NewJSONArtifact(agento11y.ArtifactKindTools, "anthropic.tools", req.Tools)
		if err != nil {
			return agento11y.Generation{}, err
		}
		artifacts = append(artifacts, artifact)
	}
	if options.includeEventsArtifact {
		artifact, err := agento11y.NewJSONArtifact(agento11y.ArtifactKindProviderEvent, "anthropic.stream_events", summary.Events)
		if err != nil {
			return agento11y.Generation{}, err
		}
		artifacts = append(artifacts, artifact)
	}

	generation := agento11y.Generation{
		ConversationID:    options.conversationID,
		ConversationTitle: options.conversationTitle,
		AgentName:         options.agentName,
		AgentVersion:      options.agentVersion,
		Model:             agento11y.ModelRef{Provider: options.providerName, Name: req.Model},
		ResponseID:        responseID,
		ResponseModel:     modelName,
		SystemPrompt:      mapSystemPrompt(req.System),
		Input:             input,
		Output:            output,
		Tools:             mapTools(req.Tools),
		MaxTokens:         controls.maxTokens,
		Temperature:       controls.temperature,
		TopP:              controls.topP,
		TopK:              controls.topK,
		OutputType:        controls.outputType,
		ToolChoice:        controls.toolChoice,
		ThinkingEnabled:   controls.thinkingEnabled,
		Usage:             usage,
		StopReason:        stopReason,
		Tags:              cloneStringMap(options.tags),
		Metadata:          metadata,
		Artifacts:         artifacts,
	}

	if err := generation.Validate(); err != nil {
		return agento11y.Generation{}, err
	}

	return generation, nil
}

func hasMessageStreamEvent(events []asdk.BetaRawMessageStreamEventUnion) bool {
	for _, event := range events {
		switch event.Type {
		case "message_start", "message_delta", "message_stop",
			"content_block_start", "content_block_delta", "content_block_stop":
			return true
		}
	}
	return false
}

func mapStreamUsage(events []asdk.BetaRawMessageStreamEventUnion) (agento11y.TokenUsage, bool, asdk.BetaServerToolUsage) {
	usage := agento11y.TokenUsage{InputSemantics: agento11y.TokenInputSemanticsInclusive}
	var rawInputTokens int64
	inputReported := false
	outputReported := false
	serverToolUsage := asdk.BetaServerToolUsage{}

	for _, event := range events {
		switch event.Type {
		case "message_start":
			startUsage := event.Message.Usage
			if tokenCountReported(startUsage.JSON.InputTokens.Valid(), startUsage.InputTokens) {
				rawInputTokens = startUsage.InputTokens
				inputReported = true
			}
			if tokenCountReported(startUsage.JSON.CacheReadInputTokens.Valid(), startUsage.CacheReadInputTokens) {
				usage.CacheReadInputTokens = startUsage.CacheReadInputTokens
				inputReported = true
			}
			if tokenCountReported(startUsage.JSON.CacheCreationInputTokens.Valid(), startUsage.CacheCreationInputTokens) {
				usage.CacheWriteInputTokens = startUsage.CacheCreationInputTokens
				inputReported = true
			}
			if tokenCountReported(startUsage.JSON.OutputTokens.Valid(), startUsage.OutputTokens) {
				usage.OutputTokens = startUsage.OutputTokens
				outputReported = true
			}
			if serverToolUsageReported(startUsage.ServerToolUse) {
				serverToolUsage = startUsage.ServerToolUse
			}
		case "message_delta":
			deltaUsage := event.Usage
			if tokenCountReported(deltaUsage.JSON.InputTokens.Valid(), deltaUsage.InputTokens) {
				rawInputTokens = deltaUsage.InputTokens
				inputReported = true
			}
			if tokenCountReported(deltaUsage.JSON.CacheReadInputTokens.Valid(), deltaUsage.CacheReadInputTokens) {
				usage.CacheReadInputTokens = deltaUsage.CacheReadInputTokens
				inputReported = true
			}
			if tokenCountReported(deltaUsage.JSON.CacheCreationInputTokens.Valid(), deltaUsage.CacheCreationInputTokens) {
				usage.CacheWriteInputTokens = deltaUsage.CacheCreationInputTokens
				inputReported = true
			}
			if tokenCountReported(deltaUsage.JSON.OutputTokens.Valid(), deltaUsage.OutputTokens) {
				usage.OutputTokens = deltaUsage.OutputTokens
				outputReported = true
			}
			if serverToolUsageReported(deltaUsage.ServerToolUse) {
				serverToolUsage = deltaUsage.ServerToolUse
			}
		}
	}

	usage.InputTokens = rawInputTokens + usage.CacheReadInputTokens + usage.CacheWriteInputTokens
	usage.InputTokensReported = inputReported
	usage.OutputTokensReported = outputReported
	if inputReported && outputReported {
		usage.TotalTokens = usage.InputTokens + usage.OutputTokens
	}
	return usage, inputReported || outputReported, serverToolUsage
}

func tokenCountReported(fieldPresent bool, value int64) bool {
	return fieldPresent || value != 0
}

func serverToolUsageReported(usage asdk.BetaServerToolUsage) bool {
	return usage.WebSearchRequests != 0 || usage.WebFetchRequests != 0 ||
		usage.JSON.WebSearchRequests.Valid() || usage.JSON.WebFetchRequests.Valid()
}

func appendStreamEventsArtifact(generation agento11y.Generation, events []asdk.BetaRawMessageStreamEventUnion, opts []Option) (agento11y.Generation, error) {
	if len(events) == 0 {
		return generation, nil
	}

	options := applyOptions(opts)
	if !options.includeEventsArtifact {
		return generation, nil
	}

	artifact, err := agento11y.NewJSONArtifact(agento11y.ArtifactKindProviderEvent, "anthropic.stream_events", events)
	if err != nil {
		return agento11y.Generation{}, err
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

func (a *streamBlockAccumulator) buildMessages(finishReason string) []agento11y.Message {
	parts := make([]agento11y.Part, 0, len(a.blocks))
	for i := 0; i <= a.maxIndex; i++ {
		block, ok := a.blocks[i]
		if !ok {
			continue
		}
		part, _, ok := block.toPart()
		if ok {
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 && finishReason == "" {
		return nil
	}
	return []agento11y.Message{{
		Role:         agento11y.RoleAssistant,
		Parts:        parts,
		FinishReason: finishReason,
	}}
}

func (b *streamBlock) toPart() (agento11y.Part, bool, bool) {
	switch b.blockType {
	case "text":
		text := b.text.String()
		if text == "" {
			return agento11y.Part{}, false, false
		}
		return agento11y.TextPart(text), false, true
	case "thinking", "redacted_thinking":
		content := b.thinking.String()
		if content == "" {
			return agento11y.Part{}, false, false
		}
		part := agento11y.ThinkingPart(content)
		part.Metadata.ProviderType = b.providerType
		return part, false, true
	case "tool_use", "server_tool_use", "mcp_tool_use":
		var inputJSON []byte
		if accumulated := b.toolJSON.String(); accumulated != "" {
			inputJSON = []byte(accumulated)
		}
		part := agento11y.ToolCallPart(agento11y.ToolCall{
			ID:        b.toolID,
			Name:      b.toolName,
			InputJSON: inputJSON,
		})
		part.Metadata.ProviderType = b.providerType
		return part, false, true
	default:
		if isToolResultType(b.blockType) {
			contentJSON, _ := marshalAny(b.resultContent)
			part := agento11y.ToolResultPart(agento11y.ToolResult{
				ToolCallID:  b.toolResultID,
				IsError:     b.isError,
				ContentJSON: contentJSON,
			})
			part.Metadata.ProviderType = b.providerType
			return part, true, true
		}
		return agento11y.Part{}, false, false
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
