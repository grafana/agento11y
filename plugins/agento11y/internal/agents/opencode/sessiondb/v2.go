package sessiondb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Provider state, attachments, snapshots and arbitrary metadata are not decoded.
type V2Message struct {
	ID         string    `json:"-"`
	SessionID  string    `json:"-"`
	Type       string    `json:"-"`
	Seq        int64     `json:"-"`
	RowCreated time.Time `json:"-"`
	RowUpdated time.Time `json:"-"`

	Time     V2Time      `json:"time"`
	Text     string      `json:"text"`
	Agent    string      `json:"agent"`
	Model    V2Model     `json:"model"`
	Content  []V2Content `json:"content"`
	Finish   string      `json:"finish"`
	Cost     *float64    `json:"cost"`
	Tokens   *V2Tokens   `json:"tokens"`
	Error    *V2Error    `json:"error"`
	Location V2Location  `json:"location"`
	Previous *V2Previous `json:"-"`
}

func (m V2Message) Terminal() bool {
	return m.Type == "assistant" && (m.Time.Completed != nil || m.Error != nil)
}

type V2Time struct {
	Created   int64  `json:"created"`
	Streamed  *int64 `json:"streamed"`
	Completed *int64 `json:"completed"`
	Ran       *int64 `json:"ran"`
}

type V2Model struct {
	ID         string `json:"id"`
	ProviderID string `json:"providerID"`
	Variant    string `json:"variant"`
}

type V2Tokens struct {
	Input     *int64 `json:"input"`
	Output    *int64 `json:"output"`
	Reasoning *int64 `json:"reasoning"`
	Cache     struct {
		Read  *int64 `json:"read"`
		Write *int64 `json:"write"`
	} `json:"cache"`
}

// UnmarshalJSON accepts integral JSON numbers, including decimal/exponent notation.
func (t *V2Tokens) UnmarshalJSON(data []byte) error {
	var raw struct {
		Input, Output, Reasoning json.RawMessage
		Cache                    struct{ Read, Write json.RawMessage }
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return errors.New("invalid token counts")
	}
	*t = V2Tokens{}
	pairs := []struct {
		src json.RawMessage
		dst **int64
	}{{raw.Input, &t.Input}, {raw.Output, &t.Output}, {raw.Reasoning, &t.Reasoning}, {raw.Cache.Read, &t.Cache.Read}, {raw.Cache.Write, &t.Cache.Write}}
	var total int64
	for _, pair := range pairs {
		if len(pair.src) == 0 || string(pair.src) == "null" {
			continue
		}
		n, err := v2Count(string(pair.src))
		if err != nil {
			return err
		}
		if n > math.MaxInt64-total {
			return errors.New("token count overflow")
		}
		total += n
		*pair.dst = &n
	}
	return nil
}

// v2Count checks decimal digits without floating-point rounding or allocating
// storage proportional to an untrusted exponent. JSON syntax was checked above.
func v2Count(text string) (int64, error) {
	invalid := errors.New("invalid token counts")
	if text == "" || text[0] == '"' {
		return 0, invalid
	}
	mantissa := text
	var exponent int64
	var exponentErr error
	if i := strings.IndexAny(text, "eE"); i >= 0 {
		mantissa = text[:i]
		exponent, exponentErr = strconv.ParseInt(text[i+1:], 10, 64)
	}
	negative := strings.HasPrefix(mantissa, "-")
	mantissa = strings.TrimPrefix(mantissa, "-")
	var fractional int64
	if i := strings.IndexByte(mantissa, '.'); i >= 0 {
		fractional = int64(len(mantissa) - i - 1)
		mantissa = mantissa[:i] + mantissa[i+1:]
	}
	digits := strings.TrimLeft(mantissa, "0")
	if digits == "" {
		return 0, nil
	}
	if negative || exponentErr != nil || exponent < -MaxPayloadBytes || exponent > MaxPayloadBytes+19 {
		return 0, invalid
	}
	scale := exponent - fractional
	if scale < 0 {
		if -scale >= int64(len(digits)) {
			return 0, invalid
		}
		cut := len(digits) + int(scale)
		if strings.Trim(digits[cut:], "0") != "" {
			return 0, invalid
		}
		digits = digits[:cut]
	} else {
		if int64(len(digits))+scale > 19 {
			return 0, invalid
		}
		digits += strings.Repeat("0", int(scale))
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, invalid
	}
	return n, nil
}

