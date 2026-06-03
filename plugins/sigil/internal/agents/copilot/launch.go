package copilot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/grafana/sigil-sdk/plugins/sigil/internal/local"
)

const (
	// PluginName is the plugin name declared in plugins/copilot/plugin.json.
	// The launcher no longer registers the plugin (it would double-fire
	// against the shared user hooks file), but it still probes for and
	// removes a plugin left over from an older sigil version.
	PluginName = "sigil-copilot"

	// userHooksFileName is the file the launcher writes into the user-level
	// Copilot hooks directory. This single file drives capture for both
	// Copilot Chat in VS Code and the copilot CLI. A dedicated, sigil-owned
	// filename lets us overwrite it without touching hand-authored hooks.
	userHooksFileName = "sigil.json"
)

// Test seams.
var (
	lookPath     = exec.LookPath
	execFn       = syscall.Exec
	runUninstall = defaultRunUninstall
	pluginList   = defaultPluginList
)

// Launch installs the shared user-level Copilot hooks file (read by both
// Copilot Chat in VS Code and the copilot CLI), resolves the `copilot` binary
// on PATH, removes any stale sigil-copilot plugin left by an older sigil
// version, and exec's copilot with the supplied args. When localEnv is
// non-nil, the child receives local-mode SIGIL_ENDPOINT,
// SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT and placeholder auth values so it talks
// to the in-process receiver instead of Sigil Cloud.
//
// The launcher deliberately does NOT register the copilot plugin: the CLI
// loads hooks from the plugin store AND ~/.copilot/hooks and runs both, so a
// plugin alongside the shared file would fire every hook (and export every
// turn) twice. The single shared file covers both surfaces; the hook
// dispatcher infers vscode vs copilot-cli at runtime.
func Launch(_ context.Context, args []string, localEnv *local.LaunchEnv, _ io.Reader, _, stderr io.Writer, logger *log.Logger, _ string) error {
	// Always ensure the shared hooks file exists, even when copilot is not
	// on PATH: Copilot Chat in VS Code reads ~/.copilot/hooks regardless of
	// whether the CLI is installed.
	installUserHooks(stderr, logger)

	bin, err := lookPath("copilot")
	if err != nil {
		return fmt.Errorf("copilot CLI not found on PATH: %w", err)
	}

	// Remove a plugin registered by an older sigil version so the CLI does
	// not double-fire (plugin + shared user file). Best-effort: a probe or
	// uninstall failure must not block the user's copilot session.
	removeStalePlugin(bin, stderr, logger)

	env := local.Environ(localEnv)
	argv := append([]string{bin}, args...)
	if err := execFn(bin, argv, env); err != nil {
		return fmt.Errorf("exec copilot: %w", err)
	}
	return nil
}

// installUserHooks writes the shared user-level Copilot hooks file and reports
// the outcome. It never returns an error: failing to install the hooks must
// not block the rest of the launch flow.
func installUserHooks(stderr io.Writer, logger *log.Logger) {
	path, wrote, err := writeUserHooks()
	if err != nil {
		logger.Printf("write user-level copilot hooks: %v", err)
		return
	}
	if wrote {
		_, _ = fmt.Fprintf(stderr,
			"sigil: installed Copilot hooks at %s (used by Copilot in VS Code and by the copilot CLI)\n",
			path)
	}
}

// removeStalePlugin uninstalls the sigil-copilot plugin if a previous sigil
// version registered it. It is best-effort: probe and uninstall failures are
// logged for SIGIL_DEBUG but never block the launch.
func removeStalePlugin(bin string, stderr io.Writer, logger *log.Logger) {
	installed, err := pluginInstalled(context.Background(), bin)
	if err != nil {
		logger.Printf("copilot plugin list probe: %v", err)
		return
	}
	if !installed {
		return
	}
	_, _ = fmt.Fprintf(stderr,
		"sigil: removing the legacy %s plugin (capture now runs from ~/.copilot/hooks)\n",
		PluginName)
	if err := runUninstall(context.Background(), bin, stderr); err != nil {
		logger.Printf("uninstall %s: %v", PluginName, err)
		_, _ = fmt.Fprintf(stderr,
			"sigil: could not remove the %s plugin: %v\n"+
				"sigil: to avoid duplicate capture, remove it manually:\n"+
				"          copilot plugin uninstall %s\n",
			PluginName, err, PluginName)
	}
}

