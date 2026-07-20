package agento11y

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	agento11yv1 "github.com/grafana/agento11y/go/proto/agento11y/v1"
	"go.opentelemetry.io/otel/trace/noop"
)

type capturingExperimentExporter struct {
	mu       sync.Mutex
	requests []*agento11yv1.ExportGenerationsRequest
}

func (e *capturingExperimentExporter) Export(_ context.Context, request *agento11yv1.ExportGenerationsRequest) (*agento11yv1.ExportGenerationsResponse, error) {
	e.mu.Lock()
	e.requests = append(e.requests, request)
	e.mu.Unlock()
	response := &agento11yv1.ExportGenerationsResponse{}
	for _, generation := range request.GetGenerations() {
		response.Results = append(response.Results, &agento11yv1.ExportGenerationResult{
			GenerationId: generation.GetId(),
			Accepted:     true,
		})
	}
	return response, nil
}

func (e *capturingExperimentExporter) ExportWorkflowSteps(_ context.Context, request *agento11yv1.ExportWorkflowStepsRequest) (*agento11yv1.ExportWorkflowStepsResponse, error) {
	response := &agento11yv1.ExportWorkflowStepsResponse{
		Results: make([]*agento11yv1.ExportWorkflowStepResult, 0, len(request.GetWorkflowSteps())),
	}
	for _, step := range request.GetWorkflowSteps() {
		response.Results = append(response.Results, &agento11yv1.ExportWorkflowStepResult{
			StepId:   step.GetId(),
			Accepted: true,
		})
	}
	return response, nil
}

func (e *capturingExperimentExporter) Shutdown(context.Context) error { return nil }

func (e *capturingExperimentExporter) firstGeneration() *agento11yv1.Generation {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.requests) == 0 || len(e.requests[0].GetGenerations()) == 0 {
		return nil
	}
	return e.requests[0].GetGenerations()[0]
}

func (e *capturingExperimentExporter) hasGeneration(generationID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, request := range e.requests {
		for _, generation := range request.GetGenerations() {
			if generation.GetId() == generationID {
				return true
			}
		}
	}
	return false
}

func (e *capturingExperimentExporter) generationCount(generationID string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	count := 0
	for _, request := range e.requests {
		for _, generation := range request.GetGenerations() {
			if generation.GetId() == generationID {
				count++
			}
		}
	}
	return count
}

func (e *capturingExperimentExporter) generations(generationID string) []*agento11yv1.Generation {
	e.mu.Lock()
	defer e.mu.Unlock()
	var generations []*agento11yv1.Generation
	for _, request := range e.requests {
		for _, generation := range request.GetGenerations() {
			if generation.GetId() == generationID {
				generations = append(generations, generation)
			}
		}
	}
	return generations
}

func TestRecordIOWithoutIOOrUsageDoesNotAttachGenerationID(t *testing.T) {
	recorder := &experimentRecorder{}
	recorder.push(http.StatusAccepted, map[string]any{"results": []map[string]any{{"score_id": "score-1", "accepted": true}}})
	server := httptest.NewServer(recorder.handler(t))
	defer server.Close()

	client := newExperimentTestClient(t, server.URL)
	trial := NewTrial(client, TrialRef{RunID: "run-empty-io", TestCaseID: "case-empty-io"})
	trial.RecordIO(RecordIOOptions{
		ModelProvider: "example",
		ModelName:     "agent",
		AgentName:     "support-agent",
	})
	score := trial.FinalScore(BoolScoreValue(true), ScoreOptions{})
	if score.GenerationID != "" {
		t.Fatalf("expected score without recorded IO or usage to omit generation_id, got %q", score.GenerationID)
	}

	accepted, err := trial.Flush(context.Background())
	if err != nil {
		t.Fatalf("flush trial: %v", err)
	}
	if accepted != 1 {
		t.Fatalf("expected one accepted score, got %d", accepted)
	}
	if recorder.requestCount() != 1 {
		t.Fatalf("expected only score export request, got %d", recorder.requestCount())
	}
	req := recorder.request(0)
	scorePayload := req.Payload["scores"].([]any)[0].(map[string]any)
	if _, ok := scorePayload["generation_id"]; ok {
		t.Fatalf("score without recorded IO or usage must not send generation_id: %#v", scorePayload)
	}
}

