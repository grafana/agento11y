package sessiondb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/opencode/sessiondb/sessiondbtest"
)

func nativeV2(t *testing.T, opts ...sessiondbtest.Option) *sessiondbtest.Builder {
	t.Helper()
	return sessiondbtest.New(t, append([]sessiondbtest.Option{sessiondbtest.WithoutV1(), sessiondbtest.WithSessionV2()}, opts...)...)
}

func selected(t *testing.T, store *Store, id string) *V2Snapshot {
	t.Helper()
	snapshot, err := store.V2Snapshot(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = snapshot.Close() })
	return snapshot
}

func TestV2MigrationReadiness(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		for index, marker := range []string{"absent", "", `{`, `null`, `"secret"`, `{ "phase":"secret" }`, `{"phase":"sessions"}`, `{"phase":"sessions","cursor":"secret"}`, `{"phase":"completed"}`} {
			t.Run(fmt.Sprintf("legacy=%v/marker=%d", legacy, index), func(t *testing.T) {
				opts := []sessiondbtest.Option{sessiondbtest.WithSessionV2()}
				if marker != "absent" {
					opts = append(opts, sessiondbtest.WithMigrationState())
				}
				db := sessiondbtest.New(t, opts...)
				if legacy {
					db.AddSession("deleted", "", "secret legacy title", "/legacy", 1, 1)
					db.AddMessage("legacy-assistant", "deleted", 1, 2, `{"role":"assistant","time":{"completed":2}}`)
				}
				if marker != "absent" {
					db.SetMigrationState(marker)
				}
				db.AddSessionV2("current", "secret current title", "/current", 1, 1)
				db.AddV2Message("assistant", "current", "assistant", 0, 1, 2, `{"time":{"created":1,"completed":2}}`)
				store, err := Open(context.Background(), db.Path)
				wantErr := strings.Contains(marker, `"sessions"`) || (legacy && marker != `{"phase":"completed"}`)
				if wantErr {
					if store != nil || !errors.Is(err, ErrUnsupportedSchema) {
						t.Fatalf("Open = (%v, %v)", store, err)
					}
					if strings.Contains(err.Error(), "secret") {
						t.Fatalf("content leaked: %v", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = store.Close() }()
				if store.Layout() != LayoutV2 {
					t.Fatal("mixed database did not select v2")
				}
				sessions, err := store.Sessions(context.Background())
				if err != nil || len(sessions) != 1 || sessions[0].ID != "current" || sessions[0].Title != "current" || sessions[0].TerminalMessageCount != 1 {
					t.Fatalf("Sessions = (%+v, %v)", sessions, err)
				}
				if _, err := store.V2Snapshot(context.Background(), "deleted"); err == nil {
					t.Fatal("resurrected a retained legacy session")
				}
			})
		}
	}
}

func TestV2RequiresEveryQueriedColumn(t *testing.T) {
	tables := append(append([]tableColumns{}, v2Tables...), tableColumns{table: "kv", columns: []string{"key", "value"}})
	for _, table := range tables {
		for _, column := range table.columns {
			t.Run(table.table+"/"+column, func(t *testing.T) {
				db := sessiondbtest.New(t, sessiondbtest.WithSessionV2(), sessiondbtest.WithMigrationState())
				db.AddSession("legacy", "", "secret", "/legacy", 1, 1)
				if _, err := db.DB.Exec(fmt.Sprintf(`alter table %q rename column %q to unused`, table.table, column)); err != nil {
					t.Fatal(err)
				}
				store, err := Open(context.Background(), db.Path)
				if store != nil || !errors.Is(err, ErrUnsupportedSchema) || !strings.Contains(err.Error(), column) {
					t.Fatalf("Open = (%v, %v)", store, err)
				}
			})
		}
	}
}

func TestV2DiscoveryIsContentFreeAndCountsOnlyTerminalAssistants(t *testing.T) {
	db := nativeV2(t)
	db.AddSessionV2("session", "secret title", "/work", 1, 2)
	db.AddSessionV2("fork", "secret fork", "/fork", 1, 2)
	db.SetV2Lineage("session", "parent", "")
	db.SetV2Lineage("fork", "", "source")
	claim := int64(1)
	db.SetV2Claim("session", &claim)
	payloads := []struct{ kind, data string }{
		{"assistant", `{"time":{"created":1,"completed":2},"content":[{"type":"text","text":"secret response"}],"metadata":{"private":"secret"}}`},
		{"assistant", `{"time":{"completed":2.5}}`},
		{"assistant", `{"error":{"type":"aborted","message":"secret error","response":{"body":"secret body"}}}`},
		{"assistant", `{"time":{"streamed":2}}`},
		{"assistant", `{"retry":{"attempt":1,"error":{"type":"provider.error","message":"secret"}}}`},
		{"assistant", `{"time":{"completed":null},"error":null}`},
		{"assistant", `{"time":{"completed":"secret"},"error":"secret"}`},
		{"user", `{"time":{"completed":2},"text":"secret user"}`},
		{"compaction", `{"time":{"completed":2},"summary":"secret summary"}`},
		{"idle", `{"error":{"type":"aborted"}}`},
	}
	var size int64
	for i, p := range payloads {
		size += int64(len(p.data))
		db.AddV2Message(fmt.Sprint(i), "session", p.kind, int64(i*2), 1, int64(10+i), p.data)
	}
	rows, err := openTestStore(t, db.Path).Sessions(context.Background())
	if err != nil || len(rows) != 2 {
		t.Fatalf("Sessions = (%+v, %v)", rows, err)
	}
	s := rows[0]
	if s.ID != "session" || s.Title != "session" || s.ParentID != "parent" || !s.Active || s.MessageCount != len(payloads) || s.TerminalMessageCount != 3 || s.LogicalSizeBytes != size || s.UpdatedAt.UnixMilli() != 19 {
		t.Fatalf("aggregate = %+v", s)
	}
	if rows[1].ForkSessionID != "source" || rows[1].ParentID != "" || rows[1].Active {
		t.Fatalf("fork = %+v", rows[1])
	}
	encoded, _ := json.Marshal(rows)
	if strings.Contains(string(encoded), "secret") {
		t.Fatalf("discovery leaked content: %s", encoded)
	}
}

func TestV2SequenceIdentityAndTypedPayload(t *testing.T) {
	db := nativeV2(t)
	db.AddSessionV2("session", "selected title", "/latest", 1, 2)
	db.AddSessionV2("parent", "parent title", "/parent", 1, 2)
	db.SetV2Lineage("session", "parent", "")
	db.AddV2Message("z", "session", "location-switched", 2, 100, 101, `{"location":{"directory":"/latest"},"previous":{"location":{"directory":"/initial"}},"time":{"created":100}}`)
	db.AddV2Message("a", "session", "assistant", 9, 1, 2, `{"id":"wrong","sessionID":"wrong","type":"user","seq":-1,"agent":"build","model":{"id":"model","providerID":"provider","variant":"variant"},"time":{"created":10,"streamed":20,"completed":30},"cost":0,"tokens":{"input":200,"output":50,"reasoning":10,"cache":{"read":30,"write":20}},"finish":"tool-calls","content":[{"type":"text","text":"hello","state":{"input":123,"opaque":"secret state"}},{"type":"reasoning","text":"reason","state":{"opaque":"secret state"}},{"type":"tool","id":"call","name":"subagent","state":{"status":"completed","input":{"prompt":"x"},"content":[{"type":"text","text":"result"},{"type":"file","uri":"secret uri"}],"metadata":{"sessionID":"child","sessionId":"legacy-child","exit":1,"opaque":"secret metadata"}},"time":{"created":11}}],"error":{"type":"provider.error","message":"message","response":{"body":"secret body"}},"providerState":{"opaque":"secret provider"}}`)
	db.AddV2Message("switch", "session", "agent-switched", 12, 0, 0, `{"agent":"plan","previous":"build"}`)
	store := openTestStore(t, db.Path)
	snapshot := selected(t, store, "session")
	parent, ok, err := snapshot.Session(context.Background(), "parent")
	if err != nil || !ok || parent.Title != "parent title" {
		t.Fatalf("parent = (%+v, %v, %v)", parent, ok, err)
	}
	if _, ok, err := snapshot.Session(context.Background(), "missing"); err != nil || ok {
		t.Fatalf("missing = (%v, %v)", ok, err)
	}
	for msg, err := range snapshot.V2Messages(context.Background(), "session") {
		if err != nil {
			t.Fatal(err)
		}
		if msg.Previous == nil || msg.Previous.Location.Directory != "/initial" {
			t.Fatalf("initial location = %+v", msg)
		}
		break
	}
	var messages []V2Message
	for msg, err := range snapshot.Messages(context.Background(), "session") {
		if err != nil {
			t.Fatal(err)
		}
		messages = append(messages, msg)
	}
	if len(messages) != 3 || messages[0].ID != "z" || messages[1].ID != "a" || messages[2].ID != "switch" {
		t.Fatalf("message order = %+v", messages)
	}
	m := messages[1]
	if m.SessionID != "session" || m.Type != "assistant" || m.Seq != 9 || !m.Terminal() || m.RowCreated.UnixMilli() != 1 || m.Time.Streamed == nil || *m.Time.Streamed != 20 || m.Cost == nil || *m.Cost != 0 || m.Tokens.Input == nil || *m.Tokens.Input != 200 || m.Model.ProviderID != "provider" {
		t.Fatalf("assistant = %+v", m)
	}
	tool := m.Content[2]
	if tool.State.Metadata.SessionID != "child" || tool.State.Metadata.LegacySessionID != "legacy-child" || tool.State.Metadata.Exit == nil || *tool.State.Metadata.Exit != 1 || len(tool.State.Content) != 2 || tool.Time.Completed != nil {
		t.Fatalf("tool = %+v", tool)
	}
	encoded, _ := json.Marshal(messages)
	if strings.Contains(string(encoded), "secret") {
		t.Fatalf("decoder retained opaque data: %s", encoded)
	}
	assertReleased(t, store)
}

func assertReleased(t *testing.T, store *Store) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for store.db.Stats().InUse != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if n := store.db.Stats().InUse; n != 0 {
		t.Fatalf("snapshot left %d connections in use", n)
	}
}

