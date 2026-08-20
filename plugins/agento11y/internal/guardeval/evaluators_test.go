package guardeval

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// toolCallSpec names one tool call for the multi-call request builder.
type toolCallSpec struct {
	name      string
	inputJSON string
}

// toolCall builds a postflight request carrying one assistant tool call.
func toolCall(name, inputJSON string) agento11y.HookEvaluateRequest {
	return toolCalls(toolCallSpec{name: name, inputJSON: inputJSON})
}

// toolCalls builds a postflight request carrying several assistant tool calls,
// in the order given.
func toolCalls(specs ...toolCallSpec) agento11y.HookEvaluateRequest {
	parts := make([]agento11y.Part, 0, len(specs))
	for i, spec := range specs {
		tc := agento11y.ToolCall{ID: fmt.Sprintf("t%d", i+1), Name: spec.name}
		if spec.inputJSON != "" {
			tc.InputJSON = json.RawMessage(spec.inputJSON)
		}
		parts = append(parts, agento11y.Part{Kind: agento11y.PartKindToolCall, ToolCall: &tc})
	}
	return agento11y.HookEvaluateRequest{
		Phase: agento11y.HookPhasePostflight,
		Input: agento11y.HookInput{Output: []agento11y.Message{{Role: "assistant", Parts: parts}}},
	}
}

// textOutput builds a HookInput whose flattened response is exactly s (one
// assistant text part), for unit-testing evaluators against a known target text.
func textOutput(s string) agento11y.HookInput {
	return agento11y.HookInput{Output: []agento11y.Message{{
		Role:  "assistant",
		Parts: []agento11y.Part{{Kind: agento11y.PartKindText, Text: s}},
	}}}
}

func TestFlatten_SerializesPartsAndToolCalls(t *testing.T) {
	cases := []struct {
		name string
		msgs []agento11y.Message
		want string
	}{
		{name: "empty", msgs: nil, want: ""},
		{name: "text trimmed", msgs: []agento11y.Message{{Parts: []agento11y.Part{{Kind: agento11y.PartKindText, Text: "  hello  "}}}}, want: "hello"},
		{
			name: "tool call name and input",
			msgs: []agento11y.Message{{Parts: []agento11y.Part{{Kind: agento11y.PartKindToolCall, ToolCall: &agento11y.ToolCall{Name: "Bash", InputJSON: json.RawMessage(`{"command":"git reset --hard"}`)}}}}},
			want: `[tool_call] Bash {"command":"git reset --hard"}`,
		},
		{
			name: "parts joined with newline",
			msgs: []agento11y.Message{{Parts: []agento11y.Part{
				{Kind: agento11y.PartKindText, Text: "ok"},
				{Kind: agento11y.PartKindToolCall, ToolCall: &agento11y.ToolCall{Name: "Read", InputJSON: json.RawMessage(`{"path":"/etc"}`)}},
			}}},
			want: "ok\n[tool_call] Read {\"path\":\"/etc\"}",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, flatten(tc.msgs))
		})
	}
}

