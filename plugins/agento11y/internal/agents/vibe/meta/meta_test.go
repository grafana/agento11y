package meta

import (
	"os"
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
	if m.Stats.SessionCost == nil {
		t.Error("SessionCost is nil, want 0.05")
	} else if *m.Stats.SessionCost != 0.05 {
		t.Errorf("SessionCost = %v, want 0.05", *m.Stats.SessionCost)
	}

	if len(m.ToolsAvailable) == 0 {
		t.Errorf("ToolsAvailable is empty")
	}
	if m.SystemPrompt.Content == "" {
		t.Errorf("SystemPrompt.Content is empty")
	}
}

func TestLoad_ModelsShapes(t *testing.T) {
	// Vibe writes config.models as an alias-keyed object from 2.21.0 on and
	// as an array before that. Both have to resolve the same model.
	tests := []struct {
		name    string
		models  string
		wantAPI string
	}{
		{
			name:    "object keyed by alias",
			models:  `{"mistral-medium-3.5": {"name": "mistral-vibe-cli-latest", "provider": "mistral", "alias": "mistral-medium-3.5"}}`,
			wantAPI: "mistral-vibe-cli-latest",
		},
		{
			name:    "array of model tables",
			models:  `[{"name": "mistral-vibe-cli-latest", "provider": "mistral", "alias": "mistral-medium-3.5"}]`,
			wantAPI: "mistral-vibe-cli-latest",
		},
		{
			name:    "object value omitting its inner alias",
			models:  `{"mistral-medium-3.5": {"name": "mistral-vibe-cli-latest", "provider": "mistral"}}`,
			wantAPI: "mistral-vibe-cli-latest",
		},
		{
			name:    "object value whose inner alias disagrees with its key",
			models:  `{"mistral-medium-3.5": {"name": "mistral-vibe-cli-latest", "provider": "mistral", "alias": "something-else"}}`,
			wantAPI: "mistral-vibe-cli-latest",
		},
		{
			// Neither shape: the lookup falls back to the alias rather than
			// failing the load, which would drop the turn export.
			name:    "out-of-contract value",
			models:  `"mistral-vibe-cli-latest"`,
			wantAPI: "mistral-medium-3.5",
		},
		{
			// No table to search, so the alias is the best answer.
			name:    "absent",
			models:  `null`,
			wantAPI: "mistral-medium-3.5",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			body := `{"session_id":"s1","config":{"active_model":"mistral-medium-3.5","models":` + tc.models + `}}`
			if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			m, err := Load(filepath.Join(dir, "messages.jsonl"))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			provider, api := m.ActiveModelRef()
			if provider != "mistral" {
				t.Errorf("provider = %q, want mistral", provider)
			}
			if api != tc.wantAPI {
				t.Errorf("api id = %q, want %q", api, tc.wantAPI)
			}
		})
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