func TestBindGenerationWithRecordIOExportsBoundGeneration(t *testing.T) {
	exporter := &capturingExperimentExporter{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/api/v1/scores:export" {
			http.NotFound(w, req)
			return
		}
		if !exporter.hasGeneration("gen-recorded") {
			http.Error(w, "recorded generation was not flushed before score export", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{{"score_id": "score-1", "accepted": true}},
		})
	}))
	defer server.Close()

	client := NewClient(Config{
		Tracer: noop.NewTracerProvider().Tracer("agento11y-go-bound-record-io-test"),
		GenerationExport: GenerationExportConfig{
			Protocol:        GenerationExportProtocolHTTP,
			Endpoint:        server.URL + "/api/v1/generations:export",
			Auth:            AuthConfig{Mode: ExportAuthModeTenant, TenantID: "tenant-a"},
			Insecure:        BoolPtr(true),
			BatchSize:       10,
			FlushInterval:   time.Hour,
			QueueSize:       100,
			MaxRetries:      0,
			PayloadMaxBytes: 1 << 20,
		},
		API:                    APIConfig{Endpoint: server.URL},
		testGenerationExporter: exporter,
	})
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })

	trial := NewTrial(client, TrialRef{RunID: "run-recorded", TestCaseID: "case-recorded"})
	trial.BindGeneration("gen-recorded", "conv-recorded")
	trial.RecordIO(RecordIOOptions{Input: "question", Output: "answer", ModelProvider: "example", ModelName: "agent"})
	trial.FinalScore(BoolScoreValue(true), ScoreOptions{})
	accepted, err := trial.Flush(context.Background())
	if err != nil {
		t.Fatalf("flush trial: %v", err)
	}
	if accepted != 1 {
		t.Fatalf("expected one accepted score, got %d", accepted)
	}
	generation := exporter.firstGeneration()
	if generation == nil || generation.GetId() != "gen-recorded" || generation.GetConversationId() != "conv-recorded" {
		t.Fatalf("expected recorded bound generation, got %#v", generation)
	}
}

func TestTrialFlushFlushesBoundGenerationBeforeScores(t *testing.T) {
	exporter := &capturingExperimentExporter{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/api/v1/scores:export" {
			http.NotFound(w, req)
			return
		}
		if !exporter.hasGeneration("gen-bound") {
			http.Error(w, "generation was not flushed before score export", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{{"score_id": "score-1", "accepted": true}},
		})
	}))
	defer server.Close()

	client := NewClient(Config{
		Tracer: noop.NewTracerProvider().Tracer("agento11y-go-trial-flush-order-test"),
		GenerationExport: GenerationExportConfig{
			Protocol:        GenerationExportProtocolHTTP,
			Endpoint:        server.URL + "/api/v1/generations:export",
			Auth:            AuthConfig{Mode: ExportAuthModeTenant, TenantID: "tenant-a"},
			Insecure:        BoolPtr(true),
			BatchSize:       10,
			FlushInterval:   time.Hour,
			QueueSize:       100,
			MaxRetries:      0,
			PayloadMaxBytes: 1 << 20,
		},
		API:                    APIConfig{Endpoint: server.URL},
		testGenerationExporter: exporter,
	})
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })

	ctx, recorder := client.StartGeneration(context.Background(), GenerationStart{
		ID:             "gen-bound",
		ConversationID: "conv-bound",
		Model:          ModelRef{Provider: "example", Name: "agent"},
	})
	recorder.SetResult(Generation{
		ID:             "gen-bound",
		ConversationID: "conv-bound",
		Model:          ModelRef{Provider: "example", Name: "agent"},
		Input:          []Message{UserTextMessage("question")},
		Output:         []Message{AssistantTextMessage("answer")},
	}, nil)
	recorder.End()

	trial := NewTrial(client, TrialRef{RunID: "run-bound", TestCaseID: "case-bound"})
	trial.BindGeneration("gen-bound", "conv-bound")
	trial.RecordIO(RecordIOOptions{Input: "question", Output: "answer", ModelProvider: "example", ModelName: "agent"})
	trial.FinalScore(BoolScoreValue(true), ScoreOptions{Evaluator: &Evaluator{EvaluatorID: "exact", Version: "1", Kind: EvaluatorKindDeterministic}})
	accepted, err := trial.Flush(ctx)
	if err != nil {
		t.Fatalf("flush trial: %v", err)
	}
	if accepted != 1 {
		t.Fatalf("expected one accepted score, got %d", accepted)
	}
	if !exporter.hasGeneration("gen-bound") {
		t.Fatal("expected bound generation to be flushed")
	}
	if got := exporter.generationCount("gen-bound"); got != 1 {
		t.Fatalf("expected bound generation to be exported once, got %d", got)
	}
}

