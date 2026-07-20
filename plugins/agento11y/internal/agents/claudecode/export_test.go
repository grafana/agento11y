package claudecode

import (
	"strings"
	"testing"

	"github.com/grafana/agento11y/go/sigil"
	"github.com/stretchr/testify/assert"
)

func TestExportConfigUserAgent(t *testing.T) {
	ua := exportConfig("https://sigil.example", "tenant", "token").Headers["User-Agent"]
	assert.True(t, strings.HasPrefix(ua, "sigil-plugin-claude-code/"), "got %q", ua)
	assert.True(t, strings.HasSuffix(ua, sigil.UserAgent()), "got %q", ua)
}
