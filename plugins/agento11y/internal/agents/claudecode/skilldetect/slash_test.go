package skilldetect

import (
	"strings"
	"testing"
)

func TestSlashCommand(t *testing.T) {
	record := func(name string) string {
		return "<command-message>x</command-message>\n<command-name>" + name + "</command-name>\n<command-args>private args</command-args>"
	}
	tests := []struct {
		name, text, want string
	}{
		{"skill", record("/openspec-explore"), "openspec-explore"},
		{"plugin namespace", record("/my-plugin:review"), "my-plugin:review"},
		{"trimmed", record("  /review  "), "review"},
		{"name first", "<command-name>/review</command-name>", "review"},
		{"no slash", record("review"), ""},
		{"path", record("//home/me/.claude/skills/review"), ""},
		{"secret", record("/glc_" + strings.Repeat("a", 24)), ""},
		{"empty", record("/"), ""},
		{"unclosed", "<command-name>/review", ""},
		{"plain prompt", "please use /review", ""},
		{"local stdout", "<local-command-stdout>Set model</local-command-stdout>", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := SlashCommand(tt.text)
			if got != tt.want || ok != (tt.want != "") {
				t.Fatalf("SlashCommand() = %q, %v; want %q", got, ok, tt.want)
			}
		})
	}
}
