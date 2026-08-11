package config

import (
	"bytes"
	"log"
	"testing"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/codex/mapper"
)

// TestLoad_SkipPromptRedaction pins the prompt-redaction opt-out for codex
// against the shared envconfig resolver, mirroring the copilot config test.
func TestLoad_SkipPromptRedaction(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{name: "redacts when unset", raw: "", want: false},
		{name: "opts out on false", raw: "false", want: true},
		{name: "redacts on true", raw: "true", want: false},
		{name: "redacts on a typo", raw: "flase", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AGENTO11Y_REDACT_INPUT_MESSAGES", tt.raw)
			t.Setenv("SIGIL_REDACT_INPUT_MESSAGES", "")
			cfg := Load(log.New(&bytes.Buffer{}, "", 0))
			if cfg.SkipPromptRedaction != tt.want {
				t.Errorf("SkipPromptRedaction = %t, want %t", cfg.SkipPromptRedaction, tt.want)
			}
		})
	}
}

// TestLoad_Guards pins the env-var contract and defaults for codex against
// the shared envconfig resolver, mirroring the copilot config test so the
// two agents stay byte-identical on the guard knobs that ship in
// ~/.config/agento11y/config.env.
func TestLoad_Guards(t *testing.T) {
	tests := []struct {
		name          string
		env           map[string]string
		wantEnabled   bool
		wantFailOpen  bool
		wantTimeoutMs int
	}{
		{
			name:          "defaults are disabled fail-open 1500ms",
			env:           map[string]string{},
			wantEnabled:   false,
			wantFailOpen:  true,
			wantTimeoutMs: 1500,
		},
		{
			name: "enabled via env",
			env: map[string]string{
				"SIGIL_GUARDS_ENABLED": "true",
			},
			wantEnabled:   true,
			wantFailOpen:  true,
			wantTimeoutMs: 1500,
		},
		{
			name: "fail-closed via env",
			env: map[string]string{
				"SIGIL_GUARDS_ENABLED":   "true",
				"SIGIL_GUARDS_FAIL_OPEN": "false",
			},
			wantEnabled:   true,
			wantFailOpen:  false,
			wantTimeoutMs: 1500,
		},
		{
			name: "timeout override",
			env: map[string]string{
				"SIGIL_GUARDS_ENABLED":    "true",
				"SIGIL_GUARDS_TIMEOUT_MS": "750",
			},
			wantEnabled:   true,
			wantFailOpen:  true,
			wantTimeoutMs: 750,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("SIGIL_GUARDS_ENABLED", "")
			t.Setenv("SIGIL_GUARDS_FAIL_OPEN", "")
			t.Setenv("SIGIL_GUARDS_TIMEOUT_MS", "")
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			cfg := Load(log.New(&bytes.Buffer{}, "", 0))
			if cfg.Guards.Enabled != tt.wantEnabled {
				t.Errorf("Guards.Enabled = %t, want %t", cfg.Guards.Enabled, tt.wantEnabled)
			}
			if cfg.Guards.FailOpen != tt.wantFailOpen {
				t.Errorf("Guards.FailOpen = %t, want %t", cfg.Guards.FailOpen, tt.wantFailOpen)
			}
			if cfg.Guards.TimeoutMs != tt.wantTimeoutMs {
				t.Errorf("Guards.TimeoutMs = %d, want %d", cfg.Guards.TimeoutMs, tt.wantTimeoutMs)
			}
		})
	}
}

// TestConfig_Agent pins the fallback a Config built without Load relies on:
// the guard request must carry the same name the mapper stamps, never a blank.
func TestConfig_Agent(t *testing.T) {
	tests := []struct {
		name      string
		agentName string
		want      string
	}{
		{name: "a resolved name is returned unchanged", agentName: "codex-e2e", want: "codex-e2e"},
		{name: "blank falls back to the product name", agentName: "", want: mapper.AgentName},
		{name: "whitespace falls back to the product name", agentName: "  ", want: mapper.AgentName},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (Config{AgentName: tt.agentName}).Agent(); got != tt.want {
				t.Errorf("Agent() = %q, want %q", got, tt.want)
			}
		})
	}
}
