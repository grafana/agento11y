// Package chatstore reads Cursor's per-session chat store: the SQLite file
// Cursor writes at ~/.cursor/chats/<workspace-hash>/<session-uuid>/store.db.
//
// The format is undocumented and unversioned. Everything below was established
// by reading real stores, so a Cursor release can change any of it.
//
// # The store
//
// Two tables:
//
//	blobs (id TEXT PRIMARY KEY, data BLOB)
//	meta  (key TEXT PRIMARY KEY, value TEXT)
//
// The store is content-addressed: blobs.id is the hex form of a hash of data,
// and one record names another by that hex ID. A live Cursor keeps almost
// everything in the write-ahead log, so a one-page store.db beside a 250 KB
// store.db-wal is the normal state and a reader that ignores the log loses the
// conversation.
//
// # The meta row
//
// One row, key "0", whose value is hex-encoded JSON rather than JSON. It names
// the session (agentId, which matches the directory name), its root record
// (latestRootBlobId, empty for a session that was created and never used), its
// title, its createdAt in Unix milliseconds, and lastUsedModel, which is
// session-wide and often absent. createdAt is the only timestamp anywhere in
// the store.
//
// # The root record
//
// blobs[latestRootBlobId] is a protobuf record with no published schema, so it
// is read by field number. These are the fields seen across 127 real stores:
//
//	 1      bytes, repeated  one 32-byte message hash per message, in order  read
//	 2      bytes, repeated  a per-turn record; older stores only            skipped
//	 3      bytes, repeated  a todo entry, with its own ms timestamps        skipped
//	 4      string           the message Cursor is still streaming           skipped
//	 5      message          {1: tokens used, 2: context window}, per session skipped
//	 8      bytes            the latest turn's record, that turn only        skipped
//	 9      string           the workspace, as file:///path                  read
//	10      varint           always 0                                        skipped
//	12, 18  message          attachment and file metadata                    skipped
//
// Field 1 is the only ordering the store records. An unknown field is skipped
// rather than rejected, because Cursor adds fields. Fields 2, 5 and 8 look
// useful and are not: field 2 is absent from most stores, field 5 is the whole
// session's context occupancy, and field 8 names one turn.
//
// # A message blob
//
// Each field-1 reference names a blob holding JSON in the shape a provider SDK
// sends: a role of system, user, assistant or tool, and content that is either
// a string or an array of parts typed text, reasoning, tool-call or tool-result.
//
// The load-bearing detail is the two encodings of content. String content is
// something Cursor prepended: its system prompt, and one environment block per
// session carrying the OS, the shell, the workspace and the git status. Array
// content is a real user prompt. That is what tells a turn boundary from
// context, and it is what [Message.StringContent] reports.
//
// # Provider IDs
//
// A part carries the IDs the model provider issued: toolCallId on a tool call
// and its result, and an id inside the reasoning part's signature. Cursor does
// not timestamp a message, but some providers put the issue time inside the ID
// itself, so these are the only per-message clock the store holds. The reader
// returns them and does not interpret them; internal/history/cursor_clock.go
// decodes the layouts.
//
// # What the format does not carry
//
// No per-turn timestamp field, no per-turn token usage, no per-turn model name,
// and no turn ID on a message. An importer has to mark all four as absent.
//
// # What reaches the caller
//
// No message body crosses into Go unless the caller asks for that message by ID
// with [Store.Messages]. [Store.PromptCount] has to know which message is a
// prompt, and has SQLite decide inside the query, so it returns a count and no
// text. [Store.ProviderIDs] extracts the IDs in the query for the same reason.
//
// internal/history/testdata/cursor/README.md covers the fixtures and the
// harness that checks a new Cursor release against this reader.
package chatstore

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protowire"

	// The pure-Go driver. The release binary builds with CGO_ENABLED=0
	// (.goreleaser.yaml), so a cgo driver cannot be used.
	_ "modernc.org/sqlite"
)

// ErrNoMeta reports a store whose meta table holds no row, which is a Cursor
// session store with nothing in it.
var ErrNoMeta = errors.New("chatstore: no meta row")

