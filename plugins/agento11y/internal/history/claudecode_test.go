package history

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/mapper"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/state"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/transcript"
)

// claudeLine builds one transcript JSONL line.
func claudeLine(fields map[string]any) string {
	data, err := json.Marshal(fields)
	if err != nil {
		panic(err)
	}
	return string(data) + "\n"
}

func claudeUserLine(sessionID, cwd, ts, text string) string {
	return claudeLine(map[string]any{
		"type":      "user",
		"sessionId": sessionID,
		"cwd":       cwd,
		"timestamp": ts,
		"message":   map[string]any{"role": "user", "content": text},
	})
}

func claudeAssistantLine(sessionID, cwd, ts, requestID, text string, blocks []map[string]any) string {
	content := []map[string]any{{"type": "text", "text": text}}
	content = append(content, blocks...)
	return claudeLine(map[string]any{
		"type":      "assistant",
		"sessionId": sessionID,
		"cwd":       cwd,
		"timestamp": ts,
		"requestId": requestID,
		"version":   "2.1.0",
		"message": map[string]any{
			"model":       "claude-sonnet-4-20250514",
			"content":     content,
			"stop_reason": "end_turn",
			"usage":       map[string]any{"input_tokens": 100, "output_tokens": 25},
		},
	})
}

func claudeToolResultLine(sessionID, toolUseID, content string) string {
	return claudeLine(map[string]any{
		"type":      "user",
		"sessionId": sessionID,
		"timestamp": "2026-01-10T12:00:30Z",
		"message": map[string]any{
			"role": "user",
			"content": []map[string]any{
				{"type": "tool_result", "tool_use_id": toolUseID, "content": content},
			},
		},
	})
}

// writeClaudeSession writes a two-turn transcript and returns its path.
func writeClaudeSession(t *testing.T, root, project, sessionID string) string {
	t.Helper()
	path := filepath.Join(root, project, sessionID+".jsonl")
	body := claudeUserLine(sessionID, "/work/repo", "2026-01-10T12:00:00Z", "explain the build") +
		claudeAssistantLine(sessionID, "/work/repo", "2026-01-10T12:00:10Z", "req-1", "It compiles.", nil) +
		claudeUserLine(sessionID, "/work/repo", "2026-01-10T12:01:00Z", "and the tests?") +
		claudeAssistantLine(sessionID, "/work/repo", "2026-01-10T12:01:10Z", "req-2", "They pass.", nil)
	return writeFile(t, path, body)
}

func claudeImporterAt(root string) *claudeImporter {
	return &claudeImporter{roots: []string{root}}
}

func collectTurns(t *testing.T, imp Importer, sess SessionPreview) []HistoricalGeneration {
	t.Helper()
	var out []HistoricalGeneration
	for gen, err := range imp.Turns(context.Background(), sess) {
		if err != nil {
			t.Fatalf("Turns: %v", err)
		}
		out = append(out, gen)
	}
	return out
}

func TestClaudeRootsResolveFromConfigDir(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/custom/claude")
	imp := &claudeImporter{}
	want := []string{filepath.Join("/custom/claude", "projects")}
	if got := imp.Roots(); !slices.Equal(got, want) {
		t.Fatalf("Roots() = %v, want %v", got, want)
	}

	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("HOME", "/home/tester")
	if got := imp.Roots(); !slices.Equal(got, []string{filepath.Join("/home/tester", ".claude", "projects")}) {
		t.Fatalf("Roots() = %v, want the home fallback", got)
	}
}

