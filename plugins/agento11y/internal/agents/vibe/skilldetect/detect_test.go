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

func assertDetect(t *testing.T, tool string, input json.RawMessage, want string) {
	t.Helper()
	got, ok := Detect(tool, input)
	if got != want || ok != (want != "") {
		t.Errorf("Detect() = (%q, %v), want (%q, %v)", got, ok, want, want != "")
	}
}

func TestDetectNames(t *testing.T) {
	secret := "glc_" + strings.Repeat("a", 24)
	if redact.New().Redact(secret) == secret {
		t.Fatal("secret fixture must match the existing redactor")
	}
	for _, tt := range []struct{ label, name, want string }{
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
	} {
		t.Run(tt.label, func(t *testing.T) {
			assertDetect(t, "skill", marshal(t, map[string]string{"name": tt.name}), tt.want)
		})
	}
}

func TestDetectInput(t *testing.T) {
	for _, tt := range []struct{ label, raw, want string }{
		{"object", `{"name":"review"}`, "review"},
		{"whitespace", " \n {\"name\":\"review\"} \t ", "review"},
		{"escaped name", `{"name":"plugin\u003areview"}`, "plugin:review"},
		{"escaped exact key", `{"na\u006de":"review"}`, "review"},
		{"extra fields", `{"name":"review","args":"private text"}`, "review"},
		{"exact key alongside alias", `{"name":"review","Name":"other"}`, "review"},
		{"skill key ignored", `{"name":"review","skill":"other"}`, "review"},
		{"outcome ignored", `{"name":"review","tool_status":"failure"}`, "review"},
		{"missing input", "", ""},
		{"whitespace only", " \n\t ", ""},
		{"malformed", `{"name":"review"`, ""},
		{"trailing JSON", `{"name":"review"} {}`, ""},
		{"array", `[{"name":"review"}]`, ""},
		{"null", `null`, ""},
		{"number", `123`, ""},
		{"boolean", `true`, ""},
		{"empty object", `{}`, ""},
		{"string", `"review"`, ""},
		{"empty string", `""`, ""},
		{"string wrapper", `"{\"name\":\"review\"}"`, ""},
		{"skill key", `{"skill":"review"}`, ""},
		{"wrong key case", `{"Name":"review"}`, ""},
		{"uppercase key", `{"NAME":"review"}`, ""},
		{"nested key", `{"input":{"name":"review"}}`, ""},
		{"null name", `{"name":null}`, ""},
		{"number name", `{"name":123}`, ""},
		{"boolean name", `{"name":true}`, ""},
		{"object name", `{"name":{"name":"review"}}`, ""},
		{"array name", `{"name":["review"]}`, ""},
		{"high surrogate", `{"name":"re\ud800view"}`, ""},
		{"low surrogate", `{"name":"re\udc00view"}`, ""},
		{"invalid UTF8 name", "{\"name\":\"re\xffview\"}", ""},
		{"invalid UTF8 elsewhere", "{\"name\":\"review\",\"extra\":\"\xff\"}", ""},
		{"invalid UTF8 key", "{\"name\":\"review\",\"\xff\":0}", ""},
	} {
		t.Run(tt.label, func(t *testing.T) {
			assertDetect(t, "skill", json.RawMessage(tt.raw), tt.want)
			assertDetect(t, "skill", marshal(t, tt.raw), "")
		})
	}
	assertDetect(t, "skill", nil, "")
}

func TestDetectWrongSignature(t *testing.T) {
	input := json.RawMessage(`{"name":"review"}`)
	for _, tool := range []string{
		"", "Skill", "SKILL", " skill", "skill ", "skill\n", "skill.read",
		"read", "Read", "read_file", "view", "bash", "mcp__skill", "plugin:skill",
	} {
		t.Run(tool, func(t *testing.T) {
			assertDetect(t, tool, input, "")
		})
	}
	for _, tool := range []string{"skill", "skill.read", "read_file", "read", "Read"} {
		assertDetect(t, tool, json.RawMessage(`{"path":".vibe/skills/review/SKILL.md"}`), "")
		assertDetect(t, tool, json.RawMessage(`{"file_path":".vibe/skills/review/SKILL.md"}`), "")
	}
}

func TestSourceDerivedFixtures(t *testing.T) {
	for _, tt := range []struct{ path, event string }{
		{"testdata/2.20.0/after_tool.json", "after_tool"},
		{"testdata/2.24.2/post_tool.json", "post_tool"},
	} {
		t.Run(tt.event, func(t *testing.T) {
			raw, err := os.ReadFile(tt.path)
			if err != nil {
				t.Fatal(err)
			}
			var fixture struct {
				Event      string          `json:"hook_event_name"`
				ToolName   string          `json:"tool_name"`
				ToolInput  json.RawMessage `json:"tool_input"`
				ToolStatus string          `json:"tool_status"`
				ToolError  json.RawMessage `json:"tool_error"`
				ToolOutput struct {
					Name string `json:"name"`
				} `json:"tool_output"`
			}
			if err := json.Unmarshal(raw, &fixture); err != nil {
				t.Fatal(err)
			}
			if fixture.Event != tt.event || fixture.ToolName != "skill" {
				t.Fatalf("fixture signature = (%q, %q)", fixture.Event, fixture.ToolName)
			}
			if fixture.ToolStatus != "success" || string(fixture.ToolError) != "null" || fixture.ToolOutput.Name != "code-review" {
				t.Fatal("fixture must describe a successful code-review result without an error")
			}
			assertDetect(t, fixture.ToolName, fixture.ToolInput, "code-review")
			assertDetect(t, fixture.ToolName, raw, "")
			assertDetect(t, fixture.ToolName, marshal(t, string(fixture.ToolInput)), "")
		})
	}
}
