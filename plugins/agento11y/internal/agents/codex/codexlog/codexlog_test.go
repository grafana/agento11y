package codexlog

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadSessionMetaSubagent(t *testing.T) {
	path := writeTranscript(t, `{"type":"session_meta","payload":{"id":"child","thread_source":"subagent","agent_nickname":"Dalton","agent_role":"reviewer","source":{"subagent":{"thread_spawn":{"parent_thread_id":"parent","depth":1}}}}}`)

	got, ok, err := ReadSessionMeta(path, LiveScanOptions())
	if err != nil {
		t.Fatalf("ReadSessionMeta: %v", err)
	}
	if !ok {
		t.Fatal("expected session_meta")
	}
	if got.SessionID != "child" || got.ThreadSource != "subagent" || got.ParentSessionID != "parent" || got.AgentNickname != "Dalton" || got.AgentRole != "reviewer" || got.AgentDepth != 1 {
		t.Fatalf("unexpected meta: %+v", got)
	}
}

func TestReadSessionMetaOrdinarySession(t *testing.T) {
	path := writeTranscript(t, `{"type":"session_meta","payload":{"id":"parent","thread_source":"cli","agent_role":"default"}}`)

	got, ok, err := ReadSessionMeta(path, LiveScanOptions())
	if err != nil {
		t.Fatalf("ReadSessionMeta: %v", err)
	}
	if !ok {
		t.Fatal("expected session_meta")
	}
	if got.SessionID != "parent" || got.ThreadSource != "cli" || got.ParentSessionID != "" {
		t.Fatalf("unexpected meta: %+v", got)
	}
}

func TestResolveSpawnLink(t *testing.T) {
	path := writeTranscript(t,
		`{"type":"session_meta","payload":{"id":"parent"}}`,
		`{"type":"turn_context","payload":{"turn_id":"turn-1"}}`,
		`{"type":"response_item","payload":{"type":"function_call","name":"spawn_agent","call_id":"call_1"}}`,
		`{"type":"response_item","payload":{"type":"function_call_output","call_id":"call_1","output":"{\"agent_id\":\"child\",\"nickname\":\"Dalton\"}"}}`,
	)

	got, ok, err := ResolveSpawnLink(path, "child", LiveScanOptions(), func(sessionID, turnID string) string {
		return "gen:" + sessionID + ":" + turnID
	})
	if err != nil {
		t.Fatalf("ResolveSpawnLink: %v", err)
	}
	if !ok {
		t.Fatal("expected spawn link")
	}
	if got.ChildSessionID != "child" || got.ParentSessionID != "parent" || got.ParentTurnID != "turn-1" || got.ParentGenerationID != "gen:parent:turn-1" || got.SpawnCallID != "call_1" || got.AgentNickname != "Dalton" {
		t.Fatalf("unexpected link: %+v", got)
	}
}

func TestResolveSpawnLinkWithParallelCalls(t *testing.T) {
	path := writeTranscript(t,
		`{"type":"session_meta","payload":{"id":"parent"}}`,
		`{"type":"turn_context","payload":{"turn_id":"turn-1"}}`,
		`{"type":"response_item","payload":{"type":"function_call","name":"spawn_agent","call_id":"call_a"}}`,
		`{"type":"response_item","payload":{"type":"function_call","name":"spawn_agent","call_id":"call_b"}}`,
		`{"type":"response_item","payload":{"type":"function_call_output","call_id":"call_a","output":{"agent_id":"other","nickname":"Ada"}}}`,
		`{"type":"response_item","payload":{"type":"function_call_output","call_id":"call_b","output":{"agent_id":"child","nickname":"Lin"}}}`,
	)

	got, ok, err := ResolveSpawnLink(path, "child", LiveScanOptions(), func(sessionID, turnID string) string {
		return "gen:" + sessionID + ":" + turnID
	})
	if err != nil {
		t.Fatalf("ResolveSpawnLink: %v", err)
	}
	if !ok {
		t.Fatal("expected spawn link")
	}
	if got.SpawnCallID != "call_b" || got.AgentNickname != "Lin" {
		t.Fatalf("unexpected link: %+v", got)
	}
}

func TestResolveSpawnLinkRequiresTurnContext(t *testing.T) {
	path := writeTranscript(t,
		`{"type":"session_meta","payload":{"id":"parent"}}`,
		`{"type":"response_item","payload":{"type":"function_call","name":"spawn_agent","call_id":"call_1"}}`,
		`{"type":"response_item","payload":{"type":"function_call_output","call_id":"call_1","output":"{\"agent_id\":\"child\"}"}}`,
	)

	_, ok, err := ResolveSpawnLink(path, "child", LiveScanOptions(), nil)
	if err != nil {
		t.Fatalf("ResolveSpawnLink: %v", err)
	}
	if ok {
		t.Fatal("expected no link without parent turn context")
	}
}

