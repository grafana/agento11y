package meta

import (
	"path/filepath"
	"testing"
)

func TestLoad(t *testing.T) {
	// The fixture is meta.json from a real ~/.vibe/logs/session run.
	// Asserting concrete numbers locks in the field names against a
	// schema we have eyes on.
	tp := filepath.Join("..", "testdata", "messages.jsonl")
	m, err := Load(tp)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if m.Config.ActiveModel != "mistral-medium-3.5" {
		t.Errorf("ActiveModel = %q, want mistral-medium-3.5", m.Config.ActiveModel)
	}
	provider, api := m.ActiveModelRef()
	if provider != "mistral" {
		t.Errorf("provider = %q, want mistral", provider)
	}
	if api != "mistral-vibe-cli-latest" {
		t.Errorf("api id = %q, want mistral-vibe-cli-latest", api)
	}

	if m.Stats.SessionPromptTokens == 0 {
		t.Errorf("SessionPromptTokens = 0, want > 0")
	}
	if m.Stats.SessionCompletionTokens == 0 {
		t.Errorf("SessionCompletionTokens = 0, want > 0")
	}
	if m.Stats.Steps == 0 {
		t.Errorf("Steps = 0, want > 0")
	}

	if len(m.ToolsAvailable) == 0 {
		t.Errorf("ToolsAvailable is empty")
	}
	if m.SystemPrompt.Content == "" {
		t.Errorf("SystemPrompt.Content is empty")
	}
}

func TestActiveModelRef_FallbackProvider(t *testing.T) {
	m := Meta{Config: Config{ActiveModel: "weird-model"}}
	provider, api := m.ActiveModelRef()
	if provider != "mistral" {
		t.Errorf("provider = %q, want fallback mistral", provider)
	}
	if api != "weird-model" {
		t.Errorf("api = %q, want fallback to alias", api)
	}
}

func TestPath(t *testing.T) {
	got := Path("/some/dir/session/messages.jsonl")
	want := filepath.Clean("/some/dir/session/meta.json")
	if got != want {
		t.Errorf("Path = %q, want %q", got, want)
	}
}
