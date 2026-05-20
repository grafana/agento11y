package login

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// nonTTYStdin returns a file that is guaranteed not to be a terminal.
// We use the read end of a pipe so .Fd() is valid but term.IsTerminal
// returns false.
func nonTTYStdin(t *testing.T) *os.File {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	t.Cleanup(func() {
		_ = r.Close()
		_ = w.Close()
	})
	return r
}

// writeDotenv creates a config.env file in a fresh temp dir and returns
// its path. An empty contents string skips file creation so callers can
// exercise the missing-file branch.
func writeDotenv(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.env")
	if contents == "" {
		return path
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write dotenv: %v", err)
	}
	return path
}

// clearSeededEnv wipes every SIGIL_* key loadSeeds reads from the
// process env. Tests need this because the host shell may have some of
// them exported (the developer running `go test` is the same user who
// uses sigil), which would otherwise leak into table cases that intend
// to exercise the "env unset" path.
func clearSeededEnv(t *testing.T) {
	t.Helper()
	for _, k := range seededKeys {
		t.Setenv(k, "")
	}
}

// TestRun_NoTTYReturnsErrNotInteractive covers the only branch of Run that
// is reachable without driving huh's TUI: when stdin is not a terminal we
// must bail with ErrNotInteractive and leave the dotenv file untouched.
// The interactive form itself is exercised by the cmd/sigil end-to-end
// tests that stub loginRun, not here.
func TestRun_NoTTYReturnsErrNotInteractive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.env")

	err := Run(context.Background(), RunOpts{
		ConfigPath: path,
		Stdin:      nonTTYStdin(t),
	})
	if !errors.Is(err, ErrNotInteractive) {
		t.Fatalf("Run err = %v, want %v", err, ErrNotInteractive)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("dotenv was written despite ErrNotInteractive: %v", statErr)
	}
}

// TestLoadSeeds covers the precedence rules loadSeeds enforces:
// process env wins over the dotenv file (the bug fix — launcher
// auto-prompts must pre-fill from SIGIL_* vars already in the user's
// shell instead of showing empty fields), the file is the fallback,
// and whitespace-only env values mirror dotenv.ApplyEnv by being
// treated as unset.
func TestLoadSeeds(t *testing.T) {
	cases := []struct {
		name string
		file string            // dotenv contents; "" means no file on disk
		env  map[string]string // process env; every key from seededKeys is asserted
		want map[string]string // "" means key must be absent/empty from seeds
	}{
		{
			name: "process env overlays dotenv file",
			file: "SIGIL_ENDPOINT=https://stale.example.com\n" +
				"SIGIL_AUTH_TENANT_ID=stale-tenant\n",
			env: map[string]string{
				"SIGIL_ENDPOINT":       "https://fresh.example.com",
				"SIGIL_AUTH_TENANT_ID": "fresh-tenant",
			},
			want: map[string]string{
				"SIGIL_ENDPOINT":                    "https://fresh.example.com",
				"SIGIL_AUTH_TENANT_ID":              "fresh-tenant",
				"SIGIL_AUTH_TOKEN":                  "",
				"SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT": "",
			},
		},
		{
			name: "dotenv file used when env unset",
			file: "SIGIL_ENDPOINT=https://file.example.com\n" +
				"SIGIL_AUTH_TENANT_ID=file-tenant\n" +
				"SIGIL_AUTH_TOKEN=file-token\n",
			want: map[string]string{
				"SIGIL_ENDPOINT":                    "https://file.example.com",
				"SIGIL_AUTH_TENANT_ID":              "file-tenant",
				"SIGIL_AUTH_TOKEN":                  "file-token",
				"SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT": "",
			},
		},
		{
			name: "whitespace env does not overlay dotenv",
			file: "SIGIL_ENDPOINT=https://file.example.com\n",
			env:  map[string]string{"SIGIL_ENDPOINT": "   "},
			want: map[string]string{
				"SIGIL_ENDPOINT":                    "https://file.example.com",
				"SIGIL_AUTH_TENANT_ID":              "",
				"SIGIL_AUTH_TOKEN":                  "",
				"SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT": "",
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clearSeededEnv(t)
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			seeds := loadSeeds(writeDotenv(t, c.file), nil)
			for k, want := range c.want {
				if got := seeds[k]; got != want {
					t.Errorf("seeds[%q] = %q, want %q", k, got, want)
				}
			}
		})
	}
}

func TestRequireURL(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"empty", "", true},
		{"missing scheme", "sigil.example.com", true},
		{"unsupported scheme", "ftp://sigil.example.com", true},
		{"missing host", "https://", true},
		{"valid http", "http://localhost:8080", false},
		{"valid https", "https://sigil.example.com/path", false},
		{"trims whitespace", "  https://sigil.example.com  ", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := requireURL(c.in)
			if (err != nil) != c.wantErr {
				t.Errorf("requireURL(%q) err = %v, wantErr = %v", c.in, err, c.wantErr)
			}
		})
	}
}

func TestAllowEmptyURL(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"empty", "", false},
		{"whitespace only", "   ", false},
		{"valid url", "https://otlp.example", false},
		{"non-empty bad url", "not a url", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := allowEmptyURL(c.in)
			if (err != nil) != c.wantErr {
				t.Errorf("allowEmptyURL(%q) err = %v, wantErr = %v", c.in, err, c.wantErr)
			}
		})
	}
}
