// Package sessiondb reads OpenCode's SQLite session store.
package sessiondb

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"iter"
	"time"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/chatstore"
	_ "modernc.org/sqlite"
)

// MaxPayloadBytes is the largest message.data or part.data value decoded.
const MaxPayloadBytes = 4 * 1024 * 1024

// Store is an open read-only OpenCode session database.
type Store struct {
	db   *sql.DB
	path string
}

// Session is one row from the session table with discovery aggregates.
type Session struct {
	ID                   string
	Title                string
	Directory            string
	ParentID             string
	CreatedAt            time.Time
	UpdatedAt            time.Time
	MessageCount         int
	TerminalMessageCount int
	LogicalSizeBytes     int64
}

// Message is one decoded message row. OpenCode strips ID and SessionID from
// data before writing it, so the reader restores them from the row columns.
type Message struct {
	ID         string    `json:"-"`
	SessionID  string    `json:"-"`
	RowCreated time.Time `json:"-"`
	RowUpdated time.Time `json:"-"`

	Role       string        `json:"role"`
	Time       MessageTime   `json:"time"`
	ParentID   string        `json:"parentID"`
	ModelID    string        `json:"modelID"`
	ProviderID string        `json:"providerID"`
	Mode       string        `json:"mode"`
	Agent      string        `json:"agent"`
	Path       MessagePath   `json:"path"`
	Cost       float64       `json:"cost"`
	Tokens     TokenCounts   `json:"tokens"`
	Finish     string        `json:"finish"`
	Error      *MessageError `json:"error"`
}

// MessageTime contains OpenCode's integer millisecond timestamps.
type MessageTime struct {
	Created   int64  `json:"created"`
	Completed *int64 `json:"completed"`
}

// MessagePath records the working directory and project root for an assistant message.
type MessagePath struct {
	CWD  string `json:"cwd"`
	Root string `json:"root"`
}

// TokenCounts is shared by assistant messages and step-finish parts.
type TokenCounts struct {
	Input     int64       `json:"input"`
	Output    int64       `json:"output"`
	Reasoning int64       `json:"reasoning"`
	Cache     CacheCounts `json:"cache"`
	observed  bool
}

