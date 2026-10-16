//go:build windows

package kiro

// On Windows the hook command names the bare executable and PATH resolves it,
// so the directory with spaces must not reach the command at all.
const (
	fixtureExecutable  = `C:\path with spaces\agento11y.exe`
	fixtureHookCommand = "agento11y kiro hook"
)
