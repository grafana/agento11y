package sigil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	sigilv1 "github.com/grafana/sigil-sdk/go/proto/sigil/v1"
	"go.opentelemetry.io/otel/trace/noop"
)

type experimentRecordedRequest struct {
	Method  string
	Path    string
	Headers http.Header
	Payload map[string]any
}

type experimentRecorder struct {
	mu        sync.Mutex
	requests  []experimentRecordedRequest
	responses []experimentResponse
}

type experimentResponse struct {
	status int
	body   any
}

func (r *experimentRecorder) push(status int, body any) {
	r.responses = append(r.responses, experimentResponse{status: status, body: body})
}

func (r *experimentRecorder) handler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var payload map[string]any
		if req.Body != nil && req.ContentLength != 0 {
			if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}

		r.mu.Lock()
		r.requests = append(r.requests, experimentRecordedRequest{
			Method:  req.Method,
			Path:    req.URL.RequestURI(),
			Headers: req.Header.Clone(),
			Payload: payload,
		})
		response := r.responses[len(r.responses)-1]
		if len(r.responses) > 1 {
			response = r.responses[0]
			r.responses = r.responses[1:]
		}
		r.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(response.status)
		if response.body != nil {
			_ = json.NewEncoder(w).Encode(response.body)
		}
	})
}

func (r *experimentRecorder) request(i int) experimentRecordedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.requests[i]
}

func (r *experimentRecorder) requestCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests)
}

func experimentBody(overrides map[string]any) map[string]any {
	body := map[string]any{
		"tenant_id":   "tenant-a",
		"run_id":      "run_1",
		"name":        "PR 123",
		"source":      "external",
		"status":      "running",
		"score_count": float64(0),
		"created_at":  "2026-05-28T12:00:00Z",
		"updated_at":  "2026-05-28T12:00:00Z",
	}
	maps.Copy(body, overrides)
	return body
}

func newExperimentTestClient(t *testing.T, serverURL string) *Client {
	t.Helper()
	client := NewClient(Config{
		Tracer: noop.NewTracerProvider().Tracer("sigil-go-experiments-test"),
		GenerationExport: GenerationExportConfig{
			Protocol:        GenerationExportProtocolHTTP,
			Endpoint:        serverURL + "/api/v1/generations:export",
			Auth:            AuthConfig{Mode: ExportAuthModeTenant, TenantID: "tenant-a"},
			Insecure:        BoolPtr(true),
			BatchSize:       10,
			FlushInterval:   time.Hour,
			QueueSize:       100,
			MaxRetries:      2,
			InitialBackoff:  time.Millisecond,
			MaxBackoff:      time.Millisecond,
			PayloadMaxBytes: 1 << 20,
		},
		API:                    APIConfig{Endpoint: serverURL},
		testGenerationExporter: newNoopGenerationExporter(nil),
	})
	t.Cleanup(func() {
		_ = client.Shutdown(context.Background())
	})
	return client
}

func TestCreateExperimentRoundTrip(t *testing.T) {
	recorder := &experimentRecorder{}
	recorder.push(http.StatusOK, experimentBody(map[string]any{
		"tags":     []string{"smoke"},
		"metadata": map[string]any{"git_sha": "abc"},
	}))
	server := httptest.NewServer(recorder.handler(t))
	defer server.Close()

	client := newExperimentTestClient(t, server.URL)
	run, err := client.CreateExperiment(context.Background(), CreateExperimentRequest{
		RunID:    "run_1",
		Name:     "PR 123",
		Source:   ExperimentSourceExternal,
		Tags:     []string{"smoke"},
		Metadata: map[string]any{"git_sha": "abc"},
	})
	if err != nil {
		t.Fatalf("create experiment: %v", err)
	}

	req := recorder.request(0)
	if req.Method != http.MethodPost || req.Path != "/api/v1/eval/experiments" {
		t.Fatalf("unexpected request %s %s", req.Method, req.Path)
	}
	if got := req.Headers.Get("X-Scope-OrgID"); got != "tenant-a" {
		t.Fatalf("expected tenant header, got %q", got)
	}
	if req.Payload["run_id"] != "run_1" || req.Payload["name"] != "PR 123" || req.Payload["source"] != "external" {
		t.Fatalf("unexpected payload: %#v", req.Payload)
	}
	if run.RunID != "run_1" || run.Status != "running" || run.CreatedAt == nil {
		t.Fatalf("unexpected run: %#v", run)
	}
}

