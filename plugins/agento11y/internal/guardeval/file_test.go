package guardeval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeGuardsFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ConfigFile)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

// loadRulesForTest compiles a rules file the way the daemon does, and fails the
// test on anything the engine would only report.
func loadRulesForTest(t *testing.T, path string) []CompiledRule {
	t.Helper()
	engine := NewEngine(Config{RulesPath: path})
	require.Empty(t, engine.Status().Errors)
	return engine.rules
}

func TestFilePath(t *testing.T) {
	assert.Equal(t, filepath.Join("/etc/agento11y", ConfigFile), FilePath("/etc/agento11y/config.env"))
	assert.Empty(t, FilePath(""), "no config file means no rules file to look for")
}

// TestParseRules covers the file shape: the array-of-tables the rules live in,
// what counts as an empty ruleset, and what is rejected outright.
func TestParseRules(t *testing.T) {
	cases := []struct {
		name     string
		content  string
		wantJSON []string
		wantErr  string
	}{
		{
			name: "a rule with nested tables",
			content: `
[[rules]]
rule_id = "no-hard-reset"
priority = 20
action_on_fail = "deny"
tool_filter.blocked_names = ["Bash"]

  [[rules.evaluators]]
  kind = "regex"
  config.target = "shell_command"
  config.reject = true
  config.patterns = ['(?i)\bgit\s+reset\s+--hard\b']
`,
			wantJSON: []string{`{"action_on_fail":"deny","evaluators":[{"config":{"patterns":["(?i)\\bgit\\s+reset\\s+--hard\\b"],"reject":true,"target":"shell_command"},"kind":"regex"}],"priority":20,"rule_id":"no-hard-reset","tool_filter":{"blocked_names":["Bash"]}}`},
		},
		{
			name: "several rules keep their file order",
			content: `
[[rules]]
rule_id = "first"

[[rules]]
rule_id = "second"
`,
			wantJSON: []string{`{"rule_id":"first"}`, `{"rule_id":"second"}`},
		},
		{
			// A field the local evaluator ignores has to survive the read, so a
			// rule exported from Cloud is not quietly stripped.
			name: "unknown fields are preserved",
			content: `
[[rules]]
rule_id = "keep"
evaluator_ids = ["pii.v2"]
short_circuit = true
`,
			wantJSON: []string{`{"evaluator_ids":["pii.v2"],"rule_id":"keep","short_circuit":true}`},
		},
		{
			name:     "an empty file is an empty ruleset",
			content:  "",
			wantJSON: nil,
		},
		{
			// TOML's whole point over JSON here. A file holding only comments is
			// a file someone is still writing, not a fault.
			name:     "a file of comments only is an empty ruleset",
			content:  "# rules go here\n# one day\n",
			wantJSON: nil,
		},
		{
			name:     "a document without a rules key is an empty ruleset",
			content:  "[something_else]\nkey = 1\n",
			wantJSON: nil,
		},
		{
			name:    "rules must be an array of tables",
			content: `rules = "nope"`,
			wantErr: `written [[rules]]`,
		},
		{
			name:    "rules entries must be tables",
			content: `rules = [1, 2]`,
			wantErr: "rules[0] must be a table",
		},
		{
			name:    "malformed TOML is an error",
			content: "[[rules]\nrule_id = ",
			wantErr: "toml",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := ParseRules([]byte(tc.content))
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			got := make([]string, 0, len(raw))
			for _, item := range raw {
				got = append(got, string(item))
			}
			if tc.wantJSON == nil {
				assert.Empty(t, got)
				return
			}
			assert.Equal(t, tc.wantJSON, got)
		})
	}
}

