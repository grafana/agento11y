package warpdb_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/warp/warpdb"
	wt "github.com/grafana/agento11y/plugins/agento11y/internal/agents/warp/warpdb/warpdbtest"
	"google.golang.org/protobuf/encoding/protowire"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func hdr(id, req string, offset time.Duration) wt.Header {
	return wt.Header{ID: id, TaskID: "root", RequestID: req, At: t0.Add(offset)}
}

func TestDecodeTaskReadsEveryMappedVariant(t *testing.T) {
	payload := wt.Task("root", "Fix the build", "",
		wt.UserQuery(hdr("m1", "r1", 0), "why is CI red?"),
		wt.ModelUsed(hdr("m2", "r1", time.Second), "claude-4-6-sonnet-high", "Claude Sonnet"),
		wt.Reasoning(hdr("m3", "r1", 2*time.Second), "look at the logs"),
		wt.ShellCall(hdr("m4", "r1", 3*time.Second), "call-1", "go test ./..."),
		wt.ReadFilesCall(hdr("m5", "r1", 3*time.Second), "call-2", "a.go", "b.go"),
		wt.MCPCall(hdr("m6", "r1", 3*time.Second), "call-3", "search_issues", map[string]any{"q": "flaky"}),
		wt.OtherCall(hdr("m7", "r1", 3*time.Second), "call-4", 19),
		wt.AgentOutput(hdr("m8", "r1", 4*time.Second), "running tests"),
		wt.RequestMetadata(hdr("m9", "r1", 5*time.Second), t0, t0.Add(5*time.Second), wt.OutcomeCompleted,
			wt.Usage{Category: "primary_agent", Model: "claude-4-6-sonnet-high", Input: 100, Output: 20, CacheRead: 300, CacheWrite: 7, InputCost: 1.5, OutputCost: 0.5},
			wt.Usage{Category: "compaction", Model: "claude-4-6-sonnet-high", BYOK: true, Input: 10, Output: 2, InputCost: 0.25, PlatformCost: 0.5},
			wt.Usage{Category: "primary_agent", Model: "gemini-3.5-flash", Input: 5, Output: 1},
		),
		wt.ShellResult(hdr("m10", "r2", 6*time.Second), "call-1", "FAIL", 1),
		wt.MCPResult(hdr("m11", "r2", 6*time.Second), "call-3", "issue 1", "issue 2"),
		wt.CancelResult(hdr("m12", "r2", 6*time.Second), "call-4"),
		wt.OtherResult(hdr("m13", "r2", 6*time.Second), "call-2", 5),
	)

	task, err := warpdb.DecodeTask(payload)
	if err != nil {
		t.Fatalf("DecodeTask: %v", err)
	}
	if task.ID != "root" || task.Description != "Fix the build" || task.ParentTaskID != "" || len(task.Messages) != 13 {
		t.Fatalf("task = %+v", task)
	}
	m := task.Messages
	if m[0].Kind != warpdb.KindUserQuery || m[0].Text != "why is CI red?" || m[0].RequestID != "r1" || !m[0].Timestamp.Equal(t0) {
		t.Fatalf("user query = %+v", m[0])
	}
	if m[1].Kind != warpdb.KindModelUsed || m[1].Model.ID != "claude-4-6-sonnet-high" || m[1].Model.DisplayName != "Claude Sonnet" {
		t.Fatalf("model used = %+v", m[1])
	}
	if m[2].Kind != warpdb.KindReasoning || m[2].Text != "look at the logs" {
		t.Fatalf("reasoning = %+v", m[2])
	}
	wantCalls := []struct{ id, name, input string }{
		{"call-1", "run_shell_command", `{"command":"go test ./..."}`},
		{"call-2", "read_files", `{"files":["a.go","b.go"]}`},
		{"call-3", "search_issues", `{"args":{"q":"flaky"},"name":"search_issues"}`},
		{"call-4", "subagent", `{}`},
	}
	for i, want := range wantCalls {
		got := m[3+i]
		if got.Kind != warpdb.KindToolCall || got.ToolCall.ID != want.id || got.ToolCall.Name != want.name {
			t.Errorf("tool call %d = %+v", i, got.ToolCall)
		}
		if !jsonEqual(t, got.ToolCall.Input, want.input) {
			t.Errorf("tool call %d input = %s, want %s", i, got.ToolCall.Input, want.input)
		}
	}
	if m[7].Kind != warpdb.KindAgentOutput || m[7].Text != "running tests" {
		t.Fatalf("output = %+v", m[7])
	}

	meta := m[8].Request
	if m[8].Kind != warpdb.KindRequestMetadata || !meta.StartedAt.Equal(t0) || !meta.EndedAt.Equal(t0.Add(5*time.Second)) || meta.Outcome != warpdb.OutcomeCompleted {
		t.Fatalf("request metadata = %+v", m[8])
	}
	wantUsage := []warpdb.ModelUsage{
		{Model: "claude-4-6-sonnet-high", Input: 110, Output: 22, CacheRead: 300, CacheWrite: 7, CostCents: 2.25},
		{Model: "gemini-3.5-flash", Input: 5, Output: 1},
	}
	if !slices.Equal(meta.Usage, wantUsage) {
		t.Fatalf("usage = %+v, want %+v", meta.Usage, wantUsage)
	}
	if meta.PlatformCents != 0.5 {
		t.Fatalf("platform cents = %v, want 0.5", meta.PlatformCents)
	}

	wantResults := []warpdb.ToolResult{
		{CallID: "call-1", Name: "run_shell_command", Content: "FAIL", IsError: true},
		{CallID: "call-3", Name: "call_mcp_tool", Content: "issue 1\nissue 2"},
		{CallID: "call-4", Content: "cancelled", IsError: true},
		{CallID: "call-2", Name: "read_files"},
	}
	for i, want := range wantResults {
		if got := m[9+i]; got.Kind != warpdb.KindToolResult || got.ToolResult != want {
			t.Errorf("tool result %d = %+v, want %+v", i, got.ToolResult, want)
		}
	}
}

