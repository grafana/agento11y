package userid

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
)

const sampleClaudeJSON = `{
  "oauthAccount": {
    "emailAddress": "a@b.com",
    "accountUuid": "uuid-123"
  }
}`

// setHomeDir points os.UserHomeDir at dir: it reads HOME on unix and
// USERPROFILE on Windows, so both are set.
func setHomeDir(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
}

func writeClaudeJSON(t *testing.T, dir, contents string) string {
	t.Helper()
	path := filepath.Join(dir, ".claude.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestLoadFromClaudeJSON(t *testing.T) {
	tests := []struct {
		name     string
		contents string
		writeFix bool
		source   string
		want     string
	}{
		{name: "email source", contents: sampleClaudeJSON, writeFix: true, source: "email", want: "a@b.com"},
		{name: "accountUuid source", contents: sampleClaudeJSON, writeFix: true, source: "accountUuid", want: "uuid-123"},
		{name: "empty source defaults to email", contents: sampleClaudeJSON, writeFix: true, source: "", want: "a@b.com"},
		{name: "unknown source falls back to email", contents: sampleClaudeJSON, writeFix: true, source: "bogus", want: "a@b.com"},
		{name: "missing file", writeFix: false, source: "email", want: ""},
		{name: "malformed JSON", contents: `{"oauthAccount":`, writeFix: true, source: "email", want: ""},
		{name: "missing oauthAccount", contents: `{"other":"x"}`, writeFix: true, source: "email", want: ""},
		{name: "empty target field email", contents: `{"oauthAccount":{"accountUuid":"uuid-123"}}`, writeFix: true, source: "email", want: ""},
		{name: "empty target field accountUuid", contents: `{"oauthAccount":{"emailAddress":"a@b.com"}}`, writeFix: true, source: "accountUuid", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, ".claude.json")
			if tt.writeFix {
				writeClaudeJSON(t, dir, tt.contents)
			}

			got := loadFromClaudeJSON(path, tt.source)
			if got != tt.want {
				t.Errorf("loadFromClaudeJSON = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolve(t *testing.T) {
	writeAccount := func(t *testing.T, path, contents string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	selectProfile := func(t *testing.T, home string) string {
		t.Helper()
		writeClaudeJSON(t, home, `{"oauthAccount":{"emailAddress":"default@example.com"}}`)
		profile := filepath.Join(home, "profile")
		t.Setenv("CLAUDE_CONFIG_DIR", profile)
		return profile
	}
	tests := []struct {
		name      string
		envUserID string
		envSource string
		writeFix  bool
		contents  string
		setup     func(t *testing.T, home string)
		want      string
	}{
		{
			name: "custom profile beats home account",
			setup: func(t *testing.T, home string) {
				writeAccount(t, filepath.Join(selectProfile(t, home), ".claude.json"), sampleClaudeJSON)
			},
			want: "a@b.com",
		},
		{
			name: "legacy profile file wins",
			setup: func(t *testing.T, home string) {
				profile := selectProfile(t, home)
				writeAccount(t, filepath.Join(profile, ".claude.json"), `{"oauthAccount":{"emailAddress":"modern@example.com"}}`)
				writeAccount(t, filepath.Join(profile, ".config.json"), sampleClaudeJSON)
			},
			want: "a@b.com",
		},
		{
			name: "default legacy directory",
			setup: func(t *testing.T, home string) {
				writeClaudeJSON(t, home, `{"oauthAccount":{"emailAddress":"modern@example.com"}}`)
				writeAccount(t, filepath.Join(home, ".claude", ".config.json"), sampleClaudeJSON)
			},
			want: "a@b.com",
		},
		{
			name: "custom OAuth profile filename",
			setup: func(t *testing.T, home string) {
				profile := selectProfile(t, home)
				t.Setenv("CLAUDE_CODE_CUSTOM_OAUTH_URL", "https://unused.invalid")
				writeAccount(t, filepath.Join(profile, ".claude.json"), `{"oauthAccount":{"emailAddress":"wrong@example.com"}}`)
				writeAccount(t, filepath.Join(profile, ".claude-custom-oauth.json"), sampleClaudeJSON)
			},
			want: "a@b.com",
		},
		{
			name: "custom OAuth default home filename",
			setup: func(t *testing.T, home string) {
				t.Setenv("CLAUDE_CODE_CUSTOM_OAUTH_URL", "https://unused.invalid")
				writeAccount(t, filepath.Join(home, ".claude-custom-oauth.json"), sampleClaudeJSON)
			},
			want: "a@b.com",
		},
		{
			name:  "missing selected profile does not reuse home",
			setup: func(t *testing.T, home string) { selectProfile(t, home) },
		},
		{
			name: "malformed selected legacy file does not reuse modern or home",
			setup: func(t *testing.T, home string) {
				profile := selectProfile(t, home)
				writeAccount(t, filepath.Join(profile, ".claude.json"), sampleClaudeJSON)
				writeAccount(t, filepath.Join(profile, ".config.json"), `{broken`)
			},
		},
		{
			name: "malformed selected modern file does not reuse home",
			setup: func(t *testing.T, home string) {
				writeAccount(t, filepath.Join(selectProfile(t, home), ".claude.json"), `{broken`)
			},
		},
		{
			name: "legacy file without identity does not reuse modern",
			setup: func(t *testing.T, home string) {
				profile := selectProfile(t, home)
				writeAccount(t, filepath.Join(profile, ".claude.json"), sampleClaudeJSON)
				writeAccount(t, filepath.Join(profile, ".config.json"), `{}`)
			},
		},
		{
			name: "selected file lacks email",
			setup: func(t *testing.T, home string) {
				writeAccount(t, filepath.Join(selectProfile(t, home), ".claude.json"), `{"oauthAccount":{"accountUuid":"selected-uuid"}}`)
			},
		},
		{
			name: "unreadable selected legacy file does not reuse modern",
			setup: func(t *testing.T, home string) {
				profile := selectProfile(t, home)
				writeAccount(t, filepath.Join(profile, ".claude.json"), sampleClaudeJSON)
				if err := os.Mkdir(filepath.Join(profile, ".config.json"), 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:      "selected profile accountUuid",
			envSource: "accountUuid",
			setup: func(t *testing.T, home string) {
				writeAccount(t, filepath.Join(selectProfile(t, home), ".claude.json"), sampleClaudeJSON)
			},
			want: "uuid-123",
		},
		{
			name:      "selected profile lacks requested UUID",
			envSource: "accountUuid",
			setup: func(t *testing.T, home string) {
				writeClaudeJSON(t, home, sampleClaudeJSON)
				profile := filepath.Join(home, "profile")
				t.Setenv("CLAUDE_CONFIG_DIR", profile)
				writeAccount(t, filepath.Join(profile, ".claude.json"), `{"oauthAccount":{"emailAddress":"selected@example.com"}}`)
			},
		},
		{
			name: "explicit preferred ID overrides profile",
			setup: func(t *testing.T, home string) {
				t.Setenv("AGENTO11Y_USER_ID", "pseudonym")
				writeAccount(t, filepath.Join(selectProfile(t, home), ".claude.json"), sampleClaudeJSON)
			},
			want: "pseudonym",
		},
		{
			name: "empty directory uses working directory legacy file",
			setup: func(t *testing.T, home string) {
				t.Setenv("CLAUDE_CONFIG_DIR", "")
				writeAccount(t, ".config.json", sampleClaudeJSON)
			},
			want: "a@b.com",
		},
		{
			name:     "empty directory falls back to modern home file",
			writeFix: true, contents: sampleClaudeJSON,
			setup: func(t *testing.T, home string) { t.Setenv("CLAUDE_CONFIG_DIR", "") },
			want:  "a@b.com",
		},
		{
			name: "relative directory modern file",
			setup: func(t *testing.T, home string) {
				t.Setenv("CLAUDE_CONFIG_DIR", "relative")
				writeAccount(t, filepath.Join("relative", ".claude.json"), sampleClaudeJSON)
			},
			want: "a@b.com",
		},
		{
			name: "relative directory legacy file",
			setup: func(t *testing.T, home string) {
				t.Setenv("CLAUDE_CONFIG_DIR", "relative")
				writeAccount(t, filepath.Join("relative", ".config.json"), sampleClaudeJSON)
			},
			want: "a@b.com",
		},
		{
			name: "legacy directory is NFC normalized",
			setup: func(t *testing.T, home string) {
				t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "cafe\u0301"))
				writeAccount(t, filepath.Join(home, "caf\u00e9", ".config.json"), sampleClaudeJSON)
			},
			want: "a@b.com",
		},
		{
			name: "modern directory preserves Unicode spelling",
			setup: func(t *testing.T, home string) {
				profile := filepath.Join(home, "cafe\u0301")
				t.Setenv("CLAUDE_CONFIG_DIR", profile)
				writeAccount(t, filepath.Join(profile, ".claude.json"), sampleClaudeJSON)
			},
			want: "a@b.com",
		},
		{
			name:      "SIGIL_USER_ID wins over file",
			envUserID: "foo",
			envSource: "accountUuid",
			writeFix:  true,
			contents:  sampleClaudeJSON,
			want:      "foo",
		},
		{
			name:      "SIGIL_USER_ID wins when no file exists",
			envUserID: "foo",
			writeFix:  false,
			want:      "foo",
		},
		{
			name:      "SIGIL_USER_ID trimmed",
			envUserID: "  alex@example.com  ",
			want:      "alex@example.com",
		},
		{
			name:      "whitespace-only SIGIL_USER_ID falls through",
			envUserID: "   ",
			writeFix:  true,
			contents:  sampleClaudeJSON,
			want:      "a@b.com",
		},
		{
			name:     "default source is email",
			writeFix: true,
			contents: sampleClaudeJSON,
			want:     "a@b.com",
		},
		{
			name:      "accountUuid source",
			envSource: "accountUuid",
			writeFix:  true,
			contents:  sampleClaudeJSON,
			want:      "uuid-123",
		},
		{
			name:      "unknown source falls back to email",
			envSource: "bogus",
			writeFix:  true,
			contents:  sampleClaudeJSON,
			want:      "a@b.com",
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
			envconfig.PinAliasEnvBlank(t)
			home := t.TempDir()
			setHomeDir(t, home)
			t.Chdir(t.TempDir())
			t.Setenv("CLAUDE_CONFIG_DIR", "")
			if err := os.Unsetenv("CLAUDE_CONFIG_DIR"); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CLAUDE_CODE_CUSTOM_OAUTH_URL", "")
			t.Setenv("SIGIL_USER_ID", tt.envUserID)
			t.Setenv("SIGIL_USER_ID_SOURCE", tt.envSource)

			if tt.writeFix {
				writeClaudeJSON(t, home, tt.contents)
			}
			if tt.setup != nil {
				tt.setup(t, home)
			}

			got := Resolve()
			if got != tt.want {
				t.Errorf("Resolve() = %q, want %q", got, tt.want)
			}
		})
	}
}
