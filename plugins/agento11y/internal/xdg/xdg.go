// Package xdg resolves XDG base directory paths for the sigil plugin
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

// LogFilePath is where SIGIL_DEBUG=true writes its log.
func LogFilePath(appName string) string {
	return filepath.Join(StateRoot(appName), "logs", appName+".log")
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
