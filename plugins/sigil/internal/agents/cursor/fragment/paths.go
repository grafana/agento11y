package fragment

import (
	"path/filepath"
	"strings"

	"github.com/grafana/sigil-sdk/plugins/sigil/internal/xdg"
)

const (
	appName        = "sigil"
	agentSubdir    = "cursor"
	fragmentPrefix = "gen-"
	fragmentSuffix = ".json"
)

// StateRoot returns the root state directory for the cursor adapter.
// Scoped under the shared sigil state root so agent data layouts can evolve
// independently.
func StateRoot() string {
	return filepath.Join(xdg.StateRoot(appName), agentSubdir)
}

// ConversationDir is the directory holding all fragments for one conversation.
// The conversation ID is routed through xdg.SafeComponent so a malformed
// payload can't escape the state root via "../" or embedded separators.
func ConversationDir(conversationID string) string {
	return filepath.Join(StateRoot(), xdg.SafeComponent(conversationID))
}

// SessionFilePath is where session metadata is stored after sessionStart.
func SessionFilePath(conversationID string) string {
	return filepath.Join(ConversationDir(conversationID), "session.json")
}

// FragmentFilePath is the JSON file for one accumulating generation. The
// generation ID is sanitised the same way as the conversation ID.
func FragmentFilePath(conversationID, generationID string) string {
	return filepath.Join(
		ConversationDir(conversationID),
		fragmentPrefix+xdg.SafeComponent(generationID)+fragmentSuffix,
	)
}

// ParseFragmentFilename returns the safe-component slug between
// fragmentPrefix and fragmentSuffix, or "" if the entry isn't a fragment.
//
// The returned slug is NOT the original generation ID — FragmentFilePath
// routes IDs through xdg.SafeComponent, which is not reversible. Callers
// that need the original ID must read it from the fragment JSON
// (see ListFragmentIDs).
func ParseFragmentFilename(entry string) string {
	if !strings.HasPrefix(entry, fragmentPrefix) || !strings.HasSuffix(entry, fragmentSuffix) {
		return ""
	}
	return entry[len(fragmentPrefix) : len(entry)-len(fragmentSuffix)]
}
