package codex

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os/exec"
	"syscall"
	"time"

	"github.com/grafana/sigil-sdk/plugins/sigil/internal/launcher"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/local"
)

const (
	// marketplaceRepo is the source argument passed to
	// `codex plugin marketplace add`.
	marketplaceRepo = "grafana/sigil-sdk"
	// marketplaceAlias is the marketplace name declared in
	// .claude-plugin/marketplace.json (shared between Claude Code and Codex
	// host plugins).
	marketplaceAlias = "grafana-sigil"
	// PluginName is the codex plugin name declared in
	// plugins/codex/.codex-plugin/plugin.json.
	PluginName = "sigil-codex"

	updateCheckTTL = 24 * time.Hour
)

// Test seams.
var (
	lookPath   = exec.LookPath
	execFn     = syscall.Exec
	runInstall = defaultRunInstall
	runUpdate  = defaultRunUpdate
	pluginList = defaultPluginList
)

// Launch resolves the `codex` binary on PATH, ensures the sigil-codex plugin
// is registered and enabled in codex's plugin store (running
// `codex plugin marketplace add` + `codex plugin add` once if it is not),
// and then exec's codex with the supplied args. When localEnv is non-nil,
// the child receives local-mode SIGIL_ENDPOINT,
// SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT and placeholder auth values so it talks
// to the in-process receiver instead of Sigil Cloud.
func Launch(ctx context.Context, args []string, localEnv *local.LaunchEnv, _ io.Reader, _, stderr io.Writer, logger *log.Logger, sigilVersion string) error {
	return launcher.Bootstrap(ctx, launcher.BootstrapSpec{
		BinName:     "codex",
		PluginLabel: PluginName,
		LookPath:    lookPath,
		ExecFn:      execFn,
		Args:        args,
		Env:         local.Environ(localEnv),
		Logger:      logger,
		Stderr:      stderr,
		// Treat a failed `codex plugin list` the same as missing: codex's
		// own CLI is the source of truth and will fail loudly if something
		// is genuinely wrong.
		Probe:       pluginInstalled,
		ProbeErrLog: "codex plugin list probe",
		Install:     runInstall,
		InstallRecoveryHint: func(w io.Writer) {
			fmt.Fprintf(w,
				"          codex plugin marketplace add %s\n"+
					"          codex plugin add %s@%s\n",
				marketplaceRepo, PluginName, marketplaceAlias)
		},
		// One-time trust step the launcher cannot automate: codex requires
		// the user to open /hooks inside the TUI and accept each
		// sigil-codex hook on first run.
		PostInstallHint: func(w io.Writer) {
			fmt.Fprintf(w,
				"agento11y: first run only — open /hooks inside codex and trust the\n"+
					"           %s hooks to start exporting turns.\n", PluginName)
		},
		Update: runUpdate,
		UpdateRecoveryHint: func(w io.Writer) {
			fmt.Fprintf(w,
				"          codex plugin marketplace upgrade\n"+
					"          codex plugin add %s@%s\n",
				PluginName, marketplaceAlias)
		},
		UpdateTTL:    updateCheckTTL,
		SigilVersion: sigilVersion,
	})
}

// Status reports whether the sigil-codex plugin is installed and enabled. It
// reuses the read-only `codex plugin list` probe and never installs, updates,
// or writes update-check state — `sigil doctor` relies on this. codex's plugin
// list does not expose a version, so version is always empty (best-effort).
func Status(ctx context.Context) (installed bool, version string, err error) {
	bin, err := lookPath("codex")
	if err != nil {
		return false, "", err
	}
	installed, err = pluginInstalled(ctx, bin)
	return installed, "", err
}

func defaultRunInstall(ctx context.Context, bin string, w io.Writer) error {
	return launcher.RunSteps(ctx, bin, w, [][]string{
		{"plugin", "marketplace", "add", marketplaceRepo},
		{"plugin", "add", PluginName + "@" + marketplaceAlias},
	})
}

func defaultRunUpdate(ctx context.Context, bin string, w io.Writer) error {
	return launcher.RunSteps(ctx, bin, w, [][]string{
		{"plugin", "marketplace", "upgrade"},
		{"plugin", "add", PluginName + "@" + marketplaceAlias},
	})
}

// defaultPluginList shells out to `codex plugin list` and returns the raw
// output. Output is human-formatted but stable: each plugin line looks like
// `  <plugin>@<marketplace> (<state>)`.
func defaultPluginList(ctx context.Context, bin string) ([]byte, error) {
	return launcher.Output(ctx, bin, "plugin", "list")
}

// pluginInstalled reports whether `sigil-codex@grafana-sigil` is registered
// and enabled in codex's plugin store.
//
// We shell out to `codex plugin list` because it's the source of truth and
// doesn't depend on the user's $XDG_CONFIG_HOME layout. Anything other than
// an `(installed, enabled)` marker is treated as not installed —
// `(installed, disabled)` shouldn't be silently re-enabled in the user's
// face but re-running install on codex's side is idempotent and surfaces
// the disabled state to the user.
func pluginInstalled(ctx context.Context, bin string) (bool, error) {
	out, err := pluginList(ctx, bin)
	if err != nil {
		return false, err
	}
	needle := []byte(PluginName + "@" + marketplaceAlias)
	for line := range bytes.SplitSeq(out, []byte{'\n'}) {
		// Anchor on the first whitespace-delimited token so suffix collisions
		// like `my-sigil-codex@grafana-sigil` or
		// `sigil-codex@grafana-sigil-staging` don't satisfy the check.
		fields := bytes.Fields(line)
		if len(fields) == 0 || !bytes.Equal(fields[0], needle) {
			continue
		}
		if bytes.Contains(line, []byte("installed, enabled")) {
			return true, nil
		}
	}
	return false, nil
}
