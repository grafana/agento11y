// Package execpath renders hook command lines that invoke the current
// executable rather than a hardcoded binary name, so hooks generated at
// install time keep working no matter which command name (agento11y, or the
// legacy sigil) the user installed.
package execpath

import (
	"fmt"
	"os"
	"runtime"
	"strings"
)

// Executable is a test seam for os.Executable. The path is deliberately NOT
// resolved through symlinks, so the stable ~/.local/bin/agento11y or
// Homebrew symlink is recorded rather than a versioned Cellar path that
// changes on upgrade.
var Executable = os.Executable

// goos is a test seam for runtime.GOOS.
var goos = runtime.GOOS

// HookCommand returns "<shell-quoted executable> <suffix>", the command line
// a host agent's hook config should run to reach this binary. On Windows the
// executable's bare name is used instead, resolved via PATH: hook hosts there
// run commands through PowerShell or cmd.exe, and neither invokes a
// POSIX-quoted path.
func HookCommand(suffix string) (string, error) {
	bin, err := Executable()
	if err != nil {
		return "", fmt.Errorf("resolve executable path: %w", err)
	}
	if goos == "windows" {
		return strings.TrimSuffix(windowsBase(bin), ".exe") + " " + suffix, nil
	}
	return Quote(bin) + " " + suffix, nil
}

// windowsBase returns the last element of a Windows path, which may use
// either backslash or forward-slash separators.
func windowsBase(p string) string {
	if i := strings.LastIndexAny(p, `\/`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// Quote single-quotes s for POSIX shells when it contains characters a
// shell would interpret, and returns it unchanged otherwise.
func Quote(s string) string {
	if !needsQuote(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// needsQuote reports whether s contains anything outside the set of
// characters a shell leaves untouched in an unquoted word.
func needsQuote(s string) bool {
	if s == "" {
		return true
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("@%+=:,./-_", r):
		default:
			return true
		}
	}
	return false
}
