package guardeval

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTransformJSONModes(t *testing.T) {
	for _, tc := range []struct {
		name, mode, input, want string
		patterns                []TransformPattern
		dropped                 bool
	}{
		{"unicode prefix", "strings", `{"x":"\u0073ecret"}`, `{"x":"X"}`, []TransformPattern{{Regex: `\bsecret\b`, Replacement: "X"}}, false},
		{"newline and tab", "strings", `{"x":"\nsecret\tsecret"}`, `{"x":"\nX\tX"}`, []TransformPattern{{Regex: `\bsecret\b`, Replacement: "X"}}, false},
		{"unicode suffix", "strings", `{"x":"secre\u0074"}`, `{"x":"X"}`, []TransformPattern{{Regex: `secret`, Replacement: "X"}}, false},
		{"keys and duplicates", "strings", ` { "\u0073ecret": "secret", "secret": 9007199254740993, "n":1.2300e+40, "untouched":"\u0061\/b" } `, ` { "\u0073ecret": "X", "secret": 9007199254740993, "n":1.2300e+40, "untouched":"\u0061\/b" } `, []TransformPattern{{Regex: `secret`, Replacement: "X"}}, false},
		{"keys only", "strings", `{"secret":1}`, `{"secret":1}`, []TransformPattern{{Regex: `secret`, Replacement: "X"}}, false},
		{"nested keys", "strings", "{\"secret\" \n: {\"secret\": [\"secret\",{\"secret\":\"secret\"}]}}", "{\"secret\" \n: {\"secret\": [\"X\",{\"secret\":\"X\"}]}}", []TransformPattern{{Regex: `secret`, Replacement: "X"}}, false},
		{"no match keeps encoding", "strings", `["\u0061",9007199254740993]`, `["\u0061",9007199254740993]`, []TransformPattern{{Regex: `secret`, Replacement: "X"}}, false},
		{"surrogate pair", "strings", `"\ud83d\udd12 secret"`, `"lock X"`, []TransformPattern{{Regex: "🔒", Replacement: "lock"}, {Regex: "secret", Replacement: "X"}}, false},
		{"literal replacement", "strings", `"secret"`, `"$1\"\n\\"`, []TransformPattern{{Regex: `(secret)`, Replacement: "$1\"\n\\"}}, false},
		{"apply each pattern once", "strings", `"\u0073ecret"`, `"secretsecret"`, []TransformPattern{{Regex: `secret`, Replacement: "secretsecret"}}, false},
		{"restored token keeps encoding", "strings", `"\u0073ecret"`, `"\u0073ecret"`, []TransformPattern{{Regex: `secret`, Replacement: "stage"}, {Regex: `stage`, Replacement: "secret"}}, false},
		{"ordered overlapping patterns", "strings", `"\u0061b"`, `"done"`, []TransformPattern{{Regex: `ab`, Replacement: "bc"}, {Regex: `bc`, Replacement: "done"}}, false},
		{"anchors per token", "strings", `["secret","\u0073ecret","secret suffix"]`, `["X","X","secret suffix"]`, []TransformPattern{{Regex: `^secret$`, Replacement: "X"}}, false},
		{"not raw spanning in strings mode", "strings", `{"secret":"value"}`, `{"secret":"value"}`, []TransformPattern{{Regex: `"secret":"value"`, Replacement: `"done":42`}}, false},
		{"raw default spans tokens", "", `{"secret":"value"}`, `{"done":42}`, []TransformPattern{{Regex: `"secret":"value"`, Replacement: `"done":42`}}, false},
		{"explicit raw spans tokens", "raw", `{"secret":"value"}`, `{"done":42}`, []TransformPattern{{Regex: `"secret":"value"`, Replacement: `"done":42`}}, false},
		{"raw retains encoded spelling", "", `"\u0073ecret"`, `"\u0073ecret"`, []TransformPattern{{Regex: `secret`, Replacement: "X"}}, false},
		{"raw invalid rewrite reported", "raw", `{"x":"secret"}`, `{"x":"secret"}`, []TransformPattern{{Regex: `secret`, Replacement: `broken"`}}, true},
		{"invalid input strings", "strings", `{"secret":`, `{"secret":`, []TransformPattern{{Regex: `secret`, Replacement: "X"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transform, err := compileTransform(&TransformConfig{JSONMode: tc.mode, Patterns: tc.patterns})
			require.NoError(t, err)
			got, changed, dropped := applyRawJSON(json.RawMessage(tc.input), transform, nil, "test")
			assert.Equal(t, tc.want, string(got))
			assert.Equal(t, tc.input != tc.want, changed)
			assert.Equal(t, tc.dropped, dropped)
		})
	}
}

func TestTransformModeComposition(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
		configs           []TransformConfig
		drops             int
	}{
		{"raw strings raw", `{"value":"secret"}`, `{"value":"complete"}`, []TransformConfig{
			{Patterns: []TransformPattern{{Regex: `"value":"secret"`, Replacement: `"value":"stage"`}}},
			{JSONMode: "strings", Patterns: []TransformPattern{{Regex: `^stage$`, Replacement: "done"}}},
			{JSONMode: "raw", Patterns: []TransformPattern{{Regex: `"done"`, Replacement: `"complete"`}}},
		}, 0},
		{"strings raw strings", `{"value":"\u0073ecret"}`, `{"value":"complete"}`, []TransformConfig{
			{JSONMode: "strings", Patterns: []TransformPattern{{Regex: `^secret$`, Replacement: "stage"}}},
			{Patterns: []TransformPattern{{Regex: `"value":"stage"`, Replacement: `"value":"done"`}}},
			{JSONMode: "strings", Patterns: []TransformPattern{{Regex: `^done$`, Replacement: "complete"}}},
		}, 0},
		{"invalid raw then strings", `{"value":"secret"}`, `{"value":"done"}`, []TransformConfig{
			{Patterns: []TransformPattern{{Regex: `secret`, Replacement: `broken"`}}},
			{JSONMode: "strings", Patterns: []TransformPattern{{Regex: `secret`, Replacement: "done"}}},
		}, 1},
		{"raw rules must not merge", `{"value":"secret"}`, `{"value":"secret"}`, []TransformConfig{
			{Patterns: []TransformPattern{{Regex: `secret`, Replacement: `broken"`}}},
			{Patterns: []TransformPattern{{Regex: `broken"`, Replacement: `done`}}},
		}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var raw []json.RawMessage
			for i, cfg := range tc.configs {
				rule, err := json.Marshal(Rule{RuleID: string(rune('a' + i)), Priority: i, Transform: &cfg})
				require.NoError(t, err)
				raw = append(raw, rule)
			}
			engine := NewRulesEngine(raw, nil)
			require.Empty(t, engine.Status().Errors)
			in := agento11y.HookInput{Output: []agento11y.Message{{Role: "assistant", Parts: []agento11y.Part{{
				Kind:     agento11y.PartKindToolCall,
				ToolCall: &agento11y.ToolCall{ID: "c1", Name: "Bash", InputJSON: json.RawMessage(tc.input)},
			}}}}}
			resp, transform, err := engine.EvaluateWithTransform(context.Background(), agento11y.HookEvaluateRequest{Phase: agento11y.HookPhasePostflight, Input: in})
			require.NoError(t, err)
			require.NotNil(t, transform)
			require.Len(t, resp.Evaluations, tc.drops)
			if tc.input == tc.want {
				assert.Nil(t, resp.TransformedInput)
			} else {
				require.NotNil(t, resp.TransformedInput)
				assert.Equal(t, tc.want, string(resp.TransformedInput.Output[0].Parts[0].ToolCall.InputJSON))
			}
			for _, apply := range []func(agento11y.HookInput, *Transform) (agento11y.HookInput, bool, []string){
				func(in agento11y.HookInput, ct *Transform) (agento11y.HookInput, bool, []string) {
					return ApplyTransform(in, ct, nil)
				},
				func(in agento11y.HookInput, ct *Transform) (agento11y.HookInput, bool, []string) {
					return ApplyRelayTransform(in, ct, nil)
				},
			} {
				got, changed, dropped := apply(in, transform)
				assert.Equal(t, tc.want, string(got.Output[0].Parts[0].ToolCall.InputJSON))
				assert.Equal(t, tc.input != tc.want, changed)
				assert.Len(t, dropped, tc.drops)
			}
			assert.Equal(t, tc.input, string(in.Output[0].Parts[0].ToolCall.InputJSON))
		})
	}
}