// ErrNoRootRecord reports a meta row that names a root record the blobs table
// does not hold. It is a damaged or half-written store, not an unused session:
// an unused session names no root at all, and [Store.Root] reports that with
// ok=false and no error.
var ErrNoRootRecord = errors.New("chatstore: the meta row names a root record that is not in the store")

// blobIDLen is the hex length of a blob ID. IDs are the hex encoding of a
// 32-byte content hash, and the root record holds the raw 32 bytes.
const blobIDLen = 64

// Roles as they appear in a message blob.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Part types as they appear in a message blob's content array.
const (
	PartText       = "text"
	PartReasoning  = "reasoning"
	PartToolCall   = "tool-call"
	PartToolResult = "tool-result"
)

// Root record field numbers. A root carries more than these two: per-turn
// records, a todo list, session-wide context occupancy, a streaming buffer and
// attachment metadata. None of those is a source an import can use, so they are
// skipped rather than decoded. The testdata README lists all of them.
const (
	rootFieldMessageID    = 1 // repeated: one blob ID per message, in order
	rootFieldWorkspaceURI = 9 // "file:///path/to/workspace"
)

// Meta is the decoded meta row. It names the session, its root record, and the
// only timestamp and model name the store records anywhere.
type Meta struct {
	AgentID          string `json:"agentId"` // the session UUID, matching the directory name
	LatestRootBlobID string `json:"latestRootBlobId"`
	Name             string `json:"name"`
	CreatedAt        int64  `json:"createdAt"` // Unix milliseconds
	Mode             string `json:"mode"`
	LastUsedModel    string `json:"lastUsedModel"`
}

// Created returns the session start time. The zero time means the store
// recorded none.
func (m Meta) Created() time.Time {
	if m.CreatedAt <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(m.CreatedAt).UTC()
}

// Root is the decoded root record: the ordered message list and the workspace
// the session ran in.
type Root struct {
	// MessageIDs are the session's message blob IDs in conversation order. This
	// is the only ordering the store records.
	MessageIDs []string
	// WorkspaceURI is the workspace the session ran in, as a file:// URI.
	WorkspaceURI string
}

