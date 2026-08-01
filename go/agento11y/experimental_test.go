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

func TestCloudTrialEvaluationBlockedWithoutTheGate(t *testing.T) {
	recorder := &experimentRecorder{}
	recorder.push(http.StatusOK, map[string]any{"trial_id": "trial-cloud"})
	server := httptest.NewServer(recorder.handler(t))
	defer server.Close()
	client := newExperimentTestClient(t, server.URL)

	// Built while the gate is still on, so the block below is the gate and not a
	// setup failure.
	trial := NewTrial(client, TrialRef{RunID: "run-cloud", TestCaseID: "case-cloud"})
	trial.BindConversation("conv-1")

	clearExperimentalGate(t)

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
