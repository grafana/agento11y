package pi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/grafana/sigil-sdk/plugins/sigil/internal/local"
)

func TestLaunch_MissingPiBinary(t *testing.T) {
	withLookPath(t, func(string) (string, error) { return "", exec.ErrNotFound })

	err := Launch(context.Background(), nil, nil, strings.NewReader(""), io.Discard, io.Discard, nopLogger(), "dev")
	if err == nil || !strings.Contains(err.Error(), "pi CLI not found") {
		t.Fatalf("err = %v, want contains \"pi CLI not found\"", err)
	}
}

func TestLaunch_SkipsInstallWhenPackagePresent(t *testing.T) {
	dir := t.TempDir()
	writeSettings(t, dir, `{"packages":["npm:@grafana/sigil-pi"]}`)
	t.Setenv("PI_CODING_AGENT_DIR", dir)

	withLookPath(t, func(string) (string, error) { return "/usr/local/bin/pi", nil })
	withRunInstall(t, func(context.Context, string, io.Writer) error {
		t.Fatal("runInstall must not be called when package is already present")
		return nil
	})

	var execArgv []string
	withExecFn(t, func(_ string, argv []string, _ []string) error {
		execArgv = append([]string{}, argv...)
		return nil
	})

	var stderr bytes.Buffer
	if err := Launch(context.Background(), []string{"--print", "hi"}, nil, strings.NewReader(""), io.Discard, &stderr, nopLogger(), "dev"); err != nil {
		t.Fatalf("Launch returned err: %v", err)
	}
	if !reflect.DeepEqual(execArgv, []string{"/usr/local/bin/pi", "--print", "hi"}) {
		t.Fatalf("exec argv = %v", execArgv)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr non-empty: %q", stderr.String())
	}
}

func TestLaunch_RunsInstallWhenPackageMissing(t *testing.T) {
	dir := t.TempDir()
	writeSettings(t, dir, `{"packages":[]}`)
	t.Setenv("PI_CODING_AGENT_DIR", dir)

	withLookPath(t, func(string) (string, error) { return "/usr/local/bin/pi", nil })

	installCalls := 0
	withRunInstall(t, func(_ context.Context, bin string, _ io.Writer) error {
		installCalls++
		if bin != "/usr/local/bin/pi" {
			t.Errorf("install bin = %q, want /usr/local/bin/pi", bin)
		}
		return nil
	})

	execCalled := false
	withExecFn(t, func(_ string, _ []string, _ []string) error {
		execCalled = true
		return nil
	})

	var stderr bytes.Buffer
	if err := Launch(context.Background(), nil, nil, strings.NewReader(""), io.Discard, &stderr, nopLogger(), "dev"); err != nil {
		t.Fatalf("Launch returned err: %v", err)
	}
	if installCalls != 1 {
		t.Fatalf("runInstall calls = %d, want 1", installCalls)
	}
	if !execCalled {
		t.Fatal("execFn was not called after install")
	}
	if !strings.Contains(stderr.String(), "installing "+PluginSource) {
		t.Fatalf("stderr missing install message: %q", stderr.String())
	}
}

// A failed `pi install` should not block the user's session. The launcher
// must print the failure and a manual-retry hint on stderr, then still exec
// pi so the workflow keeps moving (just without Sigil capture).
func TestLaunch_InstallFailureContinuesToExec(t *testing.T) {
	dir := t.TempDir()
	writeSettings(t, dir, `{"packages":[]}`)
	t.Setenv("PI_CODING_AGENT_DIR", dir)

	withLookPath(t, func(string) (string, error) { return "/usr/local/bin/pi", nil })
	withRunInstall(t, func(context.Context, string, io.Writer) error {
		return errors.New("network down")
	})

	execCalled := false
	withExecFn(t, func(_ string, _ []string, _ []string) error {
		execCalled = true
		return nil
	})

	var stderr bytes.Buffer
	if err := Launch(context.Background(), nil, nil, strings.NewReader(""), io.Discard, &stderr, nopLogger(), "dev"); err != nil {
		t.Fatalf("Launch returned err: %v", err)
	}
	if !execCalled {
		t.Fatal("execFn was not called after install failure")
	}
	got := stderr.String()
	if !strings.Contains(got, "install of "+PluginSource+" failed") {
		t.Fatalf("stderr missing failure message: %q", got)
	}
	if !strings.Contains(got, "network down") {
		t.Fatalf("stderr missing underlying error: %q", got)
	}
	if !strings.Contains(got, "pi install "+PluginSource) {
		t.Fatalf("stderr missing manual install hint: %q", got)
	}
}

