package agento11y

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// clearExperimentalGate removes the opt-in for one test so the assertions below
// see what a caller who never set the variable sees. t.Setenv records the
// previous value and restores it on cleanup; os.Unsetenv then removes the
// variable outright, which an empty string would not do.
func clearExperimentalGate(t *testing.T) {
	t.Helper()
	t.Setenv(EnvEnableExperimentalFeatures, "")
	if err := os.Unsetenv(EnvEnableExperimentalFeatures); err != nil {
		t.Fatalf("unset %s: %v", EnvEnableExperimentalFeatures, err)
	}
}

func TestExperimentalFeaturesDisabledByDefault(t *testing.T) {
	clearExperimentalGate(t)

	if ExperimentalFeaturesEnabled() {
		t.Fatal("expected the experimental gate to be off when the variable is unset")
	}
	err := RequireExperimental(FeatureCloudTrialEvaluation)
	if !errors.Is(err, ErrExperimentalFeatureDisabled) {
		t.Fatalf("expected ErrExperimentalFeatureDisabled, got %v", err)
	}
	// The message has to name the feature and the way to turn it on, because it
	// is the only place a caller learns either.
	for _, want := range []string{FeatureCloudTrialEvaluation, EnvEnableExperimentalFeatures} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err.Error(), want)
		}
	}
	if strings.Contains(err.Error(), "EnableExperimentalFeatures") {
		t.Fatalf("package-level error must not suggest a client option: %v", err)
	}
}

func TestExperimentalGateReadsTruthyValues(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{raw: "1", want: true},
		{raw: "true", want: true},
		{raw: "TRUE", want: true},
		{raw: " true ", want: true},
		{raw: "yes", want: true},
		{raw: "on", want: true},
		{raw: "", want: false},
		{raw: "0", want: false},
		{raw: "false", want: false},
		{raw: "no", want: false},
		{raw: "off", want: false},
		{raw: "maybe", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			t.Setenv(EnvEnableExperimentalFeatures, tc.raw)
			if got := ExperimentalFeaturesEnabled(); got != tc.want {
				t.Fatalf("%q: expected enabled=%v, got %v", tc.raw, tc.want, got)
			}
		})
	}
}

func TestClientExperimentalFeaturePrecedence(t *testing.T) {
	cases := []struct {
		name     string
		override *bool
		env      string
		want     bool
	}{
		{name: "explicit true beats unset environment", override: new(true), want: true},
		{name: "explicit false beats truthy environment", override: new(false), env: "true", want: false},
		{name: "nil uses truthy environment", env: "true", want: true},
		{name: "nil uses false environment", env: "false", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearExperimentalGate(t)
			if tc.env != "" {
				t.Setenv(EnvEnableExperimentalFeatures, tc.env)
			}
			client := NewClient(Config{
				EnableExperimentalFeatures: tc.override,
				testDisableWorker:          true,
			})
			t.Cleanup(func() { _ = client.Shutdown(context.Background()) })

			err := client.RequireExperimental(FeatureCloudTrialEvaluation)
			if got := err == nil; got != tc.want {
				t.Fatalf("RequireExperimental() admitted = %v, want %v; err = %v", got, tc.want, err)
			}
			if !tc.want && !errors.Is(err, ErrExperimentalFeatureDisabled) {
				t.Fatalf("expected ErrExperimentalFeatureDisabled, got %v", err)
			}
			if tc.override != nil && !*tc.override {
				if strings.Contains(err.Error(), EnvEnableExperimentalFeatures) {
					t.Fatalf("explicit false error must not suggest an environment override: %v", err)
				}
				if !strings.Contains(err.Error(), "BoolPtr(true)") {
					t.Fatalf("explicit false error must name the programmatic opt-in: %v", err)
				}
			}
		})
	}
}

