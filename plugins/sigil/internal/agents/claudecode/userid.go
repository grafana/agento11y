package claudecode

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// resolveUserID returns the user id to attach to every emitted generation.
// SIGIL_USER_ID wins when set to a non-whitespace value; otherwise we read
// ~/.claude.json using the field selected by SIGIL_USER_ID_SOURCE (default
// "email", falling back to "email" on any unrecognized value).
// Any failure resolves to "" — telemetry is best-effort.
func resolveUserID() string {
	if v := strings.TrimSpace(os.Getenv("SIGIL_USER_ID")); v != "" {
		return v
	}

	source := strings.TrimSpace(os.Getenv("SIGIL_USER_ID_SOURCE"))

	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return loadUserIDFromClaudeJSON(filepath.Join(home, ".claude.json"), source)
}

// loadUserIDFromClaudeJSON reads ~/.claude.json and returns the selected
// oauthAccount field. Unknown sources fall back to "email". Returns "" on any
// error (missing file, malformed JSON, missing field). A malformed file is
// surfaced to stderr — mirrors state.Load for the same failure class and
// helps users diagnose why their generations are missing a user id.
func loadUserIDFromClaudeJSON(path, source string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}

	var parsed struct {
		OAuthAccount struct {
			EmailAddress string `json:"emailAddress"`
			AccountUUID  string `json:"accountUuid"`
		} `json:"oauthAccount"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		fmt.Fprintln(os.Stderr, "sigil claude-code: malformed ~/.claude.json, cannot resolve user id:", err)
		return ""
	}

	switch source {
	case "accountUuid":
		return parsed.OAuthAccount.AccountUUID
	default:
		return parsed.OAuthAccount.EmailAddress
	}
}