// Workspace returns the workspace as a filesystem path, or "" when the store
// recorded none or recorded something that is not a file URI.
//
// Three forms of file URI reach this, because Cursor ships on three platforms
// and the binary that imports its history ships on the same three:
//
//	file:///work/repo             -> /work/repo
//	file:///c%3A/Users/me/repo    -> c:\Users\me\repo
//	file://server/share/repo      -> \\server\share\repo
//
// The last two are Windows paths, and a Windows path is returned with
// backslashes whatever host reads the store: the value names a workspace on the
// machine that wrote it, and a live generation from that machine names it the
// same way. The drive letter keeps the case the URI used, for the same reason.
func (r Root) Workspace() string {
	raw := strings.TrimSpace(r.WorkspaceURI)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "file" || u.Path == "" {
		return ""
	}
	if host := u.Host; host != "" && !strings.EqualFold(host, "localhost") {
		// A UNC share. Dropping the host would name a local path that is not the
		// workspace.
		return `\\` + host + strings.ReplaceAll(u.Path, "/", `\`)
	}
	if p := strings.TrimPrefix(u.Path, "/"); isWindowsDrivePath(p) {
		// "/c:/Users/me/repo" is not a path any filesystem accepts.
		return strings.ReplaceAll(p, "/", `\`)
	}
	return u.Path
}

// isWindowsDrivePath reports whether p starts with a drive letter and a colon.
func isWindowsDrivePath(p string) bool {
	if len(p) < 2 || p[1] != ':' {
		return false
	}
	c := p[0] | 0x20 // fold the case of an ASCII letter
	return c >= 'a' && c <= 'z'
}

// Part is one element of a message's content array. Only the fields an import
// uses are decoded; Cursor's own providerOptions are dropped.
type Part struct {
	Type       string          `json:"type"`
	Text       string          `json:"text"`
	ToolName   string          `json:"toolName"`
	ToolCallID string          `json:"toolCallId"`
	Args       json.RawMessage `json:"args"`
	Result     json.RawMessage `json:"result"`
	// Signature is the provider's envelope around a reasoning part. Read it
	// through [Part.ReasoningID]: the rest of it is the provider's encrypted
	// copy of the reasoning, which no import sends anywhere.
	Signature string `json:"signature"`
}

// ReasoningID is the provider's ID for a reasoning part, or "" when the part
// carries no signature or one this cannot read. The signature is JSON inside a
// JSON string, so it takes a second decode.
func (p Part) ReasoningID() string {
	if p.Signature == "" {
		return ""
	}
	var sig struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(p.Signature), &sig); err != nil {
		return ""
	}
	return strings.TrimSpace(sig.ID)
}

// Message is one decoded message blob, or one the store could not produce.
type Message struct {
	ID   string
	Role string
	// Parts is the decoded content array. A message whose content is a plain
	// string yields one text part, so a caller never has to branch on the
	// encoding.
	Parts []Part
	// StringContent is true when the source encoded content as a string rather
	// than an array. Cursor writes the system prompt and the per-session
	// environment preamble that way, and a real user prompt as an array, which
	// is what tells a turn boundary from context the agent prepended.
	StringContent bool
	// Unreadable is true when the blob the root record named could not be read:
	// no row holds it, or its bytes are not a message. Role and Parts are then
	// empty.
	//
	// It is reported rather than skipped because the lost message may have been
	// a prompt. A caller that folded what follows it into what precedes it would
	// export one turn's output as the answer to another turn's prompt.
	Unreadable bool
}

// Text returns the message's text, concatenating every text part.
func (m Message) Text() string {
	var b strings.Builder
	for _, p := range m.Parts {
		if p.Type != PartText || p.Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(p.Text)
	}
	return b.String()
}

// Store is an open read-only chat store.
type Store struct {
	db   *sql.DB
	path string
}

// Open opens path read-only. It reads nothing: SQLite connects lazily, so a file
// that is not a database, or a database that is not a Cursor session, fails at
// the first read, which for every caller is [Store.Meta].
//
// A live Cursor keeps almost everything in the write-ahead log: a store whose
// store.db is one page can hold a whole session in its store.db-wal. SQLite
// needs the WAL index (store.db-shm) to read that, and creates it when it is
// missing, so opening a store may add that sidecar to the session directory.
// The alternative, immutable=1, opens without the index but ignores the WAL
// too, which on real stores loses the entire conversation.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", URI(path)+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("chatstore: open %s: %w", path, err)
	}
	return &Store{db: db, path: path}, nil
}

// URI is path as the file: URI a query parameter such as mode=ro has to be
// attached to. The test writer builds its stores through it too, so a store is
// written to and read from the same path.
//
// A path cannot be concatenated after "file:" as it stands. SQLite reads "#" as
// the start of a fragment and "?" as the start of the query string, so a store
// under a directory holding either names a shorter file and drops the mode=ro
// that follows it: the open then creates and writes that path instead of
// reading the session. "%HH" decodes to the one character it spells. Escaping
// the path leaves SQLite the same bytes to decode back, and a Windows drive
// letter takes the leading slash sqlite.org/uri.html gives it.
func URI(path string) string {
	p := filepath.ToSlash(path)
	if filepath.IsAbs(path) && !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return "file:" + (&url.URL{Path: p}).EscapedPath()
}

// Close releases the store.
func (s *Store) Close() error { return s.db.Close() }

// Meta reads and decodes the meta row.
func (s *Store) Meta(ctx context.Context) (Meta, error) {
	var encoded string
	err := s.db.QueryRowContext(ctx, `select value from meta where key = '0'`).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return Meta{}, ErrNoMeta
	}
	if err != nil {
		// This is the first read of the file. A file that is not a database, and
		// a database with no meta table and so not a Cursor session, both fail
		// here.
		return Meta{}, fmt.Errorf("chatstore: read meta from %s: %w", s.path, err)
	}
	raw, err := hex.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return Meta{}, fmt.Errorf("chatstore: decode meta hex: %w", err)
	}
	var m Meta
	if err := json.Unmarshal(raw, &m); err != nil {
		return Meta{}, fmt.Errorf("chatstore: decode meta json: %w", err)
	}
	return m, nil
}

// Root reads and decodes the record named by [Meta.LatestRootBlobID].
//
// ok is false, with no error, when the ID is empty: that is a session Cursor
// created and no message was ever sent in. An ID that names no blob is
// [ErrNoRootRecord] instead, because a store that lost its root record lost the
// order of its whole conversation, and reporting that as an empty session would
// drop it from an import without a word.
func (s *Store) Root(ctx context.Context, id string) (root Root, ok bool, err error) {
	if strings.TrimSpace(id) == "" {
		return Root{}, false, nil
	}
	data, ok, err := s.blob(ctx, id)
	if err != nil {
		return Root{}, false, err
	}
	if !ok {
		return Root{}, false, fmt.Errorf("%w: %s", ErrNoRootRecord, id)
	}
	root, err = decodeRoot(data)
	if err != nil {
		return Root{}, false, err
	}
	return root, true, nil
}

// PromptCount counts the user prompts among ids, which is the number of prompts
// the session holds and so the number of turns it was asked for.
//
// SQLite decides which blob is a prompt, and the query returns a count, so no
// message body crosses into Go. That keeps a metadata-only preview
// metadata-only even though the role that marks a turn boundary is inside a
// message blob. It is not free: the predicate makes SQLite read and JSON-parse
// the data column of every message the session lists, so the work is linear in
// the session's total message bytes.
//
// The count is over ids, not over the rows they name. The store is
// content-addressed, so a session that sent the same prompt twice lists one blob
// twice; json_each yields one row per element of the list, which counts the
// second one.
//
// A blob that is missing or not valid JSON is not counted rather than failing
// the query, so a half-written store still previews.
func (s *Store) PromptCount(ctx context.Context, ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	// The ID list travels as one JSON array parameter. Binding one parameter per
	// ID would need chunking, because SQLite caps a statement's host parameters
	// and a long session holds thousands of messages.
	list, err := json.Marshal(ids)
	if err != nil {
		return 0, fmt.Errorf("chatstore: count prompts: %w", err)
	}
	const query = `select count(*) from json_each(?) as ref
		join blobs on blobs.id = ref.value
		where json_valid(blobs.data)
		and json_extract(blobs.data, '$.role') = 'user'
		and json_type(blobs.data, '$.content') = 'array'`
	var total int
	if err := s.db.QueryRowContext(ctx, query, string(list)).Scan(&total); err != nil {
		return 0, fmt.Errorf("chatstore: count prompts: %w", err)
	}
	return total, nil
}

// ProviderIDs returns the provider-issued IDs carried by the messages named by
// ids: every part's toolCallId, and the id inside a reasoning part's signature.
// They come back in no guaranteed order, and a caller that needs one message's
// IDs in order reads that message with [Store.Messages] instead.
//
// SQLite extracts the IDs inside the query, so a caller can date a session
// without any message text crossing into Go. The predicate makes SQLite read and
// JSON-parse every message named, so the work is linear in their bytes: pass the
// tail of a session, not all of it, unless the whole session is wanted.
//
// A blob that is missing, not JSON, or not a message with array content
// contributes nothing rather than failing the query.
func (s *Store) ProviderIDs(ctx context.Context, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	list, err := json.Marshal(ids)
	if err != nil {
		return nil, fmt.Errorf("chatstore: read provider IDs: %w", err)
	}
	// The signature is JSON held in a JSON string, so its id needs the inner
	// json_extract. json_valid guards it: json_extract raises on a string that is
	// not JSON, which would fail the whole query for one odd provider.
	const query = `select coalesce(json_extract(part.value, '$.toolCallId'), ''),
			case when json_valid(json_extract(part.value, '$.signature'))
				then coalesce(json_extract(json_extract(part.value, '$.signature'), '$.id'), '')
				else '' end
		from json_each(?) as ref
		join blobs on blobs.id = ref.value
		join json_each(blobs.data, '$.content') as part
		where json_valid(blobs.data)
		and json_type(blobs.data, '$.content') = 'array'`
	rows, err := s.db.QueryContext(ctx, query, string(list))
	if err != nil {
		return nil, fmt.Errorf("chatstore: read provider IDs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var callID, reasoningID string
		if err := rows.Scan(&callID, &reasoningID); err != nil {
			return nil, fmt.Errorf("chatstore: read provider IDs: %w", err)
		}
		for _, id := range []string{callID, reasoningID} {
			if id = strings.TrimSpace(id); id != "" {
				out = append(out, id)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chatstore: read provider IDs: %w", err)
	}
	return out, nil
}

// Messages yields the messages named by ids, in the order given, reading one
// blob at a time.
//
// A blob that is missing or undecodable yields a message with Unreadable set
// rather than an error: an import of a partially written session should lose
// that message, not the session. The placeholder keeps its place in the order,
// so a caller can see that something between two messages was lost. Only a
// failed read of the database itself, or a cancelled context, is an error.
func (s *Store) Messages(ctx context.Context, ids []string) iter.Seq2[Message, error] {
	return func(yield func(Message, error) bool) {
		for _, id := range ids {
			if err := ctx.Err(); err != nil {
				yield(Message{}, err)
				return
			}
			data, ok, err := s.blob(ctx, id)
			if err != nil {
				yield(Message{}, err)
				return
			}
			msg := Message{ID: id, Unreadable: true}
			if ok {
				if decoded, err := decodeMessage(id, data); err == nil {
					msg = decoded
				}
			}
			if !yield(msg, nil) {
				return
			}
		}
	}
}

// blob reads one blob. ok is false when no row has that ID.
func (s *Store) blob(ctx context.Context, id string) (data []byte, ok bool, err error) {
	err = s.db.QueryRowContext(ctx, `select data from blobs where id = ?`, id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("chatstore: read blob: %w", err)
	}
	return data, true, nil
}

// decodeRoot reads the root record's wire format directly. Cursor publishes no
// schema for it, so there is nothing to generate a stub from, and the two fields
// an import needs are read by field number.
func decodeRoot(data []byte) (Root, error) {
	var root Root
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			return Root{}, fmt.Errorf("chatstore: root record: %w", protowire.ParseError(n))
		}
		data = data[n:]

		switch {
		case num == rootFieldMessageID && typ == protowire.BytesType:
			id, err := consumeBlobID(&data)
			if err != nil {
				return Root{}, err
			}
			if id != "" {
				root.MessageIDs = append(root.MessageIDs, id)
			}
		case num == rootFieldWorkspaceURI && typ == protowire.BytesType:
			v, n := protowire.ConsumeBytes(data)
			if n < 0 {
				return Root{}, fmt.Errorf("chatstore: root workspace: %w", protowire.ParseError(n))
			}
			data = data[n:]
			root.WorkspaceURI = string(v)
		default:
			n := protowire.ConsumeFieldValue(num, typ, data)
			if n < 0 {
				return Root{}, fmt.Errorf("chatstore: root field %d: %w", num, protowire.ParseError(n))
			}
			data = data[n:]
		}
	}
	return root, nil
}

// consumeBlobID reads a length-delimited field holding a raw blob hash and
// returns its hex form, which is how the blobs table keys it. A value that is
// not hash-sized is not a blob ID, so it is skipped rather than turned into an
// ID that matches no row.
func consumeBlobID(data *[]byte) (string, error) {
	v, n := protowire.ConsumeBytes(*data)
	if n < 0 {
		return "", fmt.Errorf("chatstore: root blob reference: %w", protowire.ParseError(n))
	}
	*data = (*data)[n:]
	if hex.EncodedLen(len(v)) != blobIDLen {
		return "", nil
	}
	return hex.EncodeToString(v), nil
}

// decodeMessage decodes one message blob. content is a string or an array of
// parts depending on the message, and both are normalized to Parts.
func decodeMessage(id string, data []byte) (Message, error) {
	var raw struct {
		ID      string          `json:"id"`
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return Message{}, fmt.Errorf("chatstore: decode message %s: %w", id, err)
	}
	msg := Message{ID: strings.TrimSpace(raw.ID), Role: strings.TrimSpace(raw.Role)}
	if msg.Role == "" {
		return Message{}, fmt.Errorf("chatstore: message %s has no role", id)
	}

	var text string
	if err := json.Unmarshal(raw.Content, &text); err == nil {
		msg.StringContent = true
		if text != "" {
			msg.Parts = []Part{{Type: PartText, Text: text}}
		}
		return msg, nil
	}
	if err := json.Unmarshal(raw.Content, &msg.Parts); err != nil {
		return Message{}, fmt.Errorf("chatstore: decode message %s content: %w", id, err)
	}
	return msg, nil
}