func TestClientExperimentalFeatureNilReadsCurrentEnvironment(t *testing.T) {
	clearExperimentalGate(t)
	client := NewClient(Config{testDisableWorker: true})
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })

	if err := client.RequireExperimental(FeatureCloudTrialEvaluation); !errors.Is(err, ErrExperimentalFeatureDisabled) {
		t.Fatalf("expected closed initial gate, got %v", err)
	}
	t.Setenv(EnvEnableExperimentalFeatures, "true")
	if err := client.RequireExperimental(FeatureCloudTrialEvaluation); err != nil {
		t.Fatalf("expected environment change to open nil client gate, got %v", err)
	}
	t.Setenv(EnvEnableExperimentalFeatures, "false")
	if err := client.RequireExperimental(FeatureCloudTrialEvaluation); !errors.Is(err, ErrExperimentalFeatureDisabled) {
		t.Fatalf("expected environment change to close nil client gate, got %v", err)
	}
}

func TestClientExperimentalFeatureSettingsAreIsolated(t *testing.T) {
	clearExperimentalGate(t)
	openClient := NewClient(Config{EnableExperimentalFeatures: new(true), testDisableWorker: true})
	closedClient := NewClient(Config{EnableExperimentalFeatures: new(false), testDisableWorker: true})
	t.Cleanup(func() {
		_ = openClient.Shutdown(context.Background())
		_ = closedClient.Shutdown(context.Background())
	})

	if err := openClient.RequireExperimental(FeatureCloudTrialEvaluation); err != nil {
		t.Fatalf("open client: %v", err)
	}
	if err := closedClient.RequireExperimental(FeatureCloudTrialEvaluation); !errors.Is(err, ErrExperimentalFeatureDisabled) {
		t.Fatalf("closed client: expected ErrExperimentalFeatureDisabled, got %v", err)
	}
}

func TestClientCopiesExperimentalFeatureSetting(t *testing.T) {
	clearExperimentalGate(t)
	enabled := true
	client := NewClient(Config{EnableExperimentalFeatures: &enabled, testDisableWorker: true})
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })

	enabled = false
	if err := client.RequireExperimental(FeatureCloudTrialEvaluation); err != nil {
		t.Fatalf("caller mutation changed client gate: %v", err)
	}
}

func TestCloudTrialEvaluationBlockedWithoutTheGate(t *testing.T) {
	recorder := &experimentRecorder{}
	recorder.push(http.StatusOK, map[string]any{"trial_id": "trial-cloud"})
	server := httptest.NewServer(recorder.handler(t))
	defer server.Close()
	t.Setenv(EnvEnableExperimentalFeatures, "true")
	client := NewClient(Config{
		API:                        APIConfig{Endpoint: server.URL},
		EnableExperimentalFeatures: new(false),
		testGenerationExporter:     newNoopGenerationExporter(nil),
		testDisableWorker:          true,
	})
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })

	trial := NewTrial(client, TrialRef{RunID: "run-cloud", TestCaseID: "case-cloud"})
	trial.BindConversation("conv-1")

	cases := []struct {
		name string
		call func() error
	}{
		{
			name: "Trial.Evaluate",
			call: func() error {
				_, err := trial.Evaluate(context.Background(), "helpfulness")
				return err
			},
		},
		{
			name: "Client.TriggerTrialEvaluation",
			call: func() error {
				_, err := client.TriggerTrialEvaluation(context.Background(), "run-cloud", "trial-cloud", TriggerTrialEvaluationRequest{
					EvaluatorID: "helpfulness",
				})
				return err
			},
		},
		{
			name: "Client.GetTrialEvaluation",
			call: func() error {
				_, err := client.GetTrialEvaluation(context.Background(), "run-cloud", "trial-cloud", "teval-1")
				return err
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, ErrExperimentalFeatureDisabled) {
				t.Fatalf("expected ErrExperimentalFeatureDisabled, got %v", err)
			}
		})
	}

	// Trial.Evaluate creates the trial, persists the conversation binding, and
	// flushes the anchor generation before it triggers anything. A blocked call
	// must do none of that.
	if recorder.requestCount() != 0 {
		t.Fatalf("a blocked experimental call must not issue a request, got %d", recorder.requestCount())
	}
}
