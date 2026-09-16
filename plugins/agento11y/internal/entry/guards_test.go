package entry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/guardeval"
	"github.com/grafana/agento11y/plugins/agento11y/internal/local"
	"github.com/grafana/agento11y/plugins/agento11y/internal/login"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const guardsDenyRules = `[[rules]]
rule_id = "deny"
priority = 30
tool_filter.blocked_names = ["Bash(*secret*)"]
`
const guardsWarnRules = `[[rules]]
rule_id = "warn"
priority = 10
action_on_fail = "warn"
tool_filter.blocked_names = ["Bash"]
`
const guardsRedactRules = `[[rules]]
rule_id = "redact"
priority = 5
transform.json_mode = "strings"
transform.patterns = [{ regex = 'secret', replacement = 'SAFE' }]
`
const guardsExceptionRules = `[[rules]]
rule_id = "exception"
priority = 20
action_on_fail = "allow"
tool_filter.blocked_names = ["Bash"]
`
const guardsBrokenRules = `[[rules]]
rule_id = "broken"
transform.patterns = [{ regex = '(' }]
`

func guardsWriteFile(t *testing.T, path, contents string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
}

func guardsKeys(t *testing.T, raw json.RawMessage, want ...string) map[string]json.RawMessage {
	t.Helper()
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &fields))
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	assert.ElementsMatch(t, want, keys)
	return fields
}

// A single Decode into guardsTestReport accepts unknown keys, null arrays, and trailing documents.
func guardsDecodeReport(t *testing.T, data []byte) guardsTestReport {
	t.Helper()
	require.True(t, bytes.HasSuffix(data, []byte("\n")))
	decoder := json.NewDecoder(bytes.NewReader(data))
	var raw json.RawMessage
	require.NoError(t, decoder.Decode(&raw))
	var extra any
	require.ErrorIs(t, decoder.Decode(&extra), io.EOF)
	fields := guardsKeys(t, raw, "schema_version", "scope", "rules", "request", "response", "errors", "notices")
	guardsKeys(t, fields["rules"], "path", "exists", "compiled", "enforcing")
	guardsKeys(t, fields["request"], "command", "tool_name", "agent_name")
	for _, key := range []string{"errors", "notices"} {
		require.True(t, bytes.HasPrefix(fields[key], []byte("[")), "%s must be an array: %s", key, fields[key])
	}
	var report guardsTestReport
	require.NoError(t, json.Unmarshal(raw, &report))
	assert.Equal(t, 1, report.SchemaVersion)
	assert.Equal(t, "local", report.Scope)
	if report.Response != nil {
		keys := []string{"action", "evaluations"}
		if report.Response.RuleID != "" {
			keys = append(keys, "rule_id")
		}
		if report.Response.Reason != "" {
			keys = append(keys, "reason")
		}
		if report.Response.TransformedInput != nil {
			keys = append(keys, "transformed_input")
		}
		response := guardsKeys(t, fields["response"], keys...)
		require.True(t, bytes.HasPrefix(response["evaluations"], []byte("[")))
		var evaluations []json.RawMessage
		require.NoError(t, json.Unmarshal(response["evaluations"], &evaluations))
		for i, evaluation := range report.Response.Evaluations {
			keys := []string{"rule_id", "evaluator_id", "evaluator_kind", "passed", "latency_ms"}
			if evaluation.Effect != "" {
				keys = append(keys, "effect")
			}
			if evaluation.Reason != "" {
				keys = append(keys, "reason")
			}
			if evaluation.Explanation != "" {
				keys = append(keys, "explanation")
			}
			guardsKeys(t, evaluations[i], keys...)
			assert.Empty(t, evaluation.EvaluatorID)
			assert.Zero(t, evaluation.LatencyMs)
		}
	} else {
		assert.Equal(t, "null", string(fields["response"]))
	}
	return report
}

func guardsRunJSON(t *testing.T, args []string, stdin io.Reader, wantCode int) (guardsTestReport, []byte) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runGuardsCommand(append([]string{"test", "--json"}, args...), stdin, &stdout, &stderr)
	require.Equal(t, wantCode, code, "stdout=%s stderr=%s", &stdout, &stderr)
	require.Empty(t, stderr.String())
	return guardsDecodeReport(t, stdout.Bytes()), stdout.Bytes()
}

type guardsReadError struct{}

