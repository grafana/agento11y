package userid

import (
	"os"
	"path/filepath"
	"testing"
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
	tests := []struct {
		name      string
		envUserID string
		envSource string
		writeFix  bool
		contents  string
		want      string
	}{
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
			home := t.TempDir()
			setHomeDir(t, home)
			t.Setenv("SIGIL_USER_ID", tt.envUserID)
			t.Setenv("SIGIL_USER_ID_SOURCE", tt.envSource)

			if tt.writeFix {
				writeClaudeJSON(t, home, tt.contents)
			}

			got := Resolve()
			if got != tt.want {
				t.Errorf("Resolve() = %q, want %q", got, tt.want)
			}
		})
	}
}
