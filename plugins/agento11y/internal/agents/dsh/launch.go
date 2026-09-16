// Package dsh installs the agento11y plugin in managed state and launches
// DeepSeek Harness with it as a patch overlay.
package dsh

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
	"github.com/grafana/agento11y/plugins/agento11y/internal/xdg"
	"gopkg.in/yaml.v3"
)

const (
	PluginSource = PluginName + "@latest"
	PluginName   = "@grafana/agento11y-dsh"

	binName       = "dsh"
	stateDirName  = "dsh"
	patchFileName = "agento11y.cordis.yml"
	// User patches can disable the managed row by this ID.
	patchRowID  = "agento11y"
	bundleEntry = "dist/index.js"

	patchFlag = "--patch"
	// dsh treats `web` as an alias for `--profile web`.
	webSubcommand         = "web"
	pluginSubcommand      = "plugin"
	dumpDefaultConfigFlag = "--dump-default-config"

	updateCheckTTL = 24 * time.Hour
)

// dsh hands its first unrecognized token and the remainder to the app. These
// operand counts limit scans to the launcher-owned prefix.
var launcherFlagOperands = map[string]int{
	"--profile":           1,
	patchFlag:             1,
	"--dump-config":       0,
	dumpDefaultConfigFlag: 0,
}

var (
	lookPath   = exec.LookPath
	execFn     = syscall.Exec
	runInstall = defaultRunInstall
	stateDirFn = defaultStateDir
)

// Pass the overlay only when its row and bundle are valid: dsh aborts on
// unreadable patches and unresolved rows. Capture failures must not block dsh.
func Launch(ctx context.Context, args []string, localEnv *local.LaunchEnv, _ io.Reader, _, stderr io.Writer, logger *log.Logger, binaryVersion string) error {
	dir := stateDirFn()
	return launcher.Bootstrap(ctx, launcher.BootstrapSpec{
		BinName:     binName,
		PluginLabel: PluginName,
		LookPath:    lookPath,
		ExecFn:      execFn,
		ArgsFn: func() []string {
			if ok, err := installed(dir); err != nil || !ok {
				return args
			}
			return launchArgs(patchPath(dir), args)
		},
		Env:    local.Environ(localEnv),
		Logger: logger,
		Stderr: stderr,
		// Probe errors are debug-only because this adapter owns the overlay;
		// install will attempt to replace it.
		Probe:           func(context.Context, string) (bool, error) { return installed(dir) },
		ProbeErrLog:     "dsh overlay probe",
		RegisterMessage: fmt.Sprintf("agento11y: installing %s for dsh\n", PluginName),
		Install:         func(ctx context.Context, _ string, w io.Writer) error { return install(ctx, dir, w) },
		InstallRecoveryHint: func(w io.Writer) {
			printRecoveryHint(w, dir)
		},
		Update: func(ctx context.Context, _ string, w io.Writer) error { return install(ctx, dir, w) },
		UpdateRecoveryHint: func(w io.Writer) {
			printRecoveryHint(w, dir)
		},
		UpdateTTL:     updateCheckTTL,
		BinaryVersion: binaryVersion,
	})
}

// dsh applies patches in argument order, so the managed patch goes first and
// user patches can override it. The parser requires `web` options after the
// subcommand. For `-- web [args...]`, both `web` and the patch move before
// `--`. `plugin` forwards to pnpm, and dsh forbids combining
// `--dump-default-config` with `--patch`, so neither invocation gets the patch.
func launchArgs(patch string, args []string) []string {
	if runsPluginCommand(args) || dumpsDefaultConfig(args) {
		return args
	}
	argv := make([]string, 0, len(args)+2)
	if len(args) > 1 && args[0] == "--" && args[1] == webSubcommand {
		argv = append(argv, webSubcommand, patchFlag, patch, "--")
		return append(argv, args[2:]...)
	}
	if len(args) > 0 && args[0] == webSubcommand {
		argv = append(argv, webSubcommand, patchFlag, patch)
		return append(argv, args[1:]...)
	}
	argv = append(argv, patchFlag, patch)
	return append(argv, args...)
}

// dsh accepts launcher flags before `plugin`, which cannot use a patch.
func runsPluginCommand(args []string) bool {
	for i := 0; i < len(args); {
		if args[i] == pluginSubcommand {
			return true
		}
		flag, _, inlineValue := strings.Cut(args[i], "=")
		operands, owned := launcherFlagOperands[flag]
		if !owned {
			return false
		}
		if inlineValue {
			operands = 0
		}
		i += 1 + operands
	}
	return false
}

