package dotenv

import (
	"bytes"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

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
	for _, badKey := range []string{"EMPTY", "", "no_equals_line", "PATH"} {
		if _, ok := got[badKey]; ok {
			t.Errorf("malformed key %q should not be loaded", badKey)
		}
	}
}

func TestLoadDotenvQuotedValueEdgeCases(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.env")
	body := `SIGIL_DOUBLE="my secret" # comment
SIGIL_SINGLE='other secret' # comment
SIGIL_HASH_INSIDE="value # not a comment"
SIGIL_PLAIN_COMMENT=plain # trailing
SIGIL_SPACES_INSIDE="  has spaces  "
SIGIL_UNTERMINATED="oops
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := LoadDotenv(path, log.New(&bytes.Buffer{}, "", 0))

	cases := []struct{ key, want string }{
		{"SIGIL_DOUBLE", "my secret"},
		{"SIGIL_SINGLE", "other secret"},
		{"SIGIL_HASH_INSIDE", "value # not a comment"},
		{"SIGIL_PLAIN_COMMENT", "plain"},
		{"SIGIL_SPACES_INSIDE", "  has spaces  "},
		{"SIGIL_UNTERMINATED", `"oops`},
	}
	for _, tc := range cases {
		if got[tc.key] != tc.want {
			t.Errorf("got[%q] = %q; want %q", tc.key, got[tc.key], tc.want)
		}
	}
}

func TestLoadDotenvMissingFileSilent(t *testing.T) {
	var buf bytes.Buffer
	got, err := ReadDotenv("/nonexistent/path/config.env", log.New(&buf, "", 0))
	if err != nil {
		t.Errorf("missing file should not error, got %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty map, got %v", got)
	}
	if buf.Len() != 0 {
		t.Errorf("missing file should not log; got %q", buf.String())
	}
}

