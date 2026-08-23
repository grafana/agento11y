//go:build windows

package copilot

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// On Windows the hook command is the bare executable name resolved via PATH:
// Copilot runs hooks through PowerShell or cmd.exe, neither of which invokes a
// POSIX-quoted path.
const (
	hookExecPath    = `C:\Program Files\agento11y\agento11y.exe`
	hookCommandLine = "agento11y copilot hook"
)

// The Windows counterpart of the POSIX quoting rule: a path containing spaces
// never reaches the hook command, because only the extension-less base name is
// written and the shell resolves it through PATH.
func TestWriteUserHooks_UsesBareExecutableName(t *testing.T) {
	t.Setenv("COPILOT_HOME", t.TempDir())
	withExecutable(t, `C:\Users\Jane Doe\bin\agento11y.exe`)

	path, wrote, err := writeUserHooks()
	require.NoError(t, err)
	assert.True(t, wrote)
	assertValidUserHooks(t, path, "agento11y copilot hook")
}
