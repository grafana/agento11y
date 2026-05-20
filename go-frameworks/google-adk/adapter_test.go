package googleadk

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grafana/sigil-sdk/go/sigil"
)

func boolPtr(v bool) *bool {
	return &v
}

func TestResolveConversationIDPrecedence(t *testing.T) {
	conversationID, threadID := resolveConversationID(RunStartEvent{
		RunID:          "run-1",
		ConversationID: "conversation-1",
		SessionID:      "session-1",
		GroupID:        "group-1",
		ThreadID:       "thread-1",
	})
	if conversationID != "conversation-1" {
		t.Fatalf("expected conversation-1, got %q", conversationID)
	}
	if threadID != "thread-1" {
		t.Fatalf("expected thread-1, got %q", threadID)
	}

	conversationID, threadID = resolveConversationID(RunStartEvent{RunID: "run-2", SessionID: "session-2"})
	if conversationID != "session-2" {
		t.Fatalf("expected session-2, got %q", conversationID)
	}
	if threadID != "" {
		t.Fatalf("expected empty thread id, got %q", threadID)
	}

	conversationID, _ = resolveConversationID(RunStartEvent{RunID: "run-3"})
	if conversationID != "sigil:framework:google-adk:run-3" {
		t.Fatalf("unexpected fallback conversation id %q", conversationID)
	}
}

func TestResolveProviderUsesInferenceAndExplicitOverrides(t *testing.T) {
	event := RunStartEvent{ModelName: "gpt-5"}
	if got := resolveProvider("", "", event.ModelName, nil, event); got != "openai" {
		t.Fatalf("expected openai inference, got %q", got)
	}
	if got := resolveProvider("gemini", "", "gpt-5", nil, event); got != "gemini" {
		t.Fatalf("expected explicit provider override, got %q", got)
	}
	if got := resolveProvider("", "anthropic", "gpt-5", nil, event); got != "anthropic" {
		t.Fatalf("expected event provider, got %q", got)
	}
	resolver := func(_ string, _ RunStartEvent) string { return "gemini" }
	if got := resolveProvider("", "", "custom-model", resolver, event); got != "gemini" {
		t.Fatalf("expected resolver provider, got %q", got)
	}
}

func TestBuildFrameworkMetadataIncludesCanonicalKeys(t *testing.T) {
	retry := 3
	metadata := buildFrameworkMetadata(frameworkMetadataInput{
		baseMetadata:  map[string]any{"team": "infra"},
		eventMetadata: map[string]any{"event_payload": map[string]any{"step": "validate"}},
		runID:         "run-1",
		threadID:      "thread-1",
		parentRunID:   "parent-1",
		componentName: "planner",
		runType:       "chat",
		tags:          []string{"prod", "framework"},
		retryAttempt:  &retry,
		eventID:       "event-1",
	})

	if metadata[metadataRunID] != "run-1" {
		t.Fatalf("expected run id metadata")
	}
	if metadata[metadataRunType] != "chat" {
		t.Fatalf("expected run type metadata")
	}
	if metadata[metadataThreadID] != "thread-1" {
		t.Fatalf("expected thread metadata")
	}
	if metadata[metadataParentRunID] != "parent-1" {
		t.Fatalf("expected parent run metadata")
	}
	if metadata[metadataComponentName] != "planner" {
		t.Fatalf("expected component metadata")
	}
	if metadata[metadataRetryAttempt] != 3 {
		t.Fatalf("expected retry metadata")
	}
	if metadata[metadataEventID] != "event-1" {
		t.Fatalf("expected event metadata")
	}
	if metadata["team"] != "infra" {
		t.Fatalf("expected extra metadata")
	}
}

func TestBuildFrameworkMetadataNormalizesTypedNestedMaps(t *testing.T) {
	metadata := buildFrameworkMetadata(frameworkMetadataInput{
		baseMetadata: map[string]any{
			"labels":   map[string]string{"env": "prod"},
			"counters": map[string]int{"attempt": 2},
		},
		runID:   "run-typed",
		runType: "chat",
	})

	labels, ok := metadata["labels"].(map[string]any)
	if !ok {
		t.Fatalf("expected labels metadata to be normalized map[string]any, got %T", metadata["labels"])
	}
	if labels["env"] != "prod" {
		t.Fatalf("expected labels.env=prod, got %v", labels["env"])
	}

	counters, ok := metadata["counters"].(map[string]any)
	if !ok {
		t.Fatalf("expected counters metadata to be normalized map[string]any, got %T", metadata["counters"])
	}
	if counters["attempt"] != 2 {
		t.Fatalf("expected counters.attempt=2, got %#v", counters["attempt"])
	}
}

