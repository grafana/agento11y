package googleadk

import (
	"context"
	"fmt"
	"maps"
	"math"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/grafana/sigil-sdk/go/sigil"
)

const (
	frameworkName             = "google-adk"
	frameworkSource           = "handler"
	frameworkLanguage         = "go"
	maxMetadataNormalizeDepth = 5
)

const (
	metadataRunID         = "sigil.framework.run_id"
	metadataThreadID      = "sigil.framework.thread_id"
	metadataParentRunID   = "sigil.framework.parent_run_id"
	metadataComponentName = "sigil.framework.component_name"
	metadataRunType       = "sigil.framework.run_type"
	metadataTags          = "sigil.framework.tags"
	metadataRetryAttempt  = "sigil.framework.retry_attempt"
	metadataEventID       = "sigil.framework.event_id"
)

// ProviderResolver resolves provider names for framework runs.
type ProviderResolver func(modelName string, event RunStartEvent) string

// Options configures the Google ADK adapter.
type Options struct {
	AgentName        string
	AgentVersion     string
	Provider         string
	ProviderResolver ProviderResolver
	// CaptureInputs controls input capture when non-nil.
	// Nil defaults to true.
	CaptureInputs *bool
	// CaptureOutputs controls output capture when non-nil.
	// Nil defaults to true.
	CaptureOutputs *bool
	ExtraTags      map[string]string
	ExtraMetadata  map[string]any
}

// RunStartEvent is the adapter input for LLM run start callbacks.
type RunStartEvent struct {
	RunID          string
	ParentRunID    string
	ConversationID string
	SessionID      string
	GroupID        string
	ThreadID       string
	EventID        string
	ComponentName  string
	RunType        string
	RetryAttempt   *int
	ModelName      string
	Provider       string
	Stream         bool
	Prompts        []string
	InputMessages  []sigil.Message
	Tags           []string
	Metadata       map[string]any
}

// RunEndEvent is the adapter input for LLM run completion callbacks.
type RunEndEvent struct {
	RunID          string
	OutputMessages []sigil.Message
	ResponseModel  string
	StopReason     string
	Usage          sigil.TokenUsage
}

// ToolStartEvent is the adapter input for tool start callbacks.
type ToolStartEvent struct {
	RunID           string
	ConversationID  string
	SessionID       string
	GroupID         string
	ThreadID        string
	ToolCallID      string
	ToolName        string
	ToolType        string
	ToolDescription string
	Arguments       any
	// ModelName is the model that requested the tool call (e.g. "gpt-5").
	ModelName string
	// Provider is the provider that served the model (e.g. "openai").
	Provider string
}

// ToolEndEvent is the adapter input for tool completion callbacks.
type ToolEndEvent struct {
	RunID       string
	Result      any
	CompletedAt time.Time
}

type runState struct {
	recorder       *sigil.GenerationRecorder
	input          []sigil.Message
	captureOutputs bool
	firstTokenSet  bool
	outputChunks   []string
}

type toolRunState struct {
	recorder       *sigil.ToolExecutionRecorder
	arguments      any
	captureOutputs bool
}

// Callbacks exposes adapter lifecycle handlers as function fields so callers can
// wire them once into framework runner configuration.
type Callbacks struct {
	OnRunStart  func(context.Context, RunStartEvent) error
	OnRunToken  func(string, string)
	OnRunEnd    func(string, RunEndEvent) error
	OnRunError  func(string, error) error
	OnToolStart func(context.Context, ToolStartEvent) error
	OnToolEnd   func(string, ToolEndEvent) error
	OnToolError func(string, error) error
}

// Adapter bridges Google ADK lifecycle events into Sigil recorder lifecycles.
type Adapter struct {
	client         *sigil.Client
	opts           Options
	captureInputs  bool
	captureOutputs bool
	runs           map[string]*runState
	toolRuns       map[string]*toolRunState
	startRun       func(context.Context, sigil.GenerationStart, bool) *sigil.GenerationRecorder
	startTool      func(context.Context, sigil.ToolExecutionStart) *sigil.ToolExecutionRecorder
	runsMu         sync.Mutex
	toolRunsMu     sync.Mutex
}

