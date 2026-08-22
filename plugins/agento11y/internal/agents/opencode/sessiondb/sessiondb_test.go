package sessiondb

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/opencode/sessiondb/sessiondbtest"
)

func openTestStore(t *testing.T, path string) *Store {
	t.Helper()
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestSessionsReturnsContentFreeAggregates(t *testing.T) {
	db := sessiondbtest.New(t)
	db.AddSession("session-old", "parent", "Old", "/work/session-old", 1000, 2000)
	db.AddSession("session-new", "", "New", "/work/session-new", 3000, 4000)
	db.AddMessage("user", "session-old", 1000, 1001, `{"role":"user","time":{"created":1000}}`)
	db.AddMessage("complete", "session-old", 1100, 1101, `{"role":"assistant","time":{"created":1100,"completed":1200}}`)
	db.AddMessage("error", "session-old", 1300, 1301, `{"role":"assistant","time":{"created":1300},"error":{"name":"UnknownError","data":{}}}`)
	db.AddMessage("in-flight", "session-old", 1400, 1401, `{"role":"assistant","time":{"created":1400}}`)
	db.AddMessage("null-complete", "session-old", 1500, 1501, `{"role":"assistant","time":{"created":1500,"completed":null}}`)
	db.AddMessage("null-error", "session-old", 1600, 1601, `{"role":"assistant","time":{"created":1600},"error":null}`)
	db.AddPart("part-1", "complete", "session-old", 1, 2, `{"type":"text","text":"response"}`)

	got, err := openTestStore(t, db.Path).Sessions(context.Background())
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Sessions returned %d rows, want 2", len(got))
	}
	if got[0].ID != "session-new" || got[1].ID != "session-old" {
		t.Fatalf("session order = [%s %s]", got[0].ID, got[1].ID)
	}
	old := got[1]
	if old.Title != "" {
		t.Fatalf("session aggregate contains title %q", old.Title)
	}
	if old.ParentID != "parent" || old.MessageCount != 6 || old.TerminalMessageCount != 2 {
		t.Fatalf("old session aggregates = %+v", old)
	}
	if old.LogicalSizeBytes <= 0 {
		t.Fatalf("LogicalSizeBytes = %d", old.LogicalSizeBytes)
	}
	if old.CreatedAt.UnixMilli() != 1000 || old.UpdatedAt.UnixMilli() != 2000 {
		t.Fatalf("session times = (%d, %d)", old.CreatedAt.UnixMilli(), old.UpdatedAt.UnixMilli())
	}
	if got[0].MessageCount != 0 || got[0].TerminalMessageCount != 0 {
		t.Fatalf("empty session aggregates = %+v", got[0])
	}
}

func TestSessionsUseLatestChildActivity(t *testing.T) {
	tests := []struct {
		name    string
		updated int64
		add     func(*sessiondbtest.Builder)
	}{
		{
			name:    "message",
			updated: 2000,
			add: func(db *sessiondbtest.Builder) {
				db.AddMessage("assistant", "session", 800, 2000, `{"role":"assistant","time":{"created":800,"completed":900}}`)
			},
		},
		{
			name:    "part",
			updated: 3000,
			add: func(db *sessiondbtest.Builder) {
				db.AddMessage("assistant", "session", 800, 900, `{"role":"assistant","time":{"created":800,"completed":900}}`)
				db.AddPart("text", "assistant", "session", 800, 3000, `{"type":"text","text":"done"}`)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := sessiondbtest.New(t)
			db.AddSession("session", "", "Session", "/work", 500, 1000)
			tt.add(db)

			sessions, err := openTestStore(t, db.Path).Sessions(context.Background())
			if err != nil {
				t.Fatalf("Sessions: %v", err)
			}
			if len(sessions) != 1 || sessions[0].UpdatedAt.UnixMilli() != tt.updated {
				t.Fatalf("sessions = %+v, want activity %d", sessions, tt.updated)
			}
		})
	}
}

