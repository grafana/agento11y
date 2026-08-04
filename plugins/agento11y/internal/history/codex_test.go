package history

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/codex/codexlog"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/codex/fragment"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/codex/mapper"
)

func codexRecord(ts, typ string, payload map[string]any) string {
	data, err := json.Marshal(map[string]any{"timestamp": ts, "type": typ, "payload": payload})
	if err != nil {
		panic(err)
	}
	return string(data) + "\n"
}

// writeCodexRollout writes a rollout with one prompt, one tool call and one
// assistant reply, closed by a token_count.
func writeCodexRollout(t *testing.T, root, sessionID string) string {
	t.Helper()
	path := filepath.Join(root, "2026", "01", "10", "rollout-2026-01-10T12-00-00-"+sessionID+".jsonl")
	body := codexRecord("2026-01-10T12:00:00Z", "session_meta", map[string]any{"id": sessionID, "cwd": "/work/repo"}) +
		codexRecord("2026-01-10T12:00:01Z", "turn_context", map[string]any{"turn_id": "turn-1", "cwd": "/work/repo", "model": "gpt-5.5"}) +
		codexRecord("2026-01-10T12:00:02Z", "event_msg", map[string]any{"type": "user_message", "message": "run the tests"}) +
		codexRecord("2026-01-10T12:00:03Z", "response_item", map[string]any{
			"type": "function_call", "name": "shell", "call_id": "call-1",
			"arguments": `{"command":"go test ./..."}`,
		}) +
		codexRecord("2026-01-10T12:00:04Z", "response_item", map[string]any{
			"type": "function_call_output", "call_id": "call-1",
			"output": `{"exit_code":0,"stdout":"ok"}`,
		}) +
		codexRecord("2026-01-10T12:00:05Z", "response_item", map[string]any{
			"type": "message", "role": "assistant",
			"content": []map[string]any{{"type": "output_text", "text": "The tests pass."}},
		}) +
		codexRecord("2026-01-10T12:00:06Z", "event_msg", map[string]any{
			"type": "token_count",
			"info": map[string]any{
				"total_token_usage": map[string]any{"input_tokens": 100, "output_tokens": 20, "total_tokens": 120},
				"last_token_usage":  map[string]any{"input_tokens": 100, "output_tokens": 20, "total_tokens": 120},
			},
		})
	return writeFile(t, path, body)
}

func codexImporterAt(root string) *codexImporter {
	return &codexImporter{
		roots: []string{root},
		now:   func() time.Time { return time.Date(2026, 1, 10, 13, 0, 0, 0, time.UTC) },
	}
}

func codexPreview(t *testing.T, imp *codexImporter, path string) SessionPreview {
	t.Helper()
	preview, ok, err := imp.Preview(context.Background(), path)
	if err != nil || !ok {
		t.Fatalf("Preview: ok=%v err=%v", ok, err)
	}
	return preview
}

