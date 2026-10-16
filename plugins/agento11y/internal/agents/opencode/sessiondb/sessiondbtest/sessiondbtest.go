// Package sessiondbtest writes OpenCode session databases for tests.
package sessiondbtest

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/chatstore"
	_ "modernc.org/sqlite"
)

const v1Schema = `
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

// sessionMessageSchema is shared by OpenCode v1.18.29 and v2, so either option
// may create it.
const sessionMessageSchema = `
create table if not exists session_message (
	id text primary key,
	session_id text not null,
	type text not null,
	seq integer not null,
	time_created integer not null,
	time_updated integer not null,
	data text not null
);`

// sessionV2Schema copies session_v2 from OpenCode's
// packages/core/src/database/schema.gen.ts, without its foreign key.
const sessionV2Schema = `
create table session_v2 (
	id text primary key,
	project_id text not null,
	workspace_id text,
	parent_id text,
	fork_session_id text,
	fork_boundary text,
	slug text not null,
	directory text not null,
	path text,
	title text,
	version text not null,
	share_url text,
	summary_additions integer,
	summary_deletions integer,
	summary_files integer,
	summary_diffs text,
	metadata text,
	cost real default 0 not null,
	tokens_input integer default 0 not null,
	tokens_output integer default 0 not null,
	tokens_reasoning integer default 0 not null,
	tokens_cache_read integer default 0 not null,
	tokens_cache_write integer default 0 not null,
	revert text,
	permission text,
	agent text,
	model text,
	time_created integer not null,
	time_updated integer not null,
	time_idle integer,
	time_viewed integer,
	idle_outcome text,
	time_compacting integer,
	time_archived integer,
	time_suspended integer,
	resume_attempts integer default 0 not null
);`

// Option changes the schema New creates. With no options it is OpenCode v1.
type Option func(*config)

type config struct {
	withoutV1  bool
	statements []string
	drops      []string
}

// WithoutV1 omits the v1 session, message, and part tables.
func WithoutV1() Option {
	return func(c *config) { c.withoutV1 = true }
}

// WithSessionMessage adds the session_message table OpenCode v1.18.29 creates
// beside the v1 tables.
func WithSessionMessage() Option {
	return func(c *config) { c.statements = append(c.statements, sessionMessageSchema) }
}

// WithSessionV2 adds OpenCode v2's session_v2 and session_message tables.
func WithSessionV2() Option {
	return func(c *config) { c.statements = append(c.statements, sessionV2Schema, sessionMessageSchema) }
}

func WithMigrationState() Option {
	return func(c *config) {
		c.statements = append(c.statements, `create table kv (key text primary key, value text)`)
	}
}

// WithoutColumn drops one column after the other options have run.
func WithoutColumn(table, column string) Option {
	return func(c *config) {
		c.drops = append(c.drops, fmt.Sprintf("alter table %q drop column %q", table, column))
	}
}

// Builder owns one temporary OpenCode database.
type Builder struct {
	Path string
	DB   *sql.DB
	t    testing.TB
}

// New creates an empty OpenCode database.
func New(t testing.TB, opts ...Option) *Builder {
	t.Helper()
	var cfg config
	for _, opt := range opts {
		opt(&cfg)
	}
	var statements []string
	if !cfg.withoutV1 {
		statements = append(statements, v1Schema)
	}
	statements = append(append(statements, cfg.statements...), cfg.drops...)

	path := filepath.Join(t.TempDir(), "opencode.db")
	db, err := sql.Open("sqlite", chatstore.URI(path))
	if err != nil {
		t.Fatalf("sessiondbtest: open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	// Connecting creates the file, which a schema with no tables still needs.
	if err := db.Ping(); err != nil {
		t.Fatalf("sessiondbtest: create database: %v", err)
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("sessiondbtest: create schema: %v", err)
		}
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

// AddSessionV2 inserts one session_v2 row. It needs [WithSessionV2].
func (b *Builder) AddSessionV2(id, title, directory string, created, updated int64) {
	b.t.Helper()
	if _, err := b.DB.Exec(`insert into session_v2
		(id, project_id, slug, directory, title, version, time_created, time_updated)
		values (?, 'global', ?, ?, ?, '2.0.0', ?, ?)`, id, id, directory, title, created, updated); err != nil {
		b.t.Fatalf("sessiondbtest: insert session_v2: %v", err)
	}
}

func (b *Builder) SetV2Lineage(id, parent, fork string) {
	b.t.Helper()
	if _, err := b.DB.Exec(`update session_v2 set parent_id = nullif(?, ''), fork_session_id = nullif(?, '') where id = ?`, parent, fork, id); err != nil {
		b.t.Fatalf("sessiondbtest: set lineage: %v", err)
	}
}

// A nil timestamp releases the execution claim.
func (b *Builder) SetV2Claim(id string, timestamp *int64) {
	b.t.Helper()
	if _, err := b.DB.Exec(`update session_v2 set time_suspended = ? where id = ?`, timestamp, id); err != nil {
		b.t.Fatalf("sessiondbtest: set claim: %v", err)
	}
}

func (b *Builder) SetMigrationState(data string) {
	b.t.Helper()
	if _, err := b.DB.Exec(`insert or replace into kv (key, value) values ('migration.v1-v2', ?)`, data); err != nil {
		b.t.Fatalf("sessiondbtest: set migration state: %v", err)
	}
}

func (b *Builder) AddV2Message(id, sessionID, kind string, seq, created, updated int64, data string) {
	b.t.Helper()
	if _, err := b.DB.Exec(`insert into session_message
		(id, session_id, type, seq, time_created, time_updated, data) values (?, ?, ?, ?, ?, ?, ?)`,
		id, sessionID, kind, seq, created, updated, data); err != nil {
		b.t.Fatalf("sessiondbtest: insert session_message: %v", err)
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
