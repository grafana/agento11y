package claudecode

import (
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/state"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/subagent"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/transcript"
	"github.com/grafana/agento11y/plugins/agento11y/internal/xdg"
	"go.opentelemetry.io/otel/trace"
)

func TestVerifiedSkillResult(t *testing.T) {
	one := []transcript.UserContentBlock{{Type: "tool_result", ToolUseID: "one"}}
	two := append(append([]transcript.UserContentBlock{}, one...), transcript.UserContentBlock{Type: "tool_result", ToolUseID: "two"})
	for _, tc := range []struct {
		name, raw, want string
		blocks          []transcript.UserContentBlock
	}{
		{name: "literal success", raw: `{"success":true,"commandName":"review"}`, blocks: one, want: "review"},
		{name: "missing evidence", blocks: one},
		{name: "false", raw: `{"success":false,"commandName":"review"}`, blocks: one},
		{name: "string true", raw: `{"success":"true","commandName":"review"}`, blocks: one},
		{name: "missing success", raw: `{"commandName":"review"}`, blocks: one},
		{name: "unsafe name", raw: `{"success":true,"commandName":"/private/review"}`, blocks: one},
		{name: "numeric name", raw: `{"success":true,"commandName":12}`, blocks: one},
		{name: "contradictory error", raw: `{"success":true,"commandName":"review","is_error":true}`, blocks: one},
		{name: "contradictory block", raw: `{"success":true,"commandName":"review"}`, blocks: []transcript.UserContentBlock{{Type: "tool_result", ToolUseID: "one", IsError: true}}},
		{name: "ambiguous association", raw: `{"success":true,"commandName":"review"}`, blocks: two},
		{name: "explicit association", raw: `{"success":true,"commandName":"review","tool_use_id":"one"}`, blocks: two, want: "review"},
		{name: "camel association", raw: `{"success":true,"commandName":"review","toolUseId":"one"}`, blocks: two, want: "review"},
		{name: "uppercase ID alias", raw: `{"success":true,"commandName":"review","toolUseID":"one"}`, blocks: two, want: "review"},
		{name: "matching aliases", raw: `{"success":true,"commandName":"review","toolUseId":"one","toolUseID":"one"}`, blocks: two, want: "review"},
		{name: "unsupported case", raw: `{"Success":true,"CommandName":"review"}`, blocks: one},
		{name: "different explicit association", raw: `{"success":true,"commandName":"review","tool_use_id":"two"}`, blocks: two},
		{name: "conflicting association fields", raw: `{"success":true,"commandName":"review","tool_use_id":"one","toolUseId":"two"}`, blocks: two},
		{name: "duplicate result blocks", raw: `{"success":true,"commandName":"review"}`, blocks: append(append([]transcript.UserContentBlock{}, one...), one...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := verifiedSkillResult(json.RawMessage(tc.raw), tc.blocks, "one"); got != tc.want {
				t.Errorf("verified name=%q, want %q", got, tc.want)
			}
		})
	}
}

func TestToolObservationState(t *testing.T) {
	for _, tc := range []struct {
		name           string
		resultFirst    bool
		duplicateName  string
		duplicateError bool
		wantConflict   bool
	}{
		{name: "call before result"},
		{name: "result before call", resultFirst: true},
		{name: "identical duplicate", duplicateName: "review"},
		{name: "conflicting duplicate", duplicateName: "other-review", wantConflict: true},
		{name: "contradictory duplicate error", duplicateError: true, wantConflict: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			path := filepath.Join(t.TempDir(), "transcript.jsonl")
			call := buildHookAssistantLineAt("raw-session", "request", "tool_use", "2025-06-01T12:00:01Z", []any{
				map[string]any{"type": "tool_use", "id": "one", "name": "Skill", "input": map[string]any{"skill": "review", "args": "argument-sentinel"}},
			}, 7) + "\n"
			result := `{"type":"user","sessionId":"raw-session","timestamp":"2025-06-01T12:00:03Z","toolUseResult":{"success":true,"commandName":"review"},"message":{"content":[{"type":"tool_result","tool_use_id":"one","content":"result-sentinel"}]}}` + "\n"
			batches := []string{call, result}
			if tc.resultFirst {
				batches = []string{result, call}
			}
			if tc.duplicateName != "" {
				batches = append(batches, strings.ReplaceAll(result, `"commandName":"review"`, `"commandName":"`+tc.duplicateName+`"`))
			}
			if tc.duplicateError {
				batches = append(batches, strings.ReplaceAll(result, `"content":"result-sentinel"`, `"content":"result-sentinel","is_error":true`))
			}
			st := state.Session{}
			initToolState(&st, path, nil)
			logger := log.New(io.Discard, "", 0)
			for _, batch := range batches {
				f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.WriteString(batch); err != nil {
					t.Fatal(err)
				}
				if err := f.Close(); err != nil {
					t.Fatal(err)
				}
				if err := collectTools(&st, path, nil, logger); err != nil {
					t.Fatal(err)
				}
				if err := state.Save("hook-session", st); err != nil {
					t.Fatal(err)
				}
				st = state.Load("hook-session")
			}
			if len(st.Tools.Pending) != 1 {
				t.Fatalf("pending=%+v, want one scoped observation", st.Tools.Pending)
			}
			for _, p := range st.Tools.Pending {
				if p.Call == nil || p.Result == nil || p.SkillName != "review" || p.Result.VerifiedName != "review" || p.Parent.Ready || p.Result.Conflict != tc.wantConflict || p.Result.IsError != tc.duplicateError {
					t.Errorf("lost pending completion: %+v", p)
				}
				p.Parent.ID = "actual-mapped-id"
			}
			tid, _ := trace.TraceIDFromHex("0123456789abcdef0123456789abcdef")
			sid, _ := trace.SpanIDFromHex("0123456789abcdef")
			ts, _ := trace.ParseTraceState("vendor=opaque")
			sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceState: ts, Remote: true})
			var parents []*state.PendingTool
			for _, pending := range st.Tools.Pending {
				parents = append(parents, pending)
			}
			captureToolParent(parents, sc)
			if err := state.Save("hook-session", st); err != nil {
				t.Fatal(err)
			}
			st = state.Load("hook-session")
			for _, p := range st.Tools.Pending {
				got := trace.SpanContextFromContext(toolParentContext(t.Context(), p.Parent))
				if !got.IsValid() || got.IsSampled() || got.TraceID() != tid || got.SpanID() != sid || got.TraceState().String() != ts.String() || !got.IsRemote() {
					t.Errorf("parent context did not survive: %+v", got)
				}
			}
			data, err := os.ReadFile(filepath.Join(xdg.AppStateRoot(), "claude-code", state.SanitizeSessionID("hook-session")+".state"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "sentinel") {
				t.Errorf("pending state contains tool content: %s", data)
			}
			info, err := os.Stat(filepath.Join(xdg.AppStateRoot(), "claude-code", state.SanitizeSessionID("hook-session")+".state"))
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Errorf("state file mode=%v error=%v, want 0600", info, err)
			}
		})
	}
}

