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

const (
	desktopEnabledList = "agento11y-codex@agento11y  installed, enabled  /tmp/plugins/agento11y-codex\n"
	desktopLegacyList  = "agento11y-codex@agento11y  not installed  /tmp/plugins/agento11y-codex\n" +
		"sigil-codex@grafana-sigil  installed, enabled  /tmp/plugins/sigil-codex\n"
)

func withDesktopApp(t *testing.T) {
	t.Helper()
	withLookPath(t, func(name string) (string, error) {
		if name != DesktopCLIPath {
			return "", exec.ErrNotFound
		}
		return name, nil
	})
}

// withPluginListSequence serves one `codex plugin list` output per call and
// repeats the last one, so a test can describe the store before and after a
// write.
func withPluginListSequence(t *testing.T, outputs ...string) {
	t.Helper()
	calls := 0
	withPluginList(t, func(_ context.Context, bin string) ([]byte, error) {
		assert.Equal(t, DesktopCLIPath, bin)
		out := outputs[min(calls, len(outputs)-1)]
		calls++
		return []byte(out), nil
	})
}

func failWrites(t *testing.T) {
	t.Helper()
	withRunInstall(t, func(context.Context, string, io.Writer) error {
		t.Fatal("install must not run")
		return nil
	})
	withRunDesktopUpdate(t, func(context.Context, string, io.Writer) error {
		t.Fatal("update must not run")
		return nil
	})
}

func withRunDesktopUpdate(t *testing.T, fn func(context.Context, string, io.Writer) error) {
	t.Helper()
	prev := runDesktopUpdate
	t.Cleanup(func() { runDesktopUpdate = prev })
	runDesktopUpdate = fn
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

func TestInstallDesktopSkipsWhenCurrentKeyEnabled(t *testing.T) {
	withDesktopApp(t)
	withPluginListSequence(t, desktopEnabledList)
	failWrites(t)

	changed, err := InstallDesktop(context.Background(), io.Discard)
	require.NoError(t, err)
	assert.False(t, changed)
}

func TestInstallDesktopRegistersPluginWithBundledCLI(t *testing.T) {
	withDesktopApp(t)
	withPluginListSequence(t, "", desktopEnabledList)
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

func TestInstallDesktopMigratesLegacyKey(t *testing.T) {
	withDesktopApp(t)
	withPluginListSequence(t, desktopLegacyList, desktopEnabledList)
	withRunInstall(t, func(context.Context, string, io.Writer) error {
		t.Fatal("a legacy install must go through the update path")
		return nil
	})
	updated := false
	withRunDesktopUpdate(t, func(_ context.Context, bin string, _ io.Writer) error {
		updated = true
		assert.Equal(t, DesktopCLIPath, bin)
		return nil
	})

	var out bytes.Buffer
	changed, err := InstallDesktop(context.Background(), &out)
	require.NoError(t, err)
	assert.True(t, updated)
	assert.True(t, changed)
	assert.Contains(t, out.String(), "Settings > Hooks")
}

func TestInstallDesktopMigrationPrintsOnlyDesktopTrustHint(t *testing.T) {
	withDesktopApp(t)
	withPluginListSequence(t, desktopLegacyList, desktopEnabledList)
	var steps [][]string
	withRunSteps(t, func(_ context.Context, _ string, _ io.Writer, s [][]string) error {
		steps = append(steps, s...)
		return nil
	})
	withRunFirst(t, func(context.Context, string, io.Writer, [][]string) (int, error) { return 0, nil })
	withRunOutput(t, func(context.Context, string, ...string) ([]byte, error) { return nil, nil })

	var out bytes.Buffer
	changed, err := InstallDesktop(context.Background(), &out)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, [][]string{{"plugin", "marketplace", "upgrade"}}, steps)
	assert.Contains(t, out.String(), "Settings > Hooks")
	assert.NotContains(t, out.String(), "/hooks inside codex")
}

func TestInstallDesktopReportsUnchangedWhenSnapshotStillLegacy(t *testing.T) {
	withDesktopApp(t)
	withPluginListSequence(t, desktopLegacyList)
	withRunDesktopUpdate(t, func(context.Context, string, io.Writer) error { return nil })

	var out bytes.Buffer
	changed, err := InstallDesktop(context.Background(), &out)
	require.NoError(t, err)
	assert.False(t, changed)
	assert.NotContains(t, out.String(), "Settings > Hooks")
}

func TestInstallDesktopFailsWhenNothingEnabledAfterInstall(t *testing.T) {
	withDesktopApp(t)
	withPluginListSequence(t, "")
	withRunInstall(t, func(context.Context, string, io.Writer) error { return nil })

	changed, err := InstallDesktop(context.Background(), io.Discard)
	assert.False(t, changed)
	assert.ErrorContains(t, err, "not enabled after install")
}

func TestInstallDesktopReturnsInstallError(t *testing.T) {
	withDesktopApp(t)
	withPluginListSequence(t, "")
	boom := errors.New("marketplace add failed")
	withRunInstall(t, func(context.Context, string, io.Writer) error { return boom })

	var out bytes.Buffer
	changed, err := InstallDesktop(context.Background(), &out)
	assert.False(t, changed)
	assert.ErrorIs(t, err, boom)
	assert.NotContains(t, out.String(), "Settings > Hooks")
}

func TestUninstallDesktopRemovesEveryInstalledKey(t *testing.T) {
	withDesktopApp(t)
	withPluginListSequence(t,
		"agento11y-codex@agento11y  installed, enabled  /tmp/a\n"+
			"agento11y-codex@grafana-sigil  installed, disabled  /tmp/b\n"+
			"sigil-codex@grafana-sigil  not installed  /tmp/c\n"+
			"my-agento11y-codex@agento11y  installed, enabled  /tmp/d\n")
	var gotSteps [][]string
	withRunSteps(t, func(_ context.Context, bin string, _ io.Writer, steps [][]string) error {
		assert.Equal(t, DesktopCLIPath, bin)
		gotSteps = steps
		return nil
	})

	require.NoError(t, UninstallDesktop(context.Background(), io.Discard))
	assert.Equal(t, [][]string{
		{"plugin", "remove", "agento11y-codex@agento11y"},
		{"plugin", "remove", "agento11y-codex@grafana-sigil"},
	}, gotSteps)
}

func TestUninstallDesktopRemovesLegacyOnlyInstall(t *testing.T) {
	withDesktopApp(t)
	withPluginListSequence(t, desktopLegacyList)
	var gotSteps [][]string
	withRunSteps(t, func(_ context.Context, _ string, _ io.Writer, steps [][]string) error {
		gotSteps = steps
		return nil
	})

	require.NoError(t, UninstallDesktop(context.Background(), io.Discard))
	assert.Equal(t, [][]string{{"plugin", "remove", "sigil-codex@grafana-sigil"}}, gotSteps)
}

func TestUninstallDesktopNoopWhenNothingInstalled(t *testing.T) {
	withDesktopApp(t)
	withPluginListSequence(t, "agento11y-codex@agento11y  not installed  /tmp/a\n")
	withRunSteps(t, func(context.Context, string, io.Writer, [][]string) error {
		t.Fatal("nothing to remove")
		return nil
	})

	require.NoError(t, UninstallDesktop(context.Background(), io.Discard))
}

func TestDesktopStatusReadsPluginList(t *testing.T) {
	withDesktopApp(t)
	withPluginListSequence(t, desktopEnabledList)

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
