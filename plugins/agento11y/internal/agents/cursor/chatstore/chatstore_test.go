package chatstore_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/chatstore"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/chatstore/chatstoretest"
)

const (
	testSessionID = "7506c5c9-d47c-4b6f-bf36-86908e41f536"
	// The root record's field numbers, spelled out here so a test can write one
	// without the reader's unexported constants.
	rootMessageField   = 1
	rootWorkspaceField = 9
)

func builder(t *testing.T) *chatstoretest.Builder {
	t.Helper()
	return chatstoretest.New(t, filepath.Join(t.TempDir(), "store.db"), testSessionID)
}

// TestMetaRejectsWhatIsNotAChatStore covers every way the first read of a store
// can fail. Open is lazy, because SQLite connects on the first query, so a file
// that is not a database and a database that is not a Cursor session are both
// reported here rather than at Open.
func TestMetaRejectsWhatIsNotAChatStore(t *testing.T) {
	rawStore := func(body []byte) func(*testing.T) string {
		return func(t *testing.T) string {
			t.Helper()
			path := filepath.Join(t.TempDir(), "store.db")
			if err := os.WriteFile(path, body, 0o600); err != nil {
				t.Fatal(err)
			}
			return path
		}
	}
	tests := []struct {
		name  string
		write func(*testing.T) string
		// want is the sentinel the error must carry, or nil when any error will
		// do: only an empty meta table is a store this package recognizes.
		want error
	}{
		{name: "an empty file", write: rawStore(nil)},
		{name: "not a database", write: rawStore([]byte("this is not a database"))},
		{name: "a truncated database", write: rawStore([]byte("SQLite format 3\x00truncated"))},
		{
			name:  "a database with no meta table",
			write: func(t *testing.T) string { return builder(t).WithoutMetaTable().Build() },
		},
		{
			name:  "a meta table with no row",
			write: func(t *testing.T) string { return builder(t).WithoutMetaRow().Build() },
			want:  chatstore.ErrNoMeta,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := openStore(t, tt.write(t))

			_, err := store.Meta(context.Background())
			if err == nil {
				t.Fatal("Meta succeeded on a file that is not a chat store")
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Fatalf("Meta error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestMetaDecodesTheHexEncodedRow(t *testing.T) {
	tests := []struct {
		name        string
		set         func(m *chatstoretest.Meta)
		wantName    string
		wantModel   string
		wantCreated time.Time
	}{
		{
			name: "a row that carries everything",
			set: func(m *chatstoretest.Meta) {
				m.Name = "Write Quit"
				m.LastUsedModel = "claude-4-opus"
				m.CreatedAt = 1754636018789
			},
			wantName:    "Write Quit",
			wantModel:   "claude-4-opus",
			wantCreated: time.UnixMilli(1754636018789).UTC(),
		},
		{
			// About half the real stores name no model, and Created() has to
			// report the zero time rather than the epoch so a caller knows to
			// fall back to the file's own timestamp.
			name: "a row with nothing but the session's identity",
			set: func(m *chatstoretest.Meta) {
				m.Name = ""
				m.CreatedAt = 0
				m.LastUsedModel = ""
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := builder(t)
			tt.set(&b.Meta)
			b.AddPrompt("question")
			store := openStore(t, b.Build())

			meta := metaOf(t, store)
			if meta.AgentID != testSessionID {
				t.Errorf("AgentID = %q, want %q", meta.AgentID, testSessionID)
			}
			if meta.LatestRootBlobID == "" {
				t.Error("LatestRootBlobID is empty for a session that holds a message")
			}
			if meta.Name != tt.wantName || meta.LastUsedModel != tt.wantModel {
				t.Errorf("Name = %q model = %q, want %q and %q",
					meta.Name, meta.LastUsedModel, tt.wantName, tt.wantModel)
			}
			if !meta.Created().Equal(tt.wantCreated) {
				t.Errorf("Created() = %s, want %s", meta.Created(), tt.wantCreated)
			}
		})
	}
}

func TestRootDecodesTheOrderedMessageList(t *testing.T) {
	b := builder(t)
	b.WorkspaceURI = "file:///work/repo"
	b.AddSystem("you are cursor")
	b.AddPreamble("<user_info>darwin</user_info>")
	b.AddPrompt("first question")
	b.AddAssistantText("first answer")
	b.AddPrompt("second question")
	b.AddAssistantText("second answer")
	store := openStore(t, b.Build())

	root := rootOf(t, store)
	if len(root.MessageIDs) != 6 {
		t.Fatalf("got %d message IDs, want 6", len(root.MessageIDs))
	}
	if root.Workspace() != "/work/repo" {
		t.Errorf("Workspace() = %q, want /work/repo", root.Workspace())
	}

	var roles, texts []string
	for msg, err := range store.Messages(context.Background(), root.MessageIDs) {
		if err != nil {
			t.Fatalf("Messages: %v", err)
		}
		roles = append(roles, msg.Role)
		texts = append(texts, msg.Text())
	}
	wantRoles := []string{
		chatstore.RoleSystem, chatstore.RoleUser, chatstore.RoleUser,
		chatstore.RoleAssistant, chatstore.RoleUser, chatstore.RoleAssistant,
	}
	if !slices.Equal(roles, wantRoles) {
		t.Errorf("roles = %v, want %v", roles, wantRoles)
	}
	wantTexts := []string{
		"you are cursor", "<user_info>darwin</user_info>", "first question",
		"first answer", "second question", "second answer",
	}
	if !slices.Equal(texts, wantTexts) {
		t.Errorf("texts = %q, want %q", texts, wantTexts)
	}
}

// TestRootTellsAnUnusedSessionFromADamagedOne pins the difference the importer
// depends on. A session that names no root was created and never used, and it
// is skipped without a word. A root the blobs table does not hold is a store
// that lost the order of its whole conversation, and reporting that as an empty
// session would drop it from an import silently.
func TestRootTellsAnUnusedSessionFromADamagedOne(t *testing.T) {
	// Two of the 127 stores read while decoding the format named no root:
	// Cursor created the session and no message was ever sent.
	store := openStore(t, builder(t).SetRootBlobID("").Build())

	tests := []struct {
		name    string
		id      string
		wantErr error
	}{
		{name: "no root recorded", id: ""},
		{name: "whitespace", id: "   "},
		{
			name:    "the root names a blob the store does not hold",
			id:      "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff",
			wantErr: chatstore.ErrNoRootRecord,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ok, err := store.Root(context.Background(), tt.id)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Root error = %v, want %v", err, tt.wantErr)
			}
			if ok {
				t.Fatal("Root reported a record it did not read")
			}
		})
	}
}

// TestRootSkipsWhatItCannotUse covers what a newer Cursor can put in a root
// record. Neither an added field nor a reference that is not a blob ID may cost
// the reader the messages beside it.
func TestRootSkipsWhatItCannotUse(t *testing.T) {
	unknownFields := chatstoretest.AppendUnknownVarint(nil, 10, 0)
	unknownFields = chatstoretest.AppendUnknownBytes(unknownFields, 4, []byte(`{"role":"assistant"}`))
	unknownFields = chatstoretest.AppendUnknownBytes(unknownFields, 18, []byte{0x01, 0x02})

	tests := []struct {
		name  string
		extra []byte
	}{
		{name: "fields a newer cursor added", extra: unknownFields},
		{
			// Field 1 holds a 32-byte hash. A shorter value is not a blob ID, and
			// turning it into one would name a row that does not exist.
			name:  "a message reference that is not a blob ID",
			extra: chatstoretest.AppendUnknownBytes(nil, rootMessageField, []byte("too short")),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := builder(t)
			b.AddPrompt("question")
			b.UnknownRootFields = tt.extra
			store := openStore(t, b.Build())

			root := rootOf(t, store)
			if root.Workspace() != "/work/repo" {
				t.Errorf("Workspace() = %q, want /work/repo", root.Workspace())
			}
			if len(root.MessageIDs) != 1 {
				t.Errorf("got %d message IDs, want the one real message", len(root.MessageIDs))
			}
		})
	}
}

func TestRootRejectsARecordItCannotParse(t *testing.T) {
	// A blob that is not a root record must be an error, not a session with no
	// messages: the difference is a broken store against an empty one.
	tests := []struct {
		name string
		data []byte
	}{
		{
			name: "truncated length-delimited field",
			data: chatstoretest.AppendUnknownBytes(nil, rootWorkspaceField, []byte("file:///work/repo"))[:6],
		},
		{name: "tag with no value", data: []byte{0x08}},
		{name: "reserved wire type", data: []byte{0x0e, 0x01}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := builder(t)
			id := b.AddBlob(tt.data)
			b.SetRootBlobID(id)
			store := openStore(t, b.Build())

			if _, _, err := store.Root(context.Background(), id); err == nil {
				t.Fatal("Root succeeded on a blob that is not a root record")
			}
		})
	}
}

