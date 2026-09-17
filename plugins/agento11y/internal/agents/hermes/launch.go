// Package hermes wires the Hermes Agent native plugin into agento11y.
package hermes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agentinstall"
	"github.com/grafana/agento11y/plugins/agento11y/internal/launcher"
	"github.com/grafana/agento11y/plugins/agento11y/internal/local"
)

const (
	// PluginName is the Hermes directory-plugin name declared in plugin.yaml.
	PluginName = "agento11y-hermes"
	// PluginSource is the public source passed to Hermes's native plugin manager.
	PluginSource = "grafana/agento11y/plugins/hermes"
)

// ErrCLINotFound means Hermes is not available on PATH for this user.
var ErrCLINotFound = errors.New("hermes CLI not found")

func init() {
	agentinstall.Register(agentinstall.Spec{
		Name:          "hermes",
		Install:       Install,
		IsMissingHost: func(err error) bool { return errors.Is(err, ErrCLINotFound) },
	})
}

var (
	lookPath = exec.LookPath
	execFn   = syscall.Exec
	runSteps = launcher.RunSteps
)

// Launch starts Hermes after the native plugin has been installed. Installation
// is intentionally explicit: a Hermes plugin can carry Python dependencies and
// an implicit install at agent launch would mutate its environment unexpectedly.
func Launch(_ context.Context, args []string, localEnv *local.LaunchEnv, _ io.Reader, _, _ io.Writer, _ *log.Logger, _ string) error {
	bin, err := lookPath("hermes")
	if err != nil {
		return fmt.Errorf("%w; install Hermes Agent, then run `agento11y hermes install`", ErrCLINotFound)
	}
	return launcher.Exec(execFn, bin, "hermes", args, localEnvironment(localEnv))
}

// Install installs and enables AgentO11y's native Hermes plugin. Hermes owns
// its plugin state and dependency installation, so this adapter does not write
// Hermes configuration files itself.
func Install(ctx context.Context, stdout io.Writer, _ *log.Logger) (bool, error) {
	bin, err := lookPath("hermes")
	if err != nil {
		return false, fmt.Errorf("%w; install Hermes Agent or run this in the developer's user context", ErrCLINotFound)
	}
	if err := runSteps(ctx, bin, stdout, [][]string{
		{"plugins", "install", pluginSource(), "--force"},
		{"plugins", "enable", PluginName},
	}); err != nil {
		return false, err
	}
	return true, nil
}

func pluginSource() string {
	if source := os.Getenv("AGENTO11Y_HERMES_PLUGIN_SOURCE"); source != "" {
		return source
	}
	return PluginSource
}

// localEnvironment adds the SDK protocol required by AgentO11y's HTTP local
// receiver. The Python SDK otherwise defaults to gRPC, which is correct for
// Cloud but cannot deliver generations to the local viewer.
func localEnvironment(localEnv *local.LaunchEnv) []string {
	env := local.Environ(localEnv)
	if localEnv == nil {
		return env
	}
	return withEnvironment(env, "AGENTO11Y_PROTOCOL", "http", "SIGIL_PROTOCOL", "http")
}

func withEnvironment(env []string, pairs ...string) []string {
	overrides := make(map[string]string, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		overrides[pairs[i]] = pairs[i+1]
	}
	out := make([]string, 0, len(env)+len(overrides))
	for _, entry := range env {
		key, _, found := strings.Cut(entry, "=")
		if found {
			if _, overridden := overrides[key]; overridden {
				continue
			}
		}
		out = append(out, entry)
	}
	for key, value := range overrides {
		out = append(out, key+"="+value)
	}
	return out
}
