package sigil

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace/noop"
)

func TestCreateExperimentOverHTTP(t *testing.T) {
	var capturedPath string
	var capturedMethod string
	var capturedHeaders http.Header
	var capturedBody CreateExperimentRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		capturedPath = req.URL.Path
		capturedMethod = req.Method
		capturedHeaders = req.Header.Clone()

		if err := json.NewDecoder(req.Body).Decode(&capturedBody); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"tenant_id":"tenant-a",
			"run_id":"run-1",
			"name":"Dashboard smoke",
			"source":"external",
			"status":"running",
			"metadata":{"suite":"dashboard"},
			"created_at":"2026-05-17T12:00:00Z",
			"updated_at":"2026-05-17T12:00:00Z",
			"started_at":"2026-05-17T12:00:00Z"
		}`))
	}))
	defer server.Close()

	client := newExperimentTestClient(t, experimentTestClientOptions{
		apiEndpoint: server.URL,
		auth: AuthConfig{
			Mode:     ExportAuthModeTenant,
			TenantID: "tenant-a",
		},
	})
	t.Cleanup(func() {
		_ = client.Shutdown(context.Background())
	})

	experiment, err := client.CreateExperiment(context.Background(), CreateExperimentRequest{
		RunID:    " run-1 ",
		Name:     " Dashboard smoke ",
		Metadata: map[string]any{"suite": "dashboard"},
	})
	if err != nil {
		t.Fatalf("create experiment: %v", err)
	}

	if capturedMethod != http.MethodPost {
		t.Fatalf("method=%s, want POST", capturedMethod)
	}
	if capturedPath != "/api/v1/eval/experiments" {
		t.Fatalf("path=%s, want /api/v1/eval/experiments", capturedPath)
	}
	if capturedHeaders.Get("X-Scope-OrgID") != "tenant-a" {
		t.Fatalf("tenant header=%q, want tenant-a", capturedHeaders.Get("X-Scope-OrgID"))
	}
	if capturedBody.RunID != "run-1" {
		t.Fatalf("run_id=%q, want run-1", capturedBody.RunID)
	}
	if capturedBody.Source != "external" {
		t.Fatalf("source=%q, want external", capturedBody.Source)
	}
	if capturedBody.Name != "Dashboard smoke" {
		t.Fatalf("name=%q, want Dashboard smoke", capturedBody.Name)
	}
	if experiment == nil || experiment.RunID != "run-1" || experiment.Status != "running" {
		t.Fatalf("unexpected experiment: %#v", experiment)
	}
}

func TestUpdateExperimentStatus(t *testing.T) {
	var capturedPath string
	var capturedMethod string
	var capturedBody UpdateExperimentRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		capturedPath = req.URL.Path
		capturedMethod = req.Method

		if err := json.NewDecoder(req.Body).Decode(&capturedBody); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"tenant_id":"tenant-a",
			"run_id":"run-1",
			"name":"Dashboard smoke",
			"source":"external",
			"status":"succeeded",
			"created_at":"2026-05-17T12:00:00Z",
			"updated_at":"2026-05-17T12:01:00Z",
			"completed_at":"2026-05-17T12:01:00Z"
		}`))
	}))
	defer server.Close()

	client := newExperimentTestClient(t, experimentTestClientOptions{apiEndpoint: server.URL})
	t.Cleanup(func() {
		_ = client.Shutdown(context.Background())
	})

	status := " succeeded "
	experiment, err := client.UpdateExperiment(context.Background(), "run-1", UpdateExperimentRequest{Status: &status})
	if err != nil {
		t.Fatalf("update experiment: %v", err)
	}

	if capturedMethod != http.MethodPatch {
		t.Fatalf("method=%s, want PATCH", capturedMethod)
	}
	if capturedPath != "/api/v1/eval/experiments/run-1" {
		t.Fatalf("path=%s, want /api/v1/eval/experiments/run-1", capturedPath)
	}
	if capturedBody.Status == nil || *capturedBody.Status != "succeeded" {
		t.Fatalf("captured status=%v, want succeeded", capturedBody.Status)
	}
	if experiment == nil || experiment.Status != "succeeded" {
		t.Fatalf("unexpected experiment: %#v", experiment)
	}
}

