package history

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/opencode/sessiondb"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/opencode/sessiondb/sessiondbtest"
)

const openCodeV2CompletedForTest = `{"time":{"created":1000,"completed":2000},"content":[{"type":"text","text":"copied prefix"}]}`

func openCodeV2ToolAssistantForTest(tool, metadataKey, input string, terminal bool) string {
	completed := ""
	if terminal {
		completed = `,"completed":2000`
	}
	return fmt.Sprintf(`{"time":{"created":1000%s},"content":[{"type":"tool","id":"call","name":%q,"state":{"status":"completed","input":%s,"metadata":{%q:"child"}}}]}`, completed, tool, input, metadataKey)
}

func TestOpenCodeV2LineageCreationAndContinuation(t *testing.T) {
	for _, layout := range []struct {
		name, tool, metadataKey, continuationKey string
		migrated                                 bool
	}{
		{name: "native", tool: "subagent", metadataKey: "sessionID", continuationKey: "sessionID"},
		{name: "migrated", tool: "task", metadataKey: "sessionId", continuationKey: "task_id", migrated: true},
	} {
		t.Run(layout.name, func(t *testing.T) {
			for _, tc := range []struct {
				name      string
				create    bool
				input     string
				wantSpawn bool
			}{
				{name: "creation survives later references", create: true, input: `{}`, wantSpawn: true},
				{name: "continuations alone do not spawn"},
				{name: "continuation key present even when null", create: true, input: fmt.Sprintf(`{%q:null}`, layout.continuationKey)},
			} {
				t.Run(tc.name, func(t *testing.T) {
					opts := []sessiondbtest.Option{sessiondbtest.WithoutV1(), sessiondbtest.WithSessionV2()}
					if layout.migrated {
						opts = []sessiondbtest.Option{sessiondbtest.WithSessionV2(), sessiondbtest.WithMigrationState()}
					}
					db := sessiondbtest.New(t, opts...)
					if layout.migrated {
						db.AddSession("root", "", "retained legacy", "/legacy", 1, 2)
						db.SetMigrationState(`{"phase":"completed"}`)
					}
					db.AddSessionV2("root", "Root title", "/root", 1, 2)
					db.AddSessionV2("child", "Child title must not own conversation", "/child", 1, 2)
					db.SetV2Lineage("child", "root", "")
					continuation := fmt.Sprintf(`{%q:"child"}`, layout.continuationKey)
					// Insertion order and equal timestamps must not replace seq ordering.
					db.AddV2Message("later-reference", "root", "assistant", 40, 1, 2, openCodeV2ToolAssistantForTest(layout.tool, layout.metadataKey, continuation, true))
					db.AddV2Message("earlier-continuation", "root", "assistant", 5, 1, 2, openCodeV2ToolAssistantForTest(layout.tool, layout.metadataKey, continuation, true))
					db.AddV2Message("unfinished-creation", "root", "assistant", 10, 1, 2, openCodeV2ToolAssistantForTest(layout.tool, layout.metadataKey, `{}`, false))
					if tc.create {
						db.AddV2Message("spawn", "root", "assistant", 20, 1, 2, openCodeV2ToolAssistantForTest(layout.tool, layout.metadataKey, tc.input, true))
						db.AddV2Message("repeated-reference", "root", "assistant", 50, 1, 2, openCodeV2ToolAssistantForTest(layout.tool, layout.metadataKey, tc.input, true))
					}
					db.AddV2Message("child-first", "child", "assistant", 3, 1, 2, openCodeV2CompletedForTest)
					db.AddV2Message("child-incomplete", "child", "assistant", 8, 1, 2, `{"time":{"created":1000}}`)
					db.AddV2Message("child-second", "child", "assistant", 19, 1, 2, openCodeV2CompletedForTest)

					turns := openCodeV2TurnsForTest(t, db, "child")
					if len(turns) != 2 {
						t.Fatalf("child turn count = %d, want 2", len(turns))
					}
					var wantParents []string
					if tc.wantSpawn {
						wantParents = []string{(SourceRef{Agent: AgentOpenCode, SessionID: "root", SourcePath: db.Path, TurnID: "spawn", TurnIDStable: true}).GenerationID()}
					}
					if !slices.Equal(turns[0].Gen.ParentGenerationIDs, wantParents) {
						t.Errorf("first child parents = %v, want %v", turns[0].Gen.ParentGenerationIDs, wantParents)
					}
					if !slices.Equal(turns[1].Gen.ParentGenerationIDs, []string{turns[0].Gen.ID}) {
						t.Errorf("later child parents = %v, want previous terminal child", turns[1].Gen.ParentGenerationIDs)
					}
					for _, turn := range turns {
						if turn.Gen.ConversationID != "root" || turn.Gen.ConversationTitle != "" || turn.Gen.Metadata["opencode.parent_session_id"] != "root" || turn.Gen.Metadata["opencode.child_session_id"] != "child" || turn.Gen.Tags["subagent"] != "true" {
							t.Errorf("child grouping/title/metadata = %+v", turn.Gen)
						}
					}
					for _, turn := range openCodeV2TurnsForTest(t, db, "root") {
						if turn.Gen.ConversationID != "root" || turn.Gen.ConversationTitle != "Root title" {
							t.Errorf("root does not own title: %+v", turn.Gen)
						}
					}
				})
			}
		})
	}
}