// NewSigilAdapter creates a Google ADK adapter for a Sigil client.
func NewSigilAdapter(client *sigil.Client, opts Options) *Adapter {
	captureInputs := true
	if opts.CaptureInputs != nil {
		captureInputs = *opts.CaptureInputs
	}
	captureOutputs := true
	if opts.CaptureOutputs != nil {
		captureOutputs = *opts.CaptureOutputs
	}
	if opts.ExtraTags == nil {
		opts.ExtraTags = map[string]string{}
	}
	if opts.ExtraMetadata == nil {
		opts.ExtraMetadata = map[string]any{}
	}
	if opts.ProviderResolver == nil {
		opts.ProviderResolver = func(_ string, _ RunStartEvent) string { return "" }
	}
	return &Adapter{
		client:         client,
		opts:           opts,
		captureInputs:  captureInputs,
		captureOutputs: captureOutputs,
		runs:           map[string]*runState{},
		toolRuns:       map[string]*toolRunState{},
		startRun: func(ctx context.Context, start sigil.GenerationStart, stream bool) *sigil.GenerationRecorder {
			if stream {
				_, rec := client.StartStreamingGeneration(ctx, start)
				return rec
			}
			_, rec := client.StartGeneration(ctx, start)
			return rec
		},
		startTool: func(ctx context.Context, start sigil.ToolExecutionStart) *sigil.ToolExecutionRecorder {
			_, rec := client.StartToolExecution(ctx, start)
			return rec
		},
	}
}

// Callbacks returns function-based lifecycle hooks backed by this adapter.
func (a *Adapter) Callbacks() Callbacks {
	return Callbacks{
		OnRunStart:  a.OnRunStart,
		OnRunToken:  a.OnRunToken,
		OnRunEnd:    a.OnRunEnd,
		OnRunError:  a.OnRunError,
		OnToolStart: a.OnToolStart,
		OnToolEnd:   a.OnToolEnd,
		OnToolError: a.OnToolError,
	}
}

// NewCallbacks creates a Sigil adapter and returns function-based lifecycle hooks.
func NewCallbacks(client *sigil.Client, opts Options) Callbacks {
	return NewSigilAdapter(client, opts).Callbacks()
}

// OnRunStart starts a Sigil generation lifecycle for an ADK run.
func (a *Adapter) OnRunStart(ctx context.Context, event RunStartEvent) error {
	runID := strings.TrimSpace(event.RunID)
	if runID == "" {
		return nil
	}

	a.runsMu.Lock()
	defer a.runsMu.Unlock()
	if _, exists := a.runs[runID]; exists {
		return nil
	}

	conversationID, threadID := resolveConversationID(event)
	provider := resolveProvider(a.opts.Provider, event.Provider, event.ModelName, a.opts.ProviderResolver, event)
	runType := strings.TrimSpace(event.RunType)
	if runType == "" {
		runType = "chat"
	}

	metadata := buildFrameworkMetadata(frameworkMetadataInput{
		baseMetadata:  a.opts.ExtraMetadata,
		eventMetadata: event.Metadata,
		runID:         runID,
		threadID:      threadID,
		parentRunID:   strings.TrimSpace(event.ParentRunID),
		componentName: strings.TrimSpace(event.ComponentName),
		runType:       runType,
		tags:          normalizeTags(event.Tags),
		retryAttempt:  event.RetryAttempt,
		eventID:       strings.TrimSpace(event.EventID),
	})

	tags := make(map[string]string, len(a.opts.ExtraTags)+3)
	maps.Copy(tags, a.opts.ExtraTags)
	tags["sigil.framework.name"] = frameworkName
	tags["sigil.framework.source"] = frameworkSource
	tags["sigil.framework.language"] = frameworkLanguage

	start := sigil.GenerationStart{
		ConversationID: conversationID,
		AgentName:      strings.TrimSpace(a.opts.AgentName),
		AgentVersion:   strings.TrimSpace(a.opts.AgentVersion),
		Mode:           modeFromEvent(event.Stream),
		Model: sigil.ModelRef{
			Provider: provider,
			Name:     normalizeModelName(event.ModelName),
		},
		Tags:     tags,
		Metadata: metadata,
	}

	input := []sigil.Message{}
	if a.captureInputs {
		if len(event.InputMessages) > 0 {
			input = append(input, event.InputMessages...)
		} else {
			for _, prompt := range event.Prompts {
				trimmed := strings.TrimSpace(prompt)
				if trimmed == "" {
					continue
				}
				input = append(input, sigil.UserTextMessage(trimmed))
			}
		}
	}

	rec := a.startRun(ctx, start, event.Stream)
	a.runs[runID] = &runState{
		recorder:       rec,
		input:          input,
		captureOutputs: a.captureOutputs,
		outputChunks:   []string{},
	}
	return nil
}