func TestCompleteExperimentSendsStatusPatch(t *testing.T) {
	recorder := &experimentRecorder{}
	recorder.push(http.StatusOK, experimentBody(map[string]any{"status": "succeeded", "score_count": float64(3)}))
	server := httptest.NewServer(recorder.handler(t))
	defer server.Close()

	client := newExperimentTestClient(t, server.URL)
	scoreCount := 3
	run, err := client.CompleteExperiment(context.Background(), "run_1", ExperimentStatusSucceeded, CompleteExperimentOptions{ScoreCount: &scoreCount})
	if err != nil {
		t.Fatalf("complete experiment: %v", err)
	}
	req := recorder.request(0)
	if req.Method != http.MethodPatch || req.Path != "/api/v1/eval/experiments/run_1" {
		t.Fatalf("unexpected request %s %s", req.Method, req.Path)
	}
	if req.Payload["status"] != "succeeded" || req.Payload["score_count"] != float64(3) {
		t.Fatalf("unexpected payload: %#v", req.Payload)
	}
	if run.Status != "succeeded" || run.ScoreCount != 3 {
		t.Fatalf("unexpected run: %#v", run)
	}
}

func TestExportScoresRoundTripAndAcceptedCount(t *testing.T) {
	recorder := &experimentRecorder{}
	recorder.push(http.StatusAccepted, map[string]any{
		"results": []map[string]any{
			{"score_id": "sc1", "accepted": true},
			{"score_id": "sc2", "accepted": false, "error": "bad"},
		},
	})
	server := httptest.NewServer(recorder.handler(t))
	defer server.Close()

	client := newExperimentTestClient(t, server.URL)
	passed := true
	response, err := client.ExportScores(context.Background(), []ScoreItem{
		{
			ScoreID:          "sc1",
			GenerationID:     "gen1",
			ConversationID:   "conv1",
			RunID:            "run_1",
			EvaluatorID:      "smoke.reward",
			EvaluatorVersion: "2026-05-28",
			ScoreKey:         "reward",
			Value:            NumberScoreValue(0.82),
			Passed:           &passed,
			Metadata:         map[string]any{"task_id": "t1"},
			Source:           &ScoreSource{Kind: "experiment", ID: "run_1"},
		},
		{
			ScoreID:          "sc2",
			GenerationID:     "gen2",
			RunID:            "run_1",
			EvaluatorID:      "smoke.reward",
			EvaluatorVersion: "2026-05-28",
			ScoreKey:         "pass",
			Value:            BoolScoreValue(true),
		},
	})
	if err != nil {
		t.Fatalf("export scores: %v", err)
	}
	req := recorder.request(0)
	if req.Path != "/api/v1/scores:export" {
		t.Fatalf("unexpected path: %s", req.Path)
	}
	scores := req.Payload["scores"].([]any)
	first := scores[0].(map[string]any)
	if first["run_id"] != "run_1" {
		t.Fatalf("expected run_id in score payload, got %#v", first)
	}
	if value := first["value"].(map[string]any); value["number"] != 0.82 {
		t.Fatalf("unexpected score value: %#v", value)
	}
	if response.AcceptedCount() != 1 || len(response.Rejected()) != 1 || response.Rejected()[0].ScoreID != "sc2" {
		t.Fatalf("unexpected response: %#v", response)
	}
}

func TestExperimentErrorsMapNotFoundAndConflict(t *testing.T) {
	recorder := &experimentRecorder{}
	recorder.push(http.StatusNotFound, map[string]any{"error": "missing"})
	recorder.push(http.StatusConflict, map[string]any{"error": "terminal"})
	server := httptest.NewServer(recorder.handler(t))
	defer server.Close()

	client := newExperimentTestClient(t, server.URL)
	if _, err := client.GetExperiment(context.Background(), "run_missing"); !errors.Is(err, ErrExperimentNotFound) {
		t.Fatalf("expected ErrExperimentNotFound, got %v", err)
	}
	status := ExperimentStatusRunning
	if _, err := client.UpdateExperiment(context.Background(), "run_1", UpdateExperimentRequest{Status: &status}); !errors.Is(err, ErrExperimentConflict) {
		t.Fatalf("expected ErrExperimentConflict, got %v", err)
	}
}