func TestMessagesAndPartsUseStableRowOrder(t *testing.T) {
	db := sessiondbtest.New(t)
	db.AddSession("session", "", "Session", "/work/session", 1000, 2000)
	db.AddMessage("message-b", "session", 2000, 2001, `{"id":"wrong","sessionID":"wrong","role":"assistant","time":{"created":2000,"completed":2100},"parentID":"user","modelID":"model","providerID":"provider","mode":"build","path":{"cwd":"/work","root":"/work"},"cost":0.5,"tokens":{"input":10,"output":5,"reasoning":1,"cache":{"read":2,"write":3}},"finish":"tool-calls"}`)
	db.AddMessage("message-a", "session", 2000, 2001, `{"role":"user","time":{"created":2000}}`)
	db.AddMessage("message-old", "session", 1000, 1001, `{"role":"user","time":{"created":1000}}`)
	db.AddPart("part-b", "message-b", "session", 1, 2, `{"id":"wrong","messageID":"wrong","sessionID":"wrong","type":"text","text":"second"}`)
	db.AddPart("part-a", "message-b", "session", 9999, 10000, `{"type":"tool","callID":"call","tool":"task","state":{"status":"completed","input":{"prompt":"x"},"output":"ok","metadata":{"sessionId":"child","exit":1},"time":{"start":1,"end":2}}}`)

	store := openTestStore(t, db.Path)
	session, ok, err := store.Session(context.Background(), "session")
	if err != nil || !ok {
		t.Fatalf("Session = (%+v, %v, %v)", session, ok, err)
	}
	if session.Title != "Session" || session.Directory != "/work/session" {
		t.Fatalf("Session row = %+v", session)
	}
	if _, ok, err := store.Session(context.Background(), "missing"); err != nil || ok {
		t.Fatalf("missing Session = (_, %v, %v)", ok, err)
	}
	role, ok, err := store.MessageRole(context.Background(), "message-a")
	if err != nil || !ok || role != "user" {
		t.Fatalf("MessageRole = (%q, %v, %v)", role, ok, err)
	}
	if _, ok, err := store.MessageRole(context.Background(), "missing"); err != nil || ok {
		t.Fatalf("missing MessageRole = (_, %v, %v)", ok, err)
	}
	spawn, ok, err := store.SpawningMessageID(context.Background(), "session", "child")
	if err != nil || !ok || spawn != "message-b" {
		t.Fatalf("SpawningMessageID = (%q, %v, %v)", spawn, ok, err)
	}
	if _, ok, err := store.SpawningMessageID(context.Background(), "session", "missing"); err != nil || ok {
		t.Fatalf("missing SpawningMessageID = (_, %v, %v)", ok, err)
	}

	var messages []Message
	for msg, err := range store.Messages(context.Background(), "session") {
		if err != nil {
			t.Fatalf("Messages: %v", err)
		}
		messages = append(messages, msg)
	}
	if len(messages) != 3 {
		t.Fatalf("Messages returned %d rows, want 3", len(messages))
	}
	if got := []string{messages[0].ID, messages[1].ID, messages[2].ID}; fmt.Sprint(got) != "[message-old message-a message-b]" {
		t.Fatalf("message order = %v", got)
	}
	assistant := messages[2]
	if assistant.ID != "message-b" || assistant.SessionID != "session" {
		t.Fatalf("message identity = (%q, %q)", assistant.ID, assistant.SessionID)
	}
	if assistant.Tokens.Cache.Write != 3 || assistant.Finish != "tool-calls" {
		t.Fatalf("assistant payload = %+v", assistant)
	}

	parts, err := store.Parts(context.Background(), "message-b")
	if err != nil {
		t.Fatalf("Parts: %v", err)
	}
	if len(parts) != 2 || parts[0].ID != "part-a" || parts[1].ID != "part-b" {
		t.Fatalf("part order = %+v", parts)
	}
	if parts[0].MessageID != "message-b" || parts[0].SessionID != "session" {
		t.Fatalf("part identity = (%q, %q)", parts[0].MessageID, parts[0].SessionID)
	}
	if parts[0].State.Metadata.SessionID != "child" || parts[0].State.Metadata.Exit == nil || *parts[0].State.Metadata.Exit != 1 {
		t.Fatalf("tool metadata = %+v", parts[0].State.Metadata)
	}
}

