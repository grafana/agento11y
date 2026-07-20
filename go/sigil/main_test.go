package sigil_test

import (
	"os"
	"testing"

	"github.com/grafana/agento11y/go/sigil/sigiltest"
)

func TestMain(m *testing.M) {
	sigiltest.ClearAmbientEnv()
	os.Exit(m.Run())
}