// OnRunToken records streaming output chunks and first token timestamp.
func (a *Adapter) OnRunToken(runID string, token string) {
	trimmedRunID := strings.TrimSpace(runID)
	if trimmedRunID == "" {
		return
	}

	a.runsMu.Lock()
	state := a.runs[trimmedRunID]
	if state == nil {
		a.runsMu.Unlock()
		return
	}
	trimmedToken := strings.TrimSpace(token)
	if trimmedToken == "" {
		a.runsMu.Unlock()
		return
	}
	if state.captureOutputs {
		state.outputChunks = append(state.outputChunks, token)
	}
	if !state.firstTokenSet {
		state.firstTokenSet = true
		state.recorder.SetFirstTokenAt(time.Now().UTC())
	}
	a.runsMu.Unlock()
}

// OnRunEnd finalizes a successful run lifecycle.
func (a *Adapter) OnRunEnd(runID string, event RunEndEvent) error {
	trimmedRunID := strings.TrimSpace(runID)
	if trimmedRunID == "" {
		return nil
	}

	a.runsMu.Lock()
	state := a.runs[trimmedRunID]
	if state == nil {
		a.runsMu.Unlock()
		return nil
	}
	delete(a.runs, trimmedRunID)
	a.runsMu.Unlock()

	output := []sigil.Message{}
	if state.captureOutputs {
		output = event.OutputMessages
		if len(output) == 0 && len(state.outputChunks) > 0 {
			output = []sigil.Message{sigil.AssistantTextMessage(strings.Join(state.outputChunks, ""))}
		}
	}

	state.recorder.SetResult(sigil.Generation{
		Input:         state.input,
		Output:        output,
		Usage:         event.Usage.Normalize(),
		ResponseModel: strings.TrimSpace(event.ResponseModel),
		StopReason:    strings.TrimSpace(event.StopReason),
	}, nil)
	state.recorder.End()
	return state.recorder.Err()
}

// OnRunError finalizes a failed run lifecycle.
func (a *Adapter) OnRunError(runID string, runErr error) error {
	trimmedRunID := strings.TrimSpace(runID)
	if trimmedRunID == "" {
		return nil
	}
	if runErr == nil {
		runErr = fmt.Errorf("framework callback error")
	}

	a.runsMu.Lock()
	state := a.runs[trimmedRunID]
	if state == nil {
		a.runsMu.Unlock()
		return nil
	}
	delete(a.runs, trimmedRunID)
	a.runsMu.Unlock()

	if state.captureOutputs && len(state.outputChunks) > 0 {
		state.recorder.SetResult(sigil.Generation{
			Input:  state.input,
			Output: []sigil.Message{sigil.AssistantTextMessage(strings.Join(state.outputChunks, ""))},
		}, nil)
	}
	state.recorder.SetCallError(runErr)
	state.recorder.End()
	return state.recorder.Err()
}

