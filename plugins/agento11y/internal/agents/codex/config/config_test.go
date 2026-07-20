package config

import (
	"bytes"
	"log"
	"testing"
)

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
