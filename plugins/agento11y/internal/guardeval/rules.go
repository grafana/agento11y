// Package guardeval is the deterministic guard engine the --local daemon runs
// on every agent tool call: the rule schema, the on-disk ruleset, and the
// in-process evaluation of tool filters, transforms, and inline evaluators. It
// depends on no HTTP layer, so the daemon, the doctor report, and a test can
// all reach the same verdict.
package guardeval

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Rule is the guard-rule write body. JSON tags match the hook-rules API so an
// exported rule loads unchanged. Fields the local engine does not read
// (selector, short_circuit, evaluator_ids, server-managed metadata) are absent
// here and round-trip through the raw JSON the guards API stores.
type Rule struct {
	RuleID       string            `json:"rule_id" toml:"rule_id"`
	Enabled      *bool             `json:"enabled,omitempty" toml:"enabled,omitempty"`               // default true
	Phase        string            `json:"phase,omitempty" toml:"phase,omitempty"`                   // default "postflight"
	Priority     int               `json:"priority,omitempty" toml:"priority,omitempty"`             // ascending; lower runs first
	Match        map[string]any    `json:"match,omitempty" toml:"match,omitempty"`                   // agent_name, model.name, tags.*, ... globs
	ActionOnFail string            `json:"action_on_fail,omitempty" toml:"action_on_fail,omitempty"` // deny, warn, or allow (case-insensitive); empty or unknown denies
	ToolFilter   *ToolFilterConfig `json:"tool_filter,omitempty" toml:"tool_filter,omitempty"`
	Transform    *TransformConfig  `json:"transform,omitempty" toml:"transform,omitempty"`
	Evaluators   []EvaluatorSpec   `json:"evaluators,omitempty" toml:"evaluators,omitempty"` // inline deterministic evaluators run locally (see below)
	// extra holds JSON keys this struct does not model (evaluator_ids,
	// short_circuit, selector, …). DecodeRules keeps them so a Settings save
	// can write them back instead of stripping them from custom rules.
	extra map[string]any `json:"-"`
}

// EvaluatorSpec is an inline evaluator definition on a local rule. The cloud
// keys evaluator config by evaluator_id in a separate resource; a standalone
// local file inlines it here instead so a rule is self-contained. Only regex
// runs locally; heuristic, llm_judge, prompt_guard and json_schema are accepted
// but skipped. evaluator_ids (cloud refs) stay inert locally: only this field
// is enforced.
type EvaluatorSpec struct {
	Kind   string         `json:"kind" toml:"kind"`
	Config map[string]any `json:"config,omitempty" toml:"config,omitempty"`
}

// ToolFilterConfig is the tool-filter block config.
type ToolFilterConfig struct {
	BlockedNames []string `json:"blocked_names" toml:"blocked_names"`
}

// TransformConfig is the redaction config.
type TransformConfig struct {
	Patterns []TransformPattern `json:"patterns" toml:"patterns"`
}

// TransformPattern is one redaction pattern.
type TransformPattern struct {
	ID          string `json:"id,omitempty" toml:"id,omitempty"`
	Regex       string `json:"regex" toml:"regex"`
	Replacement string `json:"replacement,omitempty" toml:"replacement,omitempty"`
}

// CompiledRule is a compiled, normalized rule ready for evaluation.
type CompiledRule struct {
	// id is the rule_id as written.
	id         string
	phase      string
	priority   int
	match      []compiledMatch
	filter     *toolFilter
	transform  *Transform
	evaluators []*compiledEvaluator
	effect     Effect
}

// Effect is what a rule does to the call when one of its checks trips.
type Effect string

const (
	// EffectDeny blocks the tool call and ends the evaluation.
	EffectDeny Effect = "deny"
	// EffectWarn records the failed check and lets the call continue, so a rule
	// can be observed before it is trusted to block.
	EffectWarn Effect = "warn"
	// EffectAllow ends the evaluation in favour of the call: the rules ordered
	// after it never run. It is how an exception ("this workspace may run
	// Bash(rm ./build/*)") is written against a broader rule that would otherwise
	// deny.
	EffectAllow Effect = "allow"
)