type V2Content struct {
	Type  string      `json:"type"`
	Text  string      `json:"text"`
	ID    string      `json:"id"`
	Name  string      `json:"name"`
	State V2ToolState `json:"state"`
	Time  V2Time      `json:"time"`
}

// UnmarshalJSON does not interpret text/reasoning provider state as tool state.
func (c *V2Content) UnmarshalJSON(data []byte) error {
	var base struct {
		Type string `json:"type"`
		Text string `json:"text"`
		Time V2Time `json:"time"`
	}
	if err := json.Unmarshal(data, &base); err != nil {
		return err
	}
	*c = V2Content{Type: base.Type, Text: base.Text, Time: base.Time}
	if base.Type == "tool" {
		type plain V2Content
		return json.Unmarshal(data, (*plain)(c))
	}
	return nil
}

// V2ToolState.Input is a JSON string while streaming and an object afterward.
type V2ToolState struct {
	Status   string          `json:"status"`
	Input    json.RawMessage `json:"input"`
	Content  []V2ToolContent `json:"content"`
	Error    *V2Error        `json:"error"`
	Metadata V2ToolMetadata  `json:"metadata"`
}

type V2ToolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type V2ToolMetadata struct {
	SessionID       string `json:"sessionID"`
	LegacySessionID string `json:"sessionId"`
	Exit            *int64 `json:"exit"`
}

