package install

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/grafana/agento11y/plugins/agento11y/internal/execpath"
)

const testBin = "/opt/homebrew/bin/sigil"

func TestRun(t *testing.T) {
	const (
		superconductor = "/Users/me/.superconductor/hooks/cursor-notify.sh"
		supersetStart  = "/Users/me/.superset/hooks/cursor-hook.sh Start"
		supersetStop   = "/Users/me/.superset/hooks/cursor-hook.sh Stop"
		legacyRunSh    = "/Users/me/projects/sigil-sdk/plugins/cursor/scripts/run.sh"
		stalePath      = "/old/bin/sigil cursor hook"
	)
	wantCmd := testBin + " cursor hook"

	cases := []struct {
		name string
		// seed is the hooks.json written before Run; "" means no file.
		seed string
		// preserved commands that must survive untouched in their event.
		preserved map[string][]string
		// nonOursEvents are events whose entries Run must not touch at all.
		nonOursEvents map[string][]string
	}{
		{
			name: "fresh file create",
			seed: "",
		},
		{
			name: "null hooks treated as empty",
			seed: `{"version": 1, "hooks": null}`,
		},
		{
			name: "merge preserves other tools entries",
			seed: `{
  "version": 1,
  "hooks": {
    "sessionStart": [
      {"command": "` + legacyRunSh + `"},
      {"command": "` + superconductor + `"}
    ],
    "beforeSubmitPrompt": [
      {"command": "` + supersetStart + `"},
      {"command": "` + legacyRunSh + `"},
      {"command": "` + superconductor + `"}
    ],
    "stop": [
      {"command": "` + supersetStop + `"},
      {"command": "` + legacyRunSh + `"}
    ],
    "beforeShellExecution": [
      {"command": "` + superconductor + `"}
    ]
  }
}`,
			preserved: map[string][]string{
				"sessionStart":       {superconductor},
				"beforeSubmitPrompt": {supersetStart, superconductor},
				"stop":               {supersetStop},
			},
			nonOursEvents: map[string][]string{
				"beforeShellExecution": {superconductor},
			},
		},
		{
			name: "stale sigil path replaced in place",
			seed: `{
  "version": 1,
  "hooks": {
    "sessionStart": [
      {"command": "` + superconductor + `"},
      {"command": "` + stalePath + `"}
    ]
  }
}`,
			preserved: map[string][]string{
				"sessionStart": {superconductor},
			},
		},
		{
			name: "legacy run.sh replaced without a second entry",
			seed: `{
  "version": 1,
  "hooks": {
    "afterAgentResponse": [
      {"command": "` + legacyRunSh + `"}
    ]
  }
}`,
		},
		{
			name: "legacy env-var run.sh replaced without a second entry",
			seed: `{
  "version": 1,
  "hooks": {
    "afterAgentResponse": [
      {"command": "${CURSOR_PLUGIN_ROOT}/scripts/run.sh"}
    ]
  }
}`,
		},
		{
			name: "duplicate sigil entries collapse to one",
			seed: `{
  "version": 1,
  "hooks": {
    "sessionStart": [
      {"command": "` + legacyRunSh + `"},
      {"command": "` + stalePath + `"},
      {"command": "` + superconductor + `"}
    ]
  }
}`,
			preserved: map[string][]string{
				"sessionStart": {superconductor},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			withHome(t, home)
			withExecutable(t, testBin)
			if tc.seed != "" {
				seedHooks(t, home, tc.seed)
			}

			var stdout bytes.Buffer
			require.NoError(t, Run(&stdout, io.Discard, nopLogger()))
			assert.Contains(t, stdout.String(), "wired Cursor hooks at")

			got := readEntries(t, hooksPath(home))

			// Every wired event has exactly one agento11y entry, set to wantCmd.
			for _, ev := range cursorEvents {
				cmds := got[ev]
				require.NotEmpty(t, cmds, "event %q missing", ev)
				ours := oursCommands(cmds)
				require.Len(t, ours, 1, "event %q must have one agento11y entry, got %v", ev, cmds)
				assert.Equal(t, wantCmd, ours[0], "event %q", ev)
			}

			for ev, want := range tc.preserved {
				assert.Equal(t, want, nonOursCommands(got[ev]), "preserved entries for %q", ev)
			}
			for ev, want := range tc.nonOursEvents {
				assert.Equal(t, want, got[ev], "untouched event %q", ev)
			}
		})
	}
}

// Fresh install records the running binary's path verbatim (no symlink
// resolution) and defaults version to 1.
func TestRun_FreshFileShape(t *testing.T) {
	home := t.TempDir()
	withHome(t, home)
	withExecutable(t, testBin)

	require.NoError(t, Run(io.Discard, io.Discard, nopLogger()))

	data, err := os.ReadFile(hooksPath(home))
	require.NoError(t, err)
	var doc struct {
		Version int                         `json:"version"`
		Hooks   map[string][]map[string]any `json:"hooks"`
	}
	require.NoError(t, json.Unmarshal(data, &doc))
	assert.Equal(t, 1, doc.Version)
	require.Len(t, doc.Hooks, len(cursorEvents))
	for _, ev := range cursorEvents {
		require.Len(t, doc.Hooks[ev], 1, "event %q", ev)
		assert.Equal(t, testBin+" cursor hook", doc.Hooks[ev][0]["command"])
	}
}

