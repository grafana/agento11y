package fragment

import (
	"encoding/json"
	"io"
	"log"
	"os"
	"reflect"
	"testing"
	"time"
)

func TestToolRecordCompatibility(t *testing.T) {
	for _, tt := range []struct {
		name   string
		record ToolRecord
	}{
		{"legacy", ToolRecord{ToolName: "skill", ToolUseID: "call-1"}},
		{"recognized", ToolRecord{ToolName: "skill", ToolUseID: "call-1", SkillName: "org:review"}},
		{"ambiguous", ToolRecord{ToolName: "skill", ToolUseID: "call-1", SkillAttributionAmbiguous: true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			f := &Fragment{SessionID: "sess", TurnID: "turn", Tools: []ToolRecord{tt.record}}
			if err := Save(f); err != nil {
				t.Fatal(err)
			}
			got := LoadTolerant("sess", "turn", log.New(io.Discard, "", 0))
			if !reflect.DeepEqual(f, got) {
				t.Fatalf("round trip = %+v, want %+v", got, f)
			}
			data, err := json.Marshal(tt.record)
			if err != nil {
				t.Fatal(err)
			}
			if tt.name == "legacy" && string(data) != `{"toolName":"skill","toolUseId":"call-1"}` {
				t.Fatalf("legacy encoding = %s", data)
			}
		})
	}
}

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