func TestTrialFlushUsesRecordedGenerationWithoutDuplicate(t *testing.T) {
	exporter := &capturingExperimentExporter{}
	generationID := StableID("gen", "run-recorded-unbound", "case-recorded-unbound", 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/api/v1/scores:export" {
			http.NotFound(w, req)
			return
		}
		if !exporter.hasGeneration(generationID) {
			http.Error(w, "generation was not flushed before score export", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{{"score_id": "score-1", "accepted": true}},
		})
	}))
	defer server.Close()

	client := NewClient(Config{
		Tracer: noop.NewTracerProvider().Tracer("agento11y-go-recorded-generation-test"),
		GenerationExport: GenerationExportConfig{
			Protocol:        GenerationExportProtocolHTTP,
			Endpoint:        server.URL + "/api/v1/generations:export",
			Auth:            AuthConfig{Mode: ExportAuthModeTenant, TenantID: "tenant-a"},
			Insecure:        BoolPtr(true),
			BatchSize:       10,
			FlushInterval:   time.Hour,
			QueueSize:       100,
			MaxRetries:      0,
			PayloadMaxBytes: 1 << 20,
		},
		API:                    APIConfig{Endpoint: server.URL},
		testGenerationExporter: exporter,
	})
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })

	ctx, recorder := client.StartGeneration(context.Background(), GenerationStart{
		ID:             generationID,
		ConversationID: "conv-recorded-unbound",
		Model:          ModelRef{Provider: "example", Name: "agent"},
	})
	recorder.SetResult(Generation{
		ID:             generationID,
		ConversationID: "conv-recorded-unbound",
		Model:          ModelRef{Provider: "example", Name: "agent"},
		Input:          []Message{UserTextMessage("question")},
		Output:         []Message{AssistantTextMessage("answer")},
	}, nil)
	recorder.End()

	trial := NewTrial(client, TrialRef{RunID: "run-recorded-unbound", TestCaseID: "case-recorded-unbound"})
	trial.RecordIO(RecordIOOptions{Input: "question", Output: "answer", ModelProvider: "example", ModelName: "agent"})
	trial.FinalScore(BoolScoreValue(true), ScoreOptions{Evaluator: &Evaluator{EvaluatorID: "exact", Version: "1", Kind: EvaluatorKindDeterministic}})
	accepted, err := trial.Flush(ctx)
	if err != nil {
		t.Fatalf("flush trial: %v", err)
	}
	if accepted != 1 {
		t.Fatalf("expected one accepted score, got %d", accepted)
	}
	if got := exporter.generationCount(generationID); got != 1 {
		t.Fatalf("expected recorded generation to be exported once, got %d", got)
	}
}