func TestResolveSpawnLinkMalformedOutputFailsOpen(t *testing.T) {
	path := writeTranscript(t,
		`{"type":"session_meta","payload":{"id":"parent"}}`,
		`{"type":"turn_context","payload":{"turn_id":"turn-1"}}`,
		`{"type":"response_item","payload":{"type":"function_call","name":"spawn_agent","call_id":"call_1"}}`,
		`{"type":"response_item","payload":{"type":"function_call_output","call_id":"call_1","output":"not json"}}`,
	)

	_, ok, err := ResolveSpawnLink(path, "child", LiveScanOptions(), nil)
	if err != nil {
		t.Fatalf("ResolveSpawnLink: %v", err)
	}
	if ok {
		t.Fatal("expected malformed output to fail open")
	}
}

func TestReadTokenUsageForTurnUsesCumulativeDelta(t *testing.T) {
	path := writeTranscript(t,
		`{"type":"turn_context","payload":{"turn_id":"turn-1"}}`,
		`{"type":"response_item","payload":{"type":"reasoning"}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"cached_input_tokens":20,"output_tokens":10,"reasoning_output_tokens":3,"total_tokens":110},"last_token_usage":{"input_tokens":100,"cached_input_tokens":20,"output_tokens":10,"reasoning_output_tokens":3,"total_tokens":110},"model_context_window":200000}}}`,
		`{"type":"turn_context","payload":{"turn_id":"turn-2"}}`,
		`{"type":"response_item","payload":{"type":"reasoning"}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":170,"cached_input_tokens":60,"output_tokens":25,"reasoning_output_tokens":7,"total_tokens":195},"last_token_usage":{"input_tokens":70,"cached_input_tokens":40,"output_tokens":15,"reasoning_output_tokens":4,"total_tokens":85},"model_context_window":200000}}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":260,"cached_input_tokens":140,"output_tokens":40,"reasoning_output_tokens":12,"total_tokens":300},"last_token_usage":{"input_tokens":90,"cached_input_tokens":80,"output_tokens":15,"reasoning_output_tokens":5,"total_tokens":105},"model_context_window":200000}}}`,
	)

	got, ok, err := ReadTokenUsageForTurn(path, "turn-2", LiveScanOptions())
	if err != nil {
		t.Fatalf("ReadTokenUsageForTurn: %v", err)
	}
	if !ok {
		t.Fatal("expected token usage")
	}
	want := TokenUsage{InputTokens: 160, CachedInputTokens: 120, OutputTokens: 30, ReasoningOutputTokens: 9, TotalTokens: 190}
	if got.TurnUsage != want {
		t.Fatalf("TurnUsage = %+v, want %+v", got.TurnUsage, want)
	}
	if got.LastUsage.TotalTokens != 105 {
		t.Fatalf("LastUsage.TotalTokens = %d, want final sample 105", got.LastUsage.TotalTokens)
	}
	if got.BaselineUsage.TotalTokens != 110 || got.TotalUsage.TotalTokens != 300 || got.ModelContextWindow != 200000 || got.Source != "turn_context_delta" {
		t.Fatalf("unexpected snapshot: %+v", got)
	}
}

func TestReadTokenUsageForTurnUsesZeroBaselineForFirstTurn(t *testing.T) {
	path := writeTranscript(t,
		`{"type":"turn_context","payload":{"turn_id":"turn-1"}}`,
		`{"type":"response_item","payload":{"type":"reasoning"}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":10,"cached_input_tokens":2,"output_tokens":3,"reasoning_output_tokens":1,"total_tokens":13},"last_token_usage":{"input_tokens":10,"cached_input_tokens":2,"output_tokens":3,"reasoning_output_tokens":1,"total_tokens":13},"model_context_window":128000}}}`,
	)

	got, ok, err := ReadTokenUsageForTurn(path, "turn-1", LiveScanOptions())
	if err != nil {
		t.Fatalf("ReadTokenUsageForTurn: %v", err)
	}
	if !ok {
		t.Fatal("expected token usage")
	}
	want := TokenUsage{InputTokens: 10, CachedInputTokens: 2, OutputTokens: 3, ReasoningOutputTokens: 1, TotalTokens: 13}
	if got.TurnUsage != want {
		t.Fatalf("TurnUsage = %+v, want %+v", got.TurnUsage, want)
	}
}

