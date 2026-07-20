// Package launcher holds the small pieces shared by the agent CLI launchers:
// the execve handoff that replaces the sigil process with the target CLI, and
// a command runner that captures stdout while surfacing stderr on failure.
//
// Each launcher keeps its own lookPath/execFn/runInstall/pluginList package
// vars — the test seams — so these helpers take the exec function and command
// arguments as parameters instead of reaching for package globals. Install-flow
// orchestration stays in each launcher because the CLIs differ (single vs
// marketplace+install steps, per-step error wording).
package launcher

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strings"
	"time"

	"github.com/grafana/agento11y/plugins/agento11y/internal/updatecheck"
)

// ExecFunc matches syscall.Exec: it replaces the current process image with a
// new program. The launchers assign syscall.Exec to a package var and pass it
// here so tests can substitute a recording stub.
type ExecFunc func(argv0 string, argv []string, envv []string) error

// Exec replaces the current process with bin, prepending bin as argv[0] and
// forwarding args plus env. name is used only for the error prefix
// ("exec <name>"). Callers pass the env explicitly so local-mode launches can
// inject SIGIL_ENDPOINT overrides via local.Environ; pass os.Environ() for the
// normal path. On success the process is replaced and Exec does not return; a
// returned error means the execve syscall itself failed.
func Exec(execFn ExecFunc, bin, name string, args, env []string) error {
	argv := append([]string{bin}, args...)
	if err := execFn(bin, argv, env); err != nil {
		return fmt.Errorf("exec %s: %w", name, err)
	}
	return nil
}

// Output runs `bin args...` and returns its stdout. On failure it attaches any
// captured stderr to the error: *exec.ExitError renders only "exit status N"
// under %v and drops the CLI's own diagnostic, so we surface it explicitly.
func Output(ctx context.Context, bin string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
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

// RunSteps invokes `bin argv...` for each step in order, writing the child's
// stdout/stderr to w. On a step failure it stops and returns an error of the
// form "<bin> <argv>: <err>". claudecode, codex, and opencode all compose
// their install/update sequences this way.
func RunSteps(ctx context.Context, bin string, w io.Writer, steps [][]string) error {
	name := bin
	if idx := strings.LastIndexByte(bin, '/'); idx >= 0 {
		name = bin[idx+1:]
	}
	for _, argv := range steps {
		cmd := exec.CommandContext(ctx, bin, argv...)
		cmd.Stdout = w
		cmd.Stderr = w
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s %s: %w", name, strings.Join(argv, " "), err)
		}
	}
	return nil
}

// BootstrapSpec describes the per-agent variation Bootstrap needs to drive
// the shared lookup → probe → install/update → exec sequence. Each adapter
// (claudecode, codex, opencode, pi) builds a spec from its package-level test
// seams; closures capture those vars so test stubs swapped in via
// withRunInstall/withPluginList still take effect. copilot does not fit this
// flow (it writes a hooks file and removes a stale plugin instead of
// installing one) and only shares Output.
//
// Required fields: BinName, PluginLabel, LookPath, ExecFn, Probe, Install,
// Logger, Stderr. Everything else is optional and documented below.
type BootstrapSpec struct {
	// BinName is the CLI binary name resolved on PATH (e.g. "claude",
	// "codex", "copilot", "pi"). Used as the lookup target, the "<bin> CLI
	// not found" error prefix, and the display name in the default
	// register/refresh messages.
	BinName string
	// PluginLabel identifies the plugin in user-facing messages.
	// claudecode/codex pass the plugin name ("sigil-cc"); pi and opencode
	// pass the npm source ("npm:@grafana/agento11y-pi") because that's what the
	// user types to retry by hand.
	PluginLabel string

	// LookPath and ExecFn are the test seams forwarded from the adapter so
	// launch tests can stub PATH resolution and the execve handoff.
	LookPath func(string) (string, error)
	ExecFn   ExecFunc
	Args     []string
	Env      []string

	// Logger receives SIGIL_DEBUG diagnostics (probe failures, install/update
	// errors). Stderr is the user-facing channel for the "registering",
	// "refreshing", and recovery-hint lines.
	Logger *log.Logger
	Stderr io.Writer

	// Probe reports whether the plugin is already registered. A non-nil
	// error is logged with ProbeErrLog and the plugin is treated as missing
	// so install runs and surfaces the real diagnostic.
	Probe       func(ctx context.Context, bin string) (bool, error)
	ProbeErrLog string // log prefix for probe failures ("<prefix>: <err>")
	// ProbeErrEcho also echoes "agento11y: <ProbeErrLog> failed: <err>" to
	// Stderr. pi uses this so a broken settings file shows up in the user's
	// terminal, not just SIGIL_DEBUG.
	ProbeErrEcho bool

	// Install runs the first-time registration when Probe returns false.
	Install func(ctx context.Context, bin string, w io.Writer) error
	// InstallRecoveryHint prints manual-retry commands to Stderr when Install
	// fails. Bootstrap emits the generic "install of <label> failed" /
	// "continuing without Sigil capture" lines before calling this; the hook
	// only contributes the indented command lines.
	InstallRecoveryHint func(w io.Writer)
	// PostInstallHint, when non-nil, runs after a successful Install. codex
	// uses it for the one-time `/hooks` trust message.
	PostInstallHint func(w io.Writer)

	// Update runs the periodic refresh. When nil, Bootstrap skips the update
	// path entirely (pi defers upgrades to its own installer).
	Update             func(ctx context.Context, bin string, w io.Writer) error
	UpdateRecoveryHint func(w io.Writer)
	UpdateTTL          time.Duration
	SigilVersion       string

	// RegisterMessage overrides the default
	// "agento11y: registering <label> with <bin>\n" line printed before Install.
	// pi and opencode override it to "agento11y: installing <source> into <bin>\n".
	RegisterMessage string
}

