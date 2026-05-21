package claudecode

import (
	"context"
	"encoding/json"
	"errors"
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
	// marketplaceRepo is the source argument passed to
	// `claude plugin marketplace add`.
	marketplaceRepo = "grafana/sigil-sdk"
	// marketplaceAlias is the marketplace name declared in
	// .claude-plugin/marketplace.json.
	marketplaceAlias = "grafana-sigil"
	// PluginName is the plugin name declared in
	// plugins/claude-code/.claude-plugin/plugin.json.
	PluginName = "sigil-cc"
)

// Test seams.
var (
	lookPath   = exec.LookPath
	execFn     = syscall.Exec
	runInstall = defaultRunInstall
	getwd      = os.Getwd
)

// Launch resolves the `claude` binary on PATH, ensures the sigil-cc plugin
// is registered in Claude Code's plugin store (running `claude plugin
// marketplace add` + `claude plugin install` once if it is not), and then
// exec's claude with the supplied args. When localEnv is non-nil, the
// child receives local-mode SIGIL_ENDPOINT, SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT
// and placeholder auth values so it talks to the in-process receiver
// instead of Sigil Cloud.
func Launch(ctx context.Context, args []string, localEnv *local.LaunchEnv, _ io.Reader, _, stderr io.Writer, logger *log.Logger) error {
	bin, err := lookPath("claude")
	if err != nil {
		return fmt.Errorf("claude CLI not found on PATH: %w", err)
	}

	installed, err := pluginInstalled()
	if err != nil {
		// Treat a broken installed_plugins.json the same as missing: log the
		// parse failure for SIGIL_DEBUG, then let `claude plugin install`
		// run. claude's own CLI is the source of truth and will fail loudly
		// if something is genuinely wrong.
		logger.Printf("installed_plugins.json probe: %v", err)
		installed = false
	}
	if !installed {
		_, _ = fmt.Fprintf(stderr, "sigil: registering %s with claude\n", PluginName)
		if err := runInstall(ctx, bin, stderr); err != nil {
			// Don't block the user's claude session on a failed install
			// (offline machine, sandboxed CI, marketplace hiccup, etc.).
			// Log for SIGIL_DEBUG, point the user at the manual command,
			// and fall through to exec. claude still runs, just without
			// the sigil-cc plugin installed.
			logger.Printf("install %s: %v", PluginName, err)
			_, _ = fmt.Fprintf(stderr,
				"sigil: install of %s failed: %v\n"+
					"sigil: continuing without Sigil capture. To retry manually:\n"+
					"          claude plugin install %s@%s\n",
				PluginName, err, PluginName, marketplaceAlias)
		}
	}

	env := local.Environ(localEnv)
	argv := append([]string{bin}, args...)
	if err := execFn(bin, argv, env); err != nil {
		return fmt.Errorf("exec claude: %w", err)
	}
	return nil
}

func defaultRunInstall(ctx context.Context, bin string, w io.Writer) error {
	steps := [][]string{
		{"plugin", "marketplace", "add", marketplaceRepo},
		{"plugin", "install", PluginName + "@" + marketplaceAlias},
	}
	for _, argv := range steps {
		cmd := exec.CommandContext(ctx, bin, argv...)
		cmd.Stdout = w
		cmd.Stderr = w
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("claude %s: %w", strings.Join(argv, " "), err)
		}
	}
	return nil
}

// installedPluginsFile is the JSON shape Claude Code writes to
// $CLAUDE_CONFIG_DIR/plugins/installed_plugins.json. Top-level keys are
// metadata (`version`) plus a nested `plugins` map whose keys are
// `<plugin>@<marketplace>` strings and values are arrays of per-scope
// installation entries.
type installedPluginsFile struct {
	Plugins map[string]json.RawMessage `json:"plugins"`
}

// installedPluginEntry captures the subset of fields we need from each
// entry in the per-key install array. Claude Code tracks `scope`
// (`user`, `project`, or `local`) and, for the per-directory scopes,
// the absolute `projectPath` the install is bound to.
type installedPluginEntry struct {
	Scope       string `json:"scope"`
	ProjectPath string `json:"projectPath"`
}

// pluginInstalled reports whether the sigil-cc plugin is registered in
// Claude Code's plugin store *for the current working directory*.
//
// Claude Code records per-entry `scope` and `projectPath`: `user` scope
// is active in every session, while `project` and `local` only apply when
// the session's cwd matches the recorded `projectPath`. Treating any
// `sigil-cc@*` key as installed would let a per-directory install for an
// unrelated project suppress bootstrap here, leaving Sigil hooks inactive.
// Marketplace alias is intentionally ignored so a foreign alias is not
// reinstalled on top of.
func pluginInstalled() (bool, error) {
	path, err := installedPluginsPath()
	if err != nil {
		return false, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	var f installedPluginsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return false, fmt.Errorf("parse %s: %w", path, err)
	}
	cwd, err := getwd()
	if err != nil {
		return false, fmt.Errorf("resolve cwd: %w", err)
	}
	cwdClean := filepath.Clean(cwd)
	prefix := PluginName + "@"
	for key, raw := range f.Plugins {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		var entries []installedPluginEntry
		if err := json.Unmarshal(raw, &entries); err != nil {
			// Unexpected per-key shape: skip this key but keep scanning.
			// A user-scope entry under a different alias may still match.
			continue
		}
		for _, e := range entries {
			if e.Scope == "user" {
				return true, nil
			}
			if e.ProjectPath != "" && filepath.Clean(e.ProjectPath) == cwdClean {
				return true, nil
			}
		}
	}
	return false, nil
}

// installedPluginsPath returns the path to Claude Code's
// installed_plugins.json. It honours $CLAUDE_CONFIG_DIR (set by the user
// or tests) and falls back to ~/.claude.
func installedPluginsPath() (string, error) {
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir for claude config: %w", err)
		}
		dir = filepath.Join(home, ".claude")
	}
	return filepath.Join(dir, "plugins", "installed_plugins.json"), nil
}
