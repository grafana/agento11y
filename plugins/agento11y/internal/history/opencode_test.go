package history

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/opencode/sessiondb/sessiondbtest"
)

func TestOpenCodeRootsAndMatch(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("HOME", t.TempDir())
	dataDir := filepath.Join(dataHome, "opencode")
	if err := os.MkdirAll(filepath.Join(dataDir, "custom"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"opencode.db", "opencode-beta.db", "opencode-local.db", "notes.db"} {
		if err := os.WriteFile(filepath.Join(dataDir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("OPENCODE_DB", filepath.Join("custom", "history.sqlite"))

	imp := &opencodeImporter{}
	got := imp.Roots()
	want := []string{
		filepath.Join(dataDir, "custom", "history.sqlite"),
		filepath.Join(dataDir, "opencode-beta.db"),
		filepath.Join(dataDir, "opencode-local.db"),
		filepath.Join(dataDir, "opencode.db"),
	}
	sort.Strings(want)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("Roots = %v, want %v", got, want)
	}

	tests := []struct {
		path string
		want bool
	}{
		{path: filepath.Join(dataDir, "opencode.db"), want: true},
		{path: filepath.Join(dataDir, "opencode-nightly.db"), want: true},
		{path: filepath.Join(dataDir, "opencode.db-wal"), want: false},
		{path: filepath.Join(dataDir, "opencode.db-shm"), want: false},
		{path: filepath.Join(dataDir, "notes.db"), want: false},
		{path: filepath.Join(dataDir, "custom", "history.sqlite"), want: true},
	}
	for _, tc := range tests {
		if got := imp.Match(tc.path); got != tc.want {
			t.Errorf("Match(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestOpenCodeRootsWithoutDataDir(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	absolutePath := filepath.Join(t.TempDir(), "history.sqlite")

	tests := []struct {
		name     string
		override string
		want     []string
	}{
		{name: "absolute override", override: absolutePath, want: []string{absolutePath}},
		{name: "relative override", override: "history.sqlite"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("OPENCODE_DB", tt.override)
			if got := (&opencodeImporter{}).Roots(); !slices.Equal(got, tt.want) {
				t.Fatalf("Roots() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOpenCodePreviewUsesTheContainerPath(t *testing.T) {
	db := sessiondbtest.New(t)
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	db.AddSession("normal", "", "Useful title ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij", "/work/normal", now.Add(-2*time.Hour).UnixMilli(), now.Add(-time.Hour).UnixMilli())
	db.AddSession("placeholder", "", "New session - 2026-08-07T08:54:44.300Z", "/work/placeholder", now.Add(-3*time.Hour).UnixMilli(), now.Add(-2*time.Hour).UnixMilli())
	db.AddSession("abandoned", "", "Abandoned", "/work/abandoned", now.Add(-4*time.Hour).UnixMilli(), now.Add(-3*time.Hour).UnixMilli())
	db.AddSession("empty", "", "Empty", "/work/empty", now.Add(-5*time.Hour).UnixMilli(), now.Add(-4*time.Hour).UnixMilli())
	db.AddMessage("normal-user", "normal", 1, 2, `{"role":"user","time":{"created":1}}`)
	db.AddMessage("normal-assistant", "normal", 2, 3, `{"role":"assistant","time":{"created":2,"completed":3},"modelID":"model","providerID":"provider"}`)
	db.AddPart("normal-text", "normal-assistant", "normal", 1, 2, `{"type":"text","text":"preview-secret"}`)
	db.AddMessage("placeholder-user", "placeholder", 1, 2, `{"role":"user","time":{"created":1}}`)
	db.AddMessage("placeholder-assistant", "placeholder", 2, 3, `{"role":"assistant","time":{"created":2,"completed":3},"modelID":"model","providerID":"provider"}`)
	db.AddMessage("abandoned-user", "abandoned", 1, 2, `{"role":"user","time":{"created":1}}`)

	imp := &opencodeImporter{roots: []string{db.Path}}
	if _, ok, err := imp.Preview(context.Background(), db.Path); err != nil || ok {
		t.Fatalf("Preview = (_, %v, %v), want unused", ok, err)
	}
	got, err := Discover(context.Background(), AgentOpenCode, imp, DiscoverOptions{
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got.Sessions) != 2 {
		t.Fatalf("Discover returned %d sessions, want 2", len(got.Sessions))
	}
	byID := map[string]SessionPreview{}
	for _, sess := range got.Sessions {
		byID[sess.SessionID] = sess
		rendered := fmt.Sprintf("%+v", sess)
		if strings.Contains(rendered, "preview-secret") || strings.Contains(rendered, "Useful title") {
			t.Fatal("preview contains session content")
		}
	}
	normal := byID["normal"]
	if normal.Title != "normal" || normal.Workspace != "/work/normal" || normal.TurnCount != 1 {
		t.Fatalf("normal preview = %+v", normal)
	}
	if normal.SourcePath != db.Path || normal.StartedAt.UnixMilli() != now.Add(-2*time.Hour).UnixMilli() {
		t.Fatalf("normal preview identity = %+v", normal)
	}
	if byID["placeholder"].Title != "placeholder" {
		t.Fatalf("placeholder title = %q", byID["placeholder"].Title)
	}
	if turns := collectTurns(t, imp, normal); len(turns) != 1 || turns[0].Gen.ConversationTitle != "Useful title ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij" {
		t.Fatalf("selected normal title = %+v", turns)
	}
	if turns := collectTurns(t, imp, byID["placeholder"]); len(turns) != 1 || turns[0].Gen.ConversationTitle != "" {
		t.Fatalf("selected placeholder title = %+v", turns)
	}
	exporter := &recordingExporter{}
	if _, err := RunImport(context.Background(), ImportOptions{
		Agent: AgentOpenCode, Importer: imp, Sessions: []SessionPreview{normal},
		Exporter: exporter, Ledger: testLedger(t),
	}); err != nil {
		t.Fatalf("RunImport: %v", err)
	}
	if len(exporter.got) != 1 || exporter.got[0].Gen.ConversationTitle != "Useful title [REDACTED:github-pat]" {
		t.Fatalf("exported normal title = %+v", exporter.got)
	}
	if _, ok := byID["abandoned"]; ok {
		t.Fatal("session with no importable turns was previewed")
	}
	if _, ok := byID["empty"]; ok {
		t.Fatal("session with no messages was previewed")
	}
}

func TestOpenCodeRunImportReportsTitleTruncation(t *testing.T) {
	db := sessiondbtest.New(t)
	title := strings.Repeat("x", DefaultMaxFieldBytes+1)
	db.AddSession("session", "", title, "/work", 1, 2)
	db.AddMessage("assistant", "session", 1, 2, `{"role":"assistant","time":{"created":1,"completed":2}}`)
	imp := &opencodeImporter{}
	previews, err := imp.Previews(context.Background(), db.Path)
	if err != nil || len(previews) != 1 {
		t.Fatalf("Previews = (%+v, %v)", previews, err)
	}

	exporter := &recordingExporter{}
	_, err = RunImport(context.Background(), ImportOptions{
		Agent: AgentOpenCode, Importer: imp, Sessions: previews,
		Exporter: exporter, Ledger: testLedger(t),
	})
	if err != nil {
		t.Fatalf("RunImport: %v", err)
	}
	if len(exporter.got) != 1 {
		t.Fatalf("exported = %+v", exporter.got)
	}
	got := exporter.got[0]
	if len(got.Gen.ConversationTitle) != DefaultMaxFieldBytes || !got.Quality.Truncated {
		t.Fatalf("exported title bytes=%d quality=%+v", len(got.Gen.ConversationTitle), got.Quality)
	}
}

func TestOpenCodePreviewWarnsOnMalformedMessage(t *testing.T) {
	db := sessiondbtest.New(t)
	db.AddSession("session", "", "Session", "/work", 1, 2)
	db.AddMessage("bad-message", "session", 1, 2, `{`)

	got, err := Discover(context.Background(), AgentOpenCode, &opencodeImporter{roots: []string{db.Path}}, DiscoverOptions{})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got.Sessions) != 0 || len(got.Warnings) != 1 {
		t.Fatalf("Discover = %+v, want one warning and no sessions", got)
	}
	if !strings.Contains(got.Warnings[0], "bad-message") || !strings.Contains(got.Warnings[0], db.Path) {
		t.Fatalf("warning = %q", got.Warnings[0])
	}
}

func TestOpenCodeCollisionsKeepChildrenWithTheirRoot(t *testing.T) {
	tests := []struct {
		name      string
		roots     [2]string
		rootTurns bool
	}{
		{name: "same root and child ids", roots: [2]string{"root", "root"}, rootTurns: true},
		{name: "same child id under different roots", roots: [2]string{"root-a", "root-b"}, rootTurns: true},
		{name: "same omitted root id", roots: [2]string{"root", "root"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			imp := &opencodeImporter{}
			rootsByPath := map[string]string{}
			var sessions []SessionPreview
			for i, rootID := range tt.roots {
				db := sessiondbtest.New(t)
				db.AddSession(rootID, "", "Root", "/work", 1, 10)
				db.AddSession("child", rootID, "Child", "/work", 2, 11)
				if tt.rootTurns {
					db.AddMessage("root-assistant", rootID, 3, 4, `{"role":"assistant","time":{"created":3,"completed":4}}`)
				}
				db.AddMessage("child-assistant", "child", 5, 6, `{"role":"assistant","time":{"created":5,"completed":6}}`)
				previews, err := imp.Previews(context.Background(), db.Path)
				if err != nil {
					t.Fatalf("database %d previews: %v", i, err)
				}
				rootsByPath[db.Path] = rootID
				sessions = append(sessions, previews...)
			}

			exporter := &recordingExporter{}
			_, err := RunImport(context.Background(), ImportOptions{
				Agent:      AgentOpenCode,
				Importer:   imp,
				Sessions:   sessions,
				Collisions: DetectCollisions(sessions),
				Exporter:   exporter,
				Ledger:     testLedger(t),
			})
			if err != nil {
				t.Fatalf("RunImport: %v", err)
			}
			conversations := map[string]map[string]string{}
			for _, gen := range exporter.got {
				bySession := conversations[gen.Source.SourcePath]
				if bySession == nil {
					bySession = map[string]string{}
					conversations[gen.Source.SourcePath] = bySession
				}
				bySession[gen.Source.SessionID] = gen.Gen.ConversationID
			}
			if len(conversations) != 2 {
				t.Fatalf("conversations by source = %v", conversations)
			}
			var rootIDs []string
			for path, bySession := range conversations {
				rootID := rootsByPath[path]
				childConversation := bySession["child"]
				if childConversation == "" {
					t.Fatalf("source %s conversations = %v", path, bySession)
				}
				if rootConversation := bySession[rootID]; rootConversation != "" && rootConversation != childConversation {
					t.Fatalf("source %s conversations = %v", path, bySession)
				}
				rootIDs = append(rootIDs, childConversation)
			}
			if rootIDs[0] == rootIDs[1] {
				t.Fatalf("databases shared conversation %q", rootIDs[0])
			}
		})
	}
}

func TestOpenCodeTurnsReportsMissingSelectedSession(t *testing.T) {
	db := sessiondbtest.New(t)
	sess := SessionPreview{Agent: AgentOpenCode, SessionID: "deleted", SourcePath: db.Path}
	for _, err := range (&opencodeImporter{}).Turns(context.Background(), sess) {
		if err == nil || !strings.Contains(err.Error(), "no longer exists") {
			t.Fatalf("Turns error = %v", err)
		}
		return
	}
	t.Fatal("Turns returned no error")
}

func TestOpenCodeTurnsYieldOnlyTerminalAssistantMessages(t *testing.T) {
	db := sessiondbtest.New(t)
	db.AddSession("session", "", "Session", "/work", 1, 10)
	db.AddMessage("user", "session", 1, 2, `{"role":"user","time":{"created":1}}`)
	db.AddMessage("incomplete", "session", 2, 3, `{"role":"assistant","time":{"created":2},"modelID":"model","providerID":"provider"}`)
	db.AddPart("incomplete-text", "incomplete", "session", 1, 2, `{"type":"text","text":"unfinished"}`)
	db.AddMessage("complete", "session", 3, 4, `{"role":"assistant","time":{"created":3,"completed":4},"modelID":"model","providerID":"provider"}`)
	db.AddMessage("error", "session", 5, 6, `{"role":"assistant","time":{"created":5},"modelID":"model","providerID":"provider","error":{"name":"UnknownError","data":{}}}`)

	imp := &opencodeImporter{}
	sess := SessionPreview{Agent: AgentOpenCode, SessionID: "session", SourcePath: db.Path}
	var got []HistoricalGeneration
	for gen, err := range imp.Turns(context.Background(), sess) {
		if err != nil {
			t.Fatalf("Turns: %v", err)
		}
		got = append(got, gen)
	}
	if len(got) != 2 {
		t.Fatalf("Turns returned %d generations, want 2", len(got))
	}
	if got[0].Source.TurnID != "complete" || got[1].Source.TurnID != "error" {
		t.Fatalf("turn IDs = [%s %s]", got[0].Source.TurnID, got[1].Source.TurnID)
	}
	if got[0].Source.GenerationID() == got[1].Source.GenerationID() {
		t.Fatal("two turns received the same generation ID")
	}

	var again []string
	for gen, err := range imp.Turns(context.Background(), sess) {
		if err != nil {
			t.Fatalf("second Turns: %v", err)
		}
		again = append(again, gen.Source.GenerationID())
	}
	if fmt.Sprint(again) != fmt.Sprint([]string{got[0].Source.GenerationID(), got[1].Source.GenerationID()}) {
		t.Fatalf("second IDs = %v", again)
	}

	if _, err := db.DB.Exec(`update message set data = ? where id = 'incomplete'`,
		`{"role":"assistant","time":{"created":2,"completed":6},"modelID":"model","providerID":"provider"}`); err != nil {
		t.Fatalf("complete earlier message: %v", err)
	}
	for gen, err := range imp.Turns(context.Background(), sess) {
		if err != nil {
			t.Fatalf("Turns after completion: %v", err)
		}
		if gen.Source.TurnID == "complete" && gen.Source.GenerationID() != got[0].Source.GenerationID() {
			t.Fatalf("later completion changed complete generation ID from %s to %s", got[0].Source.GenerationID(), gen.Source.GenerationID())
		}
	}

	if _, err := db.DB.Exec(`delete from message where id = 'incomplete'`); err != nil {
		t.Fatalf("delete earlier message: %v", err)
	}
	for gen, err := range imp.Turns(context.Background(), sess) {
		if err != nil {
			t.Fatalf("Turns after deletion: %v", err)
		}
		if gen.Source.TurnID == "complete" && gen.Source.GenerationID() != got[0].Source.GenerationID() {
			t.Fatalf("earlier deletion changed complete generation ID from %s to %s", got[0].Source.GenerationID(), gen.Source.GenerationID())
		}
	}
}
