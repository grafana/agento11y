package dotenv

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteDotenv_CreatesFileAndDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "config.env")

	updates := map[string]string{
		"SIGIL_ENDPOINT":       "https://sigil.example.com",
		"SIGIL_AUTH_TENANT_ID": "tenant-a",
		"SIGIL_AUTH_TOKEN":     "secret",
	}
	if err := WriteDotenv(path, updates, log.New(&bytes.Buffer{}, "", 0)); err != nil {
		t.Fatalf("WriteDotenv: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("perm = %o, want 0600", perm)
	}

	got := LoadDotenv(path, log.New(&bytes.Buffer{}, "", 0))
	for k, want := range updates {
		if got[k] != want {
			t.Errorf("LoadDotenv[%q] = %q, want %q", k, got[k], want)
		}
	}
}

func TestWriteDotenv_PreservesExistingAllowedKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.env")
	initial := `SIGIL_ENDPOINT=https://old.example
SIGIL_AUTH_TENANT_ID=old-tenant
SIGIL_AUTH_TOKEN=old-token
SIGIL_TAGS=team=foo
OTEL_EXPORTER_OTLP_ENDPOINT=https://otlp.example.com
`
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	updates := map[string]string{
		"SIGIL_ENDPOINT":   "https://new.example",
		"SIGIL_AUTH_TOKEN": "new-token",
	}
	if err := WriteDotenv(path, updates, log.New(&bytes.Buffer{}, "", 0)); err != nil {
		t.Fatalf("WriteDotenv: %v", err)
	}
	got := LoadDotenv(path, log.New(&bytes.Buffer{}, "", 0))
	want := map[string]string{
		"SIGIL_ENDPOINT":              "https://new.example",
		"SIGIL_AUTH_TENANT_ID":        "old-tenant",
		"SIGIL_AUTH_TOKEN":            "new-token",
		"SIGIL_TAGS":                  "team=foo",
		"OTEL_EXPORTER_OTLP_ENDPOINT": "https://otlp.example.com",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("merged[%q] = %q, want %q", k, got[k], v)
		}
	}
}

func TestWriteDotenv_EmptyValueDeletesKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.env")
	initial := `SIGIL_ENDPOINT=https://old.example
SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT=https://otlp.old
`
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	updates := map[string]string{
		"SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT": "",
	}
	if err := WriteDotenv(path, updates, nil); err != nil {
		t.Fatalf("WriteDotenv: %v", err)
	}
	got := LoadDotenv(path, nil)
	if _, ok := got["SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT"]; ok {
		t.Errorf("expected SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT removed, got %q", got["SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT"])
	}
	if got["SIGIL_ENDPOINT"] != "https://old.example" {
		t.Errorf("SIGIL_ENDPOINT = %q, want preserved", got["SIGIL_ENDPOINT"])
	}
}

func TestWriteDotenv_QuotesValuesWithWhitespace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.env")
	updates := map[string]string{
		"SIGIL_ENDPOINT":   "https://sigil.example",
		"SIGIL_AUTH_TOKEN": "token with spaces and # hash",
	}
	if err := WriteDotenv(path, updates, nil); err != nil {
		t.Fatalf("WriteDotenv: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(raw), `SIGIL_AUTH_TOKEN="token with spaces and # hash"`) {
		t.Errorf("expected quoted token line, got:\n%s", raw)
	}
	got := LoadDotenv(path, nil)
	if got["SIGIL_AUTH_TOKEN"] != "token with spaces and # hash" {
		t.Errorf("round-trip failed: %q", got["SIGIL_AUTH_TOKEN"])
	}
}

func TestWriteDotenv_RejectsDisallowedKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.env")
	err := WriteDotenv(path, map[string]string{"PATH": "/tmp"}, nil)
	if err == nil {
		t.Fatal("expected error for disallowed key, got nil")
	}
	if !strings.Contains(err.Error(), "disallowed key") {
		t.Errorf("error = %v, want mention of disallowed key", err)
	}
}