func TestBuildFrameworkMetadataRejectsOverflowUintValues(t *testing.T) {
	if strconv.IntSize != 64 {
		t.Skip("overflow uint normalization test requires 64-bit uint")
	}

	overflow := uint(uint64(math.MaxInt64) + 1)
	metadata := buildFrameworkMetadata(frameworkMetadataInput{
		baseMetadata: map[string]any{
			"ok_uint":       uint(42),
			"overflow_uint": overflow,
		},
		runID:   "run-uint-overflow",
		runType: "chat",
	})

	if metadata["ok_uint"] != int64(42) {
		t.Fatalf("expected ok_uint to normalize to int64(42), got %#v", metadata["ok_uint"])
	}
	if _, exists := metadata["overflow_uint"]; exists {
		t.Fatalf("expected overflow_uint to be dropped, got %#v", metadata["overflow_uint"])
	}
}

func TestAdapterRunAndToolLifecycle(t *testing.T) {
	cfg := sigil.DefaultConfig()
	cfg.GenerationExport.Protocol = sigil.GenerationExportProtocolNone
	client := sigil.NewClient(cfg)
	t.Cleanup(func() {
		_ = client.Shutdown(context.Background())
	})

	adapter := NewSigilAdapter(client, Options{
		AgentName:      "adk-agent",
		AgentVersion:   "1.0.0",
		CaptureInputs:  boolPtr(true),
		CaptureOutputs: boolPtr(true),
	})

	ctx := context.Background()
	err := adapter.OnRunStart(ctx, RunStartEvent{
		RunID:       "run-sync",
		SessionID:   "session-42",
		ParentRunID: "parent-run",
		EventID:     "event-42",
		RunType:     "chat",
		ModelName:   "gpt-5",
		Prompts:     []string{"hello"},
		Metadata:    map[string]any{"team": "infra"},
	})
	if err != nil {
		t.Fatalf("run start: %v", err)
	}

	err = adapter.OnRunEnd("run-sync", RunEndEvent{
		RunID:          "run-sync",
		OutputMessages: []sigil.Message{sigil.AssistantTextMessage("world")},
		ResponseModel:  "gpt-5",
		StopReason:     "stop",
		Usage: sigil.TokenUsage{
			InputTokens:  4,
			OutputTokens: 2,
			TotalTokens:  6,
		},
	})
	if err != nil {
		t.Fatalf("run end: %v", err)
	}

	err = adapter.OnToolStart(ctx, ToolStartEvent{
		RunID:           "tool-run",
		SessionID:       "session-42",
		ToolName:        "lookup",
		ToolDescription: "Lookup customer profile",
		Arguments:       map[string]any{"customer_id": "42"},
	})
	if err != nil {
		t.Fatalf("tool start: %v", err)
	}
	err = adapter.OnToolEnd("tool-run", ToolEndEvent{Result: map[string]any{"status": "ok"}, CompletedAt: time.Now().UTC()})
	if err != nil {
		t.Fatalf("tool end: %v", err)
	}

	err = adapter.OnRunStart(ctx, RunStartEvent{
		RunID:     "run-stream",
		ModelName: "claude-sonnet-4-5",
		Stream:    true,
		Prompts:   []string{"stream me"},
	})
	if err != nil {
		t.Fatalf("stream start: %v", err)
	}
	adapter.OnRunToken("run-stream", "hello")
	adapter.OnRunToken("run-stream", " world")
	err = adapter.OnRunEnd("run-stream", RunEndEvent{RunID: "run-stream", ResponseModel: "claude-sonnet-4-5"})
	if err != nil {
		t.Fatalf("stream end: %v", err)
	}
}

