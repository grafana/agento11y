package guardeval

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func ptrBool(b bool) *bool { return &b }

// rawRules is how a test writes a ruleset. The engine holds raw JSON because
// that is the shape its real input has, whether the rules came out of the TOML
// file or off the wire. A test that built Rule values directly would exercise a
// decode path production never takes.
func rawRules(t *testing.T, rules ...Rule) []json.RawMessage {
	t.Helper()
	out := make([]json.RawMessage, 0, len(rules))
	for _, rule := range rules {
		data, err := json.Marshal(rule)
		require.NoError(t, err)
		out = append(out, data)
	}
	return out
}

// toolFilterRule is a rule that blocks a tool call, the smallest rule that can
// actually act locally.
func toolFilterRule(id string, priority int, action string, blocked ...string) Rule {
	return Rule{
		RuleID:       id,
		Priority:     priority,
		ActionOnFail: action,
		ToolFilter:   &ToolFilterConfig{BlockedNames: blocked},
	}
}

func redactRule(id string, priority int, regex, replacement string) Rule {
	return Rule{
		RuleID:    id,
		Priority:  priority,
		Transform: &TransformConfig{Patterns: []TransformPattern{{Regex: regex, Replacement: replacement}}},
	}
}

func compiledIDs(e *Engine) []string {
	out := make([]string, 0, len(e.rules))
	for _, r := range e.rules {
		out = append(out, r.id)
	}
	return out
}

func joinStrings(in []string) string {
	return strings.Join(in, "\n")
}

func TestParseEffect(t *testing.T) {
	cases := []struct {
		name         string
		actionOnFail string
		want         Effect
	}{
		{name: "deny", actionOnFail: "deny", want: EffectDeny},
		{name: "warn", actionOnFail: "warn", want: EffectWarn},
		{name: "allow", actionOnFail: "allow", want: EffectAllow},
		{name: "case and padding are ignored", actionOnFail: "  WaRn ", want: EffectWarn},
		{name: "allow in caps", actionOnFail: "ALLOW", want: EffectAllow},
		// An unset action_on_fail is the schema default, and a word this build
		// does not know has to resolve the same way: to the strict end, so no
		// unrecognized policy word can quietly stop blocking.
		{name: "empty defaults to deny", actionOnFail: "", want: EffectDeny},
		{name: "unknown word denies", actionOnFail: "block", want: EffectDeny},
		{name: "misspelling denies", actionOnFail: "waarn", want: EffectDeny},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, parseEffect(tc.actionOnFail))
		})
	}
}

