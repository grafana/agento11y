// Package pi implements the pi launcher adapter for the consolidated sigil
// binary. The dispatcher in cmd/agento11y routes `sigil pi [-- args...]` here.
//
// Unlike the hook adapters, this adapter owns the user's terminal: it
// bootstraps the @grafana/agento11y-pi extension in pi's settings file on first
// use, then replaces the current process with the pi binary via execve so
// signals, exit codes, and TTY behaviour pass through cleanly.
package pi

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

	"github.com/grafana/agento11y/plugins/agento11y/internal/launcher"
	"github.com/grafana/agento11y/plugins/agento11y/internal/local"
)

const (
	// PluginSource is the canonical npm spec used for fresh installs.
	PluginSource = "npm:@grafana/agento11y-pi"
	// pluginName is the package.json `name` of the extension. Used to detect
	// versioned npm specs (e.g. `npm:@grafana/agento11y-pi@0.1.1`) and
	// local-path installs that point at a checkout of this package.
	pluginName = "@grafana/agento11y-pi"
	// legacyPluginName is the pre-rename package name. Existing settings may
	// still reference it; treating it as installed avoids registering the
	// extension twice under both names.
	legacyPluginName = "@grafana/sigil-pi"
	// legacyPluginSource is the npm spec of the pre-rename package, used for
	// the `pi remove` migration step and the manual recovery hints.
	legacyPluginSource = npmPrefix + legacyPluginName
	npmPrefix          = "npm:"
	// projectConfigDirName is pi's default project config directory. Pi
	// allows overriding this via package.json `piConfig.configDir`, but the
	// default `.pi` covers every shipped install.
	projectConfigDirName = ".pi"
)

// Test seams.
var (
	lookPath   = exec.LookPath
	execFn     = syscall.Exec
	runInstall = defaultRunInstall
	runPi      = defaultRunPi
)

// Launch resolves the `pi` binary on PATH, ensures the @grafana/agento11y-pi
// extension is registered in the user's pi settings (running `pi install`
// once if it is not), and then exec's pi with the supplied args. When
// localEnv is non-nil, the child receives local-mode SIGIL_ENDPOINT,
// SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT and placeholder auth values so it
// talks to the in-process receiver instead of Grafana Cloud. The trailing
// string arg is the sigil build version; the pi adapter does not run
// auto-update checks (pi's own installer handles upgrades) so it is
// accepted to satisfy the launcher signature and ignored.
func Launch(ctx context.Context, args []string, localEnv *local.LaunchEnv, _ io.Reader, _, stderr io.Writer, logger *log.Logger, _ string) error {
	// Rewrite a pre-rename @grafana/sigil-pi npm install to the new package
	// name before probing: the old package is frozen on npm, so a legacy
	// entry would otherwise stay pinned to the last pre-rename release
	// forever. Best-effort — a failure never blocks the launch; a legacy
	// entry that survives keeps working but never updates.
	migrateLegacyInstall(ctx, stderr, logger)
	return launcher.Bootstrap(ctx, launcher.BootstrapSpec{
		BinName:     "pi",
		PluginLabel: PluginSource,
		LookPath:    lookPath,
		ExecFn:      execFn,
		Args:        args,
		Env:         local.Environ(localEnv),
		Logger:      logger,
		Stderr:      stderr,
		// Surface a settings-file probe failure on stderr too so the user
		// can see why we're falling through to install on a file we couldn't
		// read. Treat the case like a missing extension — pi's installer
		// will fail loudly if the file is genuinely broken.
		Probe:           func(context.Context, string) (bool, error) { return pluginInstalled() },
		ProbeErrLog:     "pi settings probe",
		ProbeErrEcho:    true,
		RegisterMessage: fmt.Sprintf("agento11y: installing %s into pi\n", PluginSource),
		Install:         runInstall,
		InstallRecoveryHint: func(w io.Writer) {
			fmt.Fprintf(w, "          pi install %s\n", PluginSource)
		},
		// No Update: pi's own installer handles upgrades.
	})
}

func defaultRunInstall(ctx context.Context, bin string, w io.Writer) error {
	return defaultRunPi(ctx, bin, w, "install", PluginSource)
}

