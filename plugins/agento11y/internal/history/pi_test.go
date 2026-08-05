package history

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
)

// piLine marshals one session log line.
func piLine(t *testing.T, entry map[string]any) string {
	t.Helper()
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal pi line: %v", err)
	}
	return string(data) + "\n"
}

func piHeader(t *testing.T, sessionID, cwd, ts string) string {
	t.Helper()
	return piLine(t, map[string]any{
		"type": "session", "version": 3, "id": sessionID, "timestamp": ts, "cwd": cwd,
	})
}

func piUserEntry(t *testing.T, id, parentID, ts, text string, msTS int64) string {
	t.Helper()
	return piLine(t, map[string]any{
		"type": "message", "id": id, "parentId": piParentValue(parentID), "timestamp": ts,
		"message": map[string]any{
			"role":      "user",
			"content":   []map[string]any{{"type": "text", "text": text}},
			"timestamp": msTS,
		},
	})
}

// piAssistantEntry writes an assistant entry with the fields every real one
// carries. content is passed through so a test can add thinking or tool calls.
func piAssistantEntry(t *testing.T, id, parentID, ts string, msTS int64, content []map[string]any, extra map[string]any) string {
	t.Helper()
	msg := map[string]any{
		"role":       "assistant",
		"content":    content,
		"api":        "anthropic-messages",
		"provider":   "anthropic",
		"model":      "claude-opus-4-8",
		"responseId": "msg_" + id,
		"stopReason": "stop",
		"timestamp":  msTS,
		"usage": map[string]any{
			"input": 12, "output": 34, "cacheRead": 5, "cacheWrite": 6, "totalTokens": 57,
			"cost": map[string]any{"input": 0.001, "output": 0.002, "cacheRead": 0.0, "cacheWrite": 0.003, "total": 0.006},
		},
	}
	for k, v := range extra {
		if v == nil {
			delete(msg, k)
			continue
		}
		msg[k] = v
	}
	return piLine(t, map[string]any{
		"type": "message", "id": id, "parentId": piParentValue(parentID), "timestamp": ts, "message": msg,
	})
}

func piToolResultEntry(t *testing.T, id, parentID, ts, callID, toolName, text string, isError bool) string {
	t.Helper()
	return piLine(t, map[string]any{
		"type": "message", "id": id, "parentId": piParentValue(parentID), "timestamp": ts,
		"message": map[string]any{
			"role": "toolResult", "toolCallId": callID, "toolName": toolName,
			"content": []map[string]any{{"type": "text", "text": text}},
			"isError": isError, "timestamp": 1781019440000,
		},
	})
}

func piParentValue(parentID string) any {
	if parentID == "" {
		return nil
	}
	return parentID
}

func piTextBlock(text string) map[string]any {
	return map[string]any{"type": "text", "text": text}
}

func piThinkingBlock(text string) map[string]any {
	return map[string]any{"type": "thinking", "thinking": text, "thinkingSignature": "sig"}
}

func piToolCallBlock(id, name string, args map[string]any) map[string]any {
	return map[string]any{"type": "toolCall", "id": id, "name": name, "arguments": args}
}

// writePiSession writes a two-turn session: a prompt with a tool-calling turn
// and its result, then a second prompt with a plain answer.
func writePiSession(t *testing.T, root, encodedCwd, sessionID string) string {
	t.Helper()
	path := filepath.Join(root, encodedCwd, "2026-06-09T15-37-10-848Z_"+sessionID+".jsonl")
	body := piHeader(t, sessionID, "/work/repo", "2026-06-09T15:37:10.848Z") +
		piUserEntry(t, "u1", "", "2026-06-09T15:37:20.000Z", "review the last commit", 1781019439000) +
		piAssistantEntry(t, "a1", "u1", "2026-06-09T15:37:24.526Z", 1781019441000,
			[]map[string]any{
				piThinkingBlock("I should read the log first."),
				piTextBlock("Let me gather context."),
				piToolCallBlock("toolu_1", "bash", map[string]any{"command": "git log -1"}),
			}, map[string]any{"stopReason": "toolUse"}) +
		piToolResultEntry(t, "r1", "a1", "2026-06-09T15:37:25.100Z", "toolu_1", "bash", "commit abc", false) +
		piUserEntry(t, "u2", "r1", "2026-06-09T15:38:00.000Z", "and the tests?", 1781019480000) +
		piAssistantEntry(t, "a2", "u2", "2026-06-09T15:38:06.000Z", 1781019482000,
			[]map[string]any{piTextBlock("They pass.")}, nil)
	return writeFile(t, path, body)
}

func piImporterAt(root string) *piImporter {
	return &piImporter{roots: []string{root}}
}

func piPreview(t *testing.T, imp *piImporter, path string) SessionPreview {
	t.Helper()
	preview, ok, err := imp.Preview(context.Background(), path)
	if err != nil || !ok {
		t.Fatalf("Preview: ok=%v err=%v", ok, err)
	}
	return preview
}

func TestPiRegistration(t *testing.T) {
	spec, ok := Spec(AgentPi)
	if !ok {
		t.Fatal("no spec registered for pi")
	}
	if spec.DisplayName != "pi" {
		t.Fatalf("DisplayName = %q, want pi", spec.DisplayName)
	}
	for _, name := range []string{"pi", "PI", "pi-coding-agent"} {
		if got, ok := Resolve(name); !ok || got != AgentPi {
			t.Fatalf("Resolve(%q) = %q ok=%v, want pi", name, got, ok)
		}
	}
	if _, ok := NewImporter(AgentPi); !ok {
		t.Fatal("NewImporter(pi) reported no importer")
	}
}

func TestPiRootsResolveFromTheAgentDir(t *testing.T) {
	t.Setenv("PI_CODING_AGENT_DIR", "/tmp/pi-agent")
	imp := &piImporter{}
	if got, want := imp.Roots(), []string{filepath.Join("/tmp/pi-agent", "sessions")}; !slices.Equal(got, want) {
		t.Fatalf("Roots() = %v, want %v", got, want)
	}

	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv("HOME", "/home/tester")
	want := []string{filepath.Join("/home/tester", ".pi", "agent", "sessions")}
	if got := imp.Roots(); !slices.Equal(got, want) {
		t.Fatalf("Roots() = %v, want the home fallback %v", got, want)
	}
}

