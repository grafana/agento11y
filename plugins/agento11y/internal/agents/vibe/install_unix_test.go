//go:build !windows

package vibe

import "testing"

// On POSIX hosts execpath renders the hook command from the shell-quoted
// path of the running binary, so the installed command echoes the pinned
// executable in full.
const (
	fixtureExecutable  = "/usr/local/bin/agento11y"
	fixtureHookCommand = "/usr/local/bin/agento11y vibe hook"
)

func TestEnsureHookInstalled_QuotesExecutablePath(t *testing.T) {
	got := installedHookCommand(t, "/Users/Jane Doe/bin/agento11y")
	want := "'/Users/Jane Doe/bin/agento11y' vibe hook"
	if got != want {
		t.Errorf("command = %q, want %q", got, want)
	}
}