func TestTrialRecordIOExportsSnapshotWhenRecordedGenerationDiffers(t *testing.T) {
	exporter := &capturingExperimentExporter{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/api/v1/scores:export" {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{{"score_id": "score-1", "accepted": true}},
		})
	}))
	defer server.Close()

	client := NewClient(Config{
		Tracer: noop.NewTracerProvider().Tracer("agento11y-go-recordio-stale-generation-test"),
		GenerationExport: GenerationExportConfig{
			Protocol:        GenerationExportProtocolHTTP,
			Endpoint:        server.URL + "/api/v1/generations:export",
			Auth:            AuthConfig{Mode: ExportAuthModeTenant, TenantID: "tenant-a"},
			Insecure:        BoolPtr(true),
			BatchSize:       10,
			FlushInterval:   time.Hour,
			QueueSize:       100,
			MaxRetries:      0,
			PayloadMaxBytes: 1 << 20,
		},
		API:                    APIConfig{Endpoint: server.URL},
		testGenerationExporter: exporter,
	})
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })

	ctx, recorder := client.StartGeneration(context.Background(), GenerationStart{
		ID:             "gen-stale",
		ConversationID: "conv-stale",
		Model:          ModelRef{Provider: "example", Name: "agent"},
	})
	recorder.SetResult(Generation{
		ID:             "gen-stale",
		ConversationID: "conv-stale",
		Model:          ModelRef{Provider: "example", Name: "agent"},
		Input:          []Message{UserTextMessage("stale question")},
		Output:         []Message{AssistantTextMessage("stale answer")},
	}, nil)
	recorder.End()

	trial := NewTrial(client, TrialRef{RunID: "run-stale", TestCaseID: "case-stale"})
	trial.BindGeneration("gen-stale", "conv-stale")
	trial.RecordIO(RecordIOOptions{Input: "fresh question", Output: "fresh answer", ModelProvider: "example", ModelName: "agent"})
	trial.FinalScore(BoolScoreValue(true), ScoreOptions{Evaluator: &Evaluator{EvaluatorID: "exact", Version: "1", Kind: EvaluatorKindDeterministic}})
	accepted, err := trial.Flush(ctx)
	if err != nil {
		t.Fatalf("flush trial: %v", err)
	}
	if accepted != 1 {
		t.Fatalf("expected one accepted score, got %d", accepted)
	}
	generations := exporter.generations("gen-stale")
	if len(generations) != 2 {
		t.Fatalf("expected stale and RecordIO generations, got %d", len(generations))
	}
	recorded := generations[1]
	if got := recorded.GetInput()[0].GetParts()[0].GetText(); got != "fresh question" {
		t.Fatalf("expected RecordIO input, got %q", got)
	}
	if got := recorded.GetOutput()[0].GetParts()[0].GetText(); got != "fresh answer" {
		t.Fatalf("expected RecordIO output, got %q", got)
	}
}

