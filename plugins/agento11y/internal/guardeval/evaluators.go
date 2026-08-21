package guardeval

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/grafana/agento11y/go/agento11y"
)

// Deterministic guard evaluation for --local mode. regex is the one kind that
// runs here, entirely in-process, so the daemon enforces it offline. The kinds
// it does not run (llm_judge, prompt_guard, heuristic, json_schema) are
// accepted but skipped, so an imported Cloud ruleset stays loadable and those
// rules never fire. Any other kind is an error: a typo like "regexp" would
// otherwise compile to nothing and leave the rule inert while the UI reports it
// as a cloud-only rule.
//
// Pass semantics: a rule's action_on_fail fires when an evaluator does NOT pass
// (passed == false). For a regex evaluator with reject:true (the common
// "block on match" case) a match means passed == false, so the rule denies.

const (
	evaluatorKindRegex = "regex"
	// The kinds parsed and never run locally. The first three need a model. The
	// fourth, json_schema, needs a subject that is a JSON document, and no local
	// target produces one: every target flattens to labelled text, and the
	// tool-call form ("[tool_call] name {…}") never parses. Running it would
	// deny every call it matched, on a rule whose author expected it to check a
	// structured answer. Cloud evaluates it against what it holds.
	evaluatorKindHeuristic   = "heuristic"
	evaluatorKindLLMJudge    = "llm_judge"
	evaluatorKindPromptGuard = "prompt_guard"
	evaluatorKindJSONSchema  = "json_schema"

	evalTargetResponse     = "response"
	evalTargetInput        = "input"
	evalTargetSystemPrompt = "system_prompt"
	// evalTargetShellCommand evaluates the decoded command line of a shell tool
	// call instead of the flattened, JSON-escaped tool call text. See shell.go.
	evalTargetShellCommand = "shell_command"
)

// compiledEvaluator is a ready-to-run inline evaluator.
type compiledEvaluator struct {
	kind   string
	target string
	// shell is the shell_command projection config (tool names and command
	// keys). Only the shell_command target reads it.
	shell   shellConfig
	regexes []*regexp.Regexp
	reject  bool
}

// compileEvaluator compiles one inline evaluator spec. regex yields a runnable
// evaluator; the Cloud kinds return (nil, nil) so the loader keeps the rule but
// never runs them. A malformed regex config or an unrecognised kind returns an
// error, which the PUT validation surfaces as a 400 and the loader reports as a
// skipped rule.
func compileEvaluator(spec EvaluatorSpec) (*compiledEvaluator, error) {
	kind := strings.ToLower(strings.TrimSpace(spec.Kind))
	switch kind {
	case evaluatorKindRegex:
		target, shell, err := parseTargetAndShell(spec.Config)
		if err != nil {
			return nil, err
		}
		patterns := extractRegexPatterns(spec.Config)
		if len(patterns) == 0 {
			return nil, fmt.Errorf("regex evaluator requires pattern or patterns")
		}
		res := make([]*regexp.Regexp, 0, len(patterns))
		for _, p := range patterns {
			re, err := regexp.Compile(p)
			if err != nil {
				return nil, fmt.Errorf("regex evaluator pattern %q: %w", p, err)
			}
			res = append(res, re)
		}
		return &compiledEvaluator{
			kind:    kind,
			target:  target,
			shell:   shell,
			regexes: res,
			reject:  cfgBool(spec.Config, "reject", false),
		}, nil
	case evaluatorKindHeuristic, evaluatorKindLLMJudge, evaluatorKindPromptGuard, evaluatorKindJSONSchema:
		// Kept so a Cloud ruleset round-trips, never run.
		return nil, nil
	case "":
		return nil, fmt.Errorf("evaluator kind is required")
	default:
		return nil, fmt.Errorf("evaluator kind %q is not supported locally (runs locally: regex; accepted but never run: heuristic, llm_judge, prompt_guard, json_schema)", kind)
	}
}

// runEvaluator runs one evaluator against the working input. ran is false when
// the evaluator has nothing to judge, which the caller records as no evaluation
// at all. passed and explanation are the outcome and the text for the deny
// reason.
//
// Every subject is matched on its own. The shell_command target contributes one
// subject per shell tool call, so a pattern cannot match text formed by joining
// two commands; every other target contributes exactly one. Several subjects
// still give one verdict: the evaluator answers on whether any subject matched.
//
// Only compileEvaluator's regex branch builds a compiledEvaluator, so this runs
// the patterns without asking which kind it holds.
func runEvaluator(ce *compiledEvaluator, in agento11y.HookInput) (ran, passed bool, explanation string) {
	subjects := subjectsFor(ce.target, in, ce.shell)
	if len(subjects) == 0 {
		// Only shell_command projects nothing. A rule written about shell commands
		// has no opinion on a tool that runs none, so it must not fail here: a
		// required-pattern rule would deny every non-shell call.
		return false, true, ""
	}

	matched := false
	for _, subject := range subjects {
		if matchesAnyRegex(ce.regexes, subject) {
			matched = true
			break
		}
	}
	passed = matched
	if ce.reject {
		passed = !matched
	}
	if !passed {
		// With reject the failure is a match; without it the failure is the absence
		// of a required match. The text lands in the deny reason and in the
		// recorded telemetry, so it must say which happened.
		if ce.reject {
			explanation = "matched a blocked pattern"
		} else {
			explanation = "did not match a required pattern"
		}
	}
	return true, passed, explanation
}

