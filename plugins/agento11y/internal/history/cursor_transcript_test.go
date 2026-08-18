package history

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestIsCursorParentTranscript(t *testing.T) {
	sid := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	if !isCursorParentTranscript(filepath.Join("/p/agent-transcripts", sid, sid+".jsonl")) {
		t.Fatal("expected parent transcript to match")
	}
	if isCursorParentTranscript(filepath.Join("/p/agent-transcripts", sid, "subagents", sid+".jsonl")) {
		t.Fatal("subagent transcript must not match")
	}
	if isCursorParentTranscript("") {
		t.Fatal("empty path must not match")
	}
}

func writeCursorTranscript(t *testing.T, root, project, sessionID, fixture string) string {
	t.Helper()
	dir := filepath.Join(root, project, "agent-transcripts", sessionID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join("testdata", "cursor", "transcripts", fixture)
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, sessionID+".jsonl")
	if err := os.WriteFile(dst, data, 0o640); err != nil {
		t.Fatal(err)
	}
	return dst
}

func TestCursorTranscriptPreview(t *testing.T) {
	root := t.TempDir()
	sid := "0f2b19b1-3d1f-4c1a-9a1e-2f7c1b9d4e55"
	path := writeCursorTranscript(t, root, "Users-tester-Development-demo", sid, "two-turn.jsonl")

	imp := &cursorImporter{}
	p, ok, err := imp.Preview(context.Background(), path)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if !ok {
		t.Fatal("Preview returned ok=false")
	}
	if p.SessionID != sid {
		t.Fatalf("SessionID = %q, want %q", p.SessionID, sid)
	}
	if p.Title != sid {
		t.Fatalf("Title = %q, want session id", p.Title)
	}
	if p.TurnCount != 2 {
		t.Fatalf("TurnCount = %d, want 2", p.TurnCount)
	}
	if p.SourcePath != path {
		t.Fatalf("SourcePath = %q, want %q", p.SourcePath, path)
	}
	wantStart := time.Date(2026, 5, 13, 14, 30, 0, 0, time.FixedZone("UTC+2", 2*3600))
	if !p.StartedAt.Equal(wantStart) {
		t.Fatalf("StartedAt = %v, want %v", p.StartedAt, wantStart)
	}
	wantLast := time.Date(2026, 5, 13, 14, 35, 0, 0, time.FixedZone("UTC+2", 2*3600))
	if !p.LastActivityAt.Equal(wantLast) {
		t.Fatalf("LastActivityAt = %v, want %v", p.LastActivityAt, wantLast)
	}
}

func TestCursorTranscriptPreviewEmpty(t *testing.T) {
	root := t.TempDir()
	sid := "bbbbbbbb-bbbb-cccc-dddd-eeeeeeeeeeee"
	dir := filepath.Join(root, "proj", "agent-transcripts", sid)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, sid+".jsonl")
	if err := os.WriteFile(path, nil, 0o640); err != nil {
		t.Fatal(err)
	}
	_, ok, err := (&cursorImporter{}).Preview(context.Background(), path)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if ok {
		t.Fatal("empty transcript must not be a usable session")
	}
}

func TestCursorTranscriptTurns(t *testing.T) {
	root := t.TempDir()
	sid := "0f2b19b1-3d1f-4c1a-9a1e-2f7c1b9d4e55"
	path := writeCursorTranscript(t, root, "proj", sid, "two-turn.jsonl")
	imp := &cursorImporter{}
	preview, ok, err := imp.Preview(context.Background(), path)
	if err != nil || !ok {
		t.Fatalf("Preview: ok=%v err=%v", ok, err)
	}

	var gens []HistoricalGeneration
	for g, err := range imp.Turns(context.Background(), preview) {
		if err != nil {
			t.Fatalf("Turns: %v", err)
		}
		gens = append(gens, g)
	}
	if len(gens) != 2 {
		t.Fatalf("got %d generations, want 2", len(gens))
	}
	if gens[0].Gen.ConversationID != sid {
		t.Fatalf("ConversationID = %q, want %q", gens[0].Gen.ConversationID, sid)
	}
	prompt := cursorPromptOf(t, gens[0])
	if !strings.Contains(prompt, "run the tests") {
		t.Fatalf("first prompt = %q", prompt)
	}
	if strings.Contains(prompt, "<timestamp>") {
		t.Fatal("imported prompt must not keep the timestamp wrapper")
	}
	if strings.Contains(prompt, "<user_query>") {
		t.Fatal("imported prompt must unwrap <user_query>")
	}
	if gens[0].Quality.MissingModel != true || gens[0].Quality.ApproxUsage != true {
		t.Fatalf("quality = %+v", gens[0].Quality)
	}
	if gens[0].Quality.ApproxStartedAt {
		t.Fatal("dated turn must not mark ApproxStartedAt")
	}
	if gens[1].Gen.StopReason != "completed" {
		t.Fatalf("second stop reason = %q, want completed", gens[1].Gen.StopReason)
	}
}

