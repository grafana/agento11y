//go:build !windows

package kiro

// The space in the directory checks that the hook command quotes the path.
const (
	fixtureExecutable  = "/path with spaces/agento11y"
	fixtureHookCommand = "'/path with spaces/agento11y' kiro hook"
)
