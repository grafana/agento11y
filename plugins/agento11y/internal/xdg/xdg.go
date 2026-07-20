// Package xdg resolves XDG base directory paths for the agento11y plugin
// binary. All paths land under a single appName-suffixed directory so all
// agents share state and logs.
package xdg

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var unsafePath = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

const (
	// appName is the preferred state directory name.
	appName = "agento11y"
	// legacyAppName is the pre-rename state directory, still used when it is
	// the only one present so existing installs keep their fragments, local
	// data, and update stamps. The directory is never moved or copied.
	legacyAppName = "sigil"
)

// AppStateRoot returns the launcher state root:
// $XDG_STATE_HOME/agento11y if that directory exists, otherwise the legacy
// $XDG_STATE_HOME/sigil if that exists, otherwise the new path (with the
// usual state-root fallbacks). Preferring the new path when both exist
// mirrors the config.env resolution in the dotenv package.
func AppStateRoot() string {
	preferred := StateRoot(appName)
	if _, err := os.Stat(preferred); err == nil {
		return preferred
	}
	legacy := StateRoot(legacyAppName)
	if _, err := os.Stat(legacy); err == nil {
		return legacy
	}
	return preferred
}

// StateRoot returns the root state directory.
// Honors XDG_STATE_HOME, falls back to $HOME/.local/state, then OS tempdir.
func StateRoot(appName string) string {
	if x := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); x != "" {
		if filepath.IsAbs(x) {
			return filepath.Join(x, appName)
		}
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" || !filepath.IsAbs(home) {
		return filepath.Join(os.TempDir(), appName)
	}
	return filepath.Join(home, ".local", "state", appName)
}

// ConfigRoot returns the root config directory.
// Honors XDG_CONFIG_HOME, falls back to $HOME/.config, then OS tempdir.
func ConfigRoot(appName string) string {
	if x := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); x != "" {
		if filepath.IsAbs(x) {
			return filepath.Join(x, appName)
		}
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" || !filepath.IsAbs(home) {
		return filepath.Join(os.TempDir(), appName)
	}
	return filepath.Join(home, ".config", appName)
}

// LogFilePath is where AGENTO11Y_DEBUG=true (SIGIL_DEBUG fallback) writes
// its log. The file name is always agento11y.log even when AppStateRoot
// resolves to the legacy sigil directory.
func LogFilePath() string {
	return filepath.Join(AppStateRoot(), "logs", appName+".log")
}

// ConfigFilePath returns the path to a dotenv config file.
func ConfigFilePath(appName, name string) string {
	return filepath.Join(ConfigRoot(appName), name)
}

// SafeComponent sanitises a raw string for use as a filesystem component
// by replacing unsafe characters with underscores and appending a short
// hash suffix for collision resistance.
func SafeComponent(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "unknown"
	}
	sum := sha256.Sum256([]byte(raw))
	suffix := hex.EncodeToString(sum[:])[:12]
	safe := unsafePath.ReplaceAllString(trimmed, "_")
	safe = strings.Trim(safe, "._-")
	if safe == "" {
		safe = "unknown"
	}
	maxPrefix := 120 - len(suffix) - 1
	if len(safe) > maxPrefix {
		safe = safe[:maxPrefix]
	}
	return safe + "-" + suffix
}