// Bootstrap resolves spec.BinName on PATH, probes for an existing plugin
// install, runs Install or Update as needed (logging failures and falling
// through to exec so the user's session is never blocked by a broken
// install), and finally exec's spec.BinName with spec.Args and spec.Env.
//
// Returns the lookup error when spec.BinName isn't on PATH. Returns the
// execve error when the final exec syscall fails. Install/update failures
// are intentionally not propagated.
func Bootstrap(ctx context.Context, spec BootstrapSpec) error {
	bin, err := spec.LookPath(spec.BinName)
	if err != nil {
		return fmt.Errorf("%s CLI not found on PATH: %w", spec.BinName, err)
	}

	installed, err := spec.Probe(ctx, bin)
	if err != nil {
		spec.Logger.Printf("%s: %v", spec.ProbeErrLog, err)
		if spec.ProbeErrEcho {
			fmt.Fprintf(spec.Stderr, "agento11y: %s failed: %v\n", spec.ProbeErrLog, err)
		}
		installed = false
	}

	switch {
	case !installed:
		fmt.Fprint(spec.Stderr, registerMessage(spec))
		if err := spec.Install(ctx, bin, spec.Stderr); err != nil {
			spec.Logger.Printf("install %s: %v", spec.PluginLabel, err)
			fmt.Fprintf(spec.Stderr,
				"agento11y: install of %s failed: %v\n"+
					"agento11y: continuing without Sigil capture. To retry manually:\n",
				spec.PluginLabel, err)
			if spec.InstallRecoveryHint != nil {
				spec.InstallRecoveryHint(spec.Stderr)
			}
		} else if spec.PostInstallHint != nil {
			spec.PostInstallHint(spec.Stderr)
		}
	case spec.Update != nil && updatecheck.ShouldRun(spec.PluginLabel, spec.UpdateTTL, spec.SigilVersion):
		fmt.Fprintf(spec.Stderr, "agento11y: refreshing %s in %s\n", spec.PluginLabel, spec.BinName)
		if err := spec.Update(ctx, bin, spec.Stderr); err != nil {
			spec.Logger.Printf("update %s: %v", spec.PluginLabel, err)
			fmt.Fprintf(spec.Stderr,
				"agento11y: update of %s failed: %v\n"+
					"agento11y: continuing with the installed version. To retry manually:\n",
				spec.PluginLabel, err)
			if spec.UpdateRecoveryHint != nil {
				spec.UpdateRecoveryHint(spec.Stderr)
			}
		}
		updatecheck.Record(spec.PluginLabel, spec.SigilVersion)
	}

	return Exec(spec.ExecFn, bin, spec.BinName, spec.Args, spec.Env)
}

func registerMessage(spec BootstrapSpec) string {
	if spec.RegisterMessage != "" {
		return spec.RegisterMessage
	}
	return fmt.Sprintf("agento11y: registering %s with %s\n", spec.PluginLabel, spec.BinName)
}