func TestDecodeTaskSkipsUnknownFields(t *testing.T) {
	msg := wt.AgentOutput(hdr("m1", "r1", 0), "hello")
	msg = protowire.AppendTag(msg, 99, protowire.VarintType)
	msg = protowire.AppendVarint(msg, 7)
	payload := wt.Task("root", "", "", msg)
	payload = protowire.AppendTag(payload, 42, protowire.BytesType)
	payload = protowire.AppendString(payload, "future field")

	task, err := warpdb.DecodeTask(payload)
	if err != nil {
		t.Fatalf("DecodeTask: %v", err)
	}
	if len(task.Messages) != 1 || task.Messages[0].Text != "hello" {
		t.Fatalf("task = %+v", task)
	}
}

func TestDecodeTaskKeepsMCPCallWithUndecodableArgs(t *testing.T) {
	// 0x0a 0xff is a fields entry whose length runs past the payload.
	payload := wt.Task("root", "", "", wt.MCPCallRawArgs(hdr("m1", "r1", 0), "call-1", "search_issues", []byte{0x0a, 0xff}))
	task, err := warpdb.DecodeTask(payload)
	if err != nil {
		t.Fatalf("DecodeTask: %v", err)
	}
	call := task.Messages[0].ToolCall
	if call.Name != "search_issues" || !jsonEqual(t, call.Input, `{"name":"search_issues"}`) {
		t.Fatalf("tool call = %+v (%s)", call, call.Input)
	}
}

