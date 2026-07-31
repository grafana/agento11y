package agento11y

import (
	"fmt"
	"os"
	"strings"
)

// EnvEnableExperimentalFeatures is the opt-in switch for SDK features that are
// not stable yet. Set it to 1, true, yes, or on.
//
// An experimental feature can change or be removed in any release, and is not
// covered by the compatibility the rest of the SDK aims for.
//
// This gate is separate from AGENTO11Y_USE_EXPERIMENTAL_OTEL, which stays an
// independent opt-in for experimental trial spans and evaluation-result events.
const EnvEnableExperimentalFeatures = "AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES"

// FeatureCloudTrialEvaluation names the experimental feature that grades a trial
// with an evaluator stored in the tenant.
const FeatureCloudTrialEvaluation = "cloud trial evaluation"

// ExperimentalFeaturesEnabled reports whether EnvEnableExperimentalFeatures is
// set to a truthy value.
func ExperimentalFeaturesEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvEnableExperimentalFeatures))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// RequireExperimental returns ErrExperimentalFeatureDisabled unless the
// experimental gate is set. Callers name the feature so the error says which one
// was blocked.
func RequireExperimental(feature string) error {
	if ExperimentalFeaturesEnabled() {
		return nil
	}
	name := strings.TrimSpace(feature)
	if name == "" {
		name = "this feature"
	}
	return fmt.Errorf(
		"%w: %s is experimental; set %s=true to use it",
		ErrExperimentalFeatureDisabled, name, EnvEnableExperimentalFeatures,
	)
}
