package history

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/chatstore/chatstoretest"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/fragment"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/mapper"
)

// cursorFixture is one committed session description under
// testdata/cursor. The test writes it as a real store.db, so the fixture stays
// reviewable text while the byte-level encoding comes from the one writer both
// the reader's tests and these tests use.
type cursorFixture struct {
	SessionID     string                 `json:"sessionId"`
	Name          string                 `json:"name"`
	CreatedAtMs   int64                  `json:"createdAtMs"`
	LastUsedModel string                 `json:"lastUsedModel"`
	WorkspaceURI  string                 `json:"workspaceUri"`
	Messages      []cursorFixtureMessage `json:"messages"`
}

type cursorFixtureMessage struct {
	Kind        string          `json:"kind"`
	Text        string          `json:"text"`
	ToolName    string          `json:"toolName"`
	ToolCallID  string          `json:"toolCallId"`
	ReasoningID string          `json:"reasoningId"`
	Args        json.RawMessage `json:"args"`
	Result      json.RawMessage `json:"result"`
	// IssuedAt is what the provider ID on this message encodes, written out so
	// the fixture states the time as well as the bytes that hold it. Empty on a
	// message whose IDs carry no time.
	IssuedAt string `json:"issuedAt"`
}

// turnTimes are the windows the fixture's issuedAt fields describe: one per
// turn, from the first time in the turn to the last. A turn whose messages state
// none is returned zeroed, which is the turn the importer has to interpolate.
func (fx cursorFixture) turnTimes(t *testing.T) [][2]time.Time {
	t.Helper()
	var turns [][2]time.Time
	for _, m := range fx.Messages {
		if m.Kind == "prompt" {
			turns = append(turns, [2]time.Time{})
		}
		if m.IssuedAt == "" || len(turns) == 0 {
			continue
		}
		ts, err := time.Parse(time.RFC3339, m.IssuedAt)
		if err != nil {
			t.Fatalf("fixture issuedAt %q: %v", m.IssuedAt, err)
		}
		window := &turns[len(turns)-1]
		if window[0].IsZero() || ts.Before(window[0]) {
			window[0] = ts.UTC()
		}
		if ts.After(window[1]) {
			window[1] = ts.UTC()
		}
	}
	return turns
}

// nth returns the fixture's nth message of one kind. Assertions select through
// it rather than by position, so adding a message to a fixture cannot silently
// change what another test asserts, and removing one fails here rather than
// asserting against an empty string.
func (fx cursorFixture) nth(t *testing.T, kind string, n int) cursorFixtureMessage {
	t.Helper()
	var seen int
	for _, m := range fx.Messages {
		if m.Kind != kind {
			continue
		}
		if seen == n {
			return m
		}
		seen++
	}
	t.Fatalf("the fixture holds %d messages of kind %q, want at least %d", seen, kind, n+1)
	return cursorFixtureMessage{}
}

func loadCursorFixture(t *testing.T, name string) cursorFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "cursor", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fx cursorFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("decode fixture %s: %v", name, err)
	}
	if len(fx.Messages) == 0 {
		t.Fatalf("fixture %s has no messages", name)
	}
	return fx
}

// writeCursorStore writes a session under a root, in the layout Cursor uses:
// <root>/<workspace-hash>/<session-uuid>/store.db.
func writeCursorStore(t *testing.T, root string, fx cursorFixture) string {
	t.Helper()
	dir := filepath.Join(root, "9264a52109482fc977f5460d764a5af5", fx.SessionID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	b := chatstoretest.New(t, filepath.Join(dir, "store.db"), fx.SessionID)
	b.Meta.Name = fx.Name
	b.Meta.CreatedAt = fx.CreatedAtMs
	b.Meta.LastUsedModel = fx.LastUsedModel
	b.WorkspaceURI = fx.WorkspaceURI
	for _, m := range fx.Messages {
		switch m.Kind {
		case "system":
			b.AddSystem(m.Text)
		case "preamble":
			b.AddPreamble(m.Text)
		case "prompt":
			b.AddPrompt(m.Text)
		case "assistant":
			b.AddAssistantText(m.Text)
		case "reasoning":
			if m.ReasoningID != "" {
				b.AddAssistantReasoningID(m.Text, m.ReasoningID)
				break
			}
			b.AddAssistantReasoning(m.Text)
		case "toolCall":
			b.AddToolCall(m.ToolName, m.ToolCallID, string(m.Args))
		case "toolResult":
			b.AddToolResult(m.ToolName, m.ToolCallID, string(m.Result))
		default:
			t.Fatalf("fixture message kind %q is not one this test can write", m.Kind)
		}
	}
	return b.Build()
}

func cursorImporterAt(root string) *cursorImporter {
	return &cursorImporter{
		roots: []string{root},
		now:   func() time.Time { return time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC) },
	}
}

// cursorBuild writes one session under a fresh root, in the layout Cursor uses,
// and returns an importer over that root with the store's path. It is for the
// tests that need a session shape no committed fixture holds.
func cursorBuild(t *testing.T, sessionID string, setup func(b *chatstoretest.Builder)) (*cursorImporter, string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "9264a52109482fc977f5460d764a5af5", sessionID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	b := chatstoretest.New(t, filepath.Join(dir, "store.db"), sessionID)
	setup(b)
	return cursorImporterAt(root), b.Build()
}

// cursorWalk reads every turn of a session and every problem the walk reports.
// The framework has one channel for the second, an error beside a turn, which it
// records as a warning naming the session.
func cursorWalk(t *testing.T, imp *cursorImporter, sess SessionPreview) ([]HistoricalGeneration, []string) {
	t.Helper()
	var turns []HistoricalGeneration
	var problems []string
	for gen, err := range imp.Turns(context.Background(), sess) {
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		turns = append(turns, gen)
	}
	return turns, problems
}