// copilotHooksDir resolves the user-level Copilot hooks directory. It honors
// COPILOT_HOME (which, per the Copilot config-dir reference, replaces the
// entire ~/.copilot path) and otherwise falls back to ~/.copilot/hooks.
func copilotHooksDir() (string, error) {
	if home := strings.TrimSpace(os.Getenv("COPILOT_HOME")); home != "" {
		return filepath.Join(home, "hooks"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir for copilot hooks: %w", err)
	}
	return filepath.Join(home, ".copilot", "hooks"), nil
}

// writeUserHooks renders the Sigil hook config and writes it to
// <copilot-hooks-dir>/sigil.json. The write is atomic (temp file + rename)
// and idempotent: when the on-disk content already matches, it is left
// untouched and wrote is false. It returns the target path so callers can
// report where the hooks landed.
func writeUserHooks() (path string, wrote bool, err error) {
	dir, err := copilotHooksDir()
	if err != nil {
		return "", false, err
	}
	path = filepath.Join(dir, userHooksFileName)
	content, err := renderUserHooks()
	if err != nil {
		return "", false, err
	}
	if existing, readErr := os.ReadFile(path); readErr == nil && bytes.Equal(existing, content) {
		return path, false, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", false, fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, userHooksFileName+".tmp-*")
	if err != nil {
		return "", false, fmt.Errorf("temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		cleanup()
		return "", false, fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		cleanup()
		return "", false, fmt.Errorf("chmod temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return "", false, fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return "", false, fmt.Errorf("rename to %s: %w", path, err)
	}
	return path, true, nil
}

// hookCommand mirrors a single command-hook entry in a Copilot hooks file.
type hookCommand struct {
	Type    string            `json:"type"`
	Command string            `json:"command"`
	Env     map[string]string `json:"env,omitempty"`
	Timeout int               `json:"timeout,omitempty"`
}

type hookGroup struct {
	Hooks []hookCommand `json:"hooks"`
}

type userHooksFile struct {
	Version int                    `json:"version"`
	Hooks   map[string][]hookGroup `json:"hooks"`
}

// renderUserHooks builds the shared user-level hooks JSON. It mirrors the
// events and command wiring shipped in plugins/copilot/hooks.json so both
// drive the exact same `sigil copilot hook` handler. Output is stable
// (encoding/json sorts map keys) so writeUserHooks can skip no-op rewrites.
func renderUserHooks() ([]byte, error) {
	events := []struct {
		name    string
		timeout int
	}{
		{"sessionStart", 10},
		{"sessionEnd", 10},
		{"userPromptSubmitted", 10},
		{"preToolUse", 10},
		{"postToolUse", 10},
		{"postToolUseFailure", 10},
		{"errorOccurred", 10},
		{"subagentStart", 10},
		{"subagentStop", 10},
		{"agentStop", 30},
	}
	f := userHooksFile{Version: 1, Hooks: make(map[string][]hookGroup, len(events))}
	for _, e := range events {
		f.Hooks[e.name] = []hookGroup{{
			Hooks: []hookCommand{{
				Type:    "command",
				Command: "sigil copilot hook",
				// This single file is read by BOTH Copilot Chat in VS Code and
				// the copilot CLI, so it deliberately does NOT pin
				// SIGIL_COPILOT_HOOK_SURFACE — the dispatcher infers the
				// surface (vscode vs copilot-cli) at runtime from the process
				// tree. Pinning it here would mislabel one of the two hosts.
				Env:     map[string]string{"SIGIL_COPILOT_HOOK_EVENT": e.name},
				Timeout: e.timeout,
			}},
		}}
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("render user-level copilot hooks: %w", err)
	}
	return append(b, '\n'), nil
}

func defaultRunUninstall(ctx context.Context, bin string, w io.Writer) error {
	cmd := exec.CommandContext(ctx, bin, "plugin", "uninstall", PluginName)
	cmd.Stdout = w
	cmd.Stderr = w
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("copilot plugin uninstall %s: %w", PluginName, err)
	}
	return nil
}

// defaultPluginList shells out to `copilot plugin list` and returns the raw
// output. Output is human-formatted but stable for direct installs: a
// `Installed plugins:` header followed by one `  • <plugin> (v<ver>)` line
// per plugin.
func defaultPluginList(ctx context.Context, bin string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, "plugin", "list")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := bytes.TrimSpace(stderr.Bytes()); len(msg) > 0 {
			return nil, fmt.Errorf("%w: %s", err, msg)
		}
		return nil, err
	}
	return out, nil
}

// pluginInstalled reports whether `sigil-copilot` is registered in copilot's
// plugin store. Disabled-state detection is deferred until we confirm
// copilot's `plugin list` output for a disabled plugin — for now, presence
// counts as installed.
func pluginInstalled(ctx context.Context, bin string) (bool, error) {
	out, err := pluginList(ctx, bin)
	if err != nil {
		return false, err
	}
	needle := []byte(PluginName)
	// Lines look like `  • sigil-copilot (v0.1.0)`. Strip leading whitespace
	// and the bullet glyph, then exact-match the first remaining token. This
	// rejects the `Installed plugins:` header, a bare bullet line, and
	// suffix collisions like `my-sigil-copilot`.
	for line := range bytes.SplitSeq(out, []byte{'\n'}) {
		trimmed := bytes.TrimLeft(bytes.TrimSpace(line), "•*-+ \t")
		fields := bytes.Fields(trimmed)
		if len(fields) > 0 && bytes.Equal(fields[0], needle) {
			return true, nil
		}
	}
	return false, nil
}