// UnmarshalJSON records whether the payload carried any numeric count.
func (t *TokenCounts) UnmarshalJSON(data []byte) error {
	var raw struct {
		Input     *int64 `json:"input"`
		Output    *int64 `json:"output"`
		Reasoning *int64 `json:"reasoning"`
		Cache     struct {
			Read  *int64 `json:"read"`
			Write *int64 `json:"write"`
		} `json:"cache"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*t = TokenCounts{}
	set := func(dst *int64, src *int64) {
		if src != nil {
			*dst = *src
			t.observed = true
		}
	}
	set(&t.Input, raw.Input)
	set(&t.Output, raw.Output)
	set(&t.Reasoning, raw.Reasoning)
	set(&t.Cache.Read, raw.Cache.Read)
	set(&t.Cache.Write, raw.Cache.Write)
	return nil
}

// Observed reports whether at least one numeric token field was present.
func (t TokenCounts) Observed() bool { return t.observed }

// CacheCounts holds OpenCode's disjoint cache token buckets.
type CacheCounts struct {
	Read  int64 `json:"read"`
	Write int64 `json:"write"`
}

// MessageError is a terminal assistant-message error.
type MessageError struct {
	Name string           `json:"name"`
	Data MessageErrorData `json:"data"`
}

// MessageErrorData contains the error fields used by the live mapper.
type MessageErrorData struct {
	StatusCode *int `json:"statusCode"`
}

// Part is one decoded part row. Identity fields come from row columns.
type Part struct {
	ID         string    `json:"-"`
	MessageID  string    `json:"-"`
	SessionID  string    `json:"-"`
	RowUpdated time.Time `json:"-"`

	Type      string      `json:"type"`
	Text      string      `json:"text"`
	Synthetic bool        `json:"synthetic"`
	Ignored   bool        `json:"ignored"`
	CallID    string      `json:"callID"`
	Tool      string      `json:"tool"`
	State     ToolState   `json:"state"`
	Reason    string      `json:"reason"`
	Cost      float64     `json:"cost"`
	Tokens    TokenCounts `json:"tokens"`
	Time      PartTime    `json:"time"`
}

// PartTime contains a part's own timestamps when OpenCode persisted them.
type PartTime struct {
	Start int64  `json:"start"`
	End   *int64 `json:"end"`
}

// ToolState is the persisted state of an OpenCode tool part.
type ToolState struct {
	Status   string          `json:"status"`
	Input    json.RawMessage `json:"input"`
	Output   string          `json:"output"`
	Error    *string         `json:"error"`
	Title    string          `json:"title"`
	Metadata ToolMetadata    `json:"metadata"`
	Time     PartTime        `json:"time"`
}

// ToolMetadata contains persisted task lineage and shell status.
type ToolMetadata struct {
	SessionID string `json:"sessionId"`
	Exit      *int   `json:"exit"`
}

// Open opens path read-only. SQLite connects lazily, so schema errors surface
// on the first query. The mode reads a live write-ahead log; it may create the
// missing shared-memory sidecar SQLite needs to index that log.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", chatstore.URI(path)+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("sessiondb: open %s: %w", path, err)
	}
	return &Store{db: db, path: path}, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// Sessions returns one content-free row per OpenCode session in stable order.
func (s *Store) Sessions(ctx context.Context) ([]Session, error) {
	const query = `select
		s.id,
		s.directory,
		coalesce(s.parent_id, ''),
		s.time_created,
		max(
			s.time_updated,
			coalesce(max(m.time_updated), 0),
			coalesce((select max(p.time_updated) from part p where p.session_id = s.id), 0)
		) as activity_updated,
		count(m.id),
		coalesce(sum(case
			when json_valid(m.data)
			and json_extract(m.data, '$.role') = 'assistant'
			and (json_type(m.data, '$.time.completed') in ('integer', 'real')
				or json_type(m.data, '$.error') = 'object')
			then 1 else 0 end), 0),
		coalesce(sum(length(m.data)), 0)
			+ coalesce((select sum(length(p.data)) from part p where p.session_id = s.id), 0),
		min(case when not json_valid(m.data) then m.id end)
	from session s
	left join message m on m.session_id = s.id
	group by s.id, s.directory, s.parent_id, s.time_created, s.time_updated
	order by activity_updated desc, s.id`
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("sessiondb: list sessions from %s: %w", s.path, err)
	}
	defer func() { _ = rows.Close() }()

	var sessions []Session
	for rows.Next() {
		var row Session
		var createdMS, updatedMS int64
		var malformedMessageID sql.NullString
		if err := rows.Scan(
			&row.ID,
			&row.Directory,
			&row.ParentID,
			&createdMS,
			&updatedMS,
			&row.MessageCount,
			&row.TerminalMessageCount,
			&row.LogicalSizeBytes,
			&malformedMessageID,
		); err != nil {
			return nil, fmt.Errorf("sessiondb: list sessions from %s: %w", s.path, err)
		}
		if malformedMessageID.Valid {
			return nil, fmt.Errorf("sessiondb: decode message %s from %s: malformed JSON", malformedMessageID.String, s.path)
		}
		row.CreatedAt = time.UnixMilli(createdMS)
		row.UpdatedAt = time.UnixMilli(updatedMS)
		sessions = append(sessions, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sessiondb: list sessions from %s: %w", s.path, err)
	}
	return sessions, nil
}

// Session returns one session row without reading message or part data.
func (s *Store) Session(ctx context.Context, sessionID string) (Session, bool, error) {
	const query = `select id, title, directory, coalesce(parent_id, ''), time_created, time_updated
		from session where id = ?`
	var row Session
	var createdMS, updatedMS int64
	err := s.db.QueryRowContext(ctx, query, sessionID).Scan(
		&row.ID,
		&row.Title,
		&row.Directory,
		&row.ParentID,
		&createdMS,
		&updatedMS,
	)
	if err == sql.ErrNoRows {
		return Session{}, false, nil
	}
	if err != nil {
		return Session{}, false, fmt.Errorf("sessiondb: read session %s from %s: %w", sessionID, s.path, err)
	}
	row.CreatedAt = time.UnixMilli(createdMS)
	row.UpdatedAt = time.UnixMilli(updatedMS)
	return row, true, nil
}

// MessageRole extracts one message's role without decoding its payload in Go.
func (s *Store) MessageRole(ctx context.Context, messageID string) (string, bool, error) {
	const query = `select json_extract(data, '$.role') from message where id = ? and json_valid(data)`
	var role string
	err := s.db.QueryRowContext(ctx, query, messageID).Scan(&role)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("sessiondb: read message role for %s from %s: %w", messageID, s.path, err)
	}
	return role, true, nil
}

// SpawningMessageID finds the task call that created a child session.
func (s *Store) SpawningMessageID(ctx context.Context, parentSessionID, childSessionID string) (string, bool, error) {
	const query = `select message_id
		from part
		where session_id = ?
			and json_valid(data)
			and json_extract(data, '$.type') = 'tool'
			and json_extract(data, '$.tool') = 'task'
			and json_extract(data, '$.state.metadata.sessionId') = ?
		order by id
		limit 1`
	var messageID string
	err := s.db.QueryRowContext(ctx, query, parentSessionID, childSessionID).Scan(&messageID)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("sessiondb: find spawning message for %s in %s: %w", childSessionID, s.path, err)
	}
	return messageID, true, nil
}

// Messages yields one session's decoded messages ordered by time_created and ID.
func (s *Store) Messages(ctx context.Context, sessionID string) iter.Seq2[Message, error] {
	return func(yield func(Message, error) bool) {
		const query = `select id, session_id, time_created, time_updated, data
			from message where session_id = ? order by time_created, id`
		rows, err := s.db.QueryContext(ctx, query, sessionID)
		if err != nil {
			yield(Message{}, fmt.Errorf("sessiondb: read messages for %s from %s: %w", sessionID, s.path, err))
			return
		}
		defer func() { _ = rows.Close() }()

		for rows.Next() {
			var msg Message
			var createdMS, updatedMS int64
			var data []byte
			if err := rows.Scan(&msg.ID, &msg.SessionID, &createdMS, &updatedMS, &data); err != nil {
				yield(Message{}, fmt.Errorf("sessiondb: read messages for %s from %s: %w", sessionID, s.path, err))
				return
			}
			if err := decodePayload("message", msg.ID, s.path, data, &msg); err != nil {
				yield(Message{}, err)
				return
			}
			msg.RowCreated = time.UnixMilli(createdMS)
			msg.RowUpdated = time.UnixMilli(updatedMS)
			if !yield(msg, nil) {
				return
			}
		}
		if err := rows.Err(); err != nil {
			yield(Message{}, fmt.Errorf("sessiondb: read messages for %s from %s: %w", sessionID, s.path, err))
		}
	}
}

// Parts returns one message's decoded parts ordered by ID.
func (s *Store) Parts(ctx context.Context, messageID string) ([]Part, error) {
	const query = `select id, message_id, session_id, time_updated, data
		from part where message_id = ? order by id`
	rows, err := s.db.QueryContext(ctx, query, messageID)
	if err != nil {
		return nil, fmt.Errorf("sessiondb: read parts for %s from %s: %w", messageID, s.path, err)
	}
	defer func() { _ = rows.Close() }()

	var parts []Part
	for rows.Next() {
		var part Part
		var updatedMS int64
		var data []byte
		if err := rows.Scan(&part.ID, &part.MessageID, &part.SessionID, &updatedMS, &data); err != nil {
			return nil, fmt.Errorf("sessiondb: read parts for %s from %s: %w", messageID, s.path, err)
		}
		if err := decodePayload("part", part.ID, s.path, data, &part); err != nil {
			return nil, err
		}
		part.RowUpdated = time.UnixMilli(updatedMS)
		parts = append(parts, part)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sessiondb: read parts for %s from %s: %w", messageID, s.path, err)
	}
	return parts, nil
}

func decodePayload(kind, id, path string, data []byte, dst any) error {
	if len(data) > MaxPayloadBytes {
		return fmt.Errorf("sessiondb: decode %s %s from %s: payload is %d bytes, limit is %d", kind, id, path, len(data), MaxPayloadBytes)
	}
	if err := json.Unmarshal(data, dst); err != nil {
		return fmt.Errorf("sessiondb: decode %s %s from %s: %w", kind, id, path, err)
	}
	return nil
}