// OnToolStart starts a tool execution lifecycle.
func (a *Adapter) OnToolStart(ctx context.Context, event ToolStartEvent) error {
	runID := strings.TrimSpace(event.RunID)
	if runID == "" {
		return nil
	}

	a.toolRunsMu.Lock()
	defer a.toolRunsMu.Unlock()
	if _, exists := a.toolRuns[runID]; exists {
		return nil
	}

	conversationID, _ := resolveConversationID(RunStartEvent{
		RunID:          runID,
		ConversationID: event.ConversationID,
		SessionID:      event.SessionID,
		GroupID:        event.GroupID,
		ThreadID:       event.ThreadID,
	})

	provider := resolveProvider(a.opts.Provider, event.Provider, event.ModelName, a.opts.ProviderResolver, RunStartEvent{
		ModelName: event.ModelName,
		Provider:  event.Provider,
	})

	rec := a.startTool(ctx, sigil.ToolExecutionStart{
		ToolName:        strings.TrimSpace(event.ToolName),
		ToolCallID:      strings.TrimSpace(event.ToolCallID),
		ToolType:        strings.TrimSpace(event.ToolType),
		ToolDescription: strings.TrimSpace(event.ToolDescription),
		ConversationID:  conversationID,
		AgentName:       strings.TrimSpace(a.opts.AgentName),
		AgentVersion:    strings.TrimSpace(a.opts.AgentVersion),
		RequestModel:    normalizeModelName(event.ModelName),
		RequestProvider: provider,
		IncludeContent:  a.captureInputs || a.captureOutputs,
	})

	a.toolRuns[runID] = &toolRunState{
		recorder:       rec,
		arguments:      nil,
		captureOutputs: a.captureOutputs,
	}
	if a.captureInputs {
		a.toolRuns[runID].arguments = event.Arguments
	}
	return nil
}

// OnToolEnd finalizes a successful tool lifecycle.
func (a *Adapter) OnToolEnd(runID string, event ToolEndEvent) error {
	trimmedRunID := strings.TrimSpace(runID)
	if trimmedRunID == "" {
		return nil
	}

	a.toolRunsMu.Lock()
	state := a.toolRuns[trimmedRunID]
	if state == nil {
		a.toolRunsMu.Unlock()
		return nil
	}
	delete(a.toolRuns, trimmedRunID)
	a.toolRunsMu.Unlock()

	end := sigil.ToolExecutionEnd{CompletedAt: event.CompletedAt}
	if state.arguments != nil {
		end.Arguments = state.arguments
	}
	if state.captureOutputs {
		end.Result = event.Result
	}
	state.recorder.SetResult(end)
	state.recorder.End()
	return state.recorder.Err()
}

// OnToolError finalizes a failed tool lifecycle.
func (a *Adapter) OnToolError(runID string, toolErr error) error {
	trimmedRunID := strings.TrimSpace(runID)
	if trimmedRunID == "" {
		return nil
	}
	if toolErr == nil {
		toolErr = fmt.Errorf("tool callback error")
	}

	a.toolRunsMu.Lock()
	state := a.toolRuns[trimmedRunID]
	if state == nil {
		a.toolRunsMu.Unlock()
		return nil
	}
	delete(a.toolRuns, trimmedRunID)
	a.toolRunsMu.Unlock()

	state.recorder.SetExecError(toolErr)
	state.recorder.End()
	return state.recorder.Err()
}

func modeFromEvent(stream bool) sigil.GenerationMode {
	if stream {
		return sigil.GenerationModeStream
	}
	return sigil.GenerationModeSync
}

func normalizeModelName(modelName string) string {
	trimmed := strings.TrimSpace(modelName)
	if trimmed == "" {
		return "unknown"
	}
	return trimmed
}

func resolveConversationID(event RunStartEvent) (conversationID string, threadID string) {
	if trimmed := strings.TrimSpace(event.ConversationID); trimmed != "" {
		return trimmed, strings.TrimSpace(event.ThreadID)
	}
	if trimmed := strings.TrimSpace(event.SessionID); trimmed != "" {
		return trimmed, strings.TrimSpace(event.ThreadID)
	}
	if trimmed := strings.TrimSpace(event.GroupID); trimmed != "" {
		return trimmed, strings.TrimSpace(event.ThreadID)
	}
	if trimmed := strings.TrimSpace(event.ThreadID); trimmed != "" {
		return trimmed, trimmed
	}
	return fmt.Sprintf("sigil:framework:%s:%s", frameworkName, strings.TrimSpace(event.RunID)), ""
}

