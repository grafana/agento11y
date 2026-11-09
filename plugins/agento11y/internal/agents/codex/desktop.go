package codex

import (
	"bytes"
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
// launching it or prompting. only the current key counts as installed: a legacy
// key goes through the update path, which migrates it, because the app has no
// launcher to run that migration later. changed reports whether the enabled key
// moved, read back from codex after the write.
func InstallDesktop(ctx context.Context, stdout io.Writer) (bool, error) {
	bin, err := desktopBinary()
	if err != nil {
		return false, err
	}
	current := PluginName + "@" + marketplaceAlias
	before, _ := enabledDesktopKey(ctx, bin)
	if before == current {
		return false, nil
	}
	write := runInstall
	if before != "" {
		write = runUpdate
	}
	if err := write(ctx, bin, stdout); err != nil {
		return false, err
	}
	after, err := enabledDesktopKey(ctx, bin)
	if err != nil {
		return false, fmt.Errorf("verify %s after install: %w", PluginName, err)
	}
	if after == "" {
		return false, fmt.Errorf("%s is not enabled after install", PluginName)
	}
	if after == before {
		return false, nil
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

// UninstallDesktop removes every key in pluginKeys that codex lists as
// installed, so a registration left under a pre-rename marketplace or name
// stops capturing too. the plugin is shared with the standalone cli, so this
// also stops capture there.
func UninstallDesktop(ctx context.Context, stdout io.Writer) error {
	bin, err := desktopBinary()
	if err != nil {
		return err
	}
	out, err := pluginList(ctx, bin)
	if err != nil {
		return fmt.Errorf("list codex plugins: %w", err)
	}
	var steps [][]string
	for _, key := range pluginKeys() {
		if keyInstalled(out, key) {
			steps = append(steps, []string{"plugin", "remove", key})
		}
	}
	if len(steps) == 0 {
		return nil
	}
	return runSteps(ctx, bin, stdout, steps)
}

// enabledDesktopKey returns the key of pluginKeys that codex marks installed
// and enabled, empty when none is.
func enabledDesktopKey(ctx context.Context, bin string) (string, error) {
	out, err := pluginList(ctx, bin)
	if err != nil {
		return "", err
	}
	return enabledKey(out), nil
}

// keyInstalled reports whether `codex plugin list` output lists the exact key
// as installed, enabled or not.
func keyInstalled(out []byte, key string) bool {
	needle := []byte(key)
	for line := range bytes.SplitSeq(out, []byte{'\n'}) {
		fields := bytes.Fields(line)
		if len(fields) == 0 || !bytes.Equal(fields[0], needle) {
			continue
		}
		if bytes.Contains(line, []byte("installed")) && !bytes.Contains(line, []byte("not installed")) {
			return true
		}
	}
	return false
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
