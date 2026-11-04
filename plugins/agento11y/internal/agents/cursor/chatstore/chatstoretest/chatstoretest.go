// Package chatstoretest writes a Cursor chat store for a test to read.
//
// It exists because two packages need to build one: the reader's own tests and
// the history importer's. Cursor's format is undocumented, so the writer is the
// only executable statement of what the reader expects. One copy of it means a
// format correction cannot be made in one test file and missed in the other.
//
// Every store it writes holds synthetic content. No bytes from a real
// conversation are committed to this repository.
package chatstoretest

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/chatstore"
	"google.golang.org/protobuf/encoding/protowire"
)

// Root record field numbers, repeated here rather than shared with the reader:
// a writer that reads its field numbers from the reader could not catch the
// reader using the wrong number. The field table is in the chatstore package doc.
const (
	rootFieldMessageID    = 1
	rootFieldWorkspaceURI = 9
)

// Builder accumulates a session and writes it as a store.db.
//
// The zero value is not usable; call [New]. Set the exported fields before
// calling [Builder.Build].
type Builder struct {
	t    testing.TB
	path string

	// Meta is written to the store's meta row. New fills it with a valid
	// session; a test overrides the fields it cares about.
	Meta Meta
	// WorkspaceURI becomes root field 9. Empty writes no workspace.
	WorkspaceURI string
	// WAL leaves the session in a write-ahead log instead of checkpointing it
	// into store.db, which is the state a live Cursor leaves behind. Build then
	// holds a connection open until the test ends, because SQLite checkpoints
	// and deletes the log when the last connection closes.
	WAL bool
	// UnknownRootFields adds fields the reader does not know, so a test can
	// check that a newer Cursor's additions are skipped rather than fatal.
	UnknownRootFields []byte

	blobs         []blob
	messageIDs    []string
	noMetaTable   bool
	noMetaRow     bool
	assistantSeq  int
	missingRefs   int
	rootBlobIDSet bool
}

// Meta mirrors the store's meta row. It is declared here rather than reused
// from the reader for the same reason the field numbers are.
type Meta struct {
	AgentID          string `json:"agentId"`
	LatestRootBlobID string `json:"latestRootBlobId"`
	Name             string `json:"name"`
	CreatedAt        int64  `json:"createdAt"`
	Mode             string `json:"mode"`
	LastUsedModel    string `json:"lastUsedModel"`
}

type blob struct {
	id   string
	data []byte
}

// New starts a builder for a store at path. The session it writes has a valid
// meta row naming sessionID, so a test that only cares about messages adds
// messages and nothing else.
func New(t testing.TB, path, sessionID string) *Builder {
	t.Helper()
	return &Builder{
		t:    t,
		path: path,
		Meta: Meta{
			AgentID:       sessionID,
			Name:          "New Agent",
			Mode:          "default",
			CreatedAt:     1764173721551, // 2025-11-26T14:55:21.551Z
			LastUsedModel: "gpt-5.1-codex-high",
		},
		WorkspaceURI: "file:///work/repo",
	}
}

// WithoutMetaTable writes a database with a blobs table and no meta table,
// which is a SQLite file that is not a Cursor session.
func (b *Builder) WithoutMetaTable() *Builder {
	b.noMetaTable = true
	return b
}

// WithoutMetaRow writes the meta table with no row in it.
func (b *Builder) WithoutMetaRow() *Builder {
	b.noMetaRow = true
	return b
}

// AddSystem adds the session's system prompt, which Cursor writes with string
// content.
func (b *Builder) AddSystem(text string) *Builder {
	return b.addMessage(map[string]any{"role": "system", "content": text})
}

// AddPreamble adds the environment block Cursor prepends once per session: a
// user message whose content is a string rather than an array, which is what
// keeps it from being read as a turn boundary.
func (b *Builder) AddPreamble(text string) *Builder {
	return b.addMessage(map[string]any{"role": "user", "content": text})
}

// AddPrompt adds a user prompt, the message that opens a turn.
func (b *Builder) AddPrompt(text string) *Builder {
	return b.addMessage(map[string]any{
		"role":    "user",
		"content": []any{map[string]any{"type": "text", "text": text}},
	})
}

