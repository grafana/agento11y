package usagestats

import "testing"

func TestDetectCI(t *testing.T) {
	tests := []struct {
		name         string
		env          map[string]string
		wantProvider string
		wantIsCI     bool
	}{
		{name: "no ci", wantProvider: "", wantIsCI: false},
		{name: "github actions", env: map[string]string{"GITHUB_ACTIONS": "true"}, wantProvider: "github_actions", wantIsCI: true},
		{name: "gitlab", env: map[string]string{"GITLAB_CI": "true"}, wantProvider: "gitlab", wantIsCI: true},
		{name: "jenkins", env: map[string]string{"JENKINS_URL": "https://ci.example.com"}, wantProvider: "jenkins", wantIsCI: true},
		{name: "drone", env: map[string]string{"DRONE": "true"}, wantProvider: "drone", wantIsCI: true},

		// A generic signal with no recognised provider is still CI.
		{name: "generic ci", env: map[string]string{"CI": "true"}, wantProvider: CIProviderUnknown, wantIsCI: true},
		{name: "continuous integration", env: map[string]string{"CONTINUOUS_INTEGRATION": "1"}, wantProvider: CIProviderUnknown, wantIsCI: true},
		{name: "build number", env: map[string]string{"BUILD_NUMBER": "42"}, wantProvider: CIProviderUnknown, wantIsCI: true},

		// Some environments export CI=false precisely to opt out, and
		// honouring that is the point of isEnvSet.
		{name: "ci false opts out", env: map[string]string{"CI": "false"}, wantProvider: "", wantIsCI: false},
		{name: "ci zero opts out", env: map[string]string{"CI": "0"}, wantProvider: "", wantIsCI: false},
		{name: "ci no opts out", env: map[string]string{"CI": "no"}, wantProvider: "", wantIsCI: false},
		{name: "ci blank opts out", env: map[string]string{"CI": "  "}, wantProvider: "", wantIsCI: false},

		// A named provider wins over the generic signal, which both are
		// usually set in real CI.
		{name: "provider beats generic", env: map[string]string{"CI": "true", "GITHUB_ACTIONS": "true"}, wantProvider: "github_actions", wantIsCI: true},

		// First match wins, by table order.
		{name: "first match wins", env: map[string]string{"GITLAB_CI": "true", "GITHUB_ACTIONS": "true"}, wantProvider: "github_actions", wantIsCI: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(key string) string { return tc.env[key] }
			provider, isCI := detectCI(getenv)
			if provider != tc.wantProvider || isCI != tc.wantIsCI {
				t.Errorf("detectCI() = %q, %v; want %q, %v", provider, isCI, tc.wantProvider, tc.wantIsCI)
			}
		})
	}
}

// TestCIEnvVarsCoversTheTable keeps the test-isolation helper honest: a
// provider added to the table but missing from CIEnvVars would not be scrubbed
// by TestMain, and the suite would then behave differently under that CI
// system than anywhere else.
func TestCIEnvVarsCoversTheTable(t *testing.T) {
	covered := map[string]bool{}
	for _, key := range CIEnvVars() {
		covered[key] = true
	}
	for _, p := range ciProviders {
		if !covered[p.envVar] {
			t.Errorf("CIEnvVars() omits %q (provider %q)", p.envVar, p.name)
		}
	}
	for _, key := range genericCIVars {
		if !covered[key] {
			t.Errorf("CIEnvVars() omits generic %q", key)
		}
	}
}

// TestCIProviderLabelsAreStable pins the labels themselves. They are a closed
// vocabulary shared with gcx in the same dataset, so renaming one silently
// splits a provider's rows across two names.
func TestCIProviderLabelsAreStable(t *testing.T) {
	want := []string{
		"github_actions", "gitlab", "circleci", "jenkins", "buildkite",
		"azure_pipelines", "travis", "teamcity", "bitbucket_pipelines",
		"drone", "aws_codebuild", "google_cloud_build", "semaphore",
		"appveyor", "woodpecker",
	}
	if len(ciProviders) != len(want) {
		t.Fatalf("provider table has %d entries, want %d", len(ciProviders), len(want))
	}
	for i, name := range want {
		if ciProviders[i].name != name {
			t.Errorf("ciProviders[%d].name = %q, want %q", i, ciProviders[i].name, name)
		}
	}
}