func (guardsReadError) Read([]byte) (int, error) { return 0, errors.New("stdin unavailable") }

func TestGuardsValidation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		args  []string
		stdin io.Reader
		want  []string
	}{
		{"no command", nil, nil, []string{"exactly one command argument or --stdin is required; quote the command"}},
		{"blank", []string{""}, nil, []string{"command must not be blank"}},
		{"whitespace", []string{" \t\n"}, nil, []string{"command must not be blank"}},
		{"unquoted arity", []string{"echo", "hello"}, nil, []string{"exactly one command argument or --stdin is required; quote the command"}},
		{"flags after positional", []string{"echo hello", "--tool", "shell"}, nil, []string{"exactly one command argument or --stdin is required; quote the command"}},
		{"stdin conflict", []string{"--stdin", "echo"}, guardsReadError{}, []string{"use either --stdin or one command argument, not both"}},
		{"stdin empty", []string{"--stdin"}, strings.NewReader(""), []string{"command must not be blank"}},
		{"stdin blank", []string{"--stdin"}, strings.NewReader(" \n\t"), []string{"command must not be blank"}},
		{"stdin error", []string{"--stdin"}, guardsReadError{}, []string{"read stdin: stdin unavailable"}},
		{"empty tool", []string{"--tool=", "echo"}, nil, []string{"tool name must not be blank"}},
		{"blank tool", []string{"--tool", " \t", "echo"}, nil, []string{"tool name must not be blank"}},
		{"empty rules", []string{"--rules=", "echo"}, nil, []string{"rules path must not be blank"}},
		{"blank rules", []string{"--rules", " \t", "echo"}, nil, []string{"rules path must not be blank"}},
		{"multiple errors", []string{"--rules=", "--tool=", ""}, nil, []string{"command must not be blank", "tool name must not be blank", "rules path must not be blank"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateDotenvHome(t)
			report, _ := guardsRunJSON(t, tc.args, tc.stdin, 2)
			assert.Equal(t, tc.want, report.Errors)
			assert.Nil(t, report.Response)
			assert.Empty(t, report.Notices)
			assert.False(t, report.Rules.Exists)
			assert.Zero(t, report.Rules.Compiled)
		})
	}
}

func TestGuardsPlainValidationOutput(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args       []string
		diagnostic string
	}{
		{"blank", []string{"test", ""}, "command must not be blank"},
		{"late json is positional", []string{"test", "echo safe", "--json"}, "exactly one command argument or --stdin is required; quote the command"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateDotenvHome(t)
			var stdout, stderr bytes.Buffer
			assert.Equal(t, 2, runGuardsCommand(tc.args, nil, &stdout, &stderr))
			assert.Empty(t, stderr.String())
			assert.Equal(t, "error (local dry run)\nrules path: \nenforceable rules: 0\nerror: "+tc.diagnostic+"\n", stdout.String())
		})
	}
}

func TestGuardsParseErrorsAndHelp(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args       []string
		diagnostic string
	}{
		{"no verb", nil, guardsUsage + "\n"},
		{"unknown verb", []string{"show"}, guardsUsage + "\n"},
		{"unknown flag", []string{"test", "--json", "--unknown", "echo"}, "agento11y: flag provided but not defined: -unknown\n" + guardsUsage + "\n"},
		{"missing flag value", []string{"test", "--rules"}, "agento11y: flag needs an argument: -rules\n" + guardsUsage + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			assert.Equal(t, 2, runGuardsCommand(tc.args, nil, &stdout, &stderr))
			assert.Empty(t, stdout.String())
			assert.Equal(t, tc.diagnostic, stderr.String())
		})
	}
	for _, args := range [][]string{{"help"}, {"--help"}, {"-h"}, {"test", "--help"}, {"test", "-h"}, {"test", "-help"}, {"test", "--json", "--help"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			isolateDotenvHome(t)
			var stdout, stderr bytes.Buffer
			code := withExit(t, func() { run(append([]string{"guards"}, args...), guardsReadError{}, &stdout, &stderr) })
			require.NotNil(t, code)
			assert.Zero(t, *code)
			assert.Equal(t, guardsHelp, stdout.String())
			for _, text := range []string{
				"without executing it", "nor checks Cloud rules", "even when host hooks are disabled",
				"--stdin reads one command through EOF", "no model, tags, agent version, preflight messages, or system prompt",
				"0 = completed allow", "1 = completed deny", "2 = input, file, compilation, evaluation, or output error",
				"schema_version: 1", "Do not use real secrets in shared test output",
			} {
				assert.Contains(t, stdout.String(), text)
			}
			assert.Empty(t, stderr.String())
		})
	}
}