// TestParseRulesFlattensMatch covers the one place the TOML shape and the
// matcher disagree. The matcher reads dotted keys; TOML turns an unquoted dot
// into a sub-table, so the nesting has to be undone on the way in.
func TestParseRulesFlattensMatch(t *testing.T) {
	raw, err := ParseRules([]byte(`
[[rules]]
rule_id = "scoped"

  [rules.match]
  agent_name = "claude-code"
  model.name = "claude-*"
  tags.cwd = "/repos/myproject*"
`))
	require.NoError(t, err)
	require.Len(t, raw, 1)

	var rule Rule
	require.NoError(t, json.Unmarshal(raw[0], &rule))
	assert.Equal(t, map[string]any{
		"agent_name": "claude-code",
		"model.name": "claude-*",
		"tags.cwd":   "/repos/myproject*",
	}, rule.Match)
}

// TestParseRulesAcceptsQuotedMatchKeys covers the other spelling. A key written
// with quotes already holds its dot, so it must pass through untouched rather
// than be flattened twice.
func TestParseRulesAcceptsQuotedMatchKeys(t *testing.T) {
	raw, err := ParseRules([]byte(`
[[rules]]
rule_id = "scoped"

  [rules.match]
  "model.name" = "claude-*"
`))
	require.NoError(t, err)
	require.Len(t, raw, 1)

	var rule Rule
	require.NoError(t, json.Unmarshal(raw[0], &rule))
	assert.Equal(t, map[string]any{"model.name": "claude-*"}, rule.Match)
}

// TestParseRulesLiteralStringsNeedNoEscaping is the reason the file is TOML.
// The same pattern in JSON needs every backslash doubled, which is where a
// hand-written regex goes wrong.
func TestParseRulesLiteralStringsNeedNoEscaping(t *testing.T) {
	path := writeGuardsFile(t, `
[[rules]]
rule_id = "redact"

  [[rules.transform.patterns]]
  regex = '(?i)glsa_[A-Za-z0-9]{32}'
  replacement = "[redacted]"
`)
	rules := loadRulesForTest(t, path)
	require.Len(t, rules, 1)
	require.NotNil(t, rules[0].transform)
	require.Len(t, rules[0].transform.patterns, 1)
	assert.Equal(t, `(?i)glsa_[A-Za-z0-9]{32}`, rules[0].transform.patterns[0].re.String())
}

func TestEncodeRulesRoundTrip(t *testing.T) {
	in := []Rule{
		{
			RuleID:       "block.rm",
			Phase:        "postflight",
			Match:        map[string]any{"tags.service": "api", "tags.service.name": "backend", `tags.a"b`: "quoted", "model.name": "claude-*"},
			ActionOnFail: "deny",
			ToolFilter:   &ToolFilterConfig{BlockedNames: []string{"Bash(*rm -rf*)"}},
		},
		{
			RuleID: "redact.key",
			Phase:  "postflight",
			Transform: &TransformConfig{
				JSONMode: "strings",
				Patterns: []TransformPattern{{ID: "api_key", Regex: `sk-[A-Za-z0-9]+`}},
			},
		},
	}
	data, err := EncodeRules(in)
	require.NoError(t, err)
	raw, err := ParseRules(data)
	require.NoError(t, err)
	out, errs := DecodeRules(raw)
	require.Empty(t, errs)
	require.Len(t, out, 2)
	assert.Equal(t, "block.rm", out[0].RuleID)
	assert.Equal(t, in[0].Match, out[0].Match)
	require.Empty(t, NewEngineFromContents("guards.toml", data, nil).Status().Errors)
	require.NotNil(t, out[0].ToolFilter)
	assert.Equal(t, []string{"Bash(*rm -rf*)"}, out[0].ToolFilter.BlockedNames)
	assert.Equal(t, "redact.key", out[1].RuleID)
	require.NotNil(t, out[1].Transform)
	assert.Equal(t, "strings", out[1].Transform.JSONMode)
	require.Len(t, out[1].Transform.Patterns, 1)
	assert.Equal(t, "sk-[A-Za-z0-9]+", out[1].Transform.Patterns[0].Regex)

	got := string(data)
	assert.Contains(t, got, `tool_filter.blocked_names = ["Bash(*rm -rf*)"]`)
	assert.Contains(t, got, `transform.patterns = [{ id = "api_key", regex = "sk-[A-Za-z0-9]+" }]`)
	assert.NotContains(t, got, "[rules.tool_filter]")
	assert.NotContains(t, got, "[[rules.transform.patterns]]")
}

