package guardeval

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShellRequestMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, tool, input string
		metadata          []ShellMetadata
		complete          bool
	}{
		{"bash metadata", "Bash", `{"command":"echo safe"}`, []ShellMetadata{{Dialect: "bash"}}, true},
		{"missing", "Bash", `{"command":"echo safe"}`, nil, false},
		{"wrong dialect", "Bash", `{"command":"echo safe"}`, []ShellMetadata{{Dialect: "fish"}}, false},
		{"generic tool", "shell", `{"command":"echo safe"}`, []ShellMetadata{{Dialect: "bash"}}, false},
		{"array", "Bash", `{"command":["echo","safe"]}`, []ShellMetadata{{Dialect: "bash"}}, false},
		{"null", "Bash", `{"command":null}`, []ShellMetadata{{Dialect: "bash"}}, false},
		{"duplicate", "Bash", `{"command":"echo safe"}`, []ShellMetadata{{Dialect: "bash"}, {Dialect: "bash"}}, false},
		{"wrong position", "Bash", `{"command":"echo safe"}`, []ShellMetadata{{PartIndex: 1, Dialect: "bash"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := newShellRequestCache(context.Background(), tc.metadata)
			analysis := cache.analysis(shellCallPosition{}, &agento11y.ToolCall{Name: tc.tool, InputJSON: json.RawMessage(tc.input)})
			assert.Equal(t, tc.complete, analysis.incomplete == "", analysis.incomplete)
		})
	}
}

func TestShellRequestIsolation(t *testing.T) {
	ce, err := compileEvaluator(EvaluatorSpec{Kind: "regex", Config: map[string]any{"target": "shell_command", "pattern": `rm.*-rf`, "reject": true}})
	require.NoError(t, err)
	evaluator := &shellPolicyEvaluator{profile: ShellPolicyDestructive, fallback: ce.(*regexEvaluator)}
	for _, tc := range []struct {
		commands []string
		metadata []ShellMetadata
		passed   bool
		fallback bool
	}{
		{[]string{`echo "rm -rf ./src"`, `echo safe`}, []ShellMetadata{{Dialect: "bash"}, {PartIndex: 1, Dialect: "bash"}}, true, false},
		{[]string{`echo safe`, `rm -rf ./src`}, []ShellMetadata{{Dialect: "bash"}, {PartIndex: 1, Dialect: "bash"}}, false, false},
		{[]string{`echo rm`, `echo -rf`}, nil, true, true},
		{[]string{`echo "rm -rf ./src"`}, nil, false, true},
	} {
		t.Run(strings.Join(tc.commands, "/"), func(t *testing.T) {
			var parts []agento11y.Part
			for _, command := range tc.commands {
				input, err := json.Marshal(map[string]string{"command": command})
				require.NoError(t, err)
				parts = append(parts, agento11y.ToolCallPart(agento11y.ToolCall{ID: "same", Name: "Bash", InputJSON: input}))
			}
			in := agento11y.HookInput{Output: []agento11y.Message{{Role: agento11y.RoleAssistant, Parts: parts}}}
			cache := newShellRequestCache(context.Background(), tc.metadata)
			ran, passed, explanation := evaluator.evaluate(in, cache)
			assert.True(t, ran)
			assert.Equal(t, tc.passed, passed)
			assert.Equal(t, tc.fallback, strings.Contains(explanation, "incomplete"), explanation)
			if !tc.passed {
				assert.Contains(t, explanation, "blocked by destructive policy")
				assert.NotContains(t, explanation, "operation")
			}
			assert.Len(t, cache.results, len(tc.commands))
		})
	}
}
