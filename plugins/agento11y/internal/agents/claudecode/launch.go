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
	"time"

	"github.com/grafana/agento11y/plugins/agento11y/internal/launcher"
	"github.com/grafana/agento11y/plugins/agento11y/internal/local"
)

const (
	// marketplaceRepo is the source argument passed to
	// `claude plugin marketplace add`.
	marketplaceRepo = "grafana/agento11y"
	// marketplaceAlias is the marketplace name declared in
	// .claude-plugin/marketplace.json. Claude Code keys a marketplace by the
	// name its manifest declared when the user added it and never re-keys,
	// so registrations that predate the sigil rename answer only to
	// legacyMarketplaceAlias.
	marketplaceAlias       = "agento11y"
	legacyMarketplaceAlias = "grafana-sigil"
	// PluginName is the plugin name declared in
	// plugins/claude-code/.claude-plugin/plugin.json. legacyPluginName is the
	// pre-rename name: the marketplace's renames map migrates legacy installs
	// at session start (Claude Code >= 2.1.193), so the probe treats them as
	// installed instead of reinstalling on top.
	PluginName       = "agento11y-claude-code"
	legacyPluginName = "sigil-cc"

	updateCheckTTL = 24 * time.Hour
)

// Test seams.
var (
	lookPath   = exec.LookPath
	execFn     = syscall.Exec
	runInstall = defaultRunInstall
	runUpdate  = defaultRunUpdate
	runSteps   = launcher.RunSteps
	runFirst   = launcher.RunFirst
	getwd      = os.Getwd
)

// Launch resolves the `claude` binary on PATH, ensures the Claude Code plugin
// is registered in Claude Code's plugin store (running `claude plugin
// marketplace add` + `claude plugin install` once if it is not), and then
// exec's claude with the supplied args. When localEnv is non-nil, the
// child receives local-mode SIGIL_ENDPOINT, SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT
// and placeholder auth values so it talks to the in-process receiver
// instead of Grafana Cloud.
func Launch(ctx context.Context, args []string, localEnv *local.LaunchEnv, _ io.Reader, _, stderr io.Writer, logger *log.Logger, binaryVersion string) error {
	return launcher.Bootstrap(ctx, launcher.BootstrapSpec{
		BinName:     "claude",
		PluginLabel: PluginName,
		LookPath:    lookPath,
		ExecFn:      execFn,
		Args:        args,
		Env:         local.Environ(localEnv),
		Logger:      logger,
		Stderr:      stderr,
		// Treat a broken installed_plugins.json the same as missing:
		// claude's own CLI is the source of truth and will fail loudly if
		// something is genuinely wrong.
		Probe:       func(context.Context, string) (bool, error) { return pluginInstalled() },
		ProbeErrLog: "installed_plugins.json probe",
		Install:     runInstall,
		InstallRecoveryHint: func(w io.Writer) {
			fmt.Fprintf(w,
				"          claude plugin marketplace add %s\n"+
					"          claude plugin install %s@%s\n"+
					"          (marketplaces added before the rename keep the %s alias; there run\n"+
					"           claude plugin marketplace update %s\n"+
					"           claude plugin install %s@%s)\n",
				marketplaceRepo, PluginName, marketplaceAlias,
				legacyMarketplaceAlias,
				legacyMarketplaceAlias, PluginName, legacyMarketplaceAlias)
		},
		Update: runUpdate,
		// The hint mirrors what defaultRunUpdate just attempted: the installed
		// key's alias, not the current one, since pre-rename registrations
		// keep the sticky legacy alias and commands against the current alias
		// would fail for them.
		UpdateRecoveryHint: func(w io.Writer) {
			key := updateTargetKey()
			alias := key[strings.IndexByte(key, '@')+1:]
			fmt.Fprintf(w,
				"          claude plugin marketplace update %s\n"+
					"          claude plugin update %s\n",
				alias, key)
			if strings.HasPrefix(key, legacyPluginName+"@") {
				fmt.Fprintf(w,
					"          (if the update no longer finds %s, run: claude plugin install %s@%s)\n",
					key, PluginName, alias)
			}
		},
		UpdateTTL:     updateCheckTTL,
		BinaryVersion: binaryVersion,
	})
}

// Status reports whether the Claude Code plugin is installed for the current
// working directory. It reuses the read-only pluginInstalled probe and never
// installs, updates, or writes update-check state — `agento11y doctor` relies on
// this. installed_plugins.json carries no version, so version is always empty
// (best-effort).
func Status(_ context.Context) (installed bool, version string, err error) {
	installed, err = pluginInstalled()
	return installed, "", err
}

