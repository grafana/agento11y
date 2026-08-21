package history

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
)

// pi session conformance: the Go importer and the TypeScript live plugin are
// checked against one shared fixture group, conformance/pi-sessions/.
//
// Every other importer proves it agrees with live capture by running the
// agent's own Go live mapper and comparing turn for turn (see
// TestClaudeTurnsMatchTheLiveMapper). pi has no Go live mapper, so the fixture
// is the contract instead: the same session inputs go through this importer
// here and through the pi plugin in plugins/pi/src/piSessionConformance.test.ts,
// and both normalize to the shape in generations.json.
//
// conformance/pi-sessions/README.md documents the encodings, the ${PLACEHOLDER}
// rule, and every field that cannot agree, with the reason.

const piConformanceDir = "../../../../conformance/pi-sessions"

type piFixtureCase struct {
	ID          string                       `json:"id"`
	Description string                       `json:"description"`
	SessionFile string                       `json:"session_file"`
	Files       map[string][]json.RawMessage `json:"files"`
}

type piFixtureSessions struct {
	Cases []piFixtureCase `json:"cases"`
}

func loadPiFixtureSessions(t *testing.T) piFixtureSessions {
	t.Helper()
	var out piFixtureSessions
	piReadFixture(t, "sessions.json", &out)
	if len(out.Cases) == 0 {
		t.Fatal("conformance/pi-sessions/sessions.json holds no cases")
	}
	return out
}

// loadPiFixtureGenerations returns the expected generations per case ID, parsed
// as plain JSON so the comparison is structural rather than tied to either
// language's types.
func loadPiFixtureGenerations(t *testing.T) map[string][]any {
	t.Helper()
	var doc struct {
		Cases map[string][]any `json:"cases"`
	}
	piReadFixture(t, "generations.json", &doc)
	if len(doc.Cases) == 0 {
		t.Fatal("conformance/pi-sessions/generations.json holds no cases")
	}
	return doc.Cases
}

func piReadFixture(t *testing.T, name string, into any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(piConformanceDir, name))
	if err != nil {
		t.Fatalf("read conformance/pi-sessions/%s: %v", name, err)
	}
	if err := json.Unmarshal(data, into); err != nil {
		t.Fatalf("decode conformance/pi-sessions/%s: %v", name, err)
	}
}

// materializePiFixtureCase writes a case's session files into dir, one JSON
// entry per line, and returns the path of the file under test. ${DIR} in an
// entry is replaced with dir, which is how a fork's header names its trunk
// without the fixture carrying a machine-specific path.
func materializePiFixtureCase(t *testing.T, dir string, fixture piFixtureCase) string {
	t.Helper()
	dirLiteral := piJSONStringBody(t, dir)
	for name, entries := range fixture.Files {
		var body strings.Builder
		for _, entry := range entries {
			var compact bytes.Buffer
			if err := json.Compact(&compact, entry); err != nil {
				t.Fatalf("case %s: compact entry: %v", fixture.ID, err)
			}
			body.WriteString(strings.ReplaceAll(compact.String(), "${DIR}", dirLiteral))
			body.WriteString("\n")
		}
		writeFile(t, filepath.Join(dir, name), body.String())
	}
	path := filepath.Join(dir, fixture.SessionFile)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("case %s: session_file %q is not one of the case's files", fixture.ID, fixture.SessionFile)
	}
	return path
}

// piJSONStringBody encodes s as the inside of a JSON string, so a substitution
// into already-encoded JSON stays valid: a Windows path's backslashes would
// otherwise be read back as escape sequences.
func piJSONStringBody(t *testing.T, s string) string {
	t.Helper()
	quoted, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("encode %q as JSON: %v", s, err)
	}
	return string(quoted[1 : len(quoted)-1])
}

