package guardeval

import (
	"bytes"
	"encoding/json"
	"log"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGlobSetMatchAny(t *testing.T) {
	cases := []struct {
		name     string
		actual   string
		patterns []string
		want     bool
	}{
		{name: "prefix glob matches", actual: "danger_delete", patterns: []string{"danger_*"}, want: true},
		{name: "prefix glob no match", actual: "safe_op", patterns: []string{"danger_*"}, want: false},
		{name: "case-insensitive exact", actual: "Bash", patterns: []string{"bash"}, want: true},
		{name: "no pattern matches", actual: "rm", patterns: []string{"danger_*", "echo"}, want: false},
		{name: "question mark", actual: "ab", patterns: []string{"a?"}, want: true},
		// ? counts characters, not bytes: a pattern written for one character
		// must not match half of a multi-byte one.
		{name: "question mark counts runes", actual: "aé", patterns: []string{"a?"}, want: true},
		{name: "star spans path separators", actual: "mcp/db/query", patterns: []string{"mcp*query"}, want: true},
		{name: "blank pattern matches nothing", actual: "", patterns: []string{"  "}, want: false},
		// Braces are literal, not alternation: a filter written against JSON tool
		// arguments carries unpaired ones.
		{name: "brace is literal", actual: `bash({"command":"rm -rf /"})`, patterns: []string{`bash(*{"command"*)`}, want: true},
		{name: "brace is not alternation", actual: "write_file", patterns: []string{"{read,write}_file"}, want: false},
		{name: "escaped brace still matches one brace", actual: "{lit", patterns: []string{`\{lit`}, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			set, err := compileGlobs(tc.patterns)
			require.NoError(t, err)
			assert.Equal(t, tc.want, set.matchAny(tc.actual))
		})
	}
}

// A pattern that is not a valid glob is reported when the ruleset compiles.
// Before, an unparseable pattern was skipped silently at match time, so the
// rule reported as enforcing and matched nothing.
func TestCompileGlobs_ReportsAnInvalidPattern(t *testing.T) {
	_, err := compileGlobs([]string{"Bash(*rm*)", "read([unterminated"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `pattern "read([unterminated"`)
}

func mustToolFilter(t *testing.T, blockedNames []string) *toolFilter {
	t.Helper()
	filter, err := compileToolFilter(&ToolFilterConfig{BlockedNames: blockedNames})
	require.NoError(t, err)
	return filter
}

func toolCallInput(field string, name, inputJSON string) agento11y.HookInput {
	part := agento11y.Part{
		Kind:     agento11y.PartKindToolCall,
		ToolCall: &agento11y.ToolCall{ID: "t1", Name: name},
	}
	if inputJSON != "" {
		part.ToolCall.InputJSON = json.RawMessage(inputJSON)
	}
	msg := agento11y.Message{Role: agento11y.RoleAssistant, Parts: []agento11y.Part{part}}
	in := agento11y.HookInput{}
	if field == "messages" {
		in.Messages = []agento11y.Message{msg}
	} else {
		in.Output = []agento11y.Message{msg}
	}
	return in
}

func TestCheckToolFilter(t *testing.T) {
	cases := []struct {
		name            string
		toolName        string
		inputJSON       string
		historyToolName string
		historyJSON     string
		blocked         []string
		wantBlock       bool
		wantName        string
	}{
		{name: "name glob in output", toolName: "danger_delete", blocked: []string{"danger_*"}, wantBlock: true, wantName: "danger_delete"},
		{
			name:            "blocked tool in history does not deny safe output",
			toolName:        "Bash",
			inputJSON:       `{"command":"ls"}`,
			historyToolName: "Bash",
			historyJSON:     `{"command":"rm -rf /"}`,
			blocked:         []string{"Bash(*rm -rf*)"},
		},
		{name: "argument glob blocks", toolName: "Bash", inputJSON: `{"command":"rm -rf /"}`, blocked: []string{"Bash(*rm -rf*)"}, wantBlock: true, wantName: "Bash"},
		{name: "argument glob allows safe", toolName: "Bash", inputJSON: `{"command":"ls"}`, blocked: []string{"Bash(*rm -rf*)"}, wantBlock: false},
		{
			name:      "escaped arguments block",
			toolName:  "Bash",
			inputJSON: `{"command":"rm\u0020-rf /"}`,
			blocked:   []string{"Bash(*rm -rf*)"},
			wantBlock: true,
			wantName:  "Bash",
		},
		{
			// Go's encoder escapes & by default, so this is what an SDK sends.
			name:      "html-escaped arguments block",
			toolName:  "Bash",
			inputJSON: `{"command":"ls \u0026\u0026 rm -rf /"}`,
			blocked:   []string{"Bash(*&& rm*)"},
			wantBlock: true,
			wantName:  "Bash",
		},
		{name: "no blocked names", toolName: "danger_delete", blocked: nil, wantBlock: false},
		// A qualified pattern has to cover a call with no arguments too, or
		// Name(*) blocks every use of the tool but that one.
		{name: "qualified pattern blocks a call with no arguments", toolName: "ListSecrets", blocked: []string{"ListSecrets(*)"}, wantBlock: true, wantName: "ListSecrets"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := toolCallInput("output", tc.toolName, tc.inputJSON)
			if tc.historyToolName != "" {
				in.Messages = toolCallInput("messages", tc.historyToolName, tc.historyJSON).Messages
			}
			blocked, name := checkToolFilter(in, mustToolFilter(t, tc.blocked))
			assert.Equal(t, tc.wantBlock, blocked)
			if tc.wantBlock {
				assert.Equal(t, tc.wantName, name)
			}
		})
	}
}

