// Package opencode implements the opencode launcher adapter for the
// consolidated sigil binary. The dispatcher in cmd/sigil routes
// `sigil opencode [-- args...]` here.
//
// Unlike the hook adapters, this adapter owns the user's terminal: it
// bootstraps the @grafana/sigil-opencode plugin in opencode's global
// config on first use, refreshes it periodically, then replaces the
// current process with the opencode binary via execve so signals, exit
// codes, and TTY behaviour pass through cleanly. The opencode telemetry
// plugin itself runs in-process inside opencode through opencode's
// TypeScript plugin API; the launcher only handles install/refresh and
// shared env injection.
package opencode

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

	"github.com/grafana/sigil-sdk/plugins/sigil/internal/local"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/updatecheck"
)

const (
	// PluginSource is the npm spec passed to `opencode plugin <pkg>`.
	PluginSource = "@grafana/sigil-opencode"
	// PluginName is the package.json `name` of the plugin. Used to detect
	// versioned npm specs (e.g. `@grafana/sigil-opencode@0.6.0`) and to
	// stamp update-check state.
	PluginName = "@grafana/sigil-opencode"

	updateCheckTTL = 24 * time.Hour
)

// Test seams.
var (
	lookPath     = exec.LookPath
	execFn       = syscall.Exec
	runInstall   = defaultRunInstall
	runUpdate    = defaultRunUpdate
	configPathFn = defaultConfigPath
)

// Launch resolves the `opencode` binary on PATH, ensures the
// @grafana/sigil-opencode plugin is registered in opencode's global
// config (running `opencode plugin @grafana/sigil-opencode --global`
// once if it is not), and then exec's opencode with the supplied args.
// When localEnv is non-nil, the child receives local-mode SIGIL_ENDPOINT,
// SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT and placeholder auth values so it
// talks to the in-process receiver instead of Sigil Cloud.
func Launch(ctx context.Context, args []string, localEnv *local.LaunchEnv, _ io.Reader, _, stderr io.Writer, logger *log.Logger, sigilVersion string) error {
	bin, err := lookPath("opencode")
	if err != nil {
		return fmt.Errorf("opencode CLI not found on PATH: %w", err)
	}

	installed, err := pluginInstalled()
	if err != nil {
		// Surface the probe failure so the user can see why we're falling
		// through to install on a config file we couldn't read. Treat the
		// case like a missing plugin — opencode's installer will fail loudly
		// if the file is genuinely broken.
		logger.Printf("opencode config probe: %v", err)
		_, _ = fmt.Fprintf(stderr, "sigil: opencode config probe failed: %v\n", err)
		installed = false
	}
	if !installed {
		_, _ = fmt.Fprintf(stderr, "sigil: installing %s into opencode\n", PluginSource)
		if err := runInstall(ctx, bin, stderr); err != nil {
			// Don't block the user's opencode session on a failed install
			// (offline machine, npm rate-limit, sandboxed CI, etc.). Log
			// for SIGIL_DEBUG, point the user at the manual command, and
			// fall through to exec. opencode still runs, just without
			// Sigil capture.
			logger.Printf("install %s: %v", PluginSource, err)
			_, _ = fmt.Fprintf(stderr,
				"sigil: install of %s failed: %v\n"+
					"sigil: continuing without Sigil capture. To retry manually:\n"+
					"          opencode plugin %s --global\n",
				PluginSource, err, PluginSource)
		}
	} else if updatecheck.ShouldRun(PluginName, updateCheckTTL, sigilVersion) {
		_, _ = fmt.Fprintf(stderr, "sigil: refreshing %s in opencode\n", PluginSource)
		if err := runUpdate(ctx, bin, stderr); err != nil {
			logger.Printf("update %s: %v", PluginSource, err)
			_, _ = fmt.Fprintf(stderr,
				"sigil: update of %s failed: %v\n"+
					"sigil: continuing with the installed version. To retry manually:\n"+
					"          opencode plugin %s --global --force\n",
				PluginSource, err, PluginSource)
		}
		updatecheck.Record(PluginName, sigilVersion)
	}

	env := local.Environ(localEnv)
	argv := append([]string{bin}, args...)
	if err := execFn(bin, argv, env); err != nil {
		return fmt.Errorf("exec opencode: %w", err)
	}
	return nil
}