func TestGuardsSyntheticRequest(t *testing.T) {
	for _, tc := range []struct {
		name, command, tool, agent string
		stdin                      bool
	}{
		{"quoted", `printf '%s' "a b"; echo '\\'`, "Bash", "", false},
		{"multiline stdin", "echo \"first\"\necho 'second'\n", "shell", "pi", true},
		{"dash command", "--not-a-flag", "terminal", "custom", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateDotenvHome(t)
			t.Setenv("AGENTO11Y_AGENT_NAME", "not-the-supplied-agent")
			t.Setenv("AGENTO11Y_TAGS", "team=not-in-request")
			args := []string{"--tool", tc.tool, "--agent", tc.agent}
			var stdin io.Reader = guardsReadError{}
			if tc.stdin {
				args = append(args, "--stdin")
				stdin = strings.NewReader(tc.command)
			} else {
				args = append(args, "--", tc.command)
			}
			report, _ := guardsRunJSON(t, args, stdin, 0)
			assert.Equal(t, guardsTestRequest{Command: tc.command, ToolName: tc.tool, AgentName: tc.agent}, report.Request)
			req, err := report.Request.hookRequest()
			require.NoError(t, err)
			arguments, err := json.Marshal(map[string]string{"command": tc.command})
			require.NoError(t, err)
			expected := agento11y.HookEvaluateRequest{
				Phase:   agento11y.HookPhasePostflight,
				Context: agento11y.HookContext{AgentName: tc.agent},
				Input:   agento11y.HookInput{Output: []agento11y.Message{{Role: agento11y.RoleAssistant, Parts: []agento11y.Part{{Kind: agento11y.PartKindToolCall, ToolCall: &agento11y.ToolCall{ID: "guards-test", Name: tc.tool, InputJSON: arguments}}}}}},
			}
			assert.Equal(t, expected, req)
			if tc.agent == "" {
				assert.Equal(t, agento11y.HookContext{}, req.Context)
			}
		})
	}
}

func TestGuardsDefaultPathResolution(t *testing.T) {
	for _, tc := range []struct {
		name                                      string
		preferred, legacy, explicit, missingRules bool
		wantDir                                   string
		code                                      int
	}{
		{"fresh default", false, false, false, false, "agento11y", 1},
		{"preferred config", true, false, false, false, "agento11y", 1},
		{"legacy config", false, true, false, false, "sigil", 0},
		{"preferred wins both configs", true, true, false, false, "agento11y", 1},
		{"explicit bypasses both", true, true, true, false, "custom", 0},
		{"missing preferred rules does not fall back", true, true, false, true, "agento11y", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := isolateDotenvHome(t)
			root := filepath.Join(home, "config")
			// Config.env, not guards.toml, decides directory precedence.
			if !tc.missingRules {
				guardsWriteFile(t, filepath.Join(root, "agento11y", "guards.toml"), guardsDenyRules)
			}
			legacyRules := "# no rules\n"
			if tc.missingRules {
				legacyRules = guardsDenyRules
			}
			guardsWriteFile(t, filepath.Join(root, "sigil", "guards.toml"), legacyRules)
			if tc.preferred {
				guardsWriteFile(t, filepath.Join(root, "agento11y", "config.env"), "AGENTO11Y_AGENT_NAME=must-not-load\n")
			}
			if tc.legacy {
				guardsWriteFile(t, filepath.Join(root, "sigil", "config.env"), "SIGIL_AGENT_NAME=must-not-load\n")
			}
			args := []string{}
			if tc.explicit {
				path := filepath.Join(root, "custom", "guards.toml")
				guardsWriteFile(t, path, "# explicit empty\n")
				args = append(args, "--rules", path)
			}
			report, _ := guardsRunJSON(t, append(args, "echo secret"), nil, tc.code)
			assert.Equal(t, filepath.Join(root, tc.wantDir, "guards.toml"), report.Rules.Path)
			assert.Equal(t, !tc.missingRules, report.Rules.Exists)
			assert.Empty(t, report.Request.AgentName)
			if tc.missingRules {
				require.NotNil(t, report.Response)
				assert.Equal(t, agento11y.HookActionAllow, report.Response.Action)
				assert.Empty(t, report.Response.Evaluations)
				assert.Equal(t, []string{"no local rules can enforce"}, report.Notices)
			}
		})
	}
}