func TestApplyTransform(t *testing.T) {
	mustCompile := func(p TransformPattern) *Transform {
		ct, err := compileTransform(&TransformConfig{Patterns: []TransformPattern{p}})
		require.NoError(t, err)
		return ct
	}

	t.Run("redacts text part", func(t *testing.T) {
		in := agento11y.HookInput{Messages: []agento11y.Message{{
			Role:  agento11y.RoleUser,
			Parts: []agento11y.Part{{Kind: agento11y.PartKindText, Text: "my key is sk-abc123"}},
		}}}
		out, changed, dropped := ApplyTransform(in, mustCompile(TransformPattern{ID: "secret", Regex: "sk-[a-z0-9]+", Replacement: "[REDACTED]"}), nil)
		assert.Empty(t, dropped)
		assert.True(t, changed)
		assert.Equal(t, "my key is [REDACTED]", out.Messages[0].Parts[0].Text)
		assert.Equal(t, "my key is sk-abc123", in.Messages[0].Parts[0].Text, "input must not be mutated")
	})

	t.Run("redacts tool_call input_json", func(t *testing.T) {
		in := toolCallInput("output", "Bash", `{"command":"deploy sk-abc123"}`)
		out, changed, dropped := ApplyTransform(in, mustCompile(TransformPattern{Regex: "sk-[a-z0-9]+", Replacement: "[REDACTED]"}), nil)
		assert.Empty(t, dropped)
		assert.True(t, changed)
		got := string(out.Output[0].Parts[0].ToolCall.InputJSON)
		assert.Contains(t, got, "[REDACTED]")
		assert.True(t, json.Valid([]byte(got)), "redacted input_json must stay valid JSON")
	})

	// A pattern that eats a JSON key/value pair, or a replacement carrying a
	// quote, would otherwise leave input_json unmarshalable. The response would
	// then fail to marshal and answer 500, which fail-open transport turns into
	// "allow everything".
	t.Run("a rewrite that breaks the JSON is dropped", func(t *testing.T) {
		cases := []struct {
			name    string
			pattern TransformPattern
		}{
			{name: "pattern removes a whole pair", pattern: TransformPattern{ID: "pw", Regex: `"password":\s*"[^"]*"`}},
			{name: "replacement contains a quote", pattern: TransformPattern{Regex: `hunter2`, Replacement: `x"y`}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				const args = `{"password":"hunter2","command":"ls"}`
				in := toolCallInput("output", "Bash", args)
				var buf bytes.Buffer
				out, changed, dropped := ApplyTransform(in, mustCompile(tc.pattern), log.New(&buf, "", 0))
				assert.False(t, changed, "an invalid-JSON rewrite must not count as a redaction")
				assert.JSONEq(t, args, string(out.Output[0].Parts[0].ToolCall.InputJSON))
				assert.Contains(t, buf.String(), "redaction of tool_call.input_json dropped")
				assert.Equal(t, []string{"tool_call.input_json"}, dropped, "the drop has to reach the caller, not only the log")
			})
		}
	})

	t.Run("default replacement uses id", func(t *testing.T) {
		in := agento11y.HookInput{SystemPrompt: "token sk-abc123 here"}
		out, changed, _ := ApplyTransform(in, mustCompile(TransformPattern{ID: "secret", Regex: "sk-[a-z0-9]+"}), nil)
		assert.True(t, changed)
		assert.Equal(t, "token [REDACTED:secret] here", out.SystemPrompt)
	})

	t.Run("default replacement without id", func(t *testing.T) {
		in := agento11y.HookInput{SystemPrompt: "token sk-abc123"}
		out, changed, _ := ApplyTransform(in, mustCompile(TransformPattern{Regex: "sk-[a-z0-9]+"}), nil)
		assert.True(t, changed)
		assert.Equal(t, "token [REDACTED]", out.SystemPrompt)
	})

	// transformed_input is applied as the whole input, so a part the clone drops
	// is a part the host stops sending. The transform does not rewrite media
	// fields, but it still has to carry them.
	t.Run("a media part survives a rewrite elsewhere", func(t *testing.T) {
		in := agento11y.HookInput{Messages: []agento11y.Message{{
			Role: agento11y.RoleUser,
			Parts: []agento11y.Part{
				{Kind: agento11y.PartKindText, Text: "look at this, key sk-abc123"},
				{Kind: agento11y.PartKindMedia, Media: &agento11y.Media{Kind: "image", URL: "https://example.test/a.png", MIMEType: "image/png"}},
			},
		}}}
		out, changed, _ := ApplyTransform(in, mustCompile(TransformPattern{Regex: "sk-[a-z0-9]+"}), nil)
		require.True(t, changed)
		require.Len(t, out.Messages[0].Parts, 2)
		media := out.Messages[0].Parts[1].Media
		require.NotNil(t, media, "the media part must reach the host that applies the transform")
		assert.Equal(t, "https://example.test/a.png", media.URL)
		assert.NotSame(t, in.Messages[0].Parts[1].Media, media, "input must not be mutated")
	})

	t.Run("relay mode redacts every payload field regardless of part kind", func(t *testing.T) {
		secret := "sk-abc123"
		in := agento11y.HookInput{Output: []agento11y.Message{{
			Role: agento11y.RoleAssistant,
			Parts: []agento11y.Part{{
				Kind:     agento11y.PartKindToolCall,
				Text:     secret,
				Thinking: secret,
				ToolCall: &agento11y.ToolCall{Name: "Bash", InputJSON: json.RawMessage(`{"command":"sk-abc123"}`)},
				ToolResult: &agento11y.ToolResult{
					Content: secret, ContentJSON: json.RawMessage(`{"result":"sk-abc123"}`),
				},
				Media: &agento11y.Media{Kind: "image", URL: "https://example.test/sk-abc123.png", MIMEType: "image/sk-abc123", Name: "sk-abc123.png"},
			}},
		}}}
		out, changed, dropped := ApplyRelayTransform(in, mustCompile(TransformPattern{Regex: "sk-[a-z0-9]+", Replacement: "[REDACTED]"}), nil)
		require.True(t, changed)
		assert.Empty(t, dropped)
		part := out.Output[0].Parts[0]
		assert.Equal(t, "[REDACTED]", part.Text)
		assert.Equal(t, "[REDACTED]", part.Thinking)
		assert.JSONEq(t, `{"command":"[REDACTED]"}`, string(part.ToolCall.InputJSON))
		assert.Equal(t, "[REDACTED]", part.ToolResult.Content)
		assert.JSONEq(t, `{"result":"[REDACTED]"}`, string(part.ToolResult.ContentJSON))
		assert.Equal(t, "https://example.test/[REDACTED].png", part.Media.URL)
		assert.Equal(t, "[REDACTED].png", part.Media.Name)
		assert.Equal(t, "image/sk-abc123", part.Media.MIMEType, "the media encoding is not content")
		assert.Equal(t, secret, in.Output[0].Parts[0].Thinking, "input must not be mutated")
	})

	t.Run("no match is a clone with no change", func(t *testing.T) {
		in := agento11y.HookInput{SystemPrompt: "nothing secret here"}
		out, changed, _ := ApplyTransform(in, mustCompile(TransformPattern{Regex: "sk-[a-z0-9]+"}), nil)
		assert.False(t, changed)
		assert.Equal(t, in.SystemPrompt, out.SystemPrompt)
	})
}