func TestReadTokenUsageForTurnUsesPreModelTokenCountAsBaseline(t *testing.T) {
	path := writeTranscript(t,
		`{"type":"turn_context","payload":{"turn_id":"turn-1"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user"}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":1000,"cached_input_tokens":900,"output_tokens":50,"reasoning_output_tokens":10,"total_tokens":1050},"last_token_usage":{"input_tokens":200,"cached_input_tokens":100,"output_tokens":50,"reasoning_output_tokens":10,"total_tokens":250},"model_context_window":128000}}}`,
		`{"type":"response_item","payload":{"type":"reasoning"}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":1120,"cached_input_tokens":980,"output_tokens":80,"reasoning_output_tokens":20,"total_tokens":1200},"last_token_usage":{"input_tokens":120,"cached_input_tokens":80,"output_tokens":30,"reasoning_output_tokens":10,"total_tokens":150},"model_context_window":128000}}}`,
	)

	got, ok, err := ReadTokenUsageForTurn(path, "turn-1", LiveScanOptions())
	if err != nil {
		t.Fatalf("ReadTokenUsageForTurn: %v", err)
	}
	if !ok {
		t.Fatal("expected token usage")
	}
	want := TokenUsage{InputTokens: 120, CachedInputTokens: 80, OutputTokens: 30, ReasoningOutputTokens: 10, TotalTokens: 150}
	if got.TurnUsage != want {
		t.Fatalf("TurnUsage = %+v, want %+v", got.TurnUsage, want)
	}
	if got.BaselineUsage.TotalTokens != 1050 {
		t.Fatalf("BaselineUsage.TotalTokens = %d, want 1050", got.BaselineUsage.TotalTokens)
	}
}

func TestReadTokenUsageForTurnIgnoresNullInfo(t *testing.T) {
	path := writeTranscript(t,
		`{"type":"turn_context","payload":{"turn_id":"turn-1"}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":null,"rate_limits":{"plan_type":"pro"}}}`,
	)

	_, ok, err := ReadTokenUsageForTurn(path, "turn-1", LiveScanOptions())
	if err != nil {
		t.Fatalf("ReadTokenUsageForTurn: %v", err)
	}
	if ok {
		t.Fatal("expected no usage for null info")
	}
}

func TestReadTokenUsageForTurnRequiresBaselineForLaterTurn(t *testing.T) {
	path := writeTranscript(t,
		`{"type":"turn_context","payload":{"turn_id":"turn-1"}}`,
		`{"type":"turn_context","payload":{"turn_id":"turn-2"}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15},"last_token_usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}}`,
	)

	_, ok, err := ReadTokenUsageForTurn(path, "turn-2", LiveScanOptions())
	if err != nil {
		t.Fatalf("ReadTokenUsageForTurn: %v", err)
	}
	if ok {
		t.Fatal("expected no usage without a baseline for a later turn")
	}
}

func TestReadTokenUsageForTurnRejectsNegativeDelta(t *testing.T) {
	path := writeTranscript(t,
		`{"type":"turn_context","payload":{"turn_id":"turn-1"}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"output_tokens":10,"total_tokens":110},"last_token_usage":{"input_tokens":100,"output_tokens":10,"total_tokens":110}}}}`,
		`{"type":"turn_context","payload":{"turn_id":"turn-2"}}`,
		`{"type":"response_item","payload":{"type":"reasoning"}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":90,"output_tokens":10,"total_tokens":100},"last_token_usage":{"input_tokens":90,"output_tokens":10,"total_tokens":100}}}}`,
	)

	_, ok, err := ReadTokenUsageForTurn(path, "turn-2", LiveScanOptions())
	if err != nil {
		t.Fatalf("ReadTokenUsageForTurn: %v", err)
	}
	if ok {
		t.Fatal("expected no usage for negative cumulative delta")
	}
}

// TestReadSessionMetaSkipsOversizedLine covers both sides of the live skip: a
// line past the cap before session_meta costs only that line, and a
// session_meta past the cap (a huge AGENTS.md, say) reads as absent, not as an
// error.
func TestReadSessionMetaSkipsOversizedLine(t *testing.T) {
	oversized := strings.Repeat("x", liveMaxLineBytes+1)

	path := writeTranscript(t, oversized, `{"type":"session_meta","payload":{"id":"sess"}}`)
	meta, ok, err := ReadSessionMeta(path, LiveScanOptions())
	if err != nil || !ok || meta.SessionID != "sess" {
		t.Fatalf("ReadSessionMeta = %+v ok=%v err=%v; want sess after the oversized line", meta, ok, err)
	}

	path = writeTranscript(t, `{"type":"session_meta","payload":{"id":"sess","base_instructions":{"text":"`+oversized+`"}}}`)
	_, ok, err = ReadSessionMeta(path, LiveScanOptions())
	if err != nil || ok {
		t.Fatalf("ReadSessionMeta of an oversized session_meta: ok=%v err=%v; want not found and no error", ok, err)
	}
}