func TestLaunch_LocalInjectsEnvAndForwardsArgs(t *testing.T) {
	dir := t.TempDir()
	writeSettings(t, dir, `{"packages":["npm:@grafana/sigil-pi"]}`)
	t.Setenv("PI_CODING_AGENT_DIR", dir)

	withLookPath(t, func(string) (string, error) { return "/usr/local/bin/pi", nil })
	withRunInstall(t, func(context.Context, string, io.Writer) error {
		t.Fatal("runInstall must not be called when plugin is already present")
		return nil
	})
	// Clear cloud creds inherited from the host shell so the placeholder
	// branch fires. User-set values are preserved by design — that's
	// covered in TestLaunch_NormalModeDoesNotInjectLocalEnv.
	t.Setenv("SIGIL_AUTH_TENANT_ID", "")
	t.Setenv("SIGIL_AUTH_TOKEN", "")
	// Local forces full content on this machine regardless of the configured
	// Cloud capture mode, so a preset value here must be overridden to full.
	t.Setenv("SIGIL_CONTENT_CAPTURE_MODE", "no_tool_content")

	var execEnv []string
	var execArgv []string
	withExecFn(t, func(_ string, argv []string, env []string) error {
		execArgv = append([]string{}, argv...)
		execEnv = append([]string{}, env...)
		return nil
	})

	localEnv := &local.LaunchEnv{
		Endpoint:     "http://127.0.0.1:9000",
		OTLPEndpoint: "http://127.0.0.1:9000/otlp",
	}
	if err := Launch(context.Background(), []string{"--print", "hi"}, localEnv, strings.NewReader(""), io.Discard, io.Discard, nopLogger(), "dev"); err != nil {
		t.Fatalf("Launch returned err: %v", err)
	}
	if !reflect.DeepEqual(execArgv, []string{"/usr/local/bin/pi", "--print", "hi"}) {
		t.Fatalf("exec argv = %v", execArgv)
	}
	got := envToMap(execEnv)
	if got["SIGIL_ENDPOINT"] != "http://127.0.0.1:9000" {
		t.Errorf("SIGIL_ENDPOINT = %q", got["SIGIL_ENDPOINT"])
	}
	if got["SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT"] != "http://127.0.0.1:9000/otlp" {
		t.Errorf("SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT = %q", got["SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT"])
	}
	if got["SIGIL_AUTH_TENANT_ID"] != "local" || got["SIGIL_AUTH_TOKEN"] != "local" {
		t.Errorf("placeholder auth missing: tenant=%q token=%q", got["SIGIL_AUTH_TENANT_ID"], got["SIGIL_AUTH_TOKEN"])
	}
	// Local always captures full content; the configured Cloud capture mode is
	// overridden for the local session.
	if got["SIGIL_CONTENT_CAPTURE_MODE"] != "full" {
		t.Errorf("SIGIL_CONTENT_CAPTURE_MODE = %q, want full (local forces full content)", got["SIGIL_CONTENT_CAPTURE_MODE"])
	}
}

func TestLaunch_LocalDefaultsFullContentWhenUnset(t *testing.T) {
	dir := t.TempDir()
	writeSettings(t, dir, `{"packages":["npm:@grafana/sigil-pi"]}`)
	t.Setenv("PI_CODING_AGENT_DIR", dir)
	t.Setenv("SIGIL_CONTENT_CAPTURE_MODE", "")

	withLookPath(t, func(string) (string, error) { return "/usr/local/bin/pi", nil })

	var execEnv []string
	withExecFn(t, func(_ string, _ []string, env []string) error {
		execEnv = append([]string{}, env...)
		return nil
	})

	localEnv := &local.LaunchEnv{
		Endpoint:     "http://127.0.0.1:9000",
		OTLPEndpoint: "http://127.0.0.1:9000/otlp",
	}
	if err := Launch(context.Background(), nil, localEnv, strings.NewReader(""), io.Discard, io.Discard, nopLogger(), "dev"); err != nil {
		t.Fatalf("Launch returned err: %v", err)
	}
	got := envToMap(execEnv)
	if got["SIGIL_CONTENT_CAPTURE_MODE"] != "full" {
		t.Errorf("SIGIL_CONTENT_CAPTURE_MODE = %q, want full (default in local mode)", got["SIGIL_CONTENT_CAPTURE_MODE"])
	}
}

