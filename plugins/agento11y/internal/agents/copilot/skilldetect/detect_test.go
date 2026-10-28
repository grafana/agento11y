package skilldetect

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/grafana/agento11y/plugins/agento11y/internal/redact"
)

func marshal(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestDetectNames(t *testing.T) {
	secret := "glc_" + strings.Repeat("a", 24)
	if redact.New().Redact(secret) == secret {
		t.Fatal("secret fixture must match the existing redactor")
	}
	tests := []struct {
		label, name, want string
	}{
		{"plain", "code-review", "code-review"},
		{"trimmed", "  code-review  ", "code-review"},
		{"namespace and case", "My.Plugin:Review_2", "My.Plugin:Review_2"},
		{"nested namespace", "org:plugin:review", "org:plugin:review"},
		{"unicode", "团队:审查", "团队:审查"},
		{"unicode whitespace", "\u2003review\u2003", "review"},
		{"byte limit", strings.Repeat("a", 256), strings.Repeat("a", 256)},
		{"UTF8 byte limit", strings.Repeat("é", 128), strings.Repeat("é", 128)},
		{"empty", "", ""},
		{"blank", "   ", ""},
		{"internal space", "code review", ""},
		{"over byte limit", strings.Repeat("a", 257), ""},
		{"over UTF8 byte limit", strings.Repeat("é", 129), ""},
		{"absolute path", "/private/review", ""},
		{"Windows path", `C:\private\review`, ""},
		{"UNC path", `\\host\review`, ""},
		{"relative path", "skills/review", ""},
		{"parent path", "../review", ""},
		{"URL", "https://example.test/review", ""},
		{"file URL", "file:///private/review", ""},
		{"file scheme", "file:review", ""},
		{"uppercase scheme", "HTTPS:review", ""},
		{"git scheme", "git:review", ""},
		{"github scheme", "github:review", ""},
		{"query", "review?token=private", ""},
		{"empty namespace", ":review", ""},
		{"empty middle segment", "plugin::review", ""},
		{"empty final segment", "plugin:", ""},
		{"dot segment", "plugin:..", ""},
		{"leading punctuation", "plugin:_review", ""},
		{"newline", "review\n", ""},
		{"tab", "\treview", ""},
		{"NUL", "re\x00view", ""},
		{"DEL", "review\x7f", ""},
		{"C1 control", "review\u0085", ""},
		{"format character", "review\u202e", ""},
		{"zero width", "re\u200bview", ""},
		{"replacement rune", "re\ufffdview", ""},
		{"secret", secret, ""},
		{"secret namespace", "plugin:" + secret, ""},
	}
	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			object := marshal(t, map[string]string{"skill": tt.name})
			for label, raw := range map[string]json.RawMessage{
				"object":  object,
				"wrapper": marshal(t, string(object)),
			} {
				t.Run(label, func(t *testing.T) {
					assertDetect(t, "skill", raw, tt.want)
				})
			}
		})
	}
}