func TestOnRunEndDropsOutputsWhenCaptureDisabled(t *testing.T) {
	ingest := newGenerationCaptureServer(t)
	cfg := sigil.DefaultConfig()
	cfg.GenerationExport.Protocol = sigil.GenerationExportProtocolHTTP
	cfg.GenerationExport.Endpoint = ingest.server.URL + "/api/v1/generations:export"
	cfg.GenerationExport.BatchSize = 1
	cfg.GenerationExport.QueueSize = 10
	cfg.GenerationExport.FlushInterval = time.Hour
	cfg.GenerationExport.MaxRetries = 1
	cfg.GenerationExport.InitialBackoff = time.Millisecond
	cfg.GenerationExport.MaxBackoff = 10 * time.Millisecond
	client := sigil.NewClient(cfg)
	t.Cleanup(func() {
		ingest.server.Close()
	})

	adapter := NewSigilAdapter(client, Options{
		CaptureInputs:  boolPtr(true),
		CaptureOutputs: boolPtr(false),
	})

	ctx := context.Background()
	if err := adapter.OnRunStart(ctx, RunStartEvent{
		RunID:     "run-no-output",
		SessionID: "session-42",
		ModelName: "gpt-5",
		RunType:   "chat",
		Prompts:   []string{"hello"},
	}); err != nil {
		t.Fatalf("run start: %v", err)
	}

	if err := adapter.OnRunEnd("run-no-output", RunEndEvent{
		RunID:          "run-no-output",
		OutputMessages: []sigil.Message{sigil.AssistantTextMessage("should-not-export")},
		ResponseModel:  "gpt-5",
	}); err != nil {
		t.Fatalf("run end: %v", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	request := ingest.singleRequest(t)
	generations, ok := request["generations"].([]any)
	if !ok || len(generations) != 1 {
		t.Fatalf("expected one generation in export request, got %#v", request["generations"])
	}
	generation, ok := generations[0].(map[string]any)
	if !ok {
		t.Fatalf("expected generation object, got %T", generations[0])
	}
	if rawOutput, exists := generation["output"]; exists {
		output, ok := rawOutput.([]any)
		if !ok {
			t.Fatalf("expected output array, got %T", rawOutput)
		}
		if len(output) != 0 {
			t.Fatalf("expected empty output when captureOutputs=false, got %#v", output)
		}
	}
}

func TestNewSigilAdapterCaptureDefaultsAndExplicitDisable(t *testing.T) {
	cfg := sigil.DefaultConfig()
	cfg.GenerationExport.Protocol = sigil.GenerationExportProtocolNone
	client := sigil.NewClient(cfg)
	t.Cleanup(func() {
		_ = client.Shutdown(context.Background())
	})

	defaults := NewSigilAdapter(client, Options{})
	if !defaults.captureInputs {
		t.Fatalf("expected capture inputs default true")
	}
	if !defaults.captureOutputs {
		t.Fatalf("expected capture outputs default true")
	}

	explicitOff := NewSigilAdapter(client, Options{
		CaptureInputs:  boolPtr(false),
		CaptureOutputs: boolPtr(false),
	})
	if explicitOff.captureInputs {
		t.Fatalf("expected explicit capture inputs=false to be preserved")
	}
	if explicitOff.captureOutputs {
		t.Fatalf("expected explicit capture outputs=false to be preserved")
	}

	overrideOpts := NewSigilAdapter(client, Options{
		CaptureInputs:  boolPtr(false),
		CaptureOutputs: boolPtr(true),
	})
	if overrideOpts.captureInputs {
		t.Fatalf("expected capture inputs override false to be preserved")
	}
	if !overrideOpts.captureOutputs {
		t.Fatalf("expected capture outputs override true to be preserved")
	}
}

func TestOnToolStartDropsArgumentsWhenCaptureInputsDisabled(t *testing.T) {
	cfg := sigil.DefaultConfig()
	cfg.GenerationExport.Protocol = sigil.GenerationExportProtocolNone
	client := sigil.NewClient(cfg)
	t.Cleanup(func() {
		_ = client.Shutdown(context.Background())
	})

	adapter := NewSigilAdapter(client, Options{
		CaptureInputs:  boolPtr(false),
		CaptureOutputs: boolPtr(true),
	})

	if err := adapter.OnToolStart(context.Background(), ToolStartEvent{
		RunID:     "tool-no-input-capture",
		SessionID: "session-42",
		ToolName:  "lookup",
		Arguments: map[string]any{"customer_id": "42"},
	}); err != nil {
		t.Fatalf("tool start: %v", err)
	}

	adapter.toolRunsMu.Lock()
	state := adapter.toolRuns["tool-no-input-capture"]
	adapter.toolRunsMu.Unlock()
	if state == nil {
		t.Fatalf("expected tool run state to be created")
	}
	if state.arguments != nil {
		t.Fatalf("expected tool arguments to be dropped when captureInputs=false, got %#v", state.arguments)
	}

	if err := adapter.OnToolEnd("tool-no-input-capture", ToolEndEvent{Result: map[string]any{"status": "ok"}}); err != nil {
		t.Fatalf("tool end: %v", err)
	}
}

func TestOnToolStartPropagatesToolCallFields(t *testing.T) {
	cfg := sigil.DefaultConfig()
	cfg.GenerationExport.Protocol = sigil.GenerationExportProtocolNone
	client := sigil.NewClient(cfg)
	t.Cleanup(func() {
		_ = client.Shutdown(context.Background())
	})

	adapter := NewSigilAdapter(client, Options{
		AgentName:    "adk-agent",
		AgentVersion: "1.0.0",
	})

	var captured sigil.ToolExecutionStart
	adapter.startTool = func(ctx context.Context, start sigil.ToolExecutionStart) *sigil.ToolExecutionRecorder {
		captured = start
		_, rec := client.StartToolExecution(ctx, start)
		return rec
	}

	if err := adapter.OnToolStart(context.Background(), ToolStartEvent{
		RunID:           "tool-propagation",
		SessionID:       "session-42",
		ToolCallID:      "call-weather",
		ToolName:        "weather.lookup",
		ToolType:        "function",
		ToolDescription: "Look up weather",
		Arguments:       map[string]any{"city": "Paris"},
		ModelName:       "gpt-5",
		Provider:        "openai",
	}); err != nil {
		t.Fatalf("tool start: %v", err)
	}

	if captured.ToolCallID != "call-weather" {
		t.Fatalf("expected tool call id propagation, got %q", captured.ToolCallID)
	}
	if captured.ToolType != "function" {
		t.Fatalf("expected tool type propagation, got %q", captured.ToolType)
	}
	if captured.ToolName != "weather.lookup" {
		t.Fatalf("expected tool name propagation, got %q", captured.ToolName)
	}
	if captured.ConversationID != "session-42" {
		t.Fatalf("expected conversation propagation, got %q", captured.ConversationID)
	}
	if captured.RequestModel != "gpt-5" {
		t.Fatalf("expected request model propagation, got %q", captured.RequestModel)
	}
	if captured.RequestProvider != "openai" {
		t.Fatalf("expected request provider propagation, got %q", captured.RequestProvider)
	}

	if err := adapter.OnToolEnd("tool-propagation", ToolEndEvent{CompletedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("tool end: %v", err)
	}
}

func TestOnToolStartResolvesModelAndProvider(t *testing.T) {
	tests := []struct {
		name            string
		adapterProvider string
		eventModelName  string
		eventProvider   string
		wantModel       string
		wantProvider    string
	}{
		{
			name:           "explicit event provider and model",
			eventModelName: "claude-4",
			eventProvider:  "anthropic",
			wantModel:      "claude-4",
			wantProvider:   "anthropic",
		},
		{
			name:            "adapter-level provider fallback",
			adapterProvider: "openai",
			eventModelName:  "gpt-5",
			wantModel:       "gpt-5",
			wantProvider:    "openai",
		},
		{
			name:           "inferred provider from model name",
			eventModelName: "gemini-2.0-flash",
			wantModel:      "gemini-2.0-flash",
			wantProvider:   "gemini",
		},
		{
			name:         "unknown model defaults",
			wantModel:    "unknown",
			wantProvider: "custom",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := sigil.DefaultConfig()
			cfg.GenerationExport.Protocol = sigil.GenerationExportProtocolNone
			client := sigil.NewClient(cfg)
			t.Cleanup(func() {
				_ = client.Shutdown(context.Background())
			})

			adapter := NewSigilAdapter(client, Options{Provider: tt.adapterProvider})

			var captured sigil.ToolExecutionStart
			adapter.startTool = func(ctx context.Context, start sigil.ToolExecutionStart) *sigil.ToolExecutionRecorder {
				captured = start
				_, rec := client.StartToolExecution(ctx, start)
				return rec
			}

			if err := adapter.OnToolStart(context.Background(), ToolStartEvent{
				RunID:     "tool-resolve-" + tt.name,
				SessionID: "session-42",
				ToolName:  "lookup",
				ModelName: tt.eventModelName,
				Provider:  tt.eventProvider,
			}); err != nil {
				t.Fatalf("tool start: %v", err)
			}

			if captured.RequestModel != tt.wantModel {
				t.Fatalf("expected request model %q, got %q", tt.wantModel, captured.RequestModel)
			}
			if captured.RequestProvider != tt.wantProvider {
				t.Fatalf("expected request provider %q, got %q", tt.wantProvider, captured.RequestProvider)
			}

			if err := adapter.OnToolEnd("tool-resolve-"+tt.name, ToolEndEvent{CompletedAt: time.Now().UTC()}); err != nil {
				t.Fatalf("tool end: %v", err)
			}
		})
	}
}

func TestBuildFrameworkMetadataNormalizesStructAndPointerValues(t *testing.T) {
	type metadataDetails struct {
		Enabled bool `json:"enabled"`
	}
	type metadataPayload struct {
		Name    string           `json:"name"`
		Count   int              `json:"count"`
		Details *metadataDetails `json:"details"`
		Skip    string           `json:"-"`
	}
	type metadataNode struct {
		ID   string        `json:"id"`
		Next *metadataNode `json:"next"`
	}

	cycle := &metadataNode{ID: "node-1"}
	cycle.Next = cycle

	payload := &metadataPayload{
		Name:    "payload",
		Count:   7,
		Details: &metadataDetails{Enabled: true},
		Skip:    "ignored",
	}

	metadata := buildFrameworkMetadata(frameworkMetadataInput{
		baseMetadata: map[string]any{
			"payload": payload,
			"cycle":   cycle,
		},
		runID:   "run-structs",
		runType: "chat",
	})

	normalizedPayload, ok := metadata["payload"].(map[string]any)
	if !ok {
		t.Fatalf("expected payload metadata to be normalized map[string]any, got %T", metadata["payload"])
	}
	if normalizedPayload["name"] != "payload" {
		t.Fatalf("expected payload.name=payload, got %#v", normalizedPayload["name"])
	}
	if normalizedPayload["count"] != 7 {
		t.Fatalf("expected payload.count=7, got %#v", normalizedPayload["count"])
	}
	if _, exists := normalizedPayload["Skip"]; exists {
		t.Fatalf("expected json:\"-\" field to be omitted, got %#v", normalizedPayload["Skip"])
	}
	details, ok := normalizedPayload["details"].(map[string]any)
	if !ok || details["enabled"] != true {
		t.Fatalf("expected payload.details.enabled=true, got %#v", normalizedPayload["details"])
	}

	normalizedCycle, ok := metadata["cycle"].(map[string]any)
	if !ok {
		t.Fatalf("expected cycle metadata to be normalized map[string]any, got %T", metadata["cycle"])
	}
	if normalizedCycle["id"] != "node-1" {
		t.Fatalf("expected cycle.id=node-1, got %#v", normalizedCycle["id"])
	}
	if normalizedCycle["next"] != "[circular]" {
		t.Fatalf("expected cycle.next=[circular], got %#v", normalizedCycle["next"])
	}
}

func TestOnRunStartDeduplicatesConcurrentStarts(t *testing.T) {
	cfg := sigil.DefaultConfig()
	cfg.GenerationExport.Protocol = sigil.GenerationExportProtocolNone
	client := sigil.NewClient(cfg)
	t.Cleanup(func() {
		_ = client.Shutdown(context.Background())
	})

	adapter := NewSigilAdapter(client, Options{})
	var startCalls int32
	adapter.startRun = func(ctx context.Context, start sigil.GenerationStart, stream bool) *sigil.GenerationRecorder {
		atomic.AddInt32(&startCalls, 1)
		if stream {
			_, rec := client.StartStreamingGeneration(ctx, start)
			return rec
		}
		_, rec := client.StartGeneration(ctx, start)
		return rec
	}

	startEvent := RunStartEvent{
		RunID:     "run-concurrent",
		SessionID: "session-concurrent",
		ModelName: "gpt-5",
		RunType:   "chat",
		Prompts:   []string{"hello"},
	}

	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			_ = adapter.OnRunStart(context.Background(), startEvent)
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&startCalls); got != 1 {
		t.Fatalf("expected one generation start call, got %d", got)
	}

	if err := adapter.OnRunEnd("run-concurrent", RunEndEvent{RunID: "run-concurrent"}); err != nil {
		t.Fatalf("run end: %v", err)
	}
}

func TestOnToolStartDeduplicatesConcurrentStarts(t *testing.T) {
	cfg := sigil.DefaultConfig()
	cfg.GenerationExport.Protocol = sigil.GenerationExportProtocolNone
	client := sigil.NewClient(cfg)
	t.Cleanup(func() {
		_ = client.Shutdown(context.Background())
	})

	adapter := NewSigilAdapter(client, Options{})
	var startCalls int32
	adapter.startTool = func(ctx context.Context, start sigil.ToolExecutionStart) *sigil.ToolExecutionRecorder {
		atomic.AddInt32(&startCalls, 1)
		_, rec := client.StartToolExecution(ctx, start)
		return rec
	}

	startEvent := ToolStartEvent{
		RunID:           "tool-concurrent",
		SessionID:       "session-concurrent",
		ToolName:        "lookup_customer",
		ToolDescription: "Lookup customer profile",
		Arguments:       map[string]any{"customer_id": "42"},
	}

	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			_ = adapter.OnToolStart(context.Background(), startEvent)
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&startCalls); got != 1 {
		t.Fatalf("expected one tool start call, got %d", got)
	}

	if err := adapter.OnToolEnd("tool-concurrent", ToolEndEvent{CompletedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("tool end: %v", err)
	}
}

func TestNewCallbacksProvidesOneTimeLifecycleWiring(t *testing.T) {
	cfg := sigil.DefaultConfig()
	cfg.GenerationExport.Protocol = sigil.GenerationExportProtocolNone
	client := sigil.NewClient(cfg)
	t.Cleanup(func() {
		_ = client.Shutdown(context.Background())
	})

	callbacks := NewCallbacks(client, Options{
		AgentName:      "adk-agent",
		CaptureInputs:  boolPtr(true),
		CaptureOutputs: boolPtr(true),
	})

	ctx := context.Background()
	if err := callbacks.OnRunStart(ctx, RunStartEvent{
		RunID:     "run-callbacks",
		SessionID: "session-callbacks",
		ModelName: "gpt-5",
		RunType:   "chat",
		Prompts:   []string{"hello"},
	}); err != nil {
		t.Fatalf("run start: %v", err)
	}
	callbacks.OnRunToken("run-callbacks", "hi")
	if err := callbacks.OnRunEnd("run-callbacks", RunEndEvent{
		RunID:          "run-callbacks",
		OutputMessages: []sigil.Message{sigil.AssistantTextMessage("hello")},
		ResponseModel:  "gpt-5",
	}); err != nil {
		t.Fatalf("run end: %v", err)
	}

	if err := callbacks.OnToolStart(ctx, ToolStartEvent{
		RunID:     "tool-callbacks",
		SessionID: "session-callbacks",
		ToolName:  "lookup",
		Arguments: map[string]any{"id": "42"},
		ThreadID:  "thread-callbacks",
	}); err != nil {
		t.Fatalf("tool start: %v", err)
	}
	if err := callbacks.OnToolEnd("tool-callbacks", ToolEndEvent{Result: map[string]any{"status": "ok"}}); err != nil {
		t.Fatalf("tool end: %v", err)
	}
}

type generationCaptureServer struct {
	server   *httptest.Server
	mu       sync.Mutex
	requests []map[string]any
}

func newGenerationCaptureServer(t *testing.T) *generationCaptureServer {
	t.Helper()
	capture := &generationCaptureServer{}
	capture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		request := map[string]any{}
		if err := json.Unmarshal(body, &request); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		capture.mu.Lock()
		capture.requests = append(capture.requests, request)
		capture.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(acceptanceResponse(request))
	}))
	return capture
}

func acceptanceResponse(request map[string]any) map[string]any {
	generations, _ := request["generations"].([]any)
	results := make([]map[string]any, 0, len(generations))
	for _, raw := range generations {
		generation, ok := raw.(map[string]any)
		if !ok {
			results = append(results, map[string]any{"accepted": true})
			continue
		}
		id, _ := generation["id"].(string)
		results = append(results, map[string]any{
			"generation_id": id,
			"accepted":      true,
		})
	}
	return map[string]any{"results": results}
}

func (c *generationCaptureServer) singleRequest(t *testing.T) map[string]any {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.requests) != 1 {
		t.Fatalf("expected exactly one export request, got %d", len(c.requests))
	}
	return c.requests[0]
}