func TestLaunch_NormalModeDoesNotInjectLocalEnv(t *testing.T) {
	dir := t.TempDir()
	writeSettings(t, dir, `{"packages":["npm:@grafana/sigil-pi"]}`)
	t.Setenv("PI_CODING_AGENT_DIR", dir)
	t.Setenv("SIGIL_ENDPOINT", "https://cloud.example.com")
	t.Setenv("SIGIL_AUTH_TENANT_ID", "real-tenant")
	t.Setenv("SIGIL_AUTH_TOKEN", "real-token")

	withLookPath(t, func(string) (string, error) { return "/usr/local/bin/pi", nil })

	var execEnv []string
	withExecFn(t, func(_ string, _ []string, env []string) error {
		execEnv = append([]string{}, env...)
		return nil
	})

	if err := Launch(context.Background(), nil, nil, strings.NewReader(""), io.Discard, io.Discard, nopLogger(), "dev"); err != nil {
		t.Fatalf("Launch returned err: %v", err)
	}
	got := envToMap(execEnv)
	if got["SIGIL_ENDPOINT"] != "https://cloud.example.com" {
		t.Errorf("SIGIL_ENDPOINT changed in normal mode: %q", got["SIGIL_ENDPOINT"])
	}
	if got["SIGIL_AUTH_TENANT_ID"] != "real-tenant" {
		t.Errorf("auth changed in normal mode: %q", got["SIGIL_AUTH_TENANT_ID"])
	}
}

func envToMap(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		key, value, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		m[key] = value
	}
	return m
}