func TestTrialEndFlushesGenerationWithoutScores(t *testing.T) {
	exporter := &capturingExperimentExporter{}
	recorder := &experimentRecorder{}
	recorder.push(http.StatusOK, map[string]any{"trial_id": "trial-empty-flush"})
	recorder.push(http.StatusOK, map[string]any{"trial_id": "trial-empty-flush"})
	server := httptest.NewServer(recorder.handler(t))
	defer server.Close()

	client := NewClient(Config{
		Tracer: noop.NewTracerProvider().Tracer("agento11y-go-empty-trial-flush-test"),
		GenerationExport: GenerationExportConfig{
			Protocol:        GenerationExportProtocolHTTP,
			Endpoint:        server.URL + "/api/v1/generations:export",
			Auth:            AuthConfig{Mode: ExportAuthModeTenant, TenantID: "tenant-a"},
			Insecure:        BoolPtr(true),
			BatchSize:       10,
			FlushInterval:   time.Hour,
			QueueSize:       100,
			MaxRetries:      0,
			PayloadMaxBytes: 1 << 20,
		},
		API:                    APIConfig{Endpoint: server.URL},
		testGenerationExporter: exporter,
	})
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })

	trial := NewTrial(client, TrialRef{RunID: "run-empty-flush", TestCaseID: "case-empty-flush"})
	if err := trial.Start(context.Background()); err != nil {
		t.Fatalf("start trial: %v", err)
	}
	ctx, generationRecorder := client.StartGeneration(context.Background(), GenerationStart{
		ID:             "gen-empty-flush",
		ConversationID: "conv-empty-flush",
		Model:          ModelRef{Provider: "example", Name: "agent"},
	})
	generationRecorder.SetResult(Generation{
		ID:             "gen-empty-flush",
		ConversationID: "conv-empty-flush",
		Model:          ModelRef{Provider: "example", Name: "agent"},
		Input:          []Message{UserTextMessage("question")},
		Output:         []Message{AssistantTextMessage("answer")},
	}, nil)
	generationRecorder.End()

	if err := trial.End(ctx, nil); err != nil {
		t.Fatalf("end trial: %v", err)
	}
	if !exporter.hasGeneration("gen-empty-flush") {
		t.Fatal("expected generation to be flushed when trial ends without scores")
	}
	if recorder.requestCount() != 2 {
		t.Fatalf("expected trial create and update requests, got %d", recorder.requestCount())
	}
}

func TestExperimentContextTagsExistingInstrumentationAndCapturesIDs(t *testing.T) {
	exporter := &capturingExperimentExporter{}
	client := NewClient(Config{
		Tracer: noop.NewTracerProvider().Tracer("agento11y-go-experiment-context-test"),
		GenerationExport: GenerationExportConfig{
			Protocol:        GenerationExportProtocolHTTP,
			Endpoint:        "http://example.invalid/api/v1/generations:export",
			Auth:            AuthConfig{Mode: ExportAuthModeTenant, TenantID: "tenant-a"},
			Insecure:        BoolPtr(true),
			BatchSize:       10,
			FlushInterval:   time.Hour,
			QueueSize:       100,
			MaxRetries:      1,
			InitialBackoff:  time.Millisecond,
			MaxBackoff:      time.Millisecond,
			PayloadMaxBytes: 1 << 20,
		},
		testGenerationExporter: exporter,
	})
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })

	candidate := &Candidate{AgentName: "support-bot"}
	run := NewExperimentRun(ExperimentOptions{Client: client, RunID: "run_existing", Name: "existing", Candidate: candidate})
	ctx := WithConversationID(context.Background(), "ctx-conv")
	ctx = WithAgentName(ctx, "ctx-agent")
	ctx = run.Context(ctx)

	ctx, recorder := client.StartGeneration(ctx, GenerationStart{
		Model: ModelRef{Provider: "openai", Name: "gpt-5"},
	})
	recorder.SetResult(Generation{
		Input:  []Message{UserTextMessage("hello")},
		Output: []Message{AssistantTextMessage("world")},
	}, nil)
	recorder.End()
	if err := client.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	generation := exporter.firstGeneration()
	if generation == nil {
		t.Fatal("expected exported generation")
	}
	if generation.GetTags()[ExperimentRunIDTag] != "run_existing" {
		t.Fatalf("expected experiment run tag, got %#v", generation.GetTags())
	}
	if generation.GetMetadata().GetFields()[ExperimentRunIDMetadataKey].GetStringValue() != "run_existing" {
		t.Fatalf("expected experiment run metadata, got %#v", generation.GetMetadata())
	}
	if got := generation.GetConversationId(); got != "ctx-conv" {
		t.Fatalf("expected context conversation id to win, got %q", got)
	}
	if got := generation.GetAgentName(); got != "ctx-agent" {
		t.Fatalf("expected context agent name to win, got %q", got)
	}
	if ids := run.ProducedGenerationIDs(); len(ids) != 1 || ids[0] != generation.GetId() {
		t.Fatalf("unexpected captured ids: %#v", ids)
	}
}