func TestResolveSpawnLinkAfterOversizedLine(t *testing.T) {
	path := writeTranscript(t,
		`{"type":"session_meta","payload":{"id":"parent"}}`,
		`{"type":"turn_context","payload":{"turn_id":"turn-1"}}`,
		`{"type":"response_item","payload":{"type":"function_call","name":"spawn_agent","call_id":"call_1"}}`,
		`{"type":"response_item","payload":{"type":"function_call_output","call_id":"call_0","output":"`+strings.Repeat("x", liveMaxLineBytes)+`"}}`,
		`{"type":"response_item","payload":{"type":"function_call_output","call_id":"call_1","output":"{\"agent_id\":\"child\",\"nickname\":\"Dalton\"}"}}`,
	)

	got, ok, err := ResolveSpawnLink(path, "child", LiveScanOptions(), nil)
	if err != nil || !ok || got.ParentSessionID != "parent" || got.ParentTurnID != "turn-1" || got.SpawnCallID != "call_1" {
		t.Fatalf("ResolveSpawnLink = %+v ok=%v err=%v; want the spawn after the oversized line", got, ok, err)
	}
}

// TestReadTokenUsageForTurnAfterOversizedLine is the reported bug: a
// screenshot pasted in turn 1 is stored inline as a base64 data URL, which
// makes one rollout line larger than the live cap. Every Stop rereads the
// rollout from the start, so a failure on that line cost every later turn its
// usage.
func TestReadTokenUsageForTurnAfterOversizedLine(t *testing.T) {
	image := `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,` +
		strings.Repeat("A", 1100*1024) + `"}]}}`
	path := writeTranscript(t,
		`{"type":"turn_context","payload":{"turn_id":"turn-1"}}`,
		image,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":"ok"}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"output_tokens":10,"total_tokens":110}}}}`,
		`{"type":"turn_context","payload":{"turn_id":"turn-2"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":"done"}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":300,"output_tokens":30,"total_tokens":330}}}}`,
	)
	got, ok, err := ReadTokenUsageForTurn(path, "turn-2", LiveScanOptions())
	if err != nil || !ok || got.TurnUsage.TotalTokens != 220 {
		t.Fatalf("turn-2 usage = %+v ok=%v err=%v; want total 220", got.TurnUsage, ok, err)
	}
}

