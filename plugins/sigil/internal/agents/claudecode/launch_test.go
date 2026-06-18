package claudecode

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
	"github.com/stretchr/testify/require"
)

func TestLaunch_MissingClaudeBinary(t *testing.T) {
	withLookPath(t, func(string) (string, error) { return "", exec.ErrNotFound })

	err := Launch(context.Background(), nil, nil, strings.NewReader(""), io.Discard, io.Discard, nopLogger(), "dev")
	if err == nil || !strings.Contains(err.Error(), "claude CLI not found") {
		t.Fatalf("err = %v, want contains \"claude CLI not found\"", err)
	}
}

func TestLaunch_SkipsInstallWhenPluginPresent(t *testing.T) {
	t.Setenv("SIGIL_AUTO_UPDATE", "false")

	dir := t.TempDir()
	writeInstalled(t, dir, `{"version":2,"plugins":{"sigil-cc@grafana-sigil":[{"scope":"user"}]}}`)
	t.Setenv("CLAUDE_CONFIG_DIR", dir)

	withLookPath(t, func(string) (string, error) { return "/usr/local/bin/claude", nil })
	withRunInstall(t, func(context.Context, string, io.Writer) error {
		t.Fatal("runInstall must not be called when plugin is already present")
		return nil
	})

	var execArgv []string
	withExecFn(t, func(_ string, argv []string, _ []string) error {
		execArgv = append([]string{}, argv...)
		return nil
	})

	var stderr bytes.Buffer
	if err := Launch(context.Background(), []string{"--resume", "abc"}, nil, strings.NewReader(""), io.Discard, &stderr, nopLogger(), "dev"); err != nil {
		t.Fatalf("Launch returned err: %v", err)
	}
	if !reflect.DeepEqual(execArgv, []string{"/usr/local/bin/claude", "--resume", "abc"}) {
		t.Fatalf("exec argv = %v", execArgv)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr non-empty: %q", stderr.String())
	}
}

// Regression: a per-directory install for another project must not
// suppress bootstrap in the current directory. Claude Code's plugin
// store keeps the entry under `sigil-cc@*`, but `scope: project` with
// a non-matching `projectPath` means the plugin is NOT active here.
func TestLaunch_RunsInstallWhenOnlyForeignProjectScopeEntry(t *testing.T) {
	dir := t.TempDir()
	writeInstalled(t, dir, `{"version":2,"plugins":{"sigil-cc@grafana-sigil":[{"scope":"project","projectPath":"/work/some-other-project"}]}}`)
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	withGetwd(t, func() (string, error) { return "/work/current-project", nil })

	withLookPath(t, func(string) (string, error) { return "/usr/local/bin/claude", nil })

	installCalls := 0
	withRunInstall(t, func(context.Context, string, io.Writer) error {
		installCalls++
		return nil
	})
	withExecFn(t, func(string, []string, []string) error { return nil })

	if err := Launch(context.Background(), nil, nil, strings.NewReader(""), io.Discard, io.Discard, nopLogger(), "dev"); err != nil {
		t.Fatalf("Launch returned err: %v", err)
	}
	if installCalls != 1 {
		t.Fatalf("runInstall calls = %d, want 1 (foreign project-scope entry must not suppress bootstrap)", installCalls)
	}
}