// Stop at dsh's first unrecognized token. It and the remainder belong to the
// app, where --dump-default-config must not suppress capture.
func dumpsDefaultConfig(args []string) bool {
	i := 0
	if i < len(args) && args[i] == webSubcommand {
		i++
	}
	for i < len(args) {
		flag, _, inlineValue := strings.Cut(args[i], "=")
		if flag == dumpDefaultConfigFlag {
			return true
		}
		operands, owned := launcherFlagOperands[flag]
		if !owned {
			return false
		}
		if inlineValue {
			operands = 0
		}
		i += 1 + operands
	}
	return false
}

func printRecoveryHint(w io.Writer, dir string) {
	fmt.Fprintf(w, "          npm %s\n          Then rerun the same agento11y dsh command.\n", strings.Join(npmInstallArgs(dir), " "))
}

// Write the overlay only after npm leaves the expected bundle because dsh
// aborts on an unresolved row. A failed install must not create or replace it.
func install(ctx context.Context, dir string, w io.Writer) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create directory %s: %w", dir, err)
	}
	// MkdirAll preserves an existing mode; restrict access because dsh executes
	// code from this directory.
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("chmod %s: %w", dir, err)
	}
	if err := runInstall(ctx, dir, w); err != nil {
		return err
	}
	bundle := bundlePath(dir)
	if _, err := os.Stat(bundle); err != nil {
		return fmt.Errorf("npm reported success, but agento11y could not verify the expected bundle at %s: %w", bundle, err)
	}
	return writePatch(dir)
}

// `--prefix` targets managed state. `--no-save` and `--no-package-lock` avoid
// project metadata in a directory that is not a project.
func npmInstallArgs(dir string) []string {
	return []string{
		"install", "--prefix", dir,
		"--no-save", "--no-package-lock", "--no-audit", "--no-fund",
		PluginSource,
	}
}

func defaultRunInstall(ctx context.Context, dir string, w io.Writer) error {
	npm, err := lookPath("npm")
	if err != nil {
		return fmt.Errorf("npm is not available on PATH; install npm or add it to PATH to capture dsh sessions: %w", err)
	}
	return launcher.RunSteps(ctx, npm, w, [][]string{npmInstallArgs(dir)})
}

// dsh resolves row names relative to the profile directory, so the overlay
// must name the managed bundle by absolute path.
type patchOverlay struct {
	Insert []patchRow `yaml:"insert"`
}

type patchRow struct {
	ID   string `yaml:"id"`
	Name string `yaml:"name"`
}

// Replace the overlay atomically because dsh aborts if it reads partial YAML.
func writePatch(dir string) error {
	data, err := yaml.Marshal([]patchOverlay{{
		Insert: []patchRow{{ID: patchRowID, Name: bundlePath(dir)}},
	}})
	if err != nil {
		return fmt.Errorf("render overlay: %w", err)
	}
	path := patchPath(dir)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
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

// Trust only the exact managed row and an existing bundle. Missing state
// returns false so Bootstrap can repair it before dsh sees an unresolved row.
// Overlay read failures other than absence, and malformed YAML, return errors.
func installed(dir string) (bool, error) {
	path := patchPath(dir)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	var overlay []patchOverlay
	if err := yaml.Unmarshal(data, &overlay); err != nil {
		return false, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(overlay) != 1 || len(overlay[0].Insert) != 1 {
		return false, nil
	}
	row := overlay[0].Insert[0]
	if row.ID != patchRowID || row.Name != bundlePath(dir) {
		return false, nil
	}
	if _, err := os.Stat(row.Name); err != nil {
		return false, nil
	}
	return true, nil
}

// Status reads managed state instead of PATH, so doctor can report the
// integration when dsh is absent. Missing or invalid package metadata means
// an unknown version, not an error.
func Status(_ context.Context) (bool, string, error) {
	dir := stateDirFn()
	ok, err := installed(dir)
	if err != nil || !ok {
		return false, "", err
	}
	return true, packageVersion(packageDir(dir)), nil
}

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

// AppStateRoot falls back to the legacy sigil root so existing installs
// remain usable.
func defaultStateDir() string {
	return filepath.Join(xdg.AppStateRoot(), stateDirName)
}

func patchPath(dir string) string {
	return filepath.Join(dir, patchFileName)
}

func packageDir(dir string) string {
	return filepath.Join(dir, "node_modules", filepath.FromSlash(PluginName))
}

func bundlePath(dir string) string {
	return filepath.Join(packageDir(dir), filepath.FromSlash(bundleEntry))
}
