// Package pi implements the pi launcher adapter for the consolidated sigil
// binary. The dispatcher in cmd/sigil routes `sigil pi [-- args...]` here.
//
// Unlike the hook adapters, this adapter owns the user's terminal: it
// bootstraps the @grafana/sigil-pi extension in pi's settings file on first
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

	"github.com/grafana/sigil-sdk/plugins/sigil/internal/local"
)

const (
	// PluginSource is the canonical npm spec used for fresh installs.
	PluginSource = "npm:@grafana/sigil-pi"
	// pluginName is the package.json `name` of the extension. Used to detect
	// versioned npm specs (e.g. `npm:@grafana/sigil-pi@0.1.1`) and local-path
	// installs that point at a checkout of this package.
	pluginName = "@grafana/sigil-pi"
	npmPrefix  = "npm:"
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
)

// Launch resolves the `pi` binary on PATH, ensures the @grafana/sigil-pi
// extension is registered in the user's pi settings (running `pi install`
// once if it is not), and then exec's pi with the supplied args. When
// localEnv is non-nil, the child receives local-mode SIGIL_ENDPOINT,
// SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT and placeholder auth values so it
// talks to the in-process receiver instead of Sigil Cloud. The trailing
// string arg is the sigil build version; the pi adapter does not run
// auto-update checks (pi's own installer handles upgrades) so it is
// accepted to satisfy the launcher signature and ignored.
func Launch(ctx context.Context, args []string, localEnv *local.LaunchEnv, _ io.Reader, _, stderr io.Writer, logger *log.Logger, _ string) error {
	bin, err := lookPath("pi")
	if err != nil {
		return fmt.Errorf("pi CLI not found on PATH: %w", err)
	}

	installed, err := pluginInstalled()
	if err != nil {
		logger.Printf("settings probe: %v", err)
		// Surface the probe failure so the user can see why we're falling
		// through to install on a settings file we couldn't read. Treat the
		// case like a missing extension — pi's installer will fail loudly if
		// the file is genuinely broken.
		_, _ = fmt.Fprintf(stderr, "sigil: pi settings probe failed: %v\n", err)
		installed = false
	}
	if !installed {
		_, _ = fmt.Fprintf(stderr, "sigil: installing %s into pi\n", PluginSource)
		if err := runInstall(ctx, bin, stderr); err != nil {
			// Don't block the user's pi session on a failed install (offline
			// machine, npm rate-limit, sandboxed CI, etc.). Log the failure for
			// SIGIL_DEBUG, point the user at the manual command, and fall
			// through to exec. pi still runs, just without Sigil capture.
			logger.Printf("install %s: %v", PluginSource, err)
			_, _ = fmt.Fprintf(stderr,
				"sigil: install of %s failed: %v\n"+
					"sigil: continuing without Sigil capture. To retry manually:\n"+
					"          pi install %s\n",
				PluginSource, err, PluginSource)
		}
	}

	env := local.Environ(localEnv)
	argv := append([]string{bin}, args...)
	if err := execFn(bin, argv, env); err != nil {
		return fmt.Errorf("exec pi: %w", err)
	}
	return nil
}

func defaultRunInstall(ctx context.Context, bin string, w io.Writer) error {
	cmd := exec.CommandContext(ctx, bin, "install", PluginSource)
	cmd.Stdout = w
	cmd.Stderr = w
	return cmd.Run()
}

// piSettings is the subset of pi's settings.json this launcher inspects.
type piSettings struct {
	Packages []json.RawMessage `json:"packages"`
}

// pluginInstalled reports whether the @grafana/sigil-pi extension is
// already registered in pi's settings. Pi reads two settings files: the
// global one under PI_CODING_AGENT_DIR (default ~/.pi/agent) and the
// project one at <cwd>/.pi/settings.json (used when the user runs
// `pi install -l` from a project directory). Either is enough — pi will
// load the extension at startup if it appears in either file.
//
// Settings entries may be plain strings or objects with a `source` field,
// and the source itself may be a bare npm spec (`npm:@grafana/sigil-pi`),
// a versioned npm spec (`npm:@grafana/sigil-pi@0.1.1`), or a local path.
// A missing settings file means that scope is unused — treat as not
// installed in that scope and move on.
func pluginInstalled() (bool, error) {
	_, found, err := installedPluginSource()
	return found, err
}

// Status reports whether the @grafana/sigil-pi extension is registered in pi's
// settings. It reuses the read-only settings probe and never installs — `sigil
// doctor` relies on this. When the registered source is a pinned npm spec
// (`npm:@grafana/sigil-pi@0.1.1`) that version is reported; bare specs and
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
// @grafana/sigil-pi extension, checking the global settings first and then the
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
// string registering the @grafana/sigil-pi extension, if any. A missing file
// is not an error — that scope is just unused.
func readSettingsAndCheck(path string) (source string, found bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read %s: %w", path, err)
	}
	var s piSettings
	if err := json.Unmarshal(data, &s); err != nil {
		return "", false, fmt.Errorf("parse %s: %w", path, err)
	}
	settingsDir := filepath.Dir(path)
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
		if sourceMatchesPlugin(src, settingsDir) {
			return src, true, nil
		}
	}
	return "", false, nil
}

// versionFromPiSource returns the pinned version of an npm-spec source, e.g.
// "0.1.1" from "npm:@grafana/sigil-pi@0.1.1". Bare specs and local-path
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
// @grafana/sigil-pi extension. It handles three shapes:
//   - bare npm specs: `npm:@grafana/sigil-pi`
//   - versioned npm specs: `npm:@grafana/sigil-pi@<version-or-tag>`
//   - local paths (absolute or relative to settingsDir) that point at a
//     directory whose package.json declares `name: "@grafana/sigil-pi"`
//
// git: / https: / ssh: sources are never matched — we don't publish there.
func sourceMatchesPlugin(source, settingsDir string) bool {
	if source == "" {
		return false
	}
	if after, ok := strings.CutPrefix(source, npmPrefix); ok {
		return stripNpmVersion(after) == pluginName
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

// localPathIsPlugin returns true when path is a directory containing a
// package.json with name == pluginName. Any IO/parse failure means we can't
// confirm the match — treat as a non-match rather than an error, since the
// settings file may legitimately reference other local packages we don't
// own.
func localPathIsPlugin(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return false
	}
	data, err := os.ReadFile(filepath.Join(path, "package.json"))
	if err != nil {
		return false
	}
	var pkg struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return false
	}
	return pkg.Name == pluginName
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