func TestExportScoresRetriesThenSucceedsOn5xx(t *testing.T) {
	recorder := &experimentRecorder{}
	recorder.push(http.StatusServiceUnavailable, map[string]any{"error": "unavailable"})
	recorder.push(http.StatusAccepted, map[string]any{"results": []map[string]any{{"score_id": "sc1", "accepted": true}}})
	server := httptest.NewServer(recorder.handler(t))
	defer server.Close()

	client := newExperimentTestClient(t, server.URL)
	response, err := client.ExportScores(context.Background(), []ScoreItem{{
		ScoreID:          "sc1",
		GenerationID:     "gen1",
		EvaluatorID:      "ev",
		EvaluatorVersion: "v1",
		ScoreKey:         "reward",
		Value:            NumberScoreValue(1),
	}})
	if err != nil {
		t.Fatalf("export scores: %v", err)
	}
	if response.AcceptedCount() != 1 {
		t.Fatalf("expected accepted count 1, got %d", response.AcceptedCount())
	}
	if recorder.requestCount() != 2 {
		t.Fatalf("expected one retry, got %d request(s)", recorder.requestCount())
	}
}

func TestEvalOverrideRoutesExperimentsViaProxyButScoresViaIngest(t *testing.T) {
	recorder := &experimentRecorder{}
	recorder.push(http.StatusOK, experimentBody(nil))
	recorder.push(http.StatusAccepted, map[string]any{"results": []map[string]any{{"score_id": "sc1", "accepted": true}}})
	server := httptest.NewServer(recorder.handler(t))
	defer server.Close()

	t.Setenv(envEvalEndpoint, server.URL)
	t.Setenv(envEvalPathPrefix, "/api/plugins/grafana-sigil-app/resources")
	t.Setenv(envEvalAuthToken, "glsa_test_token")

	client := newExperimentTestClient(t, server.URL)
	if _, err := client.CreateExperiment(context.Background(), CreateExperimentRequest{RunID: "run_1", Name: "cloud"}); err != nil {
		t.Fatalf("create experiment: %v", err)
	}
	createReq := recorder.request(0)
	if createReq.Path != "/api/plugins/grafana-sigil-app/resources/eval/experiments" {
		t.Fatalf("unexpected create path: %s", createReq.Path)
	}
	if got := createReq.Headers.Get("Authorization"); got != "Bearer glsa_test_token" {
		t.Fatalf("expected eval bearer token, got %q", got)
	}

	if _, err := client.ExportScores(context.Background(), []ScoreItem{{
		ScoreID:          "sc1",
		GenerationID:     "gen1",
		RunID:            "run_1",
		EvaluatorID:      "ev",
		EvaluatorVersion: "v1",
		ScoreKey:         "reward",
		Value:            NumberScoreValue(1),
	}}); err != nil {
		t.Fatalf("export scores: %v", err)
	}
	scoreReq := recorder.request(1)
	if scoreReq.Path != "/api/v1/scores:export" {
		t.Fatalf("unexpected score path: %s", scoreReq.Path)
	}
	if got := scoreReq.Headers.Get("X-Scope-OrgID"); got != "tenant-a" {
		t.Fatalf("expected tenant header, got %q", got)
	}
	if got := scoreReq.Headers.Get("Authorization"); got != "" {
		t.Fatalf("score export should not use eval bearer token, got %q", got)
	}
}