// TestReadTokenUsageForTurnClassifiesOversizedLines pins what a skipped line
// means for the turn it sits in, read from the start of the line. Wrong usage
// is worse than none, so a line whose type cannot be read, or a skipped
// token_count, gives no usage when it comes before the target turn ends.
// After that it cannot be the target's count and is ignored.
func TestReadTokenUsageForTurnClassifiesOversizedLines(t *testing.T) {
	big := strings.Repeat("A", liveMaxLineBytes)
	tokens := func(total int) string {
		return fmt.Sprintf(`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":%d,"total_tokens":%d}}}}`, total, total)
	}
	turn1 := []string{
		`{"type":"turn_context","payload":{"turn_id":"turn-1"}}`,
		`{"type":"response_item","payload":{"type":"reasoning"}}`,
		tokens(100),
	}
	tests := []struct {
		name   string
		lines  []string
		turnID string
		want   int64 // 0 means no usage
	}{
		{
			name: "model activity before any parsed activity",
			lines: append(append([]string{}, turn1...),
				`{"type":"turn_context","payload":{"turn_id":"turn-2"}}`,
				`{"timestamp":"2026-11-05T10:00:00Z","type":"response_item","payload":{"type":"custom_tool_call","name":"apply_patch","call_id":"c1","input":"`+big+`"}}`,
				tokens(300),
				`{"type":"response_item","payload":{"type":"message","role":"assistant","content":"done"}}`,
				tokens(350),
			),
			turnID: "turn-2",
			want:   250,
		},
		{
			name: "next turn_context ends the target turn",
			lines: append(append([]string{}, turn1...),
				`{"type":"turn_context","payload":{"cwd":"/work","user_instructions":"`+big+`","turn_id":"turn-2"}}`,
				`{"type":"response_item","payload":{"type":"reasoning"}}`,
				tokens(900),
			),
			turnID: "turn-1",
			want:   100,
		},
		{
			name: "own turn_context skipped",
			lines: append(append([]string{}, turn1...),
				`{"type":"turn_context","payload":{"cwd":"/work","user_instructions":"`+big+`","turn_id":"turn-2"}}`,
				`{"type":"response_item","payload":{"type":"reasoning"}}`,
				tokens(900),
			),
			turnID: "turn-2",
		},
		{
			name: "type unreadable",
			lines: append(append([]string{}, turn1...),
				`{"type":"turn_context","payload":{"turn_id":"turn-2"}}`,
				`{"payload":{"input":"`+big+`"},"type":"response_item"}`,
				`{"type":"response_item","payload":{"type":"reasoning"}}`,
				tokens(300),
			),
			turnID: "turn-2",
		},
		{
			name: "token_count skipped",
			lines: append(append([]string{}, turn1...),
				`{"type":"turn_context","payload":{"turn_id":"turn-2"}}`,
				`{"type":"response_item","payload":{"type":"reasoning"}}`,
				`{"type":"event_msg","payload":{"type":"token_count","info":{"rate_limits":"`+big+`"}}}`,
				tokens(300),
			),
			turnID: "turn-2",
		},
		{
			name: "token_count after the target turn ends",
			lines: append(append([]string{}, turn1...),
				`{"type":"turn_context","payload":{"turn_id":"turn-2"}}`,
				`{"type":"response_item","payload":{"type":"reasoning"}}`,
				tokens(300),
				`{"type":"turn_context","payload":{"turn_id":"turn-3"}}`,
				`{"type":"event_msg","payload":{"type":"token_count","info":{"rate_limits":"`+big+`"}}}`,
				tokens(400),
			),
			turnID: "turn-2",
			want:   200,
		},
		{
			name: "type unreadable after the target turn ends",
			lines: append(append([]string{}, turn1...),
				`{"type":"turn_context","payload":{"turn_id":"turn-2"}}`,
				`{"type":"response_item","payload":{"type":"reasoning"}}`,
				tokens(300),
				`{"type":"turn_context","payload":{"turn_id":"turn-3"}}`,
				`{"payload":{"input":"`+big+`"},"type":"response_item"}`,
				tokens(400),
			),
			turnID: "turn-2",
			want:   200,
		},
		{
			name: "token_count in an unknown turn after the target",
			lines: append(append([]string{}, turn1...),
				`{"type":"turn_context","payload":{"turn_id":"turn-2"}}`,
				`{"type":"response_item","payload":{"type":"reasoning"}}`,
				tokens(300),
				`{"type":"turn_context","payload":{"cwd":"/work","user_instructions":"`+big+`","turn_id":"turn-3"}}`,
				`{"type":"event_msg","payload":{"type":"token_count","info":{"rate_limits":"`+big+`"}}}`,
				tokens(400),
			),
			turnID: "turn-2",
			want:   200,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok, err := ReadTokenUsageForTurn(writeTranscript(t, tt.lines...), tt.turnID, LiveScanOptions())
			if err != nil {
				t.Fatalf("ReadTokenUsageForTurn: %v", err)
			}
			if tt.want == 0 {
				if ok {
					t.Fatalf("usage = %+v baseline=%+v; want none", got.TurnUsage, got.BaselineUsage)
				}
				return
			}
			if !ok || got.TurnUsage.TotalTokens != tt.want {
				t.Fatalf("total = %d ok=%v baseline=%d; want %d", got.TurnUsage.TotalTokens, ok, got.BaselineUsage.TotalTokens, tt.want)
			}
		})
	}
}

// oversizedMarker in a testdata/live rollout is expanded into a run of bytes
// past the live line cap, so the corpus stays a few KiB on disk.
const oversizedMarker = "@@OVERSIZED@@"

