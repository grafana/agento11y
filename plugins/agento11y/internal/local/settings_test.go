package local

import (
	"testing"

	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/stretchr/testify/assert"
)

func TestParseSettings(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want Settings
	}{
		{
			name: "empty config leaves capture unset",
			env:  map[string]string{},
			want: Settings{
				Capture:    "", // unset: not written, so runtime defaults stand
				Tags:       []Tag{},
				Guards:     guardsOff,
				AutoUpdate: true,
			},
		},
		{
			name: "full config round-trips every field",
			env: map[string]string{
				"SIGIL_CONTENT_CAPTURE_MODE": "metadata_only",
				"SIGIL_TAGS":                 "team=ai,project=demo",
				"SIGIL_GUARDS_ENABLED":       "true",
				"SIGIL_GUARDS_FAIL_OPEN":     "false",
				"SIGIL_GUARDS_TIMEOUT_MS":    "2000",
				"SIGIL_DEBUG":                "true",
				"SIGIL_AUTO_UPDATE":          "false",
				"SIGIL_USER_ID":              "alice",
			},
			want: Settings{
				Capture:      "metadata_only",
				Tags:         []Tag{{Key: "team", Value: "ai"}, {Key: "project", Value: "demo"}},
				Guards:       guardsFailClosed,
				GuardTimeout: "2000",
				Debug:        true,
				AutoUpdate:   false,
				UserID:       "alice",
			},
		},
		{
			name: "advanced capture mode is preserved",
			env:  map[string]string{"SIGIL_CONTENT_CAPTURE_MODE": "no_tool_content"},
			want: Settings{Capture: "no_tool_content", Tags: []Tag{}, Guards: guardsOff, AutoUpdate: true},
		},
		{
			name: "unknown capture mode is treated as unset",
			env:  map[string]string{"SIGIL_CONTENT_CAPTURE_MODE": "bogus"},
			want: Settings{Capture: "", Tags: []Tag{}, Guards: guardsOff, AutoUpdate: true},
		},
		{
			name: "default alias is treated as unset",
			env:  map[string]string{"SIGIL_CONTENT_CAPTURE_MODE": "default"},
			want: Settings{Capture: "", Tags: []Tag{}, Guards: guardsOff, AutoUpdate: true},
		},
		{
			name: "guards enabled without fail-open seeds fail-open",
			env:  map[string]string{"SIGIL_GUARDS_ENABLED": "true"},
			want: Settings{Capture: "", Tags: []Tag{}, Guards: guardsFailOpen, AutoUpdate: true},
		},
		{
			name: "auto-update only disabled by explicit falsey value",
			env:  map[string]string{"SIGIL_AUTO_UPDATE": "off"},
			want: Settings{Capture: "", Tags: []Tag{}, Guards: guardsOff, AutoUpdate: false},
		},
		{
			name: "malformed tag pairs are dropped",
			env:  map[string]string{"SIGIL_TAGS": "team=ai,,bad,empty="},
			want: Settings{Capture: "", Tags: []Tag{{Key: "team", Value: "ai"}}, Guards: guardsOff, AutoUpdate: true},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ParseSettings(tc.env))
		})
	}
}