func TestOpenCodeV2LineageIncompleteAncestry(t *testing.T) {
	type session struct{ id, parent string }
	for _, tc := range []struct {
		name, selected, root, parent string
		sessions                     []session
	}{
		{name: "root without turns", selected: "child", root: "root", parent: "root", sessions: []session{{"root", ""}, {"child", "root"}}},
		{name: "multiple empty ancestors", selected: "grandchild", root: "root", parent: "child", sessions: []session{{"root", ""}, {"child", "root"}, {"grandchild", "child"}}},
		{name: "missing immediate parent", selected: "child", root: "missing", parent: "missing", sessions: []session{{"child", "missing"}}},
		{name: "missing distant ancestor", selected: "child", root: "missing", parent: "middle", sessions: []session{{"child", "middle"}, {"middle", "missing"}}},
		{name: "two session cycle", selected: "a", root: "b", parent: "b", sessions: []session{{"a", "b"}, {"b", "a"}}},
		{name: "self cycle", selected: "a", root: "a", sessions: []session{{"a", "a"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := sessiondbtest.New(t, sessiondbtest.WithoutV1(), sessiondbtest.WithSessionV2())
			for _, sess := range tc.sessions {
				db.AddSessionV2(sess.id, "Title "+sess.id, "/work", 1, 2)
				db.SetV2Lineage(sess.id, sess.parent, "")
			}
			db.AddV2Message("first", tc.selected, "assistant", 2, 1, 2, openCodeV2CompletedForTest)
			db.AddV2Message("second", tc.selected, "assistant", 8, 1, 2, openCodeV2CompletedForTest)
			turns := openCodeV2TurnsForTest(t, db, tc.selected)
			if len(turns) != 2 {
				t.Fatalf("turn count = %d, want 2", len(turns))
			}
			if len(turns[0].Gen.ParentGenerationIDs) != 0 || !slices.Equal(turns[1].Gen.ParentGenerationIDs, []string{turns[0].Gen.ID}) {
				t.Errorf("unsupported spawn or lost child chain: %v, %v", turns[0].Gen.ParentGenerationIDs, turns[1].Gen.ParentGenerationIDs)
			}
			for _, turn := range turns {
				if turn.Gen.ConversationID != tc.root {
					t.Errorf("conversation = %q, want %q", turn.Gen.ConversationID, tc.root)
				}
				parent, hasParent := turn.Gen.Metadata["opencode.parent_session_id"]
				if hasParent != (tc.parent != "") || (hasParent && parent != tc.parent) {
					t.Errorf("immediate parent = %v (present %v), want %q", parent, hasParent, tc.parent)
				}
				child, hasChild := turn.Gen.Metadata["opencode.child_session_id"]
				if hasChild != (tc.root != tc.selected) || (hasChild && child != tc.selected) {
					t.Errorf("child identity = %v (present %v)", child, hasChild)
				}
				wantTitle := ""
				if tc.root == tc.selected {
					wantTitle = "Title " + tc.selected
				}
				if turn.Gen.ConversationTitle != wantTitle {
					t.Errorf("conversation title = %q, want %q", turn.Gen.ConversationTitle, wantTitle)
				}
			}
			for _, sess := range tc.sessions {
				if sess.id != tc.selected && len(openCodeV2TurnsForTest(t, db, sess.id)) != 0 {
					t.Errorf("empty ancestor %s created a generation", sess.id)
				}
			}
		})
	}
}

func TestOpenCodeV2LocationReconstructionAndForks(t *testing.T) {
	for _, tc := range []struct {
		name     string
		moves    []string
		fork     bool
		wantCWDs []string
	}{
		{name: "no moves", wantCWDs: []string{"/session/current", "/session/current", "/session/current"}},
		{name: "initial previous present", moves: []string{`{"location":{"directory":"/middle"},"previous":{"location":{"directory":"/initial"}}}`, `{"location":{"directory":"/latest"},"previous":{"location":{"directory":"/middle"}}}`}, wantCWDs: []string{"/initial", "/middle", "/latest"}},
		{name: "initial previous missing", moves: []string{`{"location":{"directory":"/middle"}}`, `{"location":{"directory":"/latest"},"previous":{"location":{"directory":"/middle"}}}`}, wantCWDs: []string{"", "/middle", "/latest"}},
		{name: "fork without moves", fork: true, wantCWDs: []string{"", "", ""}},
		{name: "fork with moves and previous", fork: true, moves: []string{`{"location":{"directory":"/middle"},"previous":{"location":{"directory":"/initial"}}}`, `{"location":{"directory":"/latest"}}`}, wantCWDs: []string{"", "", ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := sessiondbtest.New(t, sessiondbtest.WithoutV1(), sessiondbtest.WithSessionV2())
			db.AddSessionV2("selected", "Independent title", "/session/current", 1, 2)
			if tc.fork {
				db.AddSessionV2("source", "Source title", "/source/current", 1, 2)
				db.AddV2Message("source-prefix", "source", "assistant", 1, 1, 2, openCodeV2CompletedForTest)
				db.SetV2Lineage("selected", "source", "source")
			}
			for i := range 3 {
				db.AddV2Message(fmt.Sprintf("selected-%d", i), "selected", "assistant", int64(i*10+1), 1, 2, openCodeV2CompletedForTest)
				if i < len(tc.moves) {
					db.AddV2Message(fmt.Sprintf("move-%d", i), "selected", "location-switched", int64(i*10+5), 1, 2, tc.moves[i])
				}
			}
			turns := openCodeV2TurnsForTest(t, db, "selected")
			if len(turns) != len(tc.wantCWDs) {
				t.Fatalf("turn count = %d, want %d", len(turns), len(tc.wantCWDs))
			}
			for i, turn := range turns {
				cwd, present := turn.Gen.Tags["cwd"]
				if cwd != tc.wantCWDs[i] || present != (tc.wantCWDs[i] != "") {
					t.Errorf("turn %d cwd = %q (present %v), want %q", i, cwd, present, tc.wantCWDs[i])
				}
				if turn.Gen.ConversationID != "selected" || turn.Gen.ConversationTitle != "Independent title" {
					t.Errorf("independent root/title lost: %+v", turn.Gen)
				}
				if _, ok := turn.Gen.Metadata["opencode.parent_session_id"]; ok {
					t.Error("root/fork has parent-session metadata")
				}
				if _, ok := turn.Gen.Metadata["opencode.child_session_id"]; ok {
					t.Error("root/fork has child-session metadata")
				}
				if _, ok := turn.Gen.Tags["subagent"]; ok {
					t.Error("root/fork marked as subagent")
				}
				var wantParents []string
				if i > 0 {
					wantParents = []string{turns[i-1].Gen.ID}
				}
				if !slices.Equal(turn.Gen.ParentGenerationIDs, wantParents) {
					t.Errorf("turn %d parents = %v, want %v", i, turn.Gen.ParentGenerationIDs, wantParents)
				}
			}
			if tc.fork {
				source := openCodeV2TurnsForTest(t, db, "source")
				if len(source) != 1 {
					t.Fatalf("source turn count = %d, want 1", len(source))
				}
				if source[0].Gen.ConversationID != "source" || source[0].Gen.ID == turns[0].Gen.ID || !slices.Equal(openCodeTextParts(source[0].Gen.Output), openCodeTextParts(turns[0].Gen.Output)) {
					t.Error("copied prefix was lost, deduplicated or grouped with source")
				}
			}
		})
	}
}

func TestOpenCodeV2PendingInputLimit(t *testing.T) {
	for _, tc := range []struct {
		name, intervening string
		bytes             int
		wantOverflow      bool
		wantInputBytes    int
	}{
		{name: "combined exactly at limit", bytes: sessiondb.MaxPayloadBytes / 2, wantInputBytes: sessiondb.MaxPayloadBytes},
		{name: "combined overflow", bytes: sessiondb.MaxPayloadBytes/2 + 1, wantOverflow: true},
		{name: "compaction retains combined input", bytes: sessiondb.MaxPayloadBytes/2 + 1, intervening: "compaction", wantOverflow: true},
		{name: "idle releases pending budget", bytes: sessiondb.MaxPayloadBytes/2 + 1, intervening: "idle", wantInputBytes: sessiondb.MaxPayloadBytes/2 + 1},
		{name: "incomplete assistant consumes pending budget", bytes: sessiondb.MaxPayloadBytes/2 + 1, intervening: "assistant", wantInputBytes: sessiondb.MaxPayloadBytes/2 + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := sessiondbtest.New(t, sessiondbtest.WithoutV1(), sessiondbtest.WithSessionV2())
			db.AddSessionV2("session", "PRIVATE-TITLE", "/work", 1, 2)
			for i, kind := range []string{"user", "synthetic"} {
				text := "PRIVATE-PENDING-" + strings.Repeat("x", tc.bytes-len("PRIVATE-PENDING-"))
				payload, err := json.Marshal(map[string]string{"text": text})
				if err != nil {
					t.Fatal(err)
				}
				if len(payload) >= sessiondb.MaxPayloadBytes {
					t.Fatal("fixture must exercise combined limit, not per-row limit")
				}
				db.AddV2Message(fmt.Sprintf("input-%d", i), "session", kind, int64(i*10+1), 1, 2, string(payload))
				if i == 0 && tc.intervening != "" {
					db.AddV2Message("intervening", "session", tc.intervening, 5, 1, 2, `{}`)
				}
			}
			db.AddV2Message("terminal", "session", "assistant", 30, 1, 2, openCodeV2CompletedForTest)
			if !tc.wantOverflow {
				turns := openCodeV2TurnsForTest(t, db, "session")
				if len(turns) != 1 {
					t.Fatalf("turn count = %d, want 1", len(turns))
				}
				gotBytes := 0
				for _, text := range openCodeTextParts(turns[0].Gen.Input) {
					gotBytes += len(text)
				}
				if gotBytes != tc.wantInputBytes {
					t.Errorf("pending bytes = %d, want %d", gotBytes, tc.wantInputBytes)
				}
				return
			}

			store, err := sessiondb.Open(context.Background(), db.Path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = store.Close() }()
			var diagnostics []string
			turnCount := 0
			for turn, err := range opencodeV2Turns(context.Background(), store, SessionPreview{Agent: AgentOpenCode, SessionID: "session", SourcePath: db.Path}) {
				if err != nil {
					diagnostics = append(diagnostics, err.Error())
					if turn.Gen.ID != "" || turn.Source.TurnID != "" || len(turn.Gen.Input) != 0 || len(turn.Gen.Output) != 0 {
						t.Error("overflow yielded a partial generation")
					}
				} else {
					turnCount++
				}
			}
			if turnCount != 0 || len(diagnostics) != 1 {
				t.Fatalf("overflow yielded %d turns and %d diagnostics", turnCount, len(diagnostics))
			}
			want := fmt.Sprintf("opencode session session in %s: pending input exceeds %d bytes", db.Path, sessiondb.MaxPayloadBytes)
			if diagnostics[0] != want {
				t.Errorf("overflow diagnostic was not the expected content-free identity/reason (length %d)", len(diagnostics[0]))
			}
		})
	}
}

