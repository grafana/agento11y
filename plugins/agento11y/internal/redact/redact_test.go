package redact

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRedactor_Tier1Patterns(t *testing.T) {
	r := New()

	tests := []struct {
		name    string
		input   string
		wantID  string
		wantHit bool
	}{
		{"grafana cloud token", "token is glc_abcdefghijklmnopqrstuvwx", "grafana-cloud-token", true},
		{"grafana SA token", "use glsa_abcdefghijklmnopqrstuvwx", "grafana-service-account-token", true},
		{"AWS AKIA key", "AKIAIOSFODNN7EXAMPLE", "aws-access-token", true},
		{"AWS ASIA key", "ASIAIOSFODNN7EXAMPLE", "aws-access-token", true},
		{"GitHub PAT", "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij", "github-pat", true},
		{"GitHub OAuth", "gho_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij", "github-oauth", true},
		{"GitHub App token", "ghs_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij", "github-app-token", true},
		{"OpenAI project key", "sk-proj-abcdefghijklmnopqrstuvwxyz1234567890abcd", "openai-project-key", true},
		{"GCP API key", "AIzaSyA1234567890abcdefghijklmnopqrstuvw", "gcp-api-key", true},
		{"Bearer token", "Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.abc", "bearer-token", true},
		{"Slack token", "xoxb-1234567890-abcdefghij", "slack-token", true},
		{"Stripe test key", "sk_test_abcdefghijklmnopqrstuvwxyz", "stripe-key", true},
		{"SendGrid key", "SG.abcdefghijklmnopqrstuv.abcdefghijklmnopqrstuvwxyz12345678901234567", "sendgrid-api-key", true},
		{"npm token", "npm_abcdefghijklmnopqrstuvwxyz1234567890", "npm-token", true},
		{"connection string", "postgres://user:pass@localhost:5432/db", "connection-string", true},
		{"PEM private key", "-----BEGIN RSA PRIVATE KEY-----\nMIIE\n-----END RSA PRIVATE KEY-----", "private-key", true},

		{"normal text", "hello world", "", false},
		{"short token-like", "glc_short", "", false},
		{"code variable", "const myKey = getValue()", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := r.Redact(tt.input)
			hasRedaction := strings.Contains(result, "[REDACTED:")

			if hasRedaction != tt.wantHit {
				t.Errorf("Redact(%q) redacted=%v, want=%v\nresult: %s", tt.input, hasRedaction, tt.wantHit, result)
			}

			if tt.wantHit && !strings.Contains(result, "[REDACTED:"+tt.wantID+"]") {
				t.Errorf("expected pattern ID %q in result: %s", tt.wantID, result)
			}
		})
	}
}

func TestRedactor_Tier2Patterns(t *testing.T) {
	r := New()

	tests := []struct {
		name    string
		input   string
		wantHit bool
	}{
		{"env PASSWORD", "DATABASE_PASSWORD=mysecretpass123", true},
		{"env SECRET", "APP_SECRET: s3cr3t_v4lu3", true},
		{"env API_KEY", "API_KEY=abcdef12345", true},
		{"json password field", `"password": "hunter2"`, true},
		{"json secret field", `"client_secret": "abcdef123456"`, true},
		{"normal assignment", "count = 42", false},
		{"normal json", `"name": "alice"`, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := r.Redact(tt.input)
			hasRedaction := strings.Contains(result, "[REDACTED:")

			if hasRedaction != tt.wantHit {
				t.Errorf("Redact(%q) redacted=%v, want=%v\nresult: %s", tt.input, hasRedaction, tt.wantHit, result)
			}
		})
	}
}

func TestRedactor_LightweightSkipsTier2(t *testing.T) {
	r := New()

	input := "DATABASE_PASSWORD=mysecretpass123"
	result := r.RedactLightweight(input)
	if strings.Contains(result, "[REDACTED:") {
		t.Errorf("RedactLightweight should not catch Tier 2 patterns, got: %s", result)
	}

	input2 := "token glc_abcdefghijklmnopqrstuvwx"
	result2 := r.RedactLightweight(input2)
	if !strings.Contains(result2, "[REDACTED:grafana-cloud-token]") {
		t.Errorf("RedactLightweight should catch Tier 1 patterns, got: %s", result2)
	}
}

func TestRedactor_MultipleSecrets(t *testing.T) {
	r := New()

	input := "use glc_abcdefghijklmnopqrstuvwx and Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.abc"
	result := r.Redact(input)

	if !strings.Contains(result, "[REDACTED:grafana-cloud-token]") {
		t.Error("missing grafana-cloud-token redaction")
	}
	if !strings.Contains(result, "[REDACTED:bearer-token]") {
		t.Error("missing bearer-token redaction")
	}
}

