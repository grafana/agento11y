package sigil

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

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
		"tenant_id":     "tenant-a",
		"experiment_id": "run_1",
		"name":          "PR 123",
		"source":        "external",
		"status":        "running",
		"score_count":   float64(0),
		"created_at":    "2026-05-28T12:00:00Z",
		"updated_at":    "2026-05-28T12:00:00Z",
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

func TestCreateExperimentUpsertsExternalRun(t *testing.T) {
	recorder := &experimentRecorder{}
	recorder.push(http.StatusOK, map[string]any{"run": experimentBody(map[string]any{
		"tags":     []string{"smoke"},
		"metadata": map[string]any{"git_sha": "abc"},
	})})
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
	if req.Method != http.MethodPost || req.Path != "/api/v1/experiment-runs:upsert" {
		t.Fatalf("unexpected request %s %s", req.Method, req.Path)
	}
	if got := req.Headers.Get("X-Scope-OrgID"); got != "tenant-a" {
		t.Fatalf("expected tenant header, got %q", got)
	}
	if req.Payload["experiment_id"] != "run_1" || req.Payload["name"] != "PR 123" {
		t.Fatalf("unexpected payload: %#v", req.Payload)
	}
	if _, ok := req.Payload["run_id"]; ok {
		t.Fatalf("upsert payload must not send run_id: %#v", req.Payload)
	}
	source := req.Payload["source"].(map[string]any)
	if source["kind"] != "sdk" || source["id"] != "go" {
		t.Fatalf("unexpected source: %#v", source)
	}
	if run.RunID != "run_1" || run.Status != "running" || run.CreatedAt == nil {
		t.Fatalf("unexpected run: %#v", run)
	}
}

func TestFinalizeExperimentPostsCompleted(t *testing.T) {
	recorder := &experimentRecorder{}
	recorder.push(http.StatusOK, map[string]any{"run": experimentBody(map[string]any{"status": "completed", "score_count": float64(3)})})
	server := httptest.NewServer(recorder.handler(t))
	defer server.Close()

	client := newExperimentTestClient(t, server.URL)
	scoreCount := 3
	run, err := client.FinalizeExperiment(context.Background(), "run_1", ExperimentStatusSucceeded, CompleteExperimentOptions{ScoreCount: &scoreCount})
	if err != nil {
		t.Fatalf("finalize experiment: %v", err)
	}
	req := recorder.request(0)
	if req.Method != http.MethodPost || req.Path != "/api/v1/experiment-runs/run_1:finalize" {
		t.Fatalf("unexpected request %s %s", req.Method, req.Path)
	}
	if req.Payload["status"] != "completed" || req.Payload["score_count"] != float64(3) {
		t.Fatalf("unexpected payload: %#v", req.Payload)
	}
	if run.Status != "completed" || run.ScoreCount != 3 {
		t.Fatalf("unexpected run: %#v", run)
	}
}

