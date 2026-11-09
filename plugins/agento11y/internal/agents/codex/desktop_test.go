package codex

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agentinstall"
)

const desktopEnabledList = "agento11y-codex@agento11y  installed, enabled  /tmp/plugins/agento11y-codex\n"

func withDesktopApp(t *testing.T) {
	t.Helper()
	withLookPath(t, func(name string) (string, error) {
		if name != DesktopCLIPath {
			return "", exec.ErrNotFound
		}
		return name, nil
	})
}

func TestDesktopMissingAppReportsMissingHost(t *testing.T) {
	withLookPath(t, func(string) (string, error) { return "", exec.ErrNotFound })

	changed, err := InstallDesktop(context.Background(), io.Discard)
	assert.False(t, changed)
	assert.ErrorIs(t, err, ErrDesktopNotFound)

	assert.ErrorIs(t, UninstallDesktop(context.Background(), io.Discard), ErrDesktopNotFound)

	_, _, err = DesktopStatus(context.Background())
	assert.ErrorIs(t, err, ErrDesktopNotFound)
}

func TestInstallDesktopSkipsWhenPluginEnabled(t *testing.T) {
	withDesktopApp(t)
	withPluginList(t, func(_ context.Context, bin string) ([]byte, error) {
		assert.Equal(t, DesktopCLIPath, bin)
		return []byte(desktopEnabledList), nil
	})
	withRunInstall(t, func(context.Context, string, io.Writer) error {
		t.Fatal("install must not run when the plugin is already enabled")
		return nil
	})

	changed, err := InstallDesktop(context.Background(), io.Discard)
	require.NoError(t, err)
	assert.False(t, changed)
}

func TestInstallDesktopRegistersPluginWithBundledCLI(t *testing.T) {
	withDesktopApp(t)
	withPluginList(t, func(context.Context, string) ([]byte, error) { return nil, nil })
	var installedWith string
	withRunInstall(t, func(_ context.Context, bin string, _ io.Writer) error {
		installedWith = bin
		return nil
	})

	var out bytes.Buffer
	changed, err := InstallDesktop(context.Background(), &out)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, DesktopCLIPath, installedWith)
	assert.Contains(t, out.String(), "Settings > Hooks")
	assert.Contains(t, out.String(), PluginName)
}

func TestInstallDesktopReturnsInstallError(t *testing.T) {
	withDesktopApp(t)
	withPluginList(t, func(context.Context, string) ([]byte, error) { return nil, nil })
	boom := errors.New("marketplace add failed")
	withRunInstall(t, func(context.Context, string, io.Writer) error { return boom })

	var out bytes.Buffer
	changed, err := InstallDesktop(context.Background(), &out)
	assert.False(t, changed)
	assert.ErrorIs(t, err, boom)
	assert.NotContains(t, out.String(), "Settings > Hooks")
}

func TestUninstallDesktopRemovesPlugin(t *testing.T) {
	withDesktopApp(t)
	var gotBin string
	var gotSteps [][]string
	withRunSteps(t, func(_ context.Context, bin string, _ io.Writer, steps [][]string) error {
		gotBin, gotSteps = bin, steps
		return nil
	})

	require.NoError(t, UninstallDesktop(context.Background(), io.Discard))
	assert.Equal(t, DesktopCLIPath, gotBin)
	assert.Equal(t, [][]string{{"plugin", "remove", "agento11y-codex@agento11y"}}, gotSteps)
}

func TestDesktopStatusReadsPluginList(t *testing.T) {
	withDesktopApp(t)
	withPluginList(t, func(context.Context, string) ([]byte, error) { return []byte(desktopEnabledList), nil })

	installed, version, err := DesktopStatus(context.Background())
	require.NoError(t, err)
	assert.True(t, installed)
	assert.Empty(t, version)
}

func TestDesktopInstallerRegistered(t *testing.T) {
	var spec agentinstall.Spec
	for _, s := range agentinstall.All() {
		if s.Name == "chatgpt" {
			spec = s
		}
	}
	require.Equal(t, "chatgpt", spec.Name)
	assert.True(t, spec.IsMissingHost(ErrDesktopNotFound))
	assert.False(t, spec.IsMissingHost(errors.New("other")))
}
