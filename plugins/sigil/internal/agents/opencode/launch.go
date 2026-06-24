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

	"github.com/grafana/sigil-sdk/plugins/sigil/internal/launcher"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/local"
	"github.com/tailscale/hujson"
)

const (
	// PluginSource is the npm spec passed to `opencode plugin <pkg>`.
	PluginSource = "@grafana/sigil-opencode"
	// PluginName is the package.json `name` of the plugin. Used to detect
	// versioned npm specs (e.g. `@grafana/sigil-opencode@0.6.0`) in the
	// config probe.
	PluginName = "@grafana/sigil-opencode"

	updateCheckTTL = 24 * time.Hour
)

// Test seams.
var (
	lookPath    = exec.LookPath
	execFn      = syscall.Exec
	runInstall  = defaultRunInstall
	runUpdate   = defaultRunUpdate
	configDirFn = defaultConfigDir
)

// configFileNames lists the basenames opencode recognises for its
// global config, in precedence order. The docs advertise both .json
// and .jsonc; users may pick either, so we probe both.
var configFileNames = []string{"opencode.json", "opencode.jsonc"}

// Launch resolves the `opencode` binary on PATH, ensures the
// @grafana/sigil-opencode plugin is registered in opencode's global
// config (running `opencode plugin @grafana/sigil-opencode --global`
// once if it is not), and then exec's opencode with the supplied args.
// When localEnv is non-nil, the child receives local-mode SIGIL_ENDPOINT,
// SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT and placeholder auth values so it
// talks to the in-process receiver instead of Sigil Cloud.
func Launch(ctx context.Context, args []string, localEnv *local.LaunchEnv, _ io.Reader, _, stderr io.Writer, logger *log.Logger, sigilVersion string) error {
	return launcher.Bootstrap(ctx, launcher.BootstrapSpec{
		BinName:     "opencode",
		PluginLabel: PluginSource,
		LookPath:    lookPath,
		ExecFn:      execFn,
		Args:        args,
		Env:         local.Environ(localEnv),
		Logger:      logger,
		Stderr:      stderr,
		// Surface a config-file probe failure on stderr too so the user can
		// see why we're falling through to install on a file we couldn't
		// read. Treat the case like a missing plugin — opencode's installer
		// will fail loudly if the file is genuinely broken.
		Probe:           func(context.Context, string) (bool, error) { return pluginInstalled() },
		ProbeErrLog:     "opencode config probe",
		ProbeErrEcho:    true,
		RegisterMessage: fmt.Sprintf("sigil: installing %s into opencode\n", PluginSource),
		Install:         runInstall,
		InstallRecoveryHint: func(w io.Writer) {
			fmt.Fprintf(w, "          opencode plugin %s --global\n", PluginSource)
		},
		Update: runUpdate,
		UpdateRecoveryHint: func(w io.Writer) {
			fmt.Fprintf(w, "          opencode plugin %s --global --force\n", PluginSource)
		},
		UpdateTTL:    updateCheckTTL,
		SigilVersion: sigilVersion,
	})
}

func defaultRunInstall(ctx context.Context, bin string, w io.Writer) error {
	return launcher.RunSteps(ctx, bin, w, [][]string{
		{"plugin", PluginSource, "--global"},
	})
}

func defaultRunUpdate(ctx context.Context, bin string, w io.Writer) error {
	return launcher.RunSteps(ctx, bin, w, [][]string{
		{"plugin", PluginSource, "--global", "--force"},
	})
}

// opencodeConfig is the subset of opencode's global config this launcher
// inspects. The vendored @opencode-ai/plugin types declare
// `plugin?: Array<string | [string, PluginOptions]>` — strings name the
// plugin module, two-element arrays carry plugin-specific options.
type opencodeConfig struct {
	Plugin []json.RawMessage `json:"plugin"`
}

// pluginInstalled reports whether the @grafana/sigil-opencode plugin is
// already registered in opencode's global config. The config lives in
// $XDG_CONFIG_HOME/opencode (default $HOME/.config/opencode) as either
// opencode.json or opencode.jsonc. A missing file means opencode has
// never been configured with any plugins — treat as not installed.
//
// The file contents are parsed as JSONC: opencode's docs explicitly
// support comments and trailing commas (the very first config example
// in the docs has a trailing comma), so a strict json.Unmarshal would
// reject perfectly valid configs and trap us in a reinstall loop.
func pluginInstalled() (bool, error) {
	_, found, err := installedPluginSource()
	return found, err
}

// Status reports whether the @grafana/sigil-opencode plugin is registered in
// opencode's global config. It reuses the read-only config probe and never
// installs or updates — `sigil doctor` relies on this. When the registered
// spec pins a version (`@grafana/sigil-opencode@1.2.3`) that version is
// reported; an unpinned spec yields an empty (unknown) version.
func Status(_ context.Context) (installed bool, version string, err error) {
	source, found, err := installedPluginSource()
	if err != nil {
		return false, "", err
	}
	if !found {
		return false, "", nil
	}
	return true, versionFromNpmSpec(source), nil
}

// installedPluginSource reads opencode's global config and returns the plugin
// entry source string that matches @grafana/sigil-opencode, if any. The config
// lives in $XDG_CONFIG_HOME/opencode (default $HOME/.config/opencode) as either
// opencode.json or opencode.jsonc; a missing file means no plugins are
// configured. The file is parsed as JSONC because opencode's docs allow
// comments and trailing commas.
func installedPluginSource() (source string, found bool, err error) {
	dir, err := configDirFn()
	if err != nil {
		return "", false, err
	}
	var (
		data []byte
		path string
	)
	for _, name := range configFileNames {
		candidate := filepath.Join(dir, name)
		b, err := os.ReadFile(candidate)
		if err == nil {
			data = b
			path = candidate
			break
		}
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		return "", false, fmt.Errorf("read %s: %w", candidate, err)
	}
	if data == nil {
		return "", false, nil
	}
	std, err := hujson.Standardize(data)
	if err != nil {
		return "", false, fmt.Errorf("parse %s: %w", path, err)
	}
	var c opencodeConfig
	if err := json.Unmarshal(std, &c); err != nil {
		return "", false, fmt.Errorf("parse %s: %w", path, err)
	}
	for _, raw := range c.Plugin {
		var asString string
		if err := json.Unmarshal(raw, &asString); err == nil {
			if sourceMatchesPlugin(asString) {
				return asString, true, nil
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
			return name, true, nil
		}
	}
	return "", false, nil
}

// versionFromNpmSpec returns the pinned version of a scoped npm spec, e.g.
// "1.2.3" from "@grafana/sigil-opencode@1.2.3". The leading `@` of a scoped
// package is part of the name, so only a later `@` separates the version.
// Returns "" for an unpinned spec.
func versionFromNpmSpec(spec string) string {
	at := strings.LastIndex(spec, "@")
	if at <= 0 {
		return ""
	}
	return spec[at+1:]
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

// defaultConfigDir returns the directory holding opencode's global
// config, honouring $XDG_CONFIG_HOME (default $HOME/.config). Errors
// resolving the user's home directory are surfaced so callers don't
// probe a path silently rooted at CWD.
func defaultConfigDir() (string, error) {
	if dir := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(dir) {
		return filepath.Join(dir, "opencode"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir for opencode config: %w", err)
	}
	return filepath.Join(home, ".config", "opencode"), nil
}