func TestLaunch_RunsInstallWhenMissing(t *testing.T) {
	dir := t.TempDir()
	writeInstalled(t, dir, `{"version":2,"plugins":{"some-other@marketplace":[{"scope":"user"}]}}`)
	t.Setenv("CLAUDE_CONFIG_DIR", dir)

	withLookPath(t, func(string) (string, error) { return "/usr/local/bin/claude", nil })

	installCalls := 0
	withRunInstall(t, func(_ context.Context, bin string, _ io.Writer) error {
		installCalls++
		if bin != "/usr/local/bin/claude" {
			t.Errorf("install bin = %q, want /usr/local/bin/claude", bin)
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
	if !strings.Contains(stderr.String(), "registering "+PluginName+" with claude") {
		t.Fatalf("stderr missing register message: %q", stderr.String())
	}
}

func TestLaunch_RunsInstallWhenFileAbsent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)

	withLookPath(t, func(string) (string, error) { return "/usr/local/bin/claude", nil })

	installCalls := 0
	withRunInstall(t, func(context.Context, string, io.Writer) error {
		installCalls++
		return nil
	})
	withExecFn(t, func(string, []string, []string) error { return nil })

	if err := Launch(context.Background(), nil, nil, strings.NewReader(""), io.Discard, io.Discard, nopLogger(), "dev"); err != nil {
		t.Fatalf("Launch returned err: %v", err)
	}
	if installCalls != 1 {
		t.Fatalf("runInstall calls = %d, want 1", installCalls)
	}
}

func TestLaunch_RunsInstallWhenJSONMalformed(t *testing.T) {
	dir := t.TempDir()
	writeInstalled(t, dir, `{"plugins":{`)
	t.Setenv("CLAUDE_CONFIG_DIR", dir)

	withLookPath(t, func(string) (string, error) { return "/usr/local/bin/claude", nil })

	installCalls := 0
	withRunInstall(t, func(context.Context, string, io.Writer) error {
		installCalls++
		return nil
	})
	withExecFn(t, func(string, []string, []string) error { return nil })

	if err := Launch(context.Background(), nil, nil, strings.NewReader(""), io.Discard, io.Discard, nopLogger(), "dev"); err != nil {
		t.Fatalf("Launch returned err: %v", err)
	}
	if installCalls != 1 {
		t.Fatalf("runInstall calls = %d, want 1 (malformed JSON should fall through to install)", installCalls)
	}
}

// A failed `claude plugin install` must not block the user's claude session.
// The launcher should print the failure plus a manual-retry hint on stderr,
// then still exec claude so the workflow keeps moving (just without the
// sigil-cc plugin installed).
func TestLaunch_InstallFailureContinuesToExec(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)

	withLookPath(t, func(string) (string, error) { return "/usr/local/bin/claude", nil })
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
	if !strings.Contains(got, "install of "+PluginName+" failed") {
		t.Fatalf("stderr missing failure message: %q", got)
	}
	if !strings.Contains(got, "network down") {
		t.Fatalf("stderr missing underlying error: %q", got)
	}
	if !strings.Contains(got, "claude plugin install "+PluginName+"@") {
		t.Fatalf("stderr missing manual install hint: %q", got)
	}
}