func TestGuardsRulesFailures(t *testing.T) {
	for _, tc := range []struct {
		name, contents              string
		explicit, exists, directory bool
		code, compiled, enforcing   int
		action, diagnostic          string
	}{
		{name: "default missing allows", action: "allow"},
		{name: "explicit missing errors", explicit: true, code: 2, diagnostic: "read rules:"},
		{name: "explicit directory read error", explicit: true, directory: true, code: 2, diagnostic: "read rules:"},
		{name: "default directory read error", directory: true, code: 2, diagnostic: "read rules:"},
		{name: "unparsable", explicit: true, exists: true, contents: "[[rules", code: 2, diagnostic: "parse "},
		{name: "broken regex only", explicit: true, exists: true, contents: guardsBrokenRules, code: 2, diagnostic: `rule "broken"`},
		{name: "partial broken regex with valid deny", explicit: true, exists: true, contents: guardsBrokenRules + guardsDenyRules, code: 2, compiled: 1, enforcing: 1, action: "deny", diagnostic: `rule "broken"`},
		{name: "empty file", explicit: true, exists: true, action: "allow"},
		{name: "cloud only", explicit: true, exists: true, contents: "[[rules]]\nrule_id='cloud'\nevaluator_ids=['remote']\n", compiled: 1, action: "allow"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := isolateDotenvHome(t)
			path := filepath.Join(home, "config", "agento11y", "guards.toml")
			if tc.exists {
				guardsWriteFile(t, path, tc.contents)
			}
			if tc.directory {
				require.NoError(t, os.MkdirAll(path, 0o700))
			}
			args := []string{}
			if tc.explicit {
				args = append(args, "--rules", path)
			}
			report, _ := guardsRunJSON(t, append(args, "echo secret"), nil, tc.code)
			assert.Equal(t, guardsTestRules{Path: path, Exists: tc.exists, Compiled: tc.compiled, Enforcing: tc.enforcing}, report.Rules)
			if tc.diagnostic == "" {
				assert.Empty(t, report.Errors)
			} else {
				require.NotEmpty(t, report.Errors)
				assert.Contains(t, strings.Join(report.Errors, "\n"), tc.diagnostic)
			}
			if tc.action == "" {
				assert.Nil(t, report.Response)
			} else {
				require.NotNil(t, report.Response)
				assert.Equal(t, tc.action, string(report.Response.Action))
			}
			if tc.diagnostic == "" && tc.enforcing == 0 {
				assert.Equal(t, []string{"no local rules can enforce"}, report.Notices)
			}
			if tc.action == "deny" {
				assert.Equal(t, "deny", report.Response.RuleID)
				require.Len(t, report.Response.Evaluations, 1)
			}
		})
	}
}

func TestGuardsDisabledHookEnvironment(t *testing.T) {
	for _, key := range []string{"AGENTO11Y_GUARDS_ENABLED", "SIGIL_GUARDS_ENABLED"} {
		for _, disabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/rule-disabled=%t", key, disabled), func(t *testing.T) {
				isolateDotenvHome(t)
				t.Setenv(key, "false")
				contents := guardsDenyRules
				code, count := 1, 1
				if disabled {
					contents = strings.Replace(contents, "[[rules]]", "[[rules]]\nenabled = false", 1)
					code, count = 0, 0
				}
				path := filepath.Join(t.TempDir(), "guards.toml")
				guardsWriteFile(t, path, contents)
				report, _ := guardsRunJSON(t, []string{"--rules", path, "echo secret"}, nil, code)
				assert.Equal(t, count, report.Rules.Compiled)
				assert.Equal(t, count, report.Rules.Enforcing)
				require.NotNil(t, report.Response)
				assert.Len(t, report.Response.Evaluations, count)
			})
		}
	}
}

