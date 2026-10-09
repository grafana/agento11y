package usagestats

import (
	"os"
	"strings"
)

// CIProviderUnknown is reported when a generic CI signal is present but no
// provider in the table matched.
const CIProviderUnknown = "unknown"

// ciProviders maps a CI provider label to the environment variable that
// signals it. The table is ported verbatim from gcx, which took it from the
// canonical ci-info list (github.com/watson/ci-info), so the two CLIs produce
// comparable labels in the same dataset. Order matters: first match wins.
//
// These variables are read for PRESENCE ONLY. Their values are never emitted,
// and must not be: CI environment variables carry repository names, branch
// names, build URLs, and sometimes tokens.
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

// CIEnvVars returns every variable DetectCI reads. Tests use it to blank the
// whole set, because otherwise the suite behaves differently on a developer
// laptop than it does under GitHub Actions and every is_ci assertion flips.
func CIEnvVars() []string {
	out := make([]string, 0, len(ciProviders)+len(genericCIVars))
	for _, p := range ciProviders {
		out = append(out, p.envVar)
	}
	return append(out, genericCIVars...)
}

// DetectCI reports the CI provider label and whether this invocation is
// running under CI. A recognised provider returns its fixed label; a generic
// CI signal with no recognised provider returns CIProviderUnknown; no CI
// returns "" and false.
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

// isEnvSet treats a variable as set when non-empty and not explicitly falsy:
// some environments export CI=false precisely to opt out of CI-specific
// behaviour, and honouring that is the point.
func isEnvSet(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "0", "false", "no":
		return false
	default:
		return true
	}
}
