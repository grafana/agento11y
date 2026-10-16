// Package warpdbtest writes Warp databases and task payloads for tests.
package warpdbtest

import (
	"database/sql"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/chatstore"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	_ "modernc.org/sqlite"
)

// schema is the subset of Warp's tables the reader queries.
const schema = `
create table ai_queries (
	id integer primary key not null,
	exchange_id text not null,
	conversation_id text not null,
	start_ts datetime not null,
	input text not null,
	working_directory text,
	output_status text not null,
	model_id text not null default ''
);
create table agent_conversations (
	id integer primary key not null,
	conversation_id text not null,
	conversation_data text not null,
	last_modified_at timestamp not null default current_timestamp
);
create table agent_tasks (
	id integer primary key not null,
	conversation_id text not null,
	task_id text not null,
	task blob not null,
	last_modified_at timestamp not null default current_timestamp
);`

// Builder owns one temporary Warp database.
type Builder struct {
	Path string
	DB   *sql.DB
	t    testing.TB
}

// New creates an empty Warp database named warp.sqlite.
func New(t testing.TB) *Builder {
	t.Helper()
	path := filepath.Join(t.TempDir(), "warp.sqlite")
	db, err := sql.Open("sqlite", chatstore.URI(path))
	if err != nil {
		t.Fatalf("warpdbtest: open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("warpdbtest: create schema: %v", err)
	}
	return &Builder{Path: path, DB: db, t: t}
}

// SQLiteTime formats t the way Warp's SQLite columns store it.
func SQLiteTime(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05.000000") }

// AddConversation inserts one agent_conversations row.
func (b *Builder) AddConversation(id string, modified time.Time) {
	b.t.Helper()
	if _, err := b.DB.Exec(`insert into agent_conversations
		(conversation_id, conversation_data, last_modified_at) values (?, '{}', ?)`,
		id, SQLiteTime(modified)); err != nil {
		b.t.Fatalf("warpdbtest: insert conversation: %v", err)
	}
}

// AddQuery inserts one ai_queries row.
func (b *Builder) AddQuery(exchangeID, conversationID, workingDirectory string, started time.Time) {
	b.t.Helper()
	if _, err := b.DB.Exec(`insert into ai_queries
		(exchange_id, conversation_id, start_ts, input, working_directory, output_status)
		values (?, ?, ?, '[]', ?, '"Completed"')`,
		exchangeID, conversationID, SQLiteTime(started), workingDirectory); err != nil {
		b.t.Fatalf("warpdbtest: insert query: %v", err)
	}
}

// AddTask inserts one agent_tasks row holding payload.
func (b *Builder) AddTask(conversationID, taskID string, payload []byte, modified time.Time) {
	b.t.Helper()
	if _, err := b.DB.Exec(`insert into agent_tasks
		(conversation_id, task_id, task, last_modified_at) values (?, ?, ?, ?)`,
		conversationID, taskID, payload, SQLiteTime(modified)); err != nil {
		b.t.Fatalf("warpdbtest: insert task: %v", err)
	}
}

// Task encodes a Task with the given messages.
func Task(id, description, parentTaskID string, messages ...[]byte) []byte {
	var b []byte
	b = appendString(b, 1, id)
	b = appendString(b, 2, description)
	if parentTaskID != "" {
		b = appendMessage(b, 3, appendString(nil, 1, parentTaskID))
	}
	for _, msg := range messages {
		b = appendMessage(b, 5, msg)
	}
	return b
}

// Header identifies one message.
type Header struct {
	ID        string
	TaskID    string
	RequestID string
	At        time.Time
}

func message(h Header, variant protowire.Number, body []byte) []byte {
	var b []byte
	b = appendString(b, 1, h.ID)
	b = appendMessage(b, variant, body)
	b = appendString(b, 11, h.TaskID)
	b = appendString(b, 13, h.RequestID)
	if !h.At.IsZero() {
		b = appendMessage(b, 14, Timestamp(h.At))
	}
	return b
}

// UserQuery encodes a user query message.
func UserQuery(h Header, query string) []byte {
	return message(h, 2, appendString(nil, 1, query))
}

// AgentOutput encodes an agent text output message.
func AgentOutput(h Header, text string) []byte {
	return message(h, 3, appendString(nil, 1, text))
}

// Reasoning encodes an agent reasoning message.
func Reasoning(h Header, text string) []byte {
	return message(h, 15, appendString(nil, 1, text))
}

// ModelUsed encodes a model-used message.
func ModelUsed(h Header, modelID, displayName string) []byte {
	return message(h, 25, appendString(appendString(nil, 1, modelID), 2, displayName))
}

// ShellCall encodes a run_shell_command tool call.
func ShellCall(h Header, callID, command string) []byte {
	return toolCall(h, callID, 2, appendString(nil, 1, command))
}

// ReadFilesCall encodes a read_files tool call.
func ReadFilesCall(h Header, callID string, paths ...string) []byte {
	var body []byte
	for _, path := range paths {
		body = appendMessage(body, 1, appendString(nil, 1, path))
	}
	return toolCall(h, callID, 5, body)
}

// MCPCall encodes a call_mcp_tool tool call.
func MCPCall(h Header, callID, name string, args map[string]any) []byte {
	body := appendString(nil, 1, name)
	if args != nil {
		s, err := structpb.NewStruct(args)
		if err != nil {
			panic(err)
		}
		raw, err := proto.Marshal(s)
		if err != nil {
			panic(err)
		}
		body = appendMessage(body, 2, raw)
	}
	return toolCall(h, callID, 12, body)
}

// OtherCall encodes a tool call for the given ToolCall.tool oneof field with
// an empty body.
func OtherCall(h Header, callID string, field protowire.Number) []byte {
	return toolCall(h, callID, field, nil)
}

func toolCall(h Header, callID string, field protowire.Number, body []byte) []byte {
	b := appendString(nil, 1, callID)
	b = appendMessage(b, field, body)
	return message(h, 4, b)
}

// ShellResult encodes a finished run_shell_command result.
func ShellResult(h Header, callID, output string, exitCode int32) []byte {
	finished := appendString(nil, 1, output)
	finished = protowire.AppendTag(finished, 2, protowire.VarintType)
	finished = protowire.AppendVarint(finished, uint64(int64(exitCode)))
	return toolResult(h, callID, 2, appendMessage(nil, 5, finished))
}

// MCPResult encodes a successful call_mcp_tool result with text parts.
func MCPResult(h Header, callID string, texts ...string) []byte {
	var success []byte
	for _, text := range texts {
		success = appendMessage(success, 1, appendMessage(nil, 1, appendString(nil, 1, text)))
	}
	return toolResult(h, callID, 16, appendMessage(nil, 1, success))
}

// CancelResult encodes a cancelled tool result.
func CancelResult(h Header, callID string) []byte {
	return toolResult(h, callID, 14, nil)
}

// OtherResult encodes a tool result for the given ToolCallResult.result oneof
// field with an empty body.
func OtherResult(h Header, callID string, field protowire.Number) []byte {
	return toolResult(h, callID, field, nil)
}

func toolResult(h Header, callID string, field protowire.Number, body []byte) []byte {
	b := appendString(nil, 1, callID)
	b = appendMessage(b, field, body)
	return message(h, 5, b)
}

// Usage is the usage charged to one model.
type Usage struct {
	Category   string
	Model      string
	BYOK       bool
	Input      uint64
	Output     uint64
	CacheRead  uint64
	CacheWrite uint64
	InputCost  float32 // cents
	OutputCost float32 // cents
}

// Outcome values match Message.RequestMetadata.Outcome.
const (
	OutcomeCompleted = 1
	OutcomeCanceled  = 2
	OutcomeErrored   = 3
)

// RequestMetadata encodes a request metadata message.
func RequestMetadata(h Header, started, ended time.Time, outcome uint64, usage ...Usage) []byte {
	span := appendMessage(nil, 1, Timestamp(started))
	span = appendMessage(span, 2, Timestamp(ended))
	timing := appendMessage(nil, 1, span)

	var charges []byte
	for _, u := range usage {
		counts := appendVarint(nil, 1, u.Input)
		counts = appendVarint(counts, 2, u.Output)
		counts = appendVarint(counts, 3, u.CacheRead)
		counts = appendVarint(counts, 4, u.CacheWrite)
		cost := appendFloat(nil, 1, u.InputCost)
		cost = appendFloat(cost, 2, u.OutputCost)
		inference := appendMessage(appendMessage(nil, 1, counts), 2, cost)
		source := protowire.Number(1)
		if u.BYOK {
			source = 2
		}
		chargedUsage := appendMessage(nil, source, mapEntry(u.Model, inference))
		charges = appendMessage(charges, 1, mapEntry(u.Category, chargedUsage))
	}

	body := appendMessage(nil, 1, timing)
	body = appendMessage(body, 2, charges)
	body = appendVarint(body, 4, outcome)
	return message(h, 30, body)
}

// Timestamp encodes a google.protobuf.Timestamp.
func Timestamp(t time.Time) []byte {
	b := appendVarint(nil, 1, uint64(t.Unix()))
	return appendVarint(b, 2, uint64(t.Nanosecond()))
}

func mapEntry(key string, value []byte) []byte {
	return appendMessage(appendString(nil, 1, key), 2, value)
}

func appendString(b []byte, num protowire.Number, s string) []byte {
	if s == "" {
		return b
	}
	b = protowire.AppendTag(b, num, protowire.BytesType)
	return protowire.AppendString(b, s)
}

func appendMessage(b []byte, num protowire.Number, msg []byte) []byte {
	b = protowire.AppendTag(b, num, protowire.BytesType)
	return protowire.AppendBytes(b, msg)
}

func appendVarint(b []byte, num protowire.Number, v uint64) []byte {
	b = protowire.AppendTag(b, num, protowire.VarintType)
	return protowire.AppendVarint(b, v)
}

func appendFloat(b []byte, num protowire.Number, v float32) []byte {
	b = protowire.AppendTag(b, num, protowire.Fixed32Type)
	return protowire.AppendFixed32(b, math.Float32bits(v))
}
