package vibe

import (
	"os"
	"path/filepath"
	"testing"

	toml "github.com/pelletier/go-toml/v2"
)

func TestEnsureHookInstalled_FreshWrite(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VIBE_HOME", dir)

	path, wrote, err := ensureHookInstalled()
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
		t.Fatalf("hooks len = %d, want 3 (post_agent_turn + before_tool + after_tool)", len(hooks))
	}
	byName := hooksByName(hooks)
	post, ok := byName["sigil"]
	if !ok {
		t.Fatalf("missing sigil post_agent_turn entry; got %v", keys(byName))
	}
	if post["type"] != "post_agent_turn" {
		t.Errorf("type = %v, want post_agent_turn", post["type"])
	}
	if post["command"] != hookCommand {
		t.Errorf("command = %v, want %q", post["command"], hookCommand)
	}
	for name, wantType := range map[string]string{"sigil-before-tool": "before_tool", "sigil-after-tool": "after_tool"} {
		entry, ok := byName[name]
		if !ok {
			t.Fatalf("missing %q entry; got %v", name, keys(byName))
		}
		if entry["type"] != wantType {
			t.Errorf("%s type = %v, want %q", name, entry["type"], wantType)
		}
		if entry["command"] != hookCommand {
			t.Errorf("%s command = %v, want %q", name, entry["command"], hookCommand)
		}
		if entry["match"] != "*" {
			t.Errorf("%s match = %v, want *", name, entry["match"])
		}
	}
}

func TestEnsureHookInstalled_IdempotentNoOp(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VIBE_HOME", dir)

	if _, wrote, err := ensureHookInstalled(); err != nil || !wrote {
		t.Fatalf("first run: wrote=%v err=%v", wrote, err)
	}
	path, wrote, err := ensureHookInstalled()
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
	// must keep it and add (not replace) the sigil entry.
	pre := `[[hooks]]
name = "user-custom"
type = "after_tool"
command = "/bin/true"
timeout = 5
`
	path := filepath.Join(dir, "hooks.toml")
	if err := os.WriteFile(path, []byte(pre), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, _, err := ensureHookInstalled(); err != nil {
		t.Fatalf("install: %v", err)
	}
	got := readTOML(t, path)
	hooks, _ := got["hooks"].([]any)
	if len(hooks) != 4 {
		t.Fatalf("hooks len = %d, want 4 (user-custom + 3 sigil)", len(hooks))
	}
	byName := hooksByName(hooks)
	for _, want := range []string{"user-custom", "sigil", "sigil-before-tool", "sigil-after-tool"} {
		if _, ok := byName[want]; !ok {
			t.Errorf("missing hook %q; got %v", want, keys(byName))
		}
	}
	// The hand-authored hook must be left untouched.
	if byName["user-custom"]["command"] != "/bin/true" {
		t.Errorf("user-custom command = %v, want /bin/true (untouched)", byName["user-custom"]["command"])
	}
}

func TestEnsureHookInstalled_UpdatesStaleSigilEntry(t *testing.T) {
	// A previous sigil version wrote a different command. The merge
	// must overwrite our own entry (matched by name) without producing
	// a duplicate.
	dir := t.TempDir()
	t.Setenv("VIBE_HOME", dir)
	pre := `[[hooks]]
name = "sigil"
type = "post_agent_turn"
command = "old-binary vibe hook"
timeout = 10
`
	path := filepath.Join(dir, "hooks.toml")
	if err := os.WriteFile(path, []byte(pre), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, wrote, err := ensureHookInstalled(); err != nil || !wrote {
		t.Fatalf("install: wrote=%v err=%v", wrote, err)
	}
	got := readTOML(t, path)
	hooks, _ := got["hooks"].([]any)
	if len(hooks) != 3 {
		t.Fatalf("hooks len = %d, want 3 (stale sigil entry updated in place + before/after appended)", len(hooks))
	}
	byName := hooksByName(hooks)
	if byName["sigil"]["command"] != hookCommand {
		t.Errorf("command = %v, want refreshed %q", byName["sigil"]["command"], hookCommand)
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