func TestValidateRuleIDs(t *testing.T) {
	cases := []struct {
		name     string
		rules    []Rule
		wantErr  string
		wantPass bool
	}{
		{name: "unique ids pass", rules: []Rule{{RuleID: "a"}, {RuleID: "b"}}, wantPass: true},
		{name: "blank id is rejected", rules: []Rule{{RuleID: " "}}, wantErr: "rule_id is required"},
		{name: "duplicate id is rejected", rules: []Rule{{RuleID: "a"}, {RuleID: "a"}}, wantErr: "duplicates"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateRuleIDs(tc.rules)
			if tc.wantPass {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestNewRulesEngine_Compile pins what compiling does to a ruleset: the
// ordering, the rules it drops, and what it counts as able to act.
func TestNewRulesEngine_Compile(t *testing.T) {
	cases := []struct {
		name          string
		rules         []json.RawMessage
		wantIDs       []string
		wantRules     int
		wantEnforcing int
		// wantErrors are fragments every reported problem list must contain.
		wantErrors []string
	}{
		{
			name: "rules sort by priority then id",
			rules: rawRules(t,
				toolFilterRule("z-last", 20, "", "Bash"),
				toolFilterRule("a-first", 5, "", "Bash"),
				toolFilterRule("middle", 10, "", "Bash"),
				toolFilterRule("also-first", 5, "", "Bash"),
			),
			wantIDs:       []string{"a-first", "also-first", "middle", "z-last"},
			wantRules:     4,
			wantEnforcing: 4,
		},
		{
			name: "a rule disabled in the file is dropped",
			rules: rawRules(t,
				toolFilterRule("on", 0, "", "Bash"),
				Rule{RuleID: "off", Enabled: ptrBool(false), ToolFilter: &ToolFilterConfig{BlockedNames: []string{"Bash"}}},
			),
			wantIDs:       []string{"on"},
			wantRules:     1,
			wantEnforcing: 1,
		},
		{
			// A rule whose only evaluator is a Cloud kind loads and can never act,
			// so it counts as compiled but not as enforcing.
			name: "a rule with nothing local counts as compiled, not enforcing",
			rules: rawRules(t,
				Rule{RuleID: "cloud-only", Evaluators: []EvaluatorSpec{{Kind: "llm_judge"}}},
				toolFilterRule("local", 1, "", "Bash"),
			),
			wantIDs:       []string{"cloud-only", "local"},
			wantRules:     2,
			wantEnforcing: 1,
		},
		{
			// One uncompilable regex must not disarm the rest of the file.
			name: "a rule that does not compile is reported and skipped",
			rules: rawRules(t,
				redactRule("bad-regex", 0, "(", ""),
				toolFilterRule("good", 1, "", "Bash"),
			),
			wantIDs:       []string{"good"},
			wantRules:     1,
			wantEnforcing: 1,
			wantErrors:    []string{`rule "bad-regex"`},
		},
		{
			// A hand-edited file can hold something that is not a rule, and
			// dropping the whole ruleset over one entry would silently stop
			// enforcing rules that are perfectly fine.
			name: "an entry that is not a rule is reported and skipped",
			rules: []json.RawMessage{
				json.RawMessage(`{"rule_id":"ok","tool_filter":{"blocked_names":["Bash"]}}`),
				json.RawMessage(`{"rule_id":42}`),
				json.RawMessage(`"not a rule at all"`),
			},
			wantIDs:       []string{"ok"},
			wantRules:     1,
			wantEnforcing: 1,
			wantErrors:    []string{"rule[1]", "rule[2]"},
		},
		{
			name:      "no rules compile to an empty ruleset",
			rules:     nil,
			wantIDs:   []string{},
			wantRules: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logbuf bytes.Buffer
			engine := NewRulesEngine(tc.rules, log.New(&logbuf, "", 0))

			assert.Equal(t, tc.wantIDs, compiledIDs(engine), "compiled rules, in evaluation order")

			status := engine.Status()
			assert.Equal(t, tc.wantRules, status.Rules)
			assert.Equal(t, tc.wantEnforcing, status.Enforcing)
			if len(tc.wantErrors) == 0 {
				assert.Empty(t, status.Errors)
				return
			}
			for _, fragment := range tc.wantErrors {
				assert.Contains(t, joinStrings(status.Errors), fragment)
				// A problem the daemon only records in a struct nobody prints is
				// a problem nobody sees.
				assert.Contains(t, logbuf.String(), fragment)
			}
		})
	}
}

// TestEngine_Evaluate covers what a verdict says: which rule decided it, what
// the rule's effect does, and how an allow exception ends the ruleset.
func TestEngine_Evaluate(t *testing.T) {
	type wantEval struct {
		ruleID        string
		effect        string
		evaluatorKind string
		passed        bool
	}

	cases := []struct {
		name           string
		rules          []json.RawMessage
		input          agento11y.HookInput
		wantAction     agento11y.HookAction
		wantRuleID     string
		wantReasonHas  []string
		wantEvals      []wantEval
		wantTransform  bool     // transformed_input is attached
		wantRewriteHas []string // substrings of the rewritten tool call
		wantRewriteNot []string
	}{
		{
			// The deny a user reads has to name the rule they can go and change.
			name:          "a deny names the rule and why it fired",
			rules:         rawRules(t, toolFilterRule("no-bash", 10, "deny", "Bash")),
			input:         toolCallInput("output", "Bash", `{"command":"ls"}`),
			wantAction:    agento11y.HookActionDeny,
			wantRuleID:    "no-bash",
			wantReasonHas: []string{`rule "no-bash"`, "denied the call", `tool "Bash" matched tool_filter`},
			wantEvals:     []wantEval{{ruleID: "no-bash", effect: "deny"}},
		},
		{
			// The same rule set to warn stops nothing, and the evaluation says
			// warn so a consumer can tell it from a block that did happen.
			name:       "a warn rule records without blocking",
			rules:      rawRules(t, toolFilterRule("no-bash", 10, "warn", "Bash")),
			input:      toolCallInput("output", "Bash", `{"command":"ls"}`),
			wantAction: agento11y.HookActionAllow,
			wantEvals:  []wantEval{{ruleID: "no-bash", effect: "warn"}},
		},
		{
			name: "a warn tool filter does not run the same rule's evaluators",
			rules: rawRules(t, Rule{
				RuleID:       "no-bash",
				ToolFilter:   &ToolFilterConfig{BlockedNames: []string{"Bash"}},
				ActionOnFail: "warn",
				Evaluators: []EvaluatorSpec{{
					Kind: "regex",
					Config: map[string]any{
						"target":   "shell_command",
						"patterns": []any{"git status"},
						"reject":   true,
					},
				}},
			}),
			input:      toolCallInput("output", "Bash", `{"command":"git status"}`),
			wantAction: agento11y.HookActionAllow,
			wantEvals:  []wantEval{{ruleID: "no-bash", effect: "warn", evaluatorKind: "tool_filter"}},
		},
		{
			name: "a passing evaluation is reported too",
			rules: rawRules(t, Rule{
				RuleID: "require-status",
				Evaluators: []EvaluatorSpec{{
					Kind: "regex",
					Config: map[string]any{
						"target":   "response",
						"patterns": []any{"git status"},
					},
				}},
			}),
			input:      toolCallInput("output", "Bash", `{"command":"git status"}`),
			wantAction: agento11y.HookActionAllow,
			wantEvals:  []wantEval{{ruleID: "require-status", effect: "deny", passed: true}},
		},
		{
			// An allow exception ordered ahead of a deny ends the ruleset in
			// favour of the call, and keeps the redaction an earlier rule made.
			name: "an allow rule short-circuits, keeps the transform, and stops the rules after it",
			rules: rawRules(t,
				redactRule("redact-key", 0, "sk-[a-z0-9]+", "[REDACTED]"),
				toolFilterRule("allow-build-cleanup", 5, "allow", "Bash(*./build*)"),
				toolFilterRule("no-bash", 10, "deny", "Bash"),
			),
			input:         toolCallInput("output", "Bash", `{"command":"rm -rf ./build sk-abc123"}`),
			wantAction:    agento11y.HookActionAllow,
			wantRuleID:    "allow-build-cleanup",
			wantReasonHas: []string{`rule "allow-build-cleanup"`, "allowed the call"},
			wantEvals: []wantEval{
				{ruleID: "allow-build-cleanup", effect: "allow"},
			},
			wantTransform:  true,
			wantRewriteHas: []string{"[REDACTED]", "rm -rf ./build"},
			wantRewriteNot: []string{"sk-abc123"},
		},
		{
			// Priority decides which of the two runs first, so the same pair with
			// the deny ordered ahead of the exception denies.
			name: "an allow rule ordered after the deny does not rescue the call",
			rules: rawRules(t,
				toolFilterRule("no-bash", 10, "deny", "Bash"),
				toolFilterRule("allow-build-cleanup", 50, "allow", "Bash(*./build*)"),
			),
			input:      toolCallInput("output", "Bash", `{"command":"rm -rf ./build"}`),
			wantAction: agento11y.HookActionDeny,
			wantRuleID: "no-bash",
			wantEvals:  []wantEval{{ruleID: "no-bash", effect: "deny"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine := NewRulesEngine(tc.rules, nil)
			resp, transform := engine.EvaluateWithTransform(agento11y.HookEvaluateRequest{
				Phase: agento11y.HookPhasePostflight,
				Input: tc.input,
			})

			assert.Equal(t, tc.wantAction, resp.Action)
			assert.Equal(t, tc.wantRuleID, resp.RuleID)
			for _, fragment := range tc.wantReasonHas {
				assert.Contains(t, resp.Reason, fragment)
			}

			require.Len(t, resp.Evaluations, len(tc.wantEvals))
			for i, want := range tc.wantEvals {
				got := resp.Evaluations[i]
				assert.Equal(t, want.ruleID, got.RuleID)
				assert.Equal(t, want.effect, got.Effect)
				if want.evaluatorKind != "" {
					assert.Equal(t, want.evaluatorKind, got.EvaluatorKind)
				}
				assert.Equal(t, want.passed, got.Passed)
			}

			if !tc.wantTransform {
				assert.Nil(t, resp.TransformedInput)
				return
			}
			require.NotNil(t, resp.TransformedInput, "an allow keeps what the earlier rules redacted")
			require.NotNil(t, transform, "the caller can re-run the patterns over another stage's rewrite")
			rewritten := string(resp.TransformedInput.Output[0].Parts[0].ToolCall.InputJSON)
			for _, fragment := range tc.wantRewriteHas {
				assert.Contains(t, rewritten, fragment)
			}
			for _, fragment := range tc.wantRewriteNot {
				assert.NotContains(t, rewritten, fragment)
			}
		})
	}
}

// TestEngine_Nil covers the caller that has not compiled an engine yet, so it
// does not have to branch on nil before every tool call.
func TestEngine_Nil(t *testing.T) {
	var engine *Engine
	resp := engine.Evaluate(agento11y.HookEvaluateRequest{
		Phase: agento11y.HookPhasePostflight,
		Input: toolCallInput("output", "Bash", `{"command":"rm -rf /"}`),
	})
	assert.Equal(t, agento11y.HookActionAllow, resp.Action)
	assert.Empty(t, resp.Evaluations)
	assert.Equal(t, Status{}, engine.Status())
}

// TestEngine_StatusIsACopy keeps a caller that renders the status from reaching
// into the ruleset a live daemon is evaluating.
func TestEngine_StatusIsACopy(t *testing.T) {
	engine := NewRulesEngine(rawRules(t, redactRule("bad-regex", 0, "(", "")), nil)

	status := engine.Status()
	require.NotEmpty(t, status.Errors)
	status.Errors[0] = "rewritten"

	assert.NotEqual(t, "rewritten", engine.Status().Errors[0])
}

// TestNewEngine_RulesFile covers reading the ruleset off disk: where the file
// is looked for, and what a file that does not read or parse does to the
// engine. Every failure is fail-open by design, so the daemon keeps answering.
func TestNewEngine_RulesFile(t *testing.T) {
	const oneRule = `
[[rules]]
rule_id = "mine"
tool_filter.blocked_names = ["Bash"]
`

	t.Run("reads guards.toml from the config dir", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, ConfigFile), []byte(oneRule), 0o600))

		engine := NewEngine(Config{ConfigDir: dir})
		assert.Equal(t, []string{"mine"}, compiledIDs(engine))
		assert.Empty(t, engine.Status().Errors)
	})

	t.Run("RulesPath overrides the config dir", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "elsewhere.toml")
		require.NoError(t, os.WriteFile(path, []byte(oneRule), 0o600))

		engine := NewEngine(Config{ConfigDir: t.TempDir(), RulesPath: path})
		assert.Equal(t, []string{"mine"}, compiledIDs(engine))
	})

	t.Run("neither set reads nothing", func(t *testing.T) {
		// An empty path must not resolve against the working directory of
		// whoever started the daemon.
		engine := NewEngine(Config{})
		assert.Empty(t, compiledIDs(engine))
		assert.Empty(t, engine.Status().Errors)
	})

	t.Run("a missing file is silent", func(t *testing.T) {
		engine := NewEngine(Config{ConfigDir: t.TempDir()})
		assert.Empty(t, compiledIDs(engine))
		assert.Empty(t, engine.Status().Errors, "a machine that wrote no rules has nothing wrong with it")
	})

	t.Run("a malformed file is reported and logged", func(t *testing.T) {
		var logbuf bytes.Buffer
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, ConfigFile), []byte("[[rules]\nrule_id = "), 0o600))

		engine := NewEngine(Config{ConfigDir: dir, Logger: log.New(&logbuf, "", 0)})
		joined := joinStrings(engine.Status().Errors)
		assert.Contains(t, joined, ConfigFile)
		assert.Contains(t, joined, "no rules")
		assert.Contains(t, logbuf.String(), ConfigFile)
		assert.Empty(t, compiledIDs(engine))
	})

	t.Run("an unreadable file is reported", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads a mode-000 file")
		}
		dir := t.TempDir()
		path := filepath.Join(dir, ConfigFile)
		require.NoError(t, os.WriteFile(path, []byte(oneRule), 0o600))
		require.NoError(t, os.Chmod(path, 0o000))

		engine := NewEngine(Config{ConfigDir: dir})
		assert.Contains(t, joinStrings(engine.Status().Errors), "permission denied")
	})
}
