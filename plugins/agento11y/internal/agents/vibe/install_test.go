package vibe

import (
	"os"
	"path/filepath"
	"testing"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/grafana/agento11y/plugins/agento11y/internal/execpath"
)

// withExecutable pins the executable path hook commands are built from, so
// tests can assert the exact generated command line.
func withExecutable(t *testing.T, path string) {
	t.Helper()
	prev := execpath.Executable
	t.Cleanup(func() { execpath.Executable = prev })
	execpath.Executable = func() (string, error) { return path, nil }
}

func TestEnsureHookInstalled_FreshWrite(t *testing.T) {
	// The three entry names never change; only the types do, and which set
	// gets written follows the installed vibe.
	tests := []struct {
		name      string
		types     hookTypeSet
		wantTypes map[string]string
	}{
		{
			name:  "vibe 2.21.0 and later",
			types: currentHookTypes,
			wantTypes: map[string]string{
				"agento11y":             "post_agent",
				"agento11y-before-tool": "pre_tool",
				"agento11y-after-tool":  "post_tool",
			},
		},
		{
			name:  "before vibe 2.21.0",
			types: preRenameHookTypes,
			wantTypes: map[string]string{
				"agento11y":             "post_agent_turn",
				"agento11y-before-tool": "before_tool",
				"agento11y-after-tool":  "after_tool",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("VIBE_HOME", dir)
			withExecutable(t, "/usr/local/bin/agento11y")
			const wantCommand = "/usr/local/bin/agento11y vibe hook"

			path, wrote, err := ensureHookInstalled(tt.types)
			if err != nil {
				t.Fatalf("ensureHookInstalled: %v", err)
			}
			if !wrote {
				t.Errorf("wrote = false, want true on fresh install")
			}
			if path != filepath.Join(dir, "hooks.toml") {
				t.Errorf("path = %q, want %q", path, filepath.Join(dir, "hooks.toml"))
			}
			got := readTOML(t, path)
			hooks, _ := got["hooks"].([]any)
			if len(hooks) != 3 {
				t.Fatalf("hooks len = %d, want 3 (one per event)", len(hooks))
			}
			byName := hooksByName(hooks)
			for name, wantType := range tt.wantTypes {
				entry, ok := byName[name]
				if !ok {
					t.Fatalf("missing %q entry; got %v", name, keys(byName))
				}
				if entry["type"] != wantType {
					t.Errorf("%s type = %v, want %q", name, entry["type"], wantType)
				}
				if entry["command"] != wantCommand {
					t.Errorf("%s command = %v, want %q", name, entry["command"], wantCommand)
				}
			}
			// match is forbidden on the post-agent hook and required on the two
			// tool hooks.
			if _, ok := byName["agento11y"]["match"]; ok {
				t.Errorf("agento11y carries match = %v, want none", byName["agento11y"]["match"])
			}
			for _, name := range []string{"agento11y-before-tool", "agento11y-after-tool"} {
				if byName[name]["match"] != "*" {
					t.Errorf("%s match = %v, want *", name, byName[name]["match"])
				}
			}
		})
	}
}

func TestEnsureHookInstalled_IdempotentNoOp(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VIBE_HOME", dir)

	if _, wrote, err := ensureHookInstalled(currentHookTypes); err != nil || !wrote {
		t.Fatalf("first run: wrote=%v err=%v", wrote, err)
	}
	path, wrote, err := ensureHookInstalled(currentHookTypes)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if wrote {
		t.Errorf("second run wrote = true, want false (no-op)")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("hooks.toml went missing: %v", err)
	}
}

