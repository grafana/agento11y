package history

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/opencode/sessiondb"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/opencode/sessiondb/sessiondbtest"
)

func loadOpenCodeV2Fixture(t *testing.T) *sessiondbtest.Builder {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "opencode", "v2-mapping.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Messages []struct {
			ID, Type string
			Seq      int64
			Data     json.RawMessage
		}
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	db := sessiondbtest.New(t, sessiondbtest.WithoutV1(), sessiondbtest.WithSessionV2())
	db.AddSessionV2("root", "Root title", "/work/current", 1000, 15000)
	for _, row := range fixture.Messages {
		db.AddV2Message(row.ID, "root", row.Type, row.Seq, 1, 2, string(row.Data))
	}
	return db
}

func openCodeV2TurnsForTest(t *testing.T, db *sessiondbtest.Builder, id string) []HistoricalGeneration {
	t.Helper()
	store, err := sessiondb.Open(context.Background(), db.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	var turns []HistoricalGeneration
	for gen, err := range opencodeV2Turns(context.Background(), store, SessionPreview{Agent: AgentOpenCode, SessionID: id, SourcePath: db.Path}) {
		if err != nil {
			t.Fatal(err)
		}
		turns = append(turns, gen)
	}
	return turns
}

func TestOpenCodeV2ProjectionMapping(t *testing.T) {
	db := loadOpenCodeV2Fixture(t)
	turns := openCodeV2TurnsForTest(t, db, "root")
	var ids []string
	for _, turn := range turns {
		ids = append(ids, turn.Source.TurnID)
	}
	if !slices.Equal(ids, []string{"first", "second", "transparent-retry", "failed-assistant", "continuation"}) {
		t.Fatalf("terminal IDs = %v", ids)
	}
	first := turns[0]
	if first.Gen.AgentName != "opencode:build" || first.Gen.Model.Name != "test-model" || first.Gen.Model.Provider != "test-provider" || first.Gen.AgentVersion != "" || first.Gen.Mode != agento11y.GenerationModeStream || first.Gen.OperationName != "streamText" {
		t.Fatalf("model identity = %+v", first.Gen)
	}
	if first.Gen.ConversationTitle != "Root title" || first.Gen.Tags["cwd"] != "/work/current" || first.Gen.StopReason != "tool-calls" {
		t.Fatalf("generation = %+v", first.Gen)
	}
	if got := openCodeTextParts(first.Gen.Input); !slices.Equal(got, []string{"delivered prompt", "delivered steering"}) {
		t.Fatalf("compaction input = %v", got)
	}
	for i, turn := range turns {
		if i > 0 && i < 4 && len(turn.Gen.Input) != 0 {
			t.Fatalf("turn %s repeated/undelivered input", turn.Source.TurnID)
		}
		if i > 0 && !slices.Equal(turn.Gen.ParentGenerationIDs, []string{turns[i-1].Gen.ID}) {
			t.Fatalf("turn %s parents = %v", turn.Source.TurnID, turn.Gen.ParentGenerationIDs)
		}
		if !turn.Source.TurnIDStable || turn.Gen.ID != turn.Source.GenerationID() {
			t.Fatal("unstable projection identity")
		}
	}
	if got := openCodeTextParts(turns[4].Gen.Input); !slices.Equal(got, []string{"continue"}) {
		t.Fatalf("continuation input = %v", got)
	}
	wantUsage := agento11y.TokenUsage{InputTokens: 250, OutputTokens: 60, TotalTokens: 310, CacheReadInputTokens: 30, CacheWriteInputTokens: 20, ReasoningTokens: 10, InputSemantics: agento11y.TokenInputSemanticsInclusive}
	if first.Gen.Usage != wantUsage || first.Quality.ApproxUsage {
		t.Fatalf("usage = %+v, quality %+v", first.Gen.Usage, first.Quality)
	}
	if first.Gen.StartedAt.UnixMilli() != 1000 || first.Gen.CompletedAt.UnixMilli() != 2000 || first.Quality.ApproxCompletedAt {
		t.Fatalf("timestamps = %+v", first)
	}
	if first.Gen.Metadata["cost_usd"] != float64(0) || first.Gen.Metadata["opencode.input_source"] != "persisted_user_delta" || first.Gen.Metadata["opencode.settlement"] != "history" {
		t.Fatalf("metadata = %v", first.Gen.Metadata)
	}
	second := turns[1]
	if second.Gen.CompletedAt.UnixMilli() != 5000 || !second.Quality.ApproxCompletedAt || second.Gen.Metadata["opencode.completion_time_approximate"] != true {
		t.Fatal("completed-time fallback not approximate")
	}
	if !second.Quality.ApproxUsage || second.Gen.StopReason != "" || second.Gen.CallError != "" || second.Gen.Usage != (agento11y.TokenUsage{}) {
		t.Fatal("superseded row invented provider values")
	}
	if _, ok := second.Gen.Metadata["cost_usd"]; ok {
		t.Fatal("missing cost became zero")
	}
	if turns[3].Gen.CallError != "provider.failure" || !turns[3].Gen.CompletedAt.IsZero() {
		t.Fatal("structured assistant failure lost")
	}
	if turns[4].Quality.ApproxUsage || turns[4].Gen.Usage.InputSemantics != agento11y.TokenInputSemanticsInclusive {
		t.Fatal("present zero usage lost")
	}
	if first.Gen.CallError != "" {
		t.Fatal("tool failure made generation fail")
	}
	wantKinds := []agento11y.PartKind{agento11y.PartKindText, agento11y.PartKindThinking, agento11y.PartKindToolCall, agento11y.PartKindToolCall, agento11y.PartKindToolCall, agento11y.PartKindToolResult, agento11y.PartKindToolCall, agento11y.PartKindToolResult, agento11y.PartKindToolCall, agento11y.PartKindToolResult, agento11y.PartKindToolCall, agento11y.PartKindToolResult}
	if got := openCodePartKinds(first.Gen.Output); !slices.Equal(got, wantKinds) {
		t.Fatalf("ordered kinds = %v", got)
	}
	if got := string(first.Gen.Output[2].Parts[0].ToolCall.InputJSON); got != `"{\"token\":\"ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij\""` {
		t.Fatalf("stream input = %q", got)
	}
	if result := first.Gen.Output[5].Parts[0].ToolResult; !result.IsError || result.Content != "line one\nline two" {
		t.Fatalf("shell result = %+v", result)
	}
	for _, i := range []int{7, 9, 11} {
		if !first.Gen.Output[i].Parts[0].ToolResult.IsError {
			t.Fatalf("result %d not failed", i)
		}
	}
	var names []string
	for _, tool := range first.Gen.Tools {
		names = append(names, tool.Name)
	}
	if !slices.Equal(names, []string{"bash", "fetch", "invalid", "read", "shell"}) {
		t.Fatalf("tools = %v", names)
	}
	for i := range turns {
		(Sanitizer{}).Sanitize(&turns[i])
	}
	encoded, err := json.Marshal(turns)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "MUST-NOT-EXPORT") || strings.Contains(string(encoded), "CONFLICTING-") {
		t.Fatal("opaque data or JSON identity escaped")
	}
	if strings.Contains(string(encoded), "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij") || !strings.Contains(string(encoded), "REDACTED") {
		t.Fatal("streaming input/tool fallback escaped sanitizer")
	}
}