// defaultRunPi runs `bin args...` writing the child's output to w. The
// migration uses it for the `pi remove` / `pi install` steps.
func defaultRunPi(ctx context.Context, bin string, w io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout = w
	cmd.Stderr = w
	return cmd.Run()
}

// migrateLegacyInstall rewrites a legacy npm install of @grafana/sigil-pi to
// the renamed @grafana/agento11y-pi package by running `pi remove` and then
// `pi install` in each settings scope that still references the legacy spec.
// The old package is frozen on npm (releases continue only under the new
// name), so without this rewrite existing installs never see updates again.
// Version pins are dropped deliberately: pre-rename versions do not exist
// under the new package name. Local-path installs whose package.json declares
// the legacy name are developer checkouts, not npm installs, and are left
// alone.
//
// Remove-first is deliberate: if the install step fails, the next probe sees
// no registered plugin and Bootstrap retries the install through its normal
// path. Best-effort per scope — failures are logged and never block the
// launch; remove/install failures also print a manual-recovery hint on
// stderr. When pi itself is missing from PATH the pre-step stays silent and
// leaves the reporting to Bootstrap.
func migrateLegacyInstall(ctx context.Context, stderr io.Writer, logger *log.Logger) {
	bin, err := lookPath("pi")
	if err != nil {
		return
	}
	scopes := []struct {
		pathFn  func() (string, error)
		project bool
	}{
		{settingsPath, false},
		{projectSettingsPath, true},
	}
	for _, scope := range scopes {
		path, err := scope.pathFn()
		if err != nil {
			logger.Printf("pi legacy migration: %v", err)
			continue
		}
		hasLegacy, hasNew, err := scopeLegacyStatus(path)
		if err != nil {
			logger.Printf("pi legacy migration probe: %v", err)
			continue
		}
		if !hasLegacy {
			continue
		}
		migrateScope(ctx, bin, scope.project, hasNew, stderr, logger)
	}
}

// migrateScope runs the pi CLI commands migrating one settings scope. When
// the renamed package is already registered there (hasNew), only the legacy
// entry is removed — installing again would duplicate it. `pi remove` matches
// npm sources by name only, so a pinned legacy spec is removed too.
func migrateScope(ctx context.Context, bin string, project, hasNew bool, stderr io.Writer, logger *log.Logger) {
	suffix := ""
	if project {
		suffix = " -l"
	}
	fmt.Fprintf(stderr, "agento11y: migrating %s to %s in pi\n", legacyPluginSource, PluginSource)
	removeArgs := []string{"remove", legacyPluginSource}
	installArgs := []string{"install", PluginSource}
	if project {
		removeArgs = append(removeArgs, "-l")
		installArgs = append(installArgs, "-l")
	}
	if err := runPi(ctx, bin, stderr, removeArgs...); err != nil {
		logger.Printf("remove %s: %v", legacyPluginSource, err)
		fmt.Fprintf(stderr,
			"agento11y: migration of %s failed: %v\n"+
				"agento11y: continuing with the legacy package. To migrate manually:\n"+
				"          pi remove %s%s\n",
			legacyPluginSource, err, legacyPluginSource, suffix)
		if !hasNew {
			fmt.Fprintf(stderr, "          pi install %s%s\n", PluginSource, suffix)
		}
		return
	}
	if hasNew {
		return
	}
	if err := runPi(ctx, bin, stderr, installArgs...); err != nil {
		logger.Printf("install %s: %v", PluginSource, err)
		fmt.Fprintf(stderr,
			"agento11y: migration install of %s failed: %v\n"+
				"agento11y: to finish the migration manually:\n"+
				"          pi install %s%s\n",
			PluginSource, err, PluginSource, suffix)
	}
}

