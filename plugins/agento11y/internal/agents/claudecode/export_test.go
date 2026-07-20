package claudecode

import (
	"strings"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/stretchr/testify/assert"
)

func TestExportConfigUserAgent(t *testing.T) {
	ua := exportConfig("https://sigil.example", "tenant", "token").Headers["User-Agent"]
	assert.True(t, strings.HasPrefix(ua, "agento11y-plugin-claude-code/"), "got %q", ua)
	assert.True(t, strings.HasSuffix(ua, agento11y.UserAgent()), "got %q", ua)
}
