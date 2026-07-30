// Package opencode implements the opencode launcher adapter for the
// consolidated agento11y binary. The dispatcher in cmd/agento11y routes
// `sigil opencode [-- args...]` here.
//
// Unlike the hook adapters, this adapter owns the user's terminal: it
// bootstraps the @grafana/agento11y-opencode plugin in opencode's global
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
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/grafana/agento11y/plugins/agento11y/internal/launcher"
	"github.com/grafana/agento11y/plugins/agento11y/internal/local"
	"github.com/tailscale/hujson"
)

const (
	// PluginSource is the npm spec passed to `opencode plugin <pkg>`.
	PluginSource = "@grafana/agento11y-opencode"
	// PluginName is the package.json `name` of the plugin. Used to detect
	// versioned npm specs (e.g. `@grafana/agento11y-opencode@0.6.0`) in the
	// config probe.
	PluginName = "@grafana/agento11y-opencode"
	// legacyPluginName is the pre-rename package name. Existing configs may
	// still reference it; treating it as installed avoids registering the
	// plugin twice under both names.
	legacyPluginName = "@grafana/sigil-opencode"

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
// @grafana/agento11y-opencode plugin is registered in opencode's global
// config (running `opencode plugin @grafana/agento11y-opencode --global`
// once if it is not), and then exec's opencode with the supplied args.
// When localEnv is non-nil, the child receives local-mode SIGIL_ENDPOINT,
// SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT and placeholder auth values so it
// talks to the in-process receiver instead of Grafana Cloud.
func Launch(ctx context.Context, args []string, localEnv *local.LaunchEnv, _ io.Reader, _, stderr io.Writer, logger *log.Logger, binaryVersion string) error {
	// Rewrite a pre-rename @grafana/sigil-opencode config entry to the new
	// package name before probing: the old package is frozen on npm, so a
	// legacy entry would otherwise stay pinned to the last pre-rename release
	// forever. Best-effort — a failure falls through to the legacy
	// refresh-skip below.
	migrateLegacyConfig(stderr, logger)

	// The periodic refresh installs PluginSource. When the config still
	// references the legacy package name (because the migration above could
	// not rewrite it), that would register the plugin a second time under the
	// new name, so skip the refresh for legacy installs and leave the existing
	// entry alone.
	update := runUpdate
	if src, found, err := installedPluginSource(); err == nil && found && stripNpmVersion(src) == legacyPluginName {
		update = nil
	}
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
		RegisterMessage: fmt.Sprintf("agento11y: installing %s into opencode\n", PluginSource),
		Install:         runInstall,
		InstallRecoveryHint: func(w io.Writer) {
			fmt.Fprintf(w, "          opencode plugin %s --global\n", PluginSource)
		},
		Update: update,
		UpdateRecoveryHint: func(w io.Writer) {
			fmt.Fprintf(w, "          opencode plugin %s --global --force\n", PluginSource)
		},
		UpdateTTL:     updateCheckTTL,
		BinaryVersion: binaryVersion,
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

// migrateLegacyConfig rewrites legacy @grafana/sigil-opencode entries in
// opencode's global config to the renamed @grafana/agento11y-opencode
// package. The old package is frozen on npm (releases continue only under
// the new name), so a legacy entry stays pinned to the last pre-rename
// release forever. OpenCode installs the npm plugins listed in its config at
// startup, so rewriting the entry is enough — no install command needed.
// Version pins are dropped deliberately: pre-rename versions do not exist
// under the new package name.
//
// The rewrite goes through hujson so comments, formatting, and tuple options
// survive, and the file is replaced atomically (temp file + rename) so a
// failure never leaves a half-written config. Best-effort: failures are
// logged and never block the launch — patch/write failures also print a
// manual-recovery hint on stderr, and the caller's legacy refresh-skip keeps
// the frozen install working.
func migrateLegacyConfig(stderr io.Writer, logger *log.Logger) {
	path, data, err := readConfigFile()
	if err != nil {
		logger.Printf("opencode legacy migration: %v", err)
		return
	}
	if data == nil {
		return
	}
	v, err := hujson.Parse(data)
	if err != nil {
		logger.Printf("opencode legacy migration: parse %s: %v", path, err)
		return
	}
	ops, err := legacyPluginOps(v)
	if err != nil {
		logger.Printf("opencode legacy migration: scan %s: %v", path, err)
		return
	}
	if ops == nil {
		return
	}
	fmt.Fprintf(stderr, "agento11y: migrating %s to %s in %s\n", legacyPluginName, PluginName, path)
	if err := v.Patch(ops); err != nil {
		logger.Printf("opencode legacy migration: patch %s: %v", path, err)
		printMigrationRecoveryHint(stderr, err, path)
		return
	}
	if err := writeConfigAtomic(path, v.Pack()); err != nil {
		logger.Printf("opencode legacy migration: write %s: %v", path, err)
		printMigrationRecoveryHint(stderr, err, path)
		return
	}
}

// printMigrationRecoveryHint tells the user how to finish the rename by hand
// when the automatic rewrite failed, mirroring Bootstrap's recovery wording.
func printMigrationRecoveryHint(w io.Writer, err error, path string) {
	fmt.Fprintf(w,
		"agento11y: migration of %s failed: %v\n"+
			"agento11y: continuing with the installed version. To migrate manually, edit\n"+
			"          %s and replace %s with %s.\n",
		legacyPluginName, err, path, legacyPluginName, PluginName)
}

// legacyPluginOps builds the RFC 6902 operations rewriting legacy
// @grafana/sigil-opencode entries in the parsed config: the first legacy
// entry is replaced with the bare renamed package (a tuple entry keeps its
// options — only element 0 is replaced), any further legacy entries are
// removed, and when the new name is already present every legacy entry is
// removed instead. Matching is version-insensitive. Returns nil when the
// config has no legacy entry.
func legacyPluginOps(v hujson.Value) ([]byte, error) {
	std := v.Clone()
	std.Standardize()
	var c opencodeConfig
	if err := json.Unmarshal(std.Pack(), &c); err != nil {
		return nil, err
	}
	type legacyEntry struct {
		index int
		tuple bool
	}
	var legacy []legacyEntry
	hasNew := false
	for i, raw := range c.Plugin {
		name, isTuple, ok := pluginEntryName(raw)
		if !ok {
			continue
		}
		switch stripNpmVersion(name) {
		case legacyPluginName:
			legacy = append(legacy, legacyEntry{index: i, tuple: isTuple})
		case PluginName:
			hasNew = true
		}
	}
	if len(legacy) == 0 {
		return nil, nil
	}
	type patchOp struct {
		Op    string `json:"op"`
		Path  string `json:"path"`
		Value string `json:"value,omitempty"`
	}
	var ops []patchOp
	// Higher indices first: a removal shifts every entry after it, so
	// operating back-to-front keeps every remaining path valid.
	for i, e := range slices.Backward(legacy) {
		if hasNew || i > 0 {
			ops = append(ops, patchOp{Op: "remove", Path: fmt.Sprintf("/plugin/%d", e.index)})
			continue
		}
		op := patchOp{Op: "replace", Path: fmt.Sprintf("/plugin/%d", e.index), Value: PluginName}
		if e.tuple {
			op.Path += "/0"
		}
		ops = append(ops, op)
	}
	return json.Marshal(ops)
}

// writeConfigAtomic replaces path with content through a temp file in the
// same directory plus rename, so a crash never leaves a half-written config.
// The original file's permission bits are preserved when they can be read.
func writeConfigAtomic(path string, content []byte) error {
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("chmod temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return fmt.Errorf("rename to %s: %w", path, err)
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

// pluginInstalled reports whether the @grafana/agento11y-opencode plugin is
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

// Status reports whether the @grafana/agento11y-opencode plugin is registered
// in opencode's global config. It reuses the read-only config probe and never
// installs or updates — `agento11y doctor` relies on this. When the registered
// spec pins a version (`@grafana/agento11y-opencode@1.2.3`) that version is
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
// entry source string that matches @grafana/agento11y-opencode (or its legacy
// @grafana/sigil-opencode name), if any. The config
// lives in $XDG_CONFIG_HOME/opencode (default $HOME/.config/opencode) as either
// opencode.json or opencode.jsonc; a missing file means no plugins are
// configured. The file is parsed as JSONC because opencode's docs allow
// comments and trailing commas.
func installedPluginSource() (source string, found bool, err error) {
	path, data, err := readConfigFile()
	if err != nil {
		return "", false, err
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
		name, _, ok := pluginEntryName(raw)
		if !ok {
			continue
		}
		if sourceMatchesPlugin(name) {
			return name, true, nil
		}
	}
	return "", false, nil
}

// pluginEntryName decodes one plugin-array entry, which is either a plain
// string naming the plugin module or a [name, options] tuple. Returns the
// entry's name (version pin included, if any), whether the entry was a
// tuple, and whether decoding succeeded.
func pluginEntryName(raw json.RawMessage) (name string, tuple, ok bool) {
	if err := json.Unmarshal(raw, &name); err == nil {
		return name, false, true
	}
	var asTuple []json.RawMessage
	if err := json.Unmarshal(raw, &asTuple); err != nil || len(asTuple) == 0 {
		return "", false, false
	}
	if err := json.Unmarshal(asTuple[0], &name); err != nil {
		return "", false, false
	}
	return name, true, true
}

// readConfigFile locates and reads opencode's global config, probing the
// recognised basenames in precedence order. A missing file returns
// ("", nil, nil) — opencode has never been configured.
func readConfigFile() (path string, data []byte, err error) {
	dir, err := configDirFn()
	if err != nil {
		return "", nil, err
	}
	for _, name := range configFileNames {
		candidate := filepath.Join(dir, name)
		b, err := os.ReadFile(candidate)
		if err == nil {
			return candidate, b, nil
		}
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		return "", nil, fmt.Errorf("read %s: %w", candidate, err)
	}
	return "", nil, nil
}

// versionFromNpmSpec returns the pinned version of a scoped npm spec, e.g.
// "1.2.3" from "@grafana/agento11y-opencode@1.2.3". The leading `@` of a scoped
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
// @grafana/agento11y-opencode package (or its legacy @grafana/sigil-opencode
// name), accounting for optional `@<version>` suffixes (e.g.
// `@grafana/agento11y-opencode@0.6.0`, `@grafana/agento11y-opencode@next`).
func sourceMatchesPlugin(source string) bool {
	if source == "" {
		return false
	}
	name := stripNpmVersion(source)
	return name == PluginName || name == legacyPluginName
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