// version and unknown top-level keys survive a merge round-trip.
func TestRun_PreservesUnknownTopLevelKeys(t *testing.T) {
	home := t.TempDir()
	withHome(t, home)
	withExecutable(t, testBin)
	seedHooks(t, home, `{
  "version": 2,
  "extras": {"keep": ["me"]},
  "hooks": {"sessionStart": [{"command": "/Users/me/.superset/hooks/x.sh"}]}
}`)

	require.NoError(t, Run(io.Discard, io.Discard, nopLogger()))

	data, err := os.ReadFile(hooksPath(home))
	require.NoError(t, err)
	var doc struct {
		Version int             `json:"version"`
		Extras  json.RawMessage `json:"extras"`
	}
	require.NoError(t, json.Unmarshal(data, &doc))
	assert.Equal(t, 2, doc.Version, "existing version must not be reset")
	assert.JSONEq(t, `{"keep": ["me"]}`, string(doc.Extras))
}

func TestRun_Idempotent(t *testing.T) {
	home := t.TempDir()
	withHome(t, home)
	withExecutable(t, testBin)
	seedHooks(t, home, `{
  "version": 1,
  "hooks": {
    "sessionStart": [
      {"command": "/Users/me/.superconductor/hooks/cursor-notify.sh"},
      {"command": "/Users/me/projects/sigil-sdk/plugins/cursor/scripts/run.sh"}
    ]
  }
}`)

	require.NoError(t, Run(io.Discard, io.Discard, nopLogger()))
	first, err := os.ReadFile(hooksPath(home))
	require.NoError(t, err)

	var stdout bytes.Buffer
	require.NoError(t, Run(&stdout, io.Discard, nopLogger()))
	second, err := os.ReadFile(hooksPath(home))
	require.NoError(t, err)

	assert.Equal(t, string(first), string(second), "second run must not change the file")
	assert.Contains(t, stdout.String(), "already up to date")
}

// A corrupt hooks.json must abort with an error and leave the file byte-for-byte
// untouched, so a file shared with other tools is never lost to a partial
// rewrite.
func TestCorruptHooksFileAborts(t *testing.T) {
	const corrupt = `{"version": 1, "hooks": {`
	cases := []struct {
		name string
		run  func(io.Writer, io.Writer, *log.Logger) error
	}{
		{"install", Run},
		{"uninstall", Uninstall},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			withHome(t, home)
			withExecutable(t, testBin)
			dir := filepath.Join(home, ".cursor")
			require.NoError(t, os.MkdirAll(dir, 0o755))
			path := hooksPath(home)
			require.NoError(t, os.WriteFile(path, []byte(corrupt), 0o644))

			require.Error(t, tc.run(io.Discard, io.Discard, nopLogger()))

			data, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, corrupt, string(data), "file must be left untouched on parse error")
		})
	}
}

func TestUninstall(t *testing.T) {
	const (
		superconductor = "/Users/me/.superconductor/hooks/cursor-notify.sh"
		supersetStart  = "/Users/me/.superset/hooks/cursor-hook.sh Start"
		legacyRunSh    = "/Users/me/projects/sigil-sdk/plugins/cursor/scripts/run.sh"
	)

	t.Run("removes only sigil entries", func(t *testing.T) {
		home := t.TempDir()
		withHome(t, home)
		seedHooks(t, home, `{
  "version": 1,
  "hooks": {
    "sessionStart": [
      {"command": "`+legacyRunSh+`"},
      {"command": "`+superconductor+`"}
    ],
    "beforeSubmitPrompt": [
      {"command": "`+supersetStart+`"},
      {"command": "`+legacyRunSh+`"}
    ]
  }
}`)

		var stdout bytes.Buffer
		require.NoError(t, Uninstall(&stdout, io.Discard, nopLogger()))
		assert.Contains(t, stdout.String(), "removed Cursor hooks from")

		got := readEntries(t, hooksPath(home))
		assert.Equal(t, []string{superconductor}, got["sessionStart"])
		assert.Equal(t, []string{supersetStart}, got["beforeSubmitPrompt"])
	})

	t.Run("drops events left empty", func(t *testing.T) {
		home := t.TempDir()
		withHome(t, home)
		seedHooks(t, home, `{
  "version": 1,
  "hooks": {"stop": [{"command": "`+legacyRunSh+`"}]}
}`)

		require.NoError(t, Uninstall(io.Discard, io.Discard, nopLogger()))
		_, ok := readEntries(t, hooksPath(home))["stop"]
		assert.False(t, ok, "empty event array must be dropped")
	})

	t.Run("idempotent on already-clean file", func(t *testing.T) {
		home := t.TempDir()
		withHome(t, home)
		seedHooks(t, home, `{
  "version": 1,
  "hooks": {"sessionStart": [{"command": "`+superconductor+`"}]}
}`)

		require.NoError(t, Uninstall(io.Discard, io.Discard, nopLogger()))
		first, err := os.ReadFile(hooksPath(home))
		require.NoError(t, err)

		var stdout bytes.Buffer
		require.NoError(t, Uninstall(&stdout, io.Discard, nopLogger()))
		second, err := os.ReadFile(hooksPath(home))
		require.NoError(t, err)

		assert.Equal(t, string(first), string(second))
		assert.Contains(t, stdout.String(), "no Cursor hooks to remove")
		assert.Equal(t, []string{superconductor}, readEntries(t, hooksPath(home))["sessionStart"])
	})

	t.Run("missing file is a no-op", func(t *testing.T) {
		home := t.TempDir()
		withHome(t, home)

		var stdout bytes.Buffer
		require.NoError(t, Uninstall(&stdout, io.Discard, nopLogger()))
		assert.Contains(t, stdout.String(), "no Cursor hooks to remove")
		_, err := os.Stat(hooksPath(home))
		assert.True(t, os.IsNotExist(err), "uninstall must not create the file")
	})
}