// liveRollout returns the expanded copy of testdata/live/name.
func liveRollout(t testing.TB, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "live", name))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), name)
	expanded := strings.ReplaceAll(string(raw), oversizedMarker, strings.Repeat("A", liveMaxLineBytes))
	if err := os.WriteFile(path, []byte(expanded), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLiveCorpusTurnUsage reads every turn of every testdata/live rollout with
// the live budget. Each rollout but plain.jsonl has a line past the live cap.
// Every turn gets its own usage, except a turn whose own turn_context was
// skipped, which gets none.
func TestLiveCorpusTurnUsage(t *testing.T) {
	want := map[string][]string{
		"plain.jsonl":                {"in=100 out=10 total=110", "in=200 out=20 total=220", "in=240 out=30 total=270"},
		"screenshot.jsonl":           {"in=100 out=10 total=110", "in=200 out=20 total=220", "in=240 out=30 total=270"},
		"large-tool-output.jsonl":    {"in=100 out=10 total=110", "in=300 out=30 total=330", "in=100 out=10 total=110"},
		"large-session-meta.jsonl":   {"in=100 out=10 total=110", "in=150 out=15 total=165"},
		"two-screenshots.jsonl":      {"in=100 out=10 total=110", "in=200 out=20 total=220", "in=300 out=30 total=330", "in=400 out=40 total=440"},
		"large-tool-call.jsonl":      {"in=90 out=10 total=100", "in=225 out=25 total=250", "in=90 out=10 total=100"},
		"large-turn-context.jsonl":   {"in=90 out=10 total=100", "none", "in=90 out=10 total=100"},
		"late-oversized-lines.jsonl": {"in=90 out=10 total=100", "in=90 out=10 total=100", "none"},
	}
	files, _ := filepath.Glob(filepath.Join("testdata", "live", "*.jsonl"))
	if len(files) != len(want) {
		t.Fatalf("testdata/live has %d rollouts, the table has %d", len(files), len(want))
	}
	for name, turns := range want {
		path := liveRollout(t, name)
		for i, wantRow := range turns {
			turnID := fmt.Sprintf("turn-%d", i+1)
			got, ok, err := ReadTokenUsageForTurn(path, turnID, LiveScanOptions())
			row := fmt.Sprintf("in=%d out=%d total=%d", got.TurnUsage.InputTokens, got.TurnUsage.OutputTokens, got.TurnUsage.TotalTokens)
			if !ok {
				row = "none"
			}
			if err != nil || row != wantRow {
				t.Errorf("%s %s: %s err=%v; want %s", name, turnID, row, err, wantRow)
			}
		}
	}
}

// BenchmarkReadTokenUsageForTurnLive reads the last turn of a 30 MiB rollout,
// with and without one oversized line. A skipped line is drained without a
// decode, so it should cost no more than a normal line of the same size.
func BenchmarkReadTokenUsageForTurnLive(b *testing.B) {
	const turns = 1900
	text := strings.Repeat("y", 16*1024)
	build := func(withOversized bool) string {
		var sb strings.Builder
		for i := 1; i <= turns; i++ {
			fmt.Fprintf(&sb, `{"type":"turn_context","payload":{"turn_id":"turn-%d"}}`+"\n", i)
			if withOversized && i == 1 {
				sb.WriteString(`{"type":"response_item","payload":{"type":"message","role":"user","content":"` + strings.Repeat("A", 1100*1024) + `"}}` + "\n")
			}
			fmt.Fprintf(&sb, `{"type":"response_item","payload":{"type":"message","role":"assistant","content":%q}}`+"\n", text)
			fmt.Fprintf(&sb, `{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":%d,"total_tokens":%d}}}}`+"\n", i*10, i*10)
		}
		path := filepath.Join(b.TempDir(), "rollout.jsonl")
		if err := os.WriteFile(path, []byte(sb.String()), 0o600); err != nil {
			b.Fatal(err)
		}
		return path
	}
	for _, withOversized := range []bool{false, true} {
		path := build(withOversized)
		b.Run(fmt.Sprintf("oversized=%v", withOversized), func(b *testing.B) {
			for b.Loop() {
				if _, ok, err := ReadTokenUsageForTurn(path, fmt.Sprintf("turn-%d", turns), LiveScanOptions()); err != nil || !ok {
					b.Fatalf("ok=%v err=%v", ok, err)
				}
			}
		})
	}
}

func writeTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	return path
}

func TestScanRecords(t *testing.T) {
	tests := []struct {
		name      string
		lines     []string
		opts      ScanOptions
		wantTypes []string
		wantErr   bool
	}{
		{
			name: "decodes envelopes and skips blank lines",
			lines: []string{
				`{"timestamp":"2026-01-10T12:00:00Z","type":"session_meta","payload":{"id":"sess-1","cwd":"/work"}}`,
				``,
				`{"timestamp":"2026-01-10T12:00:01Z","type":"turn_context","payload":{"turn_id":"turn-1"}}`,
			},
			wantTypes: []string{"session_meta", "turn_context"},
		},
		{
			name:      "a malformed line fails the scan by default",
			lines:     []string{`{"type":"session_meta"}`, `not json`},
			wantTypes: []string{"session_meta"},
			wantErr:   true,
		},
		{
			name:      "SkipMalformedLines drops the line and keeps scanning",
			lines:     []string{`{"type":"session_meta"}`, `not json`, `{"type":"turn_context"}`},
			opts:      ScanOptions{SkipMalformedLines: true},
			wantTypes: []string{"session_meta", "turn_context"},
		},
		{
			name:      "MaxBytes fails past the budget",
			lines:     []string{`{"type":"session_meta"}`, `{"type":"turn_context"}`},
			opts:      ScanOptions{MaxBytes: 30},
			wantTypes: []string{"session_meta"},
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTranscript(t, tt.lines...)
			var got []string
			err := ScanRecords(path, tt.opts, func(rec Record) (bool, error) {
				got = append(got, rec.Type)
				return false, nil
			})
			if (err != nil) != tt.wantErr {
				t.Fatalf("ScanRecords error = %v, wantErr %v", err, tt.wantErr)
			}
			if len(got) != len(tt.wantTypes) {
				t.Fatalf("record types = %v, want %v", got, tt.wantTypes)
			}
			for i := range got {
				if got[i] != tt.wantTypes[i] {
					t.Fatalf("record types = %v, want %v", got, tt.wantTypes)
				}
			}
		})
	}
}