func TestV2MalformedAndOversizedMessages(t *testing.T) {
	for _, payload := range []string{`{"text":"secret",`, `null`, `[]`, `{"content":"secret"}`, `{"tokens":{"input":-1}}`, `{"time":{"completed":"secret"}}`, `{"text":"` + strings.Repeat("界", MaxPayloadBytes/3) + `"}`} {
		t.Run(fmt.Sprint(len(payload)), func(t *testing.T) {
			db := nativeV2(t)
			db.AddSessionV2("session", "secret", "/work", 1, 2)
			db.AddV2Message("bad", "session", "assistant", 1, 1, 2, payload)
			store := openTestStore(t, db.Path)
			snapshot := selected(t, store, "session")
			if len(payload) > MaxPayloadBytes {
				var id, sessionID, kind string
				var seq, created, updated, size int64
				var data []byte
				if err := snapshot.tx.QueryRowContext(context.Background(), v2MessagesQuery, MaxPayloadBytes, "session").Scan(
					&id, &sessionID, &kind, &seq, &created, &updated, &size, &data,
				); err != nil {
					t.Fatal(err)
				}
				if size != int64(len(payload)) || data != nil {
					t.Fatalf("oversized SQL row returned %d bytes to Go", len(data))
				}
			}
			var gotErr error
			for _, err := range snapshot.Messages(context.Background(), "session") {
				gotErr = err
			}
			if gotErr == nil || !strings.Contains(gotErr.Error(), "message bad") || strings.Contains(gotErr.Error(), "secret") || strings.Contains(gotErr.Error(), "界") {
				t.Fatalf("Messages error = %v", gotErr)
			}
			if len(payload) > MaxPayloadBytes && !strings.Contains(gotErr.Error(), "limit is") {
				t.Fatalf("size error = %v", gotErr)
			}
			assertReleased(t, store)
		})
	}
}

