package experiments

// Checks the Go experiments SDK against the shared wire fixtures in
// conformance/experiments/. Python and JavaScript run the same fixtures; see
// conformance/experiments/README.md.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	agento11y "github.com/grafana/agento11y/go/agento11y"
)

// sdkID is what this SDK substitutes for the ${SDK_ID} placeholder.
const sdkID = "go"

type conformanceInputs struct {
	ExperimentID          string   `json:"experiment_id"`
	ExperimentName        string   `json:"experiment_name"`
	SuiteID               string   `json:"suite_id"`
	SuiteVersion          string   `json:"suite_version"`
	TestCaseID            string   `json:"test_case_id"`
	Attempt               int      `json:"attempt"`
	ConversationID        string   `json:"conversation_id"`
	EvaluatorID           string   `json:"evaluator_id"`
	EvaluatorVersion      string   `json:"evaluator_version"`
	EvaluationID          string   `json:"evaluation_id"`
	ScoreEvaluatorID      string   `json:"score_evaluator_id"`
	ScoreEvaluatorVersion string   `json:"score_evaluator_version"`
	ScoreKey              string   `json:"score_key"`
	ScoreCreatedAt        string   `json:"score_created_at"`
	PlannedTrialCount     int      `json:"planned_trial_count"`
	Tags                  []string `json:"tags"`
}

type conformanceRequest struct {
	Method string         `json:"method"`
	Path   string         `json:"path"`
	Body   map[string]any `json:"body"`
}

type capturedCall struct {
	Method string
	Path   string
	Body   map[string]any
	Raw    string
}

func TestExperimentsConformanceStableIDs(t *testing.T) {
	var fixture struct {
		Vectors []struct {
			Prefix string `json:"prefix"`
			Parts  []any  `json:"parts"`
			ID     string `json:"id"`
		} `json:"vectors"`
	}
	loadFixture(t, "ids.json", &fixture)
	if len(fixture.Vectors) == 0 {
		t.Fatal("no id vectors in the fixture")
	}
	for _, vector := range fixture.Vectors {
		parts := make([]any, len(vector.Parts))
		for i, part := range vector.Parts {
			// JSON numbers decode as float64; the vectors use integers, and an
			// attempt formatted as "1.0" hashes differently from "1".
			if number, ok := part.(float64); ok && number == float64(int64(number)) {
				parts[i] = int64(number)
				continue
			}
			parts[i] = part
		}
		if got := agento11y.StableID(vector.Prefix, parts...); got != vector.ID {
			t.Errorf("StableID(%q, %v) = %q, want %q", vector.Prefix, vector.Parts, got, vector.ID)
		}
	}
}

