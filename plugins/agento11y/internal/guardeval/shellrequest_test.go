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
		{"empty", "Bash", `{"command":""}`, []ShellMetadata{{Dialect: "bash"}}, false},
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
		name                  string
		calls                 []toolCallSpec
		metadata              []ShellMetadata
		ran, passed, fallback bool
	}{
		{"parsed data", []toolCallSpec{{"Bash", `{"command":"echo \"rm -rf ./src\""}`}, {"Bash", `{"command":"echo safe"}`}}, []ShellMetadata{{Dialect: "bash"}, {PartIndex: 1, Dialect: "bash"}}, true, true, false},
		{"parsed denial", []toolCallSpec{{"Bash", `{"command":"echo safe"}`}, {"Bash", `{"command":"rm -rf ./src"}`}}, []ShellMetadata{{Dialect: "bash"}, {PartIndex: 1, Dialect: "bash"}}, true, false, false},
		{"separate subjects", []toolCallSpec{{"Bash", `{"command":"echo rm"}`}, {"Bash", `{"command":"echo -rf"}`}}, nil, true, true, true},
		{"unverified data", []toolCallSpec{{"Bash", `{"command":"echo \"rm -rf ./src\""}`}}, nil, true, false, true},
		{"nested command", []toolCallSpec{{"local_shell", `{"action":{"type":"exec","command":["bash","-c","rm -rf ./src"]}}`}}, nil, true, false, true},
		{"second command key", []toolCallSpec{{"Bash", `{"command":"echo safe","cmd":"rm -rf ./src"}`}}, nil, true, false, true},
		{"unknown key", []toolCallSpec{{"Bash", `{"payload":"rm -rf ./src"}`}}, nil, true, false, true},
		{"invalid JSON", []toolCallSpec{{"Bash", `rm -rf ./src`}}, nil, true, false, true},
		{"argv compatibility subject", []toolCallSpec{{"Bash", `{"command":["echo","rm -rf ./src"]}`}}, nil, true, false, true},
		{"incomplete uses all keys", []toolCallSpec{{"Bash", `{"command":"go test ./...","script":"rm -rf ./src"}`}}, []ShellMetadata{{Dialect: "bash"}}, true, false, true},
		{"verified command only", []toolCallSpec{{"Bash", `{"command":"echo safe","script":"rm -rf ./src"}`}}, []ShellMetadata{{Dialect: "bash"}}, true, true, false},
		{"fallback stays within call", []toolCallSpec{{"Bash", `{"command":"echo \"rm -rf ./src\""}`}, {"shell", `{"command":"echo safe"}`}}, []ShellMetadata{{Dialect: "bash"}}, true, true, true},
		{"empty arguments", []toolCallSpec{{"Bash", `{}`}}, nil, false, true, false},
		{"empty verified command", []toolCallSpec{{"Bash", `{"command":" "}`}}, []ShellMetadata{{Dialect: "bash"}}, false, true, false},
		{"at limit with metadata", []toolCallSpec{{"Bash", `{"command":"echo ` + strings.Repeat("a", maxShellCommandBytes-len("echo ")) + `"}`}}, []ShellMetadata{{Dialect: "bash"}}, true, true, false},
		{"non-shell tool", []toolCallSpec{{"Read", `{"command":"rm -rf ./src"}`}}, nil, false, true, false},
		{"over limit", []toolCallSpec{{"Bash", `{"command":"echo ` + strings.Repeat("a", maxShellCommandBytes) + `; r\\m -rf ./src"}`}}, nil, true, false, true},
		{"over limit with metadata", []toolCallSpec{{"Bash", `{"command":"echo ` + strings.Repeat("a", maxShellCommandBytes) + `; r\\m -rf ./src"}`}}, []ShellMetadata{{Dialect: "bash"}}, true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := toolCalls(tc.calls...).Input
			for _, part := range in.Output[0].Parts {
				part.ToolCall.ID = "same"
			}
			cache := newShellRequestCache(context.Background(), tc.metadata)
			ran, passed, explanation := evaluator.evaluate(in, cache)
			assert.Equal(t, tc.ran, ran)
			assert.Equal(t, tc.passed, passed)
			assert.Equal(t, tc.fallback, strings.Contains(explanation, "incomplete"), explanation)
			if !tc.passed {
				assert.Contains(t, explanation, "blocked by destructive policy")
				assert.NotContains(t, explanation, "operation")
			}
			for i, call := range tc.calls {
				if evaluator.fallback.shell.matchesTool(call.name) {
					assert.Contains(t, cache.results, shellCallPosition{part: i})
				}
			}
		})
	}
}
