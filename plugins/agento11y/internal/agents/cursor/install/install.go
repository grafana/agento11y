// Package install wires agento11y's Cursor hook into the user-level
// ~/.cursor/hooks.json from the command line.
//
// Cursor is a GUI app with no exec launch point, so unlike the other agents
// there is no `agento11y cursor` launcher to bootstrap capture on first run.
// `agento11y cursor install` writes the hook entry directly, the same way
// `agento11y copilot` writes Copilot's hooks file.
//
// hooks.json is shared with other tools (the live file holds superconductor
// and superset entries alongside agento11y's), so install MERGES into it rather
// than overwriting: it keeps unknown top-level keys and every event entry it
// does not own, and replaces its own entry in place so re-runs do not
// double-fire. The legacy /add-plugin run.sh entry is recognised on a
// best-effort basis (see isOursHook), so users should not run direct install
// and /add-plugin together.
package install

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"github.com/grafana/agento11y/plugins/agento11y/internal/execpath"
)

// cursorEvents are the hook events agento11y wires, matching the set shipped in
// plugins/cursor/hooks/hooks.json. The Cursor dispatcher infers the event
// from hook_event_name in the payload, so the same `agento11y cursor hook`
// command serves all nine.
var cursorEvents = []string{
	"sessionStart",
	"beforeSubmitPrompt",
	"preToolUse",
	"afterAgentResponse",
	"afterAgentThought",
	"postToolUse",
	"postToolUseFailure",
	"stop",
	"sessionEnd",
}

// Test seam.
var userHomeDir = os.UserHomeDir

// hookEntry is a single Cursor hook command entry. Cursor entries may carry
// other fields, but agento11y only ever writes the command; extra fields on other
// tools' entries are preserved as raw JSON, not through this type.
type hookEntry struct {
	Command string `json:"command"`
}

// Run wires `agento11y cursor hook` into ~/.cursor/hooks.json for all eight
// Cursor hook events, creating the file and parent directory when absent. It
// merges into an existing file: unknown top-level keys and other tools'
// entries are preserved, and a recognised pre-existing agento11y entry (a previous
// install, or a legacy run.sh entry when detectable) is replaced in place to
// avoid double-firing capture. The write is atomic and idempotent — when the
// result already matches what is on disk, the file is left untouched.
func Run(stdout, _ io.Writer, logger *log.Logger) error {
	path, err := cursorHooksPath()
	if err != nil {
		return err
	}
	cmd, err := execpath.HookCommand("cursor hook")
	if err != nil {
		return err
	}
	logger.Printf("cursor install: hooks=%s command=%q", path, cmd)

	doc, err := loadHooks(path)
	if err != nil {
		return err
	}

	entry, err := json.Marshal(hookEntry{Command: cmd})
	if err != nil {
		return fmt.Errorf("encode cursor hook entry: %w", err)
	}
	for _, event := range cursorEvents {
		doc.hooks[event] = upsertOurs(doc.hooks[event], entry)
	}

	wrote, err := writeHooks(path, doc)
	if err != nil {
		return err
	}
	if wrote {
		_, _ = fmt.Fprintf(stdout, "agento11y: wired Cursor hooks at %s\n", path)
	} else {
		_, _ = fmt.Fprintf(stdout, "agento11y: Cursor hooks already up to date at %s\n", path)
	}
	return nil
}

// Uninstall removes agento11y's hook entries from ~/.cursor/hooks.json, leaving
// other tools' entries and unknown keys intact. Event arrays left empty are
// dropped. The write is atomic and idempotent — a file with no agento11y entries
// is left untouched, and a missing file is a no-op (no file is created).
func Uninstall(stdout, _ io.Writer, logger *log.Logger) error {
	path, err := cursorHooksPath()
	if err != nil {
		return err
	}
	logger.Printf("cursor uninstall: hooks=%s", path)

	if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
		_, _ = fmt.Fprintf(stdout, "agento11y: no Cursor hooks to remove at %s\n", path)
		return nil
	}

	doc, err := loadHooks(path)
	if err != nil {
		return err
	}
	for event, entries := range doc.hooks {
		kept := removeOurs(entries)
		if len(kept) == 0 {
			delete(doc.hooks, event)
			continue
		}
		doc.hooks[event] = kept
	}

	wrote, err := writeHooks(path, doc)
	if err != nil {
		return err
	}
	if wrote {
		_, _ = fmt.Fprintf(stdout, "agento11y: removed Cursor hooks from %s\n", path)
	} else {
		_, _ = fmt.Fprintf(stdout, "agento11y: no Cursor hooks to remove at %s\n", path)
	}
	return nil
}

// cursorHooksPath returns the user-level Cursor hooks file path.
//
// Cursor reads ~/.cursor/hooks.json for user-global hooks. No config-dir
// override env var is documented (no CURSOR_* knob exists in Cursor's hook
// docs), so this is fixed to ~/.cursor for now.
//
// TODO: honor a Cursor config-dir override env var once one is confirmed.
func cursorHooksPath() (string, error) {
	home, err := userHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir for cursor hooks: %w", err)
	}
	return filepath.Join(home, ".cursor", "hooks.json"), nil
}

// legacyRunShLiteral is the hook command the bundled /add-plugin wiring
// registers (plugins/cursor/hooks/hooks.json). Cursor usually expands
// CURSOR_PLUGIN_ROOT when it copies the entry into the user-level hooks.json,
// leaving the repo's plugins/cursor/scripts/run.sh tail in the path, but it may
// also keep the literal, so isOursHook checks for both.
const legacyRunShLiteral = "${CURSOR_PLUGIN_ROOT}/scripts/run.sh"

