package usagestats

import (
	"os"
	"testing"

	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
)

// unroutableEndpoint is a belt-and-braces default for the whole package: even
// a bug that bypassed mode resolution entirely cannot then reach
// stats.grafana.org from a developer laptop or from CI. Port 0 is not
// connectable, so an accidental export fails immediately instead of hanging.
const unroutableEndpoint = "http://127.0.0.1:0/must-not-be-reached"

// TestMain isolates the package from the ambient environment.
//
// The stakes here are higher than for a normal suite: this package's whole job
// is to POST to a Grafana-operated endpoint, so an un-isolated run would file
// real usage events from a developer's machine and pollute the dataset with
// test traffic.
//
// Three groups are scrubbed:
//
//   - Both spellings of every alias family, so a developer shell exporting
//     AGENTO11Y_ANONYMOUS_USAGE_STATS cannot decide the mode for a test that
//     did not set it.
//   - DO_NOT_TRACK, which is unbranded and so is not covered by the alias
//     scrub, and which a privacy-minded developer plausibly has set.
//   - Every CI variable DetectCI reads. Without this the suite behaves
//     differently on a laptop than under GitHub Actions and every is_ci
//     assertion flips depending on where it runs.
func TestMain(m *testing.M) {
	for _, suffix := range envconfig.AliasSuffixes {
		_ = os.Unsetenv(envconfig.PreferredKey(suffix))
		_ = os.Unsetenv(envconfig.LegacyKey(suffix))
	}
	_ = os.Unsetenv(EnvDoNotTrack)
	for _, key := range CIEnvVars() {
		_ = os.Unsetenv(key)
	}
	_ = os.Setenv(EnvEndpoint, unroutableEndpoint)

	tmp, err := os.MkdirTemp("", "agento11y-usagestats-test-home-*")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("HOME", tmp)
	_ = os.Setenv("XDG_STATE_HOME", tmp+"/state")

	code := m.Run()
	_ = os.RemoveAll(tmp)
	os.Exit(code)
}

// TestPackageEnvIsScrubbed pins the TestMain guard. A regression here is not a
// failing assertion somewhere else — it is test traffic reaching the real
// receiver — so it gets its own test rather than being left implicit.
func TestPackageEnvIsScrubbed(t *testing.T) {
	for _, key := range append(CIEnvVars(), EnvDoNotTrack) {
		if v := os.Getenv(key); v != "" {
			t.Errorf("%s = %q at test start; TestMain must clear it", key, v)
		}
	}
	if got := os.Getenv(EnvEndpoint); got != unroutableEndpoint {
		t.Errorf("endpoint override = %q, want the unroutable default %q", got, unroutableEndpoint)
	}
	if got := endpoint(); got == DefaultEndpoint {
		t.Fatalf("endpoint() resolved to the real receiver %q during tests", got)
	}
}