// scopeLegacyStatus reports whether one pi settings file registers the legacy
// @grafana/sigil-pi package through an npm spec (hasLegacy), and whether the
// renamed package is already registered in the same file through an npm spec
// or a local path (hasNew). Local-path entries whose package.json declares
// the legacy name never count as hasLegacy — they are developer checkouts. A
// missing file means the scope is unused.
func scopeLegacyStatus(path string) (hasLegacy, hasNew bool, err error) {
	sources, err := settingsSources(path)
	if err != nil {
		return false, false, err
	}
	settingsDir := filepath.Dir(path)
	for _, src := range sources {
		if after, ok := strings.CutPrefix(src, npmPrefix); ok {
			switch stripNpmVersion(after) {
			case legacyPluginName:
				hasLegacy = true
			case pluginName:
				hasNew = true
			}
			continue
		}
		if filepath.IsAbs(src) || strings.HasPrefix(src, "./") || strings.HasPrefix(src, "../") {
			p := src
			if !filepath.IsAbs(p) {
				p = filepath.Join(settingsDir, p)
			}
			if name, ok := localPackageName(p); ok && name == pluginName {
				hasNew = true
			}
		}
	}
	return hasLegacy, hasNew, nil
}

// piSettings is the subset of pi's settings.json this launcher inspects.
type piSettings struct {
	Packages []json.RawMessage `json:"packages"`
}

// pluginInstalled reports whether the @grafana/agento11y-pi extension is
// already registered in pi's settings. Pi reads two settings files: the
// global one under PI_CODING_AGENT_DIR (default ~/.pi/agent) and the
// project one at <cwd>/.pi/settings.json (used when the user runs
// `pi install -l` from a project directory). Either is enough — pi will
// load the extension at startup if it appears in either file.
//
// Settings entries may be plain strings or objects with a `source` field,
// and the source itself may be a bare npm spec (`npm:@grafana/agento11y-pi`),
// a versioned npm spec (`npm:@grafana/agento11y-pi@0.1.1`), or a local path.
// Entries using the legacy @grafana/sigil-pi name also count as installed.
// A missing settings file means that scope is unused — treat as not
// installed in that scope and move on.
func pluginInstalled() (bool, error) {
	_, found, err := installedPluginSource()
	return found, err
}

// Status reports whether the @grafana/agento11y-pi extension is registered in
// pi's settings. It reuses the read-only settings probe and never installs —
// `agento11y doctor` relies on this. When the registered source is a pinned npm
// spec (`npm:@grafana/agento11y-pi@0.1.1`) that version is reported; bare specs and
// local-path installs yield an empty (unknown) version.
func Status(_ context.Context) (installed bool, version string, err error) {
	source, found, err := installedPluginSource()
	if err != nil {
		return false, "", err
	}
	if !found {
		return false, "", nil
	}
	return true, versionFromPiSource(source), nil
}

// installedPluginSource returns the settings source string registering the
// @grafana/agento11y-pi extension, checking the global settings first and then the
// project-scoped settings (<cwd>/.pi/settings.json, written by `pi install -l`).
func installedPluginSource() (source string, found bool, err error) {
	globalPath, err := settingsPath()
	if err != nil {
		return "", false, err
	}
	if src, found, err := readSettingsAndCheck(globalPath); err != nil {
		return "", false, err
	} else if found {
		return src, true, nil
	}

	// Pi only consults the literal cwd, no parent walking, so we mirror that.
	// A failure to resolve cwd is exceptionally rare; treat it as "no project
	// settings available" rather than blocking the launch.
	projectPath, err := projectSettingsPath()
	if err != nil {
		return "", false, nil
	}
	return readSettingsAndCheck(projectPath)
}

// readSettingsAndCheck loads one pi settings file and returns the source
// string registering the @grafana/agento11y-pi extension, if any. A missing file
// is not an error — that scope is just unused.
func readSettingsAndCheck(path string) (source string, found bool, err error) {
	sources, err := settingsSources(path)
	if err != nil {
		return "", false, err
	}
	settingsDir := filepath.Dir(path)
	for _, src := range sources {
		if sourceMatchesPlugin(src, settingsDir) {
			return src, true, nil
		}
	}
	return "", false, nil
}

