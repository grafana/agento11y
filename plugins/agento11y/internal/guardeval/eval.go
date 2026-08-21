package guardeval

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strings"

	"github.com/grafana/agento11y/go/agento11y"
)

// Deterministic guard evaluation for --local mode: rule matching, tool-filter
// glob checks, and transform (redaction). All matching is in-process; no model
// or network is involved.

const (
	// hookPhasePostflight mirrors the SDK phase every tool-call adapter sends,
	// so a wire-value change cannot silently make every rule skip.
	hookPhasePostflight = string(agento11y.HookPhasePostflight)
	// hookPhasePreflight is the other phase a rule may name. Only these two
	// compile, so a misspelled phase is reported rather than silently inert.
	hookPhasePreflight = string(agento11y.HookPhasePreflight)
)

// evaluateWithTransform selects rules by exact phase and optional match, then
// evaluates them in order. It applies each rule's transform, tool filter, and
// deterministic evaluators. A tripped deny or allow stops evaluation; a warning
// continues. Checks read the original input, so redaction cannot hide content
// from later rules. Rewrites accumulate separately in transformed_input. For
// non-deny results, the second return value lets callers reapply local patterns
// to input rewritten from an unredacted relay. logger may be nil.
func evaluateWithTransform(rules []CompiledRule, logger *log.Logger, req agento11y.HookEvaluateRequest) (Response, *Transform) {
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}

	phase := strings.ToLower(strings.TrimSpace(string(req.Phase)))
	if phase == "" {
		phase = hookPhasePostflight
	}

	original := req.Input
	working := req.Input
	// Never nil: the SDKs decode evaluations as an array, so an allow with no
	// rules has to serialize as [] rather than null.
	out := []Evaluation{}
	anyTransform := false
	transformRuleID := ""
	var appliedPatterns []compiledPattern

	// snapshot returns transformed_input only after a rewrite. ApplyTransform
	// already made working private, so no second copy is needed.
	snapshot := func() *agento11y.HookInput {
		if !anyTransform {
			return nil
		}
		return &working
	}

	// tripped applies the rule effect. Warnings continue. Denies stop without
	// transformed input because denied calls do not run. Allows stop and preserve
	// accumulated redactions; later rules do not run.
	tripped := func(rule CompiledRule, reason string) (Response, *Transform, bool) {
		switch rule.effect {
		case EffectWarn:
			return Response{}, nil, false
		case EffectAllow:
			return Response{
				Action:           agento11y.HookActionAllow,
				RuleID:           rule.id,
				Reason:           reason,
				TransformedInput: snapshot(),
				Evaluations:      out,
			}, transformOf(appliedPatterns), true
		default:
			return Response{
				Action:      agento11y.HookActionDeny,
				RuleID:      rule.id,
				Reason:      reason,
				Evaluations: out,
			}, nil, true
		}
	}

	for _, rule := range rules {
		if rule.phase != phase {
			continue
		}
		if !matchRule(rule.match, req.Context) {
			continue
		}
		// A rule with no enforceable action locally is skipped. That covers rules
		// that only reference cloud evaluator_ids and rules whose only evaluators
		// are Cloud-only kinds (heuristic, llm_judge, prompt_guard), which the local
		// engine does not run. The skip is only visible when AGENTO11Y_DEBUG=true
		// (the daemon logger is io.Discard otherwise).
		if !ruleEnforceable(rule) {
			logger.Printf("local guards: skipping rule %q: nothing enforceable locally (no tool_filter, transform, or deterministic evaluator)", rule.id)
			continue
		}

		if rule.transform != nil {
			next, changed, dropped := ApplyTransform(working, rule.transform, logger)
			working = next
			appliedPatterns = append(appliedPatterns, rule.transform.patterns...)
			if changed {
				anyTransform = true
				if transformRuleID == "" {
					transformRuleID = rule.id
				}
			}
			// Report a dropped redaction in the response because the original value
			// remains even though the rule is active.
			for _, what := range dropped {
				he := rule.evaluation("transform", false)
				he.Reason = fmt.Sprintf("%s could not redact %s: the rewritten value is not valid JSON, so the original is unchanged", rule.label(), what)
				out = append(out, he)
			}
		}

		if blocked, toolName := checkToolFilter(original, rule.filter); blocked {
			reason := rule.tripReason(fmt.Sprintf("tool %q matched tool_filter", toolName))
			he := rule.evaluation("tool_filter", false)
			he.Reason = reason
			out = append(out, he)
			if resp, applied, stop := tripped(rule, reason); stop {
				return resp, applied
			}
			// A warning records the tool-filter result and moves to the next
			// rule. Evaluators on this rule do not report the same rejection again.
			continue
		}

		// Inline evaluators read the original input. Only deterministic kinds
		// compile, so this loop never calls a model or network service.
		for _, ev := range rule.evaluators {
			ran, passed, explanation := runEvaluator(ev, original)
			if !ran {
				continue
			}
			he := rule.evaluation(ev.kind, passed)
			he.Explanation = explanation
			if passed {
				out = append(out, he)
				continue
			}
			what := fmt.Sprintf("%s evaluator failed", ev.kind)
			if explanation != "" {
				what += ": " + explanation
			}
			reason := rule.tripReason(what)
			he.Reason = reason
			out = append(out, he)
			if resp, applied, stop := tripped(rule, reason); stop {
				return resp, applied
			}
		}
	}

	return Response{
		Action:           agento11y.HookActionAllow,
		RuleID:           transformRuleID,
		TransformedInput: snapshot(),
		Evaluations:      out,
	}, transformOf(appliedPatterns)
}

