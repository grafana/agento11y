//go:build windows

package opencode

// Fixtures for the config/cache directory tests. A Unix-style path is not
// absolute on Windows, so the XDG overrides need a drive letter, and
// os.UserHomeDir reads %USERPROFILE% here.
const (
	homeEnvVar = "USERPROFILE"
	testHome   = `C:\Users\user`

	testXDGConfigHome = `C:\custom\config`
	testXDGCacheHome  = `C:\custom\cache`

	wantXDGConfigDir  = `C:\custom\config\opencode`
	wantXDGCacheDir   = `C:\custom\cache\opencode`
	wantHomeConfigDir = `C:\Users\user\.config\opencode`
	wantHomeCacheDir  = `C:\Users\user\.cache\opencode`
)
