package fragment

import (
	"path/filepath"

	"github.com/grafana/agento11y/plugins/agento11y/internal/xdg"
)

const appName = "sigil"

// agentSubdir scopes codex fragments under the shared sigil state root so
// agent-side data layouts can evolve independently.
const agentSubdir = "codex"

func StateRoot() string {
	return filepath.Join(xdg.StateRoot(appName), agentSubdir)
}

func TurnsDir(sessionID string) string {
	return filepath.Join(StateRoot(), "turns", safeComponent(sessionID))
}

func SessionsDir() string {
	return filepath.Join(StateRoot(), "sessions")
}

func SubagentLinksDir() string {
	return filepath.Join(StateRoot(), "subagents")
}

func SessionFilePath(sessionID string) string {
	return filepath.Join(SessionsDir(), safeComponent(sessionID)+".json")
}

func SubagentLinkFilePath(childSessionID string) string {
	return filepath.Join(SubagentLinksDir(), safeComponent(childSessionID)+".json")
}

func FragmentFilePath(sessionID, turnID string) string {
	return filepath.Join(TurnsDir(sessionID), safeComponent(turnID)+".json")
}

func safeComponent(raw string) string {
	return xdg.SafeComponent(raw)
}
