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

func TestLoad_DefaultsContentCaptureToMetadataOnly(t *testing.T) {
	t.Setenv("SIGIL_CONTENT_CAPTURE_MODE", "")
	cfg := Load(log.New(&bytes.Buffer{}, "", 0))
	if cfg.ContentCapture != sigil.ContentCaptureModeMetadataOnly {
		t.Fatalf("ContentCapture = %v, want metadata_only", cfg.ContentCapture)
	}
}

func TestLoad_InvalidContentCaptureFailsClosed(t *testing.T) {
	t.Setenv("SIGIL_CONTENT_CAPTURE_MODE", "surprise")
	cfg := Load(log.New(&bytes.Buffer{}, "", 0))
	if cfg.ContentCapture != sigil.ContentCaptureModeMetadataOnly {
		t.Fatalf("ContentCapture = %v, want metadata_only", cfg.ContentCapture)
	}
}

func TestHasCredentials(t *testing.T) {
	t.Setenv("SIGIL_ENDPOINT", "https://sigil.example")
	t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
	t.Setenv("SIGIL_AUTH_TOKEN", "token")
	if !HasCredentials() {
		t.Fatal("expected credentials")
	}
	t.Setenv("SIGIL_AUTH_TOKEN", "")
	if HasCredentials() {
		t.Fatal("expected missing credentials")
	}
}

func TestLoadDotenv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.env")
	body := `# leading comment
SIGIL_ENDPOINT=https://sigil.example.com
export SIGIL_AUTH_TENANT_ID=alice
SIGIL_AUTH_TOKEN="secret with spaces"
SIGIL_CONTENT_CAPTURE_MODE='full'
SIGIL_TAGS=a=1,b=2  # inline comment
SIGIL_DEBUG=true
OTEL_EXPORTER_OTLP_ENDPOINT=https://otlp.example.com/otlp
PATH=/tmp/not-loaded
no_equals_line
=missingkey
EMPTY=
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := LoadDotenv(path, log.New(&bytes.Buffer{}, "", 0))
	want := map[string]string{
		"SIGIL_ENDPOINT":              "https://sigil.example.com",
		"SIGIL_AUTH_TENANT_ID":        "alice",
		"SIGIL_AUTH_TOKEN":            "secret with spaces",
		"SIGIL_CONTENT_CAPTURE_MODE":  "full",
		"SIGIL_TAGS":                  "a=1,b=2",
		"SIGIL_DEBUG":                 "true",
		"OTEL_EXPORTER_OTLP_ENDPOINT": "https://otlp.example.com/otlp",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d (got=%v)", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("got[%q]=%q want %q", k, got[k], v)
		}
	}
	for badKey := range map[string]struct{}{"EMPTY": {}, "": {}, "no_equals_line": {}, "PATH": {}} {
		if _, ok := got[badKey]; ok {
			t.Errorf("malformed key %q should not be loaded", badKey)
		}
	}
}

func TestApplyEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	const key = "SIGIL_ENDPOINT"
	t.Setenv(key, "")
	cfgDir := filepath.Join(dir, "sigil-copilot")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.env"), []byte(key+"=from-file\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	ApplyEnv(log.New(&bytes.Buffer{}, "", 0))
	if got := os.Getenv(key); got != "from-file" {
		t.Fatalf("%s = %q, want from-file", key, got)
	}
}

func TestFilePath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	got := FilePath()
	wantSuffix := filepath.Join("sigil-copilot", "config.env")
	if !strings.HasPrefix(got, dir) || !strings.HasSuffix(got, wantSuffix) {
		t.Fatalf("FilePath() = %q, want inside %q ending %q", got, dir, wantSuffix)
	}
}