func TestToolObservationScopes(t *testing.T) {
	for _, tc := range []struct {
		name, callFile, resultFile, callSession, resultSession, callAgent, resultAgent string
	}{
		{name: "parent and child", callFile: "session.jsonl", resultFile: "session/subagents/agent-child.jsonl", callSession: "same", resultSession: "same"},
		{name: "siblings", callFile: "session/subagents/agent-a.jsonl", resultFile: "session/subagents/agent-b.jsonl", callSession: "same", resultSession: "same"},
		{name: "raw sessions", callFile: "session.jsonl", resultFile: "session.jsonl", callSession: "first", resultSession: "second"},
		{name: "sidechain agents", callFile: "session.jsonl", resultFile: "session.jsonl", callSession: "same", resultSession: "same", callAgent: "a", resultAgent: "b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			parent := filepath.Join(dir, "session.jsonl")
			st := state.Session{}
			initToolState(&st, parent, nil)
			for i, relative := range []string{tc.callFile, tc.resultFile} {
				session, agent, typ := tc.callSession, tc.callAgent, "assistant"
				content := map[string]any{"type": "tool_use", "id": "same-id", "name": "Bash", "input": map[string]any{}}
				if i == 1 {
					session, agent, typ = tc.resultSession, tc.resultAgent, "user"
					content = map[string]any{"type": "tool_result", "tool_use_id": "same-id", "content": "ok"}
				}
				raw, err := json.Marshal(map[string]any{"type": typ, "sessionId": session, "agentId": agent, "isSidechain": agent != "", "message": map[string]any{"content": []any{content}}})
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(dir, relative)
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.Write(append(raw, '\n')); err != nil {
					t.Fatal(err)
				}
				if err := f.Close(); err != nil {
					t.Fatal(err)
				}
				if err := collectTools(&st, path, nil, log.New(io.Discard, "", 0)); err != nil {
					t.Fatal(err)
				}
			}
			if len(st.Tools.Pending) != 2 {
				t.Fatalf("same-ID observations from distinct scopes joined: %+v", st.Tools.Pending)
			}
			for _, pending := range st.Tools.Pending {
				if pending.Call != nil && pending.Result != nil {
					t.Errorf("cross-scope completion: %+v", pending)
				}
			}
		})
	}
}

func TestToolCheckpointMigration(t *testing.T) {
	for _, tc := range []struct {
		name   string
		parent int64
		child  int64
	}{
		{name: "nonzero checkpoints", parent: 200, child: 400},
		{name: "initialized zero checkpoints"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "session.jsonl")
			child := filepath.Join(filepath.Dir(path), "session", "subagents", "agent-child.jsonl")
			st := state.Session{Offset: tc.parent, SubagentOffsets: map[string]int64{subagent.FileKey(child): tc.child}}
			initToolState(&st, path, []string{child})
			if st.Tools.Offsets[toolPath(path)] != tc.parent || st.Tools.Offsets[toolPath(child)] != tc.child {
				t.Fatalf("migration offsets=%v", st.Tools.Offsets)
			}
			st.Offset += 100
			st.SubagentOffsets[subagent.FileKey(child)] += 100
			initToolState(&st, path, []string{child})
			if st.Tools.Offsets[toolPath(path)] != tc.parent || st.Tools.Offsets[toolPath(child)] != tc.child {
				t.Fatalf("initialized checkpoints were reseeded: %v", st.Tools.Offsets)
			}
		})
	}
}