// Tool metadata keys are not reserved; wrong-type hints are valid custom data.
func (m *V2ToolMetadata) UnmarshalJSON(data []byte) error {
	var raw struct {
		SessionID       json.RawMessage `json:"sessionID"`
		LegacySessionID json.RawMessage `json:"sessionId"`
		Exit            json.RawMessage `json:"exit"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*m = V2ToolMetadata{}
	_ = json.Unmarshal(raw.SessionID, &m.SessionID)
	_ = json.Unmarshal(raw.LegacySessionID, &m.LegacySessionID)
	var exit *int64
	if json.Unmarshal(raw.Exit, &exit) == nil {
		m.Exit = exit
	}
	return nil
}

// V2Error omits response bodies. Only tool errors use Message; sanitize it before export.
type V2Error struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Status  *int   `json:"status"`
}

type V2Location struct {
	Directory string `json:"directory"`
}

type V2Previous struct {
	Location V2Location `json:"location"`
}

func migrationReady(ctx context.Context, q queryer, tables map[string]map[string]bool, path string) error {
	fail := func(reason string) error { return &SchemaError{Path: path, Layout: LayoutV2, Reason: reason} }
	var legacy bool
	if _, ok := tables["session"]; ok {
		if err := q.QueryRowContext(ctx, `select exists(select 1 from session)`).Scan(&legacy); err != nil {
			return fmt.Errorf("sessiondb: check legacy sessions in %s: %w", path, err)
		}
	}
	var phase string
	if columns, ok := tables["kv"]; ok {
		if missing := missingColumns(columns, []string{"key", "value"}); len(missing) > 0 {
			return fail("table kv is missing " + named("column", missing))
		}
		// SQLite reads only the phase. Invalid state and cursor contents never enter Go.
		var state sql.NullString
		err := q.QueryRowContext(ctx, `select case when json_valid(value)
			then case json_extract(value, '$.phase') when 'sessions' then 'sessions' when 'completed' then 'completed' end end
			from kv where key = 'migration.v1-v2'`).Scan(&state)
		phase = state.String
		if err != nil && err != sql.ErrNoRows {
			return fmt.Errorf("sessiondb: read migration state in %s: %w", path, err)
		}
	}
	if phase == "sessions" {
		return fail("v1-v2 migration is in progress")
	}
	if legacy && phase != "completed" {
		return fail("legacy session rows require a completed v1-v2 migration")
	}
	return nil
}

func (s *Store) v2Sessions(ctx context.Context) ([]Session, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("sessiondb: list sessions from %s: %w", s.path, err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := requireV2(ctx, tx, s.path); err != nil {
		return nil, err
	}
	const query = `select s.id, s.directory, coalesce(s.parent_id, ''), coalesce(s.fork_session_id, ''),
		s.time_created, max(s.time_updated, coalesce(max(m.time_updated), 0)), s.time_suspended is not null,
		count(m.id), coalesce(sum(case when json_valid(m.data) then case
			when m.type = 'assistant' and (json_type(m.data, '$.time.completed') in ('integer', 'real')
				or json_type(m.data, '$.error') = 'object') then 1 else 0 end else 0 end), 0),
		coalesce(sum(length(cast(m.data as blob))), 0),
		min(case when not json_valid(m.data) then m.id end)
		from session_v2 s left join session_message m on m.session_id = s.id
		group by s.id order by 6 desc, s.id`
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("sessiondb: list sessions from %s: %w", s.path, err)
	}
	defer func() { _ = rows.Close() }()
	var sessions []Session
	for rows.Next() {
		var row Session
		var created, updated int64
		var malformed sql.NullString
		if err := rows.Scan(&row.ID, &row.Directory, &row.ParentID, &row.ForkSessionID, &created, &updated,
			&row.Active, &row.MessageCount, &row.TerminalMessageCount, &row.LogicalSizeBytes, &malformed); err != nil {
			return nil, fmt.Errorf("sessiondb: list sessions from %s: %w", s.path, err)
		}
		if malformed.Valid {
			return nil, fmt.Errorf("sessiondb: decode message %s from %s: malformed JSON", malformed.String, s.path)
		}
		row.Title = row.ID
		row.CreatedAt, row.UpdatedAt = time.UnixMilli(created), time.UnixMilli(updated)
		sessions = append(sessions, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sessiondb: list sessions from %s: %w", s.path, err)
	}
	return sessions, nil
}

func requireV2(ctx context.Context, q queryer, path string) (Layout, error) {
	layout, err := inspectLayout(ctx, q, path)
	if err != nil {
		return layout, err
	}
	if layout != LayoutV2 {
		return layout, &SchemaError{Path: path, Layout: layout, Reason: "selected v2 layout is no longer present"}
	}
	return layout, nil
}

func v2Session(ctx context.Context, q queryer, path, id string) (Session, bool, error) {
	var row Session
	var created, updated int64
	err := q.QueryRowContext(ctx, `select id, coalesce(title, ''), directory, coalesce(parent_id, ''),
		coalesce(fork_session_id, ''), time_created, time_updated, time_suspended is not null
		from session_v2 where id = ?`, id).Scan(&row.ID, &row.Title, &row.Directory, &row.ParentID,
		&row.ForkSessionID, &created, &updated, &row.Active)
	if err == sql.ErrNoRows {
		return Session{}, false, nil
	}
	if err != nil {
		return Session{}, false, fmt.Errorf("sessiondb: read session %s from %s: %w", id, path, err)
	}
	row.CreatedAt, row.UpdatedAt = time.UnixMilli(created), time.UnixMilli(updated)
	return row, true, nil
}

// V2Snapshot owns one read-only transaction for a selected session and its lineage.
// Call Close even if Messages is never called. It is safe to close more than once.
type V2Snapshot struct {
	tx        *sql.Tx
	path      string
	sessionID string
	once      sync.Once
	closeErr  error
}

func (s *Store) V2Snapshot(ctx context.Context, sessionID string) (*V2Snapshot, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("sessiondb: open snapshot for %s from %s: %w", sessionID, s.path, err)
	}
	snapshot := &V2Snapshot{tx: tx, path: s.path, sessionID: sessionID}
	if _, err = requireV2(ctx, tx, s.path); err != nil {
		_ = snapshot.Close()
		return nil, err
	}
	row, ok, err := snapshot.Session(ctx, sessionID)
	if err != nil {
		_ = snapshot.Close()
		return nil, err
	}
	if !ok || row.Active {
		_ = snapshot.Close()
		reason := "session no longer exists"
		if ok {
			reason = "session has an unresolved execution claim"
		}
		return nil, fmt.Errorf("sessiondb: select session %s from %s: %s", sessionID, s.path, reason)
	}
	return snapshot, nil
}

func (s *V2Snapshot) Close() error {
	s.once.Do(func() {
		s.closeErr = s.tx.Rollback()
		if errors.Is(s.closeErr, sql.ErrTxDone) {
			s.closeErr = nil
		}
	})
	return s.closeErr
}

// Session permits stored titles only after selection, within the same snapshot.
func (s *V2Snapshot) Session(ctx context.Context, id string) (Session, bool, error) {
	return v2Session(ctx, s.tx, s.path, id)
}

// Messages streams by seq and closes the entire snapshot when iteration stops.
// Use V2Messages for preliminary lineage/location scans before this final pass.
func (s *V2Snapshot) Messages(ctx context.Context, id string) iter.Seq2[V2Message, error] {
	return func(yield func(V2Message, error) bool) {
		defer func() { _ = s.Close() }()
		if id != s.sessionID {
			yield(V2Message{}, fmt.Errorf("sessiondb: snapshot for %s cannot import session %s from %s", s.sessionID, id, s.path))
			return
		}
		for msg, err := range s.V2Messages(ctx, id) {
			if !yield(msg, err) {
				return
			}
		}
	}
}

const v2MessagesQuery = `select id, session_id, type, seq, time_created, time_updated,
	length(cast(data as blob)), case when length(cast(data as blob)) <= ? then data end
	from session_message where session_id = ? order by seq`

// V2Messages closes its rows on any exit but retains the transaction for another
// scan. The owner must Close the snapshot, or finish with Messages.
func (s *V2Snapshot) V2Messages(ctx context.Context, id string) iter.Seq2[V2Message, error] {
	return func(yield func(V2Message, error) bool) {
		rows, err := s.tx.QueryContext(ctx, v2MessagesQuery, MaxPayloadBytes, id)
		if err != nil {
			yield(V2Message{}, fmt.Errorf("sessiondb: read messages for %s from %s: %w", id, s.path, err))
			return
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			if err := ctx.Err(); err != nil {
				yield(V2Message{}, fmt.Errorf("sessiondb: read messages for %s from %s: %w", id, s.path, err))
				return
			}
			var msg V2Message
			var created, updated, size int64
			var data []byte
			if err := rows.Scan(&msg.ID, &msg.SessionID, &msg.Type, &msg.Seq, &created, &updated, &size, &data); err != nil {
				yield(V2Message{}, fmt.Errorf("sessiondb: read messages for %s from %s: %w", id, s.path, err))
				return
			}
			if size > MaxPayloadBytes {
				yield(V2Message{}, fmt.Errorf("sessiondb: decode message %s from %s: payload is %d bytes, limit is %d", msg.ID, s.path, size, MaxPayloadBytes))
				return
			}
			if err := decodeV2(data, &msg); err != nil {
				yield(V2Message{}, fmt.Errorf("sessiondb: decode message %s from %s: invalid JSON projection", msg.ID, s.path))
				return
			}
			msg.RowCreated, msg.RowUpdated = time.UnixMilli(created), time.UnixMilli(updated)
			if !yield(msg, nil) {
				return
			}
		}
		if err := ctx.Err(); err != nil {
			yield(V2Message{}, fmt.Errorf("sessiondb: read messages for %s from %s: %w", id, s.path, err))
		} else if err := rows.Err(); err != nil {
			yield(V2Message{}, fmt.Errorf("sessiondb: read messages for %s from %s: %w", id, s.path, err))
		}
	}
}

func decodeV2(data []byte, msg *V2Message) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return errors.New("invalid projection")
	}
	if err := json.Unmarshal(data, msg); err != nil {
		return err
	}
	// Other switch records have string/model previous values, not locations.
	if msg.Type == "location-switched" {
		if previous, ok := object["previous"]; ok {
			if err := json.Unmarshal(previous, &msg.Previous); err != nil {
				return err
			}
		}
	}
	return nil
}
