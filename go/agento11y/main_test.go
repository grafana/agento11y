package agento11y_test

import (
	"os"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/go/agento11y/testkit"
)

func TestMain(m *testing.M) {
	testkit.ClearAmbientEnv()
	// Experimental features are opt-in for callers but on for the whole test
	// binary, so a test of an experimental feature reads like any other test.
	// ClearAmbientEnv runs first because it strips every AGENTO11Y_* var.
	// A test that asserts the gate blocks a call turns it off with t.Setenv.
	os.Setenv(agento11y.EnvEnableExperimentalFeatures, "true")
	os.Exit(m.Run())
}