// TestWorkspace covers the URI forms Cursor writes on the three platforms the
// importing binary also ships on. A workspace names a directory on the machine
// that wrote the store, and an imported generation has to name it the way a
// live one from that machine does, or the two never group together.
func TestWorkspace(t *testing.T) {
	tests := []struct {
		uri  string
		want string
	}{
		{uri: "file:///work/repo", want: "/work/repo"},
		{uri: "file:///Users/tester/projects/repo", want: "/Users/tester/projects/repo"},
		{uri: "file://localhost/work/repo", want: "/work/repo"},
		// A Windows drive letter arrives percent-encoded, and "/c:/Users" is a
		// path no filesystem accepts.
		{uri: "file:///c%3A/Users/me/repo", want: `c:\Users\me\repo`},
		{uri: "file:///C%3A/Users/Me/Repo", want: `C:\Users\Me\Repo`},
		// A UNC share. Dropping the host would name a local directory that is
		// not the workspace.
		{uri: "file://server/share/repo", want: `\\server\share\repo`},
		{uri: "", want: ""},
		{uri: "   ", want: ""},
		{uri: "vscode-remote://ssh/work", want: ""},
		{uri: "/work/repo", want: ""},
		{uri: "file://", want: ""},
	}
	for _, tt := range tests {
		if got := (chatstore.Root{WorkspaceURI: tt.uri}).Workspace(); got != tt.want {
			t.Errorf("Workspace(%q) = %q, want %q", tt.uri, got, tt.want)
		}
	}
}

