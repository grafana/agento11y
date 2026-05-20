package redact

import "regexp"

type pattern struct {
	id   string
	re   *regexp.Regexp
	tier int
}

type Redactor struct {
	patterns []pattern
}

func New() *Redactor {
	defs := []struct {
		id   string
		expr string
		tier int
	}{
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
		{"env-secret-value", `(?i)(?:^|\W|_)(?:PASSWORD|SECRET|TOKEN|KEY|CREDENTIAL|API_KEY|PRIVATE_KEY|ACCESS_KEY)\s*[=:]\s*\S+`, 2},
		{"json-secret-field", `(?i)"(?:password|secret|token|credential|api_?key|private_?key|access_?key|client_?secret|auth_?token|secret_?key)"\s*:\s*"[^"]+"`, 2},
	}
	r := &Redactor{patterns: make([]pattern, 0, len(defs))}
	for _, d := range defs {
		r.patterns = append(r.patterns, pattern{id: d.id, re: regexp.MustCompile(d.expr), tier: d.tier})
	}
	return r
}

func (r *Redactor) Redact(text string) string {
	out := text
	for _, p := range r.patterns {
		out = p.re.ReplaceAllString(out, "[REDACTED:"+p.id+"]")
	}
	return out
}
