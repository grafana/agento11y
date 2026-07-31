package experiments

import (
	"os"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/go/agento11y/testkit"
)

func TestMain(m *testing.M) {
	testkit.ClearAmbientEnv()
	// See the note in go/agento11y/main_test.go: the gate is on for the test
	// binary, and a test that asserts it blocks a call turns it off with t.Setenv.
	os.Setenv(agento11y.EnvEnableExperimentalFeatures, "true")
	os.Exit(m.Run())
}