func TestOpenIsReadOnlyAndReadsAWriteAheadLog(t *testing.T) {
	db := sessiondbtest.New(t)
	if _, err := db.DB.Exec(`pragma journal_mode = wal`); err != nil {
		t.Fatalf("enable WAL: %v", err)
	}
	if _, err := db.DB.Exec(`pragma wal_autocheckpoint = 0`); err != nil {
		t.Fatalf("disable checkpoint: %v", err)
	}
	db.AddSession("wal-session", "", "WAL", "/work/wal-session", 1000, 2000)
	if _, err := os.Stat(db.Path + "-wal"); err != nil {
		t.Fatalf("writer left no WAL: %v", err)
	}

	store := openTestStore(t, db.Path)
	sessions, err := store.Sessions(context.Background())
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if len(sessions) != 1 || sessions[0].ID != "wal-session" {
		t.Fatalf("sessions = %+v", sessions)
	}
	if _, err := store.db.Exec(`insert into session
		(id, directory, title, time_created, time_updated) values ('write', '/', 'write', 1, 1)`); err == nil {
		t.Fatal("read-only store accepted a write")
	}
}

func TestMalformedPayloadsFailWithRowIdentity(t *testing.T) {
	tests := []struct {
		name string
		run  func(*testing.T, *Store)
	}{
		{
			name: "session aggregate",
			run: func(t *testing.T, store *Store) {
				_, err := store.Sessions(context.Background())
				if err == nil || !strings.Contains(err.Error(), "message bad-message") || !strings.Contains(err.Error(), store.path) {
					t.Fatalf("Sessions error = %v", err)
				}
			},
		},
		{
			name: "message",
			run: func(t *testing.T, store *Store) {
				for _, err := range store.Messages(context.Background(), "session") {
					if err == nil {
						continue
					}
					if !strings.Contains(err.Error(), "message bad-message") || !strings.Contains(err.Error(), store.path) {
						t.Fatalf("Messages error = %v", err)
					}
					return
				}
				t.Fatal("Messages returned no error")
			},
		},
		{
			name: "part",
			run: func(t *testing.T, store *Store) {
				_, err := store.Parts(context.Background(), "good-message")
				if err == nil || !strings.Contains(err.Error(), "part bad-part") || !strings.Contains(err.Error(), store.path) {
					t.Fatalf("Parts error = %v", err)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := sessiondbtest.New(t)
			db.AddSession("session", "", "Session", "/work/session", 1, 1)
			db.AddMessage("good-message", "session", 1, 2, `{"role":"user","time":{"created":1}}`)
			db.AddMessage("bad-message", "session", 2, 3, `{`)
			db.AddPart("bad-part", "good-message", "session", 1, 2, `{`)
			tc.run(t, openTestStore(t, db.Path))
		})
	}
}

func TestPayloadDecodeIsCapped(t *testing.T) {
	db := sessiondbtest.New(t)
	db.AddSession("session", "", "Session", "/work/session", 1, 1)
	db.AddMessage("large", "session", 1, 2, `{"role":"user","padding":"`+strings.Repeat("x", MaxPayloadBytes)+`"}`)

	for _, err := range openTestStore(t, db.Path).Messages(context.Background(), "session") {
		if err == nil || !strings.Contains(err.Error(), "limit is") {
			t.Fatalf("Messages error = %v", err)
		}
		return
	}
	t.Fatal("Messages returned no error")
}
