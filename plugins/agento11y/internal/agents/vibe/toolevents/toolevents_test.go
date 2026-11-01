package toolevents

import (
	"encoding/json"
	"os"
	"reflect"
	"runtime"
	"testing"
	"time"
)

func TestEventPersistence(t *testing.T) {
	for _, tt := range []struct {
		name  string
		event Event
	}{
		{"legacy", Event{ToolCallID: "call-1", ToolName: "skill"}},
		{"recognized", Event{ToolCallID: "call-1", ToolName: "skill", SkillName: "org:review", Status: "success", DurationMs: 1234, CompletedAt: time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)}},
		{"failure", Event{ToolCallID: "call-1", ToolName: "skill", SkillName: "review", Status: "failure", Error: "failed"}},
		{"cancelled", Event{ToolCallID: "call-1", ToolName: "skill", SkillName: "review", Status: "cancelled"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			if err := Save("sess", tt.event); err != nil {
				t.Fatal(err)
			}
			got := Load("sess")
			if len(got) != 1 || !reflect.DeepEqual(got["call-1"], tt.event) {
				t.Fatalf("round trip = %+v, want %+v", got, tt.event)
			}
			info, err := os.Stat(eventPath("sess", "call-1"))
			if err != nil {
				t.Fatal(err)
			}
			if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
				t.Fatalf("event permissions = %v", info.Mode())
			}
			if tt.name == "legacy" {
				data, err := json.Marshal(tt.event)
				if err != nil {
					t.Fatal(err)
				}
				if string(data) != `{"tool_call_id":"call-1","tool_name":"skill"}` {
					t.Fatalf("legacy JSON = %s", data)
				}
			}
			if len(Load("other-session")) != 0 {
				t.Fatal("event crossed sessions")
			}
			Clear("sess")
			if len(Load("sess")) != 0 {
				t.Fatal("events survived cleanup")
			}
		})
	}
}
