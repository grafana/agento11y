package entry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/dotenv"
	"github.com/grafana/agento11y/plugins/agento11y/internal/guardeval"
	"github.com/grafana/agento11y/plugins/agento11y/internal/local"
)

const guardsUsage = "usage: agento11y guards test [--json] [--rules path] [--tool name] [--agent name] [--stdin] [--] <command>"

const guardsHelp = `Test a shell command against saved local guard rules, without executing it.

Usage:
  agento11y guards test [--json] [--rules path] [--tool name] [--agent name] [--stdin] [--] <command>

Examples:
  agento11y guards test 'rm -rf ~/.ssh'
  agento11y guards test --json --rules ./guards.toml 'git reset --hard'
  printf '%s\n' 'echo first' 'echo second' | agento11y guards test --stdin --tool shell --agent pi

Flags must precede one quoted command argument. Use -- before a command starting
with a dash. --stdin reads one command through EOF, preserving embedded newlines.
--rules selects a file instead of guards.toml beside the resolved config.env.
A missing default file allows with a notice; a missing explicit file is an error.
--tool defaults to Bash and changes only the name, not the {"command": ...} arguments.
--agent defaults to empty. Conditional rules see only this supplied agent name.
The synthetic postflight request contains one assistant tool call (ID guards-test).
It has no model, tags, agent version, preflight messages, or system prompt.
It does not replay arbitrary host payloads.

This local-only dry run evaluates saved rules even when host hooks are disabled.
It neither executes input nor checks Cloud rules, contacts endpoints, starts a
daemon, or writes configuration. It does not predict host enforcement or delivery.
Disabled individual rules remain disabled; absent packs are not enabled.

Exit codes: 0 = completed allow (including warnings or transforms),
            1 = completed deny, 2 = input, file, compilation, evaluation, or output error.
Compilation errors take precedence, but valid rules still return a partial response.

--json emits one deterministic JSON document on stdout: schema_version: 1,
scope: "local", rules: {path, exists, compiled, enforcing}, request: {command,
tool_name, agent_name}, response, errors, and notices. Errors and notices are arrays.
Response is null if no evaluation completed; otherwise it is the engine response
with action, optional rule_id/reason/transformed_input, and an evaluations array.
Transformed input uses SDK JSON, including raw JSON tool arguments.
Failed evaluations include warnings and dropped redactions, not just denials.
A response rule ID does not identify every transform that ran.
Diagnostic JSON includes the submitted command and rewritten content.
Do not use real secrets in shared test output.
`

type guardsTestOptions struct {
	jsonOutput bool
	rulesPath  string
	explicit   bool
	stdin      bool
	request    guardsTestRequest
}

type guardsTestRequest struct {
	Command   string `json:"command"`
	ToolName  string `json:"tool_name"`
	AgentName string `json:"agent_name"`
}

type guardsTestRules struct {
	Path      string `json:"path"`
	Exists    bool   `json:"exists"`
	Compiled  int    `json:"compiled"`
	Enforcing int    `json:"enforcing"`
}

type guardsTestReport struct {
	SchemaVersion int                 `json:"schema_version"`
	Scope         string              `json:"scope"`
	Rules         guardsTestRules     `json:"rules"`
	Request       guardsTestRequest   `json:"request"`
	Response      *guardeval.Response `json:"response"`
	Errors        []string            `json:"errors"`
	Notices       []string            `json:"notices"`
}

func parseGuardsTest(args []string) (guardsTestOptions, []string, error) {
	var opts guardsTestOptions
	fs := flag.NewFlagSet("guards test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&opts.jsonOutput, "json", false, "emit JSON")
	fs.StringVar(&opts.rulesPath, "rules", "", "rules file")
	fs.StringVar(&opts.request.ToolName, "tool", "Bash", "tool name")
	fs.StringVar(&opts.request.AgentName, "agent", "", "agent name")
	fs.BoolVar(&opts.stdin, "stdin", false, "read command through EOF")
	err := fs.Parse(args)
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "rules" {
			opts.explicit = true
		}
	})
	return opts, fs.Args(), err
}

func runGuardsCommand(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	// Go otherwise terminates on a broken stdout pipe before we can return exit 2.
	pipeSignals := make(chan os.Signal, 1)
	signal.Notify(pipeSignals, syscall.SIGPIPE)
	defer signal.Stop(pipeSignals)

	if len(args) == 1 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h") {
		return writeGuardsOutput(stdout, stderr, []byte(guardsHelp), 0)
	}
	if len(args) == 0 || args[0] != "test" {
		_, _ = fmt.Fprintln(stderr, guardsUsage)
		return 2
	}
	opts, positional, err := parseGuardsTest(args[1:])
	if errors.Is(err, flag.ErrHelp) {
		return writeGuardsOutput(stdout, stderr, []byte(guardsHelp), 0)
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "agento11y: %s\n%s\n", guardsPlainValue(err.Error()), guardsUsage)
		return 2
	}
	report := prepareGuardsTest(opts, positional, stdin)
	if len(report.Errors) == 0 {
		report = evaluateGuardsTest(context.Background(), report, opts.explicit)
	}
	data, err := renderGuardsTest(report, opts.jsonOutput)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "agento11y: render guards result: %s\n", guardsPlainValue(err.Error()))
		return 2
	}
	return writeGuardsOutput(stdout, stderr, data, report.exitCode())
}

