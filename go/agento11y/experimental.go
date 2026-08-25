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

func experimentalFeaturesEnabled(override *bool) bool {
	if override != nil {
		return *override
	}
	return ExperimentalFeaturesEnabled()
}

func experimentalFeatureName(feature string) string {
	name := strings.TrimSpace(feature)
	if name == "" {
		return "this feature"
	}
	return name
}

func requireExperimental(feature string, override *bool) error {
	if experimentalFeaturesEnabled(override) {
		return nil
	}
	name := experimentalFeatureName(feature)
	if override != nil {
		return fmt.Errorf(
			"%w: %s is experimental; EnableExperimentalFeatures is false; set it to BoolPtr(true) when constructing the client to use this feature",
			ErrExperimentalFeatureDisabled, name,
		)
	}
	return fmt.Errorf(
		"%w: %s is experimental; set EnableExperimentalFeatures to BoolPtr(true) when constructing the client, or set %s=true",
		ErrExperimentalFeatureDisabled, name, EnvEnableExperimentalFeatures,
	)
}

// RequireExperimental returns ErrExperimentalFeatureDisabled unless the
// environment enables experimental features. Client code should use
// Client.RequireExperimental so a client-scoped override takes precedence.
func RequireExperimental(feature string) error {
	if ExperimentalFeaturesEnabled() {
		return nil
	}
	return fmt.Errorf(
		"%w: %s is experimental; set %s=true to use it",
		ErrExperimentalFeatureDisabled, experimentalFeatureName(feature), EnvEnableExperimentalFeatures,
	)
}

// RequireExperimental returns ErrExperimentalFeatureDisabled unless this
// client enables experimental features. Config.EnableExperimentalFeatures takes
// precedence over the current EnvEnableExperimentalFeatures value.
func (c *Client) RequireExperimental(feature string) error {
	if c == nil {
		return ErrNilClient
	}
	return requireExperimental(feature, c.config.EnableExperimentalFeatures)
}