func TestCodexMatch(t *testing.T) {
	imp := &codexImporter{}
	tests := []struct {
		path string
		want bool
	}{
		{"/s/2026/01/10/rollout-2026-01-10T12-00-00-abc.jsonl", true},
		{"/s/2026/01/10/notes.jsonl", false},
		{"/s/2026/01/10/rollout-2026-01-10T12-00-00-abc.json", false},
	}
	for _, tt := range tests {
		if got := imp.Match(tt.path); got != tt.want {
			t.Errorf("Match(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestCodexPreviewIsMetadataOnly(t *testing.T) {
	root := t.TempDir()
	path := writeCodexRollout(t, root, "019a4a49-979a-7382-abb5-a9a4e8740114")

	preview := codexPreview(t, codexImporterAt(root), path)
	if preview.SessionID != "019a4a49-979a-7382-abb5-a9a4e8740114" {
		t.Fatalf("SessionID = %q", preview.SessionID)
	}
	if preview.Title != preview.SessionID {
		t.Fatalf("Title = %q, want the session ID: a preview must not carry prompt text", preview.Title)
	}
	if preview.Workspace != "/work/repo" {
		t.Fatalf("Workspace = %q, want /work/repo", preview.Workspace)
	}
	if preview.TurnCount != 1 || preview.ApproxTurns {
		t.Fatalf("TurnCount = %d approx=%v, want an exact 1", preview.TurnCount, preview.ApproxTurns)
	}
	if !preview.StartedAt.Equal(time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("StartedAt = %s", preview.StartedAt)
	}
	if !preview.LastActivityAt.Equal(time.Date(2026, 1, 10, 12, 0, 6, 0, time.UTC)) {
		t.Fatalf("LastActivityAt = %s", preview.LastActivityAt)
	}
}

func TestCodexPreviewFallsBackToTheFilenameSessionID(t *testing.T) {
	root := t.TempDir()
	id := "019a4a49-979a-7382-abb5-a9a4e8740114"
	path := writeFile(t,
		filepath.Join(root, "rollout-2026-01-10T12-00-00-"+id+".jsonl"),
		codexRecord("2026-01-10T12:00:00Z", "turn_context", map[string]any{"turn_id": "turn-1"}),
	)
	if got := codexPreview(t, codexImporterAt(root), path).SessionID; got != id {
		t.Fatalf("SessionID = %q, want %q from the filename", got, id)
	}
}

func TestCodexTurnsMapThroughTheLiveMapper(t *testing.T) {
	root := t.TempDir()
	path := writeCodexRollout(t, root, "019a4a49-979a-7382-abb5-a9a4e8740114")
	imp := codexImporterAt(root)

	turns := collectTurns(t, imp, codexPreview(t, imp, path))
	if len(turns) != 1 {
		t.Fatalf("got %d turns, want 1", len(turns))
	}
	turn := turns[0]
	if turn.Gen.AgentName != "codex" {
		t.Fatalf("AgentName = %q, want codex", turn.Gen.AgentName)
	}
	if turn.Gen.Model.Name != "gpt-5.5" {
		t.Fatalf("Model = %+v, want gpt-5.5", turn.Gen.Model)
	}
	if turn.Gen.ConversationTitle != "run the tests" {
		t.Fatalf("ConversationTitle = %q, want the first prompt", turn.Gen.ConversationTitle)
	}
	if turn.Gen.Usage.InputTokens != 100 || turn.Gen.Usage.OutputTokens != 20 {
		t.Fatalf("Usage = %+v, want the token_count snapshot", turn.Gen.Usage)
	}
	if turn.Quality.ApproxUsage {
		t.Error("ApproxUsage = true, but the rollout recorded a token count")
	}
	if turn.Source.TurnID != "turn-1" {
		t.Fatalf("TurnID = %q, want turn-1", turn.Source.TurnID)
	}
	if turn.Gen.ID != turn.Source.GenerationID() {
		t.Fatalf("generation ID %q is not the deterministic source ID %q", turn.Gen.ID, turn.Source.GenerationID())
	}

	var toolNames []string
	for _, msg := range turn.Gen.Output {
		for _, part := range msg.Parts {
			if part.ToolCall != nil {
				toolNames = append(toolNames, part.ToolCall.Name)
			}
		}
	}
	if !slices.Equal(toolNames, []string{"shell"}) {
		t.Fatalf("tool calls = %v, want [shell]", toolNames)
	}

	// The same fragment through mapper.Map directly, the path the live hook
	// runs. An imported turn must be that output, not a second mapping.
	want := mapper.Map(mapper.Inputs{
		Fragment: &fragment.Fragment{
			SessionID:            "019a4a49-979a-7382-abb5-a9a4e8740114",
			TurnID:               "turn-1",
			Cwd:                  "/work/repo",
			Source:               "history",
			Model:                "gpt-5.5",
			Prompt:               "run the tests",
			TranscriptPath:       path,
			StartedAt:            "2026-01-10T12:00:01Z",
			LastEventAt:          "2026-01-10T12:00:06Z",
			CompletedAt:          "2026-01-10T12:00:06Z",
			LastAssistantMessage: "The tests pass.",
			Tools: []fragment.ToolRecord{{
				ToolName:     "shell",
				ToolUseID:    "call-1",
				ToolInput:    json.RawMessage(`{"command":"go test ./..."}`),
				ToolResponse: json.RawMessage(`{"exit_code":0,"stdout":"ok"}`),
				Status:       "completed",
				StartedAt:    "2026-01-10T12:00:03Z",
				CompletedAt:  "2026-01-10T12:00:04Z",
			}},
		},
		TokenSnapshot: &codexlog.TokenSnapshot{
			TurnID:     "turn-1",
			TurnUsage:  codexlog.TokenUsage{InputTokens: 100, OutputTokens: 20, TotalTokens: 120},
			LastUsage:  codexlog.TokenUsage{InputTokens: 100, OutputTokens: 20, TotalTokens: 120},
			TotalUsage: codexlog.TokenUsage{InputTokens: 100, OutputTokens: 20, TotalTokens: 120},
			Source:     "token_count_delta",
		},
		ContentCapture: agento11y.ContentCaptureModeFull,
		RawContent:     true,
		Now:            time.Date(2026, 1, 10, 13, 0, 0, 0, time.UTC),
	}).Generation
	// The importer stamps its own deterministic ID and the conversation title;
	// everything else must be the mapper's output.
	want.ID = turn.Gen.ID
	want.ConversationTitle = turn.Gen.ConversationTitle
	want.ResponseID = turn.Gen.ResponseID
	if !reflect.DeepEqual(turn.Gen, want) {
		t.Fatalf("the imported turn differs from mapper.Map:\n got %+v\nwant %+v", turn.Gen, want)
	}
}

func TestCodexTurnsAreDeterministic(t *testing.T) {
	root := t.TempDir()
	path := writeCodexRollout(t, root, "019a4a49-979a-7382-abb5-a9a4e8740114")
	imp := codexImporterAt(root)
	preview := codexPreview(t, imp, path)

	first := collectTurns(t, imp, preview)
	second := collectTurns(t, codexImporterAt(root), preview)
	if len(first) != len(second) {
		t.Fatalf("turn counts differ: %d then %d", len(first), len(second))
	}
	for i := range first {
		if a, b := first[i].Gen.ID, second[i].Gen.ID; a != b {
			t.Fatalf("turn %d generation ID changed between reads: %q then %q", i, a, b)
		}
	}
}

// TestCodexTurnsSplitOnTokenSnapshots covers a long turn Codex reports in
// steps. Collapsing the steps into one generation would report their combined
// usage as a single call.
func TestCodexTurnsSplitOnTokenSnapshots(t *testing.T) {
	root := t.TempDir()
	tokenCount := func(ts string, input, output int) string {
		return codexRecord(ts, "event_msg", map[string]any{
			"type": "token_count",
			"info": map[string]any{
				"total_token_usage": map[string]any{
					"input_tokens": input, "output_tokens": output, "total_tokens": input + output,
				},
			},
		})
	}
	body := codexRecord("2026-01-10T12:00:00Z", "session_meta", map[string]any{"id": "sess-split", "cwd": "/work/repo"}) +
		codexRecord("2026-01-10T12:00:01Z", "turn_context", map[string]any{"turn_id": "turn-1", "model": "gpt-5.5"}) +
		codexRecord("2026-01-10T12:00:02Z", "event_msg", map[string]any{"type": "user_message", "message": "long job"}) +
		codexRecord("2026-01-10T12:00:03Z", "response_item", map[string]any{
			"type": "message", "role": "assistant",
			"content": []map[string]any{{"type": "output_text", "text": "step one"}},
		}) +
		tokenCount("2026-01-10T12:00:04Z", 100, 20) +
		codexRecord("2026-01-10T12:00:05Z", "response_item", map[string]any{
			"type": "message", "role": "assistant",
			"content": []map[string]any{{"type": "output_text", "text": "step two"}},
		}) +
		tokenCount("2026-01-10T12:00:06Z", 250, 45)
	path := writeFile(t, filepath.Join(root, "rollout-2026-01-10T12-00-00-sess-split.jsonl"), body)

	imp := codexImporterAt(root)
	turns := collectTurns(t, imp, codexPreview(t, imp, path))
	if len(turns) != 2 {
		t.Fatalf("got %d turns, want 2 segments", len(turns))
	}
	if turns[0].Source.TurnID != "turn-1" {
		t.Fatalf("first segment TurnID = %q, want turn-1", turns[0].Source.TurnID)
	}
	if turns[1].Source.TurnID != "turn-1:step-000001" {
		t.Fatalf("second segment TurnID = %q, want the step suffix", turns[1].Source.TurnID)
	}
	if turns[0].Gen.Usage.TotalTokens != 120 {
		t.Fatalf("first segment usage = %+v, want 120 total", turns[0].Gen.Usage)
	}
	// The second segment's usage is the delta against the first snapshot, not
	// the running total.
	if turns[1].Gen.Usage.TotalTokens != 175 {
		t.Fatalf("second segment usage = %+v, want the 175 token delta", turns[1].Gen.Usage)
	}
	if turns[0].Gen.ID == turns[1].Gen.ID {
		t.Fatal("both segments got the same generation ID")
	}
}

func TestCodexTurnWithoutTokenUsageIsMarkedApproximate(t *testing.T) {
	root := t.TempDir()
	body := codexRecord("2026-01-10T12:00:00Z", "session_meta", map[string]any{"id": "sess-notoken"}) +
		codexRecord("2026-01-10T12:00:01Z", "turn_context", map[string]any{"turn_id": "turn-1", "model": "gpt-5.5"}) +
		codexRecord("2026-01-10T12:00:02Z", "event_msg", map[string]any{"type": "user_message", "message": "hello"}) +
		codexRecord("2026-01-10T12:00:03Z", "response_item", map[string]any{
			"type": "message", "role": "assistant",
			"content": []map[string]any{{"type": "output_text", "text": "hi"}},
		})
	path := writeFile(t, filepath.Join(root, "rollout-2026-01-10T12-00-00-sess-notoken.jsonl"), body)

	imp := codexImporterAt(root)
	turns := collectTurns(t, imp, codexPreview(t, imp, path))
	if len(turns) != 1 {
		t.Fatalf("got %d turns, want 1", len(turns))
	}
	if !turns[0].Quality.ApproxUsage {
		t.Error("ApproxUsage = false, want true for a turn with no token count")
	}
	if turns[0].Gen.Usage.TotalTokens != 0 {
		t.Fatalf("Usage = %+v, want zero", turns[0].Gen.Usage)
	}
	// The turn ended without task_complete, so its completion time is the last
	// event rather than a recorded one.
	if !turns[0].Quality.ApproxCompletedAt {
		t.Error("ApproxCompletedAt = false, want true when no completion was recorded")
	}
}

// TestCodexSubagentTurnsLinkToTheParentTurn covers a spawned agent session. The
// parent generation ID must be the deterministic import ID of the parent's
// spawning turn, so the two sessions join up in the viewer.
func TestCodexSubagentTurnsLinkToTheParentTurn(t *testing.T) {
	root := t.TempDir()
	parentID := "019a4a49-979a-7382-abb5-a9a4e8740114"
	childID := "019a4a50-1111-7382-abb5-a9a4e8740115"

	parentBody := codexRecord("2026-01-10T12:00:00Z", "session_meta", map[string]any{"id": parentID, "cwd": "/work/repo"}) +
		codexRecord("2026-01-10T12:00:01Z", "turn_context", map[string]any{"turn_id": "turn-1", "model": "gpt-5.5"}) +
		codexRecord("2026-01-10T12:00:02Z", "event_msg", map[string]any{"type": "user_message", "message": "delegate this"}) +
		codexRecord("2026-01-10T12:00:03Z", "response_item", map[string]any{
			"type": "function_call", "name": "spawn_agent", "call_id": "call-spawn",
			"arguments": `{"task":"explore"}`,
		}) +
		codexRecord("2026-01-10T12:00:04Z", "response_item", map[string]any{
			"type": "function_call_output", "call_id": "call-spawn",
			"output": fmt.Sprintf(`{"agent_id":%q,"nickname":"scout"}`, childID),
		})
	parentPath := writeFile(t, filepath.Join(root, "rollout-2026-01-10T12-00-00-"+parentID+".jsonl"), parentBody)

	childBody := codexRecord("2026-01-10T12:00:05Z", "session_meta", map[string]any{
		"id": childID, "thread_source": "subagent", "parent_session_id": parentID,
		"agent_role": "explorer", "agent_nickname": "scout", "cwd": "/work/repo",
	}) +
		codexRecord("2026-01-10T12:00:06Z", "turn_context", map[string]any{"turn_id": "turn-c1", "model": "gpt-5.5"}) +
		codexRecord("2026-01-10T12:00:07Z", "response_item", map[string]any{
			"type": "message", "role": "assistant",
			"content": []map[string]any{{"type": "output_text", "text": "explored"}},
		})
	childPath := writeFile(t, filepath.Join(root, "rollout-2026-01-10T12-00-05-"+childID+".jsonl"), childBody)

	imp := codexImporterAt(root)
	parentTurns := collectTurns(t, imp, codexPreview(t, imp, parentPath))
	if len(parentTurns) != 1 {
		t.Fatalf("got %d parent turns, want 1", len(parentTurns))
	}
	childTurns := collectTurns(t, imp, codexPreview(t, imp, childPath))
	if len(childTurns) != 1 {
		t.Fatalf("got %d child turns, want 1", len(childTurns))
	}
	child := childTurns[0]
	if child.Gen.AgentName != "codex/subagent" {
		t.Fatalf("AgentName = %q, want codex/subagent", child.Gen.AgentName)
	}
	want := []string{parentTurns[0].Gen.ID}
	if !slices.Equal(child.Gen.ParentGenerationIDs, want) {
		t.Fatalf("ParentGenerationIDs = %v, want %v", child.Gen.ParentGenerationIDs, want)
	}
	if child.Gen.ConversationID != parentID {
		t.Fatalf("ConversationID = %q, want the parent session %q", child.Gen.ConversationID, parentID)
	}
	if child.Gen.Metadata["codex.spawn_call_id"] != "call-spawn" {
		t.Fatalf("metadata = %+v, want the spawn call ID", child.Gen.Metadata)
	}
}

func TestCodexTurnsStopWhenTheConsumerStops(t *testing.T) {
	root := t.TempDir()
	var body strings.Builder
	body.WriteString(codexRecord("2026-01-10T12:00:00Z", "session_meta", map[string]any{"id": "sess-many"}))
	for i := range 20 {
		ts := time.Date(2026, 1, 10, 12, 0, i, 0, time.UTC).Format(time.RFC3339)
		body.WriteString(codexRecord(ts, "turn_context", map[string]any{"turn_id": fmt.Sprintf("turn-%d", i), "model": "gpt-5.5"}))
		body.WriteString(codexRecord(ts, "response_item", map[string]any{
			"type": "message", "role": "assistant",
			"content": []map[string]any{{"type": "output_text", "text": "reply"}},
		}))
	}
	path := writeFile(t, filepath.Join(root, "rollout-2026-01-10T12-00-00-sess-many.jsonl"), body.String())

	imp := codexImporterAt(root)
	seen := 0
	for range imp.Turns(context.Background(), codexPreview(t, imp, path)) {
		seen++
		if seen == 3 {
			break
		}
	}
	if seen != 3 {
		t.Fatalf("consumed %d turns, want 3 before breaking", seen)
	}
}

func TestCodexTurnsHonourCancellation(t *testing.T) {
	root := t.TempDir()
	path := writeCodexRollout(t, root, "019a4a49-979a-7382-abb5-a9a4e8740114")
	imp := codexImporterAt(root)
	preview := codexPreview(t, imp, path)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var gotErr error
	for _, err := range imp.Turns(ctx, preview) {
		gotErr = err
		break
	}
	if gotErr == nil {
		t.Fatal("Turns yielded no error for a cancelled context")
	}
}

func TestCodexRawContentIsSanitizedOnceByTheFramework(t *testing.T) {
	root := t.TempDir()
	secret := "glc_abcdefghijklmnopqrstuvwx"
	body := codexRecord("2026-01-10T12:00:00Z", "session_meta", map[string]any{"id": "sess-secret"}) +
		codexRecord("2026-01-10T12:00:01Z", "turn_context", map[string]any{"turn_id": "turn-1", "model": "gpt-5.5"}) +
		codexRecord("2026-01-10T12:00:02Z", "event_msg", map[string]any{"type": "user_message", "message": "my token is " + secret}) +
		codexRecord("2026-01-10T12:00:03Z", "response_item", map[string]any{
			"type": "message", "role": "assistant",
			"content": []map[string]any{{"type": "output_text", "text": "saw " + secret}},
		})
	path := writeFile(t, filepath.Join(root, "rollout-2026-01-10T12-00-00-sess-secret.jsonl"), body)

	imp := codexImporterAt(root)
	turns := collectTurns(t, imp, codexPreview(t, imp, path))
	if len(turns) != 1 {
		t.Fatalf("got %d turns, want 1", len(turns))
	}
	raw := turns[0].Gen.Input[0].Parts[0].Text
	if !strings.Contains(raw, secret) {
		t.Fatalf("the importer redacted content itself: %q", raw)
	}

	turn := turns[0]
	Sanitizer{}.Sanitize(&turn)
	cleaned := turn.Gen.Input[0].Parts[0].Text + turn.Gen.Output[0].Parts[0].Text
	if strings.Contains(cleaned, secret) {
		t.Fatalf("the framework Sanitizer left the secret in place: %q", cleaned)
	}
	if strings.Count(cleaned, "[REDACTED:grafana-cloud-token]") != 2 {
		t.Fatalf("want exactly one redaction marker per field: %q", cleaned)
	}
}

// TestCodexToolOutputAfterATokenCount covers the order Codex actually writes:
// function_call, then token_count, then function_call_output. A segment that
// closes on the token_count leaves the call in one generation and its output in
// the next, where no call matches it.
func TestCodexToolOutputAfterATokenCount(t *testing.T) {
	root := t.TempDir()
	sessionID := "019a4a49-979a-7382-abb5-a9a4e8740114"
	usage := func(total int) map[string]any {
		return map[string]any{
			"type": "token_count",
			"info": map[string]any{
				"total_token_usage": map[string]any{"input_tokens": total, "output_tokens": total / 5, "total_tokens": total},
				"last_token_usage":  map[string]any{"input_tokens": total, "output_tokens": total / 5, "total_tokens": total},
			},
		}
	}
	body := codexRecord("2026-01-10T12:00:00Z", "session_meta", map[string]any{"id": sessionID, "cwd": "/work/repo"}) +
		codexRecord("2026-01-10T12:00:01Z", "turn_context", map[string]any{"turn_id": "turn-1", "cwd": "/work/repo", "model": "gpt-5.5"}) +
		codexRecord("2026-01-10T12:00:02Z", "event_msg", map[string]any{"type": "user_message", "message": "run the tests"}) +
		codexRecord("2026-01-10T12:00:03Z", "response_item", map[string]any{"type": "reasoning"}) +
		codexRecord("2026-01-10T12:00:04Z", "response_item", map[string]any{
			"type": "function_call", "name": "shell", "call_id": "call-1",
			"arguments": `{"command":"go test ./..."}`,
		}) +
		codexRecord("2026-01-10T12:00:05Z", "event_msg", usage(100)) +
		codexRecord("2026-01-10T12:00:06Z", "response_item", map[string]any{
			"type": "function_call_output", "call_id": "call-1",
			"output": `{"exit_code":0,"stdout":"ok"}`,
		}) +
		codexRecord("2026-01-10T12:00:07Z", "response_item", map[string]any{
			"type": "message", "role": "assistant",
			"content": []map[string]any{{"type": "output_text", "text": "The tests pass."}},
		}) +
		codexRecord("2026-01-10T12:00:08Z", "event_msg", usage(180))
	path := writeFile(t, filepath.Join(root, "rollout-2026-01-10T12-00-00-"+sessionID+".jsonl"), body)

	imp := codexImporterAt(root)
	preview := codexPreview(t, imp, path)
	turns := collectTurns(t, imp, preview)
	if len(turns) != 2 {
		t.Fatalf("got %d turns, want 2 (the tool step and the answer)", len(turns))
	}

	var calls, results int
	for _, turn := range turns {
		for _, msg := range turn.Gen.Output {
			for _, part := range msg.Parts {
				if part.ToolCall == nil {
					continue
				}
				calls++
				if part.ToolCall.Name != "shell" {
					t.Fatalf("tool call name = %q, want the name from the source", part.ToolCall.Name)
				}
			}
		}
		for _, msg := range turn.Gen.Input {
			for _, part := range msg.Parts {
				if part.ToolResult == nil {
					continue
				}
				results++
				if len(part.ToolResult.ContentJSON) == 0 {
					t.Fatalf("tool result for %q has no content", part.ToolResult.ToolCallID)
				}
				if part.ToolResult.Name != "shell" {
					t.Fatalf("tool result name = %q, want the name from the source", part.ToolResult.Name)
				}
			}
		}
	}
	if calls != 1 || results != 1 {
		t.Fatalf("got %d tool calls and %d results, want one of each: the call and its output belong to one turn", calls, results)
	}
	// The preview counts the same segments the import exports.
	if preview.TurnCount != len(turns) {
		t.Fatalf("preview said %d turns, the import produced %d", preview.TurnCount, len(turns))
	}
}

// TestCodexPreviewMatchesTheImportedTurnCount pins that the preview, the
// import, and the parent-generation index agree about where a turn ends,
// because all three run the same segmenter.
func TestCodexPreviewMatchesTheImportedTurnCount(t *testing.T) {
	root := t.TempDir()
	path := writeCodexRollout(t, root, "019a4a49-979a-7382-abb5-a9a4e8740115")
	imp := codexImporterAt(root)
	preview := codexPreview(t, imp, path)
	turns := collectTurns(t, imp, preview)
	if preview.TurnCount != len(turns) {
		t.Fatalf("preview said %d turns, the import produced %d", preview.TurnCount, len(turns))
	}
}
