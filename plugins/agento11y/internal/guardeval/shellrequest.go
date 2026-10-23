package guardeval

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/grafana/agento11y/go/agento11y"
	"mvdan.cc/sh/v3/syntax"
)

// ShellMetadata attests the dialect of the final command after host rewrites.
// Hosts must supply it separately from model-controlled arguments.
// Indices address the original output message and part, not caller-controlled call IDs.
type ShellMetadata struct {
	MessageIndex int    `json:"message_index"`
	PartIndex    int    `json:"part_index"`
	Dialect      string `json:"dialect"`
}

type shellCallPosition struct{ message, part int }

type shellRequestCache struct {
	ctx      context.Context
	metadata map[shellCallPosition]string
	results  map[shellCallPosition]shellAnalysis
}

func newShellRequestCache(ctx context.Context, metadata []ShellMetadata) *shellRequestCache {
	cache := &shellRequestCache{ctx: ctx, metadata: make(map[shellCallPosition]string), results: make(map[shellCallPosition]shellAnalysis)}
	for _, item := range metadata {
		position := shellCallPosition{item.MessageIndex, item.PartIndex}
		if _, duplicate := cache.metadata[position]; duplicate {
			cache.metadata[position] = ""
		} else {
			cache.metadata[position] = item.Dialect
		}
	}
	return cache
}

func (c *shellRequestCache) analysis(position shellCallPosition, tc *agento11y.ToolCall) shellAnalysis {
	if result, found := c.results[position]; found {
		return result
	}
	result := shellAnalysis{incomplete: "missing or unsupported host shell metadata"}
	if c.metadata[position] == "bash" && (tc.Name == "Bash" || tc.Name == "bash") {
		var input map[string]json.RawMessage
		var source *string
		if json.Unmarshal(tc.InputJSON, &input) == nil && json.Unmarshal(input["command"], &source) == nil && source != nil {
			result = analyzeShell(c.ctx, *source, syntax.LangBash)
		} else {
			result.incomplete = "unsupported shell input shape"
		}
	}
	c.results[position] = result
	return result
}

type shellPolicyEvaluator struct {
	profile  ShellPolicyProfile
	fallback *regexEvaluator
}

// Keep evaluation diagnostics compatible with the saved regex rules.
func (e *shellPolicyEvaluator) kind() string { return e.fallback.kind() }

func (e *shellPolicyEvaluator) evaluate(in agento11y.HookInput, cache *shellRequestCache) (ran, passed bool, explanation string) {
	passed = true
	var reasons []string
	for mi, message := range in.Output {
		for pi, part := range message.Parts {
			if cache.ctx.Err() != nil {
				return ran, false, "shell analysis canceled"
			}
			subject, found := shellCommandOf(part.ToolCall, e.fallback.shell)
			if !found {
				continue
			}
			ran = true
			analysis := cache.analysis(shellCallPosition{mi, pi}, part.ToolCall)
			denied, incomplete := checkShellPolicy(e.profile, analysis)
			if incomplete != "" {
				denied = denied || matchesAnyRegex(e.fallback.regexes, subject)
				reason := "shell analysis incomplete: " + incomplete + "; used legacy regex matching"
				if !slices.Contains(reasons, reason) {
					reasons = append(reasons, reason)
				}
			}
			if denied {
				passed = false
			}
		}
	}
	if !passed {
		reasons = append(reasons, fmt.Sprintf("blocked by %s policy", e.profile))
	}
	return ran, passed, strings.Join(reasons, "; ")
}
