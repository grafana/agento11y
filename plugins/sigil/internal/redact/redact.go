package redact

import (
	"regexp"
)

type pattern struct {
	id          string
	re          *regexp.Regexp
	tier        int // 1 = high-confidence, 2 = heuristic
	replacement string
}

// compiledPatterns holds the package-wide set of regex patterns. Compiled
// once at process start so callers don't pay the regex-compile cost on every
// redaction and so any Redactor — including the zero value — uses the same
// rules. Keeping state here, not on the receiver, means `redact.Redactor{}`
// and `&redact.Redactor{}` behave identically to `redact.New()` and can never
// silently no-op because patterns were left nil.
var compiledPatterns = compilePatterns()

func compilePatterns() []pattern {
	defs := []struct {
		id   string
		expr string
		tier int
	}{
		// Tier 1: high-confidence token formats
		{"grafana-cloud-token", `\bglc_[A-Za-z0-9_-]{20,}`, 1},
		{"grafana-service-account-token", `\bglsa_[A-Za-z0-9_-]{20,}`, 1},
		{"aws-access-token", `\b(?:A3T[A-Z0-9]|AKIA|ASIA|ABIA|ACCA)[A-Z2-7]{16}\b`, 1},
		{"github-pat", `\bghp_[A-Za-z0-9_]{36,}`, 1},
		{"github-oauth", `\bgho_[A-Za-z0-9_]{36,}`, 1},
		{"github-app-token", `\bghs_[A-Za-z0-9_]{36,}`, 1},
		{"github-fine-grained-pat", `\bgithub_pat_[A-Za-z0-9_]{82}`, 1},
		{"anthropic-api-key", `\bsk-ant-api03-[a-zA-Z0-9_-]{93}AA`, 1},
		{"anthropic-admin-key", `\bsk-ant-admin01-[a-zA-Z0-9_-]{93}AA`, 1},
		{"openai-api-key", `\bsk-[a-zA-Z0-9]{20}T3BlbkFJ[a-zA-Z0-9]{20}`, 1},
		{"openai-project-key", `\bsk-proj-[a-zA-Z0-9_-]{40,}`, 1},
		{"openai-svcacct-key", `\bsk-svcacct-[a-zA-Z0-9_-]{40,}`, 1},
		{"gcp-api-key", `\bAIza[A-Za-z0-9_-]{35}`, 1},
		{"private-key", `-----BEGIN[A-Z ]*PRIVATE KEY-----[\s\S]*?-----END[A-Z ]*PRIVATE KEY-----`, 1},
		{"connection-string", `(?:postgres|mysql|mongodb|redis|amqp)://[^\s'"]+@[^\s'"]+`, 1},
		{"bearer-token", `[Bb]earer\s+[A-Za-z0-9_.\-~+/]{20,}={0,3}`, 1},
		{"slack-token", `\bxox[bporas]-[A-Za-z0-9-]{10,}`, 1},
		{"stripe-key", `\b[sr]k_(?:live|test)_[A-Za-z0-9]{20,}`, 1},
		{"sendgrid-api-key", `\bSG\.[A-Za-z0-9_-]{22}\.[A-Za-z0-9_-]{43}`, 1},
		{"twilio-api-key", `\bSK[a-f0-9]{32}`, 1},
		{"npm-token", `\bnpm_[A-Za-z0-9]{36}`, 1},
		{"pypi-token", `\bpypi-[A-Za-z0-9_-]{50,}`, 1},

		// Tier 2: heuristic patterns
		{"env-secret-value", `(?i)(?:^|\W|_)(?:PASSWORD|SECRET|TOKEN|KEY|CREDENTIAL|API_KEY|PRIVATE_KEY|ACCESS_KEY)\s*[=:]\s*\S+`, 2},
		{"json-secret-field", `(?i)"(?:password|secret|token|credential|api_?key|private_?key|access_?key|client_?secret|auth_?token|secret_?key)"\s*:\s*"[^"]+"`, 2},
	}

	out := make([]pattern, 0, len(defs))
	for _, d := range defs {
		out = append(out, pattern{
			id:          d.id,
			re:          regexp.MustCompile(d.expr),
			tier:        d.tier,
			replacement: "[REDACTED:" + d.id + "]",
		})
	}
	return out
}

// Redactor applies Tier 1 (high-confidence) and Tier 2 (heuristic) secret
// patterns. The zero value is ready to use; New is kept for symmetry with
// other "constructor returns *T" packages and is the recommended call site.
type Redactor struct{}

// New returns a Redactor pointer. Equivalent to &Redactor{} — kept as a stable
// API surface for callers that prefer constructor style.
func New() *Redactor { return &Redactor{} }

// Redact applies both Tier 1 and Tier 2 patterns.
func (r *Redactor) Redact(text string) string {
	return redactText(text, 2)
}

// RedactLightweight applies only Tier 1 (high-confidence) patterns.
func (r *Redactor) RedactLightweight(text string) string {
	return redactText(text, 1)
}

func redactText(text string, maxTier int) string {
	result := text
	for _, p := range compiledPatterns {
		if p.tier > maxTier {
			continue
		}
		result = p.re.ReplaceAllString(result, p.replacement)
	}
	return result
}
