//go:build !windows

package opencode

// Fixtures for the config/cache directory tests. An XDG override only counts
// when it is absolute, and os.UserHomeDir reads $HOME on these platforms.
const (
	homeEnvVar = "HOME"
	testHome   = "/home/user"

	testXDGConfigHome = "/custom/config"
	testXDGCacheHome  = "/custom/cache"

	wantXDGConfigDir  = "/custom/config/opencode"
	wantXDGCacheDir   = "/custom/cache/opencode"
	wantHomeConfigDir = "/home/user/.config/opencode"
	wantHomeCacheDir  = "/home/user/.cache/opencode"
)