func TestSettingsUpdates(t *testing.T) {
	tests := []struct {
		name string
		in   Settings
		want map[string]string
	}{
		{
			name: "unset capture is not written",
			in:   Settings{Capture: "", Guards: guardsOff, AutoUpdate: true},
			want: map[string]string{
				"SIGIL_TAGS":              "",
				"SIGIL_GUARDS_ENABLED":    "false",
				"SIGIL_GUARDS_FAIL_OPEN":  "",
				"SIGIL_GUARDS_TIMEOUT_MS": "",
				"SIGIL_DEBUG":             "",
				"SIGIL_AUTO_UPDATE":       "",
				"SIGIL_USER_ID":           "",
			},
		},
		{
			name: "defaults delete opt-in/opt-out keys",
			in:   Settings{Capture: "full", Guards: guardsOff, AutoUpdate: true},
			want: map[string]string{
				"SIGIL_CONTENT_CAPTURE_MODE": "full",
				"SIGIL_TAGS":                 "",
				"SIGIL_GUARDS_ENABLED":       "false",
				"SIGIL_GUARDS_FAIL_OPEN":     "",
				"SIGIL_GUARDS_TIMEOUT_MS":    "",
				"SIGIL_DEBUG":                "",
				"SIGIL_AUTO_UPDATE":          "",
				"SIGIL_USER_ID":              "",
			},
		},
		{
			name: "fail-open with non-default timeout writes timeout",
			in:   Settings{Capture: "full", Guards: guardsFailOpen, GuardTimeout: "2000", AutoUpdate: true},
			want: map[string]string{
				"SIGIL_CONTENT_CAPTURE_MODE": "full",
				"SIGIL_TAGS":                 "",
				"SIGIL_GUARDS_ENABLED":       "true",
				"SIGIL_GUARDS_FAIL_OPEN":     "true",
				"SIGIL_GUARDS_TIMEOUT_MS":    "2000",
				"SIGIL_DEBUG":                "",
				"SIGIL_AUTO_UPDATE":          "",
				"SIGIL_USER_ID":              "",
			},
		},
		{
			name: "default timeout value is dropped",
			in:   Settings{Capture: "full", Guards: guardsFailClosed, GuardTimeout: "1500", AutoUpdate: true},
			want: map[string]string{
				"SIGIL_CONTENT_CAPTURE_MODE": "full",
				"SIGIL_TAGS":                 "",
				"SIGIL_GUARDS_ENABLED":       "true",
				"SIGIL_GUARDS_FAIL_OPEN":     "false",
				"SIGIL_GUARDS_TIMEOUT_MS":    "",
				"SIGIL_DEBUG":                "",
				"SIGIL_AUTO_UPDATE":          "",
				"SIGIL_USER_ID":              "",
			},
		},
		{
			name: "debug on, auto-update off, tags and user id set",
			in: Settings{
				Capture:    "metadata_only",
				Tags:       []Tag{{Key: "team", Value: "ai"}, {Key: "drop", Value: ""}},
				Guards:     guardsOff,
				Debug:      true,
				AutoUpdate: false,
				UserID:     "  alice  ",
			},
			want: map[string]string{
				"SIGIL_CONTENT_CAPTURE_MODE": "metadata_only",
				"SIGIL_TAGS":                 "team=ai",
				"SIGIL_GUARDS_ENABLED":       "false",
				"SIGIL_GUARDS_FAIL_OPEN":     "",
				"SIGIL_GUARDS_TIMEOUT_MS":    "",
				"SIGIL_DEBUG":                "true",
				"SIGIL_AUTO_UPDATE":          "false",
				"SIGIL_USER_ID":              "alice",
			},
		},
		{
			name: "non-numeric timeout is treated as default",
			in:   Settings{Capture: "full", Guards: guardsFailOpen, GuardTimeout: "abc", AutoUpdate: true},
			want: map[string]string{
				"SIGIL_CONTENT_CAPTURE_MODE": "full",
				"SIGIL_TAGS":                 "",
				"SIGIL_GUARDS_ENABLED":       "true",
				"SIGIL_GUARDS_FAIL_OPEN":     "true",
				"SIGIL_GUARDS_TIMEOUT_MS":    "",
				"SIGIL_DEBUG":                "",
				"SIGIL_AUTO_UPDATE":          "",
				"SIGIL_USER_ID":              "",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// None of these cases set connection fields, so Updates always emits
			// empty (delete) markers for them and no token key. Every managed
			// key is written and deleted under both spellings.
			want := tc.want
			want["SIGIL_ENDPOINT"] = ""
			want["SIGIL_AUTH_TENANT_ID"] = ""
			want["SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT"] = ""
			assert.Equal(t, envconfig.ExpandAliases(want), tc.in.Updates())
		})
	}
}

// TestSettingsConnection covers the connection fields and the write-only,
// tri-state auth token (keep / replace / remove).
func TestSettingsConnection(t *testing.T) {
	t.Run("parse never reads the token back but reports it is set", func(t *testing.T) {
		got := ParseSettings(map[string]string{
			"SIGIL_ENDPOINT":                    "https://sigil.example.net",
			"SIGIL_AUTH_TENANT_ID":              "12345",
			"SIGIL_AUTH_TOKEN":                  "glc_secret",
			"SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT": "https://otlp.example.net/otlp",
		})
		assert.Equal(t, "https://sigil.example.net", got.Endpoint)
		assert.Equal(t, "12345", got.TenantID)
		assert.Equal(t, "https://otlp.example.net/otlp", got.OtlpEndpoint)
		assert.True(t, got.TokenSet)
		assert.Empty(t, got.Token)
	})

	t.Run("blank token is omitted so the writer preserves it", func(t *testing.T) {
		u := Settings{Endpoint: "https://x", Guards: guardsOff, AutoUpdate: true, TokenSet: true}.Updates()
		_, ok := u["SIGIL_AUTH_TOKEN"]
		assert.False(t, ok)
		assert.Equal(t, "https://x", u["SIGIL_ENDPOINT"])
	})

	t.Run("new token value is written", func(t *testing.T) {
		u := Settings{Guards: guardsOff, AutoUpdate: true, TokenSet: true, Token: "glc_new"}.Updates()
		assert.Equal(t, "glc_new", u["SIGIL_AUTH_TOKEN"])
	})

	t.Run("cleared token is deleted", func(t *testing.T) {
		u := Settings{Guards: guardsOff, AutoUpdate: true, TokenSet: true, TokenCleared: true}.Updates()
		v, ok := u["SIGIL_AUTH_TOKEN"]
		assert.True(t, ok)
		assert.Empty(t, v) // empty value = delete in WriteDotenv
	})

	t.Run("preview masks a set token and never shows the value", func(t *testing.T) {
		p := Settings{Guards: guardsOff, AutoUpdate: true, TokenSet: true, Token: "glc_new"}.previewUpdates()
		assert.Equal(t, tokenMask, p["SIGIL_AUTH_TOKEN"])

		cleared := Settings{Guards: guardsOff, AutoUpdate: true, TokenSet: true, TokenCleared: true}.previewUpdates()
		_, ok := cleared["SIGIL_AUTH_TOKEN"]
		assert.False(t, ok)
	})
}

// TestSettingsRoundTrip confirms parsing the keys Updates writes yields back
// the same Settings (after default-dropping normalisation), so the saved
// snapshot the server returns is stable.
func TestSettingsRoundTrip(t *testing.T) {
	in := Settings{
		Capture:      "no_tool_content",
		Tags:         []Tag{{Key: "team", Value: "ai"}},
		Guards:       guardsFailClosed,
		GuardTimeout: "3000",
		Debug:        true,
		AutoUpdate:   false,
		UserID:       "alice",
	}
	got := ParseSettings(in.Updates())
	assert.Equal(t, in, got)
}
