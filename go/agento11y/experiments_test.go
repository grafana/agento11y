package agento11y

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
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
		Tracer: noop.NewTracerProvider().Tracer("agento11y-go-experiments-test"),
		GenerationExport: GenerationExportConfig{
			Protocol:        GenerationExportProtocolHTTP,
			Endpoint:        serverURL + "/api/v1/generations:export",
			Auth:            AuthConfig{Mode: ExportAuthModeTenant, TenantID: "tenant-a"},
			Insecure:        new(true),
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

func TestExperimentURLTemplateAliases(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			name: "preferred template wins over legacy",
			env: map[string]string{
				envExperimentURLTemplatePreferred: "https://preferred.example/{run_id}",
				envExperimentURLTemplate:          "https://legacy.example/{run_id}",
			},
			want: "https://preferred.example/run-1",
		},
		{
			name: "legacy template still honored",
			env:  map[string]string{envExperimentURLTemplate: "https://legacy.example/{run_id}"},
			want: "https://legacy.example/run-1",
		},
		{
			name: "blank preferred falls through to legacy",
			env: map[string]string{
				envExperimentURLTemplatePreferred: "   ",
				envExperimentURLTemplate:          "https://legacy.example/{run_id}",
			},
			want: "https://legacy.example/run-1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envExperimentURLTemplatePreferred, "")
			t.Setenv(envExperimentURLTemplate, "")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			client := newExperimentTestClient(t, "https://server.example")
			if got := client.ExperimentURL("run-1"); got != tc.want {
				t.Fatalf("ExperimentURL=%q want %q", got, tc.want)
			}
		})
	}
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