func TestMatchRule(t *testing.T) {
	ctx := agento11y.HookContext{
		AgentName:    "claude-code",
		AgentVersion: "v2",
		Model:        &agento11y.HookModel{Provider: "anthropic", Name: "claude-opus-4-8"},
		Tags:         map[string]string{"env": "prod", "cwd": "/repos/myproject/sub"},
	}
	cases := []struct {
		name  string
		match map[string]any
		want  bool
	}{
		{name: "empty match", match: nil, want: true},
		{name: "agent_name glob", match: map[string]any{"agent_name": "claude-*"}, want: true},
		{name: "agent_name mismatch", match: map[string]any{"agent_name": "codex"}, want: false},
		{name: "model.name list", match: map[string]any{"model.name": []any{"gpt-4o", "claude-opus-4-8"}}, want: true},
		{name: "model.provider", match: map[string]any{"model.provider": "anthropic"}, want: true},
		{name: "tag exact", match: map[string]any{"tags.env": "prod"}, want: true},
		{name: "tag mismatch", match: map[string]any{"tags.env": "dev"}, want: false},
		// Tag values glob like every other match key, and * spans "/" so the
		// documented workspace form matches a nested directory.
		{name: "tag glob spans path separators", match: map[string]any{"tags.cwd": "/repos/myproject*"}, want: true},
		{name: "tag glob other workspace", match: map[string]any{"tags.cwd": "/repos/other*"}, want: false},
		{name: "comma-separated alternatives", match: map[string]any{"agent_name": "codex, claude-code"}, want: true},
		{name: "comma-separated no match", match: map[string]any{"agent_name": "codex, cursor"}, want: false},
		{name: "two conditions both hold", match: map[string]any{"agent_name": "claude-*", "tags.env": "prod"}, want: true},
		{name: "two conditions one fails", match: map[string]any{"agent_name": "claude-*", "tags.env": "dev"}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			compiled, err := compileMatch(tc.match)
			require.NoError(t, err)
			assert.Equal(t, tc.want, matchRule(compiled, ctx))
		})
	}
}