func TestExperimentsConformanceRequestBodies(t *testing.T) {
	inputs := loadInputs(t)
	requests := loadRequests(t)
	trialID := agento11y.StableID("trial", inputs.ExperimentID, inputs.TestCaseID, inputs.Attempt)
	createdAt, err := time.Parse(time.RFC3339, inputs.ScoreCreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	plannedCount := inputs.PlannedTrialCount
	scoreID := agento11y.StableID("score", inputs.ExperimentID, trialID, inputs.ScoreKey, inputs.ScoreEvaluatorID)
	passed := true

	cases := []struct {
		name    string
		fixture string
		call    func(context.Context, *Client) error
	}{
		{
			name:    "run upsert",
			fixture: "run_upsert",
			call: func(ctx context.Context, client *Client) error {
				_, err := client.UpsertExperiment(ctx, agento11y.CreateExperimentRequest{
					RunID: inputs.ExperimentID, Name: inputs.ExperimentName,
					Source: agento11y.ExperimentSourceExternal, Tags: inputs.Tags,
					SuiteID: inputs.SuiteID, SuiteVersion: inputs.SuiteVersion,
					PlannedTrialCount: &plannedCount,
				})
				return err
			},
		},
		{
			name:    "trial create",
			fixture: "trial_create",
			call: func(ctx context.Context, client *Client) error {
				_, err := client.UpsertTrial(ctx, inputs.ExperimentID, agento11y.UpsertTrialRequest{
					TrialID: trialID, TestCaseID: inputs.TestCaseID, Attempt: inputs.Attempt,
				})
				return err
			},
		},
		{
			name:    "conversation patch",
			fixture: "trial_patch_conversation",
			call: func(ctx context.Context, client *Client) error {
				_, err := client.UpdateTrial(ctx, inputs.ExperimentID, trialID, agento11y.UpdateTrialRequest{
					ConversationID: inputs.ConversationID,
				})
				return err
			},
		},
		{
			name:    "terminal patch",
			fixture: "trial_patch_terminal",
			call: func(ctx context.Context, client *Client) error {
				_, err := client.UpdateTrial(ctx, inputs.ExperimentID, trialID, agento11y.UpdateTrialRequest{
					Status: "completed", ConversationID: inputs.ConversationID,
				})
				return err
			},
		},
		{
			name:    "evaluate with a pinned version",
			fixture: "trial_evaluate",
			call: func(ctx context.Context, client *Client) error {
				_, err := client.TriggerTrialEvaluation(ctx, inputs.ExperimentID, trialID, TriggerTrialEvaluationRequest{
					EvaluatorID: inputs.EvaluatorID, EvaluatorVersion: inputs.EvaluatorVersion,
				})
				return err
			},
		},
		{
			name:    "evaluate with the latest version",
			fixture: "trial_evaluate_latest_version",
			call: func(ctx context.Context, client *Client) error {
				_, err := client.TriggerTrialEvaluation(ctx, inputs.ExperimentID, trialID, TriggerTrialEvaluationRequest{
					EvaluatorID: inputs.EvaluatorID,
				})
				return err
			},
		},
		{
			name:    "evaluate with a reserved trial id",
			fixture: "trial_evaluate_reserved_trial_id",
			call: func(ctx context.Context, client *Client) error {
				_, err := client.TriggerTrialEvaluation(ctx, inputs.ExperimentID, "trial:one/blue", TriggerTrialEvaluationRequest{
					EvaluatorID: inputs.EvaluatorID,
				})
				return err
			},
		},
		{
			name:    "score export",
			fixture: "scores_export",
			call: func(ctx context.Context, client *Client) error {
				_, err := client.ExportScores(ctx, []ScoreItem{{
					ScoreID: scoreID, EvaluatorID: inputs.ScoreEvaluatorID,
					EvaluatorVersion: inputs.ScoreEvaluatorVersion,
					// Local-only on purpose: no SDK puts the evaluator kind on the wire.
					EvaluatorKind:  "deterministic",
					ScoreKey:       inputs.ScoreKey,
					Value:          agento11y.BoolScoreValue(true),
					ConversationID: inputs.ConversationID, RunID: inputs.ExperimentID,
					TrialID: trialID, TestCaseID: inputs.TestCaseID,
					Passed: &passed, Explanation: "matched the expected answer",
					CreatedAt: &createdAt,
					Source:    &agento11y.ScoreSource{Kind: "experiment", ID: inputs.ExperimentID},
				}})
				return err
			},
		},
		{
			name:    "finalize",
			fixture: "run_finalize",
			call: func(ctx context.Context, client *Client) error {
				_, err := client.Finalize(ctx, inputs.ExperimentID, ExperimentStatusCompleted, nil, "")
				return err
			},
		},
		{
			name:    "finalize with a score count",
			fixture: "run_finalize_with_score_count",
			call: func(ctx context.Context, client *Client) error {
				count := 1
				_, err := client.Finalize(ctx, inputs.ExperimentID, ExperimentStatusCompleted, &count, "")
				return err
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			want, ok := requests[testCase.fixture]
			if !ok {
				t.Fatalf("fixture %q is missing from requests.json", testCase.fixture)
			}
			calls := captureCalls(t, func(ctx context.Context, client *Client) {
				if err := testCase.call(ctx, client); err != nil {
					t.Fatal(err)
				}
			})
			if len(calls) != 1 {
				t.Fatalf("got %d requests, want 1", len(calls))
			}
			assertMatchesFixture(t, calls[0], want)
		})
	}
}

func TestExperimentsConformanceEvaluationResponses(t *testing.T) {
	inputs := loadInputs(t)
	var responses map[string]json.RawMessage
	loadFixture(t, "responses.json", &responses)

	statuses := map[string]agento11y.TrialEvaluationStatus{
		"evaluation_queued":  agento11y.TrialEvaluationStatusQueued,
		"evaluation_claimed": agento11y.TrialEvaluationStatusClaimed,
		"evaluation_success": agento11y.TrialEvaluationStatusSuccess,
		"evaluation_failed":  agento11y.TrialEvaluationStatusFailed,
	}
	names := make([]string, 0, len(statuses))
	for name := range statuses {
		names = append(names, name)
	}
	sort.Strings(names)
	// Every suite checks the same field list on the same fixtures, so a misparse in
	// one SDK cannot pass while the others check a different subset.
	for _, name := range names {
		evaluation := parseEvaluationFixture(t, responses[name])
		if evaluation.Status != statuses[name] {
			t.Errorf("%s: status = %q, want %q", name, evaluation.Status, statuses[name])
		}
		if evaluation.EvaluationID != inputs.EvaluationID {
			t.Errorf("%s: evaluation_id = %q, want %q", name, evaluation.EvaluationID, inputs.EvaluationID)
		}
		if evaluation.ExperimentID != inputs.ExperimentID {
			t.Errorf("%s: experiment_id = %q, want %q", name, evaluation.ExperimentID, inputs.ExperimentID)
		}
		if want := agento11y.StableID("trial", inputs.ExperimentID, inputs.TestCaseID, inputs.Attempt); evaluation.TrialID != want {
			t.Errorf("%s: trial_id = %q, want %q", name, evaluation.TrialID, want)
		}
		if evaluation.ConversationID != inputs.ConversationID {
			t.Errorf("%s: conversation_id = %q, want %q", name, evaluation.ConversationID, inputs.ConversationID)
		}
		if evaluation.EvaluatorID != inputs.EvaluatorID {
			t.Errorf("%s: evaluator_id = %q, want %q", name, evaluation.EvaluatorID, inputs.EvaluatorID)
		}
		if evaluation.EvaluatorVersion != inputs.EvaluatorVersion {
			t.Errorf("%s: evaluator_version = %q, want %q", name, evaluation.EvaluatorVersion, inputs.EvaluatorVersion)
		}
	}

	queued := parseEvaluationFixture(t, responses["evaluation_queued"])
	if queued.Attempts != 0 || queued.TestCaseID != inputs.TestCaseID {
		t.Errorf("queued evaluation: %#v", queued)
	}
	success := parseEvaluationFixture(t, responses["evaluation_success"])
	if success.Attempts != 1 || success.TestCaseID != inputs.TestCaseID {
		t.Errorf("success evaluation: %#v", success)
	}
	if success.CreatedAt == nil || !success.CreatedAt.Equal(mustParseTime(t, "2026-01-01T00:00:00Z")) {
		t.Errorf("success created_at = %v", success.CreatedAt)
	}
	if success.UpdatedAt == nil || !success.UpdatedAt.Equal(mustParseTime(t, "2026-01-01T00:00:05Z")) {
		t.Errorf("success updated_at = %v", success.UpdatedAt)
	}

	failed := parseEvaluationFixture(t, responses["evaluation_failed"])
	if failed.Error != "grader crashed" || failed.Attempts != 3 {
		t.Errorf("failed evaluation: %#v", failed)
	}

	// An unknown status and a missing id both have to fail the call rather than
	// read as a non-terminal state the SDK would poll on.
	for _, name := range []string{"evaluation_unsupported_status", "evaluation_missing_id"} {
		body := responses[name]
		calls := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
		}))
		client := newConformanceClient(t, server.URL)
		_, err := client.GetTrialEvaluation(context.Background(), inputs.ExperimentID, "trial-1", inputs.EvaluationID)
		server.Close()
		if err == nil {
			t.Errorf("%s: expected an error", name)
		}
		if calls != 1 {
			t.Errorf("%s: sent %d requests, want 1", name, calls)
		}
	}
}

