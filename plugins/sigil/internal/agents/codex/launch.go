package codex

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/grafana/sigil-sdk/plugins/sigil/internal/local"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/updatecheck"
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
	bin, err := lookPath("codex")
	if err != nil {
		return fmt.Errorf("codex CLI not found on PATH: %w", err)
	}

	installed, err := pluginInstalled(ctx, bin)
	if err != nil {
		// Treat a failed `codex plugin list` the same as missing: log the
		// probe failure for SIGIL_DEBUG, then let `codex plugin add` run.
		// codex's own CLI is the source of truth and will fail loudly if
		// something is genuinely wrong.
		logger.Printf("codex plugin list probe: %v", err)
		installed = false
	}
	if !installed {
		_, _ = fmt.Fprintf(stderr, "sigil: registering %s with codex\n", PluginName)
		if err := runInstall(ctx, bin, stderr); err != nil {
			// Don't block the user's codex session on a failed install
			// (offline machine, sandboxed CI, marketplace hiccup, etc.).
			// Log for SIGIL_DEBUG, point the user at the manual command,
			// and fall through to exec. codex still runs, just without
			// Sigil capture.
			logger.Printf("install %s: %v", PluginName, err)
			_, _ = fmt.Fprintf(stderr,
				"sigil: install of %s failed: %v\n"+
					"sigil: continuing without Sigil capture. To retry manually:\n"+
					"          codex plugin marketplace add %s\n"+
					"          codex plugin add %s@%s\n",
				PluginName, err, marketplaceRepo, PluginName, marketplaceAlias)
		} else {
			// One-time trust step the launcher cannot automate: codex
			// requires the user to open /hooks inside the TUI and accept
			// each sigil-codex hook on first run.
			_, _ = fmt.Fprintf(stderr,
				"sigil: first run only — open /hooks inside codex and trust the\n"+
					"       %s hooks to start exporting turns.\n", PluginName)
		}
	} else if updatecheck.ShouldRun(PluginName, updateCheckTTL, sigilVersion) {
		_, _ = fmt.Fprintf(stderr, "sigil: refreshing %s in codex\n", PluginName)
		if err := runUpdate(ctx, bin, stderr); err != nil {
			logger.Printf("update %s: %v", PluginName, err)
			_, _ = fmt.Fprintf(stderr,
				"sigil: update of %s failed: %v\n"+
					"sigil: continuing with the installed version. To retry manually:\n"+
					"          codex plugin marketplace upgrade\n"+
					"          codex plugin add %s@%s\n",
				PluginName, err, PluginName, marketplaceAlias)
		}
		updatecheck.Record(PluginName, sigilVersion)
	}

	env := local.Environ(localEnv)
	argv := append([]string{bin}, args...)
	if err := execFn(bin, argv, env); err != nil {
		return fmt.Errorf("exec codex: %w", err)
	}
	return nil
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
	return runSteps(ctx, bin, w, [][]string{
		{"plugin", "marketplace", "add", marketplaceRepo},
		{"plugin", "add", PluginName + "@" + marketplaceAlias},
	})
}

func defaultRunUpdate(ctx context.Context, bin string, w io.Writer) error {
	return runSteps(ctx, bin, w, [][]string{
		{"plugin", "marketplace", "upgrade"},
		{"plugin", "add", PluginName + "@" + marketplaceAlias},
	})
}

func runSteps(ctx context.Context, bin string, w io.Writer, steps [][]string) error {
	for _, argv := range steps {
		cmd := exec.CommandContext(ctx, bin, argv...)
		cmd.Stdout = w
		cmd.Stderr = w
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("codex %s: %w", strings.Join(argv, " "), err)
		}
	}
	return nil
}

// defaultPluginList shells out to `codex plugin list` and returns the raw
// output. Output is human-formatted but stable: each plugin line looks like
// `  <plugin>@<marketplace> (<state>)`.
func defaultPluginList(ctx context.Context, bin string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, "plugin", "list")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		// `%v` on *exec.ExitError renders only "exit status N" and drops the
		// captured stderr, so attach codex's diagnostic explicitly.
		if msg := bytes.TrimSpace(stderr.Bytes()); len(msg) > 0 {
			return nil, fmt.Errorf("%w: %s", err, msg)
		}
		return nil, err
	}
	return out, nil
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
