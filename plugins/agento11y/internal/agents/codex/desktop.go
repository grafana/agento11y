package codex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agentinstall"
)

// DesktopCLIPath is the codex cli bundled inside the chatgpt desktop app. the
// app is the codex app renamed: it reads the same ~/.codex as the standalone
// cli, so a plugin added through this binary is the one the app loads. the app
// does not put it on PATH, which is why the codex launcher cannot find it.
const DesktopCLIPath = "/Applications/ChatGPT.app/Contents/Resources/codex-cli/bin/codex"

// ErrDesktopNotFound means the chatgpt desktop app is not installed for this
// user. managed deployment tooling can treat this as a skipped agent.
var ErrDesktopNotFound = errors.New("ChatGPT desktop app not found")

func init() {
	agentinstall.Register(agentinstall.Spec{
		Name: "chatgpt",
		Install: func(ctx context.Context, stdout io.Writer, _ *log.Logger) (bool, error) {
			return InstallDesktop(ctx, stdout)
		},
		IsMissingHost: func(err error) bool { return errors.Is(err, ErrDesktopNotFound) },
	})
}

// InstallDesktop registers the codex plugin for the chatgpt desktop app without
// launching it or prompting. changed is false when the plugin is already
// installed and enabled.
func InstallDesktop(ctx context.Context, stdout io.Writer) (bool, error) {
	bin, err := desktopBinary()
	if err != nil {
		return false, err
	}
	if installed, err := pluginInstalled(ctx, bin); err == nil && installed {
		return false, nil
	}
	if err := runInstall(ctx, bin, stdout); err != nil {
		return false, err
	}
	// the app skips a new plugin's hooks until the user trusts them, the same
	// review the cli asks for under /hooks.
	fmt.Fprintf(stdout,
		"agento11y: in ChatGPT, open Settings > Hooks and trust the %s hooks,\n"+
			"           then start a new Codex thread. Run `agento11y login` first if\n"+
			"           you have not chosen where sessions go.\n",
		PluginName)
	return true, nil
}

// UninstallDesktop removes the codex plugin through the desktop app's cli. the
// plugin is shared with the standalone cli, so this also stops capture there.
func UninstallDesktop(ctx context.Context, stdout io.Writer) error {
	bin, err := desktopBinary()
	if err != nil {
		return err
	}
	return runSteps(ctx, bin, stdout, [][]string{{"plugin", "remove", PluginName + "@" + marketplaceAlias}})
}

// DesktopStatus reports whether the codex plugin is installed and enabled for
// the chatgpt desktop app. like Status, it only reads plugin state.
func DesktopStatus(ctx context.Context) (installed bool, version string, err error) {
	bin, err := desktopBinary()
	if err != nil {
		return false, "", err
	}
	installed, err = pluginInstalled(ctx, bin)
	return installed, "", err
}

func desktopBinary() (string, error) {
	bin, err := lookPath(DesktopCLIPath)
	if err != nil {
		return "", fmt.Errorf("%w at %s", ErrDesktopNotFound, DesktopCLIPath)
	}
	return bin, nil
}
