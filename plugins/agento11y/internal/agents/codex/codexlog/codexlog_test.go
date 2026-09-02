package codexlog

import (
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

func TestReadSessionMetaRejectsOversizedLine(t *testing.T) {
	path := writeTranscript(t, strings.Repeat("x", liveMaxLineBytes+1))

	_, _, err := ReadSessionMeta(path, LiveScanOptions())
	if err == nil {
		t.Fatal("expected oversized line error")
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
	meta, ok := ParseSessionMeta([]byte(`{"id":"sess-1","cwd":"/work/repo"}`))
	if !ok {
		t.Fatal("ParseSessionMeta reported no metadata")
	}
	if meta.SessionID != "sess-1" || meta.Cwd != "/work/repo" {
		t.Fatalf("meta = %+v, want sess-1 at /work/repo", meta)
	}
	if meta, ok := ParseSessionMeta([]byte(`{"session_id":"sess-2"}`)); !ok || meta.SessionID != "sess-2" {
		t.Fatalf("meta = %+v ok=%v, want the session_id spelling to resolve", meta, ok)
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