func TestCursorTranscriptToolUse(t *testing.T) {
	root := t.TempDir()
	sid := "cccccccc-bbbb-cccc-dddd-eeeeeeeeeeee"
	path := writeCursorTranscript(t, root, "proj", sid, "tool-use.jsonl")
	imp := &cursorImporter{}
	preview, ok, err := imp.Preview(context.Background(), path)
	if err != nil || !ok {
		t.Fatalf("Preview: ok=%v err=%v", ok, err)
	}
	var gens []HistoricalGeneration
	for g, err := range imp.Turns(context.Background(), preview) {
		if err != nil {
			t.Fatalf("Turns: %v", err)
		}
		gens = append(gens, g)
	}
	if len(gens) != 1 {
		t.Fatalf("got %d generations, want 1", len(gens))
	}
	if names := cursorToolNames(gens[0]); !slices.Equal(names, []string{"Shell"}) {
		t.Fatalf("tools = %v, want [Shell]", names)
	}
	found := false
	for _, n := range gens[0].Quality.Notes {
		if n == cursorNoteMissingToolResults {
			found = true
		}
	}
	if !found {
		t.Fatalf("notes = %v, want %q", gens[0].Quality.Notes, cursorNoteMissingToolResults)
	}
}

func TestCursorTranscriptTurnEndedError(t *testing.T) {
	root := t.TempDir()
	sid := "dddddddd-bbbb-cccc-dddd-eeeeeeeeeeee"
	path := writeCursorTranscript(t, root, "proj", sid, "turn-ended-error.jsonl")
	imp := &cursorImporter{}
	preview, ok, err := imp.Preview(context.Background(), path)
	if err != nil || !ok {
		t.Fatalf("Preview: ok=%v err=%v", ok, err)
	}
	var gens []HistoricalGeneration
	for g, err := range imp.Turns(context.Background(), preview) {
		if err != nil {
			t.Fatalf("Turns: %v", err)
		}
		gens = append(gens, g)
	}
	if len(gens) != 1 {
		t.Fatalf("got %d generations, want 1", len(gens))
	}
	if gens[0].Gen.StopReason != "error" {
		t.Fatalf("StopReason = %q, want error", gens[0].Gen.StopReason)
	}
	if gens[0].Gen.CallError != "tool failed" {
		t.Fatalf("CallError = %q, want the turn_ended error", gens[0].Gen.CallError)
	}
}

func TestCursorTranscriptTimesSameMinute(t *testing.T) {
	stamp := time.Date(2026, 5, 13, 12, 30, 0, 0, time.UTC)
	r := &cursorTranscriptReplay{previousEnd: stamp.Add(cursorNominalStep)}
	start, end := r.times(&cursorTranscriptTurn{dated: stamp})
	if !start.Equal(stamp.Add(cursorNominalStep)) {
		t.Fatalf("start = %v, want the previous turn's end", start)
	}
	if !end.After(start) {
		t.Fatalf("end %v must be after start %v so same-minute turns stay ordered", end, start)
	}
}