func TestOpenCodeV2ToolMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, tool, metadata string
		wantError            bool
	}{
		{name: "unrelated metadata", tool: "custom", metadata: `{"private":"MUST-NOT-EXPORT"}`},
		{name: "custom string exit", tool: "custom", metadata: `{"exit":"MUST-NOT-EXPORT"}`},
		{name: "custom object sessionID", tool: "custom", metadata: `{"sessionID":{"id":"MUST-NOT-EXPORT"}}`},
		{name: "custom object sessionId", tool: "custom", metadata: `{"sessionId":{"id":"MUST-NOT-EXPORT"}}`},
		{name: "custom combined hints", tool: "custom", metadata: `{"exit":"success","sessionID":{"id":"MUST-NOT-EXPORT"},"sessionId":["MUST-NOT-EXPORT"]}`},
		{name: "custom numeric exit", tool: "custom", metadata: `{"exit":1}`},
		{name: "shell nonzero exit", tool: "shell", metadata: `{"exit":1}`, wantError: true},
		{name: "bash zero exit", tool: "bash", metadata: `{"exit":0}`},
		{name: "shell string exit", tool: "shell", metadata: `{"exit":"success"}`},
		{name: "shell object exit", tool: "shell", metadata: `{"exit":{"code":1}}`},
		{name: "shell fractional exit", tool: "shell", metadata: `{"exit":1.5}`},
		{name: "null hints", tool: "shell", metadata: `{"exit":null,"sessionID":null,"sessionId":null}`},
		{name: "subagent object sessionID", tool: "subagent", metadata: `{"sessionID":{"id":"MUST-NOT-EXPORT"}}`},
		{name: "task object sessionId", tool: "task", metadata: `{"sessionId":{"id":"MUST-NOT-EXPORT"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := sessiondbtest.New(t, sessiondbtest.WithoutV1(), sessiondbtest.WithSessionV2())
			db.AddSessionV2("root", "Title", "/work", 1, 2)
			db.AddV2Message("first", "root", "assistant", 1, 1, 2, openCodeV2CompletedForTest)
			db.AddV2Message("second", "root", "assistant", 2, 1, 2, openCodeV2CompletedForTest)
			db.AddV2Message("last", "root", "assistant", 3, 1, 2, fmt.Sprintf(`{"time":{"created":1000,"completed":2000},"content":[{"type":"tool","id":"call","name":%q,"state":{"status":"completed","input":{},"metadata":%s}}]}`, tc.tool, tc.metadata))
			imp, _ := NewImporter(AgentOpenCode)
			sessions, err := imp.(MultiSessionImporter).Previews(context.Background(), db.Path)
			if err != nil || len(sessions) != 1 || sessions[0].TurnCount != 3 {
				t.Fatalf("previews = %+v, error %v", sessions, err)
			}
			exp := &recordingExporter{}
			result, err := RunImport(context.Background(), ImportOptions{Agent: AgentOpenCode, Importer: imp, Sessions: sessions, Exporter: exp, Ledger: testLedger(t)})
			if err != nil || result.Imported != 3 || result.Failed != 0 || len(exp.got) != 3 {
				t.Fatalf("import = %+v, error %v, exported %d", result, err, len(exp.got))
			}
			toolResult := exp.got[2].Gen.Output[1].Parts[0].ToolResult
			if toolResult == nil || toolResult.IsError != tc.wantError {
				t.Fatalf("tool result = %+v, want error %v", toolResult, tc.wantError)
			}
			encoded, err := json.Marshal(exp.got)
			if err != nil || strings.Contains(string(encoded), "MUST-NOT-EXPORT") {
				t.Fatalf("arbitrary metadata escaped export, encoding error %v", err)
			}
		})
	}
}

func TestOpenCodeV2StreamingInputPreservation(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		structured  bool
	}{
		{name: "valid object", input: `{"command":"echo"}`, structured: true},
		{name: "valid object with whitespace", input: " {\"command\":\"echo\"} \n", structured: true},
		{name: "incomplete object", input: `{"command":"echo"`},
		{name: "valid prefix with trailing text", input: `{} trailing incomplete arguments`},
		{name: "consecutive values", input: `{"command":"first"}{"command":"second"}`},
		{name: "secret in trailing text", input: `{} trailing ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij`},
		{name: "empty input"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input, err := json.Marshal(tc.input)
			if err != nil {
				t.Fatal(err)
			}
			db := sessiondbtest.New(t, sessiondbtest.WithoutV1(), sessiondbtest.WithSessionV2())
			db.AddSessionV2("root", "Title", "/work", 1, 2)
			db.AddV2Message("terminal", "root", "assistant", 1, 1, 2, fmt.Sprintf(`{"time":{"created":1000,"completed":2000},"content":[{"type":"tool","id":"call","name":"custom","state":{"status":"streaming","input":%s}}]}`, input))
			imp, _ := NewImporter(AgentOpenCode)
			sessions, err := imp.(MultiSessionImporter).Previews(context.Background(), db.Path)
			if err != nil {
				t.Fatal(err)
			}
			exp := &recordingExporter{}
			result, err := RunImport(context.Background(), ImportOptions{Agent: AgentOpenCode, Importer: imp, Sessions: sessions, Exporter: exp, Ledger: testLedger(t)})
			if err != nil || result.Imported != 1 || result.Failed != 0 || len(exp.got) != 1 {
				t.Fatalf("import = %+v, error %v, exported %d", result, err, len(exp.got))
			}
			raw := exp.got[0].Gen.Output[0].Parts[0].ToolCall.InputJSON
			var got, want any
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("exported input is invalid JSON: %v", err)
			}
			if tc.structured {
				if err := json.Unmarshal([]byte(tc.input), &want); err != nil {
					t.Fatal(err)
				}
			} else {
				want = strings.ReplaceAll(tc.input, "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij", "[REDACTED:github-pat]")
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("exported input = %q, want %q", got, want)
			}
		})
	}
}

func TestOpenCodeV2UsagePresence(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		approximate   bool
		want          int64
	}{
		{name: "missing", payload: `null`, approximate: true},
		{name: "partial", payload: `{"input":0}`, approximate: true},
		{name: "complete zero", payload: `{"input":0,"output":0,"reasoning":0,"cache":{"read":0,"write":0}}`},
		{name: "integral notation", payload: `{"input":2e2,"output":50.0,"reasoning":10,"cache":{"read":30,"write":20}}`, want: 310},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var tokens *sessiondb.V2Tokens
			if err := json.Unmarshal([]byte(tc.payload), &tokens); err != nil {
				t.Fatal(err)
			}
			got, approximate := opencodeV2Usage(tokens)
			if approximate != tc.approximate || got.TotalTokens != tc.want {
				t.Fatalf("usage = %+v, approximate %v", got, approximate)
			}
		})
	}
}

func TestOpenCodeV2CreationEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, tool, input, native, legacy string
		want                              bool
	}{
		{name: "native create", tool: "subagent", input: `{}`, native: "child", want: true},
		{name: "native continue", tool: "subagent", input: `{"sessionID":"child"}`, native: "child"},
		{name: "migrated create", tool: "task", input: `{}`, legacy: "child", want: true},
		{name: "migrated continue", tool: "task", input: `{"task_id":"child"}`, legacy: "child"},
		{name: "wrong tool", tool: "shell", input: `{}`, native: "child"},
		{name: "wrong case", tool: "subagent", input: `{}`, legacy: "child"},
		{name: "missing input", tool: "subagent", native: "child"},
		{name: "streaming input", tool: "subagent", input: `"{}"`, native: "child"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			block := sessiondb.V2Content{Type: "tool", Name: tc.tool, State: sessiondb.V2ToolState{Input: json.RawMessage(tc.input), Metadata: sessiondb.V2ToolMetadata{SessionID: tc.native, LegacySessionID: tc.legacy}}}
			if got := opencodeV2CreatesChild(block, "child"); got != tc.want {
				t.Fatalf("creation=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestOpenCodeV2FixtureDeterministic(t *testing.T) {
	db := loadOpenCodeV2Fixture(t)
	if first, second := openCodeV2TurnsForTest(t, db, "root"), openCodeV2TurnsForTest(t, db, "root"); !reflect.DeepEqual(first, second) {
		t.Fatal("repeated projection import changed")
	}
}