func defaultRunInstall(ctx context.Context, bin string, w io.Writer) error {
	if err := runSteps(ctx, bin, w, [][]string{{"plugin", "marketplace", "add", marketplaceRepo}}); err != nil {
		return err
	}
	// Adding is a no-op when the marketplace is already registered, and the
	// registration keeps whatever name its manifest declared at the time. So
	// the plugin may only be addressable via the legacy alias, and a
	// marketplace copy that predates the rename lists only the legacy plugin
	// name. Try the current names first, then fall back.
	_, err := runFirst(ctx, bin, w, [][]string{
		{"plugin", "install", PluginName + "@" + marketplaceAlias},
		{"plugin", "install", PluginName + "@" + legacyMarketplaceAlias},
		{"plugin", "install", legacyPluginName + "@" + legacyMarketplaceAlias},
	})
	return err
}

// updateTargetKey returns the `<plugin>@<marketplace>` key the update path
// targets. The installed key wins because Claude Code never re-keys a
// marketplace, so registrations that predate the rename answer only to the
// sticky legacy alias. The probe reported the plugin installed just before
// the update runs, so an empty or unreadable store here is exceptional —
// fall back to the current names rather than failing the refresh.
func updateTargetKey() string {
	key, err := installedPluginKey()
	if err != nil || key == "" {
		return PluginName + "@" + marketplaceAlias
	}
	return key
}

func defaultRunUpdate(ctx context.Context, bin string, w io.Writer) error {
	key := updateTargetKey()
	alias := key[strings.IndexByte(key, '@')+1:]
	if err := runSteps(ctx, bin, w, [][]string{{"plugin", "marketplace", "update", alias}}); err != nil {
		return err
	}
	if !strings.HasPrefix(key, legacyPluginName+"@") {
		return runSteps(ctx, bin, w, [][]string{{"plugin", "update", key}})
	}
	// Legacy install. While the local marketplace copy predates the rename it
	// still lists the legacy name and a plain update works. Once the refresh
	// above pulls a post-rename copy the legacy name is gone: update fails,
	// and installing the renamed plugin under the same (sticky) alias
	// migrates the entry. Claude Code's renames map rewrites the remaining
	// legacy settings keys at the next session start.
	_, err := runFirst(ctx, bin, w, [][]string{
		{"plugin", "update", key},
		{"plugin", "install", PluginName + "@" + alias},
	})
	return err
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

// pluginInstalled reports whether the Claude Code plugin is registered in
// Claude Code's plugin store *for the current working directory*.
func pluginInstalled() (bool, error) {
	key, err := installedPluginKey()
	return key != "", err
}

// installedPluginKey returns the `<plugin>@<marketplace>` key registering
// the plugin for the current working directory, preferring the current
// plugin name over the legacy pre-rename one; empty when not installed.
// Legacy keys count as installed because Claude Code itself migrates them to
// the current name at session start via the marketplace renames map —
// reinstalling on top would be redundant.
//
// Claude Code records per-entry `scope` and `projectPath`: `user` scope
// is active in every session, while `project` and `local` only apply when
// the session's cwd matches the recorded `projectPath`. Treating any
// matching key as installed would let a per-directory install for an
// unrelated project suppress bootstrap here, leaving agento11y hooks inactive.
// Marketplace alias is intentionally ignored so a foreign alias is not
// reinstalled on top of.
func installedPluginKey() (string, error) {
	path, err := installedPluginsPath()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	var f installedPluginsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return "", fmt.Errorf("parse %s: %w", path, err)
	}
	cwd, err := getwd()
	if err != nil {
		return "", fmt.Errorf("resolve cwd: %w", err)
	}
	cwdClean := filepath.Clean(cwd)
	var legacyKey string
	for key, raw := range f.Plugins {
		isCurrent := strings.HasPrefix(key, PluginName+"@")
		if !isCurrent && !strings.HasPrefix(key, legacyPluginName+"@") {
			continue
		}
		var entries []installedPluginEntry
		if err := json.Unmarshal(raw, &entries); err != nil {
			// Unexpected per-key shape: skip this key but keep scanning.
			// A user-scope entry under a different alias may still match.
			continue
		}
		for _, e := range entries {
			if e.Scope != "user" && (e.ProjectPath == "" || filepath.Clean(e.ProjectPath) != cwdClean) {
				continue
			}
			if isCurrent {
				return key, nil
			}
			legacyKey = key
			break
		}
	}
	return legacyKey, nil
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