// isOursHook reports whether an existing entry's command is one of ours, so
// re-runs and the legacy /add-plugin run.sh wiring update in place instead of
// double-firing. It matches any command of the form `<binary> cursor hook`
// where the binary's basename is agento11y or sigil, plus the legacy run.sh
// entry in both its bundled "${CURSOR_PLUGIN_ROOT}/scripts/run.sh" form and
// the expanded form Cursor writes once it resolves CURSOR_PLUGIN_ROOT. Legacy
// detection is best-effort: an expanded path that no longer carries the
// plugins/cursor segment is not recognised.
func isOursHook(cmd string) bool {
	c := strings.TrimSpace(cmd)
	if strings.Contains(c, legacyRunShLiteral) ||
		strings.Contains(c, filepath.Join("plugins", "cursor", "scripts", "run.sh")) {
		return true
	}
	bin, ok := strings.CutSuffix(c, " cursor hook")
	if !ok {
		return false
	}
	bin = unquote(strings.TrimSpace(bin))
	base := strings.TrimSuffix(filepath.Base(bin), ".exe")
	return bin != "" && (base == "agento11y" || base == "sigil")
}

// upsertOurs replaces the first agento11y-owned entry in entries with cmd (in
// place, so other tools' entries keep their order) and drops any further
// agento11y entries; when none is present it appends cmd.
func upsertOurs(entries []json.RawMessage, cmd json.RawMessage) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(entries)+1)
	inserted := false
	for _, raw := range entries {
		if isOursHook(commandOf(raw)) {
			if !inserted {
				out = append(out, cmd)
				inserted = true
			}
			continue
		}
		out = append(out, raw)
	}
	if !inserted {
		out = append(out, cmd)
	}
	return out
}

// removeOurs returns entries with every agento11y-owned entry dropped.
func removeOurs(entries []json.RawMessage) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(entries))
	for _, raw := range entries {
		if isOursHook(commandOf(raw)) {
			continue
		}
		out = append(out, raw)
	}
	return out
}

// commandOf extracts the "command" string from a hook entry, returning ""
// when the entry is not an object or has no command (such entries are never
// treated as agento11y's).
func commandOf(raw json.RawMessage) string {
	var e hookEntry
	if err := json.Unmarshal(raw, &e); err != nil {
		return ""
	}
	return e.Command
}

// hooksDoc is the parsed hooks.json. Top-level keys are preserved as raw JSON
// (so `version` and any unknown keys survive a round-trip) with the `hooks`
// object pulled out into a per-event entry list we can edit.
type hooksDoc struct {
	top   map[string]json.RawMessage
	hooks map[string][]json.RawMessage
}

// loadHooks reads and parses path. A missing or empty file yields an empty
// document so callers can treat fresh installs and merges uniformly.
func loadHooks(path string) (*hooksDoc, error) {
	doc := &hooksDoc{
		top:   map[string]json.RawMessage{},
		hooks: map[string][]json.RawMessage{},
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return doc, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return doc, nil
	}
	if err := json.Unmarshal(data, &doc.top); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if rawHooks, ok := doc.top["hooks"]; ok {
		if err := json.Unmarshal(rawHooks, &doc.hooks); err != nil {
			return nil, fmt.Errorf("parse hooks in %s: %w", path, err)
		}
		// A literal `"hooks": null` unmarshals to a nil map; reset it so Run
		// can assign event entries without panicking.
		if doc.hooks == nil {
			doc.hooks = map[string][]json.RawMessage{}
		}
	}
	// hooks is reattached from doc.hooks at render time.
	delete(doc.top, "hooks")
	return doc, nil
}

// renderHooks serialises doc back to indented JSON. The hooks map is
// reattached under the "hooks" key and version defaults to 1 when absent.
// Output is stable (encoding/json sorts map keys) so writeHooks can skip
// no-op rewrites.
func renderHooks(doc *hooksDoc) ([]byte, error) {
	top := make(map[string]json.RawMessage, len(doc.top)+2)
	maps.Copy(top, doc.top)
	if _, ok := top["version"]; !ok {
		top["version"] = json.RawMessage("1")
	}
	hooks, err := json.Marshal(doc.hooks)
	if err != nil {
		return nil, fmt.Errorf("encode cursor hooks: %w", err)
	}
	top["hooks"] = hooks

	out, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode cursor hooks file: %w", err)
	}
	return append(out, '\n'), nil
}

// writeHooks renders doc and atomically writes it to path (temp file +
// rename). It returns wrote=false and leaves the file untouched when the
// rendered bytes already match what is on disk.
func writeHooks(path string, doc *hooksDoc) (bool, error) {
	content, err := renderHooks(doc)
	if err != nil {
		return false, err
	}
	if existing, readErr := os.ReadFile(path); readErr == nil && bytes.Equal(existing, content) {
		return false, nil
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "hooks.json.tmp-*")
	if err != nil {
		return false, fmt.Errorf("temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		cleanup()
		return false, fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		cleanup()
		return false, fmt.Errorf("chmod temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return false, fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return false, fmt.Errorf("rename to %s: %w", path, err)
	}
	return true, nil
}

// unquote strips a single matching pair of surrounding single or double
// quotes from s. It is a best-effort helper for recognising our own
// shell-quoted binary path and does not interpret escapes.
func unquote(s string) string {
	if len(s) >= 2 {
		first := s[0]
		if (first == '\'' || first == '"') && s[len(s)-1] == first {
			return s[1 : len(s)-1]
		}
	}
	return s
}