func TestCreateExperimentDecodesSourceObject(t *testing.T) {
	recorder := &experimentRecorder{}
	recorder.push(http.StatusOK, map[string]any{"run": experimentBody(map[string]any{
		"source": map[string]any{"kind": "sdk", "id": "go"},
	})})
	server := httptest.NewServer(recorder.handler(t))
	defer server.Close()

	client := newExperimentTestClient(t, server.URL)
	run, err := client.CreateExperiment(context.Background(), CreateExperimentRequest{
		RunID:  "run_1",
		Name:   "PR 123",
		Source: ExperimentSourceExternal,
	})
	if err != nil {
		t.Fatalf("create experiment: %v", err)
	}
	if run.Source != "sdk" {
		t.Fatalf("expected source kind from object response, got %q", run.Source)
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

func TestAcceptedOrErrorFailsAggregateRejections(t *testing.T) {
	accepted, err := acceptedOrError(&ExportScoresResponse{
		Accepted:      1,
		RejectedCount: 2,
	})
	if !errors.Is(err, ErrScoreExportFailed) {
		t.Fatalf("expected score export failure, got accepted=%d err=%v", accepted, err)
	}
	if accepted != 0 {
		t.Fatalf("expected no accepted count on rejection, got %d", accepted)
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

func TestExportScoresOmitsEvaluatorKindFromWire(t *testing.T) {
	recorder := &experimentRecorder{}
	recorder.push(http.StatusAccepted, map[string]any{"results": []map[string]any{{"score_id": "sc1", "accepted": true}}})
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
	if err != nil {
		t.Fatalf("export scores: %v", err)
	}
	if recorder.requestCount() != 1 {
		t.Fatalf("expected one request, got %d", recorder.requestCount())
	}
	first := recorder.request(0).Payload["scores"].([]any)[0].(map[string]any)
	if _, exists := first["evaluator_kind"]; exists {
		t.Fatalf("evaluator_kind is not part of the score ingest contract, got %#v", first)
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

func trialEvaluationBody(overrides map[string]any) map[string]any {
	body := map[string]any{
		"evaluation_id":     "teval-1",
		"experiment_id":     "exp-1",
		"trial_id":          "trial-1",
		"test_case_id":      "case-1",
		"conversation_id":   "conv-1",
		"evaluator_id":      "helpfulness",
		"evaluator_version": "v3",
		"status":            "queued",
		"attempts":          float64(0),
		"scheduled_at":      "2026-07-23T21:30:00Z",
		"created_at":        "2026-07-23T21:30:00Z",
		"updated_at":        "2026-07-23T21:30:00Z",
	}
	maps.Copy(body, overrides)
	return body
}

func TestTriggerTrialEvaluationSendsAdmittedKeysOnly(t *testing.T) {
	cases := []struct {
		name        string
		request     TriggerTrialEvaluationRequest
		wantPayload map[string]any
	}{
		{
			name:        "unpinned version omits the key",
			request:     TriggerTrialEvaluationRequest{EvaluatorID: "helpfulness"},
			wantPayload: map[string]any{"evaluator_id": "helpfulness"},
		},
		{
			name:        "pinned version is sent as given",
			request:     TriggerTrialEvaluationRequest{EvaluatorID: "helpfulness", EvaluatorVersion: "v3"},
			wantPayload: map[string]any{"evaluator_id": "helpfulness", "evaluator_version": "v3"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := &experimentRecorder{}
			recorder.push(http.StatusAccepted, trialEvaluationBody(nil))
			server := httptest.NewServer(recorder.handler(t))
			defer server.Close()

			client := newExperimentTestClient(t, server.URL)
			evaluation, err := client.TriggerTrialEvaluation(context.Background(), "exp-1", "trial-1", tc.request)
			if err != nil {
				t.Fatalf("trigger trial evaluation: %v", err)
			}
			req := recorder.request(0)
			if req.Method != http.MethodPost || req.Path != "/api/v1/experiment-runs/exp-1/trials/trial-1:evaluate" {
				t.Fatalf("unexpected request %s %s", req.Method, req.Path)
			}
			if !maps.Equal(payloadStrings(t, req.Payload), tc.wantPayload) {
				t.Fatalf("unexpected trigger body: %#v want %#v", req.Payload, tc.wantPayload)
			}
			if evaluation.EvaluationID != "teval-1" || evaluation.Status != TrialEvaluationStatusQueued {
				t.Fatalf("unexpected evaluation: %#v", evaluation)
			}
			if evaluation.Status.Terminal() {
				t.Fatal("queued evaluation must not be terminal")
			}
			if evaluation.CreatedAt == nil || evaluation.ScheduledAt == nil {
				t.Fatalf("expected parsed timestamps, got %#v", evaluation)
			}
		})
	}
}

// payloadStrings narrows a decoded JSON body to its string fields so a payload
// can be compared key by key.
func payloadStrings(t *testing.T, payload map[string]any) map[string]any {
	t.Helper()
	out := make(map[string]any, len(payload))
	for key, value := range payload {
		text, ok := value.(string)
		if !ok {
			t.Fatalf("unexpected non-string field %q: %#v", key, value)
		}
		out[key] = text
	}
	return out
}

func TestTrialEvaluationRoutesEscapeColonInTrialID(t *testing.T) {
	recorder := &experimentRecorder{}
	recorder.push(http.StatusAccepted, trialEvaluationBody(map[string]any{"trial_id": "trial:one"}))
	server := httptest.NewServer(recorder.handler(t))
	defer server.Close()

	client := newExperimentTestClient(t, server.URL)
	if _, err := client.TriggerTrialEvaluation(context.Background(), "exp-1", "trial:one", TriggerTrialEvaluationRequest{
		EvaluatorID: "helpfulness",
	}); err != nil {
		t.Fatalf("trigger trial evaluation: %v", err)
	}
	if got := recorder.request(0).Path; got != "/api/v1/experiment-runs/exp-1/trials/trial%3Aone:evaluate" {
		t.Fatalf("colon in a trial id must be percent-encoded, got %s", got)
	}
	if _, err := client.GetTrialEvaluation(context.Background(), "exp-1", "trial:one", "teval-1"); err != nil {
		t.Fatalf("get trial evaluation: %v", err)
	}
	if got := recorder.request(1).Path; got != "/api/v1/experiment-runs/exp-1/trials/trial%3Aone/evaluations/teval-1" {
		t.Fatalf("colon in a trial id must be percent-encoded, got %s", got)
	}
}

func TestGetTrialEvaluationDecodesWorkerStatus(t *testing.T) {
	cases := []struct {
		name      string
		overrides map[string]any
		wantErr   error
		errText   string
		assert    func(*testing.T, *TrialEvaluation)
	}{
		{
			name:      "success is terminal",
			overrides: map[string]any{"status": "success", "attempts": float64(1)},
			assert: func(t *testing.T, evaluation *TrialEvaluation) {
				if evaluation.Status != TrialEvaluationStatusSuccess || !evaluation.Status.Terminal() || evaluation.Attempts != 1 {
					t.Fatalf("unexpected evaluation: %#v", evaluation)
				}
			},
		},
		{
			name:      "failure carries the worker error",
			overrides: map[string]any{"status": "failed", "error": "budget exhausted"},
			assert: func(t *testing.T, evaluation *TrialEvaluation) {
				if !evaluation.Status.Terminal() || evaluation.Error != "budget exhausted" {
					t.Fatalf("unexpected evaluation: %#v", evaluation)
				}
			},
		},
		{
			name:      "claimed keeps polling",
			overrides: map[string]any{"status": "claimed"},
			assert: func(t *testing.T, evaluation *TrialEvaluation) {
				if evaluation.Status != TrialEvaluationStatusClaimed || evaluation.Status.Terminal() {
					t.Fatalf("unexpected evaluation: %#v", evaluation)
				}
			},
		},
		{
			name:      "unsupported status is rejected",
			overrides: map[string]any{"status": "abandoned"},
			wantErr:   ErrExperimentTransportFailed,
			errText:   "abandoned",
		},
		{
			name:      "missing evaluation id is rejected",
			overrides: map[string]any{"evaluation_id": ""},
			wantErr:   ErrExperimentTransportFailed,
			errText:   "evaluation_id",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := &experimentRecorder{}
			recorder.push(http.StatusOK, trialEvaluationBody(tc.overrides))
			server := httptest.NewServer(recorder.handler(t))
			defer server.Close()

			client := newExperimentTestClient(t, server.URL)
			evaluation, err := client.GetTrialEvaluation(context.Background(), "exp-1", "trial-1", "teval-1")
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("expected %v, got %v", tc.wantErr, err)
				}
				if !strings.Contains(err.Error(), tc.errText) {
					t.Fatalf("expected %q in the error, got %v", tc.errText, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("get trial evaluation: %v", err)
			}
			req := recorder.request(0)
			if req.Method != http.MethodGet || req.Path != "/api/v1/experiment-runs/exp-1/trials/trial-1/evaluations/teval-1" {
				t.Fatalf("unexpected request %s %s", req.Method, req.Path)
			}
			if req.Payload != nil || req.Headers.Get("Content-Type") != "" {
				t.Fatalf("status reads must send no body, got payload %#v content type %q", req.Payload, req.Headers.Get("Content-Type"))
			}
			tc.assert(t, evaluation)
		})
	}
}

func TestTrialEvaluationValidatesIdentifiers(t *testing.T) {
	recorder := &experimentRecorder{}
	recorder.push(http.StatusAccepted, trialEvaluationBody(nil))
	server := httptest.NewServer(recorder.handler(t))
	defer server.Close()

	client := newExperimentTestClient(t, server.URL)
	cases := []struct {
		name string
		call func() error
	}{
		{
			name: "trigger without experiment id",
			call: func() error {
				_, err := client.TriggerTrialEvaluation(context.Background(), "  ", "trial-1", TriggerTrialEvaluationRequest{EvaluatorID: "helpfulness"})
				return err
			},
		},
		{
			name: "trigger without trial id",
			call: func() error {
				_, err := client.TriggerTrialEvaluation(context.Background(), "exp-1", "", TriggerTrialEvaluationRequest{EvaluatorID: "helpfulness"})
				return err
			},
		},
		{
			name: "trigger without evaluator id",
			call: func() error {
				_, err := client.TriggerTrialEvaluation(context.Background(), "exp-1", "trial-1", TriggerTrialEvaluationRequest{EvaluatorID: " "})
				return err
			},
		},
		{
			name: "status without experiment id",
			call: func() error {
				_, err := client.GetTrialEvaluation(context.Background(), "", "trial-1", "teval-1")
				return err
			},
		},
		{
			name: "status without trial id",
			call: func() error {
				_, err := client.GetTrialEvaluation(context.Background(), "exp-1", "", "teval-1")
				return err
			},
		},
		{
			name: "status without evaluation id",
			call: func() error {
				_, err := client.GetTrialEvaluation(context.Background(), "exp-1", "trial-1", " ")
				return err
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, ErrExperimentValidationFailed) {
				t.Fatalf("expected ErrExperimentValidationFailed, got %v", err)
			}
		})
	}
	if recorder.requestCount() != 0 {
		t.Fatalf("validation must not issue a request, got %d", recorder.requestCount())
	}
}

func TestTrialEvaluationMapsNotFoundAndConflict(t *testing.T) {
	recorder := &experimentRecorder{}
	recorder.push(http.StatusNotFound, map[string]any{"error": "missing trial"})
	recorder.push(http.StatusConflict, map[string]any{"error": "experiment is no longer running"})
	server := httptest.NewServer(recorder.handler(t))
	defer server.Close()

	client := newExperimentTestClient(t, server.URL)
	if _, err := client.GetTrialEvaluation(context.Background(), "exp-1", "trial-1", "teval-missing"); !errors.Is(err, ErrExperimentNotFound) {
		t.Fatalf("expected ErrExperimentNotFound, got %v", err)
	}
	if _, err := client.TriggerTrialEvaluation(context.Background(), "exp-1", "trial-1", TriggerTrialEvaluationRequest{
		EvaluatorID: "helpfulness",
	}); !errors.Is(err, ErrExperimentConflict) {
		t.Fatalf("expected ErrExperimentConflict, got %v", err)
	}
}

func TestTrialEvaluationRetriesServiceUnavailable(t *testing.T) {
	recorder := &experimentRecorder{}
	recorder.push(http.StatusServiceUnavailable, map[string]any{"error": "trial evaluation service is unavailable"})
	recorder.push(http.StatusAccepted, trialEvaluationBody(nil))
	server := httptest.NewServer(recorder.handler(t))
	defer server.Close()

	client := newExperimentTestClient(t, server.URL)
	if _, err := client.TriggerTrialEvaluation(context.Background(), "exp-1", "trial-1", TriggerTrialEvaluationRequest{
		EvaluatorID: "helpfulness",
	}); err != nil {
		t.Fatalf("trigger trial evaluation: %v", err)
	}
	if recorder.requestCount() != 2 {
		t.Fatalf("expected one retry for a 503, got %d request(s)", recorder.requestCount())
	}
}
