package redact

import (
	"regexp"
	"strings"
)

// secretPattern is one compiled pattern from the generated table in
// patterns_gen.go. The table comes from redaction/patterns.json, which also
// feeds the four SDKs; edit that file and run `mise run generate:redaction` to
// change a pattern.
type secretPattern struct {
	id string
	re *regexp.Regexp
}

// tier2Pattern carries a replacement template that keeps the matched key and
// rewrites only the value, so JSON and env syntax survive redaction.
type tier2Pattern struct {
	id          string
	re          *regexp.Regexp
	replacement string
}

// tier1Combined alternates all tier1Patterns into a single regex so each input
// is scanned once instead of once per pattern. Each pattern is wrapped in a
// capturing group; the matched group index identifies which pattern fired.
// Scanning once is also what makes plugin output identical to the SDKs': with
// per-pattern passes an earlier pattern can rewrite text a later one would
// have matched.
var tier1Combined = func() *regexp.Regexp {
	parts := make([]string, len(tier1Patterns))
	for i, p := range tier1Patterns {
		parts[i] = "(" + p.re.String() + ")"
	}
	return regexp.MustCompile(strings.Join(parts, "|"))
}()

// Options configures a Redactor.
type Options struct {
	// RedactEmailAddresses turns on the email pattern. It defaults to false:
	// agent transcripts routinely carry commit authors and reviewer addresses,
	// and redacting them costs more context than it protects. See
	// redaction/README.md.
	RedactEmailAddresses bool
}

// Redactor applies Tier 1 (high-confidence) and Tier 2 (heuristic) secret
// patterns. The zero value is ready to use and leaves email addresses alone.
type Redactor struct {
	includeEmail bool
}

// New returns a Redactor with email redaction off.
func New() *Redactor { return &Redactor{} }

// NewWithOptions returns a Redactor configured by opts.
func NewWithOptions(opts Options) *Redactor {
	return &Redactor{includeEmail: opts.RedactEmailAddresses}
}

// Redact applies both Tier 1 and Tier 2 patterns.
func (r *Redactor) Redact(text string) string {
	text = r.redactTier1(text)
	for _, p := range tier2Patterns {
		text = p.re.ReplaceAllString(text, p.replacement)
	}
	return text
}

// RedactLightweight applies only Tier 1 (high-confidence) patterns.
func (r *Redactor) RedactLightweight(text string) string {
	return r.redactTier1(text)
}

// redactTier1 applies the combined tier 1 scan and, when enabled, the email
// pattern. Both redaction modes share it.
func (r *Redactor) redactTier1(text string) string {
	text = replaceTier1(text)
	if r.includeEmail {
		text = emailPattern.re.ReplaceAllString(text, "[REDACTED:"+emailPattern.id+"]")
	}
	return text
}

func replaceTier1(s string) string {
	matches := tier1Combined.FindAllStringSubmatchIndex(s, -1)
	if len(matches) == 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	last := 0
	for _, m := range matches {
		b.WriteString(s[last:m[0]])
		for g := 1; g <= len(tier1Patterns); g++ {
			if m[2*g] >= 0 {
				b.WriteString("[REDACTED:")
				b.WriteString(tier1Patterns[g-1].id)
				b.WriteByte(']')
				break
			}
		}
		last = m[1]
	}
	b.WriteString(s[last:])
	return b.String()
}
