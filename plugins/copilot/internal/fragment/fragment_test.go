package fragment

import (
	"io"
	"log"
	"os"
	"testing"
	"time"
)

func TestStartNextTurnAndClear(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	logger := log.New(io.Discard, "", 0)
	turnID, session, err := StartNextTurn("sess", logger, "2026-05-18T12:00:00Z")
	if err != nil {
		t.Fatalf("StartNextTurn: %v", err)
	}
	if turnID != "turn-000001" {
		t.Fatalf("turnID = %q, want turn-000001", turnID)
	}
	if session.ActiveTurnID != turnID {
		t.Fatalf("ActiveTurnID = %q, want %q", session.ActiveTurnID, turnID)
	}
	if err := ClearActiveTurn("sess", turnID, logger); err != nil {
		t.Fatalf("ClearActiveTurn: %v", err)
	}
	got := LoadSessionTolerant("sess", logger)
	if got == nil || got.ActiveTurnID != "" {
		t.Fatalf("expected cleared active turn, got %+v", got)
	}
}

func TestCleanupStaleExceptSkipsActiveFragment(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	logger := log.New(io.Discard, "", 0)
	if err := Update("sess", "turn-000001", logger, func(f *Fragment) bool {
		f.Prompt = "keep"
		return true
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(FragmentFilePath("sess", "turn-000001"), old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	CleanupStaleExcept(DefaultStaleAge, time.Now(), logger, "sess", "turn-000001")
	if got := LoadTolerant("sess", "turn-000001", logger); got == nil {
		t.Fatal("expected fragment to survive cleanup")
	}
}
