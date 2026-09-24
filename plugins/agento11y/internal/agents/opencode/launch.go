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

	"github.com/grafana/agento11y/plugins/agento11y/internal/agentinstall"
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

// ErrCLINotFound means the OpenCode binary is not available on PATH for the
// current user. Callers can defer setup until the host is installed.
var ErrCLINotFound = errors.New("opencode CLI not found")

func init() {
	agentinstall.Register(agentinstall.Spec{
		Name:          "opencode",
		Install:       Install,
		IsMissingHost: func(err error) bool { return errors.Is(err, ErrCLINotFound) },
	})
}

// Test seams.
var (
	lookPath      = exec.LookPath
	execFn        = syscall.Exec
	runInstall    = defaultRunInstall
	runUpdate     = defaultRunUpdate
	runUpdateStep = func(ctx context.Context, bin string, w io.Writer, argv []string) error {
		return launcher.RunSteps(ctx, bin, w, [][]string{argv})
	}
	configDirFn = defaultConfigDir
	cacheDirFn  = defaultCacheDir
	writeConfig = writeConfigAtomic
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
	// Launch keeps legacy migration best-effort so an interactive OpenCode
	// session can still start with its existing plugin if a rewrite fails.
	_ = migrateLegacyConfig(stderr, logger)

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

// Install registers the OpenCode plugin without starting OpenCode or prompting
// for Agent Observability credentials. The returned value is true only when
// this invocation registered the plugin.
func Install(ctx context.Context, stdout io.Writer, logger *log.Logger) (bool, error) {
	// A legacy config must be rewritten before it can count as installed. If
	// the rewrite fails, returning an error keeps fleet reconciliation retrying
	// instead of silently leaving OpenCode pinned to the frozen package.
	if err := migrateLegacyConfig(stdout, logger); err != nil {
		return false, fmt.Errorf("migrate legacy OpenCode plugin configuration: %w", err)
	}
	installed, probeErr := pluginInstalled()
	if probeErr == nil && installed {
		return false, nil
	}

	bin, err := lookPath("opencode")
	if err != nil {
		return false, fmt.Errorf("%w; install OpenCode or run this in the developer's user context", ErrCLINotFound)
	}
	if err := runInstall(ctx, bin, stdout); err != nil {
		return false, err
	}
	return true, nil
}

func defaultRunInstall(ctx context.Context, bin string, w io.Writer) error {
	return launcher.RunSteps(ctx, bin, w, [][]string{
		{"plugin", PluginSource, "--global"},
	})
}

func defaultRunUpdate(ctx context.Context, bin string, w io.Writer) (err error) {
	backup, err := backupCachedPlugin()
	if err != nil {
		return err
	}
	if backup != nil {
		defer func() {
			if err != nil {
				err = errors.Join(err, backup.restore())
				return
			}
			err = backup.discard()
		}()
	}

	// OpenCode's --force changes config but reuses an existing package directory.
	// --pure prevents the stale plugin from loading while the updater starts.
	err = runUpdateStep(ctx, bin, w, []string{"plugin", PluginSource, "--global", "--force", "--pure"})
	return err
}

type packageCacheBackup struct {
	original string
	root     string
	cached   string
}

func backupCachedPlugin() (*packageCacheBackup, error) {
	original, err := pluginCacheDir()
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(original); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("inspect cached OpenCode plugin %s: %w", original, err)
	}

	root, err := os.MkdirTemp(filepath.Dir(original), ".agento11y-opencode-backup-*")
	if err != nil {
		return nil, fmt.Errorf("create OpenCode plugin cache backup: %w", err)
	}
	cached := filepath.Join(root, filepath.Base(original))
	if err := os.Rename(original, cached); err != nil {
		_ = os.RemoveAll(root)
		return nil, fmt.Errorf("move cached OpenCode plugin %s: %w", original, err)
	}
	return &packageCacheBackup{original: original, root: root, cached: cached}, nil
}

func (b *packageCacheBackup) restore() error {
	if err := os.RemoveAll(b.original); err != nil {
		return fmt.Errorf("remove partial OpenCode plugin update %s: %w", b.original, err)
	}
	if err := os.Rename(b.cached, b.original); err != nil {
		return fmt.Errorf("restore cached OpenCode plugin %s: %w", b.original, err)
	}
	if err := os.RemoveAll(b.root); err != nil {
		return fmt.Errorf("remove OpenCode plugin cache backup %s: %w", b.root, err)
	}
	return nil
}

