package googleadk_test

import (
	"os"
	"testing"

	"github.com/grafana/agento11y/go/agento11y/testkit"
)

func TestMain(m *testing.M) {
	testkit.ClearAmbientEnv()
	os.Exit(m.Run())
}