func TestV2MalformedDiscoveryDoesNotLeakPayload(t *testing.T) {
	db := nativeV2(t)
	db.AddSessionV2("session", "secret title", "/work", 1, 2)
	db.AddV2Message("bad", "session", "assistant", 1, 1, 2, `{"secret":`)
	rows, err := openTestStore(t, db.Path).Sessions(context.Background())
	if rows != nil || err == nil || !strings.Contains(err.Error(), "message bad") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("Sessions = (%+v, %v)", rows, err)
	}
}

func TestV2SnapshotRechecksSelectedSource(t *testing.T) {
	for _, change := range []string{"claim", "delete", "migration", "schema", "layout"} {
		t.Run(change, func(t *testing.T) {
			db := nativeV2(t, sessiondbtest.WithMigrationState())
			db.AddSessionV2("session", "secret", "/work", 1, 2)
			store := openTestStore(t, db.Path)
			switch change {
			case "claim":
				n := int64(1)
				db.SetV2Claim("session", &n)
			case "delete":
				if _, err := db.DB.Exec(`delete from session_v2`); err != nil {
					t.Fatal(err)
				}
			case "migration":
				db.SetMigrationState(`{"phase":"sessions","cursor":"secret"}`)
			case "schema":
				if _, err := db.DB.Exec(`alter table session_message rename column seq to unused`); err != nil {
					t.Fatal(err)
				}
			case "layout":
				if _, err := db.DB.Exec(`drop table session_v2`); err != nil {
					t.Fatal(err)
				}
			}
			snapshot, err := store.V2Snapshot(context.Background(), "session")
			if snapshot != nil || err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatalf("V2Snapshot = (%v, %v)", snapshot, err)
			}
			assertReleased(t, store)
		})
	}
}