func TestEncodeRulesPreservesUnknownFields(t *testing.T) {
	raw, err := ParseRules([]byte(`
[[rules]]
rule_id = "block.rm"
phase = "postflight"
action_on_fail = "deny"
short_circuit = true
evaluator_ids = ["ev-1"]
tool_filter.blocked_names = ["Bash(*rm -rf*)"]
`))
	require.NoError(t, err)
	rules, errs := DecodeRules(raw)
	require.Empty(t, errs)
	require.Len(t, rules, 1)
	assert.Equal(t, true, rules[0].raw["short_circuit"])
	assert.Equal(t, []any{"ev-1"}, rules[0].raw["evaluator_ids"])

	data, err := EncodeRules(rules)
	require.NoError(t, err)
	got := string(data)
	assert.Contains(t, got, "short_circuit = true")
	assert.Contains(t, got, `evaluator_ids = ["ev-1"]`)
}

func TestEncodeRulesNestedEvaluatorConfig(t *testing.T) {
	rules := []Rule{{
		RuleID: "check.nested",
		Phase:  "postflight",
		Evaluators: []EvaluatorSpec{{
			Kind: "regex",
			Config: map[string]any{
				"target":      "response",
				"flags":       map[string]any{"i": true},
				"literal.key": map[string]any{"a.b": true},
			},
		}},
	}}
	data, err := EncodeRules(rules)
	require.NoError(t, err)
	got := string(data)
	assert.Contains(t, got, "config.target = \"response\"")
	assert.Contains(t, got, "config.flags = {i = true}")
	assert.NotContains(t, got, "map[")
	raw, err := ParseRules(data)
	require.NoError(t, err)
	decoded, errs := DecodeRules(raw)
	require.Empty(t, errs)
	require.Len(t, decoded, 1)
	assert.Equal(t, rules[0].Evaluators[0].Config, decoded[0].Evaluators[0].Config)
}

func TestEncodeTomlStringEscapesCarriageReturn(t *testing.T) {
	data, err := EncodeRules([]Rule{{
		RuleID: "block.cr",
		Phase:  "postflight",
		Match:  map[string]any{"agent name": "claude"},
		Transform: &TransformConfig{
			Patterns: []TransformPattern{{Regex: "a\rb"}},
		},
	}})
	require.NoError(t, err)
	got := string(data)
	assert.Contains(t, got, `regex = "a\rb"`)
	assert.Contains(t, got, `match."agent name" = "claude"`)
	raw, err := ParseRules(data)
	require.NoError(t, err)
	out, errs := DecodeRules(raw)
	require.Empty(t, errs)
	require.Len(t, out, 1)
	require.NotNil(t, out[0].Transform)
	assert.Equal(t, "a\rb", out[0].Transform.Patterns[0].Regex)
}