func TestGetExperimentReportParsesSummary(t *testing.T) {
	recorder := &experimentRecorder{}
	recorder.push(http.StatusOK, map[string]any{
		"run": experimentBody(map[string]any{"status": "succeeded"}),
		"summary": map[string]any{
			"n_conversations": float64(2),
			"n_generations":   float64(3),
			"n_scores":        float64(3),
			"pass_rate":       0.66,
			"mean_score":      0.8,
			"total_cost_usd":  0.5,
			"total_tokens":    float64(1200),
		},
		"breakdowns": map[string]any{"by_task": []map[string]any{{"key": "t1", "count": float64(2)}}},
		"points":     []map[string]any{{"score_id": "sc1"}},
	})
	server := httptest.NewServer(recorder.handler(t))
	defer server.Close()

	client := newExperimentTestClient(t, server.URL)
	report, err := client.GetExperimentReport(context.Background(), "run_1")
	if err != nil {
		t.Fatalf("get report: %v", err)
	}
	if recorder.request(0).Path != "/api/v1/eval/experiments/run_1/report" {
		t.Fatalf("unexpected path: %s", recorder.request(0).Path)
	}
	if report.Run.Status != "succeeded" || report.Summary.NGenerations != 3 || report.Summary.TotalTokens != 1200 {
		t.Fatalf("unexpected report: %#v", report)
	}
}

type capturingExperimentExporter struct {
	mu       sync.Mutex
	requests []*sigilv1.ExportGenerationsRequest
}

func (e *capturingExperimentExporter) Export(_ context.Context, request *sigilv1.ExportGenerationsRequest) (*sigilv1.ExportGenerationsResponse, error) {
	e.mu.Lock()
	e.requests = append(e.requests, request)
	e.mu.Unlock()
	response := &sigilv1.ExportGenerationsResponse{}
	for _, generation := range request.GetGenerations() {
		response.Results = append(response.Results, &sigilv1.ExportGenerationResult{
			GenerationId: generation.GetId(),
			Accepted:     true,
		})
	}
	return response, nil
}

func (e *capturingExperimentExporter) Shutdown(context.Context) error { return nil }

func (e *capturingExperimentExporter) firstGeneration() *sigilv1.Generation {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.requests) == 0 || len(e.requests[0].GetGenerations()) == 0 {
		return nil
	}
	return e.requests[0].GetGenerations()[0]
}