// tripReason names the rule and describes the tripped check using its actual
// effect. An allow exception must not claim that it blocked the call.
func (r CompiledRule) tripReason(what string) string {
	switch r.effect {
	case EffectAllow:
		return fmt.Sprintf("%s allowed the call: %s", r.label(), what)
	case EffectWarn:
		return fmt.Sprintf("%s warned: %s", r.label(), what)
	default:
		return fmt.Sprintf("%s denied the call: %s", r.label(), what)
	}
}

// transformOf wraps the patterns the matching rules applied, or nil when none
// ran. The caller re-runs them over an input another stage rewrote from the
// un-redacted relay.
func transformOf(patterns []compiledPattern) *Transform {
	if len(patterns) == 0 {
		return nil
	}
	return &Transform{patterns: patterns}
}

// ruleEnforceable reports whether a rule has anything that can act locally: a
// tool filter, a transform, or a deterministic evaluator. compileEvaluator
// drops the Cloud-only kinds, so a rule whose only evaluators were heuristic,
// llm_judge or prompt_guard arrives here with none.
func ruleEnforceable(rule CompiledRule) bool {
	return rule.filter != nil || rule.transform != nil || len(rule.evaluators) > 0
}

// matchRule reports whether every condition in a rule's match block holds. A
// rule with no match block reads every call. See glob.go for the pattern
// syntax.
func matchRule(match []compiledMatch, ctx agento11y.HookContext) bool {
	for _, condition := range match {
		if !condition.values.matchAny(condition.contextValue(ctx)) {
			return false
		}
	}
	return true
}

// contextValue reads the field this condition matches on. A field the request
// did not carry is the empty string, which only a pattern written to match it
// can satisfy.
func (m compiledMatch) contextValue(ctx agento11y.HookContext) string {
	switch m.field {
	case matchAgentName:
		return ctx.AgentName
	case matchAgentVersion:
		return ctx.AgentVersion
	case matchModelProvider:
		if ctx.Model == nil {
			return ""
		}
		return ctx.Model.Provider
	case matchModelName:
		if ctx.Model == nil {
			return ""
		}
		return ctx.Model.Name
	default:
		return ctx.Tags[m.tagKey]
	}
}

// checkToolFilter scans the current output for a tool call whose name (or
// name(input_json) qualified form) matches a blocked glob. `in.Messages` is
// prompt history, not the call under evaluation.
func checkToolFilter(in agento11y.HookInput, filter *toolFilter) (bool, string) {
	if filter == nil {
		return false, ""
	}
	return scanToolCalls(in.Output, filter)
}

func scanToolCalls(msgs []agento11y.Message, filter *toolFilter) (bool, string) {
	for _, msg := range msgs {
		for _, part := range msg.Parts {
			if part.ToolCall == nil {
				continue
			}
			name := strings.TrimSpace(part.ToolCall.Name)
			if name == "" {
				continue
			}
			if filter.names.matchAny(name) {
				return true, name
			}
			// Rendering the arguments costs a decode and an encode, so it waits
			// until a pattern asks for them.
			if len(filter.qualified) == 0 {
				continue
			}
			qualified := name + "(" + toolArgsForMatch(part.ToolCall.InputJSON) + ")"
			if filter.qualified.matchAny(qualified) {
				return true, name
			}
		}
	}
	return false, ""
}

// toolArgsForMatch renders tool arguments the way the pattern author reads
// them. The caller chooses its own JSON escaping, so "rm\u0020-rf" and "rm -rf"
// are the same command line on the wire, and matching the bytes as they arrived
// lets the first form walk past a filter written for the second. Decoding and
// re-encoding settles on one spelling. Bytes that are not JSON match as they
// arrived.
func toolArgsForMatch(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || !json.Valid([]byte(trimmed)) {
		return trimmed
	}
	dec := json.NewDecoder(strings.NewReader(trimmed))
	// Keep numbers as their text: re-encoding a large id through float64 would
	// print it in exponent form, which no pattern is written against.
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return trimmed
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// Without this the encoder writes < > & as \u003c \u003e \u0026, which is the
	// same miss this function exists to close.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return trimmed
	}
	return strings.TrimSpace(buf.String())
}
