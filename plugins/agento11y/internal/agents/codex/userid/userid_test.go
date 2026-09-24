package userid

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func testJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func writeAuthJSON(t *testing.T, dir, contents string) string {
	t.Helper()
	path := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestLoadFromAuthJSON(t *testing.T) {
	idToken := testJWT(t, map[string]any{"email": "chatgpt@example.com"})
	profileToken := testJWT(t, map[string]any{
		"https://api.openai.com/profile": map[string]any{"email": "profile@example.com"},
	})
	agentJWT := testJWT(t, map[string]any{"email": "agent-jwt@example.com"})

	tests := []struct {
		name     string
		contents string
		writeFix bool
		want     string
	}{
		{
			name:     "id_token email",
			writeFix: true,
			contents: `{"tokens":{"id_token":"` + idToken + `","access_token":"secret"}}`,
			want:     "chatgpt@example.com",
		},
		{
			name:     "id_token profile email",
			writeFix: true,
			contents: `{"tokens":{"id_token":"` + profileToken + `"}}`,
			want:     "profile@example.com",
		},
		{
			name:     "agent_identity record email",
			writeFix: true,
			contents: `{"agent_identity":{"email":"agent@example.com","chatgpt_user_id":"u-1"}}`,
			want:     "agent@example.com",
		},
		{
			name:     "agent_identity JWT email",
			writeFix: true,
			contents: `{"agent_identity":"` + agentJWT + `"}`,
			want:     "agent-jwt@example.com",
		},
		{
			name:     "id_token wins over agent_identity",
			writeFix: true,
			contents: `{"tokens":{"id_token":"` + idToken + `"},"agent_identity":{"email":"agent@example.com"}}`,
			want:     "chatgpt@example.com",
		},
		{
			name:     "agent_identity used when id_token has no email",
			writeFix: true,
			contents: `{"tokens":{"id_token":"` + testJWT(t, map[string]any{"sub": "no-email"}) + `"},"agent_identity":{"email":"agent@example.com"}}`,
			want:     "agent@example.com",
		},
		{
			name:     "missing file",
			writeFix: false,
			want:     "",
		},
		{
			name:     "malformed JSON",
			writeFix: true,
			contents: `{"tokens":`,
			want:     "",
		},
		{
			name:     "empty object",
			writeFix: true,
			contents: `{}`,
			want:     "",
		},
		{
			name:     "empty email fields",
			writeFix: true,
			contents: `{"tokens":{"id_token":"` + testJWT(t, map[string]any{"email": "  "}) + `"},"agent_identity":{"email":"   "}}`,
			want:     "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "auth.json")
			if tt.writeFix {
				writeAuthJSON(t, dir, tt.contents)
			}
			got := loadFromAuthJSON(path)
			if got != tt.want {
				t.Errorf("loadFromAuthJSON = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolve(t *testing.T) {
	idToken := testJWT(t, map[string]any{"email": "chatgpt@example.com"})
	authJSON := `{"tokens":{"id_token":"` + idToken + `"}}`

	tests := []struct {
		name      string
		envUserID string
		writeFix  bool
		contents  string
		useHome   bool
		want      string
	}{
		{
			name:      "USER_ID wins over file",
			envUserID: "foo",
			writeFix:  true,
			contents:  authJSON,
			want:      "foo",
		},
		{
			name:      "USER_ID wins when no file exists",
			envUserID: "foo",
			want:      "foo",
		},
		{
			name:      "USER_ID trimmed",
			envUserID: "  alex@example.com  ",
			want:      "alex@example.com",
		},
		{
			name:      "whitespace-only USER_ID falls through",
			envUserID: "   ",
			writeFix:  true,
			contents:  authJSON,
			want:      "chatgpt@example.com",
		},
		{
			name:     "CODEX_HOME auth.json",
			writeFix: true,
			contents: authJSON,
			want:     "chatgpt@example.com",
		},
		{
			name:     "HOME/.codex/auth.json when CODEX_HOME is unset",
			writeFix: true,
			contents: authJSON,
			useHome:  true,
			want:     "chatgpt@example.com",
		},
		{
			name:     "missing file returns empty",
			writeFix: false,
			want:     "",
		},
		{
			name:     "malformed file returns empty",
			writeFix: true,
			contents: `{broken`,
			want:     "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("SIGIL_USER_ID", tt.envUserID)
			t.Setenv("AGENTO11Y_USER_ID", "")

			if tt.useHome {
				t.Setenv("CODEX_HOME", "")
				if tt.writeFix {
					if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o700); err != nil {
						t.Fatalf("mkdir: %v", err)
					}
					writeAuthJSON(t, filepath.Join(home, ".codex"), tt.contents)
				}
			} else {
				codexHome := t.TempDir()
				t.Setenv("CODEX_HOME", codexHome)
				if tt.writeFix {
					writeAuthJSON(t, codexHome, tt.contents)
				}
			}

			got := Resolve()
			if got != tt.want {
				t.Errorf("Resolve() = %q, want %q", got, tt.want)
			}
		})
	}
}