func prepareGuardsTest(opts guardsTestOptions, positional []string, stdin io.Reader) guardsTestReport {
	report := guardsTestReport{
		SchemaVersion: 1, Scope: "local", Request: opts.request,
		Rules: guardsTestRules{Path: opts.rulesPath}, Errors: []string{}, Notices: []string{},
	}
	switch {
	case opts.stdin && len(positional) != 0:
		report.Errors = append(report.Errors, "use either --stdin or one command argument, not both")
	case !opts.stdin && len(positional) != 1:
		report.Errors = append(report.Errors, "exactly one command argument or --stdin is required; quote the command")
	case opts.stdin:
		data, err := io.ReadAll(stdin)
		if err != nil {
			report.Errors = append(report.Errors, "read stdin: "+err.Error())
		} else {
			report.Request.Command = string(data)
		}
	default:
		report.Request.Command = positional[0]
	}
	if len(report.Errors) == 0 && strings.TrimSpace(report.Request.Command) == "" {
		report.Errors = append(report.Errors, "command must not be blank")
	}
	if strings.TrimSpace(report.Request.ToolName) == "" {
		report.Errors = append(report.Errors, "tool name must not be blank")
	}
	if opts.explicit && strings.TrimSpace(opts.rulesPath) == "" {
		report.Errors = append(report.Errors, "rules path must not be blank")
	}
	if len(report.Errors) == 0 && !opts.explicit {
		report.Rules.Path = filepath.Join(filepath.Dir(dotenv.FilePath()), guardeval.ConfigFile)
	}
	return report
}

func (r guardsTestRequest) hookRequest() (agento11y.HookEvaluateRequest, error) {
	args, err := json.Marshal(struct {
		Command string `json:"command"`
	}{r.Command})
	if err != nil {
		return agento11y.HookEvaluateRequest{}, err
	}
	return agento11y.HookEvaluateRequest{
		Phase:   agento11y.HookPhasePostflight,
		Context: agento11y.HookContext{AgentName: r.AgentName},
		Input: agento11y.HookInput{Output: []agento11y.Message{{
			Role: agento11y.RoleAssistant,
			Parts: []agento11y.Part{agento11y.ToolCallPart(agento11y.ToolCall{
				ID: "guards-test", Name: r.ToolName, InputJSON: args,
			})},
		}}},
	}, nil
}

func evaluateGuardsTest(ctx context.Context, report guardsTestReport, explicit bool) guardsTestReport {
	data, err := os.ReadFile(report.Rules.Path)
	if err != nil && (explicit || !os.IsNotExist(err)) {
		report.Errors = append(report.Errors, "read rules: "+err.Error())
		return report
	}
	report.Rules.Exists = err == nil
	engine := local.NewGuardsEngineFromContents(report.Rules.Path, data, nil)
	status := engine.Status()
	report.Rules.Compiled = status.Rules
	report.Rules.Enforcing = status.Enforcing
	report.Errors = append(report.Errors, status.Errors...)
	if status.Enforcing == 0 {
		report.Notices = append(report.Notices, "no local rules can enforce")
	}
	if len(status.Errors) > 0 && status.Rules == 0 {
		return report
	}
	req, err := report.Request.hookRequest()
	if err != nil {
		report.Errors = append(report.Errors, "build request: "+err.Error())
		return report
	}
	resp, _, err := engine.EvaluateWithTransform(ctx, req)
	if err != nil {
		report.Errors = append(report.Errors, "evaluate rules: "+err.Error())
		return report
	}
	if resp.Evaluations == nil {
		resp.Evaluations = []guardeval.Evaluation{}
	}
	report.Response = &resp
	return report
}

func (r guardsTestReport) exitCode() int {
	if len(r.Errors) > 0 || r.Response == nil {
		return 2
	}
	if r.Response.Action == agento11y.HookActionDeny {
		return 1
	}
	return 0
}

func guardsPlainValue(value string) string {
	quoted := strconv.Quote(value)
	return quoted[1 : len(quoted)-1]
}

func renderGuardsTest(report guardsTestReport, asJSON bool) ([]byte, error) {
	if asJSON {
		data, err := json.Marshal(report)
		return append(data, '\n'), err
	}
	var out bytes.Buffer
	action := "error"
	if report.exitCode() != 2 {
		action = string(report.Response.Action)
	}
	fmt.Fprintf(&out, "%s (local dry run)\n", action)
	line := func(label, value string) {
		fmt.Fprintf(&out, "%s: %s\n", label, guardsPlainValue(value))
	}
	line("rules path", report.Rules.Path)
	line("enforceable rules", strconv.Itoa(report.Rules.Enforcing))
	for _, diagnostic := range report.Errors {
		line("error", diagnostic)
	}
	for _, notice := range report.Notices {
		line("notice", notice)
	}
	if resp := report.Response; resp != nil {
		prefix := ""
		if report.exitCode() == 2 {
			line("partial response", string(resp.Action))
			prefix = "partial response "
		}
		if resp.RuleID != "" {
			line(prefix+"rule id", resp.RuleID)
		}
		if resp.Reason != "" {
			line(prefix+"reason", resp.Reason)
		}
		for _, evaluation := range resp.Evaluations {
			if evaluation.Passed {
				continue
			}
			line(prefix+"evaluation", fmt.Sprintf("rule_id=%s effect=%s evaluator_kind=%s passed=false", evaluation.RuleID, evaluation.Effect, evaluation.EvaluatorKind))
			if evaluation.Reason != "" {
				line(prefix+"evaluation reason", evaluation.Reason)
			}
			if evaluation.Explanation != "" {
				line(prefix+"evaluation explanation", evaluation.Explanation)
			}
		}
		if resp.TransformedInput != nil {
			data, err := json.Marshal(resp.TransformedInput)
			if err != nil {
				return nil, err
			}
			line(prefix+"transformed input", string(data))
		}
	}
	return out.Bytes(), nil
}

func writeGuardsOutput(stdout, stderr io.Writer, data []byte, code int) int {
	n, err := stdout.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "agento11y: write guards result: %s\n", guardsPlainValue(err.Error()))
		return 2
	}
	return code
}
