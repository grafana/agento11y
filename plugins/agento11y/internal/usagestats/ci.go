package usagestats

import (
	"os"
	"strings"
)

// CIProviderUnknown is reported when a generic CI signal is present but no
// provider in the table matched.
const CIProviderUnknown = "unknown"

// ciProviders maps a provider label to its signature variable, following the
// ci-info list (github.com/watson/ci-info). First match wins.
//
// Read for PRESENCE ONLY: the values carry repo names, URLs, sometimes tokens.
var ciProviders = []struct{ name, envVar string }{
	{"github_actions", "GITHUB_ACTIONS"},
	{"gitlab", "GITLAB_CI"},
	{"circleci", "CIRCLECI"},
	{"jenkins", "JENKINS_URL"},
	{"buildkite", "BUILDKITE"},
	{"azure_pipelines", "TF_BUILD"},
	{"travis", "TRAVIS"},
	{"teamcity", "TEAMCITY_VERSION"},
	{"bitbucket_pipelines", "BITBUCKET_COMMIT"},
	{"drone", "DRONE"},
	{"aws_codebuild", "CODEBUILD_BUILD_ARN"},
	{"google_cloud_build", "BUILDER_OUTPUT"},
	{"semaphore", "SEMAPHORE"},
	{"appveyor", "APPVEYOR"},
	{"woodpecker", "WOODPECKER"},
}

// genericCIVars signal CI without identifying the provider.
var genericCIVars = []string{"CI", "CONTINUOUS_INTEGRATION", "BUILD_NUMBER"}

// CIEnvVars returns every variable DetectCI reads, so tests can blank the set
// and behave the same locally as in CI.
func CIEnvVars() []string {
	out := make([]string, 0, len(ciProviders)+len(genericCIVars))
	for _, p := range ciProviders {
		out = append(out, p.envVar)
	}
	return append(out, genericCIVars...)
}

// DetectCI reports the provider label and whether this is CI. A generic
// signal with no recognised provider returns CIProviderUnknown.
func DetectCI() (provider string, isCI bool) {
	return detectCI(os.Getenv)
}

func detectCI(getenv func(string) string) (string, bool) {
	for _, p := range ciProviders {
		if isEnvSet(getenv(p.envVar)) {
			return p.name, true
		}
	}
	for _, v := range genericCIVars {
		if isEnvSet(getenv(v)) {
			return CIProviderUnknown, true
		}
	}
	return "", false
}

// isEnvSet ignores explicitly falsy values: some environments export CI=false
// precisely to opt out.
func isEnvSet(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "0", "false", "no":
		return false
	default:
		return true
	}
}