func (b *packageCacheBackup) discard() error {
	if err := os.RemoveAll(b.root); err != nil {
		return fmt.Errorf("remove stale OpenCode plugin cache backup %s: %w", b.root, err)
	}
	return nil
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
// failure never leaves a half-written config. Failures are logged and never
// block Launch — patch/write failures also print a manual-recovery hint on
// stderr, and Launch's legacy refresh-skip keeps the frozen install working.
// Install returns such a failure so managed reconciliation does not report a
// legacy plugin as converged.
func migrateLegacyConfig(stderr io.Writer, logger *log.Logger) error {
	path, data, err := readConfigFile()
	if err != nil {
		logger.Printf("opencode legacy migration: %v", err)
		return err
	}
	if data == nil {
		return nil
	}
	v, err := hujson.Parse(data)
	if err != nil {
		err = fmt.Errorf("parse %s: %w", path, err)
		logger.Printf("opencode legacy migration: %v", err)
		return err
	}
	ops, err := legacyPluginOps(v)
	if err != nil {
		err = fmt.Errorf("scan %s: %w", path, err)
		logger.Printf("opencode legacy migration: %v", err)
		return err
	}
	if ops == nil {
		return nil
	}
	fmt.Fprintf(stderr, "agento11y: migrating %s to %s in %s\n", legacyPluginName, PluginName, path)
	if err := v.Patch(ops); err != nil {
		err = fmt.Errorf("patch %s: %w", path, err)
		logger.Printf("opencode legacy migration: %v", err)
		printMigrationRecoveryHint(stderr, err, path)
		return err
	}
	if err := writeConfig(path, v.Pack()); err != nil {
		err = fmt.Errorf("write %s: %w", path, err)
		logger.Printf("opencode legacy migration: %v", err)
		printMigrationRecoveryHint(stderr, err, path)
		return err
	}
	return nil
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
// in opencode's global config, and which version is installed. It reuses the
// read-only config probe and never installs or updates — `agento11y doctor`
// relies on this. The version comes from the package opencode installed into
// its cache, because the config entry is usually unpinned; a pinned spec
// (`@grafana/agento11y-opencode@1.2.3`) is the fallback. The cache belongs to
// opencode, so an absent or unreadable tree means the version is unknown, not
// an error.
func Status(_ context.Context) (installed bool, version string, err error) {
	source, found, err := installedPluginSource()
	if err != nil {
		return false, "", err
	}
	if !found {
		return false, "", nil
	}
	if version := cachedVersion(source); version != "" {
		return true, version, nil
	}
	return true, versionFromNpmSpec(source), nil
}

// cachedVersion returns the version of the plugin opencode installed for a
// config entry, or "" when it can't be read. opencode installs each plugin
// spec into its own cache directory named after the spec:
// <cache>/packages/<spec>/node_modules/<name>. Builds before opencode's cache
// migration installed straight under <cache>/node_modules/<name>, so that
// layout is probed second.
func cachedVersion(source string) string {
	cache, err := cacheDirFn()
	if err != nil {
		return ""
	}
	name := stripNpmVersion(source)
	// A scoped name and a spec both contain a `/`; convert it to the OS
	// separator so `@scope/pkg` is two directories on Windows too.
	pkg := filepath.FromSlash(name)
	candidates := []string{
		filepath.Join(cache, "packages", filepath.FromSlash(cachedPackageSpec(source)), "node_modules", pkg),
		filepath.Join(cache, "node_modules", pkg),
	}
	for _, dir := range candidates {
		if version := packageVersion(dir); version != "" {
			return version
		}
	}
	return ""
}

// cachedPackageSpec returns the spec opencode installs a config entry as, which
// is also the name of the cache directory it installs into. opencode rewrites a
// bare package name to `<name>@latest` and passes anything else through, so a
// pinned spec and a dist tag keep their own directories.
func cachedPackageSpec(source string) string {
	if versionFromNpmSpec(source) == "" {
		return source + "@latest"
	}
	return source
}

func pluginCacheDir() (string, error) {
	cache, err := cacheDirFn()
	if err != nil {
		return "", err
	}
	return filepath.Join(cache, "packages", filepath.FromSlash(cachedPackageSpec(PluginSource))), nil
}

// packageVersion reads the `version` a package directory's package.json
// declares. A missing, unreadable, or malformed file means unknown — the file
// belongs to opencode, so doctor never turns it into an error.
func packageVersion(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return ""
	}
	var pkg struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return ""
	}
	return pkg.Version
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

// defaultCacheDir returns the directory opencode installs plugin packages
// into, honouring $XDG_CACHE_HOME (default $HOME/.cache). This is the cache
// directory, not the config one: opencode keeps its config in
// $XDG_CONFIG_HOME/opencode and the installed packages here.
func defaultCacheDir() (string, error) {
	if dir := os.Getenv("XDG_CACHE_HOME"); filepath.IsAbs(dir) {
		return filepath.Join(dir, "opencode"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir for opencode cache: %w", err)
	}
	return filepath.Join(home, ".cache", "opencode"), nil
}
