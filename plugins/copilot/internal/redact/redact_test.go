package redact

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRedactText(t *testing.T) {
	got := New().Redact("token=glc_abcdefghijklmnopqrstuvwxyz")
	if strings.Contains(got, "glc_abcdefghijklmnopqrstuvwxyz") {
		t.Fatalf("secret leaked: %q", got)
	}
	if !strings.Contains(got, "[REDACTED:") {
		t.Fatalf("missing redaction marker: %q", got)
	}
}

func TestRedactJSON(t *testing.T) {
	raw := json.RawMessage(`{"Authorization":"Bearer short","clientSecret":"short-secret","safe":"visible"}`)
	got := string(New().RedactJSON(raw))
	for _, secret := range []string{"Bearer short", "short-secret"} {
		if strings.Contains(got, secret) {
			t.Fatalf("secret %q leaked in %s", secret, got)
		}
	}
	if !strings.Contains(got, "visible") {
		t.Fatalf("safe value should remain visible: %s", got)
	}
}
