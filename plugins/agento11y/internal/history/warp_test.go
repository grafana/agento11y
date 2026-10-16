package history

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
	wt "github.com/grafana/agento11y/plugins/agento11y/internal/agents/warp/warpdb/warpdbtest"
)

var warpT0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func warpHdr(id, task, req string, offset time.Duration) wt.Header {
	return wt.Header{ID: id, TaskID: task, RequestID: req, At: warpT0.Add(offset)}
}

func TestWarpDatabasePaths(t *testing.T) {
	home := filepath.Join("/", "home", "dev")
	group := filepath.Join(home, "Library", "Group Containers", "2BBY89MBSN.dev.warp", "Library", "Application Support")
	legacy := filepath.Join(home, "Library", "Application Support")
	tests := []struct {
		name, goos, home, xdg string
		want                  []string
	}{
		{
			name: "macos", goos: "darwin", home: home,
			want: []string{
				filepath.Join(group, "dev.warp.Warp-Stable", "warp.sqlite"),
				filepath.Join(group, "dev.warp.Warp-Stable", "tui", "warp.sqlite"),
				filepath.Join(group, "dev.warp.Warp-Preview", "warp.sqlite"),
				filepath.Join(group, "dev.warp.Warp-Preview", "tui", "warp.sqlite"),
				filepath.Join(legacy, "dev.warp.Warp-Stable", "warp.sqlite"),
				filepath.Join(legacy, "dev.warp.Warp-Stable", "tui", "warp.sqlite"),
				filepath.Join(legacy, "dev.warp.Warp-Preview", "warp.sqlite"),
				filepath.Join(legacy, "dev.warp.Warp-Preview", "tui", "warp.sqlite"),
			},
		},
		{
			name: "linux default state", goos: "linux", home: home,
			want: []string{
				filepath.Join(home, ".local", "state", "warp-terminal", "warp.sqlite"),
				filepath.Join(home, ".local", "state", "warp-terminal", "tui", "warp.sqlite"),
				filepath.Join(home, ".local", "state", "warp-terminal-preview", "warp.sqlite"),
				filepath.Join(home, ".local", "state", "warp-terminal-preview", "tui", "warp.sqlite"),
			},
		},
		{
			name: "linux xdg state", goos: "linux", home: home, xdg: "/state",
			want: []string{
				filepath.Join("/state", "warp-terminal", "warp.sqlite"),
				filepath.Join("/state", "warp-terminal", "tui", "warp.sqlite"),
				filepath.Join("/state", "warp-terminal-preview", "warp.sqlite"),
				filepath.Join("/state", "warp-terminal-preview", "tui", "warp.sqlite"),
			},
		},
		{name: "no home", goos: "darwin"},
		{name: "windows", goos: "windows", home: home},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := warpDatabasePaths(tt.goos, tt.home, tt.xdg); !slices.Equal(got, tt.want) {
				t.Fatalf("warpDatabasePaths = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWarpMatch(t *testing.T) {
	imp := &warpImporter{}
	for path, want := range map[string]bool{
		"/x/warp.sqlite":     true,
		"/x/tui/warp.sqlite": true,
		"/x/warp.sqlite-wal": false,
		"/x/warp.sqlite-shm": false,
		"/x/other.sqlite":    false,
	} {
		if got := imp.Match(path); got != want {
			t.Errorf("Match(%q) = %v, want %v", path, got, want)
		}
	}
}

func newWarpFixture(t *testing.T) *wt.Builder {
	t.Helper()
	db := wt.New(t)
	db.AddConversation("conv", warpT0.Add(time.Hour))
	db.AddQuery("q1", "conv", "/work/repo", warpT0)
	db.AddTask("conv", "root", wt.Task("root", "Fix the build", "",
		// Request r1: user query, then a shell call answered in r2.
		wt.UserQuery(warpHdr("m1", "root", "r1", 0), "why is CI red? token ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij"),
		wt.ModelUsed(warpHdr("m2", "root", "r1", time.Second), "claude-4-6-sonnet-high", "Claude Sonnet"),
		wt.Reasoning(warpHdr("m3", "root", "r1", 2*time.Second), "check the tests"),
		wt.ShellCall(warpHdr("m4", "root", "r1", 3*time.Second), "call-1", "go test ./..."),
		wt.RequestMetadata(warpHdr("m5", "root", "r1", 4*time.Second), warpT0, warpT0.Add(4*time.Second), wt.OutcomeCompleted,
			wt.Usage{Category: "primary_agent", Model: "claude-4-6-sonnet-high", Input: 100, Output: 20, CacheRead: 300, CacheWrite: 5, InputCost: 150, OutputCost: 50}),
		// Request r2 carries the tool result plus a subagent spawned in a child task.
		wt.ShellResult(warpHdr("m6", "root", "r2", 10*time.Second), "call-1", "FAIL", 1),
		wt.ModelUsed(warpHdr("m7", "root", "r2", 11*time.Second), "gemini-3.5-flash", "Gemini Flash"),
		wt.OtherCall(warpHdr("m8", "root", "r2", 12*time.Second), "call-2", 19),
		// Request r4 predates request metadata and was cancelled before a model ran.
		wt.UserQuery(warpHdr("m11", "root", "r4", 30*time.Second), "never mind"),
		wt.RequestMetadata(warpHdr("m12", "root", "r4", 31*time.Second), warpT0.Add(30*time.Second), warpT0.Add(31*time.Second), wt.OutcomeCanceled),
	), warpT0.Add(time.Hour))
	db.AddTask("conv", "child", wt.Task("child", "Subagent", "root",
		wt.OtherResult(warpHdr("m9", "child", "r3", 20*time.Second), "call-2", 23),
		wt.AgentOutput(warpHdr("m10", "child", "r3", 21*time.Second), "the test fails on purpose"),
	), warpT0.Add(time.Hour))
	return db
}

func warpFixtureSession(t *testing.T, imp *warpImporter, path string) SessionPreview {
	t.Helper()
	previews, err := imp.Previews(context.Background(), path)
	if err != nil {
		t.Fatalf("Previews: %v", err)
	}
	if len(previews) != 1 {
		t.Fatalf("Previews = %+v", previews)
	}
	return previews[0]
}

func TestWarpPreviewIsContentFree(t *testing.T) {
	db := newWarpFixture(t)
	imp := &warpImporter{}
	got := warpFixtureSession(t, imp, db.Path)
	if got.Agent != AgentWarp || got.SessionID != "conv" || got.Title != "conv" || got.Workspace != "/work/repo" {
		t.Fatalf("preview identity = %+v", got)
	}
	if got.TurnCount != 4 || got.ApproxTurns || got.SourcePath != db.Path || got.SizeBytes <= 0 {
		t.Fatalf("preview counts = %+v", got)
	}
	if !got.StartedAt.Equal(warpT0) || !got.LastActivityAt.Equal(warpT0.Add(time.Hour)) {
		t.Fatalf("preview times = %v .. %v", got.StartedAt, got.LastActivityAt)
	}
}

func TestWarpTurnsMapRequests(t *testing.T) {
	db := newWarpFixture(t)
	imp := &warpImporter{}
	sess := warpFixtureSession(t, imp, db.Path)
	turns := collectTurns(t, imp, sess)
	if len(turns) != 4 {
		t.Fatalf("turns = %d, want 4", len(turns))
	}
	ids := make([]string, len(turns))
	for i, turn := range turns {
		ids[i] = turn.Source.TurnID
		if turn.Gen.ConversationID != "conv" || turn.Gen.AgentName != "warp" || turn.Gen.ConversationTitle != "Fix the build" {
			t.Errorf("turn %d identity = %+v", i, turn.Gen)
		}
		if turn.Gen.Tags["cwd"] != "/work/repo" || !turn.Source.TurnIDStable || turn.Source.TurnIndex != i {
			t.Errorf("turn %d source/tags = %+v %+v", i, turn.Source, turn.Gen.Tags)
		}
		if i > 0 && !slices.Equal(turn.Gen.ParentGenerationIDs, []string{turns[i-1].Gen.ID}) {
			t.Errorf("turn %d parents = %v", i, turn.Gen.ParentGenerationIDs)
		}
	}
	if !slices.Equal(ids, []string{"r1", "r2", "r3", "r4"}) {
		t.Fatalf("turn order = %v", ids)
	}

	first := turns[0]
	if first.Gen.Model != (agento11y.ModelRef{Provider: "anthropic", Name: "claude-4-6-sonnet-high"}) {
		t.Fatalf("first model = %+v", first.Gen.Model)
	}
	wantUsage := agento11y.TokenUsage{
		InputTokens: 405, OutputTokens: 20, TotalTokens: 425,
		CacheReadInputTokens: 300, CacheWriteInputTokens: 5,
		InputSemantics: agento11y.TokenInputSemanticsInclusive,
	}
	if first.Gen.Usage != wantUsage || first.Quality.ApproxUsage {
		t.Fatalf("first usage = %+v quality=%+v", first.Gen.Usage, first.Quality)
	}
	if first.Gen.Metadata["cost_usd"] != 2.0 {
		t.Fatalf("first cost = %v", first.Gen.Metadata["cost_usd"])
	}
	if !first.Gen.StartedAt.Equal(warpT0) || !first.Gen.CompletedAt.Equal(warpT0.Add(4*time.Second)) {
		t.Fatalf("first span = %v .. %v", first.Gen.StartedAt, first.Gen.CompletedAt)
	}
	if got := warpPartKinds(first.Gen.Input); !slices.Equal(got, []agento11y.PartKind{agento11y.PartKindText}) {
		t.Fatalf("first input kinds = %v", got)
	}
	if got := warpPartKinds(first.Gen.Output); !slices.Equal(got, []agento11y.PartKind{agento11y.PartKindThinking, agento11y.PartKindToolCall}) {
		t.Fatalf("first output kinds = %v", got)
	}
	call := first.Gen.Output[1].Parts[0].ToolCall
	if call.ID != "call-1" || call.Name != "run_shell_command" || string(call.InputJSON) != `{"command":"go test ./..."}` {
		t.Fatalf("first tool call = %+v", call)
	}
	if len(first.Gen.Tools) != 1 || first.Gen.Tools[0].Name != "run_shell_command" {
		t.Fatalf("first tools = %+v", first.Gen.Tools)
	}

	second := turns[1]
	if second.Gen.Model.Name != "gemini-3.5-flash" || second.Gen.Model.Provider != "gemini" || !second.Quality.ApproxUsage {
		t.Fatalf("second model/quality = %+v %+v", second.Gen.Model, second.Quality)
	}
	result := second.Gen.Input[0].Parts[0].ToolResult
	if second.Gen.Input[0].Role != agento11y.RoleTool || result.Name != "run_shell_command" || result.Content != "FAIL" || !result.IsError {
		t.Fatalf("second input = %+v", result)
	}
	if !second.Gen.StartedAt.Equal(warpT0.Add(10*time.Second)) || !second.Gen.CompletedAt.Equal(warpT0.Add(12*time.Second)) {
		t.Fatalf("second span from messages = %v .. %v", second.Gen.StartedAt, second.Gen.CompletedAt)
	}

	third := turns[2]
	if got := third.Gen.Input[0].Parts[0].ToolResult; got.Name != "subagent" || got.ToolCallID != "call-2" {
		t.Fatalf("child-task result = %+v", got)
	}
	if third.Gen.Output[0].Parts[0].Text != "the test fails on purpose" || !third.Quality.MissingModel {
		t.Fatalf("child-task output = %+v quality=%+v", third.Gen.Output, third.Quality)
	}

	fourth := turns[3]
	if fourth.Gen.StopReason != "cancelled" || fourth.Gen.CallError != "" || !fourth.Quality.ApproxUsage {
		t.Fatalf("cancelled request = %+v quality=%+v", fourth.Gen, fourth.Quality)
	}
}

func TestWarpBilledModelResolvesToModelID(t *testing.T) {
	db := wt.New(t)
	db.AddConversation("conv", warpT0)
	db.AddTask("conv", "root", wt.Task("root", "", "",
		wt.ModelUsed(warpHdr("m1", "root", "r1", 0), "gemini-3.8-flash", "gemini 3.8 flash"),
		wt.AgentOutput(warpHdr("m2", "root", "r1", time.Second), "hi"),
		// r2 and r3 have billing records but no ModelUsed message.
		wt.ShellCall(warpHdr("m3", "root", "r2", 2*time.Second), "call-1", "ls"),
		wt.RequestMetadata(warpHdr("m4", "root", "r2", 3*time.Second), warpT0.Add(2*time.Second), warpT0.Add(3*time.Second), wt.OutcomeCompleted,
			wt.Usage{Category: "primary_agent", Model: "Gemini 3.8 Flash", Input: 1, Output: 1}),
		wt.ShellCall(warpHdr("m5", "root", "r3", 4*time.Second), "call-2", "pwd"),
		wt.RequestMetadata(warpHdr("m6", "root", "r3", 5*time.Second), warpT0.Add(4*time.Second), warpT0.Add(5*time.Second), wt.OutcomeCompleted,
			wt.Usage{Category: "primary_agent", Model: "GPT-6 Luna (low reasoning)", Input: 1, Output: 1}),
	), warpT0)

	imp := &warpImporter{}
	turns := collectTurns(t, imp, warpFixtureSession(t, imp, db.Path))
	if len(turns) != 3 {
		t.Fatalf("turns = %d, want 3", len(turns))
	}
	got := []string{turns[0].Gen.Model.Name, turns[1].Gen.Model.Name, turns[2].Gen.Model.Name}
	want := []string{"gemini-3.8-flash", "gemini-3.8-flash", "GPT-6 Luna (low reasoning)"}
	if !slices.Equal(got, want) {
		t.Fatalf("models = %v, want %v", got, want)
	}
}

func TestWarpToolResultsTakeTheirCallNameAcrossRequests(t *testing.T) {
	db := wt.New(t)
	db.AddConversation("conv", warpT0)
	db.AddTask("conv", "root", wt.Task("root", "", "",
		wt.MCPCall(warpHdr("m1", "root", "r1", 0), "call-mcp", "search_issues", map[string]any{"q": "flaky"}),
		wt.ShellCall(warpHdr("m2", "root", "r1", time.Second), "call-shell", "sleep 100"),
		wt.MCPResult(warpHdr("m3", "root", "r2", 2*time.Second), "call-mcp", "issue 1"),
		wt.CancelResult(warpHdr("m4", "root", "r2", 2*time.Second), "call-shell"),
		wt.RequestMetadata(warpHdr("m5", "root", "r2", 3*time.Second), warpT0.Add(2*time.Second), warpT0.Add(3*time.Second), wt.OutcomeCompleted,
			wt.Usage{Category: "primary_agent", Model: "m", Input: 1, Output: 1, InputCost: 100, PlatformCost: 50}),
	), warpT0)

	imp := &warpImporter{}
	turns := collectTurns(t, imp, warpFixtureSession(t, imp, db.Path))
	if len(turns) != 2 || len(turns[1].Gen.Input) != 2 {
		t.Fatalf("turns = %+v", turns)
	}
	mcp := turns[1].Gen.Input[0].Parts[0].ToolResult
	cancel := turns[1].Gen.Input[1].Parts[0].ToolResult
	if mcp.Name != "search_issues" || mcp.Content != "issue 1" {
		t.Fatalf("mcp result = %+v", mcp)
	}
	if cancel.Name != "run_shell_command" || !cancel.IsError {
		t.Fatalf("cancel result = %+v", cancel)
	}
	if got := turns[1].Gen.Metadata["cost_usd"]; got != 1.5 {
		t.Fatalf("cost_usd = %v, want 1.5 including the platform charge", got)
	}
}

func TestWarpRunImportSanitizesAndIsIdempotent(t *testing.T) {
	db := newWarpFixture(t)
	imp := &warpImporter{}
	sess := warpFixtureSession(t, imp, db.Path)
	ledger := testLedger(t)

	exporter := &recordingExporter{}
	result, err := RunImport(context.Background(), ImportOptions{
		Agent: AgentWarp, Importer: imp, Sessions: []SessionPreview{sess},
		Exporter: exporter, Ledger: ledger,
	})
	if err != nil {
		t.Fatalf("RunImport: %v", err)
	}
	if result.Imported != 4 || result.Failed != 0 || len(exporter.got) != 4 {
		t.Fatalf("first import = %+v", result)
	}
	prompt := exporter.got[0].Gen.Input[0].Parts[0].Text
	if strings.Contains(prompt, "ghp_") || !strings.Contains(prompt, "[REDACTED:github-pat]") {
		t.Fatalf("prompt was not redacted: %q", prompt)
	}

	again, err := RunImport(context.Background(), ImportOptions{
		Agent: AgentWarp, Importer: imp, Sessions: []SessionPreview{sess},
		Exporter: &recordingExporter{}, Ledger: ledger,
	})
	if err != nil {
		t.Fatalf("second RunImport: %v", err)
	}
	if again.Imported != 0 || again.Skipped != 4 {
		t.Fatalf("second import = %+v", again)
	}
}

func TestWarpDiscoverWarnsOnCorruptTask(t *testing.T) {
	db := wt.New(t)
	db.AddConversation("conv", warpT0)
	db.AddTask("conv", "broken", []byte{0x2a, 0xff}, warpT0)

	got, err := Discover(context.Background(), AgentWarp, &warpImporter{roots: []string{db.Path}}, DiscoverOptions{})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got.Sessions) != 0 || len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], db.Path) {
		t.Fatalf("Discover = %+v, want one warning and no sessions", got)
	}
}

func TestWarpTasksParentFirstGuardsCycles(t *testing.T) {
	db := wt.New(t)
	db.AddConversation("conv", warpT0)
	db.AddTask("conv", "a", wt.Task("a", "", "b", wt.AgentOutput(warpHdr("m1", "a", "r1", 0), "x")), warpT0)
	db.AddTask("conv", "b", wt.Task("b", "", "a", wt.AgentOutput(warpHdr("m2", "b", "r1", time.Second), "y")), warpT0)

	imp := &warpImporter{}
	turns := collectTurns(t, imp, warpFixtureSession(t, imp, db.Path))
	if len(turns) != 1 || len(turns[0].Gen.Output) != 2 {
		t.Fatalf("turns = %+v", turns)
	}
}

func warpPartKinds(messages []agento11y.Message) []agento11y.PartKind {
	var kinds []agento11y.PartKind
	for _, msg := range messages {
		for _, part := range msg.Parts {
			kinds = append(kinds, part.Kind)
		}
	}
	return kinds
}