func TestV2SnapshotWALConsistencyAndReadOnly(t *testing.T) {
	db := nativeV2(t, sessiondbtest.WithMigrationState())
	if _, err := db.DB.Exec(`pragma journal_mode = wal; pragma wal_autocheckpoint = 0`); err != nil {
		t.Fatal(err)
	}
	db.AddSessionV2("session", "original", "/original", 1, 2)
	db.AddSessionV2("parent", "original parent", "/parent", 1, 2)
	db.SetV2Lineage("session", "parent", "")
	db.AddV2Message("a", "session", "user", 0, 1, 2, `{"text":"original input"}`)
	store := openTestStore(t, db.Path)
	snapshot := selected(t, store, "session")
	if _, err := snapshot.tx.Exec(`update session_v2 set title = 'write'`); err == nil {
		t.Fatal("snapshot accepted a write")
	}
	if _, err := db.DB.Exec(`update session_v2 set title = 'new title'; update session_message set data = '{"text":"new input"}'`); err != nil {
		t.Fatal(err)
	}
	db.AddV2Message("b", "session", "assistant", 4, 3, 4, `{"time":{"completed":4}}`)
	db.SetMigrationState(`{"phase":"sessions"}`)
	claim := int64(100)
	db.SetV2Claim("session", &claim)
	parent, ok, err := snapshot.Session(context.Background(), "parent")
	if err != nil || !ok || parent.Title != "original parent" {
		t.Fatalf("parent = (%+v, %v, %v)", parent, ok, err)
	}
	var count int
	for msg, err := range snapshot.Messages(context.Background(), "session") {
		if err != nil {
			t.Fatal(err)
		}
		count++
		if msg.ID != "a" || msg.Text != "original input" {
			t.Fatalf("inconsistent message: %+v", msg)
		}
	}
	if count != 1 {
		t.Fatalf("snapshot read %d messages, want 1", count)
	}
	assertReleased(t, store)
	if _, err := store.V2Snapshot(context.Background(), "session"); err == nil {
		t.Fatal("fresh snapshot missed migration change")
	}
	if _, err := store.Sessions(context.Background()); err == nil {
		t.Fatal("fresh discovery missed migration change")
	}
}

func TestV2SnapshotCleanup(t *testing.T) {
	for _, mode := range []string{"complete", "early exit", "cancel before read", "cancel during read", "cancel without read", "explicit close"} {
		t.Run(mode, func(t *testing.T) {
			db := nativeV2(t)
			db.AddSessionV2("session", "title", "/work", 1, 2)
			db.AddV2Message("a", "session", "user", 0, 1, 2, `{"text":"input"}`)
			db.AddV2Message("b", "session", "assistant", 9, 3, 4, `{"time":{"completed":4}}`)
			store := openTestStore(t, db.Path)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			snapshot, err := store.V2Snapshot(ctx, "session")
			if err != nil {
				t.Fatal(err)
			}
			if mode == "cancel before read" {
				cancel()
			}
			if mode == "cancel without read" {
				cancel()
			} else if mode == "explicit close" {
				if err := snapshot.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				var gotErr error
				for _, err := range snapshot.Messages(ctx, "session") {
					if err != nil {
						gotErr = err
						break
					}
					if mode == "early exit" {
						break
					}
					if mode == "cancel during read" {
						cancel()
					}
				}
				if strings.HasPrefix(mode, "cancel") && gotErr == nil {
					t.Fatal("canceled iteration returned no error")
				}
			}
			assertReleased(t, store)
			if err := snapshot.Close(); err != nil {
				t.Fatalf("second Close: %v", err)
			}
			if _, _, err := snapshot.Session(context.Background(), "session"); err == nil {
				t.Fatal("closed snapshot remained readable")
			}
		})
	}
}

func TestV2TokenPresenceAndValidation(t *testing.T) {
	for _, input := range []string{"0", "1", "1.0", "1e2", "100e-2", "0e99999999999999999999999", "9223372036854775807"} {
		var tokens V2Tokens
		if err := json.Unmarshal([]byte(`{"input":`+input+`}`), &tokens); err != nil || tokens.Input == nil || tokens.Output != nil {
			t.Fatalf("input %s = (%+v, %v)", input, tokens, err)
		}
	}
	for _, input := range []string{`"1"`, "true", "{}", "[]", "-1", "0.5", "9223372036854775808", "1e999999999", "1e-999999999"} {
		var tokens V2Tokens
		if err := json.Unmarshal([]byte(`{"input":`+input+`}`), &tokens); err == nil {
			t.Fatalf("accepted input %s", input)
		}
	}
	var tokens V2Tokens
	if err := json.Unmarshal([]byte(`{"input":9223372036854775807,"output":1}`), &tokens); err == nil {
		t.Fatal("accepted overflowing total")
	}
}

