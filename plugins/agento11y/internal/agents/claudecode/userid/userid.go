// Package userid resolves the Claude Code user identity attached to every
// emitted generation.
//
// It is a leaf package on purpose. The live hook and the history importer must
// attribute turns identically, but the importer lives under internal/history,
// which internal/local imports, and the claudecode package imports
// internal/local. Keeping the resolver here lets both call it without that
// cycle.
package userid

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/text/unicode/norm"

	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
)

// Resolve returns the user id to attach to every emitted generation.
// The branded USER_ID family wins when set to a non-whitespace value;
// otherwise we read the selected local account file using the field selected by the
// USER_ID_SOURCE family (default "email", falling back to "email" on any
// unrecognized value). Any failure resolves to "" — telemetry is best-effort.
func Resolve() string {
	if v := envconfig.Getenv("USER_ID"); v != "" {
		return v
	}

	source := envconfig.Getenv("USER_ID_SOURCE")

	configDir, configured := os.LookupEnv("CLAUDE_CONFIG_DIR")
	if !configured {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		configDir = filepath.Join(home, ".claude")
	}

	legacy := filepath.Join(norm.NFC.String(configDir), ".config.json")
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		return loadFromClaudeJSON(legacy, source)
	}

	accountDir := configDir
	if !configured || configDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		accountDir = home
	}
	filename := ".claude.json"
	if os.Getenv("CLAUDE_CODE_CUSTOM_OAUTH_URL") != "" {
		filename = ".claude-custom-oauth.json"
	}
	return loadFromClaudeJSON(filepath.Join(accountDir, filename), source)
}

// loadFromClaudeJSON reads the selected account file and returns the selected
// oauthAccount field. Unknown sources fall back to "email". Returns "" on any
// error (missing file, malformed JSON, missing field). A malformed file is
// surfaced to stderr — mirrors state.Load for the same failure class and
// helps users diagnose why their generations are missing a user id.
func loadFromClaudeJSON(path, source string) string {
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
		fmt.Fprintln(os.Stderr, "agento11y claude-code: malformed selected account file, cannot resolve user id")
		return ""
	}

	switch source {
	case "accountUuid":
		return parsed.OAuthAccount.AccountUUID
	default:
		return parsed.OAuthAccount.EmailAddress
	}
}
