package guardeval

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gobwas/glob"
)

// Every pattern a rule matches with is a glob: `*` for any run of characters,
// `?` for one, `[a-z]` for a class, `\` to escape any of them. One syntax
// covers both places a rule names something: the match block that selects which
// calls a rule reads, and tool_filter.blocked_names.
//
// Two properties are deliberate. Matching is case-insensitive, because hosts
// disagree on capitalization ("Bash" against "bash") and a rule must not depend
// on which host produced the call. And `*` spans `/`, because the things being
// matched are working directories, paths inside tool arguments and model names,
// none of which are path segments a rule wants to stop at.
//
// Patterns compile when the ruleset does, so a malformed one is reported
// against the rule holding it rather than matching nothing on every call for
// the rest of the daemon's life.

// globSet is a set of patterns any one of which satisfies the condition.
type globSet []glob.Glob

// compileGlob compiles one pattern, lowercased so matching is
// case-insensitive. A blank pattern yields (nil, nil): the rules file is
// hand-written, and a trailing empty string in a TOML list is a typo, not a
// pattern that matches everything.
func compileGlob(pattern string) (glob.Glob, error) {
	trimmed := strings.ToLower(strings.TrimSpace(pattern))
	if trimmed == "" {
		return nil, nil
	}
	// The pattern is quoted as it was written, not as it was lowercased or
	// escaped, so the message names the text to go and change.
	g, err := glob.Compile(escapeBraces(trimmed))
	if err != nil {
		return nil, fmt.Errorf("pattern %q: %w", strings.TrimSpace(pattern), err)
	}
	return g, nil
}

// escapeBraces makes `{` and `}` literal characters.
//
// The glob library reads them as alternation, `{read,write}_file`, and a rule
// that filters on tool arguments is full of unpaired braces already:
// `Bash({"command":"rm*)` is written against a subject that is JSON. Worse, the
// library does not reject an unpaired brace. It drops it, so that pattern would
// compile, stop matching the call it was written for, and report nothing. A
// guard that silently stops guarding is the one outcome to design out, and
// alternation buys little here: blocked_names is already a list, and a match
// value already takes comma-separated alternatives.
//
// A backslash escape the user wrote is passed through with the character it
// escapes, so `\{` stays one escaped brace instead of becoming an escaped
// backslash followed by an open brace.
func escapeBraces(pattern string) string {
	if !strings.ContainsAny(pattern, "{}") {
		return pattern
	}
	var b strings.Builder
	b.Grow(len(pattern) + 4)
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch {
		case c == '\\' && i+1 < len(pattern):
			b.WriteByte(c)
			i++
			b.WriteByte(pattern[i])
		case c == '{' || c == '}':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// compileGlobs compiles a list of patterns, dropping the blanks.
func compileGlobs(patterns []string) (globSet, error) {
	out := make(globSet, 0, len(patterns))
	for _, pattern := range patterns {
		g, err := compileGlob(pattern)
		if err != nil {
			return nil, err
		}
		if g != nil {
			out = append(out, g)
		}
	}
	return out, nil
}

// matchAny reports whether the subject satisfies any pattern in the set. The
// subject is lowercased once for the whole set, not once per pattern.
func (s globSet) matchAny(subject string) bool {
	if len(s) == 0 {
		return false
	}
	normalized := strings.ToLower(strings.TrimSpace(subject))
	for _, g := range s {
		if g.Match(normalized) {
			return true
		}
	}
	return false
}

// toolFilter is a compiled tool_filter block.
type toolFilter struct {
	// names match the tool name on its own.
	names globSet
	// qualified match the "name(input_json)" form, which is what makes an
	// argument filter (`Bash(*rm*)`) different from a name filter (`Bash`): the
	// name filter must not reach into the arguments. A pattern joins this set by
	// containing "(", so a call with no arguments still forms `Name()` and
	// `Name(*)` covers it.
	qualified globSet
}

// compileToolFilter compiles a tool_filter block, or returns (nil, nil) when
// there is nothing to filter on.
func compileToolFilter(cfg *ToolFilterConfig) (*toolFilter, error) {
	if cfg == nil {
		return nil, nil
	}
	out := &toolFilter{}
	for _, pattern := range cfg.BlockedNames {
		g, err := compileGlob(pattern)
		if err != nil {
			return nil, fmt.Errorf("tool_filter.blocked_names: %w", err)
		}
		if g == nil {
			continue
		}
		if strings.Contains(pattern, "(") {
			out.qualified = append(out.qualified, g)
			continue
		}
		out.names = append(out.names, g)
	}
	if len(out.names) == 0 && len(out.qualified) == 0 {
		return nil, nil
	}
	return out, nil
}

// matchField names the piece of the request context a match key reads.
type matchField int

const (
	matchAgentName matchField = iota
	matchAgentVersion
	matchModelProvider
	matchModelName
	matchTag
)

// compiledMatch is one condition out of a rule's match block: the context field
// it reads, and the patterns any one of which satisfies it.
type compiledMatch struct {
	field matchField
	// tagKey is the tag name, for matchTag only.
	tagKey string
	values globSet
}

// compileMatch compiles a rule's match block. Keys are compiled in sorted order
// so two runs over the same rule report the same problem first; Go map order
// would otherwise make the error text depend on the run.
//
// A key this build does not know is an error rather than a condition that never
// holds. Both leave the rule enforcing nothing, and only one says so.
func compileMatch(match map[string]any) ([]compiledMatch, error) {
	if len(match) == 0 {
		return nil, nil
	}
	keys := make([]string, 0, len(match))
	for key := range match {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	out := make([]compiledMatch, 0, len(keys))
	for _, key := range keys {
		compiled, err := compileMatchKey(key, match[key])
		if err != nil {
			return nil, err
		}
		out = append(out, compiled)
	}
	return out, nil
}

func compileMatchKey(key string, raw any) (compiledMatch, error) {
	out := compiledMatch{}
	switch {
	case key == "agent_name":
		out.field = matchAgentName
	case key == "agent_version":
		out.field = matchAgentVersion
	case key == "model.provider":
		out.field = matchModelProvider
	case key == "model.name":
		out.field = matchModelName
	case strings.HasPrefix(key, "tags."):
		out.field = matchTag
		out.tagKey = strings.TrimPrefix(key, "tags.")
		if out.tagKey == "" {
			return compiledMatch{}, fmt.Errorf("match key %q names no tag", key)
		}
	default:
		return compiledMatch{}, fmt.Errorf("match key %q is not one of: agent_name, agent_version, model.provider, model.name, tags.<name>", key)
	}

	values, err := compileGlobs(expectedMatchValues(raw))
	if err != nil {
		return compiledMatch{}, fmt.Errorf("match.%s: %w", key, err)
	}
	if len(values) == 0 {
		return compiledMatch{}, fmt.Errorf("match.%s has no value to match against", key)
	}
	out.values = values
	return out, nil
}

// expectedMatchValues accepts a string or a list of strings. A scalar can carry
// comma-separated alternatives, which is what the rule editor emits.
func expectedMatchValues(raw any) []string {
	switch typed := raw.(type) {
	case string:
		var out []string
		for part := range strings.SplitSeq(typed, ",") {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				out = append(out, trimmed)
			}
		}
		return out
	case []any:
		out := make([]string, 0, len(typed))
		for _, value := range typed {
			if asString, ok := value.(string); ok {
				if trimmed := strings.TrimSpace(asString); trimmed != "" {
					out = append(out, trimmed)
				}
			}
		}
		return out
	default:
		return nil
	}
}
