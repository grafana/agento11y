package fragment

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const appDir = "sigil-copilot"

var unsafePath = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func StateRoot() string {
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		if filepath.IsAbs(x) {
			return filepath.Join(x, appDir)
		}
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" || !filepath.IsAbs(home) {
		return filepath.Join(os.TempDir(), appDir)
	}
	return filepath.Join(home, ".local", "state", appDir)
}

func TurnsDir(sessionID string) string {
	return filepath.Join(StateRoot(), "turns", safeComponent(sessionID))
}

func SessionsDir() string {
	return filepath.Join(StateRoot(), "sessions")
}

func SessionFilePath(sessionID string) string {
	return filepath.Join(SessionsDir(), safeComponent(sessionID)+".json")
}

func FragmentFilePath(sessionID, turnID string) string {
	return filepath.Join(TurnsDir(sessionID), safeComponent(turnID)+".json")
}

func LogFilePath() string {
	return filepath.Join(StateRoot(), "logs", "sigil-copilot.log")
}

func safeComponent(raw string) string {
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