// A match block the local engine cannot evaluate takes the rule out of
// enforcement either way. Reporting it is what separates a rule the user can
// fix from one that silently reads no call.
func TestCompileMatch_RejectsAConditionThatCanNeverHold(t *testing.T) {
	cases := []struct {
		name     string
		match    map[string]any
		wantText string
	}{
		{name: "unknown key", match: map[string]any{"weird_key": "x"}, wantText: `match key "weird_key" is not one of`},
		{name: "tags. with no tag name", match: map[string]any{"tags.": "x"}, wantText: `names no tag`},
		{name: "no value", match: map[string]any{"agent_name": ""}, wantText: "has no value to match against"},
		{name: "value is not a string", match: map[string]any{"agent_name": 42}, wantText: "has no value to match against"},
		{name: "value is not a valid glob", match: map[string]any{"agent_name": "claude-["}, wantText: `match.agent_name: pattern "claude-["`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := compileMatch(tc.match)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantText)
		})
	}
}

// Map iteration order is random, so a match block with two bad keys would
// otherwise report a different one on each run, and a rule the user just fixed
// would come back complaining about the other.
func TestCompileMatch_ReportsTheSameKeyOnEveryRun(t *testing.T) {
	match := map[string]any{"zzz_unknown": "x", "aaa_unknown": "x", "mmm_unknown": "x"}
	for range 20 {
		_, err := compileMatch(match)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `"aaa_unknown"`)
	}
}

