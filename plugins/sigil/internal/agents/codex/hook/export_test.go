package hook

import (
	"strings"
	"testing"

	"github.com/grafana/sigil-sdk/go/sigil"
	"github.com/stretchr/testify/assert"
)

func TestExportConfigUserAgent(t *testing.T) {
	ua := exportConfig().Headers["User-Agent"]
	assert.True(t, strings.HasPrefix(ua, "sigil-plugin-codex/"), "got %q", ua)
	assert.True(t, strings.HasSuffix(ua, sigil.UserAgent()), "got %q", ua)
}