func resolveProvider(explicitProvider string, eventProvider string, modelName string, resolver ProviderResolver, event RunStartEvent) string {
	if normalized := normalizeProvider(explicitProvider); normalized != "" {
		return normalized
	}
	if normalized := normalizeProvider(eventProvider); normalized != "" {
		return normalized
	}
	if resolver != nil {
		if normalized := normalizeProvider(resolver(modelName, event)); normalized != "" {
			return normalized
		}
	}
	return inferProvider(modelName)
}

func normalizeProvider(value string) string {
	normalized := strings.ToLower(strings.TrimSpace(value))
	switch normalized {
	case "openai", "anthropic", "gemini":
		return normalized
	case "":
		return ""
	default:
		return "custom"
	}
}

func inferProvider(modelName string) string {
	normalized := strings.ToLower(strings.TrimSpace(modelName))
	switch {
	case strings.HasPrefix(normalized, "gpt-"), strings.HasPrefix(normalized, "o1"), strings.HasPrefix(normalized, "o3"), strings.HasPrefix(normalized, "o4"):
		return "openai"
	case strings.HasPrefix(normalized, "claude-"):
		return "anthropic"
	case strings.HasPrefix(normalized, "gemini-"):
		return "gemini"
	default:
		return "custom"
	}
}

func normalizeTags(tags []string) []string {
	if len(tags) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(tags))
	for _, raw := range tags {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		if _, exists := seen[trimmed]; exists {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	return out
}

type frameworkMetadataInput struct {
	baseMetadata  map[string]any
	eventMetadata map[string]any
	runID         string
	threadID      string
	parentRunID   string
	componentName string
	runType       string
	tags          []string
	retryAttempt  *int
	eventID       string
}

func buildFrameworkMetadata(input frameworkMetadataInput) map[string]any {
	metadata := map[string]any{}
	mergeMetadata(metadata, input.baseMetadata)
	mergeMetadata(metadata, input.eventMetadata)

	metadata[metadataRunID] = strings.TrimSpace(input.runID)
	metadata[metadataRunType] = strings.TrimSpace(input.runType)
	if strings.TrimSpace(input.threadID) != "" {
		metadata[metadataThreadID] = strings.TrimSpace(input.threadID)
	}
	if strings.TrimSpace(input.parentRunID) != "" {
		metadata[metadataParentRunID] = strings.TrimSpace(input.parentRunID)
	}
	if strings.TrimSpace(input.componentName) != "" {
		metadata[metadataComponentName] = strings.TrimSpace(input.componentName)
	}
	if len(input.tags) > 0 {
		metadata[metadataTags] = input.tags
	}
	if input.retryAttempt != nil {
		metadata[metadataRetryAttempt] = *input.retryAttempt
	}
	if strings.TrimSpace(input.eventID) != "" {
		metadata[metadataEventID] = strings.TrimSpace(input.eventID)
	}

	return normalizeMetadata(metadata)
}

func mergeMetadata(dst map[string]any, src map[string]any) {
	for key, value := range src {
		trimmed := strings.TrimSpace(key)
		if trimmed == "" {
			continue
		}
		dst[trimmed] = value
	}
}

func normalizeMetadata(in map[string]any) map[string]any {
	if len(in) == 0 {
		return map[string]any{}
	}
	out := map[string]any{}
	seen := map[uintptr]struct{}{}
	for key, value := range in {
		trimmed := strings.TrimSpace(key)
		if trimmed == "" {
			continue
		}
		normalized, ok := normalizeMetadataValue(value, 0, seen)
		if !ok {
			continue
		}
		out[trimmed] = normalized
	}
	return out
}

func normalizeMetadataValue(value any, depth int, seen map[uintptr]struct{}) (any, bool) {
	if depth > maxMetadataNormalizeDepth {
		return nil, false
	}
	if value == nil {
		return nil, true
	}

	switch typed := value.(type) {
	case string:
		return typed, true
	case bool:
		return typed, true
	case int:
		return typed, true
	case int8:
		return int64(typed), true
	case int16:
		return int64(typed), true
	case int32:
		return int64(typed), true
	case int64:
		return typed, true
	case uint:
		if uint64(typed) > math.MaxInt64 {
			return nil, false
		}
		return int64(typed), true
	case uint8:
		return int64(typed), true
	case uint16:
		return int64(typed), true
	case uint32:
		return int64(typed), true
	case uint64:
		if typed > math.MaxInt64 {
			return nil, false
		}
		return int64(typed), true
	case float32:
		if !math.IsInf(float64(typed), 0) && !math.IsNaN(float64(typed)) {
			return float64(typed), true
		}
		return nil, false
	case float64:
		if !math.IsInf(typed, 0) && !math.IsNaN(typed) {
			return typed, true
		}
		return nil, false
	case time.Time:
		return typed.UTC().Format(time.RFC3339Nano), true
	case []string:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			out = append(out, item)
		}
		return out, true
	case []any:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			normalized, ok := normalizeMetadataValue(item, depth+1, seen)
			if !ok {
				continue
			}
			out = append(out, normalized)
		}
		return out, true
	case map[string]any:
		return normalizeMetadataMap(reflect.ValueOf(typed), depth, seen)
	default:
		valueOf := reflect.ValueOf(value)
		if valueOf.Kind() == reflect.Pointer {
			if valueOf.IsNil() {
				return nil, true
			}
			pointer := valueOf.Pointer()
			if _, exists := seen[pointer]; exists {
				return "[circular]", true
			}
			seen[pointer] = struct{}{}
			defer delete(seen, pointer)
			return normalizeMetadataValue(valueOf.Elem().Interface(), depth+1, seen)
		}
		if valueOf.Kind() == reflect.Struct {
			return normalizeMetadataStruct(valueOf, depth, seen)
		}
		if valueOf.Kind() == reflect.Map && valueOf.Type().Key().Kind() == reflect.String {
			return normalizeMetadataMap(valueOf, depth, seen)
		}
		if valueOf.Kind() == reflect.Slice || valueOf.Kind() == reflect.Array {
			out := make([]any, 0, valueOf.Len())
			for i := 0; i < valueOf.Len(); i++ {
				normalized, ok := normalizeMetadataValue(valueOf.Index(i).Interface(), depth+1, seen)
				if !ok {
					continue
				}
				out = append(out, normalized)
			}
			return out, true
		}
		return nil, false
	}
}