func TestGetExperimentReport(t *testing.T) {
	var capturedPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		capturedPath = req.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"run":{"tenant_id":"tenant-a","run_id":"run-1","name":"Dashboard smoke","source":"external","status":"succeeded","created_at":"2026-05-17T12:00:00Z","updated_at":"2026-05-17T12:01:00Z"},
			"summary":{"n_conversations":1,"n_generations":1,"n_scores":1,"pass_rate":1,"mean_score":1},
			"breakdowns":{"by_task":[],"by_category":[],"by_evaluator":[],"by_score_key":[],"by_evaluator_score_key":[]},
			"points":[{"conversation_id":"conv-1","generation_id":"gen-1","score_id":"score-1","evaluator_id":"llmspec","score_key":"passed","score_type":"number","value":{"number":1},"created_at":"2026-05-17T12:01:00Z"}]
		}`))
	}))
	defer server.Close()

	client := newExperimentTestClient(t, experimentTestClientOptions{apiEndpoint: server.URL})
	t.Cleanup(func() {
		_ = client.Shutdown(context.Background())
	})

	report, err := client.GetExperimentReport(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("get report: %v", err)
	}
	if capturedPath != "/api/v1/eval/experiments/run-1/report" {
		t.Fatalf("path=%s, want /api/v1/eval/experiments/run-1/report", capturedPath)
	}
	if report == nil || report.Summary.NScores != 1 || len(report.Points) != 1 {
		t.Fatalf("unexpected report: %#v", report)
	}
}

func TestExportScoresOverHTTP(t *testing.T) {
	var capturedPath string
	var capturedMethod string
	var capturedBody ExportScoresRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		capturedPath = req.URL.Path
		capturedMethod = req.Method

		if err := json.NewDecoder(req.Body).Decode(&capturedBody); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"results":[{"score_id":"score-1","accepted":true}]}`))
	}))
	defer server.Close()

	client := newExperimentTestClient(t, experimentTestClientOptions{apiEndpoint: server.URL})
	t.Cleanup(func() {
		_ = client.Shutdown(context.Background())
	})

	response, err := client.ExportScores(context.Background(), ExportScoresRequest{
		Scores: []ScoreItem{{
			ScoreID:          " score-1 ",
			GenerationID:     " gen-1 ",
			ConversationID:   " conv-1 ",
			EvaluatorID:      " llmspec ",
			EvaluatorVersion: " 1 ",
			RunID:            " run-1 ",
			ScoreKey:         " passed ",
			Value:            NumberScoreValue(1),
			Passed:           boolPtrForExperimentTest(true),
			Metadata:         map[string]any{"task_id": "dashboard.convo.yaml"},
		}},
	})
	if err != nil {
		t.Fatalf("export scores: %v", err)
	}

	if capturedMethod != http.MethodPost {
		t.Fatalf("method=%s, want POST", capturedMethod)
	}
	if capturedPath != "/api/v1/scores:export" {
		t.Fatalf("path=%s, want /api/v1/scores:export", capturedPath)
	}
	if len(capturedBody.Scores) != 1 {
		t.Fatalf("captured scores=%d, want 1", len(capturedBody.Scores))
	}
	score := capturedBody.Scores[0]
	if score.ScoreID != "score-1" || score.GenerationID != "gen-1" || score.ScoreKey != "passed" {
		t.Fatalf("score was not normalized: %#v", score)
	}
	if response == nil || len(response.Results) != 1 || !response.Results[0].Accepted {
		t.Fatalf("unexpected response: %#v", response)
	}
}

func TestExperimentStatusErrors(t *testing.T) {
	testCases := []struct {
		status int
		want   error
	}{
		{status: http.StatusBadRequest, want: ErrExperimentValidationFailed},
		{status: http.StatusConflict, want: ErrExperimentConflict},
		{status: http.StatusNotFound, want: ErrExperimentNotFound},
		{status: http.StatusUnauthorized, want: ErrExperimentTransportFailed},
	}

	for _, tc := range testCases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "nope", tc.status)
			}))
			defer server.Close()

			client := newExperimentTestClient(t, experimentTestClientOptions{apiEndpoint: server.URL})
			t.Cleanup(func() {
				_ = client.Shutdown(context.Background())
			})

			_, err := client.GetExperiment(context.Background(), "run-1")
			if !errors.Is(err, tc.want) {
				t.Fatalf("error=%v, want %v", err, tc.want)
			}
		})
	}
}

