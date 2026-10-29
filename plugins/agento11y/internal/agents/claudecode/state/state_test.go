package state

import (
	"os"
	"reflect"
	"runtime"
	"testing"
	"time"
)

func TestToolStateRoundTrip(t *testing.T) {
	for _, ready := range []bool{false, true} {
		t.Run(map[bool]string{false: "unresolved parent", true: "ready parent"}[ready], func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			at := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
			want := ToolState{Version: 1, CompletedChildren: map[string]bool{"child": true}, Offsets: map[string]int64{"/parent": 0, "/child": 120}, LegacyOffsets: map[string]int64{"later-child": 80}, LegacySpawnOffsets: map[string]int64{"/parent": 100, "/child": 120}, Pending: map[string]*PendingTool{"scoped-call": {Emitted: ready, SessionID: "raw-session", Scope: "/parent#sidechain:child", Path: "/parent", ID: "call", Name: "Skill", SkillName: "review", Call: &ToolSource{Start: 0, End: 80, At: at}, Result: &ToolResult{ToolSource: ToolSource{Start: 80, End: 120, At: at.Add(time.Second)}, IsError: true}, Parent: ToolParent{ID: "generation", ConversationID: "raw-session", AgentName: "claude-code", Model: "claude", Provider: "anthropic", Ready: ready, TraceID: "00112233445566778899aabbccddeeff", SpanID: "0011223344556677", Flags: 1, TraceState: "vendor=value"}}}, Spawns: map[string]*ToolSpawn{"spawn": {Path: "/parent", Background: true, AgentName: "explorer", ResultEnd: 120}}}
			if err := Save("session", Session{Tools: want}); err != nil {
				t.Fatal(err)
			}
			got := Load("session").Tools
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("tool metadata changed: %+v", got)
			}
			info, err := os.Stat(path("session"))
			if err != nil {
				t.Fatal(err)
			}
			if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
				t.Fatalf("state permissions=%o", info.Mode().Perm())
			}
		})
	}
}

func TestLoad_MissingFile(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	s := Load("nonexistent-session")
	if s.Offset != 0 {
		t.Errorf("Offset = %d, want 0", s.Offset)
	}
	if s.Title != "" {
		t.Errorf("Title = %q, want empty", s.Title)
	}
}

func TestSaveLoad_Roundtrip(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	want := Session{Offset: 12345, Title: "Fix the authentication bug", Model: "claude-sonnet-4-6"}
	if err := Save("test-session", want); err != nil {
		t.Fatal(err)
	}

	got := Load("test-session")
	if got.Offset != want.Offset {
		t.Errorf("Offset = %d, want %d", got.Offset, want.Offset)
	}
	if got.Title != want.Title {
		t.Errorf("Title = %q, want %q", got.Title, want.Title)
	}
	if got.Model != want.Model {
		t.Errorf("Model = %q, want %q", got.Model, want.Model)
	}
}

func TestSave_UpdatesExisting(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	if err := Save("s1", Session{Offset: 100, Title: "first"}); err != nil {
		t.Fatal(err)
	}
	if err := Save("s1", Session{Offset: 200, Title: "second"}); err != nil {
		t.Fatal(err)
	}

	got := Load("s1")
	if got.Offset != 200 {
		t.Errorf("Offset = %d, want 200", got.Offset)
	}
	if got.Title != "second" {
		t.Errorf("Title = %q, want %q", got.Title, "second")
	}
}

func TestSave_TitleWithNewlines(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	want := Session{Offset: 100, Title: "title with\nnewlines\nin it"}
	if err := Save("s1", want); err != nil {
		t.Fatal(err)
	}

	got := Load("s1")
	if got.Title != want.Title {
		t.Errorf("Title = %q, want %q", got.Title, want.Title)
	}
}

func TestLoad_CorruptedFile(t *testing.T) {
	d := t.TempDir()
	t.Setenv("XDG_STATE_HOME", d)

	// Write a corrupted state file (not valid JSON)
	sd := dir()
	if err := os.MkdirAll(sd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path("bad"), []byte("not-json"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := Load("bad")
	if s.Offset != 0 {
		t.Errorf("Offset = %d, want 0 for corrupted file", s.Offset)
	}
}

func TestSanitizeSessionID(t *testing.T) {
	for _, raw := range []string{
		"normal-session-id",
		"../../etc/cron.d/evil",
		"path/traversal",
		"back\\slash",
	} {
		got := SanitizeSessionID(raw)
		if got == "" {
			t.Errorf("SanitizeSessionID(%q) returned empty", raw)
		}
		for _, ch := range got {
			ok := (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') ||
				(ch >= '0' && ch <= '9') || ch == '.' || ch == '_' || ch == '-'
			if !ok {
				t.Errorf("SanitizeSessionID(%q) = %q contains unsafe rune %q", raw, got, ch)
			}
		}
	}
}