func TestCursorDiscoverTranscriptsWithoutChats(t *testing.T) {
	root := t.TempDir()
	projects := filepath.Join(root, "projects")
	sid := "0f2b19b1-3d1f-4c1a-9a1e-2f7c1b9d4e55"
	writeCursorTranscript(t, projects, "proj", sid, "two-turn.jsonl")
	// No chats directory at all.
	imp := &cursorImporter{roots: []string{filepath.Join(root, "chats"), projects}}
	d, err := Discover(context.Background(), AgentCursor, imp, DiscoverOptions{})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(d.Sessions) != 1 {
		t.Fatalf("sessions = %d, want 1; warnings=%v", len(d.Sessions), d.Warnings)
	}
	if d.Sessions[0].SessionID != sid {
		t.Fatalf("SessionID = %q", d.Sessions[0].SessionID)
	}
}

func TestCursorTranscriptFilterUsesTimestamp(t *testing.T) {
	root := t.TempDir()
	sid := "0f2b19b1-3d1f-4c1a-9a1e-2f7c1b9d4e55"
	path := writeCursorTranscript(t, root, "proj", sid, "two-turn.jsonl")
	// Make mtime look ancient so a buggy importer that used only mtime would
	// drop the session from a 90-day window.
	ancient := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, ancient, ancient); err != nil {
		t.Fatal(err)
	}
	imp := &cursorImporter{}
	p, ok, err := imp.Preview(context.Background(), path)
	if err != nil || !ok {
		t.Fatalf("Preview: ok=%v err=%v", ok, err)
	}
	f := NewFilter()
	f.Since = time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	selected, skipped := f.SelectSessions([]SessionPreview{p})
	if len(selected) != 1 {
		t.Fatalf("selected=%d skipped=%v; LastActivityAt=%v", len(selected), skipped, p.LastActivityAt)
	}
}

func TestParseCursorTranscriptTimestamp(t *testing.T) {
	tests := []struct {
		name string
		text string
		want time.Time
	}{
		{
			name: "full month name",
			text: "<timestamp>Wednesday, May 13, 2026, 2:30 PM (UTC+2)</timestamp>\n<user_query>\nhi\n</user_query>",
			want: time.Date(2026, 5, 13, 14, 30, 0, 0, time.FixedZone("UTC+2", 2*3600)),
		},
		{
			name: "abbreviated month name as Cursor writes today",
			text: "<timestamp>Tuesday, Jul 21, 2026, 1:04 PM (UTC+2)</timestamp>\n<user_query>\nhi\n</user_query>",
			want: time.Date(2026, 7, 21, 13, 4, 0, 0, time.FixedZone("UTC+2", 2*3600)),
		},
		{
			name: "full July still parses",
			text: "<timestamp>Tuesday, July 21, 2026, 1:04 PM (UTC+2)</timestamp>",
			want: time.Date(2026, 7, 21, 13, 4, 0, 0, time.FixedZone("UTC+2", 2*3600)),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseCursorTranscriptTimestamp(tt.text)
			if !got.Equal(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
	if !parseCursorTranscriptTimestamp("no stamp").IsZero() {
		t.Fatal("expected zero without timestamp wrapper")
	}
}

func TestCursorWorkspaceFromTranscriptPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Build a real workspace path under the fake home.
	ws := filepath.Join(home, "Development", "demo-app")
	if err := os.MkdirAll(ws, 0o750); err != nil {
		t.Fatal(err)
	}
	homeSlug := strings.ReplaceAll(filepath.ToSlash(home), "/", "-")
	homeSlug = strings.TrimPrefix(homeSlug, "-")
	slug := homeSlug + "-Development-demo-app"
	sid := "0f2b19b1-3d1f-4c1a-9a1e-2f7c1b9d4e55"
	path := filepath.Join(home, ".cursor", "projects", slug, "agent-transcripts", sid, sid+".jsonl")
	got := cursorWorkspaceFromTranscriptPath(path)
	if got != ws {
		t.Fatalf("workspace = %q, want %q", got, ws)
	}
	if cursorWorkspaceFromTranscriptPath("/nope/file.jsonl") != "" {
		t.Fatal("expected empty for unrelated path")
	}
}

func TestCursorMatchSlugPathHyphenatedDir(t *testing.T) {
	root := t.TempDir()
	ws := filepath.Join(root, "foo-bar", "baz")
	if err := os.MkdirAll(ws, 0o750); err != nil {
		t.Fatal(err)
	}
	got := cursorMatchSlugPath(root, "foo-bar-baz")
	if got != ws {
		t.Fatalf("got %q, want %q", got, ws)
	}
}
