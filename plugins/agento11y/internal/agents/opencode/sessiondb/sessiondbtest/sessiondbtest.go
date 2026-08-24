// Package sessiondbtest writes OpenCode session databases for tests.
package sessiondbtest

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/chatstore"
	_ "modernc.org/sqlite"
)

const schema = `
create table session (
	id text primary key,
	parent_id text,
	directory text not null,
	title text not null,
	time_created integer not null,
	time_updated integer not null
);
create table message (
	id text primary key,
	session_id text not null,
	time_created integer not null,
	time_updated integer not null,
	data text not null
);
create table part (
	id text primary key,
	message_id text not null,
	session_id text not null,
	time_created integer not null,
	time_updated integer not null,
	data text not null
);`

// Builder owns one temporary OpenCode database.
type Builder struct {
	Path string
	DB   *sql.DB
	t    testing.TB
}

// New creates an empty OpenCode database.
func New(t testing.TB) *Builder {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode.db")
	db, err := sql.Open("sqlite", chatstore.URI(path))
	if err != nil {
		t.Fatalf("sessiondbtest: open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("sessiondbtest: create schema: %v", err)
	}
	return &Builder{Path: path, DB: db, t: t}
}

// AddSession inserts one session row.
func (b *Builder) AddSession(id, parent, title, directory string, created, updated int64) {
	b.t.Helper()
	var parentValue any
	if parent != "" {
		parentValue = parent
	}
	if _, err := b.DB.Exec(`insert into session
		(id, parent_id, directory, title, time_created, time_updated)
		values (?, ?, ?, ?, ?, ?)`, id, parentValue, directory, title, created, updated); err != nil {
		b.t.Fatalf("sessiondbtest: insert session: %v", err)
	}
}

// AddMessage inserts one message row.
func (b *Builder) AddMessage(id, sessionID string, created, updated int64, data string) {
	b.t.Helper()
	if _, err := b.DB.Exec(`insert into message
		(id, session_id, time_created, time_updated, data) values (?, ?, ?, ?, ?)`,
		id, sessionID, created, updated, data); err != nil {
		b.t.Fatalf("sessiondbtest: insert message: %v", err)
	}
}

// AddPart inserts one part row.
func (b *Builder) AddPart(id, messageID, sessionID string, created, updated int64, data string) {
	b.t.Helper()
	if _, err := b.DB.Exec(`insert into part
		(id, message_id, session_id, time_created, time_updated, data) values (?, ?, ?, ?, ?, ?)`,
		id, messageID, sessionID, created, updated, data); err != nil {
		b.t.Fatalf("sessiondbtest: insert part: %v", err)
	}
}
