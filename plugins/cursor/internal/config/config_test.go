package config

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grafana/sigil-sdk/go/sigil"
)

func TestResolveContentCapture(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want sigil.ContentCaptureMode
		warn bool
	}{
		{"empty falls back", "", sigil.ContentCaptureModeMetadataOnly, false},
		{"full", "full", sigil.ContentCaptureModeFull, false},
		{"metadata_only", "metadata_only", sigil.ContentCaptureModeMetadataOnly, false},
		{"no_tool_content", "no_tool_content", sigil.ContentCaptureModeNoToolContent, false},
		{"case insensitive", "FULL", sigil.ContentCaptureModeFull, false},
		{"default keyword", "default", sigil.ContentCaptureModeMetadataOnly, false},
		{"unknown warns and fails closed", "fll", sigil.ContentCaptureModeMetadataOnly, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := log.New(&buf, "", 0)
			got := resolveContentCapture(tc.raw, logger)
			if got != tc.want {
				t.Errorf("resolveContentCapture(%q) = %v; want %v", tc.raw, got, tc.want)
			}
			if tc.warn && !strings.Contains(buf.String(), "unknown") {
				t.Errorf("expected warning log for %q; got %q", tc.raw, buf.String())
			}
			if !tc.warn && buf.Len() > 0 {
				t.Errorf("unexpected log for %q: %q", tc.raw, buf.String())
			}
		})
	}
}

func TestLoadDotenv_AllVariants(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.env")
	body := `# leading comment
SIGIL_ENDPOINT=https://sigil.example.com
export SIGIL_AUTH_TENANT_ID=alice
SIGIL_AUTH_TOKEN="secret with spaces"
SIGIL_CONTENT_CAPTURE_MODE='full'
SIGIL_TAGS=a=1,b=2  # inline comment
SIGIL_DEBUG=true
no_equals_line
=missingkey
EMPTY=
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := LoadDotenv(path, log.New(&bytes.Buffer{}, "", 0))
	want := map[string]string{
		"SIGIL_ENDPOINT":             "https://sigil.example.com",
		"SIGIL_AUTH_TENANT_ID":       "alice",
		"SIGIL_AUTH_TOKEN":           "secret with spaces",
		"SIGIL_CONTENT_CAPTURE_MODE": "full",
		"SIGIL_TAGS":                 "a=1,b=2",
		"SIGIL_DEBUG":                "true",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d (got=%v)", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("got[%q]=%q want %q", k, got[k], v)
		}
	}
	for badKey := range map[string]struct{}{"EMPTY": {}, "": {}, "no_equals_line": {}} {
		if _, ok := got[badKey]; ok {
			t.Errorf("malformed key %q should not be loaded", badKey)
		}
	}
}

// Quoted values keep inner content verbatim — no quote stripping breakage,
// no inline-comment chopping into the value, and `#` inside quotes is
// preserved.
func TestLoadDotenv_QuotedValueEdgeCases(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.env")
	body := `DOUBLE="my secret" # comment
SINGLE='other secret' # comment
HASH_INSIDE="value # not a comment"
PLAIN_COMMENT=plain # trailing
SPACES_INSIDE="  has spaces  "
UNTERMINATED="oops
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := LoadDotenv(path, log.New(&bytes.Buffer{}, "", 0))

	cases := []struct{ key, want string }{
		{"DOUBLE", "my secret"},
		{"SINGLE", "other secret"},
		{"HASH_INSIDE", "value # not a comment"},
		{"PLAIN_COMMENT", "plain"},
		{"SPACES_INSIDE", "  has spaces  "},
		{"UNTERMINATED", `"oops`}, // unterminated quote → leave raw
	}
	for _, tc := range cases {
		if got[tc.key] != tc.want {
			t.Errorf("got[%q] = %q; want %q", tc.key, got[tc.key], tc.want)
		}
	}
}

func TestLoadDotenv_MissingFileSilent(t *testing.T) {
	var buf bytes.Buffer
	got := LoadDotenv("/nonexistent/path/config.env", log.New(&buf, "", 0))
	if len(got) != 0 {
		t.Errorf("expected empty map, got %v", got)
	}
	if buf.Len() != 0 {
		t.Errorf("missing file should not log; got %q", buf.String())
	}
}

// ApplyEnv copies dotenv values into OS env unless the OS env already has a
// non-empty value for the same key. An empty-but-set OS value is treated as
// unset so dotenv can still fill it.
func TestApplyEnv(t *testing.T) {
	cases := []struct {
		name      string
		osValue   string // "" means t.Setenv("", "") — present but empty
		osUnset   bool   // when true, the OS env var is not set at all
		fileValue string
		want      string
	}{
		{name: "OS non-empty wins over dotenv", osValue: "from-os", fileValue: "from-file", want: "from-os"},
		{name: "OS empty falls back to dotenv", osValue: "", fileValue: "from-file", want: "from-file"},
		{name: "OS unset falls back to dotenv", osUnset: true, fileValue: "from-file", want: "from-file"},
		{name: "both empty stays empty", osValue: "", fileValue: "", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", dir)
			const key = "SIGIL_ENDPOINT"
			if tc.osUnset {
				_ = os.Unsetenv(key)
				t.Cleanup(func() { _ = os.Unsetenv(key) })
			} else {
				t.Setenv(key, tc.osValue)
			}

			cfgDir := filepath.Join(dir, "sigil-cursor")
			if err := os.MkdirAll(cfgDir, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			var body string
			if tc.fileValue != "" {
				body = key + "=" + tc.fileValue + "\n"
			}
			if err := os.WriteFile(filepath.Join(cfgDir, "config.env"), []byte(body), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}

			ApplyEnv(log.New(&bytes.Buffer{}, "", 0))
			if got := os.Getenv(key); got != tc.want {
				t.Errorf("os.Getenv(%q) = %q; want %q", key, got, tc.want)
			}
		})
	}
}

func TestHasCredentials(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		tenant   string
		token    string
		want     bool
	}{
		{"all set", "https://e", "t", "k", true},
		{"missing endpoint", "", "t", "k", false},
		{"missing tenant", "https://e", "", "k", false},
		{"missing token", "https://e", "t", "", false},
		{"all empty", "", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SIGIL_ENDPOINT", tc.endpoint)
			t.Setenv("SIGIL_AUTH_TENANT_ID", tc.tenant)
			t.Setenv("SIGIL_AUTH_TOKEN", tc.token)
			if got := HasCredentials(); got != tc.want {
				t.Errorf("HasCredentials() = %v; want %v", got, tc.want)
			}
		})
	}
}