func TestStoreSkipsOversizedConversation(t *testing.T) {
	old := warpdb.MaxTaskBytes
	t.Cleanup(func() { warpdb.MaxTaskBytes = old })

	db := wt.New(t)
	db.AddConversation("small", t0)
	db.AddConversation("large", t0)
	db.AddTask("small", "root", wt.Task("root", "", "", wt.UserQuery(hdr("m1", "r1", 0), "a")), t0)
	large := wt.Task("root", "", "", wt.UserQuery(hdr("m2", "r2", 0), "a much longer query than the limit allows"))
	db.AddTask("large", "root", large, t0)
	warpdb.MaxTaskBytes = len(large) - 1

	store, err := warpdb.Open(db.Path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	convs, err := store.Conversations(context.Background())
	if err != nil {
		t.Fatalf("Conversations: %v", err)
	}
	byID := map[string]warpdb.Conversation{}
	for _, conv := range convs {
		byID[conv.ID] = conv
	}
	if byID["small"].Oversized || !byID["large"].Oversized {
		t.Fatalf("conversations = %+v", convs)
	}
	if _, err := store.Tasks(context.Background(), "large"); err == nil {
		t.Fatal("Tasks accepted an oversized task")
	}
}

func TestDecodeTaskRejectsTruncatedPayload(t *testing.T) {
	payload := wt.Task("root", "", "", wt.AgentOutput(hdr("m1", "r1", 0), "hello"))
	if _, err := warpdb.DecodeTask(payload[:len(payload)-2]); err == nil {
		t.Fatal("DecodeTask accepted a truncated payload")
	}
}

func TestRequestIDsReadsOnlyRequestIdentity(t *testing.T) {
	payload := wt.Task("root", "", "",
		wt.UserQuery(hdr("m1", "r1", 0), "a"),
		wt.AgentOutput(hdr("m2", "r1", 0), "b"),
		wt.ShellResult(hdr("m3", "r2", 0), "c", "d", 0),
		wt.UserQuery(wt.Header{ID: "m4"}, "no request"),
	)
	got, err := warpdb.RequestIDs(payload)
	if err != nil {
		t.Fatalf("RequestIDs: %v", err)
	}
	if !slices.Equal(got, []string{"r1", "r2"}) {
		t.Fatalf("RequestIDs = %v", got)
	}
}

func TestStoreConversationsAndTasks(t *testing.T) {
	db := wt.New(t)
	db.AddConversation("older", t0.Add(time.Hour))
	db.AddConversation("newer", t0.Add(2*time.Hour))
	db.AddConversation("no-tasks", t0.Add(3*time.Hour))
	db.AddQuery("q1", "older", "/work/old", t0)
	db.AddQuery("q2", "older", "", t0.Add(30*time.Minute))
	db.AddQuery("q3", "older", "/work/latest", t0.Add(10*time.Minute))
	db.AddTask("older", "root", wt.Task("root", "Root", "",
		wt.UserQuery(hdr("m1", "r1", 0), "a"),
		wt.AgentOutput(hdr("m2", "r2", 0), "b"),
	), t0.Add(4*time.Hour))
	db.AddTask("older", "child", wt.Task("child", "Child", "root",
		wt.ShellCall(hdr("m3", "r2", 0), "c1", "ls"),
		wt.ShellResult(hdr("m4", "r3", 0), "c1", "x", 0),
	), t0.Add(time.Hour))
	db.AddTask("newer", "root", wt.Task("root", "", "", wt.UserQuery(hdr("m5", "r9", 0), "c")), t0.Add(2*time.Hour))

	store, err := warpdb.Open(db.Path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	convs, err := store.Conversations(context.Background())
	if err != nil {
		t.Fatalf("Conversations: %v", err)
	}
	if len(convs) != 2 || convs[0].ID != "older" || convs[1].ID != "newer" {
		t.Fatalf("conversations = %+v", convs)
	}
	older := convs[0]
	if older.Workspace != "/work/latest" || older.Requests != 3 || older.SizeBytes <= 0 {
		t.Fatalf("older = %+v", older)
	}
	if !older.StartedAt.Equal(t0) || !older.UpdatedAt.Equal(t0.Add(4*time.Hour)) {
		t.Fatalf("older times = %v .. %v", older.StartedAt, older.UpdatedAt)
	}
	newer := convs[1]
	if newer.Workspace != "" || newer.Requests != 1 || !newer.StartedAt.Equal(newer.UpdatedAt) {
		t.Fatalf("newer = %+v", newer)
	}

	tasks, err := store.Tasks(context.Background(), "older")
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if len(tasks) != 2 || tasks[0].ID != "root" || tasks[1].ParentTaskID != "root" || len(tasks[1].Messages) != 2 {
		t.Fatalf("tasks = %+v", tasks)
	}
}

func TestStoreReportsCorruptTask(t *testing.T) {
	db := wt.New(t)
	db.AddConversation("conv", t0)
	db.AddTask("conv", "broken", []byte{0x2a, 0xff}, t0)

	store, err := warpdb.Open(db.Path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.Conversations(context.Background()); err == nil {
		t.Fatal("Conversations accepted a corrupt task")
	}
	if _, err := store.Tasks(context.Background(), "conv"); err == nil {
		t.Fatal("Tasks accepted a corrupt task")
	}
}

func jsonEqual(t *testing.T, got json.RawMessage, want string) bool {
	t.Helper()
	var a, b any
	if err := json.Unmarshal(got, &a); err != nil {
		t.Fatalf("unmarshal %s: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &b); err != nil {
		t.Fatalf("unmarshal %s: %v", want, err)
	}
	ga, _ := json.Marshal(a)
	gb, _ := json.Marshal(b)
	return string(ga) == string(gb)
}