func TestRegexEvaluator_RejectAndTarget(t *testing.T) {
	cases := []struct {
		name            string
		config          map[string]any
		input           agento11y.HookInput
		wantPassed      bool
		wantExplanation string
	}{
		{
			name:       "reject + match on response → not passed",
			config:     map[string]any{"patterns": []any{`(?i)git\s+reset\s+--hard`}, "reject": true},
			input:      textOutput("git reset --hard HEAD~1"),
			wantPassed: false,
		},
		{
			name:       "reject + no match → passed",
			config:     map[string]any{"patterns": []any{`(?i)git\s+reset\s+--hard`}, "reject": true},
			input:      textOutput("git status"),
			wantPassed: true,
		},
		{
			name:       "matches the flattened tool call text",
			config:     map[string]any{"pattern": `(?i)rm\s+-rf`, "reject": true},
			input:      agento11y.HookInput{Output: []agento11y.Message{{Parts: []agento11y.Part{{Kind: agento11y.PartKindToolCall, ToolCall: &agento11y.ToolCall{Name: "Bash", InputJSON: json.RawMessage(`{"command":"rm -rf /tmp"}`)}}}}}},
			wantPassed: false,
		},
		{
			name:       "target input does not see the response",
			config:     map[string]any{"patterns": []any{`(?i)secret`}, "reject": true, "target": "input"},
			input:      textOutput("secret in response only"),
			wantPassed: true,
		},
		{
			// kind and phase are matched case-insensitively, so a capitalized
			// target in a hand-written guards.toml must not fail to compile and
			// take the whole rule out of enforcement.
			name:       "target is case-insensitive",
			config:     map[string]any{"patterns": []any{`(?i)secret`}, "reject": true, "target": "Input"},
			input:      textOutput("secret in response only"),
			wantPassed: true,
		},
		{
			// reject defaults to false, which inverts the verdict: the pattern
			// becomes required, so an absent match fails. The editor loads the
			// same default, so the two agree on what a saved rule means.
			name:            "no reject + no match is a failure explained as a missing required pattern",
			config:          map[string]any{"patterns": []any{`(?i)git\s+push`}},
			input:           textOutput("git status"),
			wantPassed:      false,
			wantExplanation: "did not match a required pattern",
		},
		{
			name:       "no reject + match passes",
			config:     map[string]any{"patterns": []any{`(?i)git\s+push`}},
			input:      textOutput("git push origin main"),
			wantPassed: true,
		},
		{
			name:            "reject + match explains the block",
			config:          map[string]any{"patterns": []any{`(?i)chmod\s+777`}, "reject": true},
			input:           textOutput("chmod 777 /etc/passwd"),
			wantPassed:      false,
			wantExplanation: "matched a blocked pattern",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ce, err := compileEvaluator(EvaluatorSpec{Kind: "regex", Config: tc.config})
			require.NoError(t, err)
			require.NotNil(t, ce)
			_, passed, explanation := runEvaluator(ce, tc.input)
			assert.Equal(t, tc.wantPassed, passed)
			if tc.wantExplanation != "" {
				assert.Equal(t, tc.wantExplanation, explanation)
			}
		})
	}
}

// A rule whose only evaluator is json_schema has nothing to enforce locally, so
// it must not be counted as enforcing or answer a call. It used to deny: a tool
// call flattens to "[tool_call] name {…}", which is not a JSON document, so a
// rule written to check a structured answer failed on every tool call it
// matched.
func TestEvaluate_JSONSchemaOnlyRuleDoesNotDeny(t *testing.T) {
	rules := compileRulesForTest(t, Rule{
		RuleID:       "structured-answer",
		ActionOnFail: "deny",
		Evaluators: []EvaluatorSpec{{Kind: "json_schema", Config: map[string]any{
			"schema": map[string]any{"type": "object", "required": []any{"name"}},
		}}},
	})
	require.Len(t, rules, 1)
	assert.False(t, ruleEnforceable(rules[0]))

	resp := evaluateForTest(rules, nil, agento11y.HookEvaluateRequest{
		Phase: agento11y.HookPhasePostflight,
		Input: toolCallInput("output", "Bash", `{"command":"ls"}`),
	})
	assert.Equal(t, agento11y.HookActionAllow, resp.Action)
	assert.Empty(t, resp.Evaluations)
}

