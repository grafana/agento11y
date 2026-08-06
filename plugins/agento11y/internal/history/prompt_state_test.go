package history

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPromptState(t *testing.T) {
	tests := []struct {
		name      string
		decision  PromptDecision
		wantOffer bool
		wantErr   bool
	}{
		{name: "dismissed", decision: PromptSkipped, wantOffer: false},
		{name: "imported", decision: PromptImported, wantOffer: false},
		{name: "unknown decision", decision: PromptDecision("maybe"), wantErr: true, wantOffer: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pinStateHome(t)
			offer, err := ShouldOfferPrompt(AgentClaudeCode)
			if err != nil {
				t.Fatalf("ShouldOfferPrompt: %v", err)
			}
			if !offer {
				t.Fatal("a fresh state root must still offer the import")
			}

			err = MarkPrompt(AgentClaudeCode, tt.decision)
			if (err != nil) != tt.wantErr {
				t.Fatalf("MarkPrompt error = %v, wantErr %v", err, tt.wantErr)
			}
			offer, err = ShouldOfferPrompt(AgentClaudeCode)
			if err != nil {
				t.Fatalf("ShouldOfferPrompt: %v", err)
			}
			if offer != tt.wantOffer {
				t.Fatalf("offer = %v, want %v", offer, tt.wantOffer)
			}
		})
	}
}

func TestPromptStateIsPerAgent(t *testing.T) {
	pinStateHome(t)
	if err := MarkPrompt(AgentClaudeCode, PromptSkipped); err != nil {
		t.Fatalf("MarkPrompt: %v", err)
	}
	offer, err := ShouldOfferPrompt(AgentCodex)
	if err != nil {
		t.Fatalf("ShouldOfferPrompt: %v", err)
	}
	if !offer {
		t.Fatal("dismissing one agent must not answer another agent's offer")
	}
}

func TestPromptStateRejectsAnUnknownAgent(t *testing.T) {
	pinStateHome(t)
	if _, err := ShouldOfferPrompt(AgentID("aider")); err == nil {
		t.Fatal("ShouldOfferPrompt(aider) returned no error for an unregistered agent")
	}
	if err := MarkPrompt(AgentID("aider"), PromptSkipped); err == nil {
		t.Fatal("MarkPrompt(aider) returned no error for an unregistered agent")
	}
}

// TestPromptStateUsesApplicationStateRoot pins the root selection. Writing to
// the preferred root when only the legacy one exists would create it as a side
// effect, which moves the whole binary's state root and orphans the existing
// fragment stores, hook offsets, and update stamps.
func TestPromptStateUsesApplicationStateRoot(t *testing.T) {
	state := pinStateHome(t)
	legacy := filepath.Join(state, "sigil")
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatalf("mkdir legacy: %v", err)
	}

	if err := MarkPrompt(AgentClaudeCode, PromptSkipped); err != nil {
		t.Fatalf("MarkPrompt: %v", err)
	}
	path := promptStatePath(AgentClaudeCode)
	if !strings.HasPrefix(path, legacy+string(filepath.Separator)) {
		t.Fatalf("prompt state path %q is not under the legacy state root %q", path, legacy)
	}
	if _, err := os.Stat(filepath.Join(state, "agento11y")); !os.IsNotExist(err) {
		t.Fatalf("marking the prompt created the preferred state root (stat err %v)", err)
	}

	// The file is the record, and it holds no source paths, titles, or content.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read prompt state: %v", err)
	}
	for _, forbidden := range []string{"/Users", ".jsonl", "prompt"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("prompt state %q contains %q", data, forbidden)
		}
	}
}
