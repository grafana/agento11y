package redact

import (
	"encoding/json"
	"testing"
)

// One input carries a tier 1 token and a tier 2 key/value pair, so each
// helper's output shows which tiers ran.
func TestRedactorFieldTiers(t *testing.T) {
	r := New()
	const probe = "glc_abcdefghijklmnopqrstuvwx and DB_PASSWORD=hunter2"
	const bothTiers = "[REDACTED:grafana-cloud-token] and DB_PASSWORD=[REDACTED:env-secret-value]"
	const tier1Only = "[REDACTED:grafana-cloud-token] and DB_PASSWORD=hunter2"

	tests := []struct {
		name string
		got  string
		want string
	}{
		{"prompt", r.Prompt(probe), bothTiers},
		{"system prompt", r.SystemPrompt(probe), bothTiers},
		{"tool payload", r.ToolPayload(probe), bothTiers},
		{"assistant text", r.AssistantText(probe), tier1Only},
		{"thinking", r.Thinking(probe), tier1Only},
		{"title", r.Title(probe), tier1Only},
		{"error text", r.ErrorText(probe), tier1Only},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("got  %q\nwant %q", tt.got, tt.want)
			}
		})
	}
}

func TestRedactorToolPayloadJSON(t *testing.T) {
	r := New()

	tests := []struct {
		name string
		raw  json.RawMessage
		want string
	}{
		{
			name: "redacts a quoted secret and stays parseable",
			raw:  json.RawMessage(`{"cmd":"deploy","password":"hunter2"}`),
			want: `{"cmd":"deploy","password":"[REDACTED:json-secret-field]"}`,
		},
		{
			name: "redacts a secret inside a command string",
			raw:  json.RawMessage(`{"cmd":"export TOKEN=hunter2"}`),
			want: `{"cmd":"export TOKEN=[REDACTED:env-secret-value]"}`,
		},
		{
			// Past tier 2's fixed key list, which has no "authorization".
			name: "redacts a value under a secret-looking key",
			raw:  json.RawMessage(`{"authorization":"Basic aGk6dGhlcmU="}`),
			want: `{"authorization":"[REDACTED:json-secret-field]"}`,
		},
		{
			name: "empty stays empty",
			raw:  nil,
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(r.ToolPayloadJSON(tt.raw))
			if got != tt.want {
				t.Fatalf("got  %s\nwant %s", got, tt.want)
			}
			if got == "" {
				return
			}
			var v any
			if err := json.Unmarshal([]byte(got), &v); err != nil {
				t.Errorf("output is not valid JSON: %v", err)
			}
		})
	}
}

func TestRedactorToolPayloadText(t *testing.T) {
	r := New()

	tests := []struct {
		name string
		raw  json.RawMessage
		want string
	}{
		{
			name: "unwraps a JSON string",
			raw:  json.RawMessage(`"export TOKEN=hunter2"`),
			want: `export TOKEN=[REDACTED:env-secret-value]`,
		},
		{
			name: "keeps an object as JSON",
			raw:  json.RawMessage(`{"password":"hunter2"}`),
			want: `{"password":"[REDACTED:json-secret-field]"}`,
		},
		{
			name: "empty stays empty",
			raw:  nil,
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := r.ToolPayloadText(tt.raw); got != tt.want {
				t.Errorf("got  %s\nwant %s", got, tt.want)
			}
		})
	}
}
