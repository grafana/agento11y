package experiments

import (
	"context"
	"encoding/json"
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
		ControlEndpoint:     server.URL + "/a/grafana-sigil-app",
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
		suite.TestCases[0].Weight != 2.5 {
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
		TestCaseID: "disabled", Input: "skip", Weight: 0,
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
	if roundTrip.Weight != 0 {
		t.Fatalf("zero weight changed during remote round trip: %#v", roundTrip)
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
		TestCases: []TestCase{{TestCaseID: "keep", Input: "a", Expected: "b", Weight: 2}},
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
