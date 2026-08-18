package vibe

import (
	"bytes"
	"errors"
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

// desiredHooks returns the agento11y-owned [[hooks]] entries vibe runs. Vibe
// defines exactly three event types and we wire all three: post_agent for the
// per-turn generation export, pre_tool for guard enforcement, and post_tool
// for per-tool span timing. The two tool hooks take a "*" matcher (every
// tool); match is forbidden on post_agent. Each entry is upserted by its
// unique name so repeated installs are idempotent and hand-authored hooks in
// the same file are preserved.
//
// types carries the spelling of those three events. Before vibe 2.21.0 they
// were post_agent_turn, before_tool, and after_tool. Entry names are the same
// on both sides of that rename, so an install that crosses it rewrites three
// types in place rather than adding three entries.
//
// command is the shell command vibe runs for each fire, built from this
// executable's own path so hooks keep working for users who installed only
// the agento11y (or only the legacy sigil) command.
func desiredHooks(command string, types hookTypeSet) []hooksFileEntry {
	return []hooksFileEntry{
		{Name: "agento11y", Type: types.postAgent, Command: command, Timeout: hookTimeoutSec},
		{Name: "agento11y-before-tool", Type: types.preTool, Command: command, Timeout: hookTimeoutSec, Match: "*"},
		{Name: "agento11y-after-tool", Type: types.postTool, Command: command, Timeout: hookTimeoutSec, Match: "*"},
	}
}

// legacyHookNames maps each agento11y-owned entry name to the pre-rename name
// an older version wrote. The merge drops legacy entries so a refreshed
// install does not fire every hook twice (once per name), but HooksInstalled
// still counts one whose type this vibe accepts: it keeps capturing until the
// next launch replaces it, and doctor must not report a working install as
// broken.
var legacyHookNames = map[string]string{
	"agento11y":             "sigil",
	"agento11y-before-tool": "sigil-before-tool",
	"agento11y-after-tool":  "sigil-after-tool",
}

// isLegacyHookName reports whether name is a pre-rename entry name.
func isLegacyHookName(name string) bool {
	for _, legacy := range legacyHookNames {
		if name == legacy {
			return true
		}
	}
	return false
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

// HooksInstalled reports whether every agento11y-owned entry is present in
// vibe's hooks.toml, under its current or its pre-rename name, carrying the
// type the installed vibe accepts. It reads the file directly, so
// `agento11y doctor` can report install state without vibe on PATH. Read-only:
// it never merges or writes.
//
// The type has to match because vibe validates it against an enum and skips
// the entry when it does not, so an install spelled for the other side of the
// 2.21.0 rename captures nothing.
//
// A hooks.toml holding only hand-authored hooks does not count as installed.
func HooksInstalled(types hookTypeSet) (bool, error) {
	path, err := hooksFilePath()
	if err != nil {
		return false, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	// Only each entry's name and type matter, so decode those and ignore every
	// other key a hand-authored hook may carry.
	var doc struct {
		Hooks []struct {
			Name string `toml:"name"`
			Type string `toml:"type"`
		} `toml:"hooks"`
	}
	if err := toml.Unmarshal(data, &doc); err != nil {
		return false, fmt.Errorf("parse %s: %w", path, err)
	}
	typeByName := map[string]string{}
	for _, entry := range doc.Hooks {
		typeByName[entry.Name] = entry.Type
	}
	for _, want := range desiredHooks("", types) {
		if typeByName[want.Name] == want.Type {
			continue
		}
		if legacy := legacyHookNames[want.Name]; legacy != "" && typeByName[legacy] == want.Type {
			continue
		}
		return false, nil
	}
	return true, nil
}

// ensureHookInstalled merges the three agento11y-owned entries into vibe's
// hooks.toml, spelled for the installed vibe. The write is atomic (temp file +
// rename), idempotent (skipped when the entries already match), and preserves
// any hand-authored hooks that share the same file.
//
// Returns the path that was inspected (or written) and whether the file
// was actually changed. A best-effort failure path returns the error so
// the caller can log it.
func ensureHookInstalled(types hookTypeSet) (string, bool, error) {
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
	updated, changed, err := mergeHooksTOML(existing, command, types)
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

// mergeHooksTOML decodes the existing hooks.toml bytes, drops entries with
// legacy pre-rename names, upserts every agento11y-owned entry in
// desiredHooks (each by its unique name), and
// re-encodes. If the result matches the input byte-for-byte after
// re-encoding, changed is false and the original bytes are returned so we
// never rewrite a file just to reformat whitespace.
//
// Unknown top-level keys (vibe may add future settings to hooks.toml)
// are preserved by round-tripping through a permissive map.
func mergeHooksTOML(existing []byte, command string, types hookTypeSet) (out []byte, changed bool, err error) {
	// Use a permissive map so we keep anything we don't know about.
	doc := map[string]any{}
	if len(bytes.TrimSpace(existing)) > 0 {
		if err := toml.Unmarshal(existing, &doc); err != nil {
			return nil, false, fmt.Errorf("parse hooks.toml: %w", err)
		}
	}

	hooks, _ := doc["hooks"].([]any)
	hooks = dropLegacyHooks(hooks)
	for _, desired := range desiredHooks(command, types) {
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

// dropLegacyHooks removes entries whose name matches a pre-rename hook name.
func dropLegacyHooks(hooks []any) []any {
	out := hooks[:0]
	for _, raw := range hooks {
		if entry, ok := raw.(map[string]any); ok {
			if name, _ := entry["name"].(string); isLegacyHookName(name) {
				continue
			}
		}
		out = append(out, raw)
	}
	return out
}