func TestScanRecordsStopsWhenVisitIsDone(t *testing.T) {
	path := writeTranscript(t,
		`{"type":"session_meta","payload":{"id":"sess-1"}}`,
		`{"type":"turn_context","payload":{"turn_id":"turn-1"}}`,
	)
	seen := 0
	if err := ScanRecords(path, ScanOptions{}, func(Record) (bool, error) {
		seen++
		return true, nil
	}); err != nil {
		t.Fatalf("ScanRecords: %v", err)
	}
	if seen != 1 {
		t.Fatalf("visited %d records, want 1", seen)
	}
}

func TestParseSessionMetaReadsWorkspace(t *testing.T) {
	workspace := SessionMeta{SessionID: "sess-1", Cwd: "/work/repo"}
	tests := []struct {
		name string
		raw  string
		want SessionMeta
		ok   bool
	}{
		{"absent source", `{"id":"sess-1","cwd":"/work/repo"}`, workspace, true},
		{"cli source", `{"id":"sess-1","cwd":"/work/repo","source":"cli"}`, workspace, true},
		{"exec source", `{"id":"sess-1","cwd":"/work/repo","source":"exec"}`, workspace, true},
		{"null source", `{"id":"sess-1","cwd":"/work/repo","source":null}`, workspace, true},
		{"unknown source", `{"id":"sess-1","cwd":"/work/repo","source":{"unknown":true}}`, workspace, true},
		{"numeric source", `{"id":"sess-1","cwd":"/work/repo","source":42}`, workspace, true},
		{"array source", `{"id":"sess-1","cwd":"/work/repo","source":[]}`, workspace, true},
		{"boolean source", `{"id":"sess-1","cwd":"/work/repo","source":true}`, workspace, true},
		{"invalid subagent", `{"id":"sess-1","cwd":"/work/repo","source":{"subagent":"other"}}`, workspace, true},
		{"invalid spawn", `{"id":"sess-1","cwd":"/work/repo","source":{"subagent":{"thread_spawn":[]}}}`, workspace, true},
		{"no partial linkage", `{"id":"sess-1","cwd":"/work/repo","source":{"subagent":{"thread_spawn":{"parent_thread_id":"parent","agent_role":"reviewer","agent_nickname":"scout","depth":"bad"}}}}`, workspace, true},
		{"id alias", `{"session_id":"sess-2"}`, SessionMeta{SessionID: "sess-2"}, true},
		{"id precedence", `{"id":"sess-1","session_id":"sess-2"}`, SessionMeta{SessionID: "sess-1"}, true},
		{"top-level precedence", `{"id":"child","parent_session_id":"parent","parent_thread_id":"alias","agent_role":"top-role","agent_nickname":"top-name","agent_depth":3,"depth":2,"thread_source":"explicit","source":{"subagent":{"thread_spawn":{"parent_thread_id":"nested","agent_role":"nested-role","agent_nickname":"nested-name","depth":1}}}}`, SessionMeta{SessionID: "child", ParentSessionID: "parent", AgentRole: "top-role", AgentNickname: "top-name", AgentDepth: 3, ThreadSource: "explicit"}, true},
		{"parent and depth aliases", `{"parent_thread_id":"alias","depth":2,"source":{"subagent":{"thread_spawn":{"parent_thread_id":"nested","depth":1}}}}`, SessionMeta{ParentSessionID: "alias", AgentDepth: 2, ThreadSource: "subagent"}, true},
		{"nested linkage", `{"source":{"subagent":{"thread_spawn":{"parent_thread_id":"parent","agent_role":"reviewer","agent_nickname":"scout","depth":1}}}}`, SessionMeta{ParentSessionID: "parent", AgentRole: "reviewer", AgentNickname: "scout", AgentDepth: 1, ThreadSource: "subagent"}, true},
		{"explicit thread source only", `{"thread_source":"cli"}`, SessionMeta{ThreadSource: "cli"}, true},
		{"workspace alone is not success", `{"cwd":"/work/repo","source":"cli"}`, SessionMeta{Cwd: "/work/repo"}, false},
		{"empty", `{}`, SessionMeta{}, false},
		{"malformed", `{"id":"sess-1",`, SessionMeta{}, false},
		{"numeric id", `{"id":1,"cwd":"/work/repo"}`, SessionMeta{}, false},
		{"numeric cwd", `{"id":"sess-1","cwd":1}`, SessionMeta{}, false},
		{"invalid outer depth", `{"id":"sess-1","agent_depth":"bad"}`, SessionMeta{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta, ok := ParseSessionMeta([]byte(tt.raw))
			if meta != tt.want || ok != tt.ok {
				t.Fatalf("ParseSessionMeta = %+v, %v; want %+v, %v", meta, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestSubtractUsage(t *testing.T) {
	tests := []struct {
		name     string
		final    TokenUsage
		baseline TokenUsage
		want     TokenUsage
		wantOK   bool
	}{
		{
			name:     "difference of two cumulative totals",
			final:    TokenUsage{InputTokens: 300, OutputTokens: 60, TotalTokens: 360},
			baseline: TokenUsage{InputTokens: 100, OutputTokens: 20, TotalTokens: 120},
			want:     TokenUsage{InputTokens: 200, OutputTokens: 40, TotalTokens: 240},
			wantOK:   true,
		},
		{
			name:   "zero baseline keeps the total",
			final:  TokenUsage{InputTokens: 10, TotalTokens: 10},
			want:   TokenUsage{InputTokens: 10, TotalTokens: 10},
			wantOK: true,
		},
		{
			name:     "a negative component means mismatched snapshots",
			final:    TokenUsage{InputTokens: 10},
			baseline: TokenUsage{InputTokens: 20},
			wantOK:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := SubtractUsage(tt.final, tt.baseline)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Fatalf("usage = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestMessageText(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "plain string", raw: `"hello"`, want: "hello"},
		{name: "typed parts", raw: `[{"type":"input_text","text":"a"},{"type":"output_text","text":"b"}]`, want: "a\nb"},
		{name: "unknown part types are dropped", raw: `[{"type":"image","text":"x"}]`, want: ""},
		{name: "null", raw: `null`, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MessageText([]byte(tt.raw)); got != tt.want {
				t.Fatalf("MessageText(%s) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

// TestLiveScanSkipsATornLine covers the ordinary live case: the hook reads a
// rollout the running Codex process is still appending to, so the last line can
// be half written. That must cost the line, not the scan.
func TestLiveScanSkipsATornLine(t *testing.T) {
	path := writeTranscript(t,
		`{"type":"session_meta","payload":{"id":"sess","thread_source":"cli"}}`,
		`{"type":"turn_context","payload":{"turn_id":"turn-1"}}`,
		`{"type":"event_msg","payload":{"type":"token_count","in`,
	)

	var types []string
	if err := ScanRecords(path, LiveScanOptions(), func(rec Record) (bool, error) {
		types = append(types, rec.Type)
		return false, nil
	}); err != nil {
		t.Fatalf("ScanRecords with the live budget: %v", err)
	}
	if len(types) != 2 {
		t.Fatalf("decoded %v, want the two complete records", types)
	}

	meta, ok, err := ReadSessionMeta(path, LiveScanOptions())
	if err != nil || !ok || meta.SessionID != "sess" {
		t.Fatalf("ReadSessionMeta = %+v ok=%v err=%v; a torn final line cost the whole scan", meta, ok, err)
	}
}

// TestImportScanOptionsHaveNoTotalCap pins the difference between the two
// budgets: the importer must reach the end of a rollout of any size, and
// rollouts past the live cap exist.
func TestImportScanOptionsHaveNoTotalCap(t *testing.T) {
	if got := ImportScanOptions().MaxBytes; got != 0 {
		t.Fatalf("ImportScanOptions().MaxBytes = %d, want no cap", got)
	}
	if got := LiveScanOptions().MaxBytes; got <= 0 {
		t.Fatalf("LiveScanOptions().MaxBytes = %d, want the live cap", got)
	}
	if !LiveScanOptions().SkipMalformedLines || !ImportScanOptions().SkipMalformedLines {
		t.Fatal("both budgets must tolerate a torn line")
	}
}

func TestScanRecordsSkipsOverlongLineWhenLenient(t *testing.T) {
	path := writeTranscript(t,
		`{"type":"session_meta","payload":{"id":"s"}}`,
		`{"type":"blob","payload":"`+strings.Repeat("a", 4096)+`"}`,
		`{"type":"turn_context","payload":{"turn_id":"turn-1"}}`,
	)
	var types []string
	opts := ImportScanOptions()
	opts.MaxLineBytes = 1024
	err := ScanRecords(path, opts, func(rec Record) (bool, error) {
		types = append(types, rec.Type)
		return false, nil
	})
	if err != nil {
		t.Fatalf("ScanRecords: %v", err)
	}
	if strings.Join(types, ",") != "session_meta,turn_context" {
		t.Fatalf("records after the oversized line were lost: %v", types)
	}
}

func TestScanRecordsFailsOnOverlongLineWhenStrict(t *testing.T) {
	path := writeTranscript(t,
		`{"type":"session_meta","payload":{"id":"s"}}`,
		`{"type":"blob","payload":"`+strings.Repeat("a", 4096)+`"}`,
	)
	err := ScanRecords(path, ScanOptions{MaxLineBytes: 1024}, func(Record) (bool, error) { return false, nil })
	if err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("expected a line 2 error, got %v", err)
	}
}
