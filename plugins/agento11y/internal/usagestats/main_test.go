package usagestats

import (
	"os"
	"testing"

	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
)

// unroutableEndpoint means even a bug bypassing mode resolution cannot send
// anything. Port 0 fails immediately rather than hanging.
const unroutableEndpoint = "http://127.0.0.1:0/must-not-be-reached"

// TestMain isolates the package: this code POSTs to a real endpoint, so an
// un-isolated run would report test traffic as genuine usage.
//
// Scrubs both spellings of every alias family, DO_NOT_TRACK (unbranded, so not
// covered by that scrub), and every CI variable DetectCI reads.
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

// TestPackageEnvIsScrubbed pins the TestMain guard: a regression here sends
// test traffic for real, rather than just failing an assertion.
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