func normalizeMetadataStruct(value reflect.Value, depth int, seen map[uintptr]struct{}) (any, bool) {
	out := map[string]any{}
	typ := value.Type()
	for i := 0; i < value.NumField(); i++ {
		field := typ.Field(i)
		// Skip unexported fields.
		if field.PkgPath != "" {
			continue
		}

		key := field.Name
		if tag := strings.TrimSpace(field.Tag.Get("json")); tag != "" {
			parts := strings.Split(tag, ",")
			switch parts[0] {
			case "-":
				continue
			case "":
				// Keep default field name.
			default:
				key = parts[0]
			}
		}

		normalized, ok := normalizeMetadataValue(value.Field(i).Interface(), depth+1, seen)
		if !ok {
			continue
		}
		out[key] = normalized
	}
	return out, true
}

func normalizeMetadataMap(value reflect.Value, depth int, seen map[uintptr]struct{}) (any, bool) {
	if value.IsNil() {
		return map[string]any{}, true
	}

	pointer := value.Pointer()
	if _, exists := seen[pointer]; exists {
		return "[circular]", true
	}
	seen[pointer] = struct{}{}
	defer delete(seen, pointer)

	out := map[string]any{}
	iter := value.MapRange()
	for iter.Next() {
		trimmed := strings.TrimSpace(iter.Key().String())
		if trimmed == "" {
			continue
		}
		normalized, ok := normalizeMetadataValue(iter.Value().Interface(), depth+1, seen)
		if !ok {
			continue
		}
		out[trimmed] = normalized
	}
	return out, true
}