func TestGuardsPlainGoldens(t *testing.T) {
	transformed := &agento11y.HookInput{Output: []agento11y.Message{{Role: agento11y.RoleAssistant, Parts: []agento11y.Part{{Kind: agento11y.PartKindToolCall, ToolCall: &agento11y.ToolCall{ID: "guards-test", Name: "Bash", InputJSON: json.RawMessage(`{"command":"echo SAFE"}`)}}}}}}
	for _, tc := range []struct {
		name            string
		response        *guardeval.Response
		errors, notices []string
		path, want      string
	}{
		{name: "allow", response: &guardeval.Response{Action: "allow"}, notices: []string{"no local rules can enforce"}, path: "rules.toml", want: "allow (local dry run)\nrules path: rules.toml\nenforceable rules: 0\nnotice: no local rules can enforce\n"},
		{name: "deny", response: &guardeval.Response{Action: "deny", RuleID: "block", Reason: "blocked", Evaluations: []guardeval.Evaluation{{RuleID: "block", Effect: "deny", EvaluatorKind: "regex", Reason: "blocked", Explanation: "matched"}}}, path: "rules.toml", want: "deny (local dry run)\nrules path: rules.toml\nenforceable rules: 0\nrule id: block\nreason: blocked\nevaluation: rule_id=block effect=deny evaluator_kind=regex passed=false\nevaluation reason: blocked\nevaluation explanation: matched\n"},
		{name: "warn", response: &guardeval.Response{Action: "allow", Evaluations: []guardeval.Evaluation{{RuleID: "passed", Passed: true}, {RuleID: "observe", Effect: "warn", EvaluatorKind: "tool_filter", Reason: "warning"}}}, path: "rules.toml", want: "allow (local dry run)\nrules path: rules.toml\nenforceable rules: 0\nevaluation: rule_id=observe effect=warn evaluator_kind=tool_filter passed=false\nevaluation reason: warning\n"},
		{name: "transform", response: &guardeval.Response{Action: "allow", RuleID: "redact", TransformedInput: transformed}, path: "rules.toml", want: `allow (local dry run)
rules path: rules.toml
enforceable rules: 0
rule id: redact
transformed input: {\"output\":[{\"role\":\"assistant\",\"parts\":[{\"kind\":\"tool_call\",\"tool_call\":{\"id\":\"guards-test\",\"name\":\"Bash\",\"input_json\":{\"command\":\"echo SAFE\"}},\"metadata\":{}}]}]}
`},
		{name: "error", errors: []string{"command must not be blank"}, path: "", want: "error (local dry run)\nrules path: \nenforceable rules: 0\nerror: command must not be blank\n"},
		{name: "partial deny", errors: []string{"bad regex"}, response: &guardeval.Response{Action: "deny", RuleID: "block", Reason: "blocked", Evaluations: []guardeval.Evaluation{{RuleID: "block", Effect: "deny", EvaluatorKind: "regex", Reason: "blocked", Explanation: "matched"}}}, path: "rules.toml", want: "error (local dry run)\nrules path: rules.toml\nenforceable rules: 0\nerror: bad regex\npartial response: deny\npartial response rule id: block\npartial response reason: blocked\npartial response evaluation: rule_id=block effect=deny evaluator_kind=regex passed=false\npartial response evaluation reason: blocked\npartial response evaluation explanation: matched\n"},
		{name: "terminal escapes", path: "rules\x1b[31m\n\r\t\x00\"\\", errors: []string{"bad\x1b]0;title\a"}, notices: []string{"notice\nnext"}, response: &guardeval.Response{Action: "deny", RuleID: "id\nnext", Reason: "reason\rnext", Evaluations: []guardeval.Evaluation{{RuleID: "id\x1b", Effect: "deny", EvaluatorKind: "regex", Reason: "bad\tvalue", Explanation: "explain\bvalue"}}}, want: "error (local dry run)\nrules path: rules\\x1b[31m\\n\\r\\t\\x00\\\"\\\\\nenforceable rules: 0\nerror: bad\\x1b]0;title\\a\nnotice: notice\\nnext\npartial response: deny\npartial response rule id: id\\nnext\npartial response reason: reason\\rnext\npartial response evaluation: rule_id=id\\x1b effect=deny evaluator_kind=regex passed=false\npartial response evaluation reason: bad\\tvalue\npartial response evaluation explanation: explain\\bvalue\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := guardsTestReport{Rules: guardsTestRules{Path: tc.path}, Response: tc.response, Errors: tc.errors, Notices: tc.notices}
			data, err := renderGuardsTest(report, false)
			require.NoError(t, err)
			assert.Equal(t, tc.want, string(data))
			assert.NotContains(t, string(data), "\x1b")
		})
	}
}

