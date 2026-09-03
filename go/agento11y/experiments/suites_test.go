package experiments

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestTestSuitesPullPortabilityAndBearerNormalization(t *testing.T) {
	var mu sync.Mutex
	var auth []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth = append(auth, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/test-suites/suite"):
			_, _ = w.Write([]byte(`{
				"suite_id":"suite","name":"Suite",
				"versions":[
					{"version":"v2","published":true},
					{"version":"v10","published":true},
					{"version":"v11","published":false}
				]
			}`))
		case strings.Contains(r.URL.Path, "/versions/v10/test-cases"):
			_, _ = w.Write([]byte(`{"items":[{
				"test_case_id":"scalar",
				"input":{"value":"hello"},
				"expected":{"value":4},
				"metadata":{"agento11y.sdk.portability":{"version":1,"weight":2.5,"wrapped_fields":["input","expected"]}}
			}],"next_cursor":"0"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewTestSuitesClient(TestSuitesClientOptions{
		ControlEndpoint:     server.URL + "/a/grafana-agento11y-app",
		ServiceAccountToken: "bearer token",
	})
	if err != nil {
		t.Fatal(err)
	}
	suite, err := client.PullSuite(context.Background(), "suite", "latest_published")
	if err != nil {
		t.Fatal(err)
	}
	if suite.Version != "v10" || len(suite.TestCases) != 1 ||
		suite.TestCases[0].Input != "hello" || suite.TestCases[0].Expected != float64(4) ||
		suite.TestCases[0].EffectiveWeight() != 2.5 {
		t.Fatalf("unexpected pulled suite: %#v", suite)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, value := range auth {
		if value != "Bearer token" {
			t.Fatalf("Bearer normalization failed: %q", value)
		}
	}
}

func TestZeroWeightIsPreservedInPortableSuiteMetadata(t *testing.T) {
	remote, err := localCaseToRemote(TestCase{
		TestCaseID: "disabled", Input: "skip", Weight: new(float64(0)),
	})
	if err != nil {
		t.Fatal(err)
	}
	metadata := objectMap(remote["metadata"])
	portability := objectMap(metadata[portabilityMetadataKey])
	weight, ok := numberValue(portability["weight"])
	if !ok || weight != 0 {
		t.Fatalf("explicit zero weight was not preserved: %#v", remote)
	}
	roundTrip := remoteCaseToLocal(remote)
	if roundTrip.EffectiveWeight() != 0 {
		t.Fatalf("zero weight changed during remote round trip: %#v", roundTrip)
	}
}

func TestUnsetWeightUsesPortableDefault(t *testing.T) {
	testCase := TestCase{TestCaseID: "default", Input: "run"}
	if testCase.EffectiveWeight() != 1 {
		t.Fatalf("unset weight must default to 1: %#v", testCase)
	}
	remote, err := localCaseToRemote(testCase)
	if err != nil {
		t.Fatal(err)
	}
	metadata := objectMap(remote["metadata"])
	portability := objectMap(metadata[portabilityMetadataKey])
	if _, ok := portability["weight"]; ok {
		t.Fatalf("default weight must not emit a portability weight: %#v", remote)
	}
	suite := TestSuite{
		SuiteID: "suite", TestCases: []TestCase{testCase},
	}
	data, err := MarshalSuite(suite)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "weight:") {
		t.Fatalf("default weight must be omitted from portable YAML:\n%s", data)
	}
}

func TestPushSuiteCreatesDraftPrunesAndPublishes(t *testing.T) {
	var mu sync.Mutex
	var methods []string
	getCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		methods = append(methods, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/test-suites/suite"):
			getCount++
			if getCount == 1 {
				_, _ = w.Write([]byte(`{"suite_id":"suite","name":"Old","versions":[]}`))
			} else {
				_, _ = w.Write([]byte(`{"suite_id":"suite","name":"Suite","versions":[]}`))
			}
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/versions"):
			_, _ = w.Write([]byte(`{"version":"v3","published":false,"changelog":"new"}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/versions/v3/test-cases"):
			_, _ = w.Write([]byte(`{"items":[{"test_case_id":"keep","input":{"value":"a"}},{"test_case_id":"remove","input":{"value":"x"}}]}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, ":publish"):
			_, _ = w.Write([]byte(`{"version":"v3","published":true}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer server.Close()
	client, err := NewTestSuitesClient(TestSuitesClientOptions{
		ControlEndpoint: server.URL + "/api/v1/eval", ServiceAccountToken: "token",
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.PushSuite(context.Background(), TestSuite{
		SuiteID: "suite", Name: "Suite",
		TestCases: []TestCase{{TestCaseID: "keep", Input: "a", Expected: "b", Weight: new(float64(2))}},
	}, PushSuiteOptions{Publish: true, Prune: true, Changelog: "new"})
	if err != nil {
		t.Fatal(err)
	}
	if result.SuiteVersion != "v3" || !result.Published ||
		len(result.PrunedCaseIDs) != 1 || result.PrunedCaseIDs[0] != "remove" {
		t.Fatalf("unexpected push result: %#v", result)
	}
	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, method := range methods {
		if strings.HasSuffix(method, "DELETE /api/v1/eval/test-suites/suite/versions/v3/test-cases/remove") {
			found = true
		}
	}
	if !found {
		t.Fatalf("prune request not found: %#v", methods)
	}
}

func TestResolveVersionAliasesAndDraftConflict(t *testing.T) {
	client := &TestSuitesClient{}
	suite := map[string]any{"versions": []any{
		map[string]any{"version": "v2", "published": true},
		map[string]any{"version": "v10", "published": true},
		map[string]any{"version": "v11", "published": false, "changelog": "old"},
	}}
	latest, err := client.ResolveVersion(suite, "latest")
	if err != nil || latest != "v11" {
		t.Fatalf("latest=%q err=%v", latest, err)
	}
	draft, err := client.ResolveVersion(suite, "draft")
	if err != nil || draft != "v11" {
		t.Fatalf("draft=%q err=%v", draft, err)
	}
	if err := validateDraftOptions(suiteVersions(suite)[2], "new", false); ClassifyConflict(err) != ConflictOpenDraft {
		t.Fatalf("unexpected conflict: %v (%s)", err, ClassifyConflict(err))
	}
}

func TestClassifyConflictKinds(t *testing.T) {
	cases := []struct {
		name string
		text string
		want ConflictKind
	}{
		{
			name: "pending evaluations",
			text: "status 409: cannot complete experiment with 2 pending evaluation(s)",
			want: ConflictPendingEvaluations,
		},
		{name: "running trials", text: "status 409: experiment has 1 running trial", want: ConflictRunningTrials},
		{name: "terminal state", text: "status 409: experiment already finalized", want: ConflictTerminalState},
		{name: "unrelated", text: "status 409: something else", want: ConflictUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyConflict(errors.New(tc.text)); got != tc.want {
				t.Fatalf("ClassifyConflict(%q) = %s, want %s", tc.text, got, tc.want)
			}
		})
	}
}

func TestAppPluginPathsUseCurrentPluginID(t *testing.T) {
	endpointCases := []struct {
		name  string
		given string
		want  string
	}{
		{
			name:  "app ui url is rewritten to the control path",
			given: "https://stack.grafana.net/a/grafana-agento11y-app",
			want:  "https://stack.grafana.net/api/plugins/grafana-agento11y-app/resources/eval",
		},
		{
			name:  "deep app ui url drops everything after the app path",
			given: "https://stack.grafana.net/a/grafana-agento11y-app/experiments/test-suites",
			want:  "https://stack.grafana.net/api/plugins/grafana-agento11y-app/resources/eval",
		},
		{
			name:  "control path is left alone",
			given: "https://stack.grafana.net/api/plugins/grafana-agento11y-app/resources/eval",
			want:  "https://stack.grafana.net/api/plugins/grafana-agento11y-app/resources/eval",
		},
	}
	for _, tc := range endpointCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeControlEndpoint(tc.given)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("normalizeControlEndpoint(%q)=%q want %q", tc.given, got, tc.want)
			}
		})
	}

	client := &Client{grafanaURL: "https://stack.grafana.net"}
	const wantURL = "https://stack.grafana.net/a/grafana-agento11y-app/offline-experiments/experiments/run%2F1"
	if got := client.ExperimentURL("run/1"); got != wantURL {
		t.Fatalf("ExperimentURL=%q want %q", got, wantURL)
	}
}

func TestNewTestSuitesClientEnvNames(t *testing.T) {
	envKeys := []string{
		"AGENTO11Y_GRAFANA_URL", "SIGIL_GRAFANA_URL",
		"AGENTO11Y_CONTROL_ENDPOINT", "SIGIL_CONTROL_ENDPOINT",
		"AGENTO11Y_SERVICE_ACCOUNT_TOKEN", "SIGIL_SERVICE_ACCOUNT_TOKEN",
	}
	tests := []struct {
		name        string
		env         map[string]string
		wantErr     string
		wantGrafana string
	}{
		{
			name: "legacy grafana url resolves and supplies the control endpoint",
			env: map[string]string{
				"SIGIL_GRAFANA_URL":               "https://legacy.grafana.net/",
				"AGENTO11Y_SERVICE_ACCOUNT_TOKEN": "token",
			},
			wantGrafana: "https://legacy.grafana.net",
		},
		{
			name: "canonical grafana url wins over legacy",
			env: map[string]string{
				"AGENTO11Y_GRAFANA_URL":           "https://canonical.grafana.net",
				"SIGIL_GRAFANA_URL":               "https://legacy.grafana.net",
				"AGENTO11Y_SERVICE_ACCOUNT_TOKEN": "token",
			},
			wantGrafana: "https://canonical.grafana.net",
		},
		{
			// Both control-plane names postdate the rename, so a SIGIL_ spelling
			// of either resolves nothing.
			name: "legacy control endpoint is not read",
			env: map[string]string{
				"SIGIL_CONTROL_ENDPOINT":          "https://legacy.grafana.net",
				"AGENTO11Y_SERVICE_ACCOUNT_TOKEN": "token",
			},
			wantErr: "control endpoint is required",
		},
		{
			name: "legacy service account token is not read",
			env: map[string]string{
				"AGENTO11Y_CONTROL_ENDPOINT":  "https://stack.grafana.net",
				"SIGIL_SERVICE_ACCOUNT_TOKEN": "token",
			},
			wantErr: "service account token is required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, key := range envKeys {
				t.Setenv(key, "")
			}
			for key, value := range tt.env {
				t.Setenv(key, value)
			}
			client, err := NewTestSuitesClient(TestSuitesClientOptions{})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("NewTestSuitesClient error=%v want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := client.GrafanaURL(); got != tt.wantGrafana {
				t.Fatalf("GrafanaURL=%q want %q", got, tt.wantGrafana)
			}
		})
	}
}