// The normalized generation shape both languages produce. It holds what the
// session log decides and leaves out what only a live process knows; see the
// fixture README.
type piNormalizedGeneration struct {
	ID                  string                `json:"id"`
	ConversationID      string                `json:"conversation_id"`
	ConversationTitle   string                `json:"conversation_title"`
	Model               piNormalizedModel     `json:"model"`
	ResponseID          string                `json:"response_id"`
	ResponseModel       string                `json:"response_model"`
	Mode                string                `json:"mode"`
	OperationName       string                `json:"operation_name"`
	StopReason          string                `json:"stop_reason"`
	ThinkingEnabled     bool                  `json:"thinking_enabled"`
	Usage               piNormalizedUsage     `json:"usage"`
	StartedAt           string                `json:"started_at"`
	CompletedAt         string                `json:"completed_at"`
	Input               []piNormalizedMessage `json:"input"`
	Output              []piNormalizedMessage `json:"output"`
	Tools               []piNormalizedTool    `json:"tools"`
	CostUSD             *float64              `json:"cost_usd"`
	CallError           string                `json:"call_error"`
	ParentGenerationIDs []string              `json:"parent_generation_ids"`
	Fork                *piNormalizedFork     `json:"fork"`
}

type piNormalizedModel struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
}

type piNormalizedUsage struct {
	InputTokens           int64 `json:"input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	TotalTokens           int64 `json:"total_tokens"`
	CacheReadInputTokens  int64 `json:"cache_read_input_tokens"`
	CacheWriteInputTokens int64 `json:"cache_write_input_tokens"`
	ReasoningTokens       int64 `json:"reasoning_tokens"`
}

type piNormalizedMessage struct {
	Role  string             `json:"role"`
	Parts []piNormalizedPart `json:"parts"`
}

type piNormalizedPart struct {
	Kind       string                  `json:"kind"`
	Text       string                  `json:"text,omitempty"`
	Thinking   string                  `json:"thinking,omitempty"`
	ToolCall   *piNormalizedToolCall   `json:"tool_call,omitempty"`
	ToolResult *piNormalizedToolResult `json:"tool_result,omitempty"`
}

type piNormalizedToolCall struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Arguments is the parsed tool input, not a string: the two sides encode
	// it differently on the wire (Go keeps embedded JSON, the JS SDK exports
	// base64), and only the decoded value is comparable.
	Arguments json.RawMessage `json:"arguments"`
}

type piNormalizedToolResult struct {
	ToolCallID string `json:"tool_call_id"`
	Name       string `json:"name"`
	Content    string `json:"content"`
	IsError    bool   `json:"is_error"`
}

type piNormalizedFork struct {
	ParentSessionID    string `json:"parent_session_id"`
	ParentGenerationID string `json:"parent_generation_id"`
}

// normalizePiHistoricalGeneration projects one imported turn into the shared
// shape.
func normalizePiHistoricalGeneration(turn HistoricalGeneration) piNormalizedGeneration {
	gen := turn.Gen
	out := piNormalizedGeneration{
		ID:                  gen.ID,
		ConversationID:      gen.ConversationID,
		ConversationTitle:   gen.ConversationTitle,
		Model:               piNormalizedModel{Provider: gen.Model.Provider, Name: gen.Model.Name},
		ResponseID:          gen.ResponseID,
		ResponseModel:       gen.ResponseModel,
		Mode:                strings.TrimPrefix(string(gen.Mode), "GENERATION_MODE_"),
		OperationName:       gen.OperationName,
		StopReason:          gen.StopReason,
		ThinkingEnabled:     gen.ThinkingEnabled != nil && *gen.ThinkingEnabled,
		StartedAt:           piFormatTime(gen.StartedAt),
		CompletedAt:         piFormatTime(gen.CompletedAt),
		Input:               normalizePiMessages(gen.Input),
		Output:              normalizePiMessages(gen.Output),
		Tools:               []piNormalizedTool{},
		CallError:           gen.CallError,
		ParentGenerationIDs: gen.ParentGenerationIDs,
		Usage: piNormalizedUsage{
			InputTokens:           gen.Usage.InputTokens,
			OutputTokens:          gen.Usage.OutputTokens,
			TotalTokens:           gen.Usage.TotalTokens,
			CacheReadInputTokens:  gen.Usage.CacheReadInputTokens,
			CacheWriteInputTokens: gen.Usage.CacheWriteInputTokens,
			ReasoningTokens:       gen.Usage.ReasoningTokens,
		},
	}
	if out.ParentGenerationIDs == nil {
		out.ParentGenerationIDs = []string{}
	}
	for _, tool := range gen.Tools {
		out.Tools = append(out.Tools, piNormalizedTool{Name: tool.Name})
	}
	if cost, ok := gen.Metadata["cost_usd"].(float64); ok {
		out.CostUSD = &cost
	}
	if session, ok := gen.Metadata[MetaPiForkParentSession].(string); ok {
		parentGen, _ := gen.Metadata[MetaPiForkParentGeneration].(string)
		out.Fork = &piNormalizedFork{ParentSessionID: session, ParentGenerationID: parentGen}
	}
	return out
}

type piNormalizedTool struct {
	Name string `json:"name"`
}

// piFormatTime spells a timestamp the way the fixture does, and leaves an unset
// one empty. Formatting a zero time would give 0001-01-01T00:00:00.000Z, which
// the placeholder check accepts because it is non-empty.
func piFormatTime(ts time.Time) string {
	if ts.IsZero() {
		return ""
	}
	return ts.UTC().Format("2006-01-02T15:04:05.000Z")
}

func normalizePiMessages(msgs []agento11y.Message) []piNormalizedMessage {
	out := []piNormalizedMessage{}
	for _, msg := range msgs {
		normalized := piNormalizedMessage{Role: string(msg.Role), Parts: []piNormalizedPart{}}
		for _, part := range msg.Parts {
			normalized.Parts = append(normalized.Parts, normalizePiPart(part))
		}
		out = append(out, normalized)
	}
	return out
}

func normalizePiPart(part agento11y.Part) piNormalizedPart {
	out := piNormalizedPart{Kind: string(part.Kind), Text: part.Text, Thinking: part.Thinking}
	if part.ToolCall != nil {
		args := part.ToolCall.InputJSON
		if len(bytes.TrimSpace(args)) == 0 {
			args = json.RawMessage("{}")
		}
		out.ToolCall = &piNormalizedToolCall{
			ID:        part.ToolCall.ID,
			Name:      part.ToolCall.Name,
			Arguments: args,
		}
	}
	if part.ToolResult != nil {
		out.ToolResult = &piNormalizedToolResult{
			ToolCallID: part.ToolResult.ToolCallID,
			Name:       part.ToolResult.Name,
			Content:    part.ToolResult.Content,
			IsError:    part.ToolResult.IsError,
		}
	}
	return out
}

// piNormalizedJSON re-parses a normalized generation as plain JSON, so the
// comparison sees the same value shapes the fixture parses to.
func piNormalizedJSON(t *testing.T, gen piNormalizedGeneration) any {
	t.Helper()
	data, err := json.Marshal(gen)
	if err != nil {
		t.Fatalf("marshal normalized generation: %v", err)
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("decode normalized generation: %v", err)
	}
	return out
}

func TestPiSessionConformance(t *testing.T) {
	sessions := loadPiFixtureSessions(t)
	expected := loadPiFixtureGenerations(t)

	for _, fixture := range sessions.Cases {
		t.Run(fixture.ID, func(t *testing.T) {
			want, ok := expected[fixture.ID]
			if !ok {
				t.Fatalf("conformance/pi-sessions/generations.json has no case %q", fixture.ID)
			}

			dir := t.TempDir()
			path := materializePiFixtureCase(t, dir, fixture)
			imp := piImporterAt(dir)
			turns := collectTurns(t, imp, piPreview(t, imp, path))
			if len(turns) != len(want) {
				t.Fatalf("imported %d generations, the fixture expects %d", len(turns), len(want))
			}
			for i, turn := range turns {
				got := piNormalizedJSON(t, normalizePiHistoricalGeneration(turn))
				for _, diff := range piDiffJSON("", got, want[i]) {
					t.Errorf("generation %d does not match conformance/pi-sessions/generations.json: %s", i, diff)
				}
			}
		})
	}
}

// TestPiSessionFixtureComparisonNamesDivergentFields pins the comparator, not
// the importer. TestPiSessionConformance is what checks the importer's own
// output. Each case takes a real imported generation, applies one divergence
// the fixture cannot accept, and asserts the diff names the offending path.
// Without this test a comparator that ignored a field would keep every
// conformance case green while checking nothing.
func TestPiSessionFixtureComparisonNamesDivergentFields(t *testing.T) {
	sessions := loadPiFixtureSessions(t)
	expected := loadPiFixtureGenerations(t)

	const caseID = "tool-call-turn"
	var fixture piFixtureCase
	for _, candidate := range sessions.Cases {
		if candidate.ID == caseID {
			fixture = candidate
		}
	}
	if fixture.ID == "" {
		t.Fatalf("fixture case %q is gone; this test needs a turn with usage and tool traffic", caseID)
	}

	dir := t.TempDir()
	path := materializePiFixtureCase(t, dir, fixture)
	imp := piImporterAt(dir)
	turns := collectTurns(t, imp, piPreview(t, imp, path))
	if len(turns) == 0 {
		t.Fatal("the fixture case imported no turns")
	}
	baseline := normalizePiHistoricalGeneration(turns[0])
	want := expected[caseID][0]

	tests := []struct {
		name string
		// mutate changes the normalized generation. mutateTurn changes the
		// imported turn instead, which is the only way to reach a divergence
		// the normalizer itself could hide.
		mutate     func(gen *piNormalizedGeneration)
		mutateTurn func(turn *HistoricalGeneration)
		wantPath   string
	}{
		{
			// The one usage difference that would look harmless: recomputing
			// totalTokens as input+output, which is what the Go launchers do
			// and what mapPiUsage deliberately does not.
			name: "total tokens recomputed the Go launcher way",
			mutate: func(gen *piNormalizedGeneration) {
				gen.Usage.TotalTokens = gen.Usage.InputTokens + gen.Usage.OutputTokens
			},
			wantPath: "usage.total_tokens",
		},
		{
			name: "stop reason passed through unmapped",
			mutate: func(gen *piNormalizedGeneration) {
				gen.StopReason = "toolUse"
			},
			wantPath: "stop_reason",
		},
		{
			name: "tool call arguments re-encoded as a string",
			mutate: func(gen *piNormalizedGeneration) {
				part := piMutablePart(t, gen, 1, 0)
				quoted, err := json.Marshal(string(part.ToolCall.Arguments))
				if err != nil {
					t.Fatalf("marshal arguments: %v", err)
				}
				part.ToolCall.Arguments = quoted
			},
			wantPath: "output[1].parts[0].tool_call.arguments",
		},
		{
			name: "thinking text dropped from the output",
			mutate: func(gen *piNormalizedGeneration) {
				gen.Output = append(gen.Output[:0:0], gen.Output[1:]...)
			},
			wantPath: "output",
		},
		{
			// A placeholder says the value cannot agree, not that it may be
			// missing: an empty one still has to fail.
			name: "placeholder field left empty",
			mutate: func(gen *piNormalizedGeneration) {
				gen.StartedAt = ""
			},
			wantPath: "started_at",
		},
		{
			// Same rule for a timestamp the importer never set: a zero time
			// is an absent value, not a value that cannot agree.
			name: "start timestamp left unset",
			mutateTurn: func(turn *HistoricalGeneration) {
				turn.Gen.StartedAt = time.Time{}
			},
			wantPath: "started_at",
		},
		{
			name: "completion timestamp left unset",
			mutateTurn: func(turn *HistoricalGeneration) {
				turn.Gen.CompletedAt = time.Time{}
			},
			wantPath: "completed_at",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var mutated piNormalizedGeneration
			if tc.mutateTurn != nil {
				turn := turns[0]
				tc.mutateTurn(&turn)
				mutated = normalizePiHistoricalGeneration(turn)
			} else {
				mutated = piCopyNormalizedGeneration(baseline)
				tc.mutate(&mutated)
			}

			diffs := piDiffJSON("", piNormalizedJSON(t, mutated), want)
			if len(diffs) == 0 {
				t.Fatalf("the comparator accepted a %s", tc.name)
			}
			named := false
			for _, diff := range diffs {
				if strings.HasPrefix(diff, tc.wantPath) {
					named = true
				}
			}
			if !named {
				t.Fatalf("diffs %v name no path starting with %q", diffs, tc.wantPath)
			}
		})
	}
}

// piCopyNormalizedGeneration copies a normalized generation deeply enough that
// one case's mutation cannot reach another case's baseline.
func piCopyNormalizedGeneration(gen piNormalizedGeneration) piNormalizedGeneration {
	out := gen
	out.Output = append([]piNormalizedMessage(nil), gen.Output...)
	for i := range out.Output {
		out.Output[i].Parts = append([]piNormalizedPart(nil), gen.Output[i].Parts...)
		for j, part := range out.Output[i].Parts {
			if part.ToolCall != nil {
				call := *part.ToolCall
				out.Output[i].Parts[j].ToolCall = &call
			}
		}
	}
	return out
}

func piMutablePart(t *testing.T, gen *piNormalizedGeneration, msg, part int) *piNormalizedPart {
	t.Helper()
	if len(gen.Output) <= msg || len(gen.Output[msg].Parts) <= part {
		t.Fatalf("normalized generation has no output message %d part %d", msg, part)
	}
	return &gen.Output[msg].Parts[part]
}

// piPlaceholderPattern marks a value that cannot agree across the two
// implementations, such as the generation ID scheme or a timestamp only a live
// process observes. A placeholder still requires a non-empty value: the field
// has to be there, only its content is unpinned. See the fixture README.
func piIsPlaceholder(want any) bool {
	text, ok := want.(string)
	return ok && strings.HasPrefix(text, "${") && strings.HasSuffix(text, "}")
}

// piDiffJSON reports every structural difference between got and want as a
// dotted JSON path plus the two values, so a failure names the offending field
// instead of dumping two payloads. It is the Go half of the comparator the
// TypeScript suite ports; both must treat ${PLACEHOLDER} the same way.
func piDiffJSON(path string, got, want any) []string {
	if piIsPlaceholder(want) {
		text, ok := got.(string)
		if !ok || strings.TrimSpace(text) == "" {
			return []string{fmt.Sprintf("%s: got %s, want a non-empty value for placeholder %s",
				piDiffPath(path), piDiffValue(got), piDiffValue(want))}
		}
		return nil
	}

	switch wantTyped := want.(type) {
	case map[string]any:
		gotTyped, ok := got.(map[string]any)
		if !ok {
			return []string{fmt.Sprintf("%s: got %s, want an object", piDiffPath(path), piDiffValue(got))}
		}
		var diffs []string
		for _, key := range piSortedKeys(wantTyped, gotTyped) {
			gotValue, gotHas := gotTyped[key]
			wantValue, wantHas := wantTyped[key]
			childPath := key
			if path != "" {
				childPath = path + "." + key
			}
			switch {
			case !gotHas:
				diffs = append(diffs, fmt.Sprintf("%s: missing, want %s", childPath, piDiffValue(wantValue)))
			case !wantHas:
				diffs = append(diffs, fmt.Sprintf("%s: unexpected %s", childPath, piDiffValue(gotValue)))
			default:
				diffs = append(diffs, piDiffJSON(childPath, gotValue, wantValue)...)
			}
		}
		return diffs
	case []any:
		gotTyped, ok := got.([]any)
		if !ok {
			return []string{fmt.Sprintf("%s: got %s, want an array", piDiffPath(path), piDiffValue(got))}
		}
		if len(gotTyped) != len(wantTyped) {
			return []string{fmt.Sprintf("%s: got %d items, want %d", piDiffPath(path), len(gotTyped), len(wantTyped))}
		}
		var diffs []string
		for i := range wantTyped {
			diffs = append(diffs, piDiffJSON(fmt.Sprintf("%s[%d]", path, i), gotTyped[i], wantTyped[i])...)
		}
		return diffs
	default:
		if !reflect.DeepEqual(got, want) {
			return []string{fmt.Sprintf("%s: got %s, want %s", piDiffPath(path), piDiffValue(got), piDiffValue(want))}
		}
		return nil
	}
}

func piDiffPath(path string) string {
	if path == "" {
		return "<root>"
	}
	return path
}

func piDiffValue(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	return string(data)
}

func piSortedKeys(maps ...map[string]any) []string {
	seen := map[string]struct{}{}
	for _, m := range maps {
		for key := range m {
			seen[key] = struct{}{}
		}
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