// parseEffect maps a rule's action_on_fail onto an Effect, case-insensitively.
// Empty is EffectDeny, the schema default. Anything else unknown is also
// EffectDeny: a misspelling or a word from a newer schema this build does not
// know ("block") must not degrade to warn or allow, which would let through a
// call the rule editor renders as "Deny". The error names the written word so
// the loader can report it instead of enforcing a silent default.
func parseEffect(actionOnFail string) (Effect, error) {
	trimmed := strings.TrimSpace(actionOnFail)
	switch Effect(strings.ToLower(trimmed)) {
	case EffectWarn:
		return EffectWarn, nil
	case EffectAllow:
		return EffectAllow, nil
	case EffectDeny, "":
		return EffectDeny, nil
	default:
		return EffectDeny, fmt.Errorf("action_on_fail %q is not deny, warn, or allow", trimmed)
	}
}

// label names the rule in a message a human reads. Every rule in the file was
// written by the person reading the message, so the id is the whole
// attribution they need to go and change it.
func (r CompiledRule) label() string {
	return fmt.Sprintf("rule %q", r.id)
}

// evaluation starts one HookEvaluation for this rule, pre-stamped with what a
// consumer needs to attribute the outcome without holding the ruleset: the
// rule id and the effect, which is the only thing separating a recorded
// warning from a block (both report a failed check, and only one stopped the
// call).
func (r CompiledRule) evaluation(kind string, passed bool) Evaluation {
	return Evaluation{
		RuleID:        r.id,
		Effect:        string(r.effect),
		EvaluatorKind: kind,
		Passed:        passed,
	}
}

// DecodeRules decodes each raw rule object into a Rule. A rule that does not
// decode is reported with its index and dropped, so one malformed entry does
// not discard the rest of the file.
func DecodeRules(raw []json.RawMessage) ([]Rule, []error) {
	var (
		out  []Rule
		errs []error
	)
	for i, item := range raw {
		var rule Rule
		if err := json.Unmarshal(item, &rule); err != nil {
			errs = append(errs, fmt.Errorf("rule[%d]: %w", i, err))
			continue
		}
		out = append(out, rule)
	}
	return out, errs
}

var knownRuleJSONKeys = map[string]struct{}{
	"rule_id":        {},
	"enabled":        {},
	"phase":          {},
	"priority":       {},
	"match":          {},
	"action_on_fail": {},
	"tool_filter":    {},
	"transform":      {},
	"evaluators":     {},
}

// UnmarshalJSON fills the modelled fields and stashes every other key in extra
// so a later EncodeRules can round-trip Cloud-only attributes.
func (r *Rule) UnmarshalJSON(data []byte) error {
	type alias Rule
	var parsed alias
	if err := json.Unmarshal(data, &parsed); err != nil {
		return err
	}
	*r = Rule(parsed)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for key := range knownRuleJSONKeys {
		delete(raw, key)
	}
	if len(raw) == 0 {
		return nil
	}
	r.extra = make(map[string]any, len(raw))
	for key, val := range raw {
		var decoded any
		if err := json.Unmarshal(val, &decoded); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		r.extra[key] = decoded
	}
	return nil
}

