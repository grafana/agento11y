// Package warpdb reads Warp agent conversations from Warp's SQLite store
// (warp.sqlite). The Warp app and the Warp Agent CLI keep separate databases
// with the same schema.
package warpdb

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/chatstore"
	_ "modernc.org/sqlite"
)

// MaxTaskBytes is the largest agent_tasks.task value decoded. Long-running
// conversations reach tens of megabytes. A conversation with a larger task is
// left out of discovery. It is a variable so tests can lower it.
var MaxTaskBytes = 128 * 1024 * 1024

// Store is an open read-only Warp database.
type Store struct {
	db   *sql.DB
	path string
}

// Conversation is one content-free agent conversation summary.
type Conversation struct {
	ID        string
	Workspace string // most recent working directory recorded for a query
	StartedAt time.Time
	UpdatedAt time.Time
	SizeBytes int64
	Requests  int // distinct agent requests across the conversation's tasks
	// Oversized is set when a task exceeds MaxTaskBytes; Tasks refuses it.
	Oversized bool
}

// Open opens path read-only. The mode reads a live write-ahead log; it may
// create the missing shared-memory sidecar SQLite needs to index that log.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", chatstore.URI(path)+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("warpdb: open %s: %w", path, err)
	}
	return &Store{db: db, path: path}, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// Conversations returns every conversation that has at least one task, most
// recently updated first. It scans task payloads for request IDs only.
func (s *Store) Conversations(ctx context.Context) ([]Conversation, error) {
	const query = `select
		c.conversation_id,
		c.last_modified_at,
		coalesce(max(t.last_modified_at), ''),
		coalesce(sum(length(t.task)), 0) + length(c.conversation_data),
		coalesce((select min(q.start_ts) from ai_queries q where q.conversation_id = c.conversation_id), ''),
		coalesce((select q.working_directory from ai_queries q
			where q.conversation_id = c.conversation_id and coalesce(q.working_directory, '') != ''
			order by q.start_ts desc limit 1), '')
	from agent_conversations c
	join agent_tasks t on t.conversation_id = c.conversation_id
	group by c.id, c.conversation_id, c.last_modified_at, c.conversation_data`
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("warpdb: list conversations from %s: %w", s.path, err)
	}
	defer func() { _ = rows.Close() }()

	var out []Conversation
	byID := map[string]int{}
	for rows.Next() {
		var conv Conversation
		var convModified, taskModified, started string
		if err := rows.Scan(&conv.ID, &convModified, &taskModified, &conv.SizeBytes, &started, &conv.Workspace); err != nil {
			return nil, fmt.Errorf("warpdb: list conversations from %s: %w", s.path, err)
		}
		conv.UpdatedAt = latest(parseTime(convModified), parseTime(taskModified))
		conv.StartedAt = parseTime(started)
		if conv.StartedAt.IsZero() || conv.StartedAt.After(conv.UpdatedAt) {
			conv.StartedAt = conv.UpdatedAt
		}
		byID[conv.ID] = len(out)
		out = append(out, conv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("warpdb: list conversations from %s: %w", s.path, err)
	}
	if err := s.countRequests(ctx, out, byID); err != nil {
		return nil, err
	}
	sortConversations(out)
	return out, nil
}

func (s *Store) countRequests(ctx context.Context, convs []Conversation, byID map[string]int) error {
	rows, err := s.db.QueryContext(ctx, `select conversation_id, task from agent_tasks order by id`)
	if err != nil {
		return fmt.Errorf("warpdb: scan tasks in %s: %w", s.path, err)
	}
	defer func() { _ = rows.Close() }()

	seen := map[string]map[string]bool{}
	for rows.Next() {
		var convID string
		var blob []byte
		if err := rows.Scan(&convID, &blob); err != nil {
			return fmt.Errorf("warpdb: scan tasks in %s: %w", s.path, err)
		}
		i, ok := byID[convID]
		if !ok {
			continue
		}
		if len(blob) > MaxTaskBytes {
			// Tasks refuses the conversation, so discovery must not plan it.
			convs[i].Oversized = true
			continue
		}
		ids, err := RequestIDs(blob)
		if err != nil {
			return fmt.Errorf("warpdb: scan task in conversation %s from %s: %w", convID, s.path, err)
		}
		if seen[convID] == nil {
			seen[convID] = map[string]bool{}
		}
		for _, id := range ids {
			if !seen[convID][id] {
				seen[convID][id] = true
				convs[i].Requests++
			}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("warpdb: scan tasks in %s: %w", s.path, err)
	}
	return nil
}

// Tasks decodes every task of one conversation in row order.
func (s *Store) Tasks(ctx context.Context, conversationID string) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx,
		`select task_id, task from agent_tasks where conversation_id = ? order by id`, conversationID)
	if err != nil {
		return nil, fmt.Errorf("warpdb: read tasks for %s from %s: %w", conversationID, s.path, err)
	}
	defer func() { _ = rows.Close() }()

	var tasks []Task
	for rows.Next() {
		var taskID string
		var blob []byte
		if err := rows.Scan(&taskID, &blob); err != nil {
			return nil, fmt.Errorf("warpdb: read tasks for %s from %s: %w", conversationID, s.path, err)
		}
		if len(blob) > MaxTaskBytes {
			return nil, fmt.Errorf("warpdb: task %s in %s is %d bytes, limit is %d", taskID, s.path, len(blob), MaxTaskBytes)
		}
		task, err := DecodeTask(blob)
		if err != nil {
			return nil, fmt.Errorf("warpdb: task %s in %s: %w", taskID, s.path, err)
		}
		if task.ID == "" {
			task.ID = taskID
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("warpdb: read tasks for %s from %s: %w", conversationID, s.path, err)
	}
	return tasks, nil
}

// sqliteTimeLayouts covers SQLite CURRENT_TIMESTAMP and Diesel's
// NaiveDateTime text, both written in UTC.
var sqliteTimeLayouts = []string{
	"2006-01-02 15:04:05.999999999",
	"2006-01-02T15:04:05.999999999",
	time.RFC3339Nano,
}

func parseTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	for _, layout := range sqliteTimeLayouts {
		if t, err := time.ParseInLocation(layout, raw, time.UTC); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

func latest(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func sortConversations(convs []Conversation) {
	sort.Slice(convs, func(i, j int) bool {
		a, b := convs[i], convs[j]
		if !a.UpdatedAt.Equal(b.UpdatedAt) {
			return a.UpdatedAt.After(b.UpdatedAt)
		}
		return a.ID < b.ID
	})
}