func TestPiMatch(t *testing.T) {
	imp := &piImporter{}
	tests := []struct {
		name string
		path string
		want bool
	}{
		{"session log", "/s/--work-repo--/2026-06-09T15-37-10-848Z_019ead07.jsonl", true},
		{"not jsonl", "/s/--work-repo--/2026-06-09T15-37-10-848Z_019ead07.json", false},
		{"subagent run", "/s/--work-repo--/2026-06-09T15-37-10-848Z_019ead07/43cec431/run-0/session.jsonl", false},
		{"older subagent run", "/s/--work-repo--/2026-06-09T15-37-10-848Z_019ead07/43cec431/run-4/2026-06-09T15-40-00-000Z_x.jsonl", false},
		{"artifact dump", "/s/--work-repo--/subagent-artifacts/3baf6dad_reviewer_0_output.jsonl", false},
		{"session.jsonl anywhere", "/s/--work-repo--/session.jsonl", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := imp.Match(tt.path); got != tt.want {
				t.Fatalf("Match(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestPiPreviewIsMetadataOnly(t *testing.T) {
	root := t.TempDir()
	sessionID := "019ead07-cfbf-78d3-8b03-875769426583"
	path := writePiSession(t, root, "--work-repo--", sessionID)

	preview := piPreview(t, piImporterAt(root), path)
	if preview.SessionID != sessionID {
		t.Fatalf("SessionID = %q, want %q", preview.SessionID, sessionID)
	}
	if preview.Workspace != "/work/repo" {
		t.Fatalf("Workspace = %q, want /work/repo", preview.Workspace)
	}
	if preview.Title != sessionID {
		t.Fatalf("Title = %q, want the session ID: a preview must not carry prompt text", preview.Title)
	}
	if preview.TurnCount != 2 || preview.ApproxTurns {
		t.Fatalf("TurnCount = %d approx=%v, want an exact 2", preview.TurnCount, preview.ApproxTurns)
	}
	if want := time.Date(2026, 6, 9, 15, 37, 10, 848000000, time.UTC); !preview.StartedAt.Equal(want) {
		t.Fatalf("StartedAt = %s, want %s", preview.StartedAt, want)
	}
	if want := time.Date(2026, 6, 9, 15, 38, 6, 0, time.UTC); !preview.LastActivityAt.Equal(want) {
		t.Fatalf("LastActivityAt = %s, want %s", preview.LastActivityAt, want)
	}
	// Nothing the session said may appear in the preview.
	rendered := preview.Title + preview.SessionID + preview.Workspace
	for _, content := range []string{"review the last commit", "They pass.", "read the log first", "git log -1", "commit abc"} {
		if strings.Contains(rendered, content) {
			t.Fatalf("preview leaked session content %q", content)
		}
	}
}

func TestPiPreviewTitleIsTheSessionName(t *testing.T) {
	root := t.TempDir()
	sessionID := "019ead07-cfbf-78d3-8b03-875769426583"
	path := writePiSession(t, root, "--work-repo--", sessionID)
	body, err := readFileString(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	body += piLine(t, map[string]any{
		"type": "session_info", "id": "s1", "parentId": "a2",
		"timestamp": "2026-06-09T15:39:00.000Z", "name": "Commit review",
	})
	path = writeFile(t, path, body)

	if got := piPreview(t, piImporterAt(root), path).Title; got != "Commit review" {
		t.Fatalf("Title = %q, want the user-set session name", got)
	}
}

func TestPiPreviewRejectsAFileWithNoSessionHeader(t *testing.T) {
	root := t.TempDir()
	path := writeFile(t, filepath.Join(root, "--work-repo--", "2026-06-09T15-37-10-848Z_x.jsonl"),
		piLine(t, map[string]any{"type": "message", "id": "a1", "timestamp": "2026-06-09T15:37:24.526Z"}))

	_, ok, err := piImporterAt(root).Preview(context.Background(), path)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if ok {
		t.Fatal("Preview accepted a file with no session header")
	}
}

func TestPiPreviewFallsBackToTheFilenameSessionID(t *testing.T) {
	root := t.TempDir()
	id := "019ead07-cfbf-78d3-8b03-875769426583"
	path := writeFile(t, filepath.Join(root, "--work-repo--", "2026-06-09T15-37-10-848Z_"+id+".jsonl"),
		piLine(t, map[string]any{"type": "session", "version": 3, "timestamp": "2026-06-09T15:37:10.848Z", "cwd": "/work/repo"}))

	if got := piPreview(t, piImporterAt(root), path).SessionID; got != id {
		t.Fatalf("SessionID = %q, want %q from the filename", got, id)
	}
}

// TestPiPreviewStaysInsideTheByteBudget covers a session past the budget: the
// windows read must stay bounded and the turn count is then an estimate.
func TestPiPreviewStaysInsideTheByteBudget(t *testing.T) {
	root := t.TempDir()
	sessionID := "019ead07-cfbf-78d3-8b03-875769426583"
	filler := strings.Repeat("x", 4096)
	var body strings.Builder
	body.WriteString(piHeader(t, sessionID, "/work/repo", "2026-06-09T15:37:10.848Z"))
	parent := ""
	for i := range 400 {
		id := fmt.Sprintf("a%03d", i)
		ts := time.Date(2026, 6, 9, 15, 37, 10, 0, time.UTC).Add(time.Duration(i) * time.Second).Format(time.RFC3339)
		body.WriteString(piAssistantEntry(t, id, parent, ts, 1781019441000+int64(i)*1000,
			[]map[string]any{piTextBlock("reply " + filler)}, nil))
		parent = id
	}
	path := writeFile(t, filepath.Join(root, "--work-repo--", "2026-06-09T15-37-10-848Z_"+sessionID+".jsonl"), body.String())

	win, err := ReadPreviewWindows(path, PreviewByteBudget)
	if err != nil {
		t.Fatalf("ReadPreviewWindows: %v", err)
	}
	if win.Size <= PreviewByteBudget {
		t.Fatalf("fixture is %d bytes, want more than the %d-byte budget", win.Size, PreviewByteBudget)
	}
	if read := len(win.Head) + len(win.Tail); read > PreviewByteBudget {
		t.Fatalf("preview read %d bytes, want at most %d", read, PreviewByteBudget)
	}

	preview := piPreview(t, piImporterAt(root), path)
	if !preview.ApproxTurns {
		t.Fatal("ApproxTurns = false, want true when the count was scaled from the head window")
	}
	if preview.TurnCount == 0 {
		t.Fatal("TurnCount = 0, want an estimate scaled from the head window")
	}
	if preview.SessionID != sessionID {
		t.Fatalf("SessionID = %q, want %q", preview.SessionID, sessionID)
	}
}

func TestPiPreviewMatchesTheImportedTurnCount(t *testing.T) {
	root := t.TempDir()
	path := writePiSession(t, root, "--work-repo--", "019ead07-cfbf-78d3-8b03-875769426583")
	imp := piImporterAt(root)
	preview := piPreview(t, imp, path)
	turns := collectTurns(t, imp, preview)
	if preview.TurnCount != len(turns) {
		t.Fatalf("preview said %d turns, the import produced %d", preview.TurnCount, len(turns))
	}
}

// TestPiPreviewCountLeavesOutAForksCopiedTurns pins the preview against the
// import for a fork. Counting every assistant entry in the file promised turns
// the import does not deliver, because the copied region belongs to the trunk:
// on the development machine that made 229 of the 2,231 previews small enough to
// be counted exactly disagree with their own import.
func TestPiPreviewCountLeavesOutAForksCopiedTurns(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "--work-repo--")
	trunkID := "019ead07-cfbf-78d3-8b03-875769426583"
	forkID := "019eae5b-8461-71ba-bb5b-0f947f105da6"

	trunkPath := writeFile(t, filepath.Join(dir, "2026-06-09T15-37-10-848Z_"+trunkID+".jsonl"),
		piHeader(t, trunkID, "/work/repo", "2026-06-09T15:37:10.848Z")+
			piAssistantEntry(t, "a1", "", "2026-06-09T15:37:24.000Z", 1781019441000,
				[]map[string]any{piTextBlock("one")}, nil))

	forkBody := piLine(t, map[string]any{
		"type": "session", "version": 3, "id": forkID,
		"timestamp": "2026-06-09T15:40:00.000Z", "cwd": "/work/repo", "parentSession": trunkPath,
	}) +
		// Two copied turns, then one the fork ran itself.
		piAssistantEntry(t, "a1", "", "2026-06-09T15:37:24.000Z", 1781019441000,
			[]map[string]any{piTextBlock("one")}, nil) +
		piAssistantEntry(t, "a2", "a1", "2026-06-09T15:38:24.000Z", 1781019501000,
			[]map[string]any{piTextBlock("two")}, nil) +
		piUserEntry(t, "u3", "a2", "2026-06-09T15:41:00.000Z", "again", 1781019700000) +
		piAssistantEntry(t, "a3", "u3", "2026-06-09T15:41:06.000Z", 1781019702000,
			[]map[string]any{piTextBlock("three")}, nil)
	forkPath := writeFile(t, filepath.Join(dir, "2026-06-09T15-40-00-000Z_"+forkID+".jsonl"), forkBody)

	imp := piImporterAt(root)
	preview := piPreview(t, imp, forkPath)
	turns := collectTurns(t, imp, preview)
	if len(turns) != 1 {
		t.Fatalf("imported %d turns, want only the one the fork ran", len(turns))
	}
	if preview.TurnCount != len(turns) {
		t.Fatalf("preview said %d turns, the import produced %d", preview.TurnCount, len(turns))
	}
}

// TestPiPreviewOfALargeForkCountsFromTheTail covers the one shape where the
// head window is useless: a fork bigger than the byte budget. Its copied region
// sits at the front of the file, the import skips every entry in it, so the head
// holds no countable turn and a head-only estimate reported "about 0 turns" for
// a session whose import produced dozens. On the development machine that was 84
// of the 305 sessions over the budget.
func TestPiPreviewOfALargeForkCountsFromTheTail(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "--work-repo--")
	trunkID := "019ead07-cfbf-78d3-8b03-875769426583"
	forkID := "019eae5b-8461-71ba-bb5b-0f947f105da6"
	filler := strings.Repeat("x", 4096)
	forkAt := time.Date(2026, 6, 9, 16, 0, 0, 0, time.UTC)

	// The trunk holds the copied region, so both files carry the same 400 turns
	// before the fork instant and only the fork adds turns after it.
	var copied strings.Builder
	parent := ""
	for i := range 400 {
		id := fmt.Sprintf("a%03d", i)
		ts := forkAt.Add(-time.Duration(400-i) * time.Second).Format(time.RFC3339)
		copied.WriteString(piAssistantEntry(t, id, parent, ts, 1781019441000+int64(i)*1000,
			[]map[string]any{piTextBlock("reply " + filler)}, nil))
		parent = id
	}
	trunkPath := writeFile(t, filepath.Join(dir, "2026-06-09T15-37-10-848Z_"+trunkID+".jsonl"),
		piHeader(t, trunkID, "/work/repo", "2026-06-09T15:37:10.848Z")+copied.String())

	var forkBody strings.Builder
	forkBody.WriteString(piLine(t, map[string]any{
		"type": "session", "version": 3, "id": forkID,
		"timestamp": forkAt.Format(time.RFC3339), "cwd": "/work/repo", "parentSession": trunkPath,
	}))
	forkBody.WriteString(copied.String())
	for i := range 3 {
		id := fmt.Sprintf("f%03d", i)
		ts := forkAt.Add(time.Duration(i+1) * time.Minute).Format(time.RFC3339)
		forkBody.WriteString(piAssistantEntry(t, id, parent, ts, 1781029441000+int64(i)*1000,
			[]map[string]any{piTextBlock("fork reply " + filler)}, nil))
		parent = id
	}
	forkPath := writeFile(t, filepath.Join(dir, "2026-06-09T16-00-00-000Z_"+forkID+".jsonl"), forkBody.String())

	win, err := ReadPreviewWindows(forkPath, PreviewByteBudget)
	if err != nil {
		t.Fatalf("ReadPreviewWindows: %v", err)
	}
	if win.Size <= PreviewByteBudget {
		t.Fatalf("fixture is %d bytes, want more than the %d-byte budget", win.Size, PreviewByteBudget)
	}
	if got := piCountAssistantTurns(win.HeadLines(), forkAt); got != 0 {
		t.Fatalf("head window holds %d countable turns, want 0 so the fixture covers the tail fallback", got)
	}

	imp := piImporterAt(root)
	preview := piPreview(t, imp, forkPath)
	if preview.TurnCount == 0 {
		t.Fatal("TurnCount = 0, want an estimate scaled from the tail window")
	}
	if !preview.ApproxTurns {
		t.Fatal("ApproxTurns = false, want true when the count was scaled")
	}
	if turns := collectTurns(t, imp, preview); len(turns) != 3 {
		t.Fatalf("imported %d turns, want the 3 the fork ran itself", len(turns))
	}
}

func TestPiTurnsMapOneGenerationPerAssistantEntry(t *testing.T) {
	root := t.TempDir()
	sessionID := "019ead07-cfbf-78d3-8b03-875769426583"
	path := writePiSession(t, root, "--work-repo--", sessionID)
	imp := piImporterAt(root)

	turns := collectTurns(t, imp, piPreview(t, imp, path))
	if len(turns) != 2 {
		t.Fatalf("got %d turns, want 2", len(turns))
	}

	first := turns[0]
	if first.Gen.ConversationID != sessionID {
		t.Fatalf("ConversationID = %q, want the session header ID %q", first.Gen.ConversationID, sessionID)
	}
	if first.Gen.Model.Provider != "anthropic" || first.Gen.Model.Name != "claude-opus-4-8" {
		t.Fatalf("Model = %+v, want the provider and model from the entry", first.Gen.Model)
	}
	if first.Gen.ResponseID != "msg_a1" || first.Gen.ResponseModel != "claude-opus-4-8" {
		t.Fatalf("ResponseID = %q ResponseModel = %q", first.Gen.ResponseID, first.Gen.ResponseModel)
	}
	if first.Gen.StopReason != "tool_use" {
		t.Fatalf("StopReason = %q, want tool_use for pi's toolUse", first.Gen.StopReason)
	}
	if first.Gen.Mode != "STREAM" || first.Gen.OperationName != "streamText" {
		t.Fatalf("Mode = %q OperationName = %q, want STREAM/streamText as live exports", first.Gen.Mode, first.Gen.OperationName)
	}
	if first.Gen.ThinkingEnabled == nil || !*first.Gen.ThinkingEnabled {
		t.Fatal("ThinkingEnabled is not set for a turn with a thinking block")
	}
	// pi's own totalTokens is input+output+cacheRead+cacheWrite, which is what
	// mapPiUsage passes through.
	usage := first.Gen.Usage
	if usage.InputTokens != 12 || usage.OutputTokens != 34 || usage.TotalTokens != 57 ||
		usage.CacheReadInputTokens != 5 || usage.CacheWriteInputTokens != 6 {
		t.Fatalf("Usage = %+v, want the mapPiUsage mapping", usage)
	}
	if usage.ReasoningTokens != 0 {
		t.Fatalf("ReasoningTokens = %d, want 0: live does not forward reasoning", usage.ReasoningTokens)
	}
	if got := first.Gen.Metadata["cost_usd"]; got != 0.006 {
		t.Fatalf("metadata cost_usd = %v, want 0.006", got)
	}
	if want := time.UnixMilli(1781019441000).UTC(); !first.Gen.StartedAt.Equal(want) {
		t.Fatalf("StartedAt = %s, want the assistant message timestamp %s", first.Gen.StartedAt, want)
	}
	if want := time.Date(2026, 6, 9, 15, 37, 24, 526000000, time.UTC); !first.Gen.CompletedAt.Equal(want) {
		t.Fatalf("CompletedAt = %s, want the entry timestamp %s", first.Gen.CompletedAt, want)
	}
	if first.Quality.ApproxStartedAt || first.Quality.ApproxCompletedAt {
		t.Fatalf("quality = %+v, want neither timestamp approximate: pi persists both", first.Quality)
	}
	if first.Quality.ApproxUsage || first.Quality.MissingModel {
		t.Fatalf("quality = %+v, want no approximation flags for a complete entry", first.Quality)
	}

	// The user prompt is the input, the assistant content plus the matched tool
	// result the output.
	if len(first.Gen.Input) != 1 || first.Gen.Input[0].Parts[0].Text != "review the last commit" {
		t.Fatalf("Input = %+v, want the user prompt", first.Gen.Input)
	}
	kinds := []string{}
	for _, msg := range first.Gen.Output {
		for _, part := range msg.Parts {
			kinds = append(kinds, string(part.Kind))
		}
	}
	if want := []string{"thinking", "text", "tool_call", "tool_result"}; !slices.Equal(kinds, want) {
		t.Fatalf("output part kinds = %v, want %v", kinds, want)
	}
	call := first.Gen.Output[2].Parts[0].ToolCall
	if call.Name != "bash" || call.ID != "toolu_1" || string(call.InputJSON) != `{"command":"git log -1"}` {
		t.Fatalf("tool call = %+v, want the call from the entry", call)
	}
	result := first.Gen.Output[3].Parts[0].ToolResult
	if result.ToolCallID != "toolu_1" || result.Name != "bash" || result.Content != "commit abc" || result.IsError {
		t.Fatalf("tool result = %+v, want the result answering the call", result)
	}
	if thinking := first.Gen.Output[0].Parts[0].Thinking; thinking != "I should read the log first." {
		t.Fatalf("thinking = %q, want the thinking text pi persisted", thinking)
	}

	// AgentName is left to the framework, which fills it from the agent ID.
	if first.Gen.AgentName != "" {
		t.Fatalf("AgentName = %q, want empty so Exporter.prepare fills it", first.Gen.AgentName)
	}
	var prepared HistoricalGeneration = first
	(&Exporter{}).prepare(&prepared)
	if prepared.Gen.AgentName != "pi" {
		t.Fatalf("prepared AgentName = %q, want pi", prepared.Gen.AgentName)
	}

	// The second turn carries only its own prompt and no tool traffic.
	second := turns[1]
	if len(second.Gen.Input) != 1 || second.Gen.Input[0].Parts[0].Text != "and the tests?" {
		t.Fatalf("second Input = %+v, want only the prompt since the previous turn", second.Gen.Input)
	}
	if len(second.Gen.Output) != 1 || second.Gen.Output[0].Parts[0].Text != "They pass." {
		t.Fatalf("second Output = %+v", second.Gen.Output)
	}
	if second.Gen.StopReason != "end_turn" {
		t.Fatalf("second StopReason = %q, want end_turn for pi's stop", second.Gen.StopReason)
	}
	if len(second.Gen.Tools) != 0 {
		t.Fatalf("second Tools = %+v, want none: the turn called nothing", second.Gen.Tools)
	}
	if len(first.Gen.Tools) != 1 || first.Gen.Tools[0].Name != "bash" {
		t.Fatalf("Tools = %+v, want one name-only definition for the called tool", first.Gen.Tools)
	}
	if first.Gen.Tools[0].Description != "" || len(first.Gen.Tools[0].InputSchema) != 0 {
		t.Fatal("tool definitions must be name-only: descriptions and schemas are runtime-only")
	}
}

func TestPiStopReasonMapping(t *testing.T) {
	tests := map[string]string{
		"stop":     "end_turn",
		"length":   "max_tokens",
		"toolUse":  "tool_use",
		"error":    "error",
		"aborted":  "aborted",
		"whatever": "whatever",
		"":         "",
	}
	for in, want := range tests {
		if got := piStopReason(in); got != want {
			t.Errorf("piStopReason(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPiTurnQualityFlagsWhatTheSourceLacks(t *testing.T) {
	root := t.TempDir()
	sessionID := "019ead07-cfbf-78d3-8b03-875769426583"
	body := piHeader(t, sessionID, "/work/repo", "2026-06-09T15:37:10.848Z") +
		piAssistantEntry(t, "a1", "", "2026-06-09T15:37:24.526Z", 1781019441000,
			[]map[string]any{piTextBlock("no model, no tokens")},
			map[string]any{"model": "", "usage": nil})
	path := writeFile(t, filepath.Join(root, "--work-repo--", "2026-06-09T15-37-10-848Z_"+sessionID+".jsonl"), body)

	imp := piImporterAt(root)
	turns := collectTurns(t, imp, piPreview(t, imp, path))
	if len(turns) != 1 {
		t.Fatalf("got %d turns, want the turn imported anyway", len(turns))
	}
	q := turns[0].Quality
	if !q.MissingModel || !q.ApproxUsage {
		t.Fatalf("quality = %+v, want MissingModel and ApproxUsage", q)
	}
	if q.ApproxStartedAt || q.ApproxCompletedAt {
		t.Fatalf("quality = %+v, want both timestamps exact: pi persists both", q)
	}
	if _, ok := turns[0].Gen.Metadata["cost_usd"]; ok {
		t.Fatalf("metadata = %+v, want no cost for an entry with no usage", turns[0].Gen.Metadata)
	}
}

// TestPiUsageWithOnlyACostIsApproximate covers a usage block that reports a
// cost and no token counts. Reporting "0 tokens" there would state a number the
// source does not have.
func TestPiUsageWithOnlyACostIsApproximate(t *testing.T) {
	root := t.TempDir()
	sessionID := "019ead07-cfbf-78d3-8b03-875769426583"
	body := piHeader(t, sessionID, "/work/repo", "2026-06-09T15:37:10.848Z") +
		piAssistantEntry(t, "a1", "", "2026-06-09T15:37:24.526Z", 1781019441000,
			[]map[string]any{piTextBlock("cost only")},
			map[string]any{"usage": map[string]any{"cost": map[string]any{"total": 0.02}}})
	path := writeFile(t, filepath.Join(root, "--work-repo--", "2026-06-09T15-37-10-848Z_"+sessionID+".jsonl"), body)

	imp := piImporterAt(root)
	turns := collectTurns(t, imp, piPreview(t, imp, path))
	if len(turns) != 1 {
		t.Fatalf("got %d turns, want 1", len(turns))
	}
	if !turns[0].Quality.ApproxUsage {
		t.Error("ApproxUsage = false, want true for usage with no token counts")
	}
	if got := turns[0].Gen.Metadata["cost_usd"]; got != 0.02 {
		t.Fatalf("cost_usd = %v, want 0.02", got)
	}
}

func TestPiCallErrorComesFromTheMessage(t *testing.T) {
	root := t.TempDir()
	sessionID := "019ead07-cfbf-78d3-8b03-875769426583"
	body := piHeader(t, sessionID, "/work/repo", "2026-06-09T15:37:10.848Z") +
		piAssistantEntry(t, "a1", "", "2026-06-09T15:37:24.526Z", 1781019441000,
			[]map[string]any{piTextBlock("")},
			map[string]any{"stopReason": "error", "errorMessage": "provider overloaded"})
	path := writeFile(t, filepath.Join(root, "--work-repo--", "2026-06-09T15-37-10-848Z_"+sessionID+".jsonl"), body)

	imp := piImporterAt(root)
	turns := collectTurns(t, imp, piPreview(t, imp, path))
	if len(turns) != 1 {
		t.Fatalf("got %d turns, want 1", len(turns))
	}
	if turns[0].Gen.CallError != "provider overloaded" {
		t.Fatalf("CallError = %q, want the message's errorMessage", turns[0].Gen.CallError)
	}
	if turns[0].Gen.StopReason != "error" {
		t.Fatalf("StopReason = %q, want error", turns[0].Gen.StopReason)
	}
}

// TestPiRedactedThinkingIsDropped covers pi's redacted thinking block: live
// skips it, so an import must too, but the turn still counts as thinking.
func TestPiRedactedThinkingIsDropped(t *testing.T) {
	root := t.TempDir()
	sessionID := "019ead07-cfbf-78d3-8b03-875769426583"
	body := piHeader(t, sessionID, "/work/repo", "2026-06-09T15:37:10.848Z") +
		piAssistantEntry(t, "a1", "", "2026-06-09T15:37:24.526Z", 1781019441000,
			[]map[string]any{
				{"type": "thinking", "thinking": "hidden", "redacted": true},
				piTextBlock("answer"),
			}, nil)
	path := writeFile(t, filepath.Join(root, "--work-repo--", "2026-06-09T15-37-10-848Z_"+sessionID+".jsonl"), body)

	imp := piImporterAt(root)
	turns := collectTurns(t, imp, piPreview(t, imp, path))
	if len(turns) != 1 {
		t.Fatalf("got %d turns, want 1", len(turns))
	}
	if len(turns[0].Gen.Output) != 1 || turns[0].Gen.Output[0].Parts[0].Text != "answer" {
		t.Fatalf("Output = %+v, want the text alone", turns[0].Gen.Output)
	}
	if turns[0].Gen.ThinkingEnabled == nil || !*turns[0].Gen.ThinkingEnabled {
		t.Fatal("ThinkingEnabled is not set: the turn did think, the text is just withheld")
	}
}

// TestPiTitleComesFromTheSessionNameThenThePrompt pins the title precedence
// resolveConversationTitle sets in the live plugin.
func TestPiTitleComesFromTheSessionNameThenThePrompt(t *testing.T) {
	root := t.TempDir()
	sessionID := "019ead07-cfbf-78d3-8b03-875769426583"
	path := writePiSession(t, root, "--work-repo--", sessionID)
	imp := piImporterAt(root)

	turns := collectTurns(t, imp, piPreview(t, imp, path))
	if got := turns[0].Gen.ConversationTitle; got != "review the last commit" {
		t.Fatalf("ConversationTitle = %q, want the first prompt", got)
	}

	body, err := readFileString(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	named := writeFile(t, path, body+piLine(t, map[string]any{
		"type": "session_info", "id": "s1", "parentId": "a2",
		"timestamp": "2026-06-09T15:39:00.000Z", "name": "Commit review",
	}))
	turns = collectTurns(t, imp, piPreview(t, imp, named))
	if got := turns[0].Gen.ConversationTitle; got != "Commit review" {
		t.Fatalf("ConversationTitle = %q, want the user-set session name", got)
	}
}

func TestPiTitleIsClippedToOneHundredRunes(t *testing.T) {
	long := strings.Repeat("é", 150)
	got := piClipTitle(long)
	if n := len([]rune(got)); n != piMaxTitleLen {
		t.Fatalf("clipped title is %d runes, want %d", n, piMaxTitleLen)
	}
}

// TestPiLineageFollowsTheParentIDChain covers the within-session parent chain,
// including a branch where two assistant entries share one parent.
func TestPiLineageFollowsTheParentIDChain(t *testing.T) {
	root := t.TempDir()
	sessionID := "019ead07-cfbf-78d3-8b03-875769426583"
	body := piHeader(t, sessionID, "/work/repo", "2026-06-09T15:37:10.848Z") +
		piUserEntry(t, "u1", "", "2026-06-09T15:37:20.000Z", "first", 1781019439000) +
		piAssistantEntry(t, "a1", "u1", "2026-06-09T15:37:24.000Z", 1781019441000,
			[]map[string]any{piTextBlock("one")}, nil) +
		piUserEntry(t, "u2", "a1", "2026-06-09T15:38:00.000Z", "second", 1781019480000) +
		// Two answers to the same prompt: the user regenerated, so the branch
		// point has two children and both were real model calls.
		piAssistantEntry(t, "a2", "u2", "2026-06-09T15:38:06.000Z", 1781019482000,
			[]map[string]any{piTextBlock("two")}, nil) +
		piAssistantEntry(t, "a3", "u2", "2026-06-09T15:38:20.000Z", 1781019496000,
			[]map[string]any{piTextBlock("two, again")}, nil)
	path := writeFile(t, filepath.Join(root, "--work-repo--", "2026-06-09T15-37-10-848Z_"+sessionID+".jsonl"), body)

	imp := piImporterAt(root)
	turns := collectTurns(t, imp, piPreview(t, imp, path))
	if len(turns) != 3 {
		t.Fatalf("got %d turns, want all three including the abandoned branch", len(turns))
	}
	if len(turns[0].Gen.ParentGenerationIDs) != 0 {
		t.Fatalf("first turn parents = %v, want none", turns[0].Gen.ParentGenerationIDs)
	}
	// Both regenerations hang off the assistant turn before the branch point,
	// reached through the user entry between them.
	want := []string{turns[0].Gen.ID}
	for _, turn := range turns[1:] {
		if !slices.Equal(turn.Gen.ParentGenerationIDs, want) {
			t.Fatalf("ParentGenerationIDs = %v, want %v", turn.Gen.ParentGenerationIDs, want)
		}
	}
	if turns[1].Gen.ID == turns[2].Gen.ID {
		t.Fatal("the two branches share a generation ID")
	}
}

// TestPiForkReportsTheTrunkAsMetadata covers a session whose header names a
// parentSession. The copied parent turn's generation belongs to the trunk
// conversation, so an edge would name a generation that does not exist under
// this conversation ID; live reports it as metadata instead.
func TestPiForkReportsTheTrunkAsMetadata(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "--work-repo--")
	trunkID := "019ead07-cfbf-78d3-8b03-875769426583"
	forkID := "019eae5b-8461-71ba-bb5b-0f947f105da6"

	trunkBody := piHeader(t, trunkID, "/work/repo", "2026-06-09T15:37:10.848Z") +
		piUserEntry(t, "u1", "", "2026-06-09T15:37:20.000Z", "first", 1781019439000) +
		piAssistantEntry(t, "a1", "u1", "2026-06-09T15:37:24.000Z", 1781019441000,
			[]map[string]any{piTextBlock("one")}, nil)
	trunkPath := writeFile(t, filepath.Join(dir, "2026-06-09T15-37-10-848Z_"+trunkID+".jsonl"), trunkBody)

	// The fork copies the trunk's entries with their own timestamps and stamps
	// its header at fork time, which is later than every copied entry.
	forkBody := piLine(t, map[string]any{
		"type": "session", "version": 3, "id": forkID,
		"timestamp": "2026-06-09T15:40:00.000Z", "cwd": "/work/repo", "parentSession": trunkPath,
	}) +
		piUserEntry(t, "u1", "", "2026-06-09T15:37:20.000Z", "first", 1781019439000) +
		piAssistantEntry(t, "a1", "u1", "2026-06-09T15:37:24.000Z", 1781019441000,
			[]map[string]any{piTextBlock("one")}, nil) +
		piUserEntry(t, "u2", "a1", "2026-06-09T15:41:00.000Z", "try again", 1781019700000) +
		piAssistantEntry(t, "a2", "u2", "2026-06-09T15:41:06.000Z", 1781019702000,
			[]map[string]any{piTextBlock("differently")}, nil)
	forkPath := writeFile(t, filepath.Join(dir, "2026-06-09T15-40-00-000Z_"+forkID+".jsonl"), forkBody)

	imp := piImporterAt(root)
	trunkTurns := collectTurns(t, imp, piPreview(t, imp, trunkPath))
	if len(trunkTurns) != 1 {
		t.Fatalf("got %d trunk turns, want 1", len(trunkTurns))
	}
	forkTurns := collectTurns(t, imp, piPreview(t, imp, forkPath))
	// Only the fork's own turn is imported. The copied one belongs to the trunk,
	// which exports it itself; exporting it again would report one model call
	// twice, under two conversation IDs.
	if len(forkTurns) != 1 {
		t.Fatalf("got %d fork turns, want only the fork's own turn", len(forkTurns))
	}

	// The fork's own turn: its parent was copied from the trunk, so no edge.
	own := forkTurns[0]
	if len(own.Gen.ParentGenerationIDs) != 0 {
		t.Fatalf("ParentGenerationIDs = %v, want none: a dangling parent ID is the bug this avoids", own.Gen.ParentGenerationIDs)
	}
	if got := own.Gen.Metadata[MetaPiForkParentSession]; got != trunkID {
		t.Fatalf("%s = %v, want the trunk conversation ID %q", MetaPiForkParentSession, got, trunkID)
	}
	if got := own.Gen.Metadata[MetaPiForkParentGeneration]; got != trunkTurns[0].Gen.ID {
		t.Fatalf("%s = %v, want the trunk's own imported generation ID %q",
			MetaPiForkParentGeneration, got, trunkTurns[0].Gen.ID)
	}
	// The copied prompt is not the fork's first prompt either: live sees only
	// what the fork itself sent.
	if own.Gen.ConversationTitle != "try again" {
		t.Fatalf("ConversationTitle = %q, want the fork's own first prompt", own.Gen.ConversationTitle)
	}
	if len(own.Gen.Input) != 1 || own.Gen.Input[0].Parts[0].Text != "try again" {
		t.Fatalf("Input = %+v, want only the prompt the fork sent", own.Gen.Input)
	}
}

// TestPiForkOfAForkLinksNothing covers a parent entry the trunk itself inherited
// rather than produced: the trunk holds no generation for it, so neither key is
// written. Live's forkMetadata writes both or neither for the same reason.
func TestPiForkOfAForkLinksNothing(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "--work-repo--")
	trunkID := "019ead07-cfbf-78d3-8b03-875769426583"
	forkID := "019eae5b-8461-71ba-bb5b-0f947f105da6"

	// The trunk is itself a fork: its assistant entry a1 predates its own
	// header, so the trunk inherited a1 and never ran it.
	trunkBody := piLine(t, map[string]any{
		"type": "session", "version": 3, "id": trunkID,
		"timestamp": "2026-06-09T15:39:00.000Z", "cwd": "/work/repo",
		"parentSession": filepath.Join(dir, "older.jsonl"),
	}) +
		piAssistantEntry(t, "a1", "", "2026-06-09T15:37:24.000Z", 1781019441000,
			[]map[string]any{piTextBlock("one")}, nil)
	trunkPath := writeFile(t, filepath.Join(dir, "2026-06-09T15-39-00-000Z_"+trunkID+".jsonl"), trunkBody)

	forkBody := piLine(t, map[string]any{
		"type": "session", "version": 3, "id": forkID,
		"timestamp": "2026-06-09T15:40:00.000Z", "cwd": "/work/repo", "parentSession": trunkPath,
	}) +
		piAssistantEntry(t, "a1", "", "2026-06-09T15:37:24.000Z", 1781019441000,
			[]map[string]any{piTextBlock("one")}, nil) +
		piUserEntry(t, "u2", "a1", "2026-06-09T15:41:00.000Z", "try again", 1781019700000) +
		piAssistantEntry(t, "a2", "u2", "2026-06-09T15:41:06.000Z", 1781019702000,
			[]map[string]any{piTextBlock("differently")}, nil)
	forkPath := writeFile(t, filepath.Join(dir, "2026-06-09T15-40-00-000Z_"+forkID+".jsonl"), forkBody)

	imp := piImporterAt(root)
	turns := collectTurns(t, imp, piPreview(t, imp, forkPath))
	if len(turns) != 1 {
		t.Fatalf("got %d turns, want only the fork's own turn", len(turns))
	}
	if len(turns[0].Gen.ParentGenerationIDs) != 0 {
		t.Fatalf("ParentGenerationIDs = %v, want none", turns[0].Gen.ParentGenerationIDs)
	}
	for _, key := range []string{MetaPiForkParentSession, MetaPiForkParentGeneration} {
		if _, ok := turns[0].Gen.Metadata[key]; ok {
			t.Fatalf("metadata = %+v, want neither fork key: the trunk holds no generation for the copied entry", turns[0].Gen.Metadata)
		}
	}
	if !slices.Contains(turns[0].Quality.Notes, "pi_fork_parent_not_in_trunk") {
		t.Fatalf("quality notes = %v, want the parent-not-in-trunk note", turns[0].Quality.Notes)
	}
}

// TestPiForkWithAnUnreadableTrunkDropsTheLink covers a header naming a trunk
// this machine no longer has: nothing is linked, and the turn says so.
func TestPiForkWithAnUnreadableTrunkDropsTheLink(t *testing.T) {
	root := t.TempDir()
	forkID := "019eae5b-8461-71ba-bb5b-0f947f105da6"
	body := piLine(t, map[string]any{
		"type": "session", "version": 3, "id": forkID,
		"timestamp": "2026-06-09T15:40:00.000Z", "cwd": "/work/repo",
		"parentSession": filepath.Join(root, "--work-repo--", "gone.jsonl"),
	}) +
		piAssistantEntry(t, "a1", "", "2026-06-09T15:37:24.000Z", 1781019441000,
			[]map[string]any{piTextBlock("copied")}, nil) +
		piUserEntry(t, "u2", "a1", "2026-06-09T15:41:00.000Z", "try again", 1781019700000) +
		piAssistantEntry(t, "a2", "u2", "2026-06-09T15:41:06.000Z", 1781019702000,
			[]map[string]any{piTextBlock("differently")}, nil)
	path := writeFile(t, filepath.Join(root, "--work-repo--", "2026-06-09T15-40-00-000Z_"+forkID+".jsonl"), body)

	imp := piImporterAt(root)
	turns := collectTurns(t, imp, piPreview(t, imp, path))
	if len(turns) != 1 {
		t.Fatalf("got %d turns, want only the fork's own turn", len(turns))
	}
	own := turns[0]
	if len(own.Gen.ParentGenerationIDs) != 0 {
		t.Fatalf("ParentGenerationIDs = %v, want none for an unreadable trunk", own.Gen.ParentGenerationIDs)
	}
	if _, ok := own.Gen.Metadata[MetaPiForkParentSession]; ok {
		t.Fatalf("metadata = %+v, want no trunk pointer when the trunk cannot be read", own.Gen.Metadata)
	}
	if !slices.Contains(own.Quality.Notes, "pi_fork_trunk_unreadable") {
		t.Fatalf("quality notes = %v, want the unreadable-trunk note", own.Quality.Notes)
	}
}

func TestPiTurnsAreDeterministic(t *testing.T) {
	root := t.TempDir()
	path := writePiSession(t, root, "--work-repo--", "019ead07-cfbf-78d3-8b03-875769426583")
	imp := piImporterAt(root)
	preview := piPreview(t, imp, path)

	first := collectTurns(t, imp, preview)
	second := collectTurns(t, imp, preview)
	if len(first) != len(second) {
		t.Fatalf("got %d then %d turns", len(first), len(second))
	}
	for i := range first {
		if first[i].Gen.ID != second[i].Gen.ID {
			t.Fatalf("turn %d: generation ID %q then %q", i, first[i].Gen.ID, second[i].Gen.ID)
		}
		if first[i].Source.Identity() != second[i].Source.Identity() {
			t.Fatalf("turn %d: ledger identity changed between runs", i)
		}
	}
}

func TestPiTurnsStopWhenTheConsumerStops(t *testing.T) {
	root := t.TempDir()
	sessionID := "019ead07-cfbf-78d3-8b03-875769426583"
	var body strings.Builder
	body.WriteString(piHeader(t, sessionID, "/work/repo", "2026-06-09T15:37:10.848Z"))
	parent := ""
	for i := range 20 {
		id := fmt.Sprintf("a%02d", i)
		ts := time.Date(2026, 6, 9, 15, 37, 10, 0, time.UTC).Add(time.Duration(i) * time.Second).Format(time.RFC3339)
		body.WriteString(piAssistantEntry(t, id, parent, ts, 1781019441000+int64(i)*1000,
			[]map[string]any{piTextBlock("reply")}, nil))
		parent = id
	}
	path := writeFile(t, filepath.Join(root, "--work-repo--", "2026-06-09T15-37-10-848Z_"+sessionID+".jsonl"), body.String())

	imp := piImporterAt(root)
	seen := 0
	for range imp.Turns(context.Background(), piPreview(t, imp, path)) {
		seen++
		if seen == 3 {
			break
		}
	}
	if seen != 3 {
		t.Fatalf("consumed %d turns, want 3 before breaking", seen)
	}
}

func TestPiTurnsHonourCancellation(t *testing.T) {
	root := t.TempDir()
	path := writePiSession(t, root, "--work-repo--", "019ead07-cfbf-78d3-8b03-875769426583")
	imp := piImporterAt(root)
	preview := piPreview(t, imp, path)

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

func TestPiTurnsReportAnUnreadableSession(t *testing.T) {
	imp := piImporterAt(t.TempDir())
	sess := SessionPreview{Agent: AgentPi, SessionID: "gone", SourcePath: filepath.Join(t.TempDir(), "missing.jsonl")}
	var gotErr error
	for _, err := range imp.Turns(context.Background(), sess) {
		gotErr = err
		break
	}
	if gotErr == nil {
		t.Fatal("Turns yielded no error for a missing session file")
	}
}

func TestPiRawContentIsSanitizedOnceByTheFramework(t *testing.T) {
	root := t.TempDir()
	sessionID := "019ead07-cfbf-78d3-8b03-875769426583"
	secret := "glc_abcdefghijklmnopqrstuvwx"
	body := piHeader(t, sessionID, "/work/repo", "2026-06-09T15:37:10.848Z") +
		piUserEntry(t, "u1", "", "2026-06-09T15:37:20.000Z", "my token is "+secret, 1781019439000) +
		piAssistantEntry(t, "a1", "u1", "2026-06-09T15:37:24.526Z", 1781019441000,
			[]map[string]any{piTextBlock("saw " + secret)}, nil)
	path := writeFile(t, filepath.Join(root, "--work-repo--", "2026-06-09T15-37-10-848Z_"+sessionID+".jsonl"), body)

	imp := piImporterAt(root)
	turns := collectTurns(t, imp, piPreview(t, imp, path))
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

// TestPiToolResultsBelongToTheCallingTurn covers a tool result appended after
// the next assistant entry began: it answers the earlier call, and must not be
// attached to the later turn.
func TestPiToolResultsBelongToTheCallingTurn(t *testing.T) {
	root := t.TempDir()
	sessionID := "019ead07-cfbf-78d3-8b03-875769426583"
	body := piHeader(t, sessionID, "/work/repo", "2026-06-09T15:37:10.848Z") +
		piAssistantEntry(t, "a1", "", "2026-06-09T15:37:24.000Z", 1781019441000,
			[]map[string]any{piToolCallBlock("toolu_1", "read", map[string]any{"path": "a.go"})},
			map[string]any{"stopReason": "toolUse"}) +
		piToolResultEntry(t, "r1", "a1", "2026-06-09T15:37:25.000Z", "toolu_1", "read", "package a", false) +
		piAssistantEntry(t, "a2", "r1", "2026-06-09T15:37:30.000Z", 1781019448000,
			[]map[string]any{piToolCallBlock("toolu_2", "read", map[string]any{"path": "b.go"})},
			map[string]any{"stopReason": "toolUse"}) +
		piToolResultEntry(t, "r2", "a2", "2026-06-09T15:37:31.000Z", "toolu_2", "read", "package b", false)
	path := writeFile(t, filepath.Join(root, "--work-repo--", "2026-06-09T15-37-10-848Z_"+sessionID+".jsonl"), body)

	imp := piImporterAt(root)
	turns := collectTurns(t, imp, piPreview(t, imp, path))
	if len(turns) != 2 {
		t.Fatalf("got %d turns, want 2", len(turns))
	}
	for i, want := range []string{"package a", "package b"} {
		results := 0
		for _, msg := range turns[i].Gen.Output {
			for _, part := range msg.Parts {
				if part.ToolResult == nil {
					continue
				}
				results++
				if part.ToolResult.Content != want {
					t.Fatalf("turn %d tool result = %q, want %q", i, part.ToolResult.Content, want)
				}
			}
		}
		if results != 1 {
			t.Fatalf("turn %d carries %d tool results, want exactly its own", i, results)
		}
	}
}

// TestPiToolResultTextDropsEmptyBlocks pins the flattening against
// toolResultText (plugins/pi/src/mappers.ts), which drops a text block with no
// text. Keeping one would join to a leading newline where live joins to a plain
// line, and no fixture case has a multi-block tool result to catch it.
func TestPiToolResultTextDropsEmptyBlocks(t *testing.T) {
	tests := []struct {
		name    string
		content []map[string]any
		want    string
	}{
		{"one block", []map[string]any{piTextBlock("data")}, "data"},
		{"empty first block", []map[string]any{piTextBlock(""), piTextBlock("data")}, "data"},
		{"two blocks join", []map[string]any{piTextBlock("one"), piTextBlock("two")}, "one\ntwo"},
		{"image block dropped", []map[string]any{{"type": "image", "data": "…"}, piTextBlock("data")}, "data"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			sessionID := "019ead07-cfbf-78d3-8b03-875769426583"
			result := piLine(t, map[string]any{
				"type": "message", "id": "r1", "parentId": "a1", "timestamp": "2026-06-09T15:37:25.000Z",
				"message": map[string]any{
					"role": "toolResult", "toolCallId": "toolu_1", "toolName": "read",
					"content": tt.content, "isError": false, "timestamp": 1781019440000,
				},
			})
			body := piHeader(t, sessionID, "/work/repo", "2026-06-09T15:37:10.848Z") +
				piAssistantEntry(t, "a1", "", "2026-06-09T15:37:24.000Z", 1781019441000,
					[]map[string]any{piToolCallBlock("toolu_1", "read", map[string]any{"path": "a.go"})},
					map[string]any{"stopReason": "toolUse"}) + result
			path := writeFile(t, filepath.Join(root, "--work-repo--", "2026-06-09T15-37-10-848Z_"+sessionID+".jsonl"), body)

			imp := piImporterAt(root)
			turns := collectTurns(t, imp, piPreview(t, imp, path))
			if len(turns) != 1 {
				t.Fatalf("got %d turns, want 1", len(turns))
			}
			got := piOnlyToolResult(t, turns[0])
			if got != tt.want {
				t.Fatalf("tool result content = %q, want %q", got, tt.want)
			}
		})
	}
}

// piOnlyToolResult returns the content of the turn's single tool result.
func piOnlyToolResult(t *testing.T, turn HistoricalGeneration) string {
	t.Helper()
	var found []string
	for _, msg := range turn.Gen.Output {
		for _, part := range msg.Parts {
			if part.ToolResult != nil {
				found = append(found, part.ToolResult.Content)
			}
		}
	}
	if len(found) != 1 {
		t.Fatalf("turn carries %d tool results, want 1", len(found))
	}
	return found[0]
}

// TestPiToolResultsSurviveAnEntryWithNoID covers an assistant entry whose id is
// missing or shared. The results are collected from the entry's position in the
// file, so a log that breaks its own ID rule loses no tool output silently.
func TestPiToolResultsSurviveAnEntryWithNoID(t *testing.T) {
	root := t.TempDir()
	sessionID := "019ead07-cfbf-78d3-8b03-875769426583"
	body := piHeader(t, sessionID, "/work/repo", "2026-06-09T15:37:10.848Z") +
		piAssistantEntry(t, "", "", "2026-06-09T15:37:24.000Z", 1781019441000,
			[]map[string]any{piToolCallBlock("toolu_1", "read", map[string]any{"path": "a.go"})},
			map[string]any{"stopReason": "toolUse"}) +
		piToolResultEntry(t, "r1", "", "2026-06-09T15:37:25.000Z", "toolu_1", "read", "package a", false)
	path := writeFile(t, filepath.Join(root, "--work-repo--", "2026-06-09T15-37-10-848Z_"+sessionID+".jsonl"), body)

	imp := piImporterAt(root)
	turns := collectTurns(t, imp, piPreview(t, imp, path))
	if len(turns) != 1 {
		t.Fatalf("got %d turns, want 1", len(turns))
	}
	if got := piOnlyToolResult(t, turns[0]); got != "package a" {
		t.Fatalf("tool result content = %q, want %q", got, "package a")
	}
}

// TestPiOneBadContentBlockKeepsTheRest covers schema drift inside one block: a
// tool call whose id is a number. Go's decoder fills every element it can and
// reports the type error at the end, so the turn keeps the blocks that decoded
// instead of exporting a turn that says the model produced nothing.
func TestPiOneBadContentBlockKeepsTheRest(t *testing.T) {
	root := t.TempDir()
	sessionID := "019ead07-cfbf-78d3-8b03-875769426583"
	body := piHeader(t, sessionID, "/work/repo", "2026-06-09T15:37:10.848Z") +
		piAssistantEntry(t, "a1", "", "2026-06-09T15:37:24.000Z", 1781019441000,
			[]map[string]any{
				piTextBlock("one"),
				{"type": "toolCall", "id": 7, "name": "bash", "arguments": map[string]any{}},
				piTextBlock("three"),
			}, nil)
	path := writeFile(t, filepath.Join(root, "--work-repo--", "2026-06-09T15-37-10-848Z_"+sessionID+".jsonl"), body)

	imp := piImporterAt(root)
	turns := collectTurns(t, imp, piPreview(t, imp, path))
	if len(turns) != 1 {
		t.Fatalf("got %d turns, want 1", len(turns))
	}
	var texts []string
	for _, msg := range turns[0].Gen.Output {
		for _, part := range msg.Parts {
			if part.Kind == agento11y.PartKindText {
				texts = append(texts, part.Text)
			}
		}
	}
	if !slices.Equal(texts, []string{"one", "three"}) {
		t.Fatalf("output text = %v, want both readable blocks", texts)
	}
}

// TestPiForkTrunkPointerIgnoresPathNoise covers the one way a fork's trunk
// pointer can name a generation nothing ingested: the trunk generation ID hashes
// the path, and the path comes from the header rather than from discovery. A
// header that spells the same file with a detour must still produce the ID the
// trunk's own import produced.
func TestPiForkTrunkPointerIgnoresPathNoise(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "--work-repo--")
	trunkID := "019ead07-cfbf-78d3-8b03-875769426583"
	forkID := "019eae5b-8461-71ba-bb5b-0f947f105da6"

	trunkPath := writeFile(t, filepath.Join(dir, "2026-06-09T15-37-10-848Z_"+trunkID+".jsonl"),
		piHeader(t, trunkID, "/work/repo", "2026-06-09T15:37:10.848Z")+
			piAssistantEntry(t, "a1", "", "2026-06-09T15:37:24.000Z", 1781019441000,
				[]map[string]any{piTextBlock("one")}, nil))
	// Built by concatenation, because filepath.Join would clean the detour away.
	sep := string(filepath.Separator)
	noisy := dir + sep + ".." + sep + filepath.Base(dir) + sep + filepath.Base(trunkPath)

	forkBody := piLine(t, map[string]any{
		"type": "session", "version": 3, "id": forkID,
		"timestamp": "2026-06-09T15:40:00.000Z", "cwd": "/work/repo", "parentSession": noisy,
	}) +
		piAssistantEntry(t, "a1", "", "2026-06-09T15:37:24.000Z", 1781019441000,
			[]map[string]any{piTextBlock("one")}, nil) +
		piUserEntry(t, "u2", "a1", "2026-06-09T15:41:00.000Z", "try again", 1781019700000) +
		piAssistantEntry(t, "a2", "u2", "2026-06-09T15:41:06.000Z", 1781019702000,
			[]map[string]any{piTextBlock("differently")}, nil)
	forkPath := writeFile(t, filepath.Join(dir, "2026-06-09T15-40-00-000Z_"+forkID+".jsonl"), forkBody)

	imp := piImporterAt(root)
	trunkTurns := collectTurns(t, imp, piPreview(t, imp, trunkPath))
	if len(trunkTurns) != 1 {
		t.Fatalf("got %d trunk turns, want 1", len(trunkTurns))
	}
	forkTurns := collectTurns(t, imp, piPreview(t, imp, forkPath))
	if len(forkTurns) != 1 {
		t.Fatalf("got %d fork turns, want only the fork's own turn", len(forkTurns))
	}
	if got := forkTurns[0].Gen.Metadata[MetaPiForkParentGeneration]; got != trunkTurns[0].Gen.ID {
		t.Fatalf("%s = %v, want the ID the trunk's own import produced (%q)",
			MetaPiForkParentGeneration, got, trunkTurns[0].Gen.ID)
	}
}

// TestPiSubagentRunsAreNotSessions pins the discovery exclusions against the
// real tree shape: only the top-level session file is a session.
func TestPiSubagentRunsAreNotSessions(t *testing.T) {
	root := t.TempDir()
	sessionID := "019ead07-cfbf-78d3-8b03-875769426583"
	sessionPath := writePiSession(t, root, "--work-repo--", sessionID)
	runBody := piHeader(t, "child-1", "/work/repo", "2026-06-09T15:37:40.000Z") +
		piAssistantEntry(t, "c1", "", "2026-06-09T15:37:44.000Z", 1781019460000,
			[]map[string]any{piTextBlock("child reply")}, nil)
	writeFile(t, filepath.Join(root, "--work-repo--", "2026-06-09T15-37-10-848Z_"+sessionID, "43cec431", "run-0", "session.jsonl"), runBody)
	writeFile(t, filepath.Join(root, "--work-repo--", "subagent-artifacts", "3baf6dad_reviewer_0_output.jsonl"), runBody)

	imp := piImporterAt(root)
	files, err := walkFiles(context.Background(), root, imp.Match)
	if err != nil {
		t.Fatalf("walkFiles: %v", err)
	}
	if !slices.Equal(files, []string{sessionPath}) {
		t.Fatalf("discovered %v, want only the top-level session %q", files, sessionPath)
	}
}

func readFileString(path string) (string, error) {
	data, err := os.ReadFile(path)
	return string(data), err
}
