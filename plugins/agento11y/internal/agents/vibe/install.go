package vibe

import (
	"bytes"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/grafana/agento11y/plugins/agento11y/internal/execpath"
)

// hookTimeoutSec is vibe's per-hook timeout in seconds. The handler
// already self-imposes a 20s SDK flush budget, but vibe's wrapper
// timeout is the safety net if the binary hangs before reaching the
// flush deadline.
const hookTimeoutSec = 30

// hooksFileEntry mirrors one [[hooks]] table in vibe's hooks.toml.
// Kept as a typed value (rather than a bare map literal) so the desired
// shape is documented in one place and the merge below stays readable.
type hooksFileEntry struct {
	Name    string `toml:"name"`
	Type    string `toml:"type"`
	Command string `toml:"command"`
	Timeout int    `toml:"timeout,omitempty"`
	Match   string `toml:"match,omitempty"`
}

// desiredHooks returns the sigil-owned [[hooks]] entries vibe runs. Vibe
// defines exactly three event types and we wire all three: post_agent_turn
// for the per-turn generation export, before_tool for guard enforcement, and
// after_tool for per-tool span timing. before_tool/after_tool take a "*"
// matcher (every tool); match is forbidden on post_agent_turn. Each entry is
// upserted by its unique name so repeated installs are idempotent, old
// entries written with the literal `sigil vibe hook` command are updated in
// place, and hand-authored hooks in the same file are preserved.
//
// command is the shell command vibe runs for each fire, built from this
// executable's own path so hooks keep working for users who installed only
// the agento11y (or only the legacy sigil) command.
func desiredHooks(command string) []hooksFileEntry {
	return []hooksFileEntry{
		{Name: "sigil", Type: "post_agent_turn", Command: command, Timeout: hookTimeoutSec},
		{Name: "sigil-before-tool", Type: "before_tool", Command: command, Timeout: hookTimeoutSec, Match: "*"},
		{Name: "sigil-after-tool", Type: "after_tool", Command: command, Timeout: hookTimeoutSec, Match: "*"},
	}
}

// vibeHome returns the root vibe config directory. It honors VIBE_HOME
// when set, otherwise falls back to ~/.vibe. The hooks.toml file
// lives directly under this directory.
func vibeHome() (string, error) {
	if home := strings.TrimSpace(os.Getenv("VIBE_HOME")); home != "" {
		return home, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir for vibe: %w", err)
	}
	return filepath.Join(home, ".vibe"), nil
}

// hooksFilePath returns the absolute path to vibe's hooks.toml.
func hooksFilePath() (string, error) {
	home, err := vibeHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "hooks.toml"), nil
}

// ensureHookInstalled merges a sigil-owned post_agent_turn entry into
// vibe's hooks.toml. The write is atomic (temp file + rename), idempotent
// (skipped when the entry already matches), and preserves any
// hand-authored hooks that share the same file.
//
// Returns the path that was inspected (or written) and whether the file
// was actually changed. A best-effort failure path returns the error so
// the caller can log it.
func ensureHookInstalled() (string, bool, error) {
	path, err := hooksFilePath()
	if err != nil {
		return "", false, err
	}
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return path, false, fmt.Errorf("read %s: %w", path, err)
	}

	command, err := execpath.HookCommand("vibe hook")
	if err != nil {
		return path, false, err
	}
	updated, changed, err := mergeHooksTOML(existing, command)
	if err != nil {
		return path, false, err
	}
	if !changed {
		return path, false, nil
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return path, false, fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "hooks.toml.tmp-*")
	if err != nil {
		return path, false, fmt.Errorf("temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	if _, err := tmp.Write(updated); err != nil {
		_ = tmp.Close()
		cleanup()
		return path, false, fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		cleanup()
		return path, false, fmt.Errorf("chmod temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return path, false, fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return path, false, fmt.Errorf("rename to %s: %w", path, err)
	}
	return path, true, nil
}

// mergeHooksTOML decodes the existing hooks.toml bytes, upserts every
// sigil-owned entry in desiredHooks (each by its unique name), and
// re-encodes. If the result matches the input byte-for-byte after
// re-encoding, changed is false and the original bytes are returned so we
// never rewrite a file just to reformat whitespace.
//
// Unknown top-level keys (vibe may add future settings to hooks.toml)
// are preserved by round-tripping through a permissive map.
func mergeHooksTOML(existing []byte, command string) (out []byte, changed bool, err error) {
	// Use a permissive map so we keep anything we don't know about.
	doc := map[string]any{}
	if len(bytes.TrimSpace(existing)) > 0 {
		if err := toml.Unmarshal(existing, &doc); err != nil {
			return nil, false, fmt.Errorf("parse hooks.toml: %w", err)
		}
	}

	hooks, _ := doc["hooks"].([]any)
	for _, desired := range desiredHooks(command) {
		hooks = upsertHook(hooks, desired)
	}
	doc["hooks"] = hooks

	encoded, err := toml.Marshal(doc)
	if err != nil {
		return nil, false, fmt.Errorf("encode hooks.toml: %w", err)
	}
	if bytes.Equal(bytes.TrimSpace(existing), bytes.TrimSpace(encoded)) {
		return existing, false, nil
	}
	return encoded, true, nil
}

// upsertHook replaces the entry whose name matches desired in place
// (preserving any extra keys vibe or the user added) or appends it when
// absent. The known keys are always overwritten so type/command/timeout/match
// converge on the desired shape.
func upsertHook(hooks []any, desired hooksFileEntry) []any {
	fields := map[string]any{
		"name":    desired.Name,
		"type":    desired.Type,
		"command": desired.Command,
		"timeout": int64(desired.Timeout),
	}
	if desired.Match != "" {
		fields["match"] = desired.Match
	}
	for i, raw := range hooks {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if name, _ := entry["name"].(string); name == desired.Name {
			maps.Copy(entry, fields)
			hooks[i] = entry
			return hooks
		}
	}
	return append(hooks, fields)
}
