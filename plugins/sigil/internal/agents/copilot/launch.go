package copilot

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os/exec"
	"syscall"

	"github.com/grafana/sigil-sdk/plugins/sigil/internal/local"
)

const (
	// installSource is the GitHub subdir spec passed to
	// `copilot plugin install`. Copilot CLI documents
	// `OWNER/REPO:PATH/TO/PLUGIN` as a first-class install form, so the
	// launcher uses a single-step install with no separate marketplace step.
	installSource = "grafana/sigil-sdk:plugins/copilot"
	// PluginName is the plugin name declared in plugins/copilot/plugin.json.
	PluginName = "sigil-copilot"
)

// Test seams.
var (
	lookPath   = exec.LookPath
	execFn     = syscall.Exec
	runInstall = defaultRunInstall
	pluginList = defaultPluginList
)

// Launch resolves the `copilot` binary on PATH, ensures the sigil-copilot
// plugin is registered in copilot's plugin store (running
// `copilot plugin install grafana/sigil-sdk:plugins/copilot` once if it is
// not), and then exec's copilot with the supplied args. When localEnv is
// non-nil, the child receives local-mode SIGIL_ENDPOINT,
// SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT and placeholder auth values so it talks
// to the in-process receiver instead of Sigil Cloud.
func Launch(ctx context.Context, args []string, localEnv *local.LaunchEnv, _ io.Reader, _, stderr io.Writer, logger *log.Logger) error {
	bin, err := lookPath("copilot")
	if err != nil {
		return fmt.Errorf("copilot CLI not found on PATH: %w", err)
	}

	installed, err := pluginInstalled(ctx, bin)
	if err != nil {
		// Treat a failed `copilot plugin list` the same as missing: log the
		// probe failure for SIGIL_DEBUG, then let `copilot plugin install`
		// run. copilot's own CLI is the source of truth and will fail loudly
		// if something is genuinely wrong.
		logger.Printf("copilot plugin list probe: %v", err)
		installed = false
	}
	if !installed {
		_, _ = fmt.Fprintf(stderr, "sigil: registering %s with copilot\n", PluginName)
		if err := runInstall(ctx, bin, stderr); err != nil {
			// Don't block the user's copilot session on a failed install
			// (offline machine, sandboxed CI, GitHub rate limit, etc.). Log
			// for SIGIL_DEBUG, point the user at the manual command, and
			// fall through to exec. copilot still runs, just without Sigil
			// capture.
			logger.Printf("install %s: %v", PluginName, err)
			_, _ = fmt.Fprintf(stderr,
				"sigil: install of %s failed: %v\n"+
					"sigil: continuing without Sigil capture. To retry manually:\n"+
					"          copilot plugin install %s\n",
				PluginName, err, installSource)
		}
	}

	env := local.Environ(localEnv)
	argv := append([]string{bin}, args...)
	if err := execFn(bin, argv, env); err != nil {
		return fmt.Errorf("exec copilot: %w", err)
	}
	return nil
}

func defaultRunInstall(ctx context.Context, bin string, w io.Writer) error {
	cmd := exec.CommandContext(ctx, bin, "plugin", "install", installSource)
	cmd.Stdout = w
	cmd.Stderr = w
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("copilot plugin install %s: %w", installSource, err)
	}
	return nil
}

// defaultPluginList shells out to `copilot plugin list` and returns the raw
// output. Output is human-formatted but stable for direct installs: a
// `Installed plugins:` header followed by one `  • <plugin> (v<ver>)` line
// per plugin.
func defaultPluginList(ctx context.Context, bin string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, "plugin", "list")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := bytes.TrimSpace(stderr.Bytes()); len(msg) > 0 {
			return nil, fmt.Errorf("%w: %s", err, msg)
		}
		return nil, err
	}
	return out, nil
}

// pluginInstalled reports whether `sigil-copilot` is registered in copilot's
// plugin store. Disabled-state detection is deferred until we confirm
// copilot's `plugin list` output for a disabled plugin — for now, presence
// counts as installed.
func pluginInstalled(ctx context.Context, bin string) (bool, error) {
	out, err := pluginList(ctx, bin)
	if err != nil {
		return false, err
	}
	needle := []byte(PluginName)
	// Lines look like `  • sigil-copilot (v0.1.0)`. Strip leading whitespace
	// and the bullet glyph, then exact-match the first remaining token. This
	// rejects the `Installed plugins:` header, a bare bullet line, and
	// suffix collisions like `my-sigil-copilot`.
	for line := range bytes.SplitSeq(out, []byte{'\n'}) {
		trimmed := bytes.TrimLeft(bytes.TrimSpace(line), "•*-+ \t")
		fields := bytes.Fields(trimmed)
		if len(fields) > 0 && bytes.Equal(fields[0], needle) {
			return true, nil
		}
	}
	return false, nil
}
