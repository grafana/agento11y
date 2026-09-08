package claudecode

import (
	"strings"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/stretchr/testify/assert"
)

func TestExportConfigEndpoint(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		want     string
	}{
		{"base URL", "https://sigil.example", "https://sigil.example/api/v1/generations:export"},
		{"trailing slash trimmed", "https://sigil.example/", "https://sigil.example/api/v1/generations:export"},
		{"pasted full export URL", "https://sigil.example/api/v1/generations:export", "https://sigil.example/api/v1/generations:export"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, exportConfig(tc.endpoint, "tenant", "token").Endpoint)
		})
	}
}

func TestExportConfigUserAgent(t *testing.T) {
	ua := exportConfig("https://sigil.example", "tenant", "token").Headers["User-Agent"]
	assert.True(t, strings.HasPrefix(ua, "agento11y-plugin-claude-code/"), "got %q", ua)
	assert.True(t, strings.HasSuffix(ua, agento11y.UserAgent()), "got %q", ua)
}