// setCursorStoreTimes fixes the store's modification time, which is the
// session's last activity and so the far end of every interpolated turn window.
func setCursorStoreTimes(t *testing.T, path string, mod time.Time) {
	t.Helper()
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func cursorPreview(t *testing.T, imp *cursorImporter, path string) SessionPreview {
	t.Helper()
	preview, ok, err := imp.Preview(context.Background(), path)
	if err != nil || !ok {
		t.Fatalf("Preview: ok=%v err=%v", ok, err)
	}
	return preview
}

// cursorSession writes a fixture and previews it, which is what an import
// consumes.
func cursorSession(t *testing.T, name string) (*cursorImporter, SessionPreview) {
	t.Helper()
	root := t.TempDir()
	fx := loadCursorFixture(t, name)
	path := writeCursorStore(t, root, fx)
	setCursorStoreTimes(t, path, time.UnixMilli(fx.CreatedAtMs).UTC().Add(10*time.Minute))
	imp := cursorImporterAt(root)
	return imp, cursorPreview(t, imp, path)
}

func TestCursorRoots(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	tests := []struct {
		name string
		imp  *cursorImporter
		want []string
	}{
		{
			name: "cursor's chats and projects directories under the home directory",
			imp:  &cursorImporter{},
			want: []string{
				filepath.Join("/home/tester", ".cursor", "chats"),
				filepath.Join("/home/tester", ".cursor", "projects"),
			},
		},
		{
			name: "an override wins",
			imp:  &cursorImporter{roots: []string{"/custom/chats"}},
			want: []string{"/custom/chats"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.imp.Roots(); !slices.Equal(got, tt.want) {
				t.Fatalf("Roots() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCursorMatch(t *testing.T) {
	imp := &cursorImporter{}
	sid := "0f2b19b1-3d1f-4c1a-9a1e-2f7c1b9d4e55"
	tests := []struct {
		path string
		want bool
	}{
		{path: "/c/ws/sess/store.db", want: true},
		// SQLite's siblings sit next to the database. Matching either would
		// preview and import the same session more than once.
		{path: "/c/ws/sess/store.db-wal", want: false},
		{path: "/c/ws/sess/store.db-shm", want: false},
		{path: "/c/ws/sess/store.db-journal", want: false},
		{path: "/c/ws/sess/other.db", want: false},
		{path: "/c/ws/sess/store.sqlite", want: false},
		{path: "/c/ws/sess/store.db.bak", want: false},
		{
			path: filepath.Join("/home/u/.cursor/projects/proj/agent-transcripts", sid, sid+".jsonl"),
			want: true,
		},
		{
			path: filepath.Join("/home/u/.cursor/projects/proj/agent-transcripts", sid, "subagents", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee.jsonl"),
			want: false,
		},
		{
			path: filepath.Join("/home/u/.cursor/projects/proj/agent-transcripts", sid, "other.jsonl"),
			want: false,
		},
	}
	for _, tt := range tests {
		if got := imp.Match(tt.path); got != tt.want {
			t.Errorf("Match(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestCursorPreviewIsMetadataOnly(t *testing.T) {
	root := t.TempDir()
	fx := loadCursorFixture(t, "tool-call.json")
	path := writeCursorStore(t, root, fx)
	modTime := time.UnixMilli(fx.CreatedAtMs).UTC().Add(10 * time.Minute)
	setCursorStoreTimes(t, path, modTime)

	preview := cursorPreview(t, cursorImporterAt(root), path)
	if preview.SessionID != fx.SessionID {
		t.Errorf("SessionID = %q, want %q", preview.SessionID, fx.SessionID)
	}
	if preview.Title != preview.SessionID {
		t.Errorf("Title = %q, want the session ID: a preview must not carry prompt text", preview.Title)
	}
	if preview.Workspace != "/work/repo" {
		t.Errorf("Workspace = %q, want /work/repo", preview.Workspace)
	}
	// The count is the session's prompts, and an interrupted session's last
	// prompt was never answered, so an import can produce fewer generations
	// than the plan offers.
	if preview.TurnCount != 2 || !preview.ApproxTurns {
		t.Errorf("TurnCount = %d approx=%v, want 2 marked approximate", preview.TurnCount, preview.ApproxTurns)
	}
	if want := time.UnixMilli(fx.CreatedAtMs).UTC(); !preview.StartedAt.Equal(want) {
		t.Errorf("StartedAt = %s, want the meta row's createdAt %s", preview.StartedAt, want)
	}
	// The session's last provider ID says when it stopped. The file was touched
	// after that, and a modification time is a record of the last process to open
	// the store rather than of the conversation.
	windows := fx.turnTimes(t)
	if want := windows[len(windows)-1][1]; !preview.LastActivityAt.Equal(want) {
		t.Errorf("LastActivityAt = %s, want the last provider ID's %s rather than the file's %s",
			preview.LastActivityAt, want, modTime)
	}
	if preview.SizeBytes <= 0 {
		t.Errorf("SizeBytes = %d, want the store's size on disk", preview.SizeBytes)
	}
	assertNoCursorFixtureContent(t, fx, previewText(preview))
}

// TestCursorPreviewDoesNotDecodeMessageBlobs seeds a store whose root record is
// valid and whose message blobs are not. A preview reads session metadata and
// counts turns, so it must succeed here: anything that decodes a message would
// fail instead.
func TestCursorPreviewDoesNotDecodeMessageBlobs(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "ws", "0f2b19b1-3d1f-4c1a-9a1e-2f7c1b9d4e55")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	b := chatstoretest.New(t, filepath.Join(dir, "store.db"), "0f2b19b1-3d1f-4c1a-9a1e-2f7c1b9d4e55")
	b.AddPrompt("a prompt that does count")
	b.AddRawMessage([]byte("not a message at all"))
	b.AddRawMessage([]byte(`{"role":"assistant","content":`))
	b.ReferenceMissingBlob()
	path := b.Build()

	preview := cursorPreview(t, cursorImporterAt(root), path)
	if preview.SessionID != "0f2b19b1-3d1f-4c1a-9a1e-2f7c1b9d4e55" {
		t.Errorf("SessionID = %q", preview.SessionID)
	}
	if preview.TurnCount != 1 {
		t.Errorf("TurnCount = %d, want 1: the undecodable blobs are not prompts", preview.TurnCount)
	}
	if preview.StartedAt.IsZero() || preview.LastActivityAt.IsZero() {
		t.Errorf("StartedAt = %s LastActivityAt = %s, want both set", preview.StartedAt, preview.LastActivityAt)
	}
	if strings.Contains(previewText(preview), "a prompt that does count") {
		t.Error("the preview carries prompt text")
	}
}

func TestCursorPreviewSkipsASessionWithNoRootRecord(t *testing.T) {
	// Cursor writes a store as soon as a session is created. Two of the 127
	// real stores read while decoding the format were in this state.
	root := t.TempDir()
	dir := filepath.Join(root, "ws", "a4a36598-322f-46d9-a027-743c4ff717d1")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	path := chatstoretest.New(t, filepath.Join(dir, "store.db"), "a4a36598-322f-46d9-a027-743c4ff717d1").
		SetRootBlobID("").Build()

	_, ok, err := cursorImporterAt(root).Preview(context.Background(), path)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if ok {
		t.Fatal("Preview accepted a session that holds no messages")
	}
}

func TestCursorPreviewFallsBackToTheDirectorySessionID(t *testing.T) {
	// A meta row with no agentId still names the session: Cursor uses the
	// session UUID as the directory name.
	const sessionID = "c79fa22f-6abd-485a-a356-969b14185490"
	root := t.TempDir()
	dir := filepath.Join(root, "ws", sessionID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	b := chatstoretest.New(t, filepath.Join(dir, "store.db"), "")
	b.AddPrompt("hello")
	b.AddAssistantText("hi")
	path := b.Build()

	preview := cursorPreview(t, cursorImporterAt(root), path)
	if preview.SessionID != sessionID {
		t.Fatalf("SessionID = %q, want the directory name %q", preview.SessionID, sessionID)
	}
}

func TestCursorPreviewSizeCountsTheWriteAheadLog(t *testing.T) {
	// A live Cursor leaves store.db one page long with the session in the log,
	// so the database alone understates a session by orders of magnitude.
	imp, path := cursorBuild(t, "b36bf594-9429-4132-be1b-4aff4c07cb12", func(b *chatstoretest.Builder) {
		b.WAL = true
		b.AddPrompt("a prompt long enough to matter " + strings.Repeat("x", 4096))
		b.AddAssistantText("a reply")
	})

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	preview := cursorPreview(t, imp, path)
	if preview.SizeBytes <= info.Size() {
		t.Fatalf("SizeBytes = %d, want more than the %d-byte database", preview.SizeBytes, info.Size())
	}
}

// TestCursorPreviewTakesTheLastWriteFromTheLog covers the state most stores are
// in on a machine where Cursor has run: a store.db written once when the session
// was created, beside a log holding the hours of conversation that followed. Of
// 127 real stores, 72 were like this. Reading store.db's timestamp alone would
// interpolate every turn of a long session into its first seconds, sort the
// session to the wrong end of the list, and let --since drop it.
func TestCursorPreviewTakesTheLastWriteFromTheLog(t *testing.T) {
	created := time.Date(2026, 1, 20, 9, 0, 0, 0, time.UTC)
	imp, path := cursorBuild(t, "e35b6c1d-3f34-4a53-9a9c-4f1c2b3d4e5f", func(b *chatstoretest.Builder) {
		b.WAL = true
		b.Meta.CreatedAt = created.UnixMilli()
		b.AddPrompt("a question")
		b.AddAssistantText("an answer")
	})
	dbMod := created.Add(13 * time.Second)
	walMod := created.Add(3 * time.Hour)
	setCursorStoreTimes(t, path, dbMod)
	setCursorStoreTimes(t, path+"-wal", walMod)

	preview := cursorPreview(t, imp, path)
	if !preview.LastActivityAt.Equal(walMod) {
		t.Fatalf("LastActivityAt = %s, want the log's %s rather than the database's %s",
			preview.LastActivityAt, walMod, dbMod)
	}

	// Discovery decides from the same write whether Cursor is still writing the
	// session, so the skip that holds back an in-progress session has to see the
	// log too.
	d, err := Discover(context.Background(), AgentCursor, imp, DiscoverOptions{
		Now: func() time.Time { return walMod.Add(time.Minute) },
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(d.Sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(d.Sessions))
	}
	if !d.Sessions[0].Active {
		t.Error("Active = false for a session written a minute ago")
	}
}

func TestCursorTurnsPreserveMessageAndToolOrder(t *testing.T) {
	imp, sess := cursorSession(t, "tool-call.json")
	turns := collectTurns(t, imp, sess)
	if len(turns) != 2 {
		t.Fatalf("got %d turns, want 2", len(turns))
	}

	first := turns[0]
	if got := cursorPromptOf(t, first); !strings.Contains(got, "Run the tests and tell me what fails.") {
		t.Errorf("first prompt = %q", got)
	}
	// The environment preamble precedes the first prompt and the model saw it,
	// so it is part of that prompt rather than dropped.
	if got := cursorPromptOf(t, first); !strings.Contains(got, "<user_info>") {
		t.Errorf("first prompt dropped the environment preamble: %q", got)
	}
	if names := cursorToolNames(first); !slices.Equal(names, []string{"Read", "Shell"}) {
		t.Errorf("first turn tool order = %v, want [Read Shell]", names)
	}
	if names := cursorToolNames(turns[1]); !slices.Equal(names, []string{"Read"}) {
		t.Errorf("second turn tool order = %v, want [Read]", names)
	}
	if got := cursorAssistantOf(t, first); got != "One test fails: TestParseFlag." {
		t.Errorf("first assistant text = %q", got)
	}
	if first.Gen.ThinkingEnabled == nil || !*first.Gen.ThinkingEnabled {
		t.Error("ThinkingEnabled is not set for a turn that holds a reasoning part")
	}
	// The second turn has no reasoning part of its own.
	if turns[1].Gen.ThinkingEnabled != nil {
		t.Error("ThinkingEnabled leaked from the first turn into the second")
	}
	if turns[0].Gen.ID == turns[1].Gen.ID {
		t.Error("both turns got the same generation ID")
	}
}

func TestCursorTurnsCarryToolArgumentsAndResults(t *testing.T) {
	imp, sess := cursorSession(t, "tool-call.json")
	turns := collectTurns(t, imp, sess)

	tools := cursorTools(turns[0])
	if len(tools) == 0 {
		t.Fatal("the first turn holds no tool call")
	}
	call, result := tools[0].call, tools[0].result
	if call.Name != "Read" {
		t.Fatalf("first tool call = %q, want Read", call.Name)
	}
	if !strings.Contains(string(call.InputJSON), "/work/repo/Makefile") {
		t.Errorf("tool call input = %s", call.InputJSON)
	}
	if !strings.Contains(string(result.ContentJSON), "go test ./...") {
		t.Errorf("tool result = %s", result.ContentJSON)
	}
	if result.ToolCallID != call.ID {
		t.Errorf("result ToolCallID = %q, want the call's %q", result.ToolCallID, call.ID)
	}
}

// TestCursorTurnsJoinTheAssistantTextItWrote covers an assistant that wrote,
// called a tool, then wrote again. Cursor stores each of those messages
// separately, and the live mapper concatenates a fragment's assistant segments
// with no separator, because in the live path a segment is a streaming delta.
// The importer therefore has to join the messages itself, or the reply exports
// as "...the Makefile.One test fails...".
func TestCursorTurnsJoinTheAssistantTextItWrote(t *testing.T) {
	imp, path := cursorBuild(t, "a1d0c6e8-3f34-4a53-9a9c-4f1c2b3d4e5f", func(b *chatstoretest.Builder) {
		b.AddPrompt("run the tests")
		b.AddAssistantText("Checking the Makefile.")
		b.AddToolCall("Shell", "call-1", `{"command":"go test ./..."}`)
		b.AddToolResult("Shell", "call-1", `"FAIL"`)
		b.AddAssistantText("One test fails.")
	})

	turns := collectTurns(t, imp, cursorPreview(t, imp, path))
	if len(turns) != 1 {
		t.Fatalf("got %d turns, want 1", len(turns))
	}
	if got, want := cursorAssistantOf(t, turns[0]), "Checking the Makefile.\n\nOne test fails."; got != want {
		t.Fatalf("assistant text = %q, want %q", got, want)
	}
}

// TestCursorTurnsGiveAPreambleToThePromptAfterIt covers context Cursor prepends
// mid-session. A user message with string content is something the agent added,
// and the model saw it as part of the prompt that follows it. Attaching it to
// the turn that is already open would export one turn's question carrying the
// next turn's context, and leave the next turn without it.
func TestCursorTurnsGiveAPreambleToThePromptAfterIt(t *testing.T) {
	imp, path := cursorBuild(t, "f0e1d2c3-3f34-4a53-9a9c-4f1c2b3d4e5f", func(b *chatstoretest.Builder) {
		b.AddPrompt("the first question")
		b.AddAssistantText("the first answer")
		b.AddPreamble("<git_status>branch main</git_status>")
		b.AddPrompt("the second question")
		b.AddAssistantText("the second answer")
	})

	turns := collectTurns(t, imp, cursorPreview(t, imp, path))
	if len(turns) != 2 {
		t.Fatalf("got %d turns, want 2", len(turns))
	}
	if got := cursorPromptOf(t, turns[0]); got != "the first question" {
		t.Errorf("first prompt = %q, want the context to stay out of it", got)
	}
	if got, want := cursorPromptOf(t, turns[1]), "<git_status>branch main</git_status>\n\nthe second question"; got != want {
		t.Errorf("second prompt = %q, want %q", got, want)
	}
}

// TestCursorTurnsPairEachResultWithItsCall covers the two ways Cursor's own IDs
// stop identifying a call. A turn spans a whole agentic run, so one ID can be
// used more than once inside it, and a result must reach the call it answers
// rather than overwriting an earlier one.
func TestCursorTurnsPairEachResultWithItsCall(t *testing.T) {
	type pair struct{ name, input, output string }
	tests := []struct {
		name       string
		setup      func(b *chatstoretest.Builder)
		want       []pair
		wantOrphan bool
	}{
		{
			name: "two calls under one ID",
			setup: func(b *chatstoretest.Builder) {
				b.AddToolCall("Read", "call-1", `{"path":"/a"}`)
				b.AddToolResult("Read", "call-1", `"first file"`)
				b.AddToolCall("Read", "call-1", `{"path":"/b"}`)
				b.AddToolResult("Read", "call-1", `"second file"`)
			},
			want: []pair{
				{name: "Read", input: `{"path":"/a"}`, output: `"first file"`},
				{name: "Read", input: `{"path":"/b"}`, output: `"second file"`},
			},
		},
		{
			// A call with no ID is paired by position instead, and its result
			// must reach it rather than be recorded as a second, half-empty
			// tool record.
			name: "a call and a result that carry no ID",
			setup: func(b *chatstoretest.Builder) {
				b.AddToolCall("Read", "", `{"path":"/a"}`)
				b.AddToolResult("Read", "", `"first file"`)
			},
			want: []pair{{name: "Read", input: `{"path":"/a"}`, output: `"first file"`}},
		},
		{
			name: "a result whose call is not in the turn",
			setup: func(b *chatstoretest.Builder) {
				b.AddToolResult("Read", "call-orphan", `"contents"`)
			},
			want:       []pair{{name: "Read", output: `"contents"`}},
			wantOrphan: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			imp, path := cursorBuild(t, "sess-tools", func(b *chatstoretest.Builder) {
				b.AddPrompt("use the tools")
				tt.setup(b)
				b.AddAssistantText("done")
			})

			turns := collectTurns(t, imp, cursorPreview(t, imp, path))
			if len(turns) != 1 {
				t.Fatalf("got %d turns, want 1", len(turns))
			}
			var got []pair
			for _, tool := range cursorTools(turns[0]) {
				got = append(got, pair{
					name:   tool.call.Name,
					input:  string(tool.call.InputJSON),
					output: string(tool.result.ContentJSON),
				})
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("tools = %+v, want %+v", got, tt.want)
			}
			if orphan := slices.Contains(turns[0].Quality.Notes, cursorNoteOrphanToolResult); orphan != tt.wantOrphan {
				t.Errorf("one-sided pairing recorded = %v, want %v: %v",
					orphan, tt.wantOrphan, turns[0].Quality.Notes)
			}
		})
	}
}

// TestCursorTurnsReportAMessageItCannotRead covers a blob the store cannot
// produce. The lost message may have held a prompt, so what follows it cannot be
// attributed: folding it into the turn before would export one turn's output as
// the answer to another turn's question, with nothing in the generation saying
// so. The turn ends at the gap instead, both sides carry a note, and the session
// reports what it lost.
func TestCursorTurnsReportAMessageItCannotRead(t *testing.T) {
	imp, path := cursorBuild(t, "b9d1f0a2-3f34-4a53-9a9c-4f1c2b3d4e5f", func(b *chatstoretest.Builder) {
		b.AddPrompt("the first question")
		b.AddAssistantText("the first answer")
		b.ReferenceMissingBlob() // where a prompt was
		b.AddAssistantText("the answer to the question that is gone")
		b.AddPrompt("the second question")
		b.AddAssistantText("the second answer")
	})

	turns, problems := cursorWalk(t, imp, cursorPreview(t, imp, path))
	if len(turns) != 2 {
		t.Fatalf("got %d turns, want the two whose prompts survived", len(turns))
	}
	if got := cursorAssistantOf(t, turns[0]); got != "the first answer" {
		t.Errorf("first assistant text = %q, want the orphaned answer left out of it", got)
	}
	if got := cursorPromptOf(t, turns[1]); got != "the second question" {
		t.Errorf("second prompt = %q", got)
	}
	for i, turn := range turns {
		if !slices.Contains(turn.Quality.Notes, cursorNoteUnreadableMessage) {
			t.Errorf("turn %d notes = %v, want the lost message recorded", i, turn.Quality.Notes)
		}
	}
	if len(problems) != 1 || !strings.Contains(problems[0], "1 messages could not be read") {
		t.Errorf("problems = %v, want one naming the message it lost", problems)
	}
}

func TestCursorTurnsAreDeterministic(t *testing.T) {
	root := t.TempDir()
	fx := loadCursorFixture(t, "tool-call.json")
	path := writeCursorStore(t, root, fx)
	setCursorStoreTimes(t, path, time.UnixMilli(fx.CreatedAtMs).UTC().Add(10*time.Minute))

	imp := cursorImporterAt(root)
	sess := cursorPreview(t, imp, path)
	first := collectTurns(t, imp, sess)
	second := collectTurns(t, cursorImporterAt(root), cursorPreview(t, cursorImporterAt(root), path))

	if len(first) != len(second) {
		t.Fatalf("turn counts differ: %d then %d", len(first), len(second))
	}
	for i := range first {
		if a, b := first[i].Gen.ID, second[i].Gen.ID; a != b {
			t.Errorf("turn %d generation ID changed between reads: %q then %q", i, a, b)
		}
		if a, b := first[i].Gen.StartedAt, second[i].Gen.StartedAt; !a.Equal(b) {
			t.Errorf("turn %d inferred start time changed between reads: %s then %s", i, a, b)
		}
		if !reflect.DeepEqual(first[i].Gen, second[i].Gen) {
			t.Errorf("turn %d differs between reads", i)
		}
	}
}

// TestCursorTurnMatchesTheLiveMapper pins each importer field to the live Cursor
// mapping, for a turn with no tools and for one with two. If it drifts, an
// imported turn and a live one stop agreeing. The expected fragment is spelled
// out rather than derived, so a change in how the importer groups messages shows
// up here as a diff instead of moving both sides at once.
func TestCursorTurnMatchesTheLiveMapper(t *testing.T) {
	tests := []struct {
		name     string
		fixture  string
		want     func(t *testing.T, fx cursorFixture) *fragment.Fragment
		wantTurn int
	}{
		{
			name:     "a turn with no tools",
			fixture:  "two-turn.json",
			wantTurn: 2,
			want: func(t *testing.T, fx cursorFixture) *fragment.Fragment {
				return &fragment.Fragment{
					// The environment preamble precedes the first prompt and the
					// model saw it, so it is part of that prompt.
					UserPrompt:      fx.nth(t, "preamble", 0).Text + "\n\n" + fx.nth(t, "prompt", 0).Text,
					ThinkingPresent: true,
					Assistant:       []fragment.AssistantSegment{{Text: fx.nth(t, "assistant", 0).Text}},
				}
			},
		},
		{
			name:     "a turn with two tool calls",
			fixture:  "tool-call.json",
			wantTurn: 2,
			want: func(t *testing.T, fx cursorFixture) *fragment.Fragment {
				tool := func(n int) fragment.ToolRecord {
					call, result := fx.nth(t, "toolCall", n), fx.nth(t, "toolResult", n)
					if call.ToolCallID != result.ToolCallID {
						t.Fatalf("fixture call %d and result %d name different calls", n, n)
					}
					return fragment.ToolRecord{
						ToolName:   call.ToolName,
						ToolUseID:  call.ToolCallID,
						ToolInput:  call.Args,
						ToolOutput: result.Result,
						Status:     "completed",
					}
				}
				return &fragment.Fragment{
					UserPrompt:      fx.nth(t, "preamble", 0).Text + "\n\n" + fx.nth(t, "prompt", 0).Text,
					ThinkingPresent: true,
					Assistant:       []fragment.AssistantSegment{{Text: fx.nth(t, "assistant", 0).Text}},
					Tools:           []fragment.ToolRecord{tool(0), tool(1)},
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			fx := loadCursorFixture(t, tt.fixture)
			path := writeCursorStore(t, root, fx)
			start := time.UnixMilli(fx.CreatedAtMs).UTC()
			setCursorStoreTimes(t, path, start.Add(10*time.Minute))

			imp := cursorImporterAt(root)
			turns := collectTurns(t, imp, cursorPreview(t, imp, path))
			if len(turns) != tt.wantTurn {
				t.Fatalf("got %d turns, want %d", len(turns), tt.wantTurn)
			}
			turn := turns[0]

			// A fixture whose IDs carry times is dated from them. One whose IDs
			// do not takes half of the ten-minute span, because both fixtures
			// hold two turns.
			end := start.Add(5 * time.Minute)
			if window := fx.turnTimes(t)[0]; !window[0].IsZero() {
				start, end = window[0], window[1]
			}
			frag := tt.want(t, fx)
			frag.ConversationID = fx.SessionID
			frag.GenerationID = "history-turn-000000"
			frag.Model = fx.LastUsedModel
			frag.StartedAt = start.Format(time.RFC3339Nano)
			frag.LastEventAt = end.Format(time.RFC3339Nano)

			want := mapper.MapFragment(mapper.Inputs{
				Fragment: frag,
				Session: &fragment.Session{
					ConversationID:    fx.SessionID,
					ConversationTitle: fx.Name,
					WorkspaceRoots:    []string{"/work/repo"},
					StartedAt:         start.Format(time.RFC3339Nano),
				},
				Stop:           &mapper.StopInput{Status: string(mapper.StopStatusCompleted)},
				ContentCapture: agento11y.ContentCaptureModeFull,
				Now:            time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC),
			}).Generation
			// The importer stamps its own deterministic ID; everything else must
			// be the mapper's output.
			want.ID = turn.Gen.ID
			if !reflect.DeepEqual(turn.Gen, want) {
				t.Fatalf("the imported turn differs from mapper.MapFragment:\n got %+v\nwant %+v", turn.Gen, want)
			}
		})
	}
}

func TestCursorTurnQuality(t *testing.T) {
	tests := []struct {
		name  string
		setup func(b *chatstoretest.Builder)
		check func(t *testing.T, turn HistoricalGeneration)
	}{
		{
			// The store records no per-turn timestamp and no per-turn token
			// count anywhere, so every imported turn approximates both.
			name: "usage and start time are always approximate",
			setup: func(b *chatstoretest.Builder) {
				b.AddPrompt("hello")
				b.AddAssistantText("hi")
			},
			check: func(t *testing.T, turn HistoricalGeneration) {
				if !turn.Quality.ApproxUsage {
					t.Error("ApproxUsage = false, want true: the store holds no per-turn usage")
				}
				if !turn.Quality.ApproxStartedAt || !turn.Quality.ApproxCompletedAt {
					t.Error("the turn's inferred times are not flagged")
				}
				if turn.Gen.Usage != (agento11y.TokenUsage{}) {
					t.Errorf("Usage = %+v, want zero rather than an invented figure", turn.Gen.Usage)
				}
			},
		},
		{
			name: "a session that names no model",
			setup: func(b *chatstoretest.Builder) {
				b.Meta.LastUsedModel = ""
				b.AddPrompt("hello")
				b.AddAssistantText("hi")
			},
			check: func(t *testing.T, turn HistoricalGeneration) {
				if !turn.Quality.MissingModel {
					t.Error("MissingModel = false, want true")
				}
			},
		},
		{
			name: "a session that names a model",
			setup: func(b *chatstoretest.Builder) {
				b.Meta.LastUsedModel = "gemini-3-pro"
				b.AddPrompt("hello")
				b.AddAssistantText("hi")
			},
			check: func(t *testing.T, turn HistoricalGeneration) {
				if turn.Quality.MissingModel {
					t.Error("MissingModel = true for a session that names one")
				}
				if turn.Gen.Model.Name != "gemini-3-pro" {
					t.Errorf("Model.Name = %q, want gemini-3-pro", turn.Gen.Model.Name)
				}
			},
		},
		{
			// No message carries a turn ID, and the per-turn records that do are
			// absent from most stores, so every turn uses the framework's
			// ordinal ID and says so.
			name: "every turn records that it has no source turn ID",
			setup: func(b *chatstoretest.Builder) {
				b.AddPrompt("hello")
				b.AddAssistantText("hi")
			},
			check: func(t *testing.T, turn HistoricalGeneration) {
				if !slices.Contains(turn.Quality.Notes, cursorNoteMissingTurnID) {
					t.Errorf("Notes = %v, want the missing turn ID recorded", turn.Quality.Notes)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			imp, path := cursorBuild(t, "sess-quality", tt.setup)
			turns := collectTurns(t, imp, cursorPreview(t, imp, path))
			if len(turns) != 1 {
				t.Fatalf("got %d turns, want 1", len(turns))
			}
			tt.check(t, turns[0])
		})
	}
}

// TestCursorTurnsDropATurnWithNoModelOutput covers a prompt nothing answered.
// Cursor writes the message list as it streams, so an interrupted session ends
// with a prompt and nothing after it, and exporting that would be a generation
// the model never produced. A session where that is the only prompt is
// indistinguishable from a format change, so it reports one.
func TestCursorTurnsDropATurnWithNoModelOutput(t *testing.T) {
	tests := []struct {
		name         string
		setup        func(b *chatstoretest.Builder)
		wantPrompts  []string
		wantProblems []string
	}{
		{
			name: "the last prompt of an interrupted session",
			setup: func(b *chatstoretest.Builder) {
				b.AddPrompt("answered")
				b.AddAssistantText("an answer")
				b.AddPrompt("never answered")
			},
			wantPrompts: []string{"answered"},
		},
		{
			// Cursor holds the reply it is still streaming in root field 4 and
			// appends it to the message list when it finishes, so a session
			// interrupted before that lists a prompt and no answer. One real store
			// on the machine this was written on is in exactly that state.
			// Reporting it would fail every later import of the same store, and no
			// rerun could fix it.
			name: "a session interrupted before its only answer was listed",
			setup: func(b *chatstoretest.Builder) {
				b.AddSystem("you are cursor")
				b.AddPreamble("<user_info>darwin</user_info>")
				b.AddPrompt("never answered")
			},
		},
		{
			// The preview counted this prompt in SQLite and the walk decoded the
			// same messages in Go, so the two disagreeing over a message the walk
			// cannot place is what a change to Cursor's format looks like from
			// here. Without the warning the run reports importing nothing and gives
			// no reason, which is what an empty history looks like too.
			name: "a session whose messages cannot be placed in a turn",
			setup: func(b *chatstoretest.Builder) {
				b.AddRawMessage([]byte(`{"role":"chatbot","content":[{"type":"text","text":"an answer"}]}`))
				b.AddPrompt("never answered")
			},
			wantProblems: []string{"no turn was rebuilt from 1 prompts, and 1 messages could not be placed"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			imp, path := cursorBuild(t, "sess-interrupted", tt.setup)

			sess := cursorPreview(t, imp, path)
			turns, problems := cursorWalk(t, imp, sess)
			var prompts []string
			for _, turn := range turns {
				prompts = append(prompts, cursorPromptOf(t, turn))
			}
			if !slices.Equal(prompts, tt.wantPrompts) {
				t.Errorf("prompts = %q, want %q", prompts, tt.wantPrompts)
			}
			if len(problems) != len(tt.wantProblems) {
				t.Fatalf("problems = %v, want %d", problems, len(tt.wantProblems))
			}
			for i, want := range tt.wantProblems {
				if !strings.Contains(problems[i], want) {
					t.Errorf("problem %d = %q, want it to say %q", i, problems[i], want)
				}
			}
		})
	}
}

// TestCursorUnwrapsThePrompt covers the wrapper Cursor puts around a prompt
// before it sends it. The live hook reads what the user typed, so an imported
// turn has to show the same thing rather than the model's copy of it.
func TestCursorUnwrapsThePrompt(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "the shape 366 of 386 real prompts have",
			in:   "<user_query>\nrun the tests\n</user_query>",
			want: "run the tests",
		},
		{
			// Cursor puts an attachment ahead of the wrapper. It is context it
			// added, like the environment block, so it stays.
			name: "an attachment before the wrapper is kept",
			in:   "<attached_files>\nplan.md\n</attached_files>\n<user_query>execute the plan</user_query>",
			want: "<attached_files>\nplan.md\n</attached_files>\n\nexecute the plan",
		},
		{
			name: "a prompt with no wrapper is untouched",
			in:   "run the tests",
			want: "run the tests",
		},
		{
			// A prompt quoting the tag, which is one this session could produce.
			// Only the outermost pair goes, so the quoted text survives.
			name: "a prompt that quotes the tag",
			in:   "<user_query>why is &lt;user_query&gt; in <user_query>my prompt</user_query>?</user_query>",
			want: "why is &lt;user_query&gt; in <user_query>my prompt</user_query>?",
		},
		{
			name: "a closing tag with no opening one",
			in:   "run the tests</user_query>",
			want: "run the tests</user_query>",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cursorUnwrapPrompt(tt.in); got != tt.want {
				t.Errorf("cursorUnwrapPrompt() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestCursorImportedPromptMatchesTheTypedOne walks the wrapper through a real
// store, with the environment block Cursor sends beside it.
func TestCursorImportedPromptMatchesTheTypedOne(t *testing.T) {
	imp, path := cursorBuild(t, "9f1e2d3c-4b5a-4968-8778-6f5e4d3c2b1a", func(b *chatstoretest.Builder) {
		b.AddSystem("you are cursor")
		b.AddPreamble("<user_info>darwin</user_info>")
		b.AddPrompt("<user_query>\nrun the tests\n</user_query>")
		b.AddAssistantText("one test fails")
	})

	turns := collectTurns(t, imp, cursorPreview(t, imp, path))
	if len(turns) != 1 {
		t.Fatalf("got %d turns, want 1", len(turns))
	}
	got := cursorPromptOf(t, turns[0])
	want := "<user_info>darwin</user_info>\n\nrun the tests"
	if got != want {
		t.Errorf("prompt = %q, want %q", got, want)
	}
}

// TestCursorTurnsAreDatedFromProviderIDs covers the source a turn's times
// actually come from. Cursor timestamps the session and nothing in it, but the
// IDs the provider issued are in the store and some providers put the issue time
// inside the ID, so a turn holding one is measured rather than guessed.
func TestCursorTurnsAreDatedFromProviderIDs(t *testing.T) {
	created := time.Date(2026, 1, 12, 8, 30, 0, 0, time.UTC)
	firstReasoning := created.Add(20 * time.Second)
	firstCall := created.Add(35 * time.Second)
	thirdCall := created.Add(11 * time.Minute)

	imp, path := cursorBuild(t, "6d0f5f4e-2b1a-4c3d-8e9f-0a1b2c3d4e5f", func(b *chatstoretest.Builder) {
		b.Meta.CreatedAt = created.UnixMilli()
		b.AddPrompt("the first question")
		b.AddAssistantReasoningID("**thinking**", cursorNewID("rs", firstReasoning))
		b.AddToolCall("Read", "call_quk1E4T6bsSwIEOeoUeuavFk\n"+cursorNewID("fc", firstCall), `{"path":"/a"}`)
		b.AddToolResult("Read", "call_quk1E4T6bsSwIEOeoUeuavFk\n"+cursorNewID("fc", firstCall), `"a"`)
		b.AddAssistantText("the first answer")

		// A turn whose model issues IDs with no time in them, in a session
		// whose other turns carry one.
		b.AddPrompt("the second question")
		b.AddToolCall("Read", "toolu_01Q9x7cjWCXN7c9v49MAxbiT", `{"path":"/b"}`)
		b.AddAssistantText("the second answer")

		b.AddPrompt("the third question")
		b.AddToolCall("Read", "call_Kj3mNp7QrStUvWxYzAbCdEfG\n"+cursorNewID("fc", thirdCall), `{"path":"/c"}`)
		b.AddAssistantText("the third answer")
	})
	// The store was opened long after the session ran, which is what a
	// checkpoint leaves behind. None of the turns may take its time from that.
	setCursorStoreTimes(t, path, created.Add(200*24*time.Hour))

	preview := cursorPreview(t, imp, path)
	if !preview.LastActivityAt.Equal(thirdCall) {
		t.Errorf("LastActivityAt = %s, want the last provider ID's %s", preview.LastActivityAt, thirdCall)
	}

	turns := collectTurns(t, imp, preview)
	if len(turns) != 3 {
		t.Fatalf("got %d turns, want 3", len(turns))
	}
	if !turns[0].Gen.StartedAt.Equal(firstReasoning) || !turns[0].Gen.CompletedAt.Equal(firstCall) {
		t.Errorf("turn 0 ran %s to %s, want %s to %s",
			turns[0].Gen.StartedAt, turns[0].Gen.CompletedAt, firstReasoning, firstCall)
	}
	if !turns[2].Gen.StartedAt.Equal(thirdCall) || !turns[2].Gen.CompletedAt.Equal(thirdCall) {
		t.Errorf("turn 2 ran %s to %s, want both at %s",
			turns[2].Gen.StartedAt, turns[2].Gen.CompletedAt, thirdCall)
	}
	// The undated turn falls back to a share of the session's span, which says
	// nothing about where in the session it sits, so all it has to do is stay
	// between the turns around it.
	if turns[1].Gen.StartedAt.Before(turns[0].Gen.CompletedAt) || turns[1].Gen.CompletedAt.After(turns[2].Gen.StartedAt) {
		t.Errorf("the undated turn ran %s to %s, outside the turns around it (%s and %s)",
			turns[1].Gen.StartedAt, turns[1].Gen.CompletedAt, turns[0].Gen.CompletedAt, turns[2].Gen.StartedAt)
	}
	for i, turn := range turns {
		wantInterpolated := i == 1
		if got := slices.Contains(turn.Quality.Notes, cursorNoteInterpolatedTimes); got != wantInterpolated {
			t.Errorf("turn %d interpolated-times note = %v, want %v", i, got, wantInterpolated)
		}
		if !turn.Quality.ApproxStartedAt || !turn.Quality.ApproxCompletedAt {
			t.Errorf("turn %d is not flagged approximate: a provider ID dates it to the second, "+
				"on the provider's clock, from the first item after the prompt", i)
		}
	}
}

// TestCursorPreviewDistrustsAFileTime covers the modification times a store
// carries once anything has opened it since. Cursor checkpoints a session's log
// when it next opens the database, which re-dates the file to that moment: of
// 127 real stores, 45 had an emptied log stamped months after the session ran,
// 42 of them to the same second.
func TestCursorPreviewDistrustsAFileTime(t *testing.T) {
	created := time.Date(2026, 1, 12, 8, 30, 0, 0, time.UTC)
	tests := []struct {
		name     string
		emptyWAL bool
		dbMod    time.Time
		walMod   time.Time
		want     time.Time
	}{
		{
			name:  "a file written while the session ran dates it",
			dbMod: created.Add(20 * time.Minute),
			want:  created.Add(20 * time.Minute),
		},
		{
			name:  "a file written months later says nothing about the session",
			dbMod: created.Add(264 * 24 * time.Hour),
			want:  created,
		},
		{
			// The same batch write reaches store.db, not only the log: three of
			// 127 real stores were created in the same millisecond and written in
			// the same second sixteen hours later, which gave each of them one
			// sixteen-hour turn.
			name:  "a file written overnight is a checkpoint, not the session's end",
			dbMod: created.Add(16 * time.Hour),
			want:  created,
		},
		{
			// A checkpoint writes the log's frames into the database and
			// truncates it, so an empty log holds none of the session and its
			// time is the checkpoint's.
			name:     "an emptied log is not a record of a write",
			emptyWAL: true,
			dbMod:    created.Add(5 * time.Minute),
			walMod:   created.Add(200 * 24 * time.Hour),
			want:     created.Add(5 * time.Minute),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			imp, path := cursorBuild(t, "b1c2d3e4-5f60-4718-8293-a4b5c6d7e8f9", func(b *chatstoretest.Builder) {
				b.Meta.CreatedAt = created.UnixMilli()
				b.Meta.LastUsedModel = "claude-4-opus"
				b.AddPrompt("a question")
				b.AddToolCall("Read", "toolu_01Q9x7cjWCXN7c9v49MAxbiT", `{"path":"/a"}`)
				b.AddAssistantText("an answer")
			})
			setCursorStoreTimes(t, path, tt.dbMod)
			if tt.emptyWAL {
				if err := os.WriteFile(path+"-wal", nil, 0o600); err != nil {
					t.Fatal(err)
				}
				setCursorStoreTimes(t, path+"-wal", tt.walMod)
			}

			preview := cursorPreview(t, imp, path)
			if !preview.LastActivityAt.Equal(tt.want) {
				t.Errorf("LastActivityAt = %s, want %s", preview.LastActivityAt, tt.want)
			}
			if preview.LastActivityAt.Before(preview.StartedAt) {
				t.Errorf("the session ends at %s, before it starts at %s",
					preview.LastActivityAt, preview.StartedAt)
			}
		})
	}
}

// TestCursorTurnWindows covers the three shapes the fallback span can have, for
// the sessions whose provider issues IDs with no time in them. Their turns are
// interpolated across the session's span: deterministic, in order, and a guess,
// which is why every turn is flagged.
func TestCursorTurnWindows(t *testing.T) {
	start := time.Date(2026, 1, 12, 8, 30, 0, 0, time.UTC)
	tests := []struct {
		name       string
		createdAt  int64
		mod        time.Time
		wantStarts []time.Time
		wantEnd    time.Time
	}{
		{
			name:       "the session's span is divided over its turns",
			createdAt:  start.UnixMilli(),
			mod:        start.Add(10 * time.Minute),
			wantStarts: []time.Time{start, start.Add(5 * time.Minute)},
			wantEnd:    start.Add(10 * time.Minute),
		},
		{
			// A session written in under a millisecond, or one whose file was
			// touched before its createdAt, has no span to divide. The turns are
			// then spaced by [cursorNominalStep], which is not a duration: two
			// turns sharing an instant would leave the order they are shown in to
			// whatever reads them.
			name:       "a file older than the session start leaves no span",
			createdAt:  start.UnixMilli(),
			mod:        start.Add(-time.Hour),
			wantStarts: []time.Time{start, start.Add(cursorNominalStep)},
			wantEnd:    start.Add(2 * cursorNominalStep),
		},
		{
			name:       "a store with no createdAt falls back to the file's time",
			createdAt:  0,
			mod:        start.Add(time.Hour),
			wantStarts: []time.Time{start.Add(time.Hour), start.Add(time.Hour + cursorNominalStep)},
			wantEnd:    start.Add(time.Hour + 2*cursorNominalStep),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			imp, path := cursorBuild(t, "sess-windows", func(b *chatstoretest.Builder) {
				b.Meta.CreatedAt = tt.createdAt
				b.AddPrompt("the first question")
				b.AddAssistantText("the first answer")
				b.AddPrompt("the second question")
				b.AddAssistantText("the second answer")
			})
			setCursorStoreTimes(t, path, tt.mod)

			turns := collectTurns(t, imp, cursorPreview(t, imp, path))
			if len(turns) != len(tt.wantStarts) {
				t.Fatalf("got %d turns, want %d", len(turns), len(tt.wantStarts))
			}
			for i, turn := range turns {
				if !turn.Gen.StartedAt.Equal(tt.wantStarts[i]) {
					t.Errorf("turn %d StartedAt = %s, want %s", i, turn.Gen.StartedAt, tt.wantStarts[i])
				}
				if !turn.Quality.ApproxStartedAt || !turn.Quality.ApproxCompletedAt {
					t.Errorf("turn %d carries interpolated times that are not flagged", i)
				}
			}
			if !turns[0].Gen.CompletedAt.Equal(turns[1].Gen.StartedAt) {
				t.Errorf("the windows do not meet: %s then %s", turns[0].Gen.CompletedAt, turns[1].Gen.StartedAt)
			}
			if !turns[1].Gen.CompletedAt.Equal(tt.wantEnd) {
				t.Errorf("last CompletedAt = %s, want the session end %s", turns[1].Gen.CompletedAt, tt.wantEnd)
			}
		})
	}
}

func TestCursorTurnsStopWhenTheConsumerStops(t *testing.T) {
	imp, sess := cursorSession(t, "tool-call.json")
	seen := 0
	for range imp.Turns(context.Background(), sess) {
		seen++
		break
	}
	if seen != 1 {
		t.Fatalf("read %d turns after breaking at 1", seen)
	}
}

func TestCursorTurnsReportACancelledContext(t *testing.T) {
	imp, sess := cursorSession(t, "two-turn.json")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, err := range imp.Turns(ctx, sess) {
		if err == nil {
			t.Fatal("Turns yielded a turn for a cancelled context")
		}
		return
	}
	t.Fatal("Turns yielded nothing for a cancelled context")
}

func TestCursorTurnsReportAnUnreadableStore(t *testing.T) {
	imp, sess := cursorSession(t, "two-turn.json")
	sess.SourcePath = filepath.Join(t.TempDir(), "gone", "store.db")

	for _, err := range imp.Turns(context.Background(), sess) {
		if err == nil {
			t.Fatal("Turns yielded a turn for a store that cannot be opened")
		}
		return
	}
	t.Fatal("Turns yielded nothing for a store that cannot be opened")
}

// TestCursorDiscoverSurvivesADamagedStore puts a truncated store beside a valid
// one. Discovery must report the valid session and name the broken path, not
// abort the whole scan.
func TestCursorDiscoverSurvivesADamagedStore(t *testing.T) {
	root := t.TempDir()
	fx := loadCursorFixture(t, "two-turn.json")
	good := writeCursorStore(t, root, fx)

	brokenDir := filepath.Join(root, "ws-broken", "c79fa22f-6abd-485a-a356-969b14185490")
	if err := os.MkdirAll(brokenDir, 0o750); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(brokenDir, "store.db")
	// A real SQLite header with the rest of the file cut off.
	if err := os.WriteFile(broken, []byte("SQLite format 3\x00 and then nothing"), 0o600); err != nil {
		t.Fatal(err)
	}

	imp := cursorImporterAt(root)
	d, err := Discover(context.Background(), AgentCursor, imp, DiscoverOptions{
		Now: func() time.Time { return time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(d.Sessions) != 1 {
		t.Fatalf("got %d sessions, want the one valid store", len(d.Sessions))
	}
	if d.Sessions[0].SourcePath != good {
		t.Errorf("SourcePath = %q, want %q", d.Sessions[0].SourcePath, good)
	}
	if len(d.Warnings) != 1 {
		t.Fatalf("got %d warnings, want one: %v", len(d.Warnings), d.Warnings)
	}
	if !strings.Contains(d.Warnings[0], broken) {
		t.Errorf("warning = %q, want it to name %q", d.Warnings[0], broken)
	}
}

func TestCursorDiscoverIgnoresTheSQLiteSiblings(t *testing.T) {
	root := t.TempDir()
	fx := loadCursorFixture(t, "two-turn.json")
	path := writeCursorStore(t, root, fx)
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.WriteFile(path+suffix, []byte("sidecar"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	d, err := Discover(context.Background(), AgentCursor, cursorImporterAt(root), DiscoverOptions{})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(d.Sessions) != 1 {
		t.Fatalf("got %d sessions, want one: the siblings must not be previewed", len(d.Sessions))
	}
	if len(d.Warnings) != 0 {
		t.Fatalf("warnings = %v, want none", d.Warnings)
	}
}

func TestCursorIsRegistered(t *testing.T) {
	imp, ok := NewImporter(AgentCursor)
	if !ok || imp == nil {
		t.Fatal("no importer registered for cursor")
	}
	for _, name := range []string{"cursor", "Cursor", "cursor-agent"} {
		got, ok := Resolve(name)
		if !ok || got != AgentCursor {
			t.Errorf("Resolve(%q) = %q, %v", name, got, ok)
		}
	}
	spec, ok := Spec(AgentCursor)
	if !ok || spec.DisplayName != "Cursor" {
		t.Errorf("Spec = %+v, ok=%v", spec, ok)
	}
}

// previewText joins every string a preview carries, so a test can assert that
// none of them holds session content.
func previewText(p SessionPreview) string {
	return strings.Join([]string{string(p.Agent), p.SessionID, p.Title, p.Workspace, p.SourcePath}, "\x00")
}

func assertNoCursorFixtureContent(t *testing.T, fx cursorFixture, text string) {
	t.Helper()
	for _, m := range fx.Messages {
		if m.Text != "" && strings.Contains(text, m.Text) {
			t.Errorf("the preview carries message text: %q", m.Text)
		}
		if len(m.Result) > 0 && strings.Contains(text, string(m.Result)) {
			t.Errorf("the preview carries a tool result: %s", m.Result)
		}
	}
}

func cursorPromptOf(t *testing.T, turn HistoricalGeneration) string {
	t.Helper()
	for _, msg := range turn.Gen.Input {
		if msg.Role != agento11y.RoleUser {
			continue
		}
		for _, p := range msg.Parts {
			if p.Kind == agento11y.PartKindText {
				return p.Text
			}
		}
	}
	return ""
}

func cursorAssistantOf(t *testing.T, turn HistoricalGeneration) string {
	t.Helper()
	for _, msg := range turn.Gen.Output {
		if msg.Role != agento11y.RoleAssistant {
			continue
		}
		for _, p := range msg.Parts {
			if p.Kind == agento11y.PartKindText {
				return p.Text
			}
		}
	}
	return ""
}

// cursorToolNames lists the tool calls in the order the generation holds them.
func cursorToolNames(turn HistoricalGeneration) []string {
	var out []string
	for _, tool := range cursorTools(turn) {
		out = append(out, tool.call.Name)
	}
	return out
}

// cursorTool is one tool call with the result that answers it.
type cursorTool struct {
	call   agento11y.ToolCall
	result agento11y.ToolResult
}

// cursorTools zips a generation's tool calls with its tool results. The mapper
// emits one call and one result per tool record, in order, so the pairing is
// positional: Cursor can reuse one call ID for two calls in the same turn, and
// then the ID identifies nothing.
func cursorTools(turn HistoricalGeneration) []cursorTool {
	var results []agento11y.ToolResult
	for _, msg := range turn.Gen.Input {
		for _, p := range msg.Parts {
			if p.Kind == agento11y.PartKindToolResult && p.ToolResult != nil {
				results = append(results, *p.ToolResult)
			}
		}
	}
	var out []cursorTool
	for _, msg := range turn.Gen.Output {
		for _, p := range msg.Parts {
			if p.Kind != agento11y.PartKindToolCall || p.ToolCall == nil {
				continue
			}
			tool := cursorTool{call: *p.ToolCall}
			if len(out) < len(results) {
				tool.result = results[len(out)]
			}
			out = append(out, tool)
		}
	}
	return out
}