func TestGuardEvaluate_EvaluatorOnlyRuleSkippedAndLogged(t *testing.T) {
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	path := writeGuardsFile(t, `
[[rules]]
rule_id = "judge-only"
enabled = true
phase = "postflight"
evaluator_ids = ["guard.content_moderation"]
`)
	rules := loadRulesForTest(t, path)

	resp := evaluateForTest(rules, logger, agento11y.HookEvaluateRequest{
		Phase: agento11y.HookPhasePostflight,
		Input: agento11y.HookInput{Output: []agento11y.Message{{
			Role:  agento11y.RoleAssistant,
			Parts: []agento11y.Part{{Kind: agento11y.PartKindText, Text: "hi"}},
		}}},
	})

	assert.Equal(t, agento11y.HookActionAllow, resp.Action)
	assert.Contains(t, buf.String(), "nothing enforceable locally")
	assert.Contains(t, buf.String(), "judge-only")
}

// TestEvaluateGuards_TransformCannotHideALaterRule pins the ordering that makes
// a ruleset safe to combine: every check reads the request as it arrived, so a
// redaction cannot erase the text another rule blocks on. Priority decides only
// what the host runs.
func TestEvaluateGuards_TransformCannotHideALaterRule(t *testing.T) {
	path := writeGuardsFile(t, `
[[rules]]
rule_id = "redact-path"
priority = 0
transform.patterns = [{ regex = "/etc/passwd", replacement = "/tmp/x" }]

[[rules]]
rule_id = "block-passwd"
priority = 10
tool_filter.blocked_names = ["Read(*/etc/passwd*)"]
`)

	resp := evaluateForTest(loadRulesForTest(t, path), nil, agento11y.HookEvaluateRequest{
		Phase: agento11y.HookPhasePostflight,
		Input: toolCallInput("output", "Read", `{"path":"/etc/passwd"}`),
	})

	assert.Equal(t, agento11y.HookActionDeny, resp.Action)
	assert.Equal(t, "block-passwd", resp.RuleID)
	assert.Nil(t, resp.TransformedInput, "a deny does not rewrite a call that will not run")
}

// TestEvaluateGuards_DroppedRedactionIsReported covers a redaction the engine
// had to throw away. The rule keeps reporting as enforcing, so the response is
// the only place the caller can learn the value is still in the call.
func TestEvaluateGuards_DroppedRedactionIsReported(t *testing.T) {
	// The pattern is a TOML literal string, so the quotes and the \s need no
	// escaping on top of the regex's own.
	path := writeGuardsFile(t, `
[[rules]]
rule_id = "eat-the-pair"
transform.patterns = [{ regex = '"password":\s*"[^"]*"', replacement = "" }]
`)

	resp := evaluateForTest(loadRulesForTest(t, path), nil, agento11y.HookEvaluateRequest{
		Phase: agento11y.HookPhasePostflight,
		Input: toolCallInput("output", "Bash", `{"password":"hunter2","command":"ls"}`),
	})

	assert.Equal(t, agento11y.HookActionAllow, resp.Action)
	assert.Nil(t, resp.TransformedInput)
	require.Len(t, resp.Evaluations, 1)
	assert.Equal(t, "eat-the-pair", resp.Evaluations[0].RuleID)
	assert.Equal(t, "transform", resp.Evaluations[0].EvaluatorKind)
	assert.False(t, resp.Evaluations[0].Passed)
	assert.Contains(t, resp.Evaluations[0].Reason, "tool_call.input_json")
}

func TestFromHookResponse_CarriesEveryCloudEvaluationField(t *testing.T) {
	resp := FromHookResponse(agento11y.HookEvaluateResponse{
		Action: agento11y.HookActionDeny,
		RuleID: "cloud-rule",
		Reason: "blocked",
		Evaluations: []agento11y.HookEvaluation{{
			RuleID:        "cloud-rule",
			EvaluatorID:   "pii.v2",
			EvaluatorKind: "llm_judge",
			Passed:        false,
			LatencyMs:     42,
			Explanation:   "found an address",
			Reason:        "pii",
		}},
	})

	require.Len(t, resp.Evaluations, 1)
	assert.Equal(t, Evaluation{
		RuleID:        "cloud-rule",
		EvaluatorID:   "pii.v2",
		EvaluatorKind: "llm_judge",
		Passed:        false,
		LatencyMs:     42,
		Explanation:   "found an address",
		Reason:        "pii",
	}, resp.Evaluations[0])
}
