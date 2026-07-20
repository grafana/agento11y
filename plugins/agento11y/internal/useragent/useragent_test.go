package useragent

import (
	"strings"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/stretchr/testify/assert"
)

func TestFor(t *testing.T) {
	old := Version
	t.Cleanup(func() { Version = old })
	Version = "1.2.3"

	for _, agent := range []string{"claude-code", "cursor", "codex", "copilot"} {
		t.Run(agent, func(t *testing.T) {
			got := For(agent)
			assert.True(t, strings.HasPrefix(got, "agento11y-plugin-"+agent+"/1.2.3 "),
				"want prefix agento11y-plugin-%s/1.2.3, got %q", agent, got)
			assert.True(t, strings.HasSuffix(got, agento11y.UserAgent()),
				"want suffix %q, got %q", agento11y.UserAgent(), got)
		})
	}
}