func TestEncodeRulesRawOverlay(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
		edit              func(*Rule)
	}{
		{"omissions", `{"rule_id":"custom"}`, `{"rule_id":"custom"}`, nil},
		{"empty_blocks", `{"rule_id":"custom","phase":"","priority":0,"match":{},"tool_filter":{},"transform":{"patterns":[]},"evaluators":[]}`, `{"rule_id":"custom","phase":"","priority":0,"match":{},"tool_filter":{},"transform":{"patterns":[]},"evaluators":[]}`, nil},
		{"whitespace", `{"rule_id":" custom ","phase":" PostFlight ","action_on_fail":" WARN ","transform":{"patterns":[{"id":" padded ","regex":" x ","replacement":"  "}]}}`, `{"rule_id":" custom ","phase":" PostFlight ","action_on_fail":" WARN ","transform":{"patterns":[{"id":" padded ","regex":" x ","replacement":"  "}]}}`, nil},
		{"literal_metadata", `{"rule_id":"custom","owner.name":"ops","owner":{"team.name":"platform"},"evaluators":[{"kind":"regex","note.name":"audit","config":{"target":"input","literal.key":"value","nested":{"a.b":true}}}]}`, `{"rule_id":"custom","owner.name":"ops","owner":{"team.name":"platform"},"evaluators":[{"kind":"regex","note.name":"audit","config":{"target":"input","literal.key":"value","nested":{"a.b":true}}}]}`, nil},
		{"nested_edit", `{"rule_id":"custom","tool_filter":{"blocked_names":["Bash"],"future":{}},"transform":{"patterns":[{"regex":"old","note.key":"keep"}],"future":true},"evaluators":[{"kind":"regex","future":[],"config":{"target":"input","nested":{"a.b":true}}}]}`, `{"rule_id":"custom","tool_filter":{"blocked_names":[],"future":{}},"transform":{"patterns":[{"regex":"new","note.key":"keep"}],"future":true},"evaluators":[{"kind":"regex","future":[],"config":{"target":"response","nested":{"a.b":true}}}]}`, func(r *Rule) {
			r.ToolFilter.BlockedNames = []string{}
			r.Transform.Patterns[0].Regex = "new"
			r.Evaluators[0].Config["target"] = "response"
		}},
		{"clear_fields", `{"rule_id":"custom","priority":8,"tool_filter":{"blocked_names":["Bash"],"future":true},"future":true}`, `{"rule_id":"custom","future":true}`, func(r *Rule) {
			r.Priority = 0
			r.ToolFilter = nil
		}},
		{"enable_only", `{"rule_id":"custom","enabled":false,"transform":{},"future":{}}`, `{"rule_id":"custom","transform":{},"future":{}}`, func(r *Rule) { r.Enabled = nil }},
		{"scalar_edit", `{"rule_id":"custom","action_on_fail":" WARN ","future":{}}`, `{"rule_id":"renamed","priority":9,"action_on_fail":" WARN ","future":{}}`, func(r *Rule) {
			r.RuleID = "renamed"
			r.Priority = 9
		}},
		{"list_resize", `{"rule_id":"custom","transform":{"patterns":[{"regex":"a","note":"keep"},{"regex":"b"}]},"evaluators":[{"kind":"regex","note":"keep"}]}`, `{"rule_id":"custom","transform":{"patterns":[{"regex":"a","note":"keep"}]},"evaluators":[{"kind":"regex","note":"keep"},{"kind":"heuristic"}]}`, func(r *Rule) {
			r.Transform.Patterns = r.Transform.Patterns[:1]
			r.Evaluators = append(r.Evaluators, EvaluatorSpec{Kind: "heuristic"})
		}},
		{"map_edit", `{"rule_id":"custom","evaluators":[{"kind":"regex","note":"keep","config":{"drop":true,"nested":{"a.b":true,"keep":false}}}]}`, `{"rule_id":"custom","evaluators":[{"kind":"regex","note":"keep","config":{"nested":{"a.b":false,"keep":false}}}]}`, func(r *Rule) {
			delete(r.Evaluators[0].Config, "drop")
			r.Evaluators[0].Config["nested"].(map[string]any)["a.b"] = false
		}},
		{"exact_number", `{"rule_id":"custom","revision":9007199254740993}`, `{"rule_id":"custom","revision":9007199254740993}`, nil},
		{"large_positive", `{"rule_id":"custom","metadata":{"limit":1e20}}`, `{"rule_id":"custom","metadata":{"limit":100000000000000000000}}`, nil},
		{"large_negative", `{"rule_id":"custom","metadata":{"limit":-1e20}}`, `{"rule_id":"custom","metadata":{"limit":-100000000000000000000}}`, nil},
		{"int64_boundaries", `{"rule_id":"custom","metadata":{"limits":[9223372036854775807,-9223372036854775808,9223372036854775806,-9223372036854775807]}}`, `{"rule_id":"custom","metadata":{"limits":[9223372036854775807,-9223372036854775808,9223372036854775806,-9223372036854775807]}}`, nil},
		{"float_int64_boundaries", `{"rule_id":"custom","metadata":{"limits":[9.223372036854776e18,-9.223372036854776e18]}}`, `{"rule_id":"custom","metadata":{"limits":[9223372036854776000,-9223372036854776000]}}`, nil},
		{"float_controls", `{"rule_id":"custom","metadata":{"limits":[1e18,-1e18,1e21,-1e21,0,1.25,1e-20]}}`, `{"rule_id":"custom","metadata":{"limits":[1000000000000000000,-1000000000000000000,1e+21,-1e+21,0,1.25,1e-20]}}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rules, errs := DecodeRules([]json.RawMessage{json.RawMessage(tc.input)})
			require.Empty(t, errs)
			if tc.edit != nil {
				tc.edit(&rules[0])
			}
			for range 2 {
				data, err := EncodeRules(rules)
				require.NoError(t, err)
				again, err := EncodeRules(rules)
				require.NoError(t, err)
				assert.Equal(t, data, again)
				raw, err := ParseRules(data)
				require.NoError(t, err)
				require.Len(t, raw, 1)
				want, err := decodeRuleFields([]byte(tc.want))
				require.NoError(t, err)
				got, err := decodeRuleFields(raw[0])
				require.NoError(t, err)
				assert.Equal(t, want, got)
				rules, errs = DecodeRules(raw)
				require.Empty(t, errs)
			}
		})
	}
}

func TestEncodeRulesNewStockShape(t *testing.T) {
	data, err := EncodeRules([]Rule{{
		RuleID: "stock", Priority: 5, ActionOnFail: "deny",
		ToolFilter: &ToolFilterConfig{BlockedNames: []string{"Bash"}},
		Transform:  &TransformConfig{Patterns: []TransformPattern{{ID: "x", Regex: "secret"}}},
		Evaluators: []EvaluatorSpec{{Kind: "regex", Config: map[string]any{"target": "input"}}},
	}})
	require.NoError(t, err)
	assert.Equal(t, rulesFileHeader+`[[rules]]
rule_id = "stock"
phase = "postflight"
priority = 5
action_on_fail = "deny"
tool_filter.blocked_names = ["Bash"]
transform.patterns = [{ id = "x", regex = "secret" }]

[[rules.evaluators]]
kind = "regex"
config.target = "input"
`, string(data))
}

func TestWriteRulesRejectsInvalidEncoding(t *testing.T) {
	for _, value := range []json.Number{"1e", "1e309"} {
		t.Run(value.String(), func(t *testing.T) {
			original := "[[rules]]\nrule_id = \"custom\"\ntool_filter.blocked_names = [\"BlockedTool\"]\n"
			path := writeGuardsFile(t, original)
			rules := []Rule{{
				RuleID: "invalid",
				Evaluators: []EvaluatorSpec{{
					Kind:   "regex",
					Config: map[string]any{"limit": value},
				}},
			}}
			data, err := EncodeRules(rules)
			assert.ErrorContains(t, err, "encoded guards.toml")
			assert.Nil(t, data)
			assert.ErrorContains(t, WriteRules(path, rules), "encoded guards.toml")
			stored, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, original, string(stored))
		})
	}
}

func TestEncodeRulesEmptyIsCommentOnly(t *testing.T) {
	data, err := EncodeRules(nil)
	require.NoError(t, err)
	raw, err := ParseRules(data)
	require.NoError(t, err)
	assert.Empty(t, raw)
}
