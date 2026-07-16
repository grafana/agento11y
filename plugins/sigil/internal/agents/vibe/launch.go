package vibe

import (
	"context"
	"fmt"
	"io"
	"log"
	"os/exec"
	"syscall"

	"github.com/grafana/sigil-sdk/plugins/sigil/internal/local"
)

// Test seams.
var (
	lookPath = exec.LookPath
	execFn   = syscall.Exec
)

// Launch resolves the `vibe` binary on PATH, ensures the sigil-owned
// post_agent_turn hook is installed into vibe's hooks.toml, and then
// execs vibe with the supplied args.
//
// VIBE_ENABLE_EXPERIMENTAL_HOOKS=true is injected into the child env so
// vibe loads hooks.toml even on builds where the
// enable_experimental_hooks setting is still gated. Vibe uses
// pydantic-settings with env_prefix="VIBE_", so the env override is
// recognised without further config.toml editing.
//
// When localEnv is non-nil, the child receives local-mode SIGIL_ENDPOINT,
// SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT and placeholder auth values so it
// talks to the in-process receiver instead of Sigil Cloud.
func Launch(_ context.Context, args []string, localEnv *local.LaunchEnv, _ io.Reader, _, stderr io.Writer, logger *log.Logger, _ string) error {
	bin, err := lookPath("vibe")
	if err != nil {
		return fmt.Errorf("vibe CLI not found on PATH: %w", err)
	}

	installHook(stderr, logger)

	env := envWithExperimentalHooks(local.Environ(localEnv))
	argv := append([]string{bin}, args...)
	if err := execFn(bin, argv, env); err != nil {
		return fmt.Errorf("exec vibe: %w", err)
	}
	return nil
}

// installHook upserts the sigil entry into hooks.toml and reports the
// outcome on stderr. Failures are logged but never block the launch:
// the user explicitly asked to run vibe, so a Sigil install hiccup must
// not gate that.
func installHook(stderr io.Writer, logger *log.Logger) {
	path, wrote, err := ensureHookInstalled()
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
// the user's shell does not silently disable our hook.
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