func TestRedactor_NoFalsePositivesOnCode(t *testing.T) {
	r := New()

	codeSnippets := []string{
		`func getAPIKey() string { return os.Getenv("API_KEY") }`,
		`if err != nil { return fmt.Errorf("bearer auth failed: %w", err) }`,
		`const maxTokens = 4096`,
		`log.Info("processing request", "token_count", len(tokens))`,
		`var sk = newSecretKeeper()`,
	}

	for _, code := range codeSnippets {
		result := r.RedactLightweight(code)
		if strings.Contains(result, "[REDACTED:") {
			t.Errorf("false positive on code: %q -> %q", code, result)
		}
	}
}

func TestRedactCoversRepoStandardTokenCorpus(t *testing.T) {
	cases := map[string]string{
		"anthropic-admin-key": "sk-ant-admin01-" + strings.Repeat("A", 93) + "AA",
		"openai-api-key":      "sk-" + strings.Repeat("A", 20) + "T3BlbkFJ" + strings.Repeat("B", 20),
		"sendgrid-api-key":    "SG." + strings.Repeat("A", 22) + "." + strings.Repeat("B", 43),
		"twilio-api-key":      "SK" + strings.Repeat("a", 32),
		"npm-token":           "npm_" + strings.Repeat("A", 36),
		"pypi-token":          "pypi-" + strings.Repeat("A", 50),
	}
	red := New()
	for name, secret := range cases {
		t.Run(name, func(t *testing.T) {
			got := red.Redact("secret=" + secret)
			if strings.Contains(got, secret) {
				t.Fatalf("secret leaked: %s", got)
			}
			if !strings.Contains(got, "[REDACTED:") {
				t.Fatalf("missing redaction marker for %s: %s", name, got)
			}
		})
	}
}

func TestRedactJSONCoversRepoStandardTokenCorpus(t *testing.T) {
	secret := "SG." + strings.Repeat("A", 22) + "." + strings.Repeat("B", 43)
	raw := json.RawMessage(`{"value":"` + secret + `"}`)

	got := string(New().RedactJSON(raw))
	if strings.Contains(got, secret) {
		t.Fatalf("secret leaked: %s", got)
	}
	if !strings.Contains(got, "[REDACTED:sendgrid-api-key]") {
		t.Fatalf("missing sendgrid marker: %s", got)
	}
}

func TestRedactJSONKeepsTokenCountFieldsVisible(t *testing.T) {
	raw := json.RawMessage(`{
		"max_tokens": 100,
		"input_tokens": 20,
		"output_tokens": 10,
		"total_tokens": 30,
		"promptTokenCount": 40,
		"token_count": 50,
		"tokenCount": 60,
		"tokenUsage": {"input_tokens": 20},
		"authToken": "secret",
		"idToken": "id-secret",
		"id_token": "snake-secret"
	}`)

	var got map[string]any
	if err := json.Unmarshal(New().RedactJSON(raw), &got); err != nil {
		t.Fatalf("unmarshal redacted json: %v", err)
	}
	for _, key := range []string{"max_tokens", "input_tokens", "output_tokens", "total_tokens", "promptTokenCount", "token_count", "tokenCount"} {
		if got[key] == "[REDACTED:json-secret-field]" {
			t.Fatalf("%s was over-redacted: %#v", key, got)
		}
	}
	if _, ok := got["tokenUsage"].(map[string]any); !ok {
		t.Fatalf("tokenUsage was over-redacted: %#v", got)
	}
	if got["authToken"] != "[REDACTED:json-secret-field]" {
		t.Fatalf("authToken was not redacted: %#v", got)
	}
	if got["idToken"] != "[REDACTED:json-secret-field]" {
		t.Fatalf("idToken was not redacted: %#v", got)
	}
	if got["id_token"] != "[REDACTED:json-secret-field]" {
		t.Fatalf("id_token was not redacted: %#v", got)
	}
}

func TestRedactJSONRedactsServiceTokenKeys(t *testing.T) {
	raw := json.RawMessage(`{
		"github_token": "short-github-token",
		"grafanaToken": "short-grafana-token",
		"slack_token": "short-slack-token",
		"serviceToken": "short-service-token"
	}`)

	var got map[string]any
	if err := json.Unmarshal(New().RedactJSON(raw), &got); err != nil {
		t.Fatalf("unmarshal redacted json: %v", err)
	}
	for _, key := range []string{"github_token", "grafanaToken", "slack_token", "serviceToken"} {
		if got[key] != "[REDACTED:json-secret-field]" {
			t.Fatalf("%s was not redacted: %#v", key, got)
		}
	}
}