func matchesAnyRegex(regexes []*regexp.Regexp, subject string) bool {
	for _, re := range regexes {
		if re.MatchString(subject) {
			return true
		}
	}
	return false
}

// flatten renders messages to a single string for matching. A tool call renders
// as "[tool_call] name input_json" so a regex can match the tool name and
// arguments; text parts are trimmed; thinking and tool results are skipped. The
// exact string here is the contract a regex is written against, so it must stay
// stable.
func flatten(messages []agento11y.Message) string {
	if len(messages) == 0 {
		return ""
	}
	parts := make([]string, 0, len(messages))
	for _, m := range messages {
		for i := range m.Parts {
			if f := flattenPart(m.Parts[i]); f != "" {
				parts = append(parts, f)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func flattenPart(p agento11y.Part) string {
	if text := strings.TrimSpace(p.Text); text != "" {
		return text
	}
	if p.ToolCall != nil {
		name := strings.TrimSpace(p.ToolCall.Name)
		input := strings.TrimSpace(string(p.ToolCall.InputJSON))
		switch {
		case name != "" && input != "":
			return "[tool_call] " + name + " " + input
		case name != "":
			return "[tool_call] " + name
		case input != "":
			return "[tool_call] " + input
		default:
			return "[tool_call]"
		}
	}
	return ""
}

// parseEvalTarget validates the target; empty defaults to "response". The
// comparison is case-insensitive, like kind, phase, and action_on_fail: a
// hand-written "Shell_Command" would otherwise fail to compile and take the
// whole rule out of enforcement.
func parseEvalTarget(raw string) (string, error) {
	trimmed := strings.ToLower(strings.TrimSpace(raw))
	if trimmed == "" {
		return evalTargetResponse, nil
	}
	switch trimmed {
	case evalTargetResponse, evalTargetInput, evalTargetSystemPrompt, evalTargetShellCommand:
		return trimmed, nil
	default:
		// The message quotes what was written, not the lowercased form, so it
		// names the text to go and change.
		return "", fmt.Errorf("target %q is invalid; must be one of: response, input, system_prompt, shell_command", strings.TrimSpace(raw))
	}
}

func parseTargetAndShell(config map[string]any) (string, shellConfig, error) {
	target, err := parseEvalTarget(cfgString(config, "target", ""))
	if err != nil {
		return "", shellConfig{}, err
	}
	return target, parseShellConfig(), nil
}

// subjectsFor returns the subjects an evaluator judges for the given target. It
// computes only the target it returns: the shell_command projection costs a
// JSON decode per tool call, and flattening a long message history is not free
// either. shell_command yields one subject per shell tool call, and none when
// the call runs no command; every other target yields exactly one.
func subjectsFor(target string, in agento11y.HookInput, shell shellConfig) []string {
	switch target {
	case evalTargetShellCommand:
		return shellCommands(in, shell)
	case evalTargetInput:
		return []string{flatten(in.Messages)}
	case evalTargetSystemPrompt:
		return []string{in.SystemPrompt}
	default:
		return []string{flatten(in.Output)}
	}
}

func cfgString(config map[string]any, key, def string) string {
	if config == nil {
		return def
	}
	if v, ok := config[key].(string); ok {
		return v
	}
	return def
}

func cfgBool(config map[string]any, key string, def bool) bool {
	if config == nil {
		return def
	}
	if v, ok := config[key].(bool); ok {
		return v
	}
	return def
}

// extractRegexPatterns reads a single "pattern" string or a "patterns" array of
// strings from the evaluator config.
func extractRegexPatterns(config map[string]any) []string {
	if config == nil {
		return nil
	}
	if pattern, ok := config["pattern"].(string); ok && strings.TrimSpace(pattern) != "" {
		return []string{strings.TrimSpace(pattern)}
	}
	switch typed := config["patterns"].(type) {
	case []any:
		out := make([]string, 0, len(typed))
		for _, v := range typed {
			if s, ok := v.(string); ok {
				if trimmed := strings.TrimSpace(s); trimmed != "" {
					out = append(out, trimmed)
				}
			}
		}
		return out
	}
	return nil
}