func defaultRunInstall(ctx context.Context, bin string, w io.Writer) error {
	cmd := exec.CommandContext(ctx, bin, "plugin", PluginSource, "--global")
	cmd.Stdout = w
	cmd.Stderr = w
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("opencode plugin %s --global: %w", PluginSource, err)
	}
	return nil
}

func defaultRunUpdate(ctx context.Context, bin string, w io.Writer) error {
	cmd := exec.CommandContext(ctx, bin, "plugin", PluginSource, "--global", "--force")
	cmd.Stdout = w
	cmd.Stderr = w
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("opencode plugin %s --global --force: %w", PluginSource, err)
	}
	return nil
}

// opencodeConfig is the subset of opencode's global config this launcher
// inspects. The vendored @opencode-ai/plugin types declare
// `plugin?: Array<string | [string, PluginOptions]>` — strings name the
// plugin module, two-element arrays carry plugin-specific options.
type opencodeConfig struct {
	Plugin []json.RawMessage `json:"plugin"`
}

// pluginInstalled reports whether the @grafana/sigil-opencode plugin is
// already registered in opencode's global config. The config lives at
// $XDG_CONFIG_HOME/opencode/opencode.json (default
// $HOME/.config/opencode/opencode.json). A missing file means opencode
// has never been configured with any plugins — treat as not installed.
func pluginInstalled() (bool, error) {
	path, err := configPathFn()
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
	var c opencodeConfig
	if err := json.Unmarshal(data, &c); err != nil {
		return false, fmt.Errorf("parse %s: %w", path, err)
	}
	for _, raw := range c.Plugin {
		var asString string
		if err := json.Unmarshal(raw, &asString); err == nil {
			if sourceMatchesPlugin(asString) {
				return true, nil
			}
			continue
		}
		// Tuple form: ["@grafana/sigil-opencode", {...}]
		var asTuple []json.RawMessage
		if err := json.Unmarshal(raw, &asTuple); err != nil {
			continue
		}
		if len(asTuple) == 0 {
			continue
		}
		var name string
		if err := json.Unmarshal(asTuple[0], &name); err != nil {
			continue
		}
		if sourceMatchesPlugin(name) {
			return true, nil
		}
	}
	return false, nil
}

// sourceMatchesPlugin returns true when a plugin entry identifies the
// @grafana/sigil-opencode package, accounting for optional `@<version>`
// suffixes (e.g. `@grafana/sigil-opencode@0.6.0`,
// `@grafana/sigil-opencode@next`).
func sourceMatchesPlugin(source string) bool {
	if source == "" {
		return false
	}
	return stripNpmVersion(source) == PluginName
}

// stripNpmVersion returns the package name portion of an npm spec,
// stripping the trailing `@<version>` segment if present. Scoped
// packages start with `@scope/...`; the leading `@` (index 0) is part of
// the name, not a version separator, so we look for the LAST `@` after
// index 0.
func stripNpmVersion(spec string) string {
	at := strings.LastIndex(spec, "@")
	if at <= 0 {
		return spec
	}
	return spec[:at]
}

// defaultConfigPath returns the path to opencode's global config file,
// honouring $XDG_CONFIG_HOME (default $HOME/.config). Errors resolving
// the user's home directory are surfaced so callers don't probe a path
// silently rooted at CWD.
func defaultConfigPath() (string, error) {
	if dir := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(dir) {
		return filepath.Join(dir, "opencode", "opencode.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir for opencode config: %w", err)
	}
	return filepath.Join(home, ".config", "opencode", "opencode.json"), nil
}
