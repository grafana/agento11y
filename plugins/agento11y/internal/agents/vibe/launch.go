package vibe

import (
	"context"
	"fmt"
	"io"
	"log"
	"os/exec"
	"syscall"

	"github.com/grafana/agento11y/plugins/agento11y/internal/local"
)

// Test seams.
var (
	lookPath = exec.LookPath
	execFn   = syscall.Exec
)

// Launch resolves the `vibe` binary on PATH, ensures the three agento11y-owned
// hook entries are installed in vibe's hooks.toml, and then execs vibe with
// the supplied args.
//
// The entries are spelled for the installed vibe, which the install path reads
// from `vibe --version`, because 2.21.0 renamed all three hook types.
//
// VIBE_ENABLE_EXPERIMENTAL_HOOKS=true is injected into the child env because a
// pre-2.21.0 vibe loads hooks.toml only behind that flag. Vibe uses
// pydantic-settings with env_prefix="VIBE_", so the env override is
// recognised without further config.toml editing. 2.21.0 removed the flag
// along with the gate, and its env layer ignores unknown VIBE_ variables, so
// setting it is inert there rather than an error. It stays unconditional: a
// version probe that fails guesses current, and dropping the flag on a
// mis-detected old vibe would turn hooks off entirely.
//
// When localEnv is non-nil, the child receives local-mode SIGIL_ENDPOINT,
// SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT and placeholder auth values so it
// talks to the in-process receiver instead of Grafana Cloud.
func Launch(ctx context.Context, args []string, localEnv *local.LaunchEnv, _ io.Reader, _, stderr io.Writer, logger *log.Logger, _ string) error {
	bin, err := lookPath("vibe")
	if err != nil {
		return fmt.Errorf("vibe CLI not found on PATH: %w", err)
	}

	installHook(ctx, stderr, logger)

	env := envWithExperimentalHooks(local.Environ(localEnv))
	argv := append([]string{bin}, args...)
	if err := execFn(bin, argv, env); err != nil {
		return fmt.Errorf("exec vibe: %w", err)
	}
	return nil
}

// Status reports whether agento11y capture is configured for vibe. Capture is
// driven by the agento11y-owned entries the launcher merges into vibe's
// hooks.toml, so doctor checks for those, spelled for the installed vibe. The
// reported version stays empty: the column holds the plugin version for every
// other agent, and vibe capture ships inside this binary. It never installs,
// updates, or removes anything — `agento11y doctor` relies on this.
func Status(ctx context.Context) (installed bool, version string, err error) {
	installed, err = HooksInstalled(hookTypesFor(ctx, nil))
	return installed, "", err
}

// installHook upserts the agento11y entries into hooks.toml and reports the
// outcome on stderr. Failures are logged but never block the launch:
// the user explicitly asked to run vibe, so an agento11y install hiccup must
// not gate that.
func installHook(ctx context.Context, stderr io.Writer, logger *log.Logger) {
	path, wrote, err := ensureHookInstalled(hookTypesFor(ctx, logger))
	if err != nil {
		logger.Printf("install vibe hook: %v", err)
		_, _ = fmt.Fprintf(stderr, "agento11y: could not install vibe hook (%v); continuing without capture\n", err)
		return
	}
	if wrote {
		_, _ = fmt.Fprintf(stderr, "agento11y: installed Vibe hook at %s\n", path)
	}
}

// envWithExperimentalHooks returns env with VIBE_ENABLE_EXPERIMENTAL_HOOKS
// forced to "true". Any existing value is replaced so a stale "false" in
// the user's shell does not silently disable our hook on a pre-2.21.0 vibe.
func envWithExperimentalHooks(env []string) []string {
	const key = "VIBE_ENABLE_EXPERIMENTAL_HOOKS"
	const want = "true"
	out := make([]string, 0, len(env)+1)
	replaced := false
	prefix := key + "="
	for _, kv := range env {
		if len(kv) >= len(prefix) && kv[:len(prefix)] == prefix {
			out = append(out, prefix+want)
			replaced = true
			continue
		}
		out = append(out, kv)
	}
	if !replaced {
		out = append(out, prefix+want)
	}
	return out
}
