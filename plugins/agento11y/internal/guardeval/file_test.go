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
			ActionOnFail: "deny",
			ToolFilter:   &ToolFilterConfig{BlockedNames: []string{"Bash(*rm -rf*)"}},
		},
		{
			RuleID: "redact.key",
			Phase:  "postflight",
			Transform: &TransformConfig{
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
	require.NotNil(t, out[0].ToolFilter)
	assert.Equal(t, []string{"Bash(*rm -rf*)"}, out[0].ToolFilter.BlockedNames)
	assert.Equal(t, "redact.key", out[1].RuleID)
	require.NotNil(t, out[1].Transform)
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
	assert.Equal(t, true, rules[0].extra["short_circuit"])
	assert.Equal(t, []any{"ev-1"}, rules[0].extra["evaluator_ids"])

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
				"target": "response",
				"flags":  map[string]any{"i": true},
			},
		}},
	}}
	data, err := EncodeRules(rules)
	require.NoError(t, err)
	got := string(data)
	assert.Contains(t, got, "config.target = \"response\"")
	assert.Contains(t, got, "config.flags = {i = true}")
	assert.NotContains(t, got, "map[")
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

func TestEncodeRulesEmptyIsCommentOnly(t *testing.T) {
	data, err := EncodeRules(nil)
	require.NoError(t, err)
	raw, err := ParseRules(data)
	require.NoError(t, err)
	assert.Empty(t, raw)
}