func TestExperimentsConformanceReportEnvelopes(t *testing.T) {
	inputs := loadInputs(t)
	var responses map[string]json.RawMessage
	loadFixture(t, "responses.json", &responses)

	var fromExperiment, fromRun agento11y.ExperimentReport
	if err := json.Unmarshal(responses["report_experiment_envelope"], &fromExperiment); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(responses["report_run_envelope"], &fromRun); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fromExperiment, fromRun) {
		t.Fatalf("the two report envelopes parsed differently:\n%#v\n%#v", fromExperiment, fromRun)
	}
	if fromExperiment.Run.RunID != inputs.ExperimentID {
		t.Errorf("run id = %q, want %q", fromExperiment.Run.RunID, inputs.ExperimentID)
	}
	if fromExperiment.Summary.TrialCount != 1 {
		t.Errorf("trial count = %d, want 1", fromExperiment.Summary.TrialCount)
	}
	// Only a score under the "final" key feeds the pass rate, and a stored
	// evaluator writes under its own key.
	if fromExperiment.Summary.PassRateValue != nil {
		t.Errorf("pass rate = %v, want unset", *fromExperiment.Summary.PassRateValue)
	}
}

func TestExperimentsConformanceCloudEvaluatedTrialCallOrder(t *testing.T) {
	inputs := loadInputs(t)
	requests := loadRequests(t)
	trialID := agento11y.StableID("trial", inputs.ExperimentID, inputs.TestCaseID, inputs.Attempt)
	var responses map[string]json.RawMessage
	loadFixture(t, "responses.json", &responses)

	calls := captureCallsWith(t, func(path string, method string) json.RawMessage {
		switch {
		case strings.HasSuffix(path, ":evaluate"):
			return responses["evaluation_success"]
		case strings.Contains(path, "/evaluations/"):
			return responses["evaluation_success"]
		case strings.HasSuffix(path, ":upsert"):
			return responses["run_upsert_response"]
		case strings.HasSuffix(path, ":finalize"):
			return responses["run_finalize_response"]
		default:
			_ = method
			return json.RawMessage(`{"trial_id":"` + trialID + `"}`)
		}
	}, func(ctx context.Context, client *Client) {
		count := inputs.PlannedTrialCount
		experiment, err := NewExperiment(client, ExperimentOptions{
			ExperimentID: inputs.ExperimentID, Name: inputs.ExperimentName,
			Tags: inputs.Tags, PlannedTrialCount: &count,
			Suite: &TestSuite{SuiteID: inputs.SuiteID, Version: inputs.SuiteVersion},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := experiment.Enter(ctx); err != nil {
			t.Fatal(err)
		}
		if err := experiment.WithTrialByCaseID(ctx, inputs.TestCaseID, func(ctx context.Context, trial *Trial) error {
			trial.BindConversation(inputs.ConversationID)
			_, err := trial.Evaluate(ctx, inputs.EvaluatorID, EvaluateOptions{
				EvaluatorVersion: inputs.EvaluatorVersion,
			})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		scoreCount := 1
		if err := experiment.Finalize(ctx, ExperimentStatusCompleted, FinalizeOptions{ScoreCount: &scoreCount}); err != nil {
			t.Fatal(err)
		}
	})

	got := make([]string, 0, len(calls))
	for _, call := range calls {
		got = append(got, call.Method+" "+call.Path)
		if call.Path == "/api/v1/scores:export" {
			t.Error("a cloud-evaluated trial must write no local score")
		}
	}
	want := []string{
		"POST /api/v1/experiment-runs:upsert",
		"POST /api/v1/experiment-runs/" + inputs.ExperimentID + "/trials",
		"PATCH /api/v1/experiment-runs/" + inputs.ExperimentID + "/trials/" + trialID,
		"POST /api/v1/experiment-runs/" + inputs.ExperimentID + "/trials/" + trialID + ":evaluate",
		"PATCH /api/v1/experiment-runs/" + inputs.ExperimentID + "/trials/" + trialID,
		"POST /api/v1/experiment-runs/" + inputs.ExperimentID + ":finalize",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("call order:\ngot  %v\nwant %v", got, want)
	}
	assertMatchesFixture(t, calls[2], requests["trial_patch_conversation"])
	assertMatchesFixture(t, calls[3], requests["trial_evaluate"])
	// The caller's score count is dropped once a trial queued an evaluation.
	assertMatchesFixture(t, calls[5], requests["run_finalize"])
}

// --- helpers -------------------------------------------------------------- //

func fixturePath(t *testing.T, name string) string {
	t.Helper()
	// go/agento11y/experiments -> repository root
	return filepath.Join("..", "..", "..", "conformance", "experiments", name)
}

func loadFixture(t *testing.T, name string, out any) {
	t.Helper()
	raw, err := os.ReadFile(fixturePath(t, name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

func loadInputs(t *testing.T) conformanceInputs {
	t.Helper()
	var inputs conformanceInputs
	loadFixture(t, "inputs.json", &inputs)
	return inputs
}

func loadRequests(t *testing.T) map[string]conformanceRequest {
	t.Helper()
	var requests map[string]conformanceRequest
	loadFixture(t, "requests.json", &requests)
	for name, request := range requests {
		request.Body = substituteSDKID(request.Body).(map[string]any)
		requests[name] = request
	}
	return requests
}

// substituteSDKID replaces the ${SDK_ID} placeholder every source object carries.
func substituteSDKID(value any) any {
	switch typed := value.(type) {
	case string:
		if typed == "${SDK_ID}" {
			return sdkID
		}
		return typed
	case []any:
		for i, item := range typed {
			typed[i] = substituteSDKID(item)
		}
		return typed
	case map[string]any:
		for key, item := range typed {
			typed[key] = substituteSDKID(item)
		}
		return typed
	default:
		return value
	}
}

func newConformanceClient(t *testing.T, endpoint string) *Client {
	t.Helper()
	useOTel := false
	redact := false
	client, err := NewClient(ClientOptions{
		Endpoint: endpoint, IngestToken: "token",
		UseExperimentalOTel: &useOTel, RedactSecrets: &redact,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func captureCalls(t *testing.T, run func(context.Context, *Client)) []capturedCall {
	t.Helper()
	var responses map[string]json.RawMessage
	loadFixture(t, "responses.json", &responses)
	// The evaluation routes validate their response, so they get the canned one.
	return captureCallsWith(t, func(path string, _ string) json.RawMessage {
		if strings.HasSuffix(path, ":evaluate") || strings.Contains(path, "/evaluations/") {
			return responses["evaluation_queued"]
		}
		return json.RawMessage(`{}`)
	}, run)
}

func captureCallsWith(
	t *testing.T,
	respond func(path string, method string) json.RawMessage,
	run func(context.Context, *Client),
) []capturedCall {
	t.Helper()
	var mu sync.Mutex
	var calls []capturedCall
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &body)
		}
		mu.Lock()
		calls = append(calls, capturedCall{
			Method: r.Method, Path: r.URL.EscapedPath(), Body: body, Raw: string(raw),
		})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(respond(r.URL.Path, r.Method))
	}))
	defer server.Close()

	client := newConformanceClient(t, server.URL)
	run(context.Background(), client)
	_ = client.Shutdown(context.Background())

	mu.Lock()
	defer mu.Unlock()
	return calls
}

func assertMatchesFixture(t *testing.T, got capturedCall, want conformanceRequest) {
	t.Helper()
	if got.Method != want.Method {
		t.Errorf("method = %q, want %q", got.Method, want.Method)
	}
	if got.Path != want.Path {
		t.Errorf("path = %q, want %q", got.Path, want.Path)
	}
	if differences := diffJSON(got.Body, want.Body, ""); len(differences) > 0 {
		t.Errorf("body differs from the fixture:\n%s\ngot: %s", strings.Join(differences, "\n"), got.Raw)
	}
}

// diffJSON reports structural differences as dotted JSON paths so a failure names
// the offending field. A fixture's documentation-only "comment" key is ignored.
func diffJSON(got, want any, path string) []string {
	label := path
	if label == "" {
		label = "<root>"
	}
	switch wanted := want.(type) {
	case map[string]any:
		gotMap, ok := got.(map[string]any)
		if !ok {
			return []string{fmt.Sprintf("%s: got %v, want an object", label, got)}
		}
		var differences []string
		keys := make([]string, 0, len(wanted))
		for key := range wanted {
			if key == "comment" {
				continue
			}
			keys = append(keys, key)
		}
		sort.Strings(keys)
		gotKeys := make([]string, 0, len(gotMap))
		for key := range gotMap {
			gotKeys = append(gotKeys, key)
		}
		sort.Strings(gotKeys)
		for _, key := range gotKeys {
			if _, expected := wanted[key]; !expected {
				differences = append(differences, fmt.Sprintf("%s.%s: unexpected field %v", path, key, gotMap[key]))
			}
		}
		for _, key := range keys {
			value, present := gotMap[key]
			if !present {
				differences = append(differences, fmt.Sprintf("%s.%s: missing, want %v", path, key, wanted[key]))
				continue
			}
			differences = append(differences, diffJSON(value, wanted[key], path+"."+key)...)
		}
		return differences
	case []any:
		gotSlice, ok := got.([]any)
		if !ok {
			return []string{fmt.Sprintf("%s: got %v, want an array", label, got)}
		}
		if len(gotSlice) != len(wanted) {
			return []string{fmt.Sprintf("%s: got %d items, want %d", label, len(gotSlice), len(wanted))}
		}
		var differences []string
		for i := range wanted {
			differences = append(differences, diffJSON(gotSlice[i], wanted[i], fmt.Sprintf("%s[%d]", path, i))...)
		}
		return differences
	default:
		if !reflect.DeepEqual(got, want) {
			return []string{fmt.Sprintf("%s: got %#v, want %#v", label, got, want)}
		}
		return nil
	}
}

// parseEvaluationFixture serves the fixture and reads it back through
// GetTrialEvaluation, so the assertions cover the SDK's own decode path rather
// than a plain json.Unmarshal the SDK never performs.
func parseEvaluationFixture(t *testing.T, raw json.RawMessage) agento11y.TrialEvaluation {
	t.Helper()
	inputs := loadInputs(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	}))
	defer server.Close()
	evaluation, err := newConformanceClient(t, server.URL).GetTrialEvaluation(
		context.Background(), inputs.ExperimentID, "trial-1", inputs.EvaluationID,
	)
	if err != nil {
		t.Fatal(err)
	}
	return *evaluation
}

func mustParseTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
