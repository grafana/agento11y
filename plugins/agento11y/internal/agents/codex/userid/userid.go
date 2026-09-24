// Package userid resolves the Codex user identity attached to every
// emitted generation.
//
// Codex hook payloads carry no user field. The live hook and the history
// importer must attribute turns the same way, so both call Resolve here:
// AGENTO11Y_USER_ID (or SIGIL_USER_ID) wins, otherwise the signed-in ChatGPT
// email is read from $CODEX_HOME/auth.json (default ~/.codex/auth.json).
// Any failure resolves to "" — telemetry is best-effort, and hooks must not
// write to stderr.
package userid

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
)

// Resolve returns the user id to attach to every emitted generation.
// The branded USER_ID family wins when set to a non-whitespace value;
// otherwise we read the ChatGPT account email from Codex's auth cache.
func Resolve() string {
	if v := envconfig.Getenv("USER_ID"); v != "" {
		return v
	}
	return loadFromAuthJSON(authPath())
}

func authPath() string {
	if home := strings.TrimSpace(os.Getenv("CODEX_HOME")); home != "" {
		return filepath.Join(home, "auth.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex", "auth.json")
}

// authFile is the subset of $CODEX_HOME/auth.json that can name a user.
// tokens.id_token is a JWT; agent_identity is either a JWT string or a
// record with an email field. The rest of the file (access tokens, API
// keys) is ignored and never logged.
type authFile struct {
	Tokens *struct {
		IDToken string `json:"id_token"`
	} `json:"tokens"`
	AgentIdentity json.RawMessage `json:"agent_identity"`
}

type agentIdentityRecord struct {
	Email string `json:"email"`
}

func loadFromAuthJSON(path string) string {
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var parsed authFile
	if err := json.Unmarshal(data, &parsed); err != nil {
		return ""
	}
	if parsed.Tokens != nil {
		if email := emailFromJWT(parsed.Tokens.IDToken); email != "" {
			return email
		}
	}
	return emailFromAgentIdentity(parsed.AgentIdentity)
}

func emailFromAgentIdentity(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var jwt string
	if err := json.Unmarshal(raw, &jwt); err == nil {
		return emailFromJWT(jwt)
	}
	var rec agentIdentityRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return ""
	}
	return strings.TrimSpace(rec.Email)
}

// idTokenClaims is the email-bearing subset of a ChatGPT id_token payload.
// Codex itself reads `email` and `https://api.openai.com/profile`.email.
type idTokenClaims struct {
	Email   string `json:"email"`
	Profile struct {
		Email string `json:"email"`
	} `json:"https://api.openai.com/profile"`
}

func emailFromJWT(jwt string) string {
	jwt = strings.TrimSpace(jwt)
	parts := strings.Split(jwt, ".")
	if len(parts) < 2 || parts[1] == "" {
		return ""
	}
	payload, err := decodeJWTPayload(parts[1])
	if err != nil {
		return ""
	}
	var claims idTokenClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	if email := strings.TrimSpace(claims.Email); email != "" {
		return email
	}
	return strings.TrimSpace(claims.Profile.Email)
}

func decodeJWTPayload(segment string) ([]byte, error) {
	if raw, err := base64.RawURLEncoding.DecodeString(segment); err == nil {
		return raw, nil
	}
	return base64.URLEncoding.DecodeString(segment)
}