// compileGuardRules drops disabled rules, applies the write-body defaults
// (enabled true, phase postflight, action_on_fail deny), pre-compiles the match
// globs, tool-filter globs and transform regexes, and sorts by priority then
// rule_id. A rule that fails to compile is skipped and reported; the remaining
// rules still enforce.
func compileGuardRules(raw []Rule) ([]CompiledRule, []error) {
	out := make([]CompiledRule, 0, len(raw))
	var errs []error
	for _, r := range raw {
		if r.Enabled != nil && !*r.Enabled {
			continue
		}
		phase := strings.ToLower(strings.TrimSpace(r.Phase))
		if phase == "" {
			phase = hookPhasePostflight
		}
		// Phases are matched by exact string, so a typo would compile, count as
		// enforcing, and never fire. Report it instead.
		if phase != hookPhasePostflight && phase != hookPhasePreflight {
			errs = append(errs, fmt.Errorf("rule %q: phase %q is not %s or %s", r.RuleID, r.Phase, hookPhasePreflight, hookPhasePostflight))
			continue
		}
		match, err := compileMatch(r.Match)
		if err != nil {
			errs = append(errs, fmt.Errorf("rule %q: %w", r.RuleID, err))
			continue
		}

		filter, err := compileToolFilter(r.ToolFilter)
		if err != nil {
			errs = append(errs, fmt.Errorf("rule %q: %w", r.RuleID, err))
			continue
		}

		transform, err := compileTransform(r.Transform)
		if err != nil {
			errs = append(errs, fmt.Errorf("rule %q: %w", r.RuleID, err))
			continue
		}

		var (
			evaluators []*compiledEvaluator
			evalErr    error
		)
		for i, spec := range r.Evaluators {
			ce, err := compileEvaluator(spec)
			if err != nil {
				evalErr = fmt.Errorf("rule %q evaluator[%d]: %w", r.RuleID, i, err)
				break
			}
			if ce != nil {
				evaluators = append(evaluators, ce)
			}
		}
		if evalErr != nil {
			errs = append(errs, evalErr)
			continue
		}

		effect, err := parseEffect(r.ActionOnFail)
		if err != nil {
			errs = append(errs, fmt.Errorf("rule %q: %w", r.RuleID, err))
		}

		out = append(out, CompiledRule{
			id:         r.RuleID,
			phase:      phase,
			priority:   r.Priority,
			match:      match,
			filter:     filter,
			transform:  transform,
			evaluators: evaluators,
			effect:     effect,
		})
	}

	sortCompiledRules(out)
	return out, errs
}

// sortCompiledRules puts a ruleset in evaluation order: ascending priority,
// then rule id. Priority decides everything, so an allow exception can be
// ordered ahead of the deny it excepts. The sort is stable, so two rules that
// tie on both keys keep the order they were written in.
func sortCompiledRules(rules []CompiledRule) {
	sort.SliceStable(rules, func(i, j int) bool {
		if rules[i].priority != rules[j].priority {
			return rules[i].priority < rules[j].priority
		}
		return rules[i].id < rules[j].id
	})
}

// filterRuleIDs requires a present and unique rule_id. Consumers treat
// rule_id as identity (the conversation view's deep link, exported guard
// metadata, the doctor row), so a blank or duplicated id is dropped rather
// than compiled twice under the same name.
//
// Every problem is reported so a file with two faults names both. The first
// occurrence of a duplicated id is kept; later copies are skipped. A ruleset
// already on disk should not stop guarding over a naming fault on a sibling.
func filterRuleIDs(rules []Rule) ([]Rule, []error) {
	seen := map[string]int{}
	out := make([]Rule, 0, len(rules))
	var errs []error
	for i, r := range rules {
		id := strings.TrimSpace(r.RuleID)
		if id == "" {
			errs = append(errs, fmt.Errorf("rule[%d]: rule_id is required", i))
			continue
		}
		if prev, ok := seen[id]; ok {
			errs = append(errs, fmt.Errorf("rule[%d]: rule_id %q duplicates rule[%d]", i, id, prev))
			continue
		}
		seen[id] = i
		r.RuleID = id
		out = append(out, r)
	}
	return out, errs
}

// compileTransform compiles a transform config into ready-to-run patterns.
// Returns (nil, nil) when there is nothing to transform. An empty replacement
// defaults to "[REDACTED:{id}]" (or "[REDACTED]" when no id).
func compileTransform(cfg *TransformConfig) (*Transform, error) {
	if cfg == nil || len(cfg.Patterns) == 0 {
		return nil, nil
	}
	patterns := make([]compiledPattern, 0, len(cfg.Patterns))
	for i, p := range cfg.Patterns {
		pat := strings.TrimSpace(p.Regex)
		if pat == "" {
			return nil, fmt.Errorf("transform.patterns[%d].regex is required", i)
		}
		re, err := regexp.Compile(pat)
		if err != nil {
			return nil, fmt.Errorf("transform.patterns[%d].regex: %w", i, err)
		}
		// Do not trim Replacement: padded and whitespace-only values are meaningful.
		repl := p.Replacement
		if repl == "" {
			if id := strings.TrimSpace(p.ID); id != "" {
				repl = fmt.Sprintf("[REDACTED:%s]", id)
			} else {
				repl = "[REDACTED]"
			}
		}
		patterns = append(patterns, compiledPattern{re: re, repl: repl})
	}
	return &Transform{patterns: patterns}, nil
}