type guardsFailWriter struct{ short bool }

func (w guardsFailWriter) Write(p []byte) (int, error) {
	if w.short {
		return len(p) - 1, nil
	}
	return 0, errors.New("output failed\x1b\n")
}

func TestGuardsOutputFailures(t *testing.T) {
	for _, short := range []bool{false, true} {
		for _, mode := range []string{"allow", "deny", "help"} {
			t.Run(fmt.Sprintf("%s/short=%t", mode, short), func(t *testing.T) {
				isolateDotenvHome(t)
				path := filepath.Join(t.TempDir(), "guards.toml")
				guardsWriteFile(t, path, guardsDenyRules)
				args := []string{"guards", "test", "--rules", path, "echo safe"}
				if mode == "deny" {
					args[len(args)-1] = "echo secret"
				}
				if mode == "help" {
					args = []string{"guards", "help"}
				}
				var stderr bytes.Buffer
				code := withExit(t, func() { run(args, nil, guardsFailWriter{short: short}, &stderr) })
				require.NotNil(t, code)
				assert.Equal(t, 2, *code)
				want := "agento11y: write guards result: output failed\\x1b\\n\n"
				if short {
					want = "agento11y: write guards result: short write\n"
				}
				assert.Equal(t, want, stderr.String())
			})
		}
	}
}

func TestGuardsCanceledEvaluation(t *testing.T) {
	for _, contents := range []string{guardsDenyRules, guardsRedactRules} {
		t.Run(strings.Split(contents, "\n")[1], func(t *testing.T) {
			isolateDotenvHome(t)
			path := filepath.Join(t.TempDir(), "guards.toml")
			guardsWriteFile(t, path, contents)
			opts, positional, err := parseGuardsTest([]string{"--rules", path, "echo secret"})
			require.NoError(t, err)
			report := prepareGuardsTest(opts, positional, nil)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			report = evaluateGuardsTest(ctx, report, true)
			assert.Equal(t, []string{"evaluate rules: context canceled"}, report.Errors)
			assert.Nil(t, report.Response)
			assert.Equal(t, 2, report.exitCode())
			data, err := renderGuardsTest(report, true)
			require.NoError(t, err)
			assert.Nil(t, guardsDecodeReport(t, data).Response)
		})
	}
}

type guardsRoundTripper func(*http.Request) (*http.Response, error)

func (f guardsRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type guardsFileSnapshot struct {
	Mode     fs.FileMode
	ModTime  time.Time
	Contents string
}

func guardsSnapshotTree(t *testing.T, root string) map[string]guardsFileSnapshot {
	t.Helper()
	result := map[string]guardsFileSnapshot{}
	require.NoError(t, filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		name, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		snapshot := guardsFileSnapshot{Mode: info.Mode(), ModTime: info.ModTime()}
		if !entry.IsDir() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			snapshot.Contents = string(data)
		}
		result[name] = snapshot
		return nil
	}))
	return result
}