func TestInstallThenUninstallRoundTrip(t *testing.T) {
	const superconductor = "/Users/me/.superconductor/hooks/cursor-notify.sh"
	home := t.TempDir()
	withHome(t, home)
	withExecutable(t, testBin)
	seedHooks(t, home, `{
  "version": 1,
  "hooks": {"sessionStart": [{"command": "`+superconductor+`"}]}
}`)

	require.NoError(t, Run(io.Discard, io.Discard, nopLogger()))
	require.NoError(t, Uninstall(io.Discard, io.Discard, nopLogger()))

	got := readEntries(t, hooksPath(home))
	assert.Equal(t, []string{superconductor}, got["sessionStart"])
	for _, ev := range cursorEvents {
		assert.Empty(t, oursCommands(got[ev]), "event %q still has an agento11y entry", ev)
	}
}

func TestIsOursHook(t *testing.T) {
	cases := []struct {
		cmd  string
		want bool
	}{
		{"/opt/homebrew/bin/sigil cursor hook", true},
		{"sigil cursor hook", true},
		{"/opt/homebrew/bin/agento11y cursor hook", true},
		{"agento11y cursor hook", true},
		{"agento11y.exe cursor hook", true},
		{"'/Users/me/with space/bin/agento11y' cursor hook", true},
		{"/Users/me/.local/bin/sigil cursor hook", true},
		{"'/Users/me/with space/bin/sigil' cursor hook", true},
		{"/Users/me/projects/sigil-sdk/plugins/cursor/scripts/run.sh", true},
		{"${CURSOR_PLUGIN_ROOT}/scripts/run.sh", true},
		{"/opt/homebrew/bin/sigil.exe cursor hook", true},
		{"  /opt/homebrew/bin/sigil cursor hook  ", true},
		{"/usr/bin/notsigil cursor hook", false},
		{"/opt/homebrew/bin/sigil claude hook", false},
		{"/Users/me/.superconductor/hooks/cursor-notify.sh", false},
		{"/Users/me/.superset/hooks/cursor-hook.sh Start", false},
		{"", false},
		{"cursor hook", false},
	}
	for _, tc := range cases {
		t.Run(tc.cmd, func(t *testing.T) {
			assert.Equal(t, tc.want, isOursHook(tc.cmd))
		})
	}
}

// --- helpers ---

func withHome(t *testing.T, home string) {
	t.Helper()
	prev := userHomeDir
	t.Cleanup(func() { userHomeDir = prev })
	userHomeDir = func() (string, error) { return home, nil }
}

func withExecutable(t *testing.T, path string) {
	t.Helper()
	prev := execpath.Executable
	t.Cleanup(func() { execpath.Executable = prev })
	execpath.Executable = func() (string, error) { return path, nil }
}

func hooksPath(home string) string {
	return filepath.Join(home, ".cursor", "hooks.json")
}

func seedHooks(t *testing.T, home, content string) {
	t.Helper()
	require.True(t, json.Valid([]byte(content)), "seed must be valid JSON")
	dir := filepath.Join(home, ".cursor")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hooks.json"), []byte(content), 0o644))
}

// readEntries parses the hooks file into event -> ordered command strings.
func readEntries(t *testing.T, path string) map[string][]string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var doc struct {
		Hooks map[string][]struct {
			Command string `json:"command"`
		} `json:"hooks"`
	}
	require.NoError(t, json.Unmarshal(data, &doc))
	out := map[string][]string{}
	for ev, entries := range doc.Hooks {
		cmds := make([]string, 0, len(entries))
		for _, e := range entries {
			cmds = append(cmds, e.Command)
		}
		out[ev] = cmds
	}
	return out
}

func oursCommands(cmds []string) []string {
	var out []string
	for _, c := range cmds {
		if isOursHook(c) {
			out = append(out, c)
		}
	}
	return out
}

func nonOursCommands(cmds []string) []string {
	var out []string
	for _, c := range cmds {
		if !isOursHook(c) {
			out = append(out, c)
		}
	}
	return out
}

func nopLogger() *log.Logger {
	return log.New(io.Discard, "", 0)
}