// AddAssistantText adds an assistant reply.
func (b *Builder) AddAssistantText(text string) *Builder {
	return b.addAssistant(map[string]any{"type": "text", "text": text})
}

// AddAssistantReasoning adds an assistant reasoning block.
func (b *Builder) AddAssistantReasoning(text string) *Builder {
	return b.AddAssistantReasoningID(text, "rs_test")
}

// AddAssistantReasoningID adds an assistant reasoning block whose signature
// names the reasoning item. The signature is JSON held in a JSON string, which
// is the shape Cursor writes, and the ID inside it is the only place a
// reasoning part records which provider item it was.
func (b *Builder) AddAssistantReasoningID(text, reasoningID string) *Builder {
	b.t.Helper()
	signature, err := json.Marshal(map[string]any{"id": reasoningID, "type": "reasoning"})
	if err != nil {
		b.t.Fatalf("chatstoretest: encode reasoning signature: %v", err)
	}
	return b.addAssistant(map[string]any{
		"type": "reasoning", "text": text,
		"signature": string(signature),
	})
}

// AddToolCall adds an assistant message calling one tool. args must be JSON.
func (b *Builder) AddToolCall(name, callID, args string) *Builder {
	return b.addAssistant(map[string]any{
		"type": "tool-call", "toolName": name, "toolCallId": callID,
		"args": json.RawMessage(args),
	})
}

// AddToolResult adds the tool message answering a call. result must be JSON.
func (b *Builder) AddToolResult(name, callID, result string) *Builder {
	return b.addMessage(map[string]any{
		"role": "tool", "id": callID,
		"content": []any{map[string]any{
			"type": "tool-result", "toolName": name, "toolCallId": callID,
			"result": json.RawMessage(result),
		}},
	})
}

// AddRawMessage adds a blob to the message list without checking it, so a test
// can put a corrupt or unparseable message in a session.
func (b *Builder) AddRawMessage(data []byte) *Builder {
	id := blobID(data)
	b.blobs = append(b.blobs, blob{id: id, data: data})
	b.messageIDs = append(b.messageIDs, id)
	return b
}

// ReferenceMissingBlob adds a message reference that names no blob, which is
// how a session partially written to disk reads.
func (b *Builder) ReferenceMissingBlob() *Builder {
	b.missingRefs++
	sum := sha256.Sum256(fmt.Appendf(nil, "chatstoretest missing %d", b.missingRefs))
	b.messageIDs = append(b.messageIDs, hex.EncodeToString(sum[:]))
	return b
}

// AddBlob stores a blob nothing references, so a test can prove a reader does
// not walk the whole table.
func (b *Builder) AddBlob(data []byte) string {
	id := blobID(data)
	b.blobs = append(b.blobs, blob{id: id, data: data})
	return id
}

// SetRootBlobID overrides the root reference in the meta row, so a test can
// point a session at a blob that is not a root record, or at no blob at all.
//
// Build then writes no root record of its own, because there would be nothing
// pointing at it. Any message added stays in the table unreferenced.
func (b *Builder) SetRootBlobID(id string) *Builder {
	b.Meta.LatestRootBlobID = id
	b.rootBlobIDSet = true
	return b
}

func (b *Builder) addAssistant(part map[string]any) *Builder {
	b.assistantSeq++
	return b.addMessage(map[string]any{
		"role":    "assistant",
		"id":      fmt.Sprintf("%d", b.assistantSeq),
		"content": []any{part},
	})
}

func (b *Builder) addMessage(msg map[string]any) *Builder {
	b.t.Helper()
	data, err := json.Marshal(msg)
	if err != nil {
		b.t.Fatalf("chatstoretest: encode message: %v", err)
	}
	return b.AddRawMessage(data)
}

