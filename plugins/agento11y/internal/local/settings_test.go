package local

import (
	"testing"

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
				Theme:      themeDark,
				Capture:    "", // unset: not written, so runtime defaults stand
				Tags:       []Tag{},
				Guards:     guardsOff,
				AutoUpdate: true,
			},
		},
		{
			name: "valid theme",
			env:  map[string]string{"SIGIL_THEME": "system"},
			want: Settings{Theme: themeSystem, Capture: "", Tags: []Tag{}, Guards: guardsOff, AutoUpdate: true},
		},
		{
			name: "invalid theme defaults dark",
			env:  map[string]string{"AGENTO11Y_THEME": "sepia"},
			want: Settings{Theme: themeDark, Capture: "", Tags: []Tag{}, Guards: guardsOff, AutoUpdate: true},
		},
		{
			name: "preferred theme wins over legacy",
			env: map[string]string{
				"AGENTO11Y_THEME": "light",
				"SIGIL_THEME":     "system",
			},
			want: Settings{Theme: themeLight, Capture: "", Tags: []Tag{}, Guards: guardsOff, AutoUpdate: true},
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
				"SIGIL_LOCAL_FORWARD":        "true",
			},
			want: Settings{
				Theme:        themeDark,
				Capture:      "metadata_only",
				Tags:         []Tag{{Key: "team", Value: "ai"}, {Key: "project", Value: "demo"}},
				Guards:       guardsFailClosed,
				GuardTimeout: "2000",
				Debug:        true,
				AutoUpdate:   false,
				UserID:       "alice",
				LocalForward: true,
			},
		},
		{
			name: "local forward is opt-in and only truthy values enable it",
			env:  map[string]string{"SIGIL_LOCAL_FORWARD": "nope"},
			want: Settings{Theme: themeDark, Capture: "", Tags: []Tag{}, Guards: guardsOff, AutoUpdate: true},
		},
		{
			name: "preferred local forward spelling wins over legacy",
			env: map[string]string{
				"AGENTO11Y_LOCAL_FORWARD": "true",
				"SIGIL_LOCAL_FORWARD":     "false",
			},
			want: Settings{Theme: themeDark, Capture: "", Tags: []Tag{}, Guards: guardsOff, AutoUpdate: true, LocalForward: true},
		},
		{
			name: "advanced capture mode is preserved",
			env:  map[string]string{"SIGIL_CONTENT_CAPTURE_MODE": "no_tool_content"},
			want: Settings{Theme: themeDark, Capture: "no_tool_content", Tags: []Tag{}, Guards: guardsOff, AutoUpdate: true},
		},
		{
			name: "unknown capture mode is treated as unset",
			env:  map[string]string{"SIGIL_CONTENT_CAPTURE_MODE": "bogus"},
			want: Settings{Theme: themeDark, Capture: "", Tags: []Tag{}, Guards: guardsOff, AutoUpdate: true},
		},
		{
			name: "default alias is treated as unset",
			env:  map[string]string{"SIGIL_CONTENT_CAPTURE_MODE": "default"},
			want: Settings{Theme: themeDark, Capture: "", Tags: []Tag{}, Guards: guardsOff, AutoUpdate: true},
		},
		{
			name: "guards enabled without fail-open seeds fail-open",
			env:  map[string]string{"SIGIL_GUARDS_ENABLED": "true"},
			want: Settings{Theme: themeDark, Capture: "", Tags: []Tag{}, Guards: guardsFailOpen, AutoUpdate: true},
		},
		{
			name: "auto-update only disabled by explicit falsey value",
			env:  map[string]string{"SIGIL_AUTO_UPDATE": "off"},
			want: Settings{Theme: themeDark, Capture: "", Tags: []Tag{}, Guards: guardsOff, AutoUpdate: false},
		},
		{
			name: "malformed tag pairs are dropped",
			env:  map[string]string{"SIGIL_TAGS": "team=ai,,bad,empty="},
			want: Settings{Theme: themeDark, Capture: "", Tags: []Tag{{Key: "team", Value: "ai"}}, Guards: guardsOff, AutoUpdate: true},
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
				"AGENTO11Y_TAGS":              "",
				"AGENTO11Y_GUARDS_ENABLED":    "false",
				"AGENTO11Y_GUARDS_FAIL_OPEN":  "",
				"AGENTO11Y_GUARDS_TIMEOUT_MS": "",
				"AGENTO11Y_DEBUG":             "",
				"AGENTO11Y_AUTO_UPDATE":       "",
				"AGENTO11Y_USER_ID":           "",
			},
		},
		{
			name: "defaults delete opt-in/opt-out keys",
			in:   Settings{Capture: "full", Guards: guardsOff, AutoUpdate: true},
			want: map[string]string{
				"AGENTO11Y_CONTENT_CAPTURE_MODE": "full",
				"AGENTO11Y_TAGS":                 "",
				"AGENTO11Y_GUARDS_ENABLED":       "false",
				"AGENTO11Y_GUARDS_FAIL_OPEN":     "",
				"AGENTO11Y_GUARDS_TIMEOUT_MS":    "",
				"AGENTO11Y_DEBUG":                "",
				"AGENTO11Y_AUTO_UPDATE":          "",
				"AGENTO11Y_USER_ID":              "",
			},
		},
		{
			name: "fail-open with non-default timeout writes timeout",
			in:   Settings{Capture: "full", Guards: guardsFailOpen, GuardTimeout: "2000", AutoUpdate: true},
			want: map[string]string{
				"AGENTO11Y_CONTENT_CAPTURE_MODE": "full",
				"AGENTO11Y_TAGS":                 "",
				"AGENTO11Y_GUARDS_ENABLED":       "true",
				"AGENTO11Y_GUARDS_FAIL_OPEN":     "true",
				"AGENTO11Y_GUARDS_TIMEOUT_MS":    "2000",
				"AGENTO11Y_DEBUG":                "",
				"AGENTO11Y_AUTO_UPDATE":          "",
				"AGENTO11Y_USER_ID":              "",
			},
		},
		{
			name: "default timeout value is dropped",
			in:   Settings{Capture: "full", Guards: guardsFailClosed, GuardTimeout: "1500", AutoUpdate: true},
			want: map[string]string{
				"AGENTO11Y_CONTENT_CAPTURE_MODE": "full",
				"AGENTO11Y_TAGS":                 "",
				"AGENTO11Y_GUARDS_ENABLED":       "true",
				"AGENTO11Y_GUARDS_FAIL_OPEN":     "false",
				"AGENTO11Y_GUARDS_TIMEOUT_MS":    "",
				"AGENTO11Y_DEBUG":                "",
				"AGENTO11Y_AUTO_UPDATE":          "",
				"AGENTO11Y_USER_ID":              "",
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
				"AGENTO11Y_CONTENT_CAPTURE_MODE": "metadata_only",
				"AGENTO11Y_TAGS":                 "team=ai",
				"AGENTO11Y_GUARDS_ENABLED":       "false",
				"AGENTO11Y_GUARDS_FAIL_OPEN":     "",
				"AGENTO11Y_GUARDS_TIMEOUT_MS":    "",
				"AGENTO11Y_DEBUG":                "true",
				"AGENTO11Y_AUTO_UPDATE":          "false",
				"AGENTO11Y_USER_ID":              "alice",
			},
		},
		{
			name: "non-numeric timeout is treated as default",
			in:   Settings{Capture: "full", Guards: guardsFailOpen, GuardTimeout: "abc", AutoUpdate: true},
			want: map[string]string{
				"AGENTO11Y_CONTENT_CAPTURE_MODE": "full",
				"AGENTO11Y_TAGS":                 "",
				"AGENTO11Y_GUARDS_ENABLED":       "true",
				"AGENTO11Y_GUARDS_FAIL_OPEN":     "true",
				"AGENTO11Y_GUARDS_TIMEOUT_MS":    "",
				"AGENTO11Y_DEBUG":                "",
				"AGENTO11Y_AUTO_UPDATE":          "",
				"AGENTO11Y_USER_ID":              "",
			},
		},
		{
			name: "local forward on is written",
			in:   Settings{Capture: "full", Guards: guardsOff, AutoUpdate: true, LocalForward: true},
			want: map[string]string{
				"AGENTO11Y_CONTENT_CAPTURE_MODE": "full",
				"AGENTO11Y_TAGS":                 "",
				"AGENTO11Y_GUARDS_ENABLED":       "false",
				"AGENTO11Y_GUARDS_FAIL_OPEN":     "",
				"AGENTO11Y_GUARDS_TIMEOUT_MS":    "",
				"AGENTO11Y_DEBUG":                "",
				"AGENTO11Y_AUTO_UPDATE":          "",
				"AGENTO11Y_USER_ID":              "",
				"AGENTO11Y_LOCAL_FORWARD":        "true",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// None of these cases set connection fields, so Updates always emits
			// empty (delete) markers for them and no token key.
			want := tc.want
			want["AGENTO11Y_THEME"] = string(normalizeTheme(tc.in.Theme))
			want["AGENTO11Y_ENDPOINT"] = ""
			want["AGENTO11Y_AUTH_TENANT_ID"] = ""
			want["AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT"] = ""
			// LOCAL_FORWARD is written explicitly in both directions: only a
			// literal false on disk can override the value the daemon
			// materialized into its own environment at boot.
			if _, ok := want["AGENTO11Y_LOCAL_FORWARD"]; !ok {
				want["AGENTO11Y_LOCAL_FORWARD"] = "false"
			}
			assert.Equal(t, want, tc.in.Updates())
		})
	}

	themeTests := []struct {
		name  string
		theme Theme
		want  string
	}{
		{name: "missing theme normalizes dark", want: "dark"},
		{name: "dark theme", theme: themeDark, want: "dark"},
		{name: "light theme", theme: themeLight, want: "light"},
		{name: "system theme", theme: themeSystem, want: "system"},
		{name: "invalid theme normalizes dark", theme: "sepia", want: "dark"},
	}
	for _, tc := range themeTests {
		t.Run(tc.name, func(t *testing.T) {
			u := (Settings{Theme: tc.theme, Guards: guardsOff, AutoUpdate: true}).Updates()
			assert.Equal(t, tc.want, u["AGENTO11Y_THEME"])
			assert.NotContains(t, u, "SIGIL_THEME")
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
		assert.NotContains(t, u, "AGENTO11Y_AUTH_TOKEN")
		assert.NotContains(t, u, "SIGIL_AUTH_TOKEN")
		assert.Equal(t, "https://x", u["AGENTO11Y_ENDPOINT"])
	})

	t.Run("new token value is written", func(t *testing.T) {
		u := Settings{Guards: guardsOff, AutoUpdate: true, TokenSet: true, Token: "glc_new"}.Updates()
		assert.Equal(t, "glc_new", u["AGENTO11Y_AUTH_TOKEN"])
		assert.NotContains(t, u, "SIGIL_AUTH_TOKEN")
	})

	t.Run("cleared token is deleted", func(t *testing.T) {
		u := Settings{Guards: guardsOff, AutoUpdate: true, TokenSet: true, TokenCleared: true}.Updates()
		v, ok := u["AGENTO11Y_AUTH_TOKEN"]
		assert.True(t, ok)
		assert.Empty(t, v) // empty value = delete in WriteDotenv
		assert.NotContains(t, u, "SIGIL_AUTH_TOKEN")
	})

	t.Run("preview masks a set token and never shows the value", func(t *testing.T) {
		p := Settings{Guards: guardsOff, AutoUpdate: true, TokenSet: true, Token: "glc_new"}.previewUpdates()
		assert.Equal(t, tokenMask, p["AGENTO11Y_AUTH_TOKEN"])
		for key := range p {
			assert.NotRegexp(t, `^SIGIL_`, key)
		}

		cleared := Settings{Guards: guardsOff, AutoUpdate: true, TokenSet: true, TokenCleared: true}.previewUpdates()
		assert.NotContains(t, cleared, "AGENTO11Y_AUTH_TOKEN")
		assert.NotContains(t, cleared, "SIGIL_AUTH_TOKEN")
	})

	// OTEL_EXPORTER_OTLP_HEADERS carries a second copy of the OTLP credential,
	// so it is write-only and tri-state like the token. It has no branded
	// spelling: the raw key is the only one read and written.
	t.Run("otlp headers are reported as set but never read back", func(t *testing.T) {
		got := ParseSettings(map[string]string{"OTEL_EXPORTER_OTLP_HEADERS": "Authorization=Basic c2VjcmV0"})
		assert.True(t, got.OtlpHeadersSet)
		assert.Empty(t, got.OtlpHeaders)
	})

	t.Run("blank otlp headers are omitted so the writer preserves them", func(t *testing.T) {
		u := Settings{Guards: guardsOff, AutoUpdate: true, OtlpHeadersSet: true}.Updates()
		_, ok := u["OTEL_EXPORTER_OTLP_HEADERS"]
		assert.False(t, ok)
	})

	t.Run("new otlp headers value is written, and only under the raw key", func(t *testing.T) {
		u := Settings{Guards: guardsOff, AutoUpdate: true, OtlpHeaders: "Authorization=Basic c2VjcmV0"}.Updates()
		assert.Equal(t, "Authorization=Basic c2VjcmV0", u["OTEL_EXPORTER_OTLP_HEADERS"])
		assert.NotContains(t, u, "AGENTO11Y_OTEL_EXPORTER_OTLP_HEADERS")
		assert.NotContains(t, u, "SIGIL_OTEL_EXPORTER_OTLP_HEADERS")
	})

	t.Run("cleared otlp headers are deleted", func(t *testing.T) {
		u := Settings{Guards: guardsOff, AutoUpdate: true, OtlpHeadersSet: true, OtlpHeadersCleared: true}.Updates()
		v, ok := u["OTEL_EXPORTER_OTLP_HEADERS"]
		assert.True(t, ok)
		assert.Empty(t, v)
	})

	// The saved headers carry their own copy of the OTLP credential, and
	// otel.ExporterHeaders prefers an explicit Authorization entry over the Basic
	// auth it synthesizes from the tenant ID and the token. A write that replaces
	// or removes the token therefore drops them, the way `agento11y login` drops
	// them when a new token is entered and no block was pasted. Without this, the
	// Reset button in Edit connection leaves OTLP authenticating with the token
	// the user just removed.
	t.Run("a token write drops the saved otlp headers", func(t *testing.T) {
		tests := []struct {
			name     string
			settings Settings
			written  bool // the key is part of the write; absent means the file keeps its value
			value    string
			masked   bool // the preview still shows a header line
		}{
			{name: "token reset", settings: Settings{TokenCleared: true}, written: true},
			{name: "new token", settings: Settings{Token: "glc_new"}, written: true},
			{
				name:     "headers pasted with the token win",
				settings: Settings{Token: "glc_new", OtlpHeaders: "Authorization=Basic bmV3"},
				written:  true,
				value:    "Authorization=Basic bmV3",
				masked:   true,
			},
			{name: "token untouched", settings: Settings{}, masked: true},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				s := tt.settings
				s.Guards, s.AutoUpdate = guardsOff, true
				s.TokenSet, s.OtlpHeadersSet = true, true

				v, ok := s.Updates()["OTEL_EXPORTER_OTLP_HEADERS"]
				assert.Equal(t, tt.written, ok)
				assert.Equal(t, tt.value, v)

				_, shown := s.previewUpdates()["OTEL_EXPORTER_OTLP_HEADERS"]
				assert.Equal(t, tt.masked, shown)
			})
		}
	})

	t.Run("preview masks set otlp headers", func(t *testing.T) {
		p := Settings{Guards: guardsOff, AutoUpdate: true, OtlpHeaders: "Authorization=Basic c2VjcmV0"}.previewUpdates()
		assert.Equal(t, tokenMask, p["OTEL_EXPORTER_OTLP_HEADERS"])

		cleared := Settings{Guards: guardsOff, AutoUpdate: true, OtlpHeadersSet: true, OtlpHeadersCleared: true}.previewUpdates()
		_, ok := cleared["OTEL_EXPORTER_OTLP_HEADERS"]
		assert.False(t, ok)
	})
}

// TestSettingsRoundTrip confirms parsing the keys Updates writes yields back
// the same Settings (after default-dropping normalisation), so the saved
// snapshot the server returns is stable.
func TestSettingsRoundTrip(t *testing.T) {
	in := Settings{
		Theme:        themeSystem,
		Capture:      "no_tool_content",
		Tags:         []Tag{{Key: "team", Value: "ai"}},
		Guards:       guardsFailClosed,
		GuardTimeout: "3000",
		Debug:        true,
		AutoUpdate:   false,
		UserID:       "alice",
		LocalForward: true,
	}
	got := ParseSettings(in.Updates())
	assert.Equal(t, in, got)
}