func TestLaunch_LocalInjectsEnvAndForwardsArgs(t *testing.T) {
	dir := t.TempDir()
	writeInstalled(t, dir, `{"version":2,"plugins":{"sigil-cc@grafana-sigil":[{"scope":"user"}]}}`)
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("SIGIL_AUTH_TENANT_ID", "")
	t.Setenv("SIGIL_AUTH_TOKEN", "")
	t.Setenv("SIGIL_CONTENT_CAPTURE_MODE", "")

	withLookPath(t, func(string) (string, error) { return "/usr/local/bin/claude", nil })
	withRunInstall(t, func(context.Context, string, io.Writer) error {
		t.Fatal("runInstall must not be called when plugin is already present")
		return nil
	})

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
	if err := Launch(context.Background(), []string{"--resume", "abc"}, localEnv, strings.NewReader(""), io.Discard, io.Discard, nopLogger(), "dev"); err != nil {
		t.Fatalf("Launch returned err: %v", err)
	}
	if !reflect.DeepEqual(execArgv, []string{"/usr/local/bin/claude", "--resume", "abc"}) {
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
	if got["SIGIL_CONTENT_CAPTURE_MODE"] != "full" {
		t.Errorf("SIGIL_CONTENT_CAPTURE_MODE = %q, want full", got["SIGIL_CONTENT_CAPTURE_MODE"])
	}
}

func TestLaunch_NormalModeDoesNotInjectLocalEnv(t *testing.T) {
	dir := t.TempDir()
	writeInstalled(t, dir, `{"version":2,"plugins":{"sigil-cc@grafana-sigil":[{"scope":"user"}]}}`)
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("SIGIL_ENDPOINT", "https://cloud.example.com")
	t.Setenv("SIGIL_AUTH_TENANT_ID", "real-tenant")
	t.Setenv("SIGIL_AUTH_TOKEN", "real-token")

	withLookPath(t, func(string) (string, error) { return "/usr/local/bin/claude", nil })

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
		t.Errorf("SIGIL_ENDPOINT changed: %q", got["SIGIL_ENDPOINT"])
	}
	if got["SIGIL_AUTH_TENANT_ID"] != "real-tenant" {
		t.Errorf("SIGIL_AUTH_TENANT_ID changed: %q", got["SIGIL_AUTH_TENANT_ID"])
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

func TestPluginInstalled_KeyShapes(t *testing.T) {
	const cwd = "/work/current-project"
	const otherDir = "/work/other-project"

	cases := []struct {
		name     string
		write    bool
		contents string
		want     bool
		wantErr  bool
	}{
		{
			name:     "user scope matches anywhere",
			write:    true,
			contents: `{"version":2,"plugins":{"sigil-cc@grafana-sigil":[{"scope":"user"}]}}`,
			want:     true,
		},
		{
			name:     "foreign marketplace still matches plugin name",
			write:    true,
			contents: `{"version":2,"plugins":{"sigil-cc@other-marketplace":[{"scope":"user"}]}}`,
			want:     true,
		},
		{
			name:     "project scope matching cwd counts",
			write:    true,
			contents: `{"version":2,"plugins":{"sigil-cc@grafana-sigil":[{"scope":"project","projectPath":"` + cwd + `"}]}}`,
			want:     true,
		},
		{
			name:     "project scope matching cwd via uncleaned path",
			write:    true,
			contents: `{"version":2,"plugins":{"sigil-cc@grafana-sigil":[{"scope":"project","projectPath":"/work/current-project/./"}]}}`,
			want:     true,
		},
		{
			name:     "project scope for another directory does not count",
			write:    true,
			contents: `{"version":2,"plugins":{"sigil-cc@grafana-sigil":[{"scope":"project","projectPath":"` + otherDir + `"}]}}`,
			want:     false,
		},
		{
			name:     "local scope matching cwd counts",
			write:    true,
			contents: `{"version":2,"plugins":{"sigil-cc@grafana-sigil":[{"scope":"local","projectPath":"` + cwd + `"}]}}`,
			want:     true,
		},
		{
			name:     "local scope for another directory does not count",
			write:    true,
			contents: `{"version":2,"plugins":{"sigil-cc@grafana-sigil":[{"scope":"local","projectPath":"` + otherDir + `"}]}}`,
			want:     false,
		},
		{
			name:     "mixed entries: user scope wins over foreign projectPath",
			write:    true,
			contents: `{"version":2,"plugins":{"sigil-cc@grafana-sigil":[{"scope":"project","projectPath":"` + otherDir + `"},{"scope":"user"}]}}`,
			want:     true,
		},
		{
			name:     "only foreign per-directory installs do not count",
			write:    true,
			contents: `{"version":2,"plugins":{"sigil-cc@grafana-sigil":[{"scope":"project","projectPath":"` + otherDir + `"},{"scope":"local","projectPath":"/work/yet-another"}]}}`,
			want:     false,
		},
		{
			name:     "empty top-level object",
			write:    true,
			contents: `{}`,
			want:     false,
		},
		{
			name:     "empty plugins map",
			write:    true,
			contents: `{"version":2,"plugins":{}}`,
			want:     false,
		},
		{
			name:     "empty entry array",
			write:    true,
			contents: `{"version":2,"plugins":{"sigil-cc@grafana-sigil":[]}}`,
			want:     false,
		},
		{
			name:     "unrelated plugins only",
			write:    true,
			contents: `{"version":2,"plugins":{"other-plugin@grafana-sigil":[{"scope":"user"}]}}`,
			want:     false,
		},
		{
			name:     "sigil-cc entry with unexpected shape is skipped, not fatal",
			write:    true,
			contents: `{"version":2,"plugins":{"sigil-cc@grafana-sigil":"oops"}}`,
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
			contents: `{"plugins":{`,
			wantErr:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.write {
				writeInstalled(t, dir, tc.contents)
			}
			t.Setenv("CLAUDE_CONFIG_DIR", dir)
			withGetwd(t, func() (string, error) { return cwd, nil })

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

func TestPluginInstalled_GetwdFailureSurfaces(t *testing.T) {
	dir := t.TempDir()
	writeInstalled(t, dir, `{"version":2,"plugins":{"sigil-cc@grafana-sigil":[{"scope":"project","projectPath":"/whatever"}]}}`)
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	withGetwd(t, func() (string, error) { return "", errors.New("no cwd") })

	if _, err := pluginInstalled(); err == nil || !strings.Contains(err.Error(), "resolve cwd") {
		t.Fatalf("err = %v, want contains \"resolve cwd\"", err)
	}
}

func writeInstalled(t *testing.T, dir, contents string) {
	t.Helper()
	pluginsDir := filepath.Join(dir, "plugins")
	if err := os.MkdirAll(pluginsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pluginsDir, "installed_plugins.json"), []byte(contents), 0o600); err != nil {
		t.Fatalf("write installed_plugins.json: %v", err)
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

func withGetwd(t *testing.T, fn func() (string, error)) {
	t.Helper()
	prev := getwd
	t.Cleanup(func() { getwd = prev })
	getwd = fn
}

func withRunUpdate(t *testing.T, fn func(context.Context, string, io.Writer) error) {
	t.Helper()
	prev := runUpdate
	t.Cleanup(func() { runUpdate = prev })
	runUpdate = fn
}

func launch(t *testing.T, args []string, stdout, stderr io.Writer) error {
	t.Helper()
	return Launch(context.Background(), args, nil, strings.NewReader(""), stdout, stderr, nopLogger(), "dev")
}

func launchOK(t *testing.T, args []string, stdout, stderr io.Writer) {
	t.Helper()
	require.NoError(t, launch(t, args, stdout, stderr))
}

func nopLogger() *log.Logger {
	return log.New(io.Discard, "", 0)
}

func TestLaunch_RefreshesInstalledPlugin(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)

	dir := t.TempDir()
	writeInstalled(t, dir, `{"version":2,"plugins":{"sigil-cc@grafana-sigil":[{"scope":"user"}]}}`)
	t.Setenv("CLAUDE_CONFIG_DIR", dir)

	withLookPath(t, func(string) (string, error) { return "/usr/local/bin/claude", nil })
	withRunInstall(t, func(context.Context, string, io.Writer) error {
		t.Fatal("runInstall must not be called when plugin is already present")
		return nil
	})

	updateCalls := 0
	withRunUpdate(t, func(_ context.Context, bin string, _ io.Writer) error {
		updateCalls++
		if bin != "/usr/local/bin/claude" {
			t.Errorf("update bin = %q", bin)
		}
		return nil
	})
	withExecFn(t, func(string, []string, []string) error { return nil })

	var stderr bytes.Buffer
	launchOK(t, nil, io.Discard, &stderr)
	if updateCalls != 1 {
		t.Fatalf("runUpdate calls = %d, want 1", updateCalls)
	}
	if !strings.Contains(stderr.String(), "refreshing "+PluginName+" in claude") {
		t.Fatalf("stderr missing refresh message: %q", stderr.String())
	}
	stamp := filepath.Join(state, "sigil", "update-checks", PluginName+".stamp")
	if _, err := os.Stat(stamp); err != nil {
		t.Fatalf("expected update stamp: %v", err)
	}
}

func TestStatus(t *testing.T) {
	tests := []struct {
		name          string
		installed     string // installed_plugins.json content; empty means not written
		wantInstalled bool
	}{
		{name: "installed", installed: `{"version":2,"plugins":{"sigil-cc@grafana-sigil":[{"scope":"user"}]}}`, wantInstalled: true},
		{name: "not installed", wantInstalled: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.installed != "" {
				writeInstalled(t, dir, tc.installed)
			}
			t.Setenv("CLAUDE_CONFIG_DIR", dir)

			installed, version, err := Status(context.Background())
			require.NoError(t, err)
			require.Equal(t, tc.wantInstalled, installed)
			require.Empty(t, version) // installed_plugins.json carries no version
		})
	}
}