func TestPluginInstalled_RecognisesBothShapes(t *testing.T) {
	// localPluginDir is a sibling directory referenced by relative-path test
	// cases. It is materialised below before each subtest runs.
	const localPluginDir = "local-plugin"

	cases := []struct {
		name     string
		write    bool
		contents string
		// localPkg, when non-empty, is written to <tmpdir>/local-plugin/package.json
		// so relative/absolute path entries in `contents` resolve to a real package.
		localPkg string
		want     bool
		wantErr  bool
	}{
		{
			name:     "string entry matches",
			write:    true,
			contents: `{"packages":["npm:@grafana/sigil-pi"]}`,
			want:     true,
		},
		{
			name:     "object entry matches",
			write:    true,
			contents: `{"packages":[{"source":"npm:@grafana/sigil-pi","alias":"sigil"}]}`,
			want:     true,
		},
		{
			name:     "versioned npm string matches",
			write:    true,
			contents: `{"packages":["npm:@grafana/sigil-pi@0.1.1"]}`,
			want:     true,
		},
		{
			name:     "versioned npm object matches",
			write:    true,
			contents: `{"packages":[{"source":"npm:@grafana/sigil-pi@1.0.0-rc.3"}]}`,
			want:     true,
		},
		{
			name:     "npm dist-tag matches",
			write:    true,
			contents: `{"packages":["npm:@grafana/sigil-pi@next"]}`,
			want:     true,
		},
		{
			name:     "similar npm name does not match",
			write:    true,
			contents: `{"packages":["npm:@grafana/sigil-pi-extra","npm:@grafana/sigil-pi-extra@1.0.0"]}`,
			want:     false,
		},
		{
			name:     "relative local path matches",
			write:    true,
			contents: `{"packages":["./local-plugin"]}`,
			localPkg: `{"name":"@grafana/sigil-pi"}`,
			want:     true,
		},
		{
			name:     "relative local path with wrong name does not match",
			write:    true,
			contents: `{"packages":["./local-plugin"]}`,
			localPkg: `{"name":"@grafana/other"}`,
			want:     false,
		},
		{
			name:     "missing local path does not match and does not error",
			write:    true,
			contents: `{"packages":["/does/not/exist","./also-missing"]}`,
			want:     false,
		},
		{
			name:     "git source does not match",
			write:    true,
			contents: `{"packages":["git:github.com/grafana/sigil-sdk"]}`,
			want:     false,
		},
		{
			name:     "unrelated entries do not match",
			write:    true,
			contents: `{"packages":["npm:other-pkg",{"source":"npm:another"}]}`,
			want:     false,
		},
		{
			name:     "empty packages list",
			write:    true,
			contents: `{"packages":[]}`,
			want:     false,
		},
		{
			name:  "missing file treated as not installed",
			write: false,
			want:  false,
		},
		{
			name:     "malformed json surfaces error",
			write:    true,
			contents: `{"packages":[`,
			wantErr:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.write {
				writeSettings(t, dir, tc.contents)
			}
			if tc.localPkg != "" {
				pkgDir := filepath.Join(dir, localPluginDir)
				if err := os.MkdirAll(pkgDir, 0o755); err != nil {
					t.Fatalf("mkdir local plugin: %v", err)
				}
				if err := os.WriteFile(filepath.Join(pkgDir, "package.json"), []byte(tc.localPkg), 0o600); err != nil {
					t.Fatalf("write local package.json: %v", err)
				}
			}
			t.Setenv("PI_CODING_AGENT_DIR", dir)

			got, err := pluginInstalled()
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("got = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPluginInstalled_ProjectScope(t *testing.T) {
	// Project-scoped installs (`pi install -l`) land in <cwd>/.pi/settings.json
	// rather than the global file. The launcher must consider those installed
	// too — otherwise it re-runs `pi install` on every invocation, writing
	// to the global file the user explicitly chose to avoid.
	cases := []struct {
		name    string
		global  string // contents of global settings.json ("" = don't create)
		project string // contents of <cwd>/.pi/settings.json ("" = don't create)
		want    bool
		wantErr bool
	}{
		{
			name:    "project-only install matches",
			global:  `{"packages":[]}`,
			project: `{"packages":["npm:@grafana/sigil-pi"]}`,
			want:    true,
		},
		{
			name:    "project-only versioned install matches",
			global:  `{"packages":[]}`,
			project: `{"packages":["npm:@grafana/sigil-pi@0.1.1"]}`,
			want:    true,
		},
		{
			name:    "global wins without consulting project",
			global:  `{"packages":["npm:@grafana/sigil-pi"]}`,
			project: `{"packages":[]}`,
			want:    true,
		},
		{
			name:    "both scopes have it",
			global:  `{"packages":["npm:@grafana/sigil-pi"]}`,
			project: `{"packages":["npm:@grafana/sigil-pi@0.2.0"]}`,
			want:    true,
		},
		{
			name:    "neither scope has it",
			global:  `{"packages":["npm:other"]}`,
			project: `{"packages":["npm:another"]}`,
			want:    false,
		},
		{
			name:    "project file absent falls back to global only",
			global:  `{"packages":["npm:@grafana/sigil-pi"]}`,
			project: "",
			want:    true,
		},
		{
			name:    "project file absent and global lacks plugin",
			global:  `{"packages":[]}`,
			project: "",
			want:    false,
		},
		{
			name:    "malformed project settings surfaces error",
			global:  `{"packages":[]}`,
			project: `{"packages":[`,
			wantErr: true,
		},
		{
			name:    "global file absent, project has plugin",
			global:  "",
			project: `{"packages":["npm:@grafana/sigil-pi"]}`,
			want:    true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			globalDir := t.TempDir()
			projectRoot := t.TempDir()
			if tc.global != "" {
				writeSettings(t, globalDir, tc.global)
			}
			if tc.project != "" {
				projectPiDir := filepath.Join(projectRoot, ".pi")
				if err := os.MkdirAll(projectPiDir, 0o755); err != nil {
					t.Fatalf("mkdir project .pi: %v", err)
				}
				writeSettings(t, projectPiDir, tc.project)
			}
			t.Setenv("PI_CODING_AGENT_DIR", globalDir)
			t.Chdir(projectRoot)

			got, err := pluginInstalled()
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("got = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPluginInstalled_ProjectScopeLocalPath(t *testing.T) {
	// Relative paths in project settings resolve against the project .pi dir,
	// not the global one. Make sure that resolution works end to end.
	globalDir := t.TempDir()
	projectRoot := t.TempDir()

	projectPiDir := filepath.Join(projectRoot, ".pi")
	if err := os.MkdirAll(projectPiDir, 0o755); err != nil {
		t.Fatalf("mkdir project .pi: %v", err)
	}
	// Local plugin sits at <projectRoot>/.pi/local-plugin/, referenced as
	// "./local-plugin" from <projectRoot>/.pi/settings.json.
	pkgDir := filepath.Join(projectPiDir, "local-plugin")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatalf("mkdir local plugin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "package.json"), []byte(`{"name":"@grafana/sigil-pi"}`), 0o600); err != nil {
		t.Fatalf("write package.json: %v", err)
	}
	writeSettings(t, projectPiDir, `{"packages":["./local-plugin"]}`)

	t.Setenv("PI_CODING_AGENT_DIR", globalDir)
	t.Chdir(projectRoot)

	got, err := pluginInstalled()
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !got {
		t.Fatal("relative local path in project settings should match")
	}
}

func TestPluginInstalled_AbsoluteLocalPath(t *testing.T) {
	// Absolute paths are resolved as-is, independent of settingsDir.
	dir := t.TempDir()
	pkgDir := filepath.Join(t.TempDir(), "sigil-pi-checkout")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "package.json"), []byte(`{"name":"@grafana/sigil-pi"}`), 0o600); err != nil {
		t.Fatalf("write package.json: %v", err)
	}
	contents := `{"packages":["` + pkgDir + `"]}`
	writeSettings(t, dir, contents)
	t.Setenv("PI_CODING_AGENT_DIR", dir)

	got, err := pluginInstalled()
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !got {
		t.Fatal("absolute local path should match")
	}
}

func TestStripNpmVersion(t *testing.T) {
	cases := map[string]string{
		"@grafana/sigil-pi":       "@grafana/sigil-pi",
		"@grafana/sigil-pi@0.1.1": "@grafana/sigil-pi",
		"@grafana/sigil-pi@next":  "@grafana/sigil-pi",
		"pkg":                     "pkg",
		"pkg@1.0.0":               "pkg",
		"@grafana/sigil-pi-extra": "@grafana/sigil-pi-extra",
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			if got := stripNpmVersion(in); got != want {
				t.Fatalf("stripNpmVersion(%q) = %q, want %q", in, got, want)
			}
		})
	}
}

func writeSettings(t *testing.T, dir, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(contents), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
}

func withLookPath(t *testing.T, fn func(string) (string, error)) {
	t.Helper()
	prev := lookPath
	t.Cleanup(func() { lookPath = prev })
	lookPath = fn
}

func withRunInstall(t *testing.T, fn func(context.Context, string, io.Writer) error) {
	t.Helper()
	prev := runInstall
	t.Cleanup(func() { runInstall = prev })
	runInstall = fn
}

func withExecFn(t *testing.T, fn func(string, []string, []string) error) {
	t.Helper()
	prev := execFn
	t.Cleanup(func() { execFn = prev })
	execFn = fn
}

func nopLogger() *log.Logger {
	return log.New(io.Discard, "", 0)
}

func TestVersionFromPiSource(t *testing.T) {
	cases := []struct {
		source string
		want   string
	}{
		{source: "npm:@grafana/sigil-pi", want: ""},
		{source: "npm:@grafana/sigil-pi@0.1.1", want: "0.1.1"},
		{source: "npm:@grafana/sigil-pi@1.0.0-rc.3", want: "1.0.0-rc.3"},
		{source: "npm:@grafana/sigil-pi@next", want: "next"},
		{source: "./local-plugin", want: ""},
		{source: "/abs/path", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.source, func(t *testing.T) {
			if got := versionFromPiSource(tc.source); got != tc.want {
				t.Fatalf("versionFromPiSource(%q) = %q, want %q", tc.source, got, tc.want)
			}
		})
	}
}

func TestStatus(t *testing.T) {
	tests := []struct {
		name          string
		settings      string
		wantInstalled bool
		wantVersion   string
	}{
		{name: "installed reports version", settings: `{"packages":["npm:@grafana/sigil-pi@0.1.1"]}`, wantInstalled: true, wantVersion: "0.1.1"},
		{name: "not installed", settings: `{"packages":[]}`, wantInstalled: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("PI_CODING_AGENT_DIR", dir)
			writeSettings(t, dir, tc.settings)

			installed, version, err := Status(context.Background())
			if err != nil {
				t.Fatalf("Status err: %v", err)
			}
			if installed != tc.wantInstalled {
				t.Fatalf("installed = %v, want %v", installed, tc.wantInstalled)
			}
			if version != tc.wantVersion {
				t.Fatalf("version = %q, want %q", version, tc.wantVersion)
			}
		})
	}
}