func TestStringModeAcrossPayloads(t *testing.T) {
	transform, err := compileTransform(&TransformConfig{JSONMode: "strings", Patterns: []TransformPattern{{Regex: `\bsecret\b`, Replacement: "X"}}})
	require.NoError(t, err)
	parts := []agento11y.Part{
		{Kind: agento11y.PartKindText, Text: "secret"},
		{Kind: agento11y.PartKindThinking, Thinking: "secret"},
		{Kind: agento11y.PartKindToolCall, ToolCall: &agento11y.ToolCall{InputJSON: json.RawMessage(`{"command":"\u0073ecret"}`)}},
		{Kind: agento11y.PartKindToolResult, ToolResult: &agento11y.ToolResult{Content: "secret", ContentJSON: json.RawMessage(`"\u0073ecret"`)}},
	}
	in := agento11y.HookInput{
		SystemPrompt: "secret", ConversationPreview: "secret",
		Messages: []agento11y.Message{{Parts: parts}}, Output: []agento11y.Message{{Parts: parts}},
		Tools: []agento11y.ToolDefinition{{Description: "secret", InputSchema: json.RawMessage(`{"description":"\u0073ecret"}`)}},
	}
	before, err := json.Marshal(in)
	require.NoError(t, err)
	for _, relay := range []bool{false, true} {
		out, changed, dropped := applyTransform(in, transform, nil, relay)
		require.True(t, changed)
		require.Empty(t, dropped)
		assert.Equal(t, "X", out.SystemPrompt)
		assert.Equal(t, "X", out.ConversationPreview)
		assert.Equal(t, "X", out.Tools[0].Description)
		assert.Equal(t, `{"description":"X"}`, string(out.Tools[0].InputSchema))
		for _, messages := range [][]agento11y.Message{out.Messages, out.Output} {
			assert.Equal(t, "X", messages[0].Parts[0].Text)
			wantThinking := "secret"
			if relay {
				wantThinking = "X"
			}
			assert.Equal(t, wantThinking, messages[0].Parts[1].Thinking)
			assert.Equal(t, `{"command":"X"}`, string(messages[0].Parts[2].ToolCall.InputJSON))
			assert.Equal(t, "X", messages[0].Parts[3].ToolResult.Content)
			assert.Equal(t, `"X"`, string(messages[0].Parts[3].ToolResult.ContentJSON))
		}
	}
	after, err := json.Marshal(in)
	require.NoError(t, err)
	assert.Equal(t, before, after)
}