func TestOpenCodeV2StableNativeIdentityAfterEarlierRowChanges(t *testing.T) {
	for _, change := range []string{"complete", "delete"} {
		t.Run(change, func(t *testing.T) {
			db := sessiondbtest.New(t, sessiondbtest.WithoutV1(), sessiondbtest.WithSessionV2())
			db.AddSessionV2("session", "Title", "/work", 1, 2)
			db.AddV2Message("earlier", "session", "assistant", 3, 1, 2, `{"time":{"created":1000}}`)
			db.AddV2Message("survivor", "session", "assistant", 99, 1, 2, openCodeV2CompletedForTest)
			before := openCodeV2TurnsForTest(t, db, "session")
			if len(before) != 1 || before[0].Source.TurnID != "survivor" {
				t.Fatalf("initial terminal turns = %+v", before)
			}
			if change == "complete" {
				if _, err := db.DB.Exec(`update session_message set data = ? where id = 'earlier'`, openCodeV2CompletedForTest); err != nil {
					t.Fatal(err)
				}
			} else if _, err := db.DB.Exec(`delete from session_message where id = 'earlier'`); err != nil {
				t.Fatal(err)
			}
			after := openCodeV2TurnsForTest(t, db, "session")
			wantCount := 1
			if change == "complete" {
				wantCount = 2
			}
			if len(after) != wantCount {
				t.Fatalf("after %s turn count = %d, want %d", change, len(after), wantCount)
			}
			survivor := after[len(after)-1]
			wantSource := SourceRef{Agent: AgentOpenCode, SessionID: "session", SourcePath: db.Path, TurnID: "survivor", TurnIDStable: true}
			for _, turn := range []HistoricalGeneration{before[0], survivor} {
				if !turn.Source.TurnIDStable || turn.Source.TurnID != "survivor" || turn.Source.SessionID != "session" || turn.Source.SourcePath != db.Path || turn.Source.Agent != AgentOpenCode || turn.Gen.ID != wantSource.GenerationID() || turn.Source.Identity() != wantSource.Identity() {
					t.Errorf("native identity changed: %+v", turn.Source)
				}
			}
			if change == "complete" {
				if after[0].Source.TurnID != "earlier" || !slices.Equal(survivor.Gen.ParentGenerationIDs, []string{after[0].Gen.ID}) {
					t.Error("newly completed earlier row was skipped or not chained")
				}
			} else if before[0].Source.TurnIndex == survivor.Source.TurnIndex || len(survivor.Gen.ParentGenerationIDs) != 0 {
				t.Error("fixture did not shift survivor index, or deletion invented a parent")
			}
		})
	}
}
