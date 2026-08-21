//go:build windows

package install

// testBin is the fake path of the running binary the execpath seam reports,
// and wantHookCmd the hook command line execpath renders from it: on Windows
// the bare command name, resolved through PATH by the shell Cursor runs hooks
// with.
const (
	testBin     = `C:\Users\me\AppData\Local\Programs\sigil\sigil.exe`
	wantHookCmd = "sigil cursor hook"
)
