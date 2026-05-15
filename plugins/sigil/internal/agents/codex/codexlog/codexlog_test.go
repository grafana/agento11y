package codexlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadSessionMetaSubagent(t *testing.T) {
	path := writeTranscript(t, `{"type":"session_meta","payload":{"id":"child","thread_source":"subagent","agent_nickname":"Dalton","agent_role":"reviewer","source":{"subagent":{"thread_spawn":{"parent_thread_id":"parent","depth":1}}}}}`)

	got, ok, err := ReadSessionMeta(path)
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

	got, ok, err := ReadSessionMeta(path)
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

	got, ok, err := ResolveSpawnLink(path, "child", func(sessionID, turnID string) string {
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

	got, ok, err := ResolveSpawnLink(path, "child", func(sessionID, turnID string) string {
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

	_, ok, err := ResolveSpawnLink(path, "child", nil)
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

	_, ok, err := ResolveSpawnLink(path, "child", nil)
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

	got, ok, err := ReadTokenUsageForTurn(path, "turn-2")
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

	got, ok, err := ReadTokenUsageForTurn(path, "turn-1")
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

	got, ok, err := ReadTokenUsageForTurn(path, "turn-1")
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

	_, ok, err := ReadTokenUsageForTurn(path, "turn-1")
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

	_, ok, err := ReadTokenUsageForTurn(path, "turn-2")
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

	_, ok, err := ReadTokenUsageForTurn(path, "turn-2")
	if err != nil {
		t.Fatalf("ReadTokenUsageForTurn: %v", err)
	}
	if ok {
		t.Fatal("expected no usage for negative cumulative delta")
	}
}

func TestReadSessionMetaRejectsOversizedLine(t *testing.T) {
	path := writeTranscript(t, strings.Repeat("x", maxLineBytes+1))

	_, _, err := ReadSessionMeta(path)
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