func TestEnsureHookInstalled_PreservesExistingHook(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VIBE_HOME", dir)

	// A user already has a hand-authored hook in hooks.toml. Install
	// must keep it and add (not replace) the agento11y entry.
	pre := `[[hooks]]
name = "user-custom"
type = "post_tool"
command = "/bin/true"
timeout = 5
`
	path := filepath.Join(dir, "hooks.toml")
	if err := os.WriteFile(path, []byte(pre), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, _, err := ensureHookInstalled(currentHookTypes); err != nil {
		t.Fatalf("install: %v", err)
	}
	got := readTOML(t, path)
	hooks, _ := got["hooks"].([]any)
	if len(hooks) != 4 {
		t.Fatalf("hooks len = %d, want 4 (user-custom + 3 agento11y)", len(hooks))
	}
	byName := hooksByName(hooks)
	for _, want := range []string{"user-custom", "agento11y", "agento11y-before-tool", "agento11y-after-tool"} {
		if _, ok := byName[want]; !ok {
			t.Errorf("missing hook %q; got %v", want, keys(byName))
		}
	}
	// The hand-authored hook must be left untouched.
	if byName["user-custom"]["command"] != "/bin/true" {
		t.Errorf("user-custom command = %v, want /bin/true (untouched)", byName["user-custom"]["command"])
	}
}

func TestEnsureHookInstalled_ReplacesLegacySigilEntries(t *testing.T) {
	// A previous version wrote hooks under the pre-rename sigil names. The
	// merge must drop those entries and install the agento11y-named ones so
	// vibe does not fire every hook twice.
	dir := t.TempDir()
	t.Setenv("VIBE_HOME", dir)
	withExecutable(t, "/usr/local/bin/agento11y")
	const wantCommand = "/usr/local/bin/agento11y vibe hook"
	pre := `[[hooks]]
name = "sigil"
type = "post_agent_turn"
command = "sigil vibe hook"
timeout = 10

[[hooks]]
name = "sigil-before-tool"
type = "before_tool"
command = "sigil vibe hook"
timeout = 10
match = "*"
`
	path := filepath.Join(dir, "hooks.toml")
	if err := os.WriteFile(path, []byte(pre), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, wrote, err := ensureHookInstalled(currentHookTypes); err != nil || !wrote {
		t.Fatalf("install: wrote=%v err=%v", wrote, err)
	}
	got := readTOML(t, path)
	hooks, _ := got["hooks"].([]any)
	if len(hooks) != 3 {
		t.Fatalf("hooks len = %d, want 3 (legacy entries dropped, agento11y entries installed)", len(hooks))
	}
	byName := hooksByName(hooks)
	for _, legacy := range []string{"sigil", "sigil-before-tool", "sigil-after-tool"} {
		if _, ok := byName[legacy]; ok {
			t.Errorf("legacy hook %q still present; got %v", legacy, keys(byName))
		}
	}
	if byName["agento11y"]["command"] != wantCommand {
		t.Errorf("command = %v, want refreshed %q", byName["agento11y"]["command"], wantCommand)
	}
}

func TestEnsureHookInstalled_ConvertsTypesInPlace(t *testing.T) {
	// Entry names are the same on both sides of the 2.21.0 rename, so crossing
	// it (a vibe upgrade, or a downgrade) must rewrite three types in place
	// rather than add a second entry per event. Both directions matter: the
	// launcher writes whatever the vibe on PATH accepts, and that answer can
	// change under a hooks.toml that already exists.
	tests := []struct {
		name  string
		seed  hookTypeSet
		into  hookTypeSet
		want  map[string]string
		count int
	}{
		{
			name: "pre-rename install on a 2.21.0 or later vibe",
			seed: preRenameHookTypes,
			into: currentHookTypes,
			want: map[string]string{
				"agento11y":             "post_agent",
				"agento11y-before-tool": "pre_tool",
				"agento11y-after-tool":  "post_tool",
			},
		},
		{
			name: "current install on a pre-2.21.0 vibe",
			seed: currentHookTypes,
			into: preRenameHookTypes,
			want: map[string]string{
				"agento11y":             "post_agent_turn",
				"agento11y-before-tool": "before_tool",
				"agento11y-after-tool":  "after_tool",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("VIBE_HOME", dir)
			seeded, _, err := mergeHooksTOML(nil, "agento11y vibe hook", tt.seed)
			if err != nil {
				t.Fatalf("render seed: %v", err)
			}
			path := filepath.Join(dir, "hooks.toml")
			if err := os.WriteFile(path, seeded, 0o644); err != nil {
				t.Fatalf("seed: %v", err)
			}
			if _, wrote, err := ensureHookInstalled(tt.into); err != nil || !wrote {
				t.Fatalf("install: wrote=%v err=%v", wrote, err)
			}
			got := readTOML(t, path)
			hooks, _ := got["hooks"].([]any)
			if len(hooks) != 3 {
				t.Fatalf("hooks len = %d, want 3 (types converted in place); got %v", len(hooks), hooks)
			}
			byName := hooksByName(hooks)
			for name, wantType := range tt.want {
				if byName[name]["type"] != wantType {
					t.Errorf("%s type = %v, want %q", name, byName[name]["type"], wantType)
				}
			}
		})
	}
}

func TestEnsureHookInstalled_QuotesExecutablePath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VIBE_HOME", dir)
	withExecutable(t, "/Users/Jane Doe/bin/agento11y")

	path, _, err := ensureHookInstalled(currentHookTypes)
	if err != nil {
		t.Fatalf("ensureHookInstalled: %v", err)
	}
	got := readTOML(t, path)
	hooks, _ := got["hooks"].([]any)
	byName := hooksByName(hooks)
	want := "'/Users/Jane Doe/bin/agento11y' vibe hook"
	if byName["agento11y"]["command"] != want {
		t.Errorf("command = %v, want %q", byName["agento11y"]["command"], want)
	}
}

func TestHooksInstalled(t *testing.T) {
	entryCmd := func(name, typ, command string) string {
		return "[[hooks]]\nname = \"" + name + "\"\ntype = \"" + typ + "\"\ncommand = \"" + command + "\"\ntimeout = 30\n\n"
	}
	entry := func(name, typ string) string {
		return entryCmd(name, typ, "agento11y vibe hook")
	}
	all := entry("agento11y", "post_agent") +
		entry("agento11y-before-tool", "pre_tool") +
		entry("agento11y-after-tool", "post_tool")
	allPreRename := entry("agento11y", "post_agent_turn") +
		entry("agento11y-before-tool", "before_tool") +
		entry("agento11y-after-tool", "after_tool")
	// Every binary that wrote the sigil names also wrote the pre-2.21.0 types.
	allLegacy := entryCmd("sigil", "post_agent_turn", "sigil vibe hook") +
		entryCmd("sigil-before-tool", "before_tool", "sigil vibe hook") +
		entryCmd("sigil-after-tool", "after_tool", "sigil vibe hook")
	staleCommand := entryCmd("agento11y", "post_agent", "/old/path/agento11y vibe hook") +
		entryCmd("agento11y-before-tool", "pre_tool", "/old/path/agento11y vibe hook") +
		entryCmd("agento11y-after-tool", "post_tool", "/old/path/agento11y vibe hook")
	userHook := entry("user-custom", "post_tool")

	// types is the set the installed vibe accepts. An entry carrying the other
	// set is skipped by vibe at load, so it does not count as installed no
	// matter which name it goes under.
	tests := []struct {
		name    string
		file    string
		types   hookTypeSet
		write   bool // false leaves hooks.toml absent
		want    bool
		wantErr bool
	}{
		{name: "all entries present", file: all, types: currentHookTypes, write: true, want: true},
		{name: "all entries plus hand-authored hook", file: userHook + all, types: currentHookTypes, write: true, want: true},
		// The command is never compared: an install written by a binary at a
		// different path still fires.
		{name: "stale command", file: staleCommand, types: currentHookTypes, write: true, want: true},
		{name: "one entry missing", file: entry("agento11y", "post_agent") + entry("agento11y-before-tool", "pre_tool"), types: currentHookTypes, write: true},
		{name: "pre-2.21.0 type under a current name", file: entry("agento11y", "post_agent_turn") + entry("agento11y-before-tool", "pre_tool") + entry("agento11y-after-tool", "post_tool"), types: currentHookTypes, write: true},
		{name: "pre-2.21.0 types on a current vibe", file: allPreRename, types: currentHookTypes, write: true},
		{name: "pre-2.21.0 types on a pre-2.21.0 vibe", file: allPreRename, types: preRenameHookTypes, write: true, want: true},
		{name: "current types on a pre-2.21.0 vibe", file: all, types: preRenameHookTypes, write: true},
		// A sigil-era install still captures on the vibe it was written for, and
		// doctor must not call that broken. It survives only until the next
		// launch, which replaces it with the agento11y-named entries.
		{name: "legacy names and types on a pre-2.21.0 vibe", file: allLegacy, types: preRenameHookTypes, write: true, want: true},
		{name: "legacy names and types on a current vibe", file: allLegacy, types: currentHookTypes, write: true},
		{name: "legacy names with one missing", file: entryCmd("sigil", "post_agent_turn", "sigil vibe hook") + entryCmd("sigil-before-tool", "before_tool", "sigil vibe hook"), types: preRenameHookTypes, write: true},
		{name: "current and legacy names mixed", file: entry("agento11y", "post_agent_turn") + entryCmd("sigil-before-tool", "before_tool", "sigil vibe hook") + entry("agento11y-after-tool", "after_tool"), types: preRenameHookTypes, write: true, want: true},
		{name: "only hand-authored hooks", file: userHook, types: currentHookTypes, write: true},
		{name: "empty file", file: "", types: currentHookTypes, write: true},
		{name: "no file", types: currentHookTypes},
		{name: "invalid toml", file: "[[hooks]\nname =", types: currentHookTypes, write: true, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("VIBE_HOME", dir)
			if tt.write {
				if err := os.WriteFile(filepath.Join(dir, "hooks.toml"), []byte(tt.file), 0o644); err != nil {
					t.Fatalf("seed: %v", err)
				}
			}
			got, err := HooksInstalled(tt.types)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("HooksInstalled() error = nil, want non-nil")
				}
			} else if err != nil {
				t.Fatalf("HooksInstalled(): %v", err)
			}
			if got != tt.want {
				t.Errorf("HooksInstalled() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestHooksInstalled_AfterInstall pins the probe to the writer: whatever
// ensureHookInstalled writes must read back as installed, for either vibe.
func TestHooksInstalled_AfterInstall(t *testing.T) {
	for name, types := range map[string]hookTypeSet{
		"vibe 2.21.0 and later": currentHookTypes,
		"before vibe 2.21.0":    preRenameHookTypes,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("VIBE_HOME", dir)

			if _, _, err := ensureHookInstalled(types); err != nil {
				t.Fatalf("ensureHookInstalled: %v", err)
			}
			got, err := HooksInstalled(types)
			if err != nil {
				t.Fatalf("HooksInstalled() after install: %v", err)
			}
			if !got {
				t.Errorf("HooksInstalled() = false after install, want true")
			}
		})
	}
}

func TestVibeHome_HonorsEnv(t *testing.T) {
	t.Setenv("VIBE_HOME", "/custom/vibe-root")
	got, err := vibeHome()
	if err != nil {
		t.Fatalf("vibeHome: %v", err)
	}
	if got != "/custom/vibe-root" {
		t.Errorf("vibeHome = %q, want /custom/vibe-root", got)
	}
}

func TestVibeHome_DefaultsToHomeDotVibe(t *testing.T) {
	t.Setenv("VIBE_HOME", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("UserHomeDir: %v", err)
	}
	got, err := vibeHome()
	if err != nil {
		t.Fatalf("vibeHome: %v", err)
	}
	want := filepath.Join(home, ".vibe")
	if got != want {
		t.Errorf("vibeHome = %q, want %q", got, want)
	}
}

func readTOML(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	out := map[string]any{}
	if err := toml.Unmarshal(data, &out); err != nil {
		t.Fatalf("parse %s: %v\nbody:\n%s", path, err, string(data))
	}
	return out
}

func hooksByName(hooks []any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, h := range hooks {
		entry, ok := h.(map[string]any)
		if !ok {
			continue
		}
		if name, ok := entry["name"].(string); ok {
			out[name] = entry
		}
	}
	return out
}

func keys(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
