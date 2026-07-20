package fragment

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestUpdateSaveLoadDelete(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	logger := log.New(&bytes.Buffer{}, "", 0)
	if err := Update("sess", "turn", logger, func(f *Fragment) bool {
		f.Model = "gpt-5.5"
		return true
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got := LoadTolerant("sess", "turn", logger)
	if got == nil {
		t.Fatal("expected fragment")
	}
	if got.Model != "gpt-5.5" {
		t.Fatalf("Model = %q", got.Model)
	}
	if err := Delete("sess", "turn"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := LoadTolerant("sess", "turn", logger); got != nil {
		t.Fatalf("expected deleted fragment, got %+v", got)
	}
}

func TestUpdateSessionSaveLoad(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	logger := log.New(&bytes.Buffer{}, "", 0)
	if err := UpdateSession("sess", logger, func(s *Session) bool {
		s.Model = "gpt-5.5"
		TouchSession(s, "2026-05-11T10:00:00Z")
		return true
	}); err != nil {
		t.Fatalf("UpdateSession: %v", err)
	}
	got := LoadSessionTolerant("sess", logger)
	if got == nil {
		t.Fatal("expected session")
	}
	if got.Model != "gpt-5.5" {
		t.Fatalf("Model = %q", got.Model)
	}
	if got.StartedAt == "" || got.LastEventAt == "" {
		t.Fatalf("timestamps not populated: %+v", got)
	}
}

func TestUpdateSubagentLinkSaveLoadDelete(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	logger := log.New(&bytes.Buffer{}, "", 0)
	if err := UpdateSubagentLink("child", logger, func(link *SubagentLink) bool {
		link.ParentSessionID = "parent"
		link.AgentRole = "reviewer"
		return true
	}); err != nil {
		t.Fatalf("UpdateSubagentLink: %v", err)
	}
	got := LoadSubagentLinkTolerant("child", logger)
	if got == nil {
		t.Fatal("expected subagent link")
	}
	if got.ParentSessionID != "parent" || got.AgentRole != "reviewer" {
		t.Fatalf("unexpected link: %+v", got)
	}
	if err := DeleteSubagentLink("child"); err != nil {
		t.Fatalf("DeleteSubagentLink: %v", err)
	}
	if got := LoadSubagentLinkTolerant("child", logger); got != nil {
		t.Fatalf("expected deleted link, got %+v", got)
	}
}

func TestConcurrentUpdatesPreserveToolRecords(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	logger := log.New(&bytes.Buffer{}, "", 0)
	const workers = 40

	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := Update("sess", "turn", logger, func(f *Fragment) bool {
				f.Tools = append(f.Tools, ToolRecord{ToolName: "Bash", ToolUseID: string(rune('a' + i))})
				return true
			}); err != nil {
				t.Errorf("Update(%d): %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	got := LoadTolerant("sess", "turn", logger)
	if got == nil {
		t.Fatal("expected fragment")
	}
	if len(got.Tools) != workers {
		t.Fatalf("tool records = %d, want %d", len(got.Tools), workers)
	}
}

func TestCleanupStaleRemovesOldFragmentsSessionsAndSubagentLinks(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	logger := log.New(&bytes.Buffer{}, "", 0)
	if err := Update("sess", "turn", logger, func(f *Fragment) bool {
		f.Model = "gpt-5.5"
		return true
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := UpdateSession("sess", logger, func(s *Session) bool {
		s.Model = "gpt-5.5"
		return true
	}); err != nil {
		t.Fatalf("UpdateSession: %v", err)
	}
	if err := UpdateSubagentLink("sess", logger, func(link *SubagentLink) bool {
		link.ParentSessionID = "parent"
		return true
	}); err != nil {
		t.Fatalf("UpdateSubagentLink: %v", err)
	}
	old := time.Now().Add(-48 * time.Hour)
	for _, path := range []string{FragmentFilePath("sess", "turn"), SessionFilePath("sess"), SubagentLinkFilePath("sess")} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatalf("chtimes %s: %v", path, err)
		}
	}

	CleanupStale(24*time.Hour, time.Now(), logger)

	for _, path := range []string{FragmentFilePath("sess", "turn"), SessionFilePath("sess"), SubagentLinkFilePath("sess")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("expected stale file removed: %s err=%v", path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(StateRoot(), "logs")); err == nil {
		t.Fatal("cleanup should not create or remove log directory")
	}
}
