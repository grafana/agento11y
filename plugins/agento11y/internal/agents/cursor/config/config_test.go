package config

import (
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