func TestV2CompletedMigrationWithOnlyDeletedLegacySessions(t *testing.T) {
	db := sessiondbtest.New(t, sessiondbtest.WithSessionV2(), sessiondbtest.WithMigrationState())
	db.SetMigrationState(`{"phase":"completed"}`)
	db.AddSession("deleted", "", "secret legacy title", "/legacy", 1, 2)
	db.AddMessage("old", "deleted", 1, 2, `{"role":"assistant","time":{"completed":2}}`)
	store := openTestStore(t, db.Path)
	sessions, err := store.Sessions(context.Background())
	if err != nil || len(sessions) != 0 {
		t.Fatalf("Sessions = (%+v, %v)", sessions, err)
	}
	if _, found, err := store.Session(context.Background(), "deleted"); found || err != nil {
		t.Fatalf("legacy Session = (%v, %v)", found, err)
	}
	if _, err := store.V2Snapshot(context.Background(), "deleted"); err == nil {
		t.Fatal("selected a deleted v2 session")
	}
	assertReleased(t, store)
}

func TestV2PayloadByteBoundary(t *testing.T) {
	for _, extra := range []int{0, 1} {
		db := nativeV2(t)
		db.AddSessionV2("session", "title", "/work", 1, 2)
		payload := `{"text":"` + strings.Repeat("x", MaxPayloadBytes-len(`{"text":""}`)+extra) + `"}`
		db.AddV2Message("row", "session", "user", 0, 1, 2, payload)
		store := openTestStore(t, db.Path)
		snapshot := selected(t, store, "session")
		var count int
		for msg, err := range snapshot.Messages(context.Background(), "session") {
			count++
			if extra == 0 {
				if err != nil || len(msg.Text) != MaxPayloadBytes-len(`{"text":""}`) {
					t.Fatalf("boundary row = (%d bytes, %v)", len(msg.Text), err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "limit is") {
				t.Fatalf("oversized error = %v", err)
			}
		}
		if count != 1 {
			t.Fatalf("iterator yielded %d rows", count)
		}
		assertReleased(t, store)
	}
}

func TestV2ToolStatesAndTerminalCriteria(t *testing.T) {
	cases := []struct{ state, input string }{
		{"streaming", `"{malformed input"`},
		{"running", `{"command":"run"}`},
		{"completed", `{}`},
		{"error", `{}`},
	}
	for _, tc := range cases {
		payload := `{"time":{"created":1},"retry":{"attempt":2},"content":[{"type":"tool","id":"call","name":"shell","state":{"status":"` + tc.state + `","input":` + tc.input + `,"error":{"type":"tool.execution","message":"sanitizable fallback","response":{"body":"secret body"}},"content":[{"type":"text","text":"one"},{"type":"text","text":"two"}],"metadata":{"exit":1}}}]}`
		msg := V2Message{ID: "row", SessionID: "session", Type: "assistant"}
		if err := decodeV2([]byte(payload), &msg); err != nil {
			t.Fatal(err)
		}
		if msg.Terminal() {
			t.Fatal("tool state or retry made an assistant terminal")
		}
		state := msg.Content[0].State
		if state.Status != tc.state || string(state.Input) != tc.input || state.Error.Message != "sanitizable fallback" || len(state.Content) != 2 {
			t.Fatalf("state = %+v", state)
		}
		encoded, _ := json.Marshal(msg)
		if strings.Contains(string(encoded), "secret body") {
			t.Fatalf("retained error body: %s", encoded)
		}
	}
}

func TestV2SnapshotDoesNotImportAnotherSession(t *testing.T) {
	db := nativeV2(t)
	db.AddSessionV2("selected", "selected", "/selected", 1, 2)
	db.AddSessionV2("other", "other", "/other", 1, 2)
	store := openTestStore(t, db.Path)
	snapshot := selected(t, store, "selected")
	var gotErr error
	for _, err := range snapshot.Messages(context.Background(), "other") {
		gotErr = err
	}
	if gotErr == nil {
		t.Fatal("imported a session that was not checked for execution claims")
	}
	assertReleased(t, store)
}