func TestExportScoresValidation(t *testing.T) {
	client := newExperimentTestClient(t, experimentTestClientOptions{apiEndpoint: "http://example.invalid"})
	t.Cleanup(func() {
		_ = client.Shutdown(context.Background())
	})

	_, err := client.ExportScores(context.Background(), ExportScoresRequest{})
	if !errors.Is(err, ErrScoreValidationFailed) {
		t.Fatalf("error=%v, want ErrScoreValidationFailed", err)
	}

	_, err = client.ExportScores(context.Background(), ExportScoresRequest{
		Scores: []ScoreItem{{
			ScoreID:          "score-1",
			GenerationID:     "gen-1",
			EvaluatorID:      "llmspec",
			EvaluatorVersion: "1",
			ScoreKey:         "passed",
			Value: ScoreValue{
				Number: float64PtrForExperimentTest(1),
				Bool:   boolPtrForExperimentTest(true),
			},
		}},
	})
	if !errors.Is(err, ErrScoreValidationFailed) {
		t.Fatalf("error=%v, want ErrScoreValidationFailed", err)
	}
}

func TestExportScoresStatusError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer server.Close()

	client := newExperimentTestClient(t, experimentTestClientOptions{apiEndpoint: server.URL})
	t.Cleanup(func() {
		_ = client.Shutdown(context.Background())
	})

	_, err := client.ExportScores(context.Background(), ExportScoresRequest{
		Scores: []ScoreItem{{
			ScoreID:          "score-1",
			GenerationID:     "gen-1",
			EvaluatorID:      "llmspec",
			EvaluatorVersion: "1",
			ScoreKey:         "passed",
			Value:            NumberScoreValue(1),
		}},
	})
	if !errors.Is(err, ErrScoreValidationFailed) {
		t.Fatalf("error=%v, want ErrScoreValidationFailed", err)
	}
}

func TestExperimentUsesBearerHeader(t *testing.T) {
	var authorizationHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		authorizationHeader = req.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tenant_id":"tenant-a","run_id":"run-1","name":"Dashboard smoke","source":"external","status":"running","created_at":"2026-05-17T12:00:00Z","updated_at":"2026-05-17T12:00:00Z"}`))
	}))
	defer server.Close()

	client := newExperimentTestClient(t, experimentTestClientOptions{
		apiEndpoint: strings.TrimPrefix(server.URL, "http://"),
		auth: AuthConfig{
			Mode:        ExportAuthModeBearer,
			BearerToken: "token-a",
		},
	})
	t.Cleanup(func() {
		_ = client.Shutdown(context.Background())
	})

	_, err := client.GetExperiment(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("get experiment: %v", err)
	}
	if authorizationHeader != "Bearer token-a" {
		t.Fatalf("authorization=%q, want bearer token", authorizationHeader)
	}
}

type experimentTestClientOptions struct {
	apiEndpoint string
	auth        AuthConfig
}

func newExperimentTestClient(t *testing.T, options experimentTestClientOptions) *Client {
	t.Helper()
	return NewClient(Config{
		Tracer: noop.NewTracerProvider().Tracer("sigil-go-experiment-test"),
		GenerationExport: GenerationExportConfig{
			Protocol:        GenerationExportProtocolHTTP,
			Endpoint:        options.apiEndpoint + "/api/v1/generations:export",
			Auth:            options.auth,
			Insecure:        BoolPtr(true),
			BatchSize:       1,
			FlushInterval:   time.Hour,
			QueueSize:       1,
			MaxRetries:      1,
			InitialBackoff:  time.Millisecond,
			MaxBackoff:      time.Millisecond,
			PayloadMaxBytes: 1 << 20,
		},
		API: APIConfig{
			Endpoint: options.apiEndpoint,
		},
		testGenerationExporter: newNoopGenerationExporter(nil),
		testDisableWorker:      true,
	})
}

func boolPtrForExperimentTest(v bool) *bool {
	return &v
}

func float64PtrForExperimentTest(v float64) *float64 {
	return &v
}