// No parallelism: entry seams, DefaultTransport, and process env are global.
func TestGuardsMainRunIsReadOnlyAndOffline(t *testing.T) {
	legacy, err := os.ReadFile("../local/testdata/guardpacks/afa354d4.toml")
	require.NoError(t, err)
	for _, tc := range []struct {
		name, contents string
		explicit       bool
		code           int
	}{
		{"allow default", "# empty\n", false, 0},
		{"deny explicit", guardsDenyRules, true, 1},
		{"transform default", guardsRedactRules, false, 0},
		{"legacy upgrade never writes", string(legacy), false, 0},
		{"compilation error explicit", guardsBrokenRules + guardsDenyRules, true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := isolateDotenvHome(t)
			var transportCalls, endpointCalls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				endpointCalls.Add(1)
				http.Error(w, "guards dry run must not contact endpoints", http.StatusServiceUnavailable)
			}))
			t.Cleanup(server.Close)
			previousTransport := http.DefaultTransport
			http.DefaultTransport = guardsRoundTripper(func(*http.Request) (*http.Response, error) {
				transportCalls.Add(1)
				return nil, errors.New("HTTP forbidden during guards dry run")
			})
			t.Cleanup(func() { http.DefaultTransport = previousTransport })
			for _, prefix := range []string{"AGENTO11Y_", "SIGIL_"} {
				t.Setenv(prefix+"ENDPOINT", server.URL)
				t.Setenv(prefix+"GUARDS_ENDPOINT", server.URL+"/hooks")
				t.Setenv(prefix+"AUTH_TENANT_ID", "synthetic-tenant")
				t.Setenv(prefix+"AUTH_TOKEN", "synthetic-not-a-credential")
				t.Setenv(prefix+"LOCAL", "true")
				t.Setenv(prefix+"LOCAL_FORWARD", "true")
				t.Setenv(prefix+"DEBUG", "true")
				t.Setenv(prefix+"GUARDS_ENABLED", "false")
			}
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", server.URL+"/otlp")
			t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", server.URL+"/traces")
			t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", server.URL+"/metrics")
			t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "Authorization=synthetic")
			var seamCalls []string
			forbiddenLauncher := func(context.Context, []string, *local.LaunchEnv, io.Reader, io.Writer, io.Writer, *log.Logger, string) error {
				seamCalls = append(seamCalls, "launcher")
				return errors.New("launcher forbidden")
			}
			stubs := map[string]agentLauncher{"guards": forbiddenLauncher}
			for name := range launchers {
				stubs[name] = forbiddenLauncher
			}
			withStubLaunchers(t, stubs)
			withStubLoginRun(t, func(context.Context, login.RunOpts) (login.Result, error) {
				seamCalls = append(seamCalls, "login")
				return login.Result{}, errors.New("login forbidden")
			})
			oldEnsure, oldSupported := historyEnsureLocal, hookLocalReceiverSupported
			t.Cleanup(func() { historyEnsureLocal, hookLocalReceiverSupported = oldEnsure, oldSupported })
			historyEnsureLocal = func(context.Context) (string, error) {
				seamCalls = append(seamCalls, "daemon")
				return "", errors.New("daemon forbidden")
			}
			hookLocalReceiverSupported = func() bool { seamCalls = append(seamCalls, "hook daemon"); return false }
			// Direct local.EnsureRunning has no injectable seam. The complete HOME
			// snapshot additionally detects state/config writes from that path.
			config := filepath.Join(home, "config", "agento11y", "config.env")
			guardsWriteFile(t, config, "AGENTO11Y_AGENT_NAME=must-not-materialize\nSIGIL_AUTH_TOKEN=file-synthetic\nAGENTO11Y_ENDPOINT="+server.URL+"\n")
			path := filepath.Join(filepath.Dir(config), "guards.toml")
			if tc.explicit {
				path = filepath.Join(home, "explicit", "guards.toml")
			}
			guardsWriteFile(t, path, tc.contents)
			require.NoError(t, os.Chmod(path, 0o640))
			stamp := time.Unix(1234567890, 0)
			require.NoError(t, os.Chtimes(path, stamp, stamp))
			sentinel := filepath.Join(home, "must-not-exist")
			command := "echo secret; touch '" + sentinel + "'"
			args := []string{"guards", "test", "--json"}
			if tc.explicit {
				args = append(args, "--rules", path)
			}
			args = append(args, command)
			beforeTree := guardsSnapshotTree(t, home)
			beforeEnv := os.Environ()
			sort.Strings(beforeEnv)
			var stdout, stderr bytes.Buffer
			code := withExit(t, func() { run(args, guardsReadError{}, &stdout, &stderr) })
			require.NotNil(t, code)
			assert.Equal(t, tc.code, *code)
			assert.Empty(t, stderr.String())
			report := guardsDecodeReport(t, stdout.Bytes())
			assert.Equal(t, command, report.Request.Command)
			assert.Empty(t, report.Request.AgentName)
			afterEnv := os.Environ()
			sort.Strings(afterEnv)
			assert.Equal(t, beforeEnv, afterEnv, "dry run must not materialize dotenv or rewrite env")
			assert.Equal(t, beforeTree, guardsSnapshotTree(t, home), "HOME tree, rules bytes, modes and mtimes must stay unchanged")
			_, err := os.Stat(sentinel)
			assert.True(t, os.IsNotExist(err), "submitted command was executed")
			assert.Empty(t, seamCalls)
			assert.Zero(t, transportCalls.Load())
			assert.Zero(t, endpointCalls.Load())
		})
	}
}

