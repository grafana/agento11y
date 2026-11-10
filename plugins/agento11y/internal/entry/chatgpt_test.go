package entry

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/codex"
)

func withStubChatGPT(t *testing.T, install func(context.Context, io.Writer) (bool, error), uninstall func(context.Context, io.Writer) error) {
	t.Helper()
	prevInstall, prevUninstall := chatgptInstall, chatgptUninstall
	t.Cleanup(func() { chatgptInstall, chatgptUninstall = prevInstall, prevUninstall })
	chatgptInstall, chatgptUninstall = install, uninstall
}

func TestRun_ChatGPTInstallAndUninstallDispatch(t *testing.T) {
	installed, removed := false, false
	withStubChatGPT(t,
		func(context.Context, io.Writer) (bool, error) { installed = true; return true, nil },
		func(context.Context, io.Writer) error { removed = true; return nil },
	)

	var stdout, stderr bytes.Buffer
	gotExit := withExit(t, func() { run([]string{"chatgpt", "install", "--json"}, strings.NewReader(""), &stdout, &stderr) })
	require.Nil(t, gotExit, "stderr=%q", stderr.String())
	var result agentInstallResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &result), "stdout=%q", stdout.String())
	assert.True(t, installed)
	assert.Equal(t, agentInstallResult{Agent: "chatgpt", Status: "installed"}, result)

	gotExit = withExit(t, func() { run([]string{"chatgpt", "uninstall"}, strings.NewReader(""), &stdout, &stderr) })
	require.Nil(t, gotExit, "stderr=%q", stderr.String())
	assert.True(t, removed)
	assert.Empty(t, stderr.String())
}

func TestRun_ChatGPTInstallReportsMissingHost(t *testing.T) {
	withStubChatGPT(t,
		func(context.Context, io.Writer) (bool, error) { return false, codex.ErrDesktopNotFound },
		nil,
	)

	var stdout, stderr bytes.Buffer
	gotExit := withExit(t, func() { run([]string{"chatgpt", "install", "--json"}, strings.NewReader(""), &stdout, &stderr) })
	require.Nil(t, gotExit, "stderr=%q", stderr.String())
	var result agentInstallResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &result), "stdout=%q", stdout.String())
	assert.Equal(t, agentInstallResult{Agent: "chatgpt", Status: "missing_host"}, result)
}

func TestRun_ChatGPTUninstallRejectsArguments(t *testing.T) {
	withStubChatGPT(t, nil, func(context.Context, io.Writer) error {
		t.Fatal("uninstall must not run with extra arguments")
		return nil
	})

	var stdout, stderr bytes.Buffer
	gotExit := withExit(t, func() { run([]string{"chatgpt", "uninstall", "extra"}, strings.NewReader(""), &stdout, &stderr) })
	require.NotNil(t, gotExit)
	assert.Equal(t, 2, *gotExit)
}

func TestRun_ChatGPTUninstallReportsError(t *testing.T) {
	withStubChatGPT(t, nil, func(context.Context, io.Writer) error { return codex.ErrDesktopNotFound })

	var stdout, stderr bytes.Buffer
	gotExit := withExit(t, func() { run([]string{"chatgpt", "uninstall"}, strings.NewReader(""), &stdout, &stderr) })
	require.NotNil(t, gotExit)
	assert.Equal(t, 1, *gotExit)
	assert.Contains(t, stderr.String(), "ChatGPT desktop app not found")
}

func TestRun_ChatGPTWithoutCommandPrintsHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	withExit(t, func() { run([]string{"chatgpt"}, strings.NewReader(""), &stdout, &stderr) })
	out := stdout.String() + stderr.String()
	assert.Contains(t, out, "install")
	assert.Contains(t, out, "uninstall")
}