// TestReadDotenvReportsReadFailures covers what LoadDotenv cannot express:
// callers that must fail closed need to tell "no file" apart from "the file is
// there but unreadable".
func TestReadDotenvReportsReadFailures(t *testing.T) {
	dir := t.TempDir()

	t.Run("open fails", func(t *testing.T) {
		path := filepath.Join(dir, "unreadable.env")
		if err := os.WriteFile(path, []byte("AGENTO11Y_LOCAL_FORWARD=false\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		makeUnreadable(t, path)

		var buf bytes.Buffer
		got, err := ReadDotenv(path, log.New(&buf, "", 0))
		if err == nil {
			t.Fatalf("expected an error, got map %v", got)
		}
		if buf.Len() == 0 {
			t.Error("an unreadable file should log")
		}
		if len(LoadDotenv(path, nil)) != 0 {
			t.Error("LoadDotenv should still swallow the error and return no pairs")
		}
	})

	t.Run("read fails", func(t *testing.T) {
		// A directory opens fine and fails on the first read.
		path := filepath.Join(dir, "config.env.d")
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadDotenv(path, nil); err == nil {
			t.Error("expected a scan error for a directory")
		}
	})
}

// TestParseDotenv holds the syntax cases; the file wrapper shares this code.
func TestParseDotenv(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want map[string]string
	}{
		{
			name: "export prefix, quotes, comments and blank lines",
			in: "# copied from Grafana\n" +
				"\n" +
				`export AGENTO11Y_ENDPOINT="https://example.invalid" # copied from Grafana` + "\n" +
				"AGENTO11Y_AUTH_TOKEN='glc_test'\n",
			want: map[string]string{
				"AGENTO11Y_ENDPOINT":   "https://example.invalid",
				"AGENTO11Y_AUTH_TOKEN": "glc_test",
			},
		},
		{
			name: "disallowed keys are filtered",
			in:   "AGENTO11Y_AUTH_TOKEN=glc_test\nUNRELATED_SECRET=do-not-load\n",
			want: map[string]string{"AGENTO11Y_AUTH_TOKEN": "glc_test"},
		},
		{
			// The launcher only speaks HTTP, so the protocol key is not allowed.
			name: "OTLP headers kept, OTLP protocol dropped",
			in: "OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf\n" +
				`OTEL_EXPORTER_OTLP_HEADERS="Authorization=Basic dGVzdA=="` + "\n",
			want: map[string]string{"OTEL_EXPORTER_OTLP_HEADERS": "Authorization=Basic dGVzdA=="},
		},
		{
			name: "empty input",
			in:   "",
			want: map[string]string{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseDotenv(strings.NewReader(c.in))
			if err != nil {
				t.Fatalf("ParseDotenv err = %v", err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("ParseDotenv() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestParseDotenvReaderFailure(t *testing.T) {
	r := io.MultiReader(
		strings.NewReader("AGENTO11Y_AUTH_TOKEN=glc_test\n"),
		&failingReader{err: io.ErrUnexpectedEOF},
	)
	got, err := ParseDotenv(r)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("ParseDotenv err = %v, want %v", err, io.ErrUnexpectedEOF)
	}
	if got["AGENTO11Y_AUTH_TOKEN"] != "glc_test" {
		t.Errorf("pairs parsed before the failure should be returned, got %v", got)
	}
}

type failingReader struct{ err error }

func (f *failingReader) Read([]byte) (int, error) { return 0, f.err }

func TestApplyEnv(t *testing.T) {
	cases := []struct {
		name      string
		osValue   string
		osUnset   bool
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
			// ApplyEnv materializes winners under both spellings via
			// os.Setenv, so pin the preferred name blank per subtest to keep
			// cases hermetic.
			t.Setenv("AGENTO11Y_ENDPOINT", "")
			const key = "SIGIL_ENDPOINT"
			if tc.osUnset {
				_ = os.Unsetenv(key)
				t.Cleanup(func() { _ = os.Unsetenv(key) })
			} else {
				t.Setenv(key, tc.osValue)
			}
			cfgDir := filepath.Join(dir, "sigil")
			if err := os.MkdirAll(cfgDir, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if tc.fileValue != "" {
				if err := os.WriteFile(filepath.Join(cfgDir, "config.env"), []byte(key+"="+tc.fileValue+"\n"), 0o600); err != nil {
					t.Fatalf("write: %v", err)
				}
			}
			ApplyEnv(log.New(&bytes.Buffer{}, "", 0))
			if got := os.Getenv(key); got != tc.want {
				t.Fatalf("%s = %q, want %q", key, got, tc.want)
			}
			if tc.want != "" {
				if got := os.Getenv("AGENTO11Y_ENDPOINT"); got != tc.want {
					t.Fatalf("AGENTO11Y_ENDPOINT = %q, want %q (materialized under both names)", got, tc.want)
				}
			}
		})
	}
}

func TestApplyEnvAliasFamilies(t *testing.T) {
	cases := []struct {
		name   string
		suffix string
		os     map[string]string
		file   string
		want   string
	}{
		{
			name: "shell preferred beats shell legacy",
			os:   map[string]string{"AGENTO11Y_ENDPOINT": "os-preferred", "SIGIL_ENDPOINT": "os-legacy"},
			want: "os-preferred",
		},
		{
			name: "shell legacy beats file preferred",
			os:   map[string]string{"SIGIL_ENDPOINT": "os-legacy"},
			file: "AGENTO11Y_ENDPOINT=file-preferred\n",
			want: "os-legacy",
		},
		{
			name: "file preferred beats file legacy",
			file: "AGENTO11Y_ENDPOINT=file-preferred\nSIGIL_ENDPOINT=file-legacy\n",
			want: "file-preferred",
		},
		{
			name: "blank shell preferred falls through to shell legacy",
			os:   map[string]string{"AGENTO11Y_ENDPOINT": "   ", "SIGIL_ENDPOINT": "os-legacy"},
			want: "os-legacy",
		},
		{
			name: "file legacy applies when nothing else set",
			file: "SIGIL_ENDPOINT=file-legacy\n",
			want: "file-legacy",
		},
		{
			name:   "local allowed hosts shell legacy beats file preferred",
			suffix: "LOCAL_ALLOWED_HOSTS",
			os:     map[string]string{"SIGIL_LOCAL_ALLOWED_HOSTS": "shell.example"},
			file:   "AGENTO11Y_LOCAL_ALLOWED_HOSTS=file.example\n",
			want:   "shell.example",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", dir)
			suffix := tc.suffix
			if suffix == "" {
				suffix = "ENDPOINT"
			}
			preferred := "AGENTO11Y_" + suffix
			legacy := "SIGIL_" + suffix
			t.Setenv(preferred, "")
			t.Setenv(legacy, "")
			for k, v := range tc.os {
				t.Setenv(k, v)
			}
			cfgDir := filepath.Join(dir, "sigil")
			if err := os.MkdirAll(cfgDir, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if tc.file != "" {
				if err := os.WriteFile(filepath.Join(cfgDir, "config.env"), []byte(tc.file), 0o600); err != nil {
					t.Fatalf("write: %v", err)
				}
			}
			ApplyEnv(log.New(&bytes.Buffer{}, "", 0))
			if got := os.Getenv(preferred); got != tc.want {
				t.Fatalf("%s = %q, want %q", preferred, got, tc.want)
			}
			if got := os.Getenv(legacy); got != tc.want {
				t.Fatalf("%s = %q, want %q", legacy, got, tc.want)
			}
		})
	}
}

func TestFilePath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	got := FilePath()
	wantSuffix := filepath.Join("agento11y", "config.env")
	if !strings.HasPrefix(got, dir) || !strings.HasSuffix(got, wantSuffix) {
		t.Fatalf("FilePath() = %q, want inside %q ending %q", got, dir, wantSuffix)
	}
}

func TestFilePathDefaultsToHomeDotConfigWhenXDGUnset(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	got := FilePath()
	want := filepath.Join(home, ".config", "agento11y", "config.env")
	if got != want {
		t.Fatalf("FilePath() = %q, want %q", got, want)
	}
}

func TestFilePathResolution(t *testing.T) {
	cases := []struct {
		name       string
		makeNew    bool
		makeLegacy bool
		wantApp    string
	}{
		{name: "neither exists defaults to new", wantApp: "agento11y"},
		{name: "only new exists", makeNew: true, wantApp: "agento11y"},
		{name: "only legacy exists", makeLegacy: true, wantApp: "sigil"},
		{name: "both exist prefers new", makeNew: true, makeLegacy: true, wantApp: "agento11y"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", dir)
			for app, make := range map[string]bool{"agento11y": tc.makeNew, "sigil": tc.makeLegacy} {
				if !make {
					continue
				}
				if err := os.MkdirAll(filepath.Join(dir, app), 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				if err := os.WriteFile(filepath.Join(dir, app, "config.env"), []byte("SIGIL_ENDPOINT=x\n"), 0o600); err != nil {
					t.Fatalf("write: %v", err)
				}
			}
			want := filepath.Join(dir, tc.wantApp, "config.env")
			if got := FilePath(); got != want {
				t.Fatalf("FilePath() = %q, want %q", got, want)
			}
		})
	}
}
