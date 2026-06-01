package useragent

import (
	"strings"
	"testing"

	"github.com/grafana/sigil-sdk/go/sigil"
	"github.com/stretchr/testify/assert"
)

func TestFor(t *testing.T) {
	old := SigilVersion
	t.Cleanup(func() { SigilVersion = old })
	SigilVersion = "1.2.3"

	for _, agent := range []string{"claude-code", "cursor", "codex", "copilot"} {
		t.Run(agent, func(t *testing.T) {
			got := For(agent)
			assert.True(t, strings.HasPrefix(got, "sigil-plugin-"+agent+"/1.2.3 "),
				"want prefix sigil-plugin-%s/1.2.3, got %q", agent, got)
			assert.True(t, strings.HasSuffix(got, sigil.UserAgent()),
				"want suffix %q, got %q", sigil.UserAgent(), got)
		})
	}
}
