//go:build !windows

package copilot

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// On POSIX hosts the hook command embeds the absolute executable path, so the
// hooks keep reaching this binary even when it is not on the host agent's PATH.
const (
	hookExecPath    = "/usr/local/bin/agento11y"
	hookCommandLine = "/usr/local/bin/agento11y copilot hook"
)

// The generated hook command must shell-quote executable paths a shell would
// otherwise split or interpret.
func TestWriteUserHooks_QuotesExecutablePath(t *testing.T) {
	t.Setenv("COPILOT_HOME", t.TempDir())
	withExecutable(t, "/Users/Jane Doe/bin/agento11y")

	path, wrote, err := writeUserHooks()
	require.NoError(t, err)
	assert.True(t, wrote)
	assertValidUserHooks(t, path, "'/Users/Jane Doe/bin/agento11y' copilot hook")
}