func TestDetectInput(t *testing.T) {
	tests := []struct {
		name, raw, want string
	}{
		{"object", `{"skill":"review"}`, "review"},
		{"whitespace", " \n {\"skill\":\"review\"} \t ", "review"},
		{"escaped name", `{"skill":"plugin\u003areview"}`, "plugin:review"},
		{"escaped exact key", `{"sk\u0069ll":"review"}`, "review"},
		{"extra fields", `{"skill":"review","args":"private text"}`, "review"},
		{"exact key alongside alias", `{"skill":"review","Skill":"other"}`, "review"},
		{"failure field ignored", `{"skill":"review","resultType":"failure"}`, "review"},
		{"surface field ignored", `{"skill":"review","surface":"vscode"}`, "review"},
		{"missing input", "", ""},
		{"whitespace only", " \n\t ", ""},
		{"malformed", `{"skill":"review"`, ""},
		{"trailing JSON", `{"skill":"review"} {}`, ""},
		{"array", `[{"skill":"review"}]`, ""},
		{"null", `null`, ""},
		{"number", `123`, ""},
		{"boolean", `true`, ""},
		{"missing key", `{"name":"review"}`, ""},
		{"wrong key case", `{"Skill":"review"}`, ""},
		{"uppercase key", `{"SKILL":"review"}`, ""},
		{"nested key", `{"input":{"skill":"review"}}`, ""},
		{"null skill", `{"skill":null}`, ""},
		{"number skill", `{"skill":123}`, ""},
		{"boolean skill", `{"skill":true}`, ""},
		{"object skill", `{"skill":{"name":"review"}}`, ""},
		{"array skill", `{"skill":["review"]}`, ""},
		{"unpaired surrogate", `{"skill":"re\ud800view"}`, ""},
		{"invalid UTF8 name", "{\"skill\":\"re\xffview\"}", ""},
		{"invalid UTF8 elsewhere", "{\"skill\":\"review\",\"extra\":\"\xff\"}", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertDetect(t, "skill", json.RawMessage(tt.raw), tt.want)
			// json.Marshal replaces invalid UTF-8.
			if !strings.Contains(tt.raw, "\xff") {
				assertDetect(t, "skill", marshal(t, tt.raw), tt.want)
			}
		})
	}
	for _, raw := range []string{
		`"review"`,
		`""`,
		`"{\"skill\":\"review\"}" {}`,
		`"{\"skill\":\"review\"}`,
		string(marshal(t, string(marshal(t, `{"skill":"review"}`)))),
		"\"{\\\"skill\\\":\\\"review\\\",\\\"extra\\\":\\\"\xff\\\"}\"",
	} {
		assertDetect(t, "skill", json.RawMessage(raw), "")
	}
	assertDetect(t, "skill", nil, "")
}

func TestDetectWrongSignature(t *testing.T) {
	input := json.RawMessage(`{"skill":"review"}`)
	for _, tool := range []string{"", "Skill", "SKILL", " skill", "skill ", "skill\n", "read", "view", "bash", "mcp__skill", "plugin:skill"} {
		t.Run(tool, func(t *testing.T) {
			assertDetect(t, tool, input, "")
			assertDetect(t, tool, marshal(t, string(input)), "")
		})
	}
	assertDetect(t, "view", json.RawMessage(`{"path":".github/skills/review/SKILL.md"}`), "")
}

func assertDetect(t *testing.T, tool string, raw json.RawMessage, want string) {
	t.Helper()
	got, ok := Detect(tool, raw)
	if got != want || ok != (want != "") {
		t.Errorf("Detect() = (%q, %v), want (%q, %v)", got, ok, want, want != "")
	}
}

func TestSourceDerivedFixtures(t *testing.T) {
	for _, tt := range []struct {
		file, hookType, want, effective string
	}{
		{"success.json", "postToolUse", "effective-review", "effective-review"},
		{"stale-failure.json", "postToolUseFailure", "original-review", "missing-review"},
	} {
		t.Run(tt.file, func(t *testing.T) {
			path := "testdata/cli-1.0.54-d1896a6/" + tt.file
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var fixture struct {
				PreToolUse struct {
					Input struct {
						ToolName string
						ToolArgs json.RawMessage
					}
					Output struct {
						ModifiedArgs json.RawMessage
					}
				}
				Completion struct {
					HookType string
					Input    struct {
						ToolName   string
						ToolArgs   json.RawMessage
						ToolResult struct{ ResultType string }
						Error      string
					}
				}
			}
			if err := json.Unmarshal(raw, &fixture); err != nil {
				t.Fatal(err)
			}
			pre := fixture.PreToolUse
			assertDetect(t, pre.Input.ToolName, pre.Input.ToolArgs, "original-review")
			assertDetect(t, pre.Input.ToolName, pre.Output.ModifiedArgs, tt.effective)
			completion := fixture.Completion
			if completion.HookType != tt.hookType {
				t.Fatalf("hook type = %q, want %q", completion.HookType, tt.hookType)
			}
			assertDetect(t, completion.Input.ToolName, completion.Input.ToolArgs, tt.want)
			if tt.hookType == "postToolUse" {
				if completion.Input.ToolResult.ResultType != "success" || completion.Input.Error != "" {
					t.Fatal("success fixture must contain a successful result without an error")
				}
			} else if completion.Input.Error != "Skill not found: missing-review" {
				t.Fatal("failure fixture must refer to the effective argument, not the stale argument")
			}
		})
	}
}