func TestEngineReportsRemovedTargetAndKeepsOtherRules(t *testing.T) {
	rules := []Rule{
		{
			RuleID: "removed.target",
			Evaluators: []EvaluatorSpec{{Kind: "regex", Config: map[string]any{
				"target": "tool_and_prompt", "pattern": "secret", "reject": true,
			}}},
		},
		{
			RuleID: "block.rm",
			ToolFilter: &ToolFilterConfig{
				BlockedNames: []string{"Bash(*rm -rf*)"},
			},
		},
	}
	raw := make([]json.RawMessage, 0, len(rules))
	for _, rule := range rules {
		encoded, err := json.Marshal(rule)
		require.NoError(t, err)
		raw = append(raw, encoded)
	}

	engine := NewRulesEngine(raw, nil)
	require.Len(t, engine.Status().Errors, 1)
	assert.Contains(t, engine.Status().Errors[0], `target "tool_and_prompt" is invalid`)
	assert.Contains(t, engine.Status().Errors[0], "response, input, system_prompt, shell_command")

	resp := engine.Evaluate(toolCall("Bash", `{"command":"rm -rf /tmp/x"}`))
	assert.Equal(t, agento11y.HookActionDeny, resp.Action)
	assert.Equal(t, "block.rm", resp.RuleID)
}

func TestCompileEvaluator_KindHandling(t *testing.T) {
	cases := []struct {
		name    string
		kind    string
		wantErr string
	}{
		{name: "llm_judge is kept but inert", kind: "llm_judge"},
		{name: "prompt_guard is kept but inert", kind: "prompt_guard"},
		{name: "heuristic is kept but inert", kind: "heuristic"},
		// json_schema is inert for a different reason than the three above: it
		// needs no model, but no local target hands it a JSON document to check.
		{name: "json_schema is kept but inert", kind: "json_schema"},
		// A typo must not compile to nothing: the rule would then be listed as a
		// deliberate cloud-only rule and never fire.
		{name: "typo rejected", kind: "regexp", wantErr: "not supported locally"},
		{name: "unknown kind rejected", kind: "something_unknown", wantErr: "not supported locally"},
		{name: "missing kind rejected", kind: "", wantErr: "evaluator kind is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ce, err := compileEvaluator(EvaluatorSpec{Kind: tc.kind, Config: map[string]any{"x": 1}})
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Nil(t, ce, "kind %q should not be runnable locally", tc.kind)
		})
	}
}

// inlineRegexRule compiles a single postflight rule with one inline regex
// evaluator (reject:true), mirroring what the pack/editor emit.
func inlineRegexRule(t *testing.T, action, pattern string) []CompiledRule {
	t.Helper()
	return compileRulesForTest(t, Rule{
		RuleID:       "r",
		Phase:        "postflight",
		ActionOnFail: action,
		Evaluators:   []EvaluatorSpec{{Kind: "regex", Config: map[string]any{"target": "response", "patterns": []any{pattern}, "reject": true}}},
	})
}

func TestGuardEvaluate_InlineRegexDeniesAndWarns(t *testing.T) {
	t.Run("deny on match", func(t *testing.T) {
		resp := evaluateForTest(inlineRegexRule(t, "deny", `(?i)git\s+reset\s+--hard`), nil, toolCall("Bash", `{"command":"git reset --hard HEAD~1"}`))
		assert.Equal(t, agento11y.HookActionDeny, resp.Action)
		assert.Equal(t, "r", resp.RuleID)
		require.Len(t, resp.Evaluations, 1)
		assert.Equal(t, "regex", resp.Evaluations[0].EvaluatorKind)
		assert.False(t, resp.Evaluations[0].Passed)
	})
	t.Run("allow when no match", func(t *testing.T) {
		resp := evaluateForTest(inlineRegexRule(t, "deny", `(?i)git\s+reset\s+--hard`), nil, toolCall("Bash", `{"command":"git status"}`))
		assert.Equal(t, agento11y.HookActionAllow, resp.Action)
	})
	t.Run("warn records but allows", func(t *testing.T) {
		resp := evaluateForTest(inlineRegexRule(t, "warn", `(?i)git\s+reset\s+--hard`), nil, toolCall("Bash", `{"command":"git reset --hard"}`))
		assert.Equal(t, agento11y.HookActionAllow, resp.Action)
		require.Len(t, resp.Evaluations, 1)
		assert.False(t, resp.Evaluations[0].Passed)
	})
}
