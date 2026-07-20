package fragment

import (
	"path/filepath"

	"github.com/grafana/agento11y/plugins/agento11y/internal/xdg"
)

// agentSubdir scopes copilot fragments under the shared agento11y state root
// so agent-side data layouts can evolve independently.
const agentSubdir = "copilot"

func StateRoot() string {
	return filepath.Join(xdg.AppStateRoot(), agentSubdir)
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

func safeComponent(raw string) string {
	return xdg.SafeComponent(raw)
}