func TestExportScoresUsesExperimentIDAndTrialID(t *testing.T) {
	recorder := &experimentRecorder{}
	recorder.push(http.StatusAccepted, map[string]any{
		"results": []map[string]any{
			{"score_id": "sc1", "accepted": true, "status": "accepted"},
			{"score_id": "sc2", "accepted": false, "status": "duplicate"},
			{"score_id": "sc3", "accepted": false, "status": "rejected", "error": "bad"},
		},
	})
	server := httptest.NewServer(recorder.handler(t))
	defer server.Close()

	client := newExperimentTestClient(t, server.URL)
	passed := true
	response, err := client.ExportScores(context.Background(), []ScoreItem{
		{
			ScoreID:          "sc1",
			TrialID:          "trial-1",
			TestCaseID:       "case-1",
			RunID:            "run_1",
			EvaluatorID:      "smoke.reward",
			EvaluatorVersion: "2026-05-28",
			EvaluatorKind:    "deterministic",
			ScoreKey:         "reward",
			Value:            NumberScoreValue(0.82),
			Passed:           &passed,
			Metadata:         map[string]any{"task_id": "case-1"},
			Source:           &ScoreSource{Kind: "experiment", ID: "run_1"},
		},
		{
			ScoreID:          "sc2",
			TrialID:          "trial-1",
			RunID:            "run_1",
			EvaluatorID:      "smoke.reward",
			EvaluatorVersion: "2026-05-28",
			ScoreKey:         "pass",
			Value:            BoolScoreValue(true),
		},
		{
			ScoreID:          "sc3",
			TrialID:          "trial-1",
			RunID:            "run_1",
			EvaluatorID:      "smoke.reward",
			EvaluatorVersion: "2026-05-28",
			ScoreKey:         "bad",
			Value:            StringScoreValue("bad"),
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
	if first["experiment_id"] != "run_1" || first["trial_id"] != "trial-1" || first["test_case_id"] != "case-1" {
		t.Fatalf("unexpected score payload: %#v", first)
	}
	if _, ok := first["run_id"]; ok {
		t.Fatalf("score payload must not send run_id: %#v", first)
	}
	if value := first["value"].(map[string]any); value["number"] != 0.82 {
		t.Fatalf("unexpected score value: %#v", value)
	}
	if response.AcceptedCount() != 1 || response.DuplicateCount() != 1 || len(response.Rejected()) != 1 || response.Rejected()[0].ScoreID != "sc3" {
		t.Fatalf("unexpected response: %#v", response)
	}
}

func TestAcceptedOrErrorDoesNotCountDuplicatesAsAccepted(t *testing.T) {
	accepted, err := acceptedOrError(&ExportScoresResponse{
		Results: []ExportScoreResult{
			{ScoreID: "sc1", Accepted: true, Status: "accepted"},
			{ScoreID: "sc2", Accepted: false, Status: "duplicate"},
		},
	})
	if err != nil {
		t.Fatalf("accepted or error: %v", err)
	}
	if accepted != 1 {
		t.Fatalf("expected only newly accepted scores to count, got %d", accepted)
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
	if _, err := client.FinalizeExperiment(context.Background(), "run_1", ExperimentStatusCompleted, CompleteExperimentOptions{}); !errors.Is(err, ErrExperimentConflict) {
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
		TrialID:          "trial-1",
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

func TestExportScoresDoesNotRetryWithoutEvaluatorKind(t *testing.T) {
	recorder := &experimentRecorder{}
	recorder.push(http.StatusBadRequest, map[string]any{"error": `json: unknown field "evaluator_kind"`})
	server := httptest.NewServer(recorder.handler(t))
	defer server.Close()

	client := newExperimentTestClient(t, server.URL)
	_, err := client.ExportScores(context.Background(), []ScoreItem{{
		ScoreID:          "sc1",
		TrialID:          "trial-1",
		EvaluatorID:      "ev",
		EvaluatorVersion: "v1",
		EvaluatorKind:    "deterministic",
		ScoreKey:         "reward",
		Value:            NumberScoreValue(1),
	}})
	if !errors.Is(err, ErrScoreValidationFailed) {
		t.Fatalf("expected validation error, got %v", err)
	}
	if recorder.requestCount() != 1 {
		t.Fatalf("expected no evaluator_kind fallback retry, got %d request(s)", recorder.requestCount())
	}
	first := recorder.request(0).Payload["scores"].([]any)[0].(map[string]any)
	if first["evaluator_kind"] != "deterministic" {
		t.Fatalf("expected first request to include evaluator_kind, got %#v", first)
	}
}

func TestGetExperimentReportParsesTypedTrialSummary(t *testing.T) {
	recorder := &experimentRecorder{}
	recorder.push(http.StatusOK, map[string]any{
		"experiment": experimentBody(map[string]any{"status": "completed"}),
		"summary": map[string]any{
			"test_case_count": float64(2),
			"trial_count":     float64(3),
			"completed_count": float64(3),
			"pass_rate":       0.66,
			"pass_at_k":       map[string]float64{"1": 0.66},
			"pass_power_k":    map[string]float64{"1": 0.66},
			"final_score_avg": 0.8,
			"total_cost":      0.5,
			"total_tokens":    float64(1200),
		},
		"rows": []map[string]any{{
			"test_case_id": "t1",
			"test_case_snapshot": map[string]any{
				"test_case_id": "t1",
				"name":         "Case 1",
				"input":        "2+2",
				"expected":     "4",
			},
			"summary": map[string]any{
				"trial_count":     float64(1),
				"completed_count": float64(1),
				"pass_at_k":       map[string]bool{"1": true},
				"trial_pass_rate": 1.0,
			},
			"trials": []map[string]any{{
				"trial": map[string]any{
					"trial_id":      "trial-1",
					"experiment_id": "run_1",
					"test_case_id":  "t1",
					"attempt":       float64(1),
					"status":        "completed",
				},
				"final_score": map[string]any{
					"score_id":          "score-final",
					"evaluator_id":      "exact",
					"evaluator_version": "1",
					"score_key":         "final",
					"score_type":        "number",
					"value":             map[string]any{"number": 1.0},
					"passed":            true,
				},
				"scores": []map[string]any{{
					"score_id":          "score-final",
					"evaluator_id":      "exact",
					"evaluator_version": "1",
					"score_key":         "final",
					"score_type":        "number",
					"value":             map[string]any{"number": 1.0},
				}},
				"artifacts": []map[string]any{{
					"artifact_id": "artifact-1",
					"parent_kind": "test_case_trial",
					"parent_id":   "trial-1",
					"name":        "output",
					"kind":        "json",
				}},
			}},
		}},
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
	if report.Run.Status != "completed" || report.Summary.TestCaseCount != 2 || report.Summary.TotalTokens != 1200 || len(report.Rows) != 1 {
		t.Fatalf("unexpected report: %#v", report)
	}
	row := report.Rows[0]
	if row.TestCaseID != "t1" || row.TestCaseSnapshot == nil || row.TestCaseSnapshot.Name != "Case 1" || row.TestCaseSnapshot.Input != "2+2" || row.TestCaseSnapshot.Expected != "4" {
		t.Fatalf("unexpected typed row: %#v", row)
	}
	if len(row.Trials) != 1 || row.Trials[0].Trial.TrialID != "trial-1" || row.Trials[0].FinalScore == nil {
		t.Fatalf("unexpected typed trial result: %#v", row.Trials)
	}
	if row.Trials[0].FinalScore.Value.Number == nil || *row.Trials[0].FinalScore.Value.Number != 1 {
		t.Fatalf("unexpected typed final score: %#v", row.Trials[0].FinalScore)
	}
	if len(row.Trials[0].Artifacts) != 1 || row.Trials[0].Artifacts[0].ArtifactID != "artifact-1" {
		t.Fatalf("unexpected typed artifacts: %#v", row.Trials[0].Artifacts)
	}
}

func TestListExperimentScoresParsesTypedScores(t *testing.T) {
	recorder := &experimentRecorder{}
	recorder.push(http.StatusOK, map[string]any{
		"items": []map[string]any{{
			"tenant_id":         "tenant-a",
			"score_id":          "score-1",
			"generation_id":     "gen-1",
			"experiment_id":     "run_1",
			"trial_id":          "trial-1",
			"test_case_id":      "case-1",
			"evaluator_id":      "exact",
			"evaluator_version": "1",
			"score_key":         "final",
			"score_type":        "number",
			"value":             map[string]any{"number": 0.75},
			"passed":            true,
			"source_kind":       "experiment",
			"source_id":         "run_1",
			"agent_name":        "agent",
			"effective_version": "v1",
		}},
		"next_cursor": "42",
	})
	server := httptest.NewServer(recorder.handler(t))
	defer server.Close()

	client := newExperimentTestClient(t, server.URL)
	response, err := client.ListExperimentScores(context.Background(), "run_1", 25, "")
	if err != nil {
		t.Fatalf("list scores: %v", err)
	}
	if recorder.request(0).Path != "/api/v1/eval/experiments/run_1/scores?limit=25" {
		t.Fatalf("unexpected path: %s", recorder.request(0).Path)
	}
	if response.NextCursor != "42" || len(response.Items) != 1 {
		t.Fatalf("unexpected score list: %#v", response)
	}
	score := response.Items[0]
	if score.ScoreID != "score-1" || score.ScoreType != ScoreTypeNumber || score.Value.Number == nil || *score.Value.Number != 0.75 {
		t.Fatalf("unexpected typed score: %#v", score)
	}
}