func TestExperimentContextTagsExistingInstrumentationAndCapturesIDs(t *testing.T) {
	exporter := &capturingExperimentExporter{}
	client := NewClient(Config{
		Tracer: noop.NewTracerProvider().Tracer("sigil-go-experiment-context-test"),
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

	run := NewExperimentRun(ExperimentOptions{
		Client:        client,
		RunID:         "run_existing",
		Name:          "existing",
		Upload:        UploadModeContinuous,
		AgentName:     "support-bot",
		ExtraTags:     map[string]string{"suite": "smoke"},
		ExtraMetadata: map[string]any{"candidate": "abc"},
	})
	ctx := run.Context(context.Background())

	ctx, recorder := client.StartGeneration(ctx, GenerationStart{
		Model: sigilModelRef("openai", "gpt-5"),
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
	if ids := run.ProducedGenerationIDs(); len(ids) != 1 || ids[0] != generation.GetId() {
		t.Fatalf("unexpected captured ids: %#v", ids)
	}
}

func TestExperimentRunnerExportsScoresAndFinalizesSucceeded(t *testing.T) {
	recorder := &experimentRecorder{}
	recorder.push(http.StatusOK, experimentBody(map[string]any{"run_id": "run_1"}))
	recorder.push(http.StatusAccepted, map[string]any{"results": []map[string]any{{"score_id": "score-1", "accepted": true}}})
	recorder.push(http.StatusOK, experimentBody(map[string]any{"run_id": "run_1", "status": "succeeded", "score_count": float64(1)}))
	server := httptest.NewServer(recorder.handler(t))
	defer server.Close()

	client := newExperimentTestClient(t, server.URL)
	runner := ExperimentRunner{
		Client:      client,
		RunID:       "run_1",
		Name:        "smoke",
		Dataset:     map[string]any{"id": "support_smoke", "version": "2026-05-28"},
		Candidate:   map[string]any{"git_sha": "abc123"},
		Tags:        []string{"smoke"},
		Upload:      UploadModeContinuous,
		FetchReport: false,
	}
	result, err := runner.Run(
		context.Background(),
		[]DatasetItem{{ID: "it1", Input: "2+2", Expected: "4", Metadata: map[string]any{"task_id": "math"}}},
		func(_ context.Context, item DatasetItem) (TargetResult, error) {
			return TargetResult{Output: item.Expected, GenerationIDs: []string{"gen-it1"}, ConversationID: "conv-it1"}, nil
		},
		[]DatasetScorer{
			func(_ context.Context, item DatasetItem, result TargetResult) ([]ScoreOutput, error) {
				passed := result.Output == item.Expected
				return []ScoreOutput{{
					EvaluatorID:      "smoke.reward",
					EvaluatorVersion: "2026-05-28",
					ScoreKey:         "reward",
					Value:            NumberScoreValue(1),
					Passed:           &passed,
					Metadata:         map[string]any{"task_id": item.Metadata["task_id"]},
				}}, nil
			},
		},
	)
	if err != nil {
		t.Fatalf("run experiment: %v", err)
	}
	if result.RunID != "run_1" || result.AcceptedScores != 1 || !strings.Contains(result.URL, "run_1") {
		t.Fatalf("unexpected result: %#v", result)
	}
	createReq := recorder.request(0)
	if createReq.Payload["source"] != "external" || createReq.Payload["run_id"] != "run_1" {
		t.Fatalf("unexpected create payload: %#v", createReq.Payload)
	}
	scoreReq := recorder.request(1)
	scores := scoreReq.Payload["scores"].([]any)
	score := scores[0].(map[string]any)
	if score["run_id"] != "run_1" || score["generation_id"] != "gen-it1" || score["conversation_id"] != "conv-it1" {
		t.Fatalf("unexpected score payload: %#v", score)
	}
	metadata := score["metadata"].(map[string]any)
	if metadata["dataset_id"] != "support_smoke" || metadata["item_id"] != "it1" {
		t.Fatalf("unexpected score metadata: %#v", metadata)
	}
	completeReq := recorder.request(2)
	if completeReq.Method != http.MethodPatch || completeReq.Payload["status"] != "succeeded" || completeReq.Payload["score_count"] != float64(1) {
		t.Fatalf("unexpected complete request: %#v", completeReq)
	}
}

func TestExperimentRunnerPassesContextForExistingInstrumentation(t *testing.T) {
	recorder := &experimentRecorder{}
	recorder.push(http.StatusOK, experimentBody(map[string]any{"run_id": "run_ctx"}))
	recorder.push(http.StatusAccepted, map[string]any{"results": []map[string]any{{"score_id": "score-1", "accepted": true}}})
	recorder.push(http.StatusOK, experimentBody(map[string]any{"run_id": "run_ctx", "status": "succeeded", "score_count": float64(1)}))
	server := httptest.NewServer(recorder.handler(t))
	defer server.Close()

	client := newExperimentTestClient(t, server.URL)
	runner := ExperimentRunner{
		Client:      client,
		RunID:       "run_ctx",
		Name:        "context",
		Upload:      UploadModeContinuous,
		FetchReport: false,
	}
	_, err := runner.Run(
		context.Background(),
		[]DatasetItem{{ID: "it1", Input: "question", Expected: "answer"}},
		func(ctx context.Context, item DatasetItem) (TargetResult, error) {
			_, rec := client.StartGeneration(ctx, GenerationStart{Model: sigilModelRef("example", "existing-agent")})
			rec.SetResult(Generation{
				Input:  []Message{UserTextMessage(fmt.Sprint(item.Input))},
				Output: []Message{AssistantTextMessage(fmt.Sprint(item.Expected))},
			}, nil)
			rec.End()
			return TargetResult{Output: item.Expected}, nil
		},
		[]DatasetScorer{
			func(context.Context, DatasetItem, TargetResult) ([]ScoreOutput, error) {
				passed := true
				return []ScoreOutput{{
					EvaluatorID:      "example.exact_match",
					EvaluatorVersion: "2026-06-02",
					ScoreKey:         "exact_match",
					Value:            NumberScoreValue(1),
					Passed:           &passed,
				}}, nil
			},
		},
	)
	if err != nil {
		t.Fatalf("run experiment: %v", err)
	}
	scoreReq := recorder.request(1)
	score := scoreReq.Payload["scores"].([]any)[0].(map[string]any)
	if score["generation_id"] == "" {
		t.Fatalf("expected score to attach to captured generation id: %#v", score)
	}
	if score["conversation_id"] != StableID("conv", "run_ctx", "it1") {
		t.Fatalf("unexpected score conversation id: %#v", score)
	}
}

func sigilModelRef(provider string, name string) ModelRef {
	return ModelRef{Provider: provider, Name: name}
}
