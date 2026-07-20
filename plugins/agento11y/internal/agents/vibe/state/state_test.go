package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveLoad_Roundtrip(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	want := Session{
		Offset:                  4242,
		SessionPromptTokens:     100,
		SessionCompletionTokens: 25,
		Title:                   "list files",
	}
	if err := Save("session-A", want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, found := Load("session-A")
	if !found {
		t.Error("found = false, want true for a saved session")
	}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestLoad_MissingReturnsZero(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	got, found := Load("nope")
	if found {
		t.Error("found = true, want false for a missing session")
	}
	if (got != Session{}) {
		t.Errorf("want zero Session, got %+v", got)
	}
}

func TestLoad_CorruptResets(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)

	// Place a malformed state file at the expected path. Load should
	// log to stderr and hand back a zero Session.
	p := filepath.Join(dir, "sigil", "vibe", SanitizeSessionID("sess-X")+".state")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, found := Load("sess-X")
	if found {
		t.Error("found = true, want false for a corrupt state file")
	}
	if (got != Session{}) {
		t.Errorf("want zero Session on corrupt, got %+v", got)
	}
}