// Build writes the store and returns its path.
func (b *Builder) Build() string {
	b.t.Helper()

	root := b.encodeRoot()
	if !b.rootBlobIDSet && len(root) > 0 {
		id := blobID(root)
		b.blobs = append(b.blobs, blob{id: id, data: root})
		b.Meta.LatestRootBlobID = id
	}

	// The reader's URI form, so a temp directory holding a character the URI
	// syntax uses, such as the "#01" a repeated subtest name puts in one, writes
	// the store where the reader then looks for it.
	db, err := sql.Open("sqlite", chatstore.URI(b.path))
	if err != nil {
		b.t.Fatalf("chatstoretest: open %s: %v", b.path, err)
	}
	defer func() {
		if !b.WAL {
			_ = db.Close()
		}
	}()

	// journal_mode is set before any table exists, so the schema itself lands
	// in the log when WAL is asked for.
	mode := "delete"
	if b.WAL {
		mode = "wal"
	}
	if _, err := db.Exec(`pragma journal_mode = ` + mode); err != nil {
		b.t.Fatalf("chatstoretest: set journal mode: %v", err)
	}
	if _, err := db.Exec(`create table blobs (id TEXT PRIMARY KEY, data BLOB)`); err != nil {
		b.t.Fatalf("chatstoretest: create blobs: %v", err)
	}
	if !b.noMetaTable {
		if _, err := db.Exec(`create table meta (key TEXT PRIMARY KEY, value TEXT)`); err != nil {
			b.t.Fatalf("chatstoretest: create meta: %v", err)
		}
	}
	for _, bl := range b.blobs {
		if _, err := db.Exec(`insert or ignore into blobs (id, data) values (?, ?)`, bl.id, bl.data); err != nil {
			b.t.Fatalf("chatstoretest: insert blob: %v", err)
		}
	}
	if !b.noMetaTable && !b.noMetaRow {
		meta, err := json.Marshal(b.Meta)
		if err != nil {
			b.t.Fatalf("chatstoretest: encode meta: %v", err)
		}
		if _, err := db.Exec(`insert into meta (key, value) values ('0', ?)`, hex.EncodeToString(meta)); err != nil {
			b.t.Fatalf("chatstoretest: insert meta: %v", err)
		}
	}
	if b.WAL {
		b.holdOpen(db)
	}
	return b.path
}

// holdOpen keeps one connection to the store for the rest of the test, so the
// write-ahead log stays beside store.db. SQLite checkpoints the log into the
// database and deletes it when the last connection closes, which would undo
// exactly the state the caller asked for.
func (b *Builder) holdOpen(db *sql.DB) {
	b.t.Helper()
	conn, err := db.Conn(context.Background())
	if err != nil {
		b.t.Fatalf("chatstoretest: hold a connection open: %v", err)
	}
	b.t.Cleanup(func() {
		_ = conn.Close()
		_ = db.Close()
	})
}

// encodeRoot writes the root record: one field-1 reference per message in
// order, then the workspace URI, then whatever a test wants appended.
func (b *Builder) encodeRoot() []byte {
	var out []byte
	for _, id := range b.messageIDs {
		raw, err := hex.DecodeString(id)
		if err != nil {
			b.t.Fatalf("chatstoretest: blob ID %q is not hex: %v", id, err)
		}
		out = protowire.AppendTag(out, rootFieldMessageID, protowire.BytesType)
		out = protowire.AppendBytes(out, raw)
	}
	if b.WorkspaceURI != "" {
		out = protowire.AppendTag(out, rootFieldWorkspaceURI, protowire.BytesType)
		out = protowire.AppendString(out, b.WorkspaceURI)
	}
	return append(out, b.UnknownRootFields...)
}

// blobID is the store's addressing scheme: the hex SHA-256 of the blob.
//
// Cursor's own hash is not necessarily SHA-256, and a reader must not care: it
// looks blobs up by the ID the root record names. Using a content hash here
// keeps the writer's IDs stable and collision-free without inventing a counter.
func blobID(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// AppendUnknownVarint returns a root field the reader does not know, for
// [Builder.UnknownRootFields].
func AppendUnknownVarint(dst []byte, field int, value uint64) []byte {
	dst = protowire.AppendTag(dst, protowire.Number(field), protowire.VarintType)
	return protowire.AppendVarint(dst, value)
}

// AppendUnknownBytes returns a length-delimited root field the reader does not
// know, for [Builder.UnknownRootFields].
func AppendUnknownBytes(dst []byte, field int, value []byte) []byte {
	dst = protowire.AppendTag(dst, protowire.Number(field), protowire.BytesType)
	return protowire.AppendBytes(dst, value)
}