func TestGuardsEngineParityAndJSON(t *testing.T) {
	legacy, err := os.ReadFile("../local/testdata/guardpacks/afa354d4.toml")
	require.NoError(t, err)
	for _, tc := range []struct {
		name, contents, command, action, ruleID string
		code, compiled                          int
		failedEffects                           []string
		transformed                             bool
	}{
		{"allow", "", "echo secret", "allow", "", 0, 0, nil, false},
		{"warn then deny", guardsDenyRules + guardsWarnRules, "echo secret", "deny", "deny", 1, 2, []string{"warn", "deny"}, false},
		{"warn only", guardsWarnRules, "echo secret", "allow", "", 0, 1, []string{"warn"}, false},
		{"redaction then exception", guardsDenyRules + guardsExceptionRules + guardsRedactRules, "echo secret", "allow", "exception", 0, 3, []string{"allow"}, true},
		{"redaction before deny checks original", guardsDenyRules + guardsRedactRules, "echo secret", "deny", "deny", 1, 2, []string{"deny"}, false},
		{"successful transform", guardsRedactRules, "echo secret", "allow", "redact", 0, 1, nil, true},
		{"dropped redaction", "[[rules]]\nrule_id='drop'\ntransform.patterns=[{regex='secret', replacement='broken\"'}]\n", "echo secret", "allow", "", 0, 1, []string{"deny"}, false},
		{"legacy fixture upgraded", string(legacy), "git reset HEAD --hard", "deny", "pack.git", 1, 6, []string{"deny"}, false},
		{"disabled legacy stays disabled", strings.ReplaceAll(string(legacy), "[[rules]]", "[[rules]]\nenabled = false"), "git reset HEAD --hard", "allow", "", 0, 0, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateDotenvHome(t)
			path := filepath.Join(t.TempDir(), "guards.toml")
			guardsWriteFile(t, path, tc.contents)
			args := []string{"--rules", path, tc.command}
			report, data := guardsRunJSON(t, args, nil, tc.code)
			for range 3 {
				_, repeated := guardsRunJSON(t, args, nil, tc.code)
				assert.Equal(t, string(data), string(repeated), "JSON must be byte stable")
			}
			assert.Equal(t, tc.compiled, report.Rules.Compiled)
			assert.Equal(t, tc.compiled, report.Rules.Enforcing)
			assert.Empty(t, report.Errors)
			require.NotNil(t, report.Response)
			assert.Equal(t, tc.action, string(report.Response.Action))
			assert.Equal(t, tc.ruleID, report.Response.RuleID)
			var effects []string
			for _, evaluation := range report.Response.Evaluations {
				if !evaluation.Passed {
					effects = append(effects, evaluation.Effect)
				}
			}
			assert.Equal(t, tc.failedEffects, effects)
			// This is the same in-memory legacy-upgrading entrypoint the daemon uses.
			engine := local.NewGuardsEngineFromContents(path, []byte(tc.contents), nil)
			req, err := (guardsTestRequest{Command: tc.command, ToolName: "Bash"}).hookRequest()
			require.NoError(t, err)
			expected, _, err := engine.EvaluateWithTransform(context.Background(), req)
			require.NoError(t, err)
			assert.Equal(t, expected, *report.Response)
			if tc.transformed {
				require.NotNil(t, report.Response.TransformedInput)
				assert.Equal(t, `{"command":"echo SAFE"}`, string(report.Response.TransformedInput.Output[0].Parts[0].ToolCall.InputJSON))
				var wire map[string]any
				require.NoError(t, json.Unmarshal(data, &wire))
				transformed := wire["response"].(map[string]any)["transformed_input"]
				sdkJSON, err := json.Marshal(expected.TransformedInput)
				require.NoError(t, err)
				actual, err := json.Marshal(transformed)
				require.NoError(t, err)
				assert.JSONEq(t, string(sdkJSON), string(actual))
				// The SDK embeds arguments as JSON, never a JSON string or base64 bytes.
				assert.Contains(t, string(data), `"input_json":{"command":"echo SAFE"}`)
			} else {
				assert.Nil(t, report.Response.TransformedInput)
			}
			if tc.name == "dropped redaction" {
				require.Len(t, report.Response.Evaluations, 1)
				assert.Equal(t, "transform", report.Response.Evaluations[0].EvaluatorKind)
				assert.Contains(t, report.Response.Evaluations[0].Reason, "original is unchanged")
			}
		})
	}
}
