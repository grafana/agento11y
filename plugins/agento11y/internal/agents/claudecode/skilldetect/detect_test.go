package skilldetect

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/grafana/agento11y/plugins/agento11y/internal/redact"
)

func TestDetect(t *testing.T) {
	input := func(name string) json.RawMessage {
		data, err := json.Marshal(map[string]string{"skill": name})
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	secret := "glc_" + strings.Repeat("a", 24)
	if redact.New().Redact(secret) == secret {
		t.Fatal("secret fixture must match the existing redactor")
	}
	tests := []struct {
		name, tool string
		raw        json.RawMessage
		want       string
	}{
		{"native", "Skill", input("code-review"), "code-review"},
		{"trimmed", "Skill", input("  code-review  "), "code-review"},
		{"namespace preserved", "Skill", input("My.Plugin:Review_2"), "My.Plugin:Review_2"},
		{"nested namespaces", "Skill", input("org:plugin:review"), "org:plugin:review"},
		{"unicode", "Skill", input("团队:审查"), "团队:审查"},
		{"256 bytes", "Skill", input(strings.Repeat("a", 256)), strings.Repeat("a", 256)},
		{"256 UTF8 bytes", "Skill", input(strings.Repeat("é", 128)), strings.Repeat("é", 128)},
		{"escaped name", "Skill", json.RawMessage(`{"skill":"plugin\u003areview"}`), "plugin:review"},
		{"extra fields ignored", "Skill", json.RawMessage(`{"skill":"review","args":"private text"}`), "review"},
		{"Read skill path", "Read", json.RawMessage(`{"file_path":"/home/me/.claude/skills/review/SKILL.md"}`), ""},
		{"Read with skill argument", "Read", input("review"), ""},
		{"Read relative skill path", "Read", json.RawMessage(`{"file_path":".claude/skills/review/SKILL.md"}`), ""},
		{"Read plugin skill path", "Read", json.RawMessage(`{"file_path":"/home/me/.claude/plugins/cache/team/plugin/1/skills/review/SKILL.md"}`), ""},
		{"lowercase tool", "skill", input("review"), ""},
		{"padded tool", " Skill ", input("review"), ""},
		{"other tool", "Bash", input("review"), ""},
		{"missing input", "Skill", nil, ""},
		{"malformed JSON", "Skill", json.RawMessage(`{"skill":"review"`), ""},
		{"trailing JSON", "Skill", json.RawMessage(`{"skill":"review"} {}`), ""},
		{"string input", "Skill", json.RawMessage(`"review"`), ""},
		{"array input", "Skill", json.RawMessage(`[{"skill":"review"}]`), ""},
		{"null input", "Skill", json.RawMessage(`null`), ""},
		{"missing skill", "Skill", json.RawMessage(`{"name":"review"}`), ""},
		{"wrong key case", "Skill", json.RawMessage(`{"Skill":"review"}`), ""},
		{"null skill", "Skill", json.RawMessage(`{"skill":null}`), ""},
		{"number skill", "Skill", json.RawMessage(`{"skill":123}`), ""},
		{"bool skill", "Skill", json.RawMessage(`{"skill":true}`), ""},
		{"object skill", "Skill", json.RawMessage(`{"skill":{"name":"review"}}`), ""},
		{"array skill", "Skill", json.RawMessage(`{"skill":["review"]}`), ""},
		{"empty", "Skill", input(""), ""},
		{"blank", "Skill", input("   "), ""},
		{"internal space", "Skill", input("code review"), ""},
		{"257 bytes", "Skill", input(strings.Repeat("a", 257)), ""},
		{"258 UTF8 bytes", "Skill", input(strings.Repeat("é", 129)), ""},
		{"POSIX absolute path", "Skill", input("/private/review"), ""},
		{"Windows absolute path", "Skill", input(`C:\private\review`), ""},
		{"UNC path", "Skill", input(`\\host\review`), ""},
		{"relative path", "Skill", input("skills/review"), ""},
		{"parent path", "Skill", input("../review"), ""},
		{"URL", "Skill", input("https://example.test/review"), ""},
		{"file URL", "Skill", input("file:///private/review"), ""},
		{"file scheme", "Skill", input("file:review"), ""},
		{"HTTPS scheme", "Skill", input("HTTPS:review"), ""},
		{"git scheme", "Skill", input("git:review"), ""},
		{"plain scheme word", "Skill", input("file"), "file"},
		{"query", "Skill", input("review?token=private"), ""},
		{"empty namespace", "Skill", input(":review"), ""},
		{"empty namespace segment", "Skill", input("plugin::review"), ""},
		{"empty final segment", "Skill", input("plugin:"), ""},
		{"dot segment", "Skill", input("plugin:.."), ""},
		{"control newline", "Skill", input("review\n"), ""},
		{"control tab", "Skill", input("\treview"), ""},
		{"control NUL", "Skill", input("re\x00view"), ""},
		{"control DEL", "Skill", input("review\x7f"), ""},
		{"control C1", "Skill", input("review\u0085"), ""},
		{"format character", "Skill", input("review\u202e"), ""},
		{"invalid UTF8", "Skill", json.RawMessage("{\"skill\":\"re\xffview\"}"), ""},
		{"unpaired surrogate", "Skill", json.RawMessage(`{"skill":"re\ud800view"}`), ""},
		{"secret name", "Skill", input(secret), ""},
		{"secret namespace", "Skill", input("plugin:" + secret), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := Detect(tt.tool, tt.raw)
			if got != tt.want || ok != (tt.want != "") {
				t.Errorf("Detect() = (%q, %v), want (%q, %v)", got, ok, tt.want, tt.want != "")
			}
		})
	}
}
