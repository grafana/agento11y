//go:build windows

package vibe

import "testing"

// On Windows vibe runs hooks through cmd.exe or PowerShell, neither of which
// invokes a POSIX-quoted path, so execpath renders the bare executable name
// and lets PATH resolve it.
const (
	fixtureExecutable  = `C:\Program Files\agento11y\agento11y.exe`
	fixtureHookCommand = "agento11y vibe hook"
)

// The Windows counterpart of the POSIX quoting test: an install directory
// holding characters a shell would interpret must not reach the command,
// because the directory is dropped rather than quoted.
func TestEnsureHookInstalled_UsesBareExecutableName(t *testing.T) {
	got := installedHookCommand(t, `C:\Users\Jane Doe\bin\agento11y.exe`)
	want := "agento11y vibe hook"
	if got != want {
		t.Errorf("command = %q, want %q", got, want)
	}
}
