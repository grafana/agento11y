package config

import (
	"bytes"
	"log"
	"testing"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/mapper"
)

// TestConfig_Agent pins the fallback a Config built without Load relies on:
// the guard request must carry the same name the mapper stamps, never a blank.
func TestConfig_Agent(t *testing.T) {
	tests := []struct {
		name      string
		agentName string
		want      string
	}{
		{name: "a resolved name is returned unchanged", agentName: "cursor-e2e", want: "cursor-e2e"},
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