func TestPromptCount(t *testing.T) {
	tests := []struct {
		name  string
		build func(b *chatstoretest.Builder)
		want  int
	}{
		{
			name: "system and preamble do not open a turn",
			build: func(b *chatstoretest.Builder) {
				b.AddSystem("you are cursor")
				b.AddPreamble("<user_info>darwin</user_info>")
				b.AddPrompt("first question")
				b.AddAssistantText("first answer")
				b.AddToolCall("Read", "call-1", `{"path":"/tmp/x"}`)
				b.AddToolResult("Read", "call-1", `"ok"`)
				b.AddPrompt("second question")
				b.AddAssistantText("second answer")
			},
			want: 2,
		},
		{
			name: "an undecodable blob is skipped, not fatal",
			build: func(b *chatstoretest.Builder) {
				b.AddPrompt("first question")
				b.AddRawMessage([]byte("not json at all"))
				b.AddPrompt("second question")
			},
			want: 2,
		},
		{
			name: "a missing blob is skipped",
			build: func(b *chatstoretest.Builder) {
				b.AddPrompt("first question")
				b.ReferenceMissingBlob()
			},
			want: 1,
		},
		{
			// The ID list travels as one JSON parameter, so a session with more
			// messages than SQLite allows host parameters is still counted whole.
			name: "a list longer than SQLite's host-parameter limit",
			build: func(b *chatstoretest.Builder) {
				for i := range 600 {
					b.AddPrompt("question " + string(rune('a'+i%26)))
					b.AddAssistantText("answer")
				}
			},
			want: 600,
		},
		{
			// The store is content-addressed, so a session that sent the same
			// prompt twice lists one blob twice. Both are turns.
			name: "a prompt sent twice is listed twice",
			build: func(b *chatstoretest.Builder) {
				b.AddPrompt("say it again")
				b.AddAssistantText("again")
				b.AddPrompt("say it again")
				b.AddAssistantText("again")
			},
			want: 2,
		},
		{
			// A preview counts the turns the root record names, not every prompt
			// the table happens to hold.
			name: "a prompt the root record does not list",
			build: func(b *chatstoretest.Builder) {
				b.AddPrompt("listed")
				b.AddBlob([]byte(`{"role":"user","content":[{"type":"text","text":"unlisted"}]}`))
			},
			want: 1,
		},
		{
			name:  "an empty session",
			build: func(*chatstoretest.Builder) {},
			want:  0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := builder(t)
			tt.build(b)
			store := openStore(t, b.Build())

			var ids []string
			if root, ok, err := store.Root(context.Background(), metaOf(t, store).LatestRootBlobID); err != nil {
				t.Fatalf("Root: %v", err)
			} else if ok {
				ids = root.MessageIDs
			}
			got, err := store.PromptCount(context.Background(), ids)
			if err != nil {
				t.Fatalf("PromptCount: %v", err)
			}
			if got != tt.want {
				t.Fatalf("PromptCount = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestMessagesReportABlobItCannotRead puts one unusable blob between two good
// messages. The lost message keeps its place in the sequence, as a message with
// Unreadable set: what follows a lost blob cannot be attributed, because the
// blob may have held the prompt the rest of the turn answers.
func TestMessagesReportABlobItCannotRead(t *testing.T) {
	tests := []struct {
		name string
		add  func(b *chatstoretest.Builder)
	}{
		{
			name: "not json",
			add:  func(b *chatstoretest.Builder) { b.AddRawMessage([]byte("not json")) },
		},
		{
			name: "truncated json",
			add:  func(b *chatstoretest.Builder) { b.AddRawMessage([]byte(`{"role":"assistant","content":`)) },
		},
		{
			name: "no role",
			add:  func(b *chatstoretest.Builder) { b.AddRawMessage([]byte(`{"content":"hi"}`)) },
		},
		{
			name: "no content",
			add:  func(b *chatstoretest.Builder) { b.AddRawMessage([]byte(`{"role":"user"}`)) },
		},
		{
			name: "content is neither a string nor an array",
			add:  func(b *chatstoretest.Builder) { b.AddRawMessage([]byte(`{"role":"user","content":{"text":"hi"}}`)) },
		},
		{
			name: "the root names a blob that is not there",
			add:  func(b *chatstoretest.Builder) { b.ReferenceMissingBlob() },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := builder(t)
			b.AddPrompt("kept")
			tt.add(b)
			b.AddAssistantText("also kept")
			store := openStore(t, b.Build())

			root := rootOf(t, store)
			var texts []string
			var lost []int
			for msg, err := range store.Messages(context.Background(), root.MessageIDs) {
				if err != nil {
					t.Fatalf("Messages: %v", err)
				}
				if msg.Unreadable {
					lost = append(lost, len(texts))
					if msg.Role != "" || len(msg.Parts) != 0 {
						t.Errorf("an unreadable message carries %q and %d parts", msg.Role, len(msg.Parts))
					}
					if msg.ID != root.MessageIDs[len(texts)] {
						t.Errorf("ID = %q, want the blob ID the root named", msg.ID)
					}
				}
				texts = append(texts, msg.Text())
			}
			if !slices.Equal(texts, []string{"kept", "", "also kept"}) {
				t.Errorf("texts = %q, want the lost message to keep its place", texts)
			}
			if !slices.Equal(lost, []int{1}) {
				t.Errorf("unreadable at %v, want the middle message only", lost)
			}
		})
	}
}

func TestProviderIDs(t *testing.T) {
	const reasoningID = "rs_01101c93959630f1016920d7bb3f6c5d1e40a27b8c9d0e1f20"
	const callID = "call_quk1E4T6bsSwIEOeoUeuavFk\nfc_01101c93959630f1016920d7bf7a1b2c3d4e5f60718293a4b5"

	tests := []struct {
		name  string
		build func(b *chatstoretest.Builder)
		want  []string
	}{
		{
			// A tool call ID reaches the caller as the store wrote it, newline
			// and all: which half of it carries what is not this package's to
			// decide.
			name: "a call ID and a reasoning ID",
			build: func(b *chatstoretest.Builder) {
				b.AddPrompt("read the file")
				b.AddAssistantReasoningID("**thinking**", reasoningID)
				b.AddToolCall("Read", callID, `{"path":"/tmp/x"}`)
				b.AddToolResult("Read", callID, `"contents"`)
			},
			want: []string{reasoningID, callID, callID},
		},
		{
			// The system prompt and the environment preamble have string
			// content, which json_each would walk as a scalar.
			name: "a session whose messages carry no IDs",
			build: func(b *chatstoretest.Builder) {
				b.AddSystem("you are cursor")
				b.AddPreamble("<user_info>darwin</user_info>")
				b.AddPrompt("a question")
				b.AddAssistantText("an answer")
			},
		},
		{
			// A reasoning signature is JSON inside a JSON string. One that is
			// neither must not fail the query for the rest of the session.
			name: "a signature that is not JSON",
			build: func(b *chatstoretest.Builder) {
				b.AddRawMessage([]byte(`{"role":"assistant","content":` +
					`[{"type":"reasoning","text":"t","signature":"not json"}]}`))
				b.AddToolCall("Read", callID, `{"path":"/tmp/x"}`)
			},
			want: []string{callID},
		},
		{
			name: "an undecodable blob and a missing one are skipped",
			build: func(b *chatstoretest.Builder) {
				b.AddRawMessage([]byte("not json at all"))
				b.ReferenceMissingBlob()
				b.AddToolCall("Read", callID, `{"path":"/tmp/x"}`)
			},
			want: []string{callID},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := builder(t)
			tt.build(b)
			store := openStore(t, b.Build())

			got, err := store.ProviderIDs(context.Background(), rootOf(t, store).MessageIDs)
			if err != nil {
				t.Fatalf("ProviderIDs: %v", err)
			}
			slices.Sort(got)
			want := slices.Clone(tt.want)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("ProviderIDs = %q, want %q", got, want)
			}
		})
	}
}

func TestProviderIDsReadsNoMessageText(t *testing.T) {
	b := builder(t)
	b.AddPrompt("a prompt nobody may read")
	b.AddToolCall("Read", "call-1", `{"path":"/secret"}`)
	b.AddToolResult("Read", "call-1", `"secret contents"`)
	store := openStore(t, b.Build())

	got, err := store.ProviderIDs(context.Background(), rootOf(t, store).MessageIDs)
	if err != nil {
		t.Fatalf("ProviderIDs: %v", err)
	}
	for _, id := range got {
		for _, text := range []string{"a prompt nobody may read", "/secret", "secret contents"} {
			if strings.Contains(id, text) {
				t.Errorf("ProviderIDs returned %q, which holds message text", id)
			}
		}
	}
}

func TestReasoningIDReadsTheSignature(t *testing.T) {
	tests := []struct {
		name string
		part chatstore.Part
		want string
	}{
		{
			name: "the provider's ID for the reasoning item",
			part: chatstore.Part{Signature: `{"id":"rs_abc","encrypted_content":"..."}`},
			want: "rs_abc",
		},
		{
			name: "no signature",
			part: chatstore.Part{},
		},
		{
			name: "a signature that is not JSON",
			part: chatstore.Part{Signature: "opaque"},
		},
		{
			name: "a signature with no ID in it",
			part: chatstore.Part{Signature: `{"type":"reasoning"}`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.part.ReasoningID(); got != tt.want {
				t.Errorf("ReasoningID() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMessagesDecodeToolCallsAndResults(t *testing.T) {
	b := builder(t)
	b.AddPrompt("read the file")
	b.AddAssistantReasoning("**thinking**")
	b.AddToolCall("Read", "call-1", `{"path":"/tmp/x"}`)
	b.AddToolResult("Read", "call-1", `"file contents"`)
	store := openStore(t, b.Build())

	root := rootOf(t, store)
	var got []chatstore.Part
	for msg, err := range store.Messages(context.Background(), root.MessageIDs) {
		if err != nil {
			t.Fatalf("Messages: %v", err)
		}
		got = append(got, msg.Parts...)
	}
	if len(got) != 4 {
		t.Fatalf("got %d parts, want 4: %+v", len(got), got)
	}
	if got[1].Type != chatstore.PartReasoning || got[1].Text != "**thinking**" {
		t.Errorf("reasoning part = %+v", got[1])
	}
	call := got[2]
	if call.Type != chatstore.PartToolCall || call.ToolName != "Read" || call.ToolCallID != "call-1" {
		t.Errorf("tool-call part = %+v", call)
	}
	if string(call.Args) != `{"path":"/tmp/x"}` {
		t.Errorf("tool-call args = %s", call.Args)
	}
	result := got[3]
	if result.Type != chatstore.PartToolResult || result.ToolCallID != "call-1" {
		t.Errorf("tool-result part = %+v", result)
	}
	if string(result.Result) != `"file contents"` {
		t.Errorf("tool-result = %s", result.Result)
	}
}

func TestMessagesStopWhenTheConsumerStops(t *testing.T) {
	b := builder(t)
	for range 10 {
		b.AddPrompt("question")
	}
	store := openStore(t, b.Build())

	root := rootOf(t, store)
	seen := 0
	for range store.Messages(context.Background(), root.MessageIDs) {
		seen++
		if seen == 3 {
			break
		}
	}
	if seen != 3 {
		t.Fatalf("read %d messages after breaking at 3", seen)
	}
}

func TestMessagesReportACancelledContext(t *testing.T) {
	b := builder(t)
	b.AddPrompt("question")
	store := openStore(t, b.Build())

	root := rootOf(t, store)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, err := range store.Messages(ctx, root.MessageIDs) {
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		return
	}
	t.Fatal("Messages yielded nothing for a cancelled context")
}

func TestMessageText(t *testing.T) {
	tests := []struct {
		name  string
		parts []chatstore.Part
		want  string
	}{
		{
			name: "text parts are joined and other kinds ignored",
			parts: []chatstore.Part{
				{Type: chatstore.PartText, Text: "first"},
				{Type: chatstore.PartReasoning, Text: "ignored"},
				{Type: chatstore.PartText, Text: ""},
				{Type: chatstore.PartText, Text: "second"},
			},
			want: "first\n\nsecond",
		},
		{name: "no parts", parts: nil, want: ""},
		{
			name:  "no text parts",
			parts: []chatstore.Part{{Type: chatstore.PartToolCall, ToolName: "Read"}},
			want:  "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (chatstore.Message{Parts: tt.parts}).Text(); got != tt.want {
				t.Fatalf("Text() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestOpenReadsAWriteAheadLog covers the state most stores are in on a machine
// where Cursor has run: a one-page store.db beside a store.db-wal holding the
// whole session. Opening read-only must see the log's content, which is why the
// reader cannot use SQLite's immutable=1.
func TestOpenReadsAWriteAheadLog(t *testing.T) {
	b := builder(t)
	b.WAL = true
	b.AddPrompt("in the log")
	path := b.Build()

	if _, err := os.Stat(path + "-wal"); err != nil {
		t.Fatalf("the builder left no write-ahead log: %v", err)
	}

	store := openStore(t, path)
	root := rootOf(t, store)
	if len(root.MessageIDs) != 1 {
		t.Fatalf("got %d message IDs, want the one in the log", len(root.MessageIDs))
	}
}

// TestOpenReadsAPathTheURISyntaxSpellsDifferently covers the characters a file:
// URI gives a meaning to, in the directories a store sits under. "#" starts a
// fragment and "?" the query string, so either one used to end the path early
// and swallow the mode=ro behind it: the open then created the shorter path and
// held it read-write. "%41" used to decode to "A".
func TestOpenReadsAPathTheURISyntaxSpellsDifferently(t *testing.T) {
	tests := []struct {
		name string
		dir  string
	}{
		{name: "a space", dir: "chat store"},
		{name: "a fragment marker", dir: "chats#01"},
		{name: "a query marker", dir: "chats?01"},
		{name: "an escape", dir: "chats%41"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, tt.dir)
			if err := os.MkdirAll(dir, 0o750); err != nil {
				t.Skipf("this platform has no path like it: %v", err)
			}
			b := chatstoretest.New(t, filepath.Join(dir, "store.db"), testSessionID)
			b.AddPrompt("a question")
			path := b.Build()

			if got := metaOf(t, openStore(t, path)).AgentID; got != testSessionID {
				t.Fatalf("AgentID = %q, want the session written at %s", got, path)
			}
			entries, err := os.ReadDir(root)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 {
				t.Fatalf("the session directory has %d siblings, want none: the open wrote a path of its own",
					len(entries)-1)
			}
		})
	}
}

func openStore(t *testing.T, path string) *chatstore.Store {
	t.Helper()
	store, err := chatstore.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func metaOf(t *testing.T, store *chatstore.Store) chatstore.Meta {
	t.Helper()
	meta, err := store.Meta(context.Background())
	if err != nil {
		t.Fatalf("Meta: %v", err)
	}
	return meta
}

func rootOf(t *testing.T, store *chatstore.Store) chatstore.Root {
	t.Helper()
	root, ok, err := store.Root(context.Background(), metaOf(t, store).LatestRootBlobID)
	if err != nil || !ok {
		t.Fatalf("Root: ok=%v err=%v", ok, err)
	}
	return root
}
