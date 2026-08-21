//go:build !windows

package install

// testBin is the fake path of the running binary the execpath seam reports,
// and wantHookCmd the hook command line execpath renders from it: on Unix the
// full path, shell-quoted when needed.
const (
	testBin     = "/opt/homebrew/bin/sigil"
	wantHookCmd = testBin + " cursor hook"
)