// settingsSources loads one pi settings file and returns the source string of
// every package entry, unwrapping both plain-string and `{"source": ...}`
// object shapes. A missing file is not an error — that scope is just unused.
func settingsSources(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var s piSettings
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	sources := make([]string, 0, len(s.Packages))
	for _, raw := range s.Packages {
		var src string
		if err := json.Unmarshal(raw, &src); err != nil {
			var asObj struct {
				Source string `json:"source"`
			}
			if err := json.Unmarshal(raw, &asObj); err != nil {
				continue
			}
			src = asObj.Source
		}
		sources = append(sources, src)
	}
	return sources, nil
}

// versionFromPiSource returns the pinned version of an npm-spec source, e.g.
// "0.1.1" from "npm:@grafana/agento11y-pi@0.1.1". Bare specs and local-path
// sources yield "".
func versionFromPiSource(source string) string {
	after, ok := strings.CutPrefix(source, npmPrefix)
	if !ok {
		return ""
	}
	at := strings.LastIndex(after, "@")
	if at <= 0 {
		return ""
	}
	return after[at+1:]
}

// sourceMatchesPlugin returns true when a pi settings source identifies the
// @grafana/agento11y-pi extension (or its legacy @grafana/sigil-pi name). It
// handles three shapes:
//   - bare npm specs: `npm:@grafana/agento11y-pi`
//   - versioned npm specs: `npm:@grafana/agento11y-pi@<version-or-tag>`
//   - local paths (absolute or relative to settingsDir) that point at a
//     directory whose package.json declares `name: "@grafana/agento11y-pi"`
//
// git: / https: / ssh: sources are never matched — we don't publish there.
func sourceMatchesPlugin(source, settingsDir string) bool {
	if source == "" {
		return false
	}
	if after, ok := strings.CutPrefix(source, npmPrefix); ok {
		return matchesPluginName(stripNpmVersion(after))
	}
	if filepath.IsAbs(source) || strings.HasPrefix(source, "./") || strings.HasPrefix(source, "../") {
		path := source
		if !filepath.IsAbs(path) {
			path = filepath.Join(settingsDir, path)
		}
		return localPathIsPlugin(path)
	}
	return false
}

// stripNpmVersion returns the package name portion of an npm spec, stripping
// the trailing `@<version>` segment if present. Scoped packages start with
// `@scope/...`; the leading `@` (index 0) is part of the name, not a version
// separator, so we look for the LAST `@` after index 0.
func stripNpmVersion(spec string) string {
	at := strings.LastIndex(spec, "@")
	if at <= 0 {
		return spec
	}
	return spec[:at]
}

// matchesPluginName reports whether name is the extension's package name,
// accepting the legacy pre-rename name so existing installs are not
// duplicated under the new name.
func matchesPluginName(name string) bool {
	return name == pluginName || name == legacyPluginName
}

// localPathIsPlugin returns true when path is a directory containing a
// package.json whose name matches the plugin (current or legacy pre-rename
// name).
func localPathIsPlugin(path string) bool {
	name, ok := localPackageName(path)
	return ok && matchesPluginName(name)
}

// localPackageName returns the package.json name of a local package
// directory. Any IO/parse failure means we can't confirm the name — treat as
// unknown rather than an error, since the settings file may legitimately
// reference other local packages we don't own.
func localPackageName(path string) (string, bool) {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return "", false
	}
	data, err := os.ReadFile(filepath.Join(path, "package.json"))
	if err != nil {
		return "", false
	}
	var pkg struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return "", false
	}
	return pkg.Name, true
}

// settingsPath returns the path to pi's global settings.json, honouring
// PI_CODING_AGENT_DIR (default $HOME/.pi/agent) per `pi --help`. Errors
// resolving the user's home directory are surfaced so callers don't probe a
// path silently rooted at CWD.
func settingsPath() (string, error) {
	dir := os.Getenv("PI_CODING_AGENT_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir for pi settings: %w", err)
		}
		dir = filepath.Join(home, ".pi", "agent")
	}
	return filepath.Join(dir, "settings.json"), nil
}

// projectSettingsPath returns the path to pi's project-scoped settings.json
// (<cwd>/.pi/settings.json). Pi only consults the literal cwd — it does not
// walk up parent directories — so we don't either.
func projectSettingsPath() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve cwd for pi project settings: %w", err)
	}
	return filepath.Join(cwd, projectConfigDirName, "settings.json"), nil
}