func TestClaudeMatch(t *testing.T) {
	imp := &claudeImporter{}
	tests := []struct {
		path string
		want bool
	}{
		{"/p/sess.jsonl", true},
		{"/p/notes.txt", false},
		{"/p/sess/subagents/agent-1.jsonl", false},
	}
	for _, tt := range tests {
		if got := imp.Match(tt.path); got != tt.want {
			t.Errorf("Match(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestClaudePreviewIsMetadataOnly(t *testing.T) {
	root := t.TempDir()
	path := writeClaudeSession(t, root, "-work-repo", "sess-a")

	preview, ok, err := claudeImporterAt(root).Preview(context.Background(), path)
	if err != nil || !ok {
		t.Fatalf("Preview: ok=%v err=%v", ok, err)
	}
	if preview.SessionID != "sess-a" {
		t.Fatalf("SessionID = %q, want sess-a", preview.SessionID)
	}
	if preview.Title != "sess-a" {
		t.Fatalf("Title = %q, want the session ID: a preview must not carry prompt text", preview.Title)
	}
	if preview.Workspace != "/work/repo" {
		t.Fatalf("Workspace = %q, want /work/repo", preview.Workspace)
	}
	if preview.TurnCount != 2 || preview.ApproxTurns {
		t.Fatalf("TurnCount = %d approx=%v, want an exact 2", preview.TurnCount, preview.ApproxTurns)
	}
	wantStart := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	if !preview.StartedAt.Equal(wantStart) {
		t.Fatalf("StartedAt = %s, want %s", preview.StartedAt, wantStart)
	}
	wantLast := time.Date(2026, 1, 10, 12, 1, 10, 0, time.UTC)
	if !preview.LastActivityAt.Equal(wantLast) {
		t.Fatalf("LastActivityAt = %s, want %s", preview.LastActivityAt, wantLast)
	}
}

// TestClaudePreviewIsBounded is the guard for the fork's full-decode preview,
// which put a 5.3 GB decode behind an interactive request.
//
// The fixture is 200 MB with a marker line in the middle: a timestamp in 2030
// that no head or tail line carries. A preview that reports it read past the
// budget.
func TestClaudePreviewIsBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("writes a 200 MB fixture")
	}
	root := t.TempDir()
	path := filepath.Join(root, "-work-repo", "big.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	filler := strings.Repeat("x", 200_000)
	base := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	write := func(s string) {
		if _, err := f.WriteString(s); err != nil {
			t.Fatal(err)
		}
	}
	write(claudeUserLine("sess-big", "/work/repo", base.Format(time.RFC3339), "start"))
	const turns = 1000
	for i := range turns {
		ts := base.Add(time.Duration(i) * time.Second)
		if i == turns/2 {
			ts = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
		}
		write(claudeAssistantLine("sess-big", "/work/repo", ts.Format(time.RFC3339), fmt.Sprintf("req-%d", i), filler, nil))
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() < 200_000_000 {
		t.Fatalf("fixture is %d bytes, want at least 200 MB", info.Size())
	}

	win, err := ReadPreviewWindows(path, PreviewByteBudget)
	if err != nil {
		t.Fatalf("ReadPreviewWindows: %v", err)
	}
	if read := len(win.Head) + len(win.Tail); read > PreviewByteBudget {
		t.Fatalf("preview windows hold %d bytes, want no more than the %d byte budget", read, PreviewByteBudget)
	}

	preview, ok, err := claudeImporterAt(root).Preview(context.Background(), path)
	if err != nil || !ok {
		t.Fatalf("Preview: ok=%v err=%v", ok, err)
	}
	if preview.SessionID != "sess-big" {
		t.Fatalf("SessionID = %q, want sess-big", preview.SessionID)
	}
	if !preview.ApproxTurns {
		t.Fatal("ApproxTurns = false, want true for a file past the preview budget")
	}
	if preview.TurnCount <= 0 {
		t.Fatalf("TurnCount = %d, want a positive estimate", preview.TurnCount)
	}
	if preview.LastActivityAt.Year() >= 2030 {
		t.Fatalf("LastActivityAt = %s, want a head or tail timestamp: the middle of the file was read", preview.LastActivityAt)
	}
	if preview.SizeBytes != info.Size() {
		t.Fatalf("SizeBytes = %d, want the stat size %d", preview.SizeBytes, info.Size())
	}
}

// TestClaudeTurnsMatchTheLiveMapper pins the reuse rule: an imported turn is
// what live capture would have produced for the same lines, not a second
// mapping of the same transcript.
func TestClaudeTurnsMatchTheLiveMapper(t *testing.T) {
	root := t.TempDir()
	path := writeClaudeSession(t, root, "-work-repo", "sess-a")
	imp := claudeImporterAt(root)

	preview, ok, err := imp.Preview(context.Background(), path)
	if err != nil || !ok {
		t.Fatalf("Preview: ok=%v err=%v", ok, err)
	}
	turns := collectTurns(t, imp, preview)
	if len(turns) != 2 {
		t.Fatalf("got %d turns, want 2", len(turns))
	}

	// The same lines through Coalesce and Process directly, the path the live
	// hook runs.
	lines, _, err := transcript.Read(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := mapper.Process(mapper.CoalesceSession(lines), &state.Session{}, mapper.Options{SessionID: preview.SessionID}, nil)
	if len(want) != len(turns) {
		t.Fatalf("the importer produced %d turns, the live mapper %d", len(turns), len(want))
	}
	for i := range want {
		got := turns[i].Gen
		// The importer fills the user ID the live hook attaches separately.
		got.UserID = want[i].UserID
		// The generation ID is the import's own: the mapper derives it from
		// the request ID, and one request ID can produce two coalesced turns,
		// so the mapper's ID is not unique within a session.
		if got.ID != turns[i].Source.GenerationID() {
			t.Fatalf("turn %d ID = %q, want the deterministic %q", i, got.ID, turns[i].Source.GenerationID())
		}
		got.ID = want[i].ID
		if !reflect.DeepEqual(got, want[i]) {
			t.Fatalf("turn %d differs from the live mapper:\n got %+v\nwant %+v", i, got, want[i])
		}
	}
	for i, turn := range turns {
		if turn.Gen.AgentName != "claude-code" {
			t.Errorf("turn %d AgentName = %q, want claude-code", i, turn.Gen.AgentName)
		}
		if turn.Source.TurnIndex != i {
			t.Errorf("turn %d TurnIndex = %d", i, turn.Source.TurnIndex)
		}
		if turn.Source.SourcePath != path {
			t.Errorf("turn %d SourcePath = %q, want %q", i, turn.Source.SourcePath, path)
		}
		if turn.Quality.ApproxUsage {
			t.Errorf("turn %d reports approximate usage, but the source recorded tokens", i)
		}
	}
	if got := turns[0].Gen.Input[0].Parts[0].Text; got != "explain the build" {
		t.Fatalf("first input = %q, want the first prompt", got)
	}
	if got := turns[1].Gen.Output[0].Parts[0].Text; got != "They pass." {
		t.Fatalf("second output = %q, want the second response", got)
	}
}

// TestClaudeTurnsAreDeterministic pins that a second read of the same file
// produces the same generation IDs, the property re-import idempotency rests
// on.
func TestClaudeTurnsAreDeterministic(t *testing.T) {
	root := t.TempDir()
	path := writeClaudeSession(t, root, "-work-repo", "sess-a")
	imp := claudeImporterAt(root)
	preview, _, err := imp.Preview(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}

	first := collectTurns(t, imp, preview)
	second := collectTurns(t, claudeImporterAt(root), preview)
	if len(first) != len(second) {
		t.Fatalf("turn counts differ: %d then %d", len(first), len(second))
	}
	for i := range first {
		if a, b := first[i].Source.GenerationID(), second[i].Source.GenerationID(); a != b {
			t.Fatalf("turn %d generation ID changed between reads: %q then %q", i, a, b)
		}
	}
}

func TestClaudeSubagentTurnsLinkToTheirParent(t *testing.T) {
	root := t.TempDir()
	sessionID := "sess-sub"
	project := filepath.Join(root, "-work-repo")
	subDir := filepath.Join(project, sessionID, "subagents")
	agentID := "a18f9a9d9f1f3d28e"

	spawn := claudeLine(map[string]any{
		"type":      "assistant",
		"sessionId": sessionID,
		"cwd":       "/work/repo",
		"timestamp": "2026-01-10T12:00:10Z",
		"requestId": "req-1",
		"message": map[string]any{
			"model":       "claude-sonnet-4-20250514",
			"stop_reason": "tool_use",
			"usage":       map[string]any{"input_tokens": 10, "output_tokens": 5},
			"content": []map[string]any{
				{"type": "tool_use", "id": "tu_agent", "name": "Agent", "input": map[string]any{"subagent_type": "explorer"}},
			},
		},
	})
	parentBody := claudeUserLine(sessionID, "/work/repo", "2026-01-10T12:00:00Z", "explore the repo") +
		spawn +
		claudeToolResultLine(sessionID, "tu_agent", "agentId: "+agentID+"\nTranscript dir: "+subDir+"\nfound three packages") +
		claudeAssistantLine(sessionID, "/work/repo", "2026-01-10T12:02:00Z", "req-2", "Summarised.", nil)
	parentPath := writeFile(t, filepath.Join(project, sessionID+".jsonl"), parentBody)

	// The subagent transcript: sidechain lines with no stop_reason, which is
	// what Claude Code writes.
	subBody := claudeLine(map[string]any{
		"type":        "user",
		"sessionId":   sessionID,
		"isSidechain": true,
		"agentId":     agentID,
		"timestamp":   "2026-01-10T12:00:11Z",
		"message":     map[string]any{"role": "user", "content": "explore the repo"},
	}) + claudeLine(map[string]any{
		"type":             "assistant",
		"sessionId":        sessionID,
		"isSidechain":      true,
		"agentId":          agentID,
		"attributionAgent": "explorer",
		"timestamp":        "2026-01-10T12:00:20Z",
		"requestId":        "req-side",
		"message": map[string]any{
			"model":   "claude-sonnet-4-20250514",
			"usage":   map[string]any{"input_tokens": 10, "output_tokens": 7},
			"content": []map[string]any{{"type": "tool_use", "id": "tu_read", "name": "Read", "input": map[string]any{"file_path": "x.go"}}},
		},
	}) + claudeLine(map[string]any{
		"type":        "user",
		"sessionId":   sessionID,
		"isSidechain": true,
		"timestamp":   "2026-01-10T12:00:25Z",
		"message": map[string]any{
			"role":    "user",
			"content": []map[string]any{{"type": "tool_result", "tool_use_id": "tu_read", "content": "package x"}},
		},
	})
	writeFile(t, filepath.Join(subDir, "agent-"+agentID+".jsonl"), subBody)

	imp := claudeImporterAt(root)
	preview, ok, err := imp.Preview(context.Background(), parentPath)
	if err != nil || !ok {
		t.Fatalf("Preview: ok=%v err=%v", ok, err)
	}
	turns := collectTurns(t, imp, preview)

	var parentSpawnID string
	var subagentTurns []HistoricalGeneration
	for _, turn := range turns {
		switch {
		case turn.Gen.AgentName == "claude-code" && strings.HasSuffix(turn.Source.SourcePath, sessionID+".jsonl"):
			for _, msg := range turn.Gen.Output {
				for _, part := range msg.Parts {
					if part.ToolCall != nil && part.ToolCall.Name == "Agent" {
						parentSpawnID = turn.Gen.ID
					}
				}
			}
		case strings.Contains(turn.Source.SourcePath, "/subagents/"):
			subagentTurns = append(subagentTurns, turn)
		}
	}
	if parentSpawnID == "" {
		t.Fatal("no parent generation with an Agent tool call")
	}
	if len(subagentTurns) != 1 {
		t.Fatalf("got %d subagent turns, want 1", len(subagentTurns))
	}
	sub := subagentTurns[0]
	if sub.Gen.AgentName != "claude-code/explorer" {
		t.Fatalf("subagent AgentName = %q, want claude-code/explorer", sub.Gen.AgentName)
	}
	if !slices.Equal(sub.Gen.ParentGenerationIDs, []string{parentSpawnID}) {
		t.Fatalf("ParentGenerationIDs = %v, want [%s]", sub.Gen.ParentGenerationIDs, parentSpawnID)
	}
	if sub.Gen.ConversationID != sessionID {
		t.Fatalf("subagent ConversationID = %q, want %q", sub.Gen.ConversationID, sessionID)
	}

	// The subagent has its own transcript, so the parent's one-line Agent
	// summary must not also appear.
	for _, turn := range turns {
		if turn.Gen.AgentName == "claude-code/explorer" && !strings.Contains(turn.Source.SourcePath, "/subagents/") {
			t.Fatalf("the parent transcript still produced a summary generation %q", turn.Gen.ID)
		}
	}
}

func TestClaudeDiscoverSkipsActiveSessions(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 1, 10, 13, 0, 0, 0, time.UTC)
	stale := writeClaudeSession(t, root, "-work-repo", "sess-stale")
	live := writeClaudeSession(t, root, "-work-repo", "sess-live")
	setModTime(t, stale, now.Add(-2*time.Hour))
	setModTime(t, live, now.Add(-time.Minute))

	discovery, err := Discover(context.Background(), AgentClaudeCode, claudeImporterAt(root), DiscoverOptions{
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(discovery.Sessions) != 2 {
		t.Fatalf("got %d sessions, want 2", len(discovery.Sessions))
	}
	selected, skipped := NewFilter().SelectSessions(discovery.Sessions)
	if len(selected) != 1 || selected[0].SessionID != "sess-stale" {
		t.Fatalf("selected = %v, want only sess-stale", sessionIDs(selected))
	}
	if len(skipped) != 1 || skipped[0].Reason != SkipActiveSession {
		t.Fatalf("skipped = %+v, want one active session", skipped)
	}
}

func TestClaudeQuality(t *testing.T) {
	root := t.TempDir()
	// An assistant line with no model and no usage: Claude Code writes these
	// when it recovers from a transport error.
	body := claudeUserLine("sess-q", "/work/repo", "2026-01-10T12:00:00Z", "hi") +
		claudeLine(map[string]any{
			"type":      "assistant",
			"sessionId": "sess-q",
			"timestamp": "2026-01-10T12:00:05Z",
			"requestId": "req-1",
			"message": map[string]any{
				"content":     []map[string]any{{"type": "text", "text": "hello"}},
				"stop_reason": "end_turn",
				"usage":       map[string]any{"output_tokens": 1},
			},
		})
	path := writeFile(t, filepath.Join(root, "-work-repo", "sess-q.jsonl"), body)

	imp := claudeImporterAt(root)
	preview, _, err := imp.Preview(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	turns := collectTurns(t, imp, preview)
	if len(turns) != 1 {
		t.Fatalf("got %d turns, want 1", len(turns))
	}
	if !turns[0].Quality.MissingModel {
		t.Error("MissingModel = false, want true for a line with no model")
	}
	if turns[0].Quality.ApproxStartedAt || turns[0].Quality.ApproxCompletedAt {
		t.Error("timestamps reported as approximate, but the source carried them")
	}
}

// claudeNullStopReasonSession writes the transcript shape Claude Code produced
// from version 2.0.62 to 2.1.63 and still produces in subagent transcripts:
// stop_reason null on every assistant line, one line per streamed content
// block, and one turn split across two coalesced groups by an interleaved tool
// result.
func claudeNullStopReasonSession(t *testing.T, root, sessionID string) string {
	t.Helper()
	assistant := func(ts, requestID string, content []map[string]any) string {
		return claudeLine(map[string]any{
			"type":      "assistant",
			"sessionId": sessionID,
			"cwd":       "/work/repo",
			"timestamp": ts,
			"requestId": requestID,
			"version":   "2.1.22",
			"message": map[string]any{
				"model":       "claude-sonnet-4-20250514",
				"content":     content,
				"stop_reason": nil,
				"usage":       map[string]any{"input_tokens": 100, "output_tokens": 25},
			},
		})
	}
	body := claudeUserLine(sessionID, "/work/repo", "2026-01-10T12:00:00Z", "read the file") +
		// One request streamed as two lines, then its tool result, then more
		// output under the same request ID. Coalescing splits that into two
		// groups, which is why the mapper's request-derived ID is not unique.
		assistant("2026-01-10T12:00:05Z", "req-1", []map[string]any{{"type": "text", "text": "Reading."}}) +
		assistant("2026-01-10T12:00:06Z", "req-1", []map[string]any{
			{"type": "tool_use", "id": "tu_1", "name": "Read", "input": map[string]any{"file_path": "x.go"}},
		}) +
		claudeToolResultLine(sessionID, "tu_1", "package x") +
		assistant("2026-01-10T12:00:09Z", "req-1", []map[string]any{{"type": "text", "text": "It is package x."}})
	return writeFile(t, filepath.Join(root, "-work-repo", sessionID+".jsonl"), body)
}

// TestClaudeImportsTurnsWithoutAStopReason covers the transcripts a third of
// this machine's sessions are written in. The live reader holds back a turn
// with no terminal stop_reason, waiting for a next read; an imported file has
// no next read, so those turns must be imported rather than dropped.
func TestClaudeImportsTurnsWithoutAStopReason(t *testing.T) {
	root := t.TempDir()
	path := claudeNullStopReasonSession(t, root, "sess-null")
	imp := claudeImporterAt(root)

	preview, ok, err := imp.Preview(context.Background(), path)
	if err != nil || !ok {
		t.Fatalf("Preview: ok=%v err=%v", ok, err)
	}
	turns := collectTurns(t, imp, preview)
	if len(turns) != 2 {
		t.Fatalf("got %d turns, want 2 (the tool call and the answer after it)", len(turns))
	}
	if got := turns[0].Gen.Output[0].Parts[0].Text; got != "Reading." {
		t.Fatalf("first turn output = %q", got)
	}
	last := turns[len(turns)-1].Gen
	if len(last.Output) == 0 || last.Output[0].Parts[0].Text != "It is package x." {
		t.Fatalf("the final answer was dropped: %+v", last.Output)
	}
}

// TestClaudeSplitRequestKeepsDistinctGenerationIDs pins the fix for turns that
// share a request ID. The mapper derives its ID from the request, so both
// halves of a split request carry one ID; the viewer keeps the newest record
// per generation ID, so the first half would disappear.
func TestClaudeSplitRequestKeepsDistinctGenerationIDs(t *testing.T) {
	root := t.TempDir()
	path := claudeNullStopReasonSession(t, root, "sess-split")
	imp := claudeImporterAt(root)

	preview, _, err := imp.Preview(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	turns := collectTurns(t, imp, preview)
	if len(turns) < 2 {
		t.Fatalf("got %d turns, want the split request to produce two", len(turns))
	}
	if turns[0].Source.TurnID != turns[1].Source.TurnID {
		t.Fatalf("test fixture no longer splits one request: %q and %q",
			turns[0].Source.TurnID, turns[1].Source.TurnID)
	}
	seen := map[string]int{}
	for i, turn := range turns {
		if turn.Gen.ID != turn.Source.GenerationID() {
			t.Fatalf("turn %d ID = %q, want the deterministic %q", i, turn.Gen.ID, turn.Source.GenerationID())
		}
		seen[turn.Gen.ID]++
	}
	for id, n := range seen {
		if n > 1 {
			t.Fatalf("generation ID %s used by %d turns; the viewer keeps only the last", id, n)
		}
	}
}

// TestClaudeSubagentFinalAnswerIsImported covers a subagent run that ends in a
// plain-text answer: no tool call closes it, and nothing follows it in the
// file, so it is the turn most easily lost.
func TestClaudeSubagentFinalAnswerIsImported(t *testing.T) {
	root := t.TempDir()
	sessionID := "sess-answer"
	project := filepath.Join(root, "-work-repo")
	subDir := filepath.Join(project, sessionID, "subagents")
	agentID := "b27c1f3e4d5a6b7c8"

	spawn := claudeLine(map[string]any{
		"type": "assistant", "sessionId": sessionID, "cwd": "/work/repo",
		"timestamp": "2026-01-10T12:00:10Z", "requestId": "req-1",
		"message": map[string]any{
			"model": "claude-sonnet-4-20250514", "stop_reason": "tool_use",
			"usage": map[string]any{"input_tokens": 10, "output_tokens": 5},
			"content": []map[string]any{
				{"type": "tool_use", "id": "tu_agent", "name": "Agent", "input": map[string]any{"subagent_type": "explorer"}},
			},
		},
	})
	parentBody := claudeUserLine(sessionID, "/work/repo", "2026-01-10T12:00:00Z", "explore the repo") +
		spawn +
		claudeToolResultLine(sessionID, "tu_agent", "agentId: "+agentID+"\nTranscript dir: "+subDir+"\nthree packages")
	parentPath := writeFile(t, filepath.Join(project, sessionID+".jsonl"), parentBody)

	sidechainAssistant := func(ts, requestID string, content []map[string]any) string {
		return claudeLine(map[string]any{
			"type": "assistant", "sessionId": sessionID, "isSidechain": true,
			"agentId": agentID, "attributionAgent": "explorer",
			"timestamp": ts, "requestId": requestID,
			"message": map[string]any{
				"model":       "claude-sonnet-4-20250514",
				"stop_reason": nil,
				"usage":       map[string]any{"input_tokens": 10, "output_tokens": 7},
				"content":     content,
			},
		})
	}
	subBody := claudeLine(map[string]any{
		"type": "user", "sessionId": sessionID, "isSidechain": true, "agentId": agentID,
		"timestamp": "2026-01-10T12:00:11Z",
		"message":   map[string]any{"role": "user", "content": "explore the repo"},
	}) +
		sidechainAssistant("2026-01-10T12:00:20Z", "req-side-1", []map[string]any{
			{"type": "tool_use", "id": "tu_read", "name": "Read", "input": map[string]any{"file_path": "x.go"}},
		}) +
		claudeLine(map[string]any{
			"type": "user", "sessionId": sessionID, "isSidechain": true,
			"timestamp": "2026-01-10T12:00:25Z",
			"message": map[string]any{
				"role":    "user",
				"content": []map[string]any{{"type": "tool_result", "tool_use_id": "tu_read", "content": "package x"}},
			},
		}) +
		// The subagent's answer: plain text, no stop_reason, last line of the
		// file.
		sidechainAssistant("2026-01-10T12:00:30Z", "req-side-2", []map[string]any{
			{"type": "text", "text": "The repo has three packages."},
		})
	writeFile(t, filepath.Join(subDir, "agent-"+agentID+".jsonl"), subBody)

	imp := claudeImporterAt(root)
	preview, _, err := imp.Preview(context.Background(), parentPath)
	if err != nil {
		t.Fatal(err)
	}

	var answers []string
	for _, turn := range collectTurns(t, imp, preview) {
		if !strings.Contains(turn.Source.SourcePath, "/subagents/") {
			continue
		}
		for _, msg := range turn.Gen.Output {
			for _, part := range msg.Parts {
				if part.Text != "" {
					answers = append(answers, part.Text)
				}
			}
		}
	}
	if !slices.Contains(answers, "The repo has three packages.") {
		t.Fatalf("the subagent's final answer is missing; imported texts = %v", answers)
	}
}

// TestClaudePreviewCountsRequestsNotLines pins the preview unit. Claude Code
// writes one line per content block, so counting assistant lines reported
// several times the turns an import produces.
func TestClaudePreviewCountsRequestsNotLines(t *testing.T) {
	root := t.TempDir()
	path := claudeNullStopReasonSession(t, root, "sess-count")
	imp := claudeImporterAt(root)

	preview, ok, err := imp.Preview(context.Background(), path)
	if err != nil || !ok {
		t.Fatalf("Preview: ok=%v err=%v", ok, err)
	}
	if preview.ApproxTurns {
		t.Fatal("a session inside the preview budget is counted exactly")
	}
	// Three assistant lines, one request: the file holds one request's worth of
	// model output, which the import turns into two generations at most.
	if preview.TurnCount != 1 {
		t.Fatalf("TurnCount = %d, want 1 request", preview.TurnCount)
	}
}

// TestClaudeSplitRequestKeepsParentLinks covers the parent links of a
// transcript whose request was split into two turns. Both halves leave the
// mapper under one request-derived ID, so a parent link that names that ID must
// still resolve to the half that made it.
func TestClaudeSplitRequestKeepsParentLinks(t *testing.T) {
	t.Run("subagent chain", func(t *testing.T) {
		root := t.TempDir()
		sessionID := "sess-split-sub"
		project := filepath.Join(root, "-work-repo")
		subDir := filepath.Join(project, sessionID, "subagents")
		agentID := "c39d2e4f5a6b7c8d9"

		spawn := claudeLine(map[string]any{
			"type": "assistant", "sessionId": sessionID, "cwd": "/work/repo",
			"timestamp": "2026-01-10T12:00:10Z", "requestId": "req-1",
			"message": map[string]any{
				"model": "claude-sonnet-4-20250514", "stop_reason": "tool_use",
				"usage": map[string]any{"input_tokens": 10, "output_tokens": 5},
				"content": []map[string]any{
					{"type": "tool_use", "id": "tu_agent", "name": "Agent", "input": map[string]any{"subagent_type": "explorer"}},
				},
			},
		})
		parentBody := claudeUserLine(sessionID, "/work/repo", "2026-01-10T12:00:00Z", "explore the repo") +
			spawn +
			claudeToolResultLine(sessionID, "tu_agent", "agentId: "+agentID+"\nTranscript dir: "+subDir+"\nthree packages")
		parentPath := writeFile(t, filepath.Join(project, sessionID+".jsonl"), parentBody)

		sidechainAssistant := func(ts string, content []map[string]any) string {
			return claudeLine(map[string]any{
				"type": "assistant", "sessionId": sessionID, "isSidechain": true,
				"agentId": agentID, "attributionAgent": "explorer",
				// One request ID for both halves, which is what a split request
				// looks like in a subagent transcript.
				"timestamp": ts, "requestId": "req-side",
				"message": map[string]any{
					"model":       "claude-sonnet-4-20250514",
					"stop_reason": nil,
					"usage":       map[string]any{"input_tokens": 10, "output_tokens": 7},
					"content":     content,
				},
			})
		}
		subBody := claudeLine(map[string]any{
			"type": "user", "sessionId": sessionID, "isSidechain": true, "agentId": agentID,
			"timestamp": "2026-01-10T12:00:11Z",
			"message":   map[string]any{"role": "user", "content": "explore the repo"},
		}) +
			sidechainAssistant("2026-01-10T12:00:20Z", []map[string]any{
				{"type": "tool_use", "id": "tu_read", "name": "Read", "input": map[string]any{"file_path": "x.go"}},
			}) +
			claudeLine(map[string]any{
				"type": "user", "sessionId": sessionID, "isSidechain": true,
				"timestamp": "2026-01-10T12:00:25Z",
				"message": map[string]any{
					"role":    "user",
					"content": []map[string]any{{"type": "tool_result", "tool_use_id": "tu_read", "content": "package x"}},
				},
			}) +
			sidechainAssistant("2026-01-10T12:00:30Z", []map[string]any{
				{"type": "text", "text": "The repo has three packages."},
			})
		writeFile(t, filepath.Join(subDir, "agent-"+agentID+".jsonl"), subBody)

		imp := claudeImporterAt(root)
		preview, _, err := imp.Preview(context.Background(), parentPath)
		if err != nil {
			t.Fatal(err)
		}
		turns := collectTurns(t, imp, preview)

		spawnID := claudeSpawnTurnID(t, turns, sessionID)
		var sub []HistoricalGeneration
		for _, turn := range turns {
			if strings.Contains(turn.Source.SourcePath, "/subagents/") {
				sub = append(sub, turn)
			}
		}
		if len(sub) != 2 {
			t.Fatalf("got %d subagent turns, want the split request to produce two", len(sub))
		}
		if !slices.Equal(sub[0].Gen.ParentGenerationIDs, []string{spawnID}) {
			t.Fatalf("first subagent turn parents = %v, want the spawning turn [%s]", sub[0].Gen.ParentGenerationIDs, spawnID)
		}
		if slices.Contains(sub[1].Gen.ParentGenerationIDs, sub[1].Gen.ID) {
			t.Fatalf("second subagent turn is its own parent: %v", sub[1].Gen.ParentGenerationIDs)
		}
		if !slices.Equal(sub[1].Gen.ParentGenerationIDs, []string{sub[0].Gen.ID}) {
			t.Fatalf("second subagent turn parents = %v, want the turn before it [%s]", sub[1].Gen.ParentGenerationIDs, sub[0].Gen.ID)
		}
	})

	// Claude Code reuses one request ID across the session transcript and a
	// subagent transcript: 67 of the 1438 sessions with subagents on this
	// machine do it. The mapper derives its generation ID from the request, so
	// the two turns arrive under one ID from different files, and only a
	// session-wide rename map keeps them apart.
	t.Run("request id shared across transcripts", func(t *testing.T) {
		root := t.TempDir()
		sessionID := "sess-shared-req"
		project := filepath.Join(root, "-work-repo")
		subDir := filepath.Join(project, sessionID, "subagents")
		agentID := "b17c4d9e0f1a2b3c4"

		parentBody := claudeUserLine(sessionID, "/work/repo", "2026-01-10T12:00:00Z", "explore the repo") +
			claudeLine(map[string]any{
				"type": "assistant", "sessionId": sessionID, "cwd": "/work/repo",
				"timestamp": "2026-01-10T12:00:10Z", "requestId": "req-shared",
				"message": map[string]any{
					"model": "claude-sonnet-4-20250514", "stop_reason": "tool_use",
					"usage": map[string]any{"input_tokens": 10, "output_tokens": 5},
					"content": []map[string]any{
						{"type": "tool_use", "id": "tu_agent", "name": "Agent", "input": map[string]any{"subagent_type": "explorer"}},
					},
				},
			}) +
			claudeToolResultLine(sessionID, "tu_agent", "agentId: "+agentID+"\nTranscript dir: "+subDir+"\nthree packages")
		parentPath := writeFile(t, filepath.Join(project, sessionID+".jsonl"), parentBody)

		subBody := claudeLine(map[string]any{
			"type": "user", "sessionId": sessionID, "isSidechain": true, "agentId": agentID,
			"timestamp": "2026-01-10T12:00:11Z",
			"message":   map[string]any{"role": "user", "content": "explore the repo"},
		}) +
			claudeLine(map[string]any{
				"type": "assistant", "sessionId": sessionID, "isSidechain": true,
				"agentId": agentID, "attributionAgent": "explorer",
				// The same request ID the spawning turn carries.
				"timestamp": "2026-01-10T12:00:20Z", "requestId": "req-shared",
				"message": map[string]any{
					"model": "claude-sonnet-4-20250514", "stop_reason": nil,
					"usage":   map[string]any{"input_tokens": 10, "output_tokens": 7},
					"content": []map[string]any{{"type": "text", "text": "The repo has three packages."}},
				},
			})
		writeFile(t, filepath.Join(subDir, "agent-"+agentID+".jsonl"), subBody)

		imp := claudeImporterAt(root)
		preview, _, err := imp.Preview(context.Background(), parentPath)
		if err != nil {
			t.Fatal(err)
		}
		turns := collectTurns(t, imp, preview)

		spawnID := claudeSpawnTurnID(t, turns, sessionID)
		var sub []HistoricalGeneration
		for _, turn := range turns {
			if strings.Contains(turn.Source.SourcePath, "/subagents/") {
				sub = append(sub, turn)
			}
		}
		if len(sub) != 1 {
			t.Fatalf("got %d subagent turns, want 1", len(sub))
		}
		if slices.Contains(sub[0].Gen.ParentGenerationIDs, sub[0].Gen.ID) {
			t.Fatalf("the subagent turn is its own parent: %v", sub[0].Gen.ParentGenerationIDs)
		}
		if !slices.Equal(sub[0].Gen.ParentGenerationIDs, []string{spawnID}) {
			t.Fatalf("subagent parents = %v, want the spawning turn [%s]", sub[0].Gen.ParentGenerationIDs, spawnID)
		}
	})

	t.Run("synthesised subagent turn", func(t *testing.T) {
		root := t.TempDir()
		sessionID := "sess-split-spawn"
		project := filepath.Join(root, "-work-repo")

		assistant := func(ts string, content []map[string]any) string {
			return claudeLine(map[string]any{
				"type": "assistant", "sessionId": sessionID, "cwd": "/work/repo",
				// The spawning half and the half after the tool result share one
				// request ID.
				"timestamp": ts, "requestId": "req-1", "version": "2.1.22",
				"message": map[string]any{
					"model":       "claude-sonnet-4-20250514",
					"stop_reason": nil,
					"usage":       map[string]any{"input_tokens": 10, "output_tokens": 5},
					"content":     content,
				},
			})
		}
		// No subagents directory here, so the Agent call keeps the one-line
		// summary generation the mapper synthesises for it.
		body := claudeUserLine(sessionID, "/work/repo", "2026-01-10T12:00:00Z", "explore the repo") +
			assistant("2026-01-10T12:00:10Z", []map[string]any{
				{"type": "tool_use", "id": "tu_agent", "name": "Agent", "input": map[string]any{"subagent_type": "explorer"}},
			}) +
			claudeToolResultLine(sessionID, "tu_agent", "three packages") +
			assistant("2026-01-10T12:00:40Z", []map[string]any{{"type": "text", "text": "Summarised."}})
		path := writeFile(t, filepath.Join(project, sessionID+".jsonl"), body)

		imp := claudeImporterAt(root)
		preview, _, err := imp.Preview(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		turns := collectTurns(t, imp, preview)

		spawnID := claudeSpawnTurnID(t, turns, sessionID)
		var synth *HistoricalGeneration
		for i, turn := range turns {
			if turn.Gen.AgentName == "claude-code/explorer" {
				synth = &turns[i]
			}
		}
		if synth == nil {
			t.Fatal("no synthesised subagent generation")
		}
		if !slices.Equal(synth.Gen.ParentGenerationIDs, []string{spawnID}) {
			t.Fatalf("subagent parents = %v, want the spawning turn [%s]", synth.Gen.ParentGenerationIDs, spawnID)
		}
	})
}

// claudeSpawnTurnID returns the ID of the main-transcript turn that made the
// Agent tool call.
func claudeSpawnTurnID(t *testing.T, turns []HistoricalGeneration, sessionID string) string {
	t.Helper()
	var id string
	for _, turn := range turns {
		if !strings.HasSuffix(turn.Source.SourcePath, sessionID+".jsonl") {
			continue
		}
		for _, msg := range turn.Gen.Output {
			for _, part := range msg.Parts {
				if part.ToolCall != nil && part.ToolCall.Name == "Agent" {
					id = turn.Gen.ID
				}
			}
		}
	}
	if id == "" {
		t.Fatal("no generation with an Agent tool call")
	}
	return id
}
