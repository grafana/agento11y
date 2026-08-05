package mapper

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/grafana/agento11y/go/agento11y"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/fragment"
	"github.com/grafana/agento11y/plugins/agento11y/internal/redact"
)

// Secrets reused across the redaction tests: a tier-1 token and a value
// under a sensitive JSON key.
const (
	testToken  = "glc_abcdefghijklmnopqrstuvwxyz"
	testAPIKey = "kR7fQ2wLmZ9xTb4vNc1JhY6s"
)

// fixedTime gives every test a deterministic "now".
var fixedTime = time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)

func basicFragment(t *testing.T) *fragment.Fragment {
	t.Helper()
	return &fragment.Fragment{
		ConversationID:  "conv-1",
		GenerationID:    "gen-1",
		UserPrompt:      "hello",
		Assistant:       []fragment.AssistantSegment{{Text: "hi there"}},
		Tools:           []fragment.ToolRecord{{ToolName: "Read", ToolUseID: "t1", ToolInput: json.RawMessage(`{"path":"x"}`), ToolOutput: json.RawMessage(`"contents"`), Status: "completed", Cwd: "/repo"}},
		Model:           "claude-sonnet-4-6",
		Provider:        "anthropic",
		StartedAt:       "2026-04-28T11:59:00Z",
		LastEventAt:     "2026-04-28T12:00:30Z",
		ThinkingPresent: true,
	}
}

// TestMapFragment_BasicFields covers the non-content-capture-dependent
// fields that MapFragment populates from a fragment.
func TestMapFragment_BasicFields(t *testing.T) {
	got := MapFragment(Inputs{
		Fragment:       basicFragment(t),
		ContentCapture: agento11y.ContentCaptureModeFull,
		Now:            fixedTime,
	})

	if got.StopStatus != StopStatusCompleted {
		t.Errorf("StopStatus = %v; want completed", got.StopStatus)
	}
	if got.Generation.Model.Provider != "anthropic" || got.Generation.Model.Name != "claude-sonnet-4-6" {
		t.Errorf("Model = %+v; want anthropic/claude-sonnet-4-6", got.Generation.Model)
	}
	if got.Generation.ThinkingEnabled == nil || !*got.Generation.ThinkingEnabled {
		t.Errorf("ThinkingEnabled = %v; want true", got.Generation.ThinkingEnabled)
	}
}

// TestMapFragment_ContentCaptureModes covers what every supported
// ContentCaptureMode includes or strips in the gRPC payload that
// buildMessages produces.
//
//   - Full and FullWithMetadataSpans carry full content; they only diverge
//     in what the OTel span exposes, which is decided elsewhere.
//   - NoToolContent keeps the tool_call/tool_result skeleton but strips
//     argument and result bytes.
//   - MetadataOnly drops user prompts, assistant text, and the tool_result
//     message entirely.
//   - Default is the zero-value enum. envconfig.ResolveContentMode maps it
//     to MetadataOnly, but a caller that bypasses the config layer (or
//     constructs Inputs directly in tests) might pass it through, so
//     buildMessages re-applies the same mapping defensively.
//
// Every content field carries a secret, so each case also asserts that the
// mode exports no raw secret and that the redaction marker appears exactly
// in the modes that keep content.
func TestMapFragment_ContentCaptureModes(t *testing.T) {
	cases := []struct {
		name            string
		mode            agento11y.ContentCaptureMode
		wantUserPrompt  bool
		wantAssistant   bool
		wantToolInput   bool
		wantToolResult  bool // tool_result message present in input
		wantToolContent bool // tool_result carries ContentJSON or Content
	}{
		{"full", agento11y.ContentCaptureModeFull, true, true, true, true, true},
		{"full_with_metadata_spans", agento11y.ContentCaptureModeFullWithMetadataSpans, true, true, true, true, true},
		{"no_tool_content", agento11y.ContentCaptureModeNoToolContent, true, true, false, true, false},
		{"metadata_only", agento11y.ContentCaptureModeMetadataOnly, false, false, false, false, false},
		{"default", agento11y.ContentCaptureModeDefault, false, false, false, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			frag := basicFragment(t)
			frag.UserPrompt = "hello " + testToken
			frag.Assistant = []fragment.AssistantSegment{{Text: "hi there " + testToken}}
			frag.Tools[0].ToolInput = json.RawMessage(`{"path":"x","api_key":"` + testAPIKey + `"}`)
			frag.Tools[0].ToolOutput = json.RawMessage(`"contents ` + testToken + `"`)

			got := MapFragment(Inputs{
				Fragment:       frag,
				ContentCapture: tc.mode,
				Now:            fixedTime,
			})

			var gotPrompt, gotAssistant, gotToolInput, gotToolResult, gotToolContent bool
			for _, msg := range got.Generation.Input {
				for _, p := range msg.Parts {
					if strings.HasPrefix(p.Text, "hello") {
						gotPrompt = true
					}
				}
				if msg.Role == agento11y.RoleTool {
					gotToolResult = true
					for _, p := range msg.Parts {
						if p.ToolResult != nil && (p.ToolResult.Content != "" || len(p.ToolResult.ContentJSON) > 0) {
							gotToolContent = true
						}
					}
				}
			}
			for _, msg := range got.Generation.Output {
				for _, p := range msg.Parts {
					if strings.HasPrefix(p.Text, "hi there") {
						gotAssistant = true
					}
					if p.ToolCall != nil && len(p.ToolCall.InputJSON) > 0 {
						gotToolInput = true
					}
				}
			}

			if gotPrompt != tc.wantUserPrompt {
				t.Errorf("user prompt present = %v; want %v", gotPrompt, tc.wantUserPrompt)
			}
			if gotAssistant != tc.wantAssistant {
				t.Errorf("assistant text present = %v; want %v", gotAssistant, tc.wantAssistant)
			}
			if gotToolInput != tc.wantToolInput {
				t.Errorf("tool_call InputJSON present = %v; want %v", gotToolInput, tc.wantToolInput)
			}
			if gotToolResult != tc.wantToolResult {
				t.Errorf("tool_result message present = %v; want %v", gotToolResult, tc.wantToolResult)
			}
			if gotToolContent != tc.wantToolContent {
				t.Errorf("tool_result content present = %v; want %v", gotToolContent, tc.wantToolContent)
			}

			// ToolCall.InputJSON and ToolResult.ContentJSON are
			// json.RawMessage, so marshaling the generation keeps them
			// readable and one scan covers every content field.
			body, err := json.Marshal(got.Generation)
			if err != nil {
				t.Fatalf("marshal generation: %v", err)
			}
			for _, secret := range []string{testToken, testAPIKey} {
				if bytes.Contains(body, []byte(secret)) {
					t.Errorf("mode %s leaks raw secret %q: %s", tc.mode, secret, body)
				}
			}
			// The prompt carries the tier-1 token, so the marker is visible
			// in exactly the modes that keep the prompt.
			if marked := bytes.Contains(body, []byte("[REDACTED:")); marked != tc.wantUserPrompt {
				t.Errorf("redaction marker present = %v; want %v: %s", marked, tc.wantUserPrompt, body)
			}
		})
	}
}

// TestMapFragment_RedactsSecrets covers the redaction boundary: every
// content field the mapper exports in full mode goes through the shared
// redactor, and nothing else in the generation moves.
func TestMapFragment_RedactsSecrets(t *testing.T) {
	const token = testToken
	const apiKey = testAPIKey

	in, out := int64(80), int64(22)
	frag := &fragment.Fragment{
		ConversationID: "conv-1",
		GenerationID:   "gen-1",
		UserPrompt:     "deploy using " + token,
		Assistant:      []fragment.AssistantSegment{{Text: "I used " + token}},
		Tools: []fragment.ToolRecord{{
			ToolName:   "Bash",
			ToolUseID:  "t1",
			ToolInput:  json.RawMessage(`{"command":"deploy.sh","api_key":"` + apiKey + `"}`),
			ToolOutput: json.RawMessage(`{"stdout":"authenticated with ` + token + `"}`),
			Status:     "completed",
			Cwd:        "/repo/" + token,
		}},
		Model:      "gpt-5-cursor",
		Provider:   "openai",
		TokenUsage: &fragment.TokenCounts{InputTokens: &in, OutputTokens: &out},
	}

	got := MapFragment(Inputs{
		Fragment:       frag,
		Session:        &fragment.Session{ConversationTitle: "deploy using " + token},
		ContentCapture: agento11y.ContentCaptureModeFull,
		Now:            fixedTime,
	})

	var userText, assistantText, toolInput, toolOutput string
	for _, msg := range got.Generation.Input {
		for _, p := range msg.Parts {
			if msg.Role == agento11y.RoleUser && p.Kind == agento11y.PartKindText {
				userText = p.Text
			}
			if p.ToolResult != nil {
				toolOutput = string(p.ToolResult.ContentJSON)
			}
		}
	}
	for _, msg := range got.Generation.Output {
		for _, p := range msg.Parts {
			if p.Kind == agento11y.PartKindText {
				assistantText = p.Text
			}
			if p.ToolCall != nil {
				toolInput = string(p.ToolCall.InputJSON)
			}
		}
	}

	fields := []struct {
		name     string
		value    string
		wantMark string
	}{
		{"conversation_title", got.Generation.ConversationTitle, "[REDACTED:grafana-cloud-token]"},
		{"start conversation_title", got.Start.ConversationTitle, "[REDACTED:grafana-cloud-token]"},
		{"user prompt", userText, "[REDACTED:grafana-cloud-token]"},
		{"assistant text", assistantText, "[REDACTED:grafana-cloud-token]"},
		{"tool_call input", toolInput, "[REDACTED:json-secret-field]"},
		{"tool_result content", toolOutput, "[REDACTED:grafana-cloud-token]"},
	}
	for _, f := range fields {
		t.Run(f.name, func(t *testing.T) {
			if f.value == "" {
				t.Fatalf("%s is empty; expected redacted content", f.name)
			}
			if strings.Contains(f.value, token) {
				t.Errorf("%s leaks the raw token: %s", f.name, f.value)
			}
			if strings.Contains(f.value, apiKey) {
				t.Errorf("%s leaks the raw api_key: %s", f.name, f.value)
			}
			if !strings.Contains(f.value, f.wantMark) {
				t.Errorf("%s = %s; want it to contain %s", f.name, f.value, f.wantMark)
			}
		})
	}

	t.Run("tool json stays structurally valid", func(t *testing.T) {
		var input map[string]any
		if err := json.Unmarshal([]byte(toolInput), &input); err != nil {
			t.Fatalf("tool_call input is not valid JSON: %v (%s)", err, toolInput)
		}
		if input["command"] != "deploy.sh" {
			t.Errorf("non-secret tool argument changed: command = %v; want deploy.sh", input["command"])
		}
		var output map[string]any
		if err := json.Unmarshal([]byte(toolOutput), &output); err != nil {
			t.Fatalf("tool_result content is not valid JSON: %v (%s)", err, toolOutput)
		}
	})

	t.Run("metadata untouched", func(t *testing.T) {
		if len(got.Generation.Tools) != 1 || got.Generation.Tools[0].Name != "Bash" {
			t.Errorf("tool definitions = %+v; want a single Bash entry", got.Generation.Tools)
		}
		if got.Generation.Usage.InputTokens != 80 || got.Generation.Usage.OutputTokens != 22 {
			t.Errorf("usage = %+v; want 80/22", got.Generation.Usage)
		}
		// A secret-shaped cwd proves the tag path skips the redactor:
		// redaction touches content, not metadata.
		if want := "/repo/" + token; got.Generation.Tags["cwd"] != want {
			t.Errorf("cwd tag = %q; want %q", got.Generation.Tags["cwd"], want)
		}
		if got.Generation.ID != "gen-1" || got.Generation.ConversationID != "conv-1" {
			t.Errorf("ids = %q/%q; want gen-1/conv-1", got.Generation.ID, got.Generation.ConversationID)
		}
		if got.Generation.Model.Name != "gpt-5-cursor" || got.Generation.Model.Provider != "openai" {
			t.Errorf("model = %+v; want openai/gpt-5-cursor", got.Generation.Model)
		}
	})
}

// TestMapFragment_RedactionTiers pins the per-field tier split. The title
// gets tier 1 only; the prompt and assistant text get tier 2 as well.
// Without these cases either half can be swapped for the other with no test
// turning red, because every other secret in the package is a tier-1 token
// or a sensitive JSON key.
func TestMapFragment_RedactionTiers(t *testing.T) {
	cases := []struct {
		name       string
		title      string
		text       string
		wantTitle  string
		wantPrompt string
	}{
		{
			// Tier 2's `KEY: value` heuristic would swallow the rest of the
			// line. Cursor titles are truncated first prompts, so ordinary
			// text like this is the common case.
			name:       "title survives the tier 2 heuristic",
			title:      "fix the API key: rotation script",
			text:       "hello",
			wantTitle:  "fix the API key: rotation script",
			wantPrompt: "hello",
		},
		{
			name:       "tier 1 token is redacted everywhere",
			title:      "deploy using " + testToken,
			text:       "deploy using " + testToken,
			wantTitle:  "deploy using [REDACTED:grafana-cloud-token]",
			wantPrompt: "deploy using [REDACTED:grafana-cloud-token]",
		},
		{
			name:       "prompt and assistant text get tier 2",
			title:      "run the deploy",
			text:       "run with PASSWORD=hunter2",
			wantTitle:  "run the deploy",
			wantPrompt: "run with PASSWORD=[REDACTED:env-secret-value]",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			frag := basicFragment(t)
			frag.UserPrompt = tc.text
			frag.Assistant = []fragment.AssistantSegment{{Text: tc.text}}

			got := MapFragment(Inputs{
				Fragment:       frag,
				Session:        &fragment.Session{ConversationTitle: tc.title},
				ContentCapture: agento11y.ContentCaptureModeFull,
				Now:            fixedTime,
			})

			if got.Generation.ConversationTitle != tc.wantTitle {
				t.Errorf("title = %q; want %q", got.Generation.ConversationTitle, tc.wantTitle)
			}
			var prompt, assistant string
			for _, msg := range got.Generation.Input {
				for _, p := range msg.Parts {
					if msg.Role == agento11y.RoleUser && p.Kind == agento11y.PartKindText {
						prompt = p.Text
					}
				}
			}
			for _, msg := range got.Generation.Output {
				for _, p := range msg.Parts {
					if p.Kind == agento11y.PartKindText {
						assistant = p.Text
					}
				}
			}
			if prompt != tc.wantPrompt {
				t.Errorf("prompt = %q; want %q", prompt, tc.wantPrompt)
			}
			if assistant != tc.wantPrompt {
				t.Errorf("assistant text = %q; want %q", assistant, tc.wantPrompt)
			}
		})
	}
}

func TestResolveStopStatus(t *testing.T) {
	cases := []struct {
		in   string
		want StopStatus
	}{
		{"", StopStatusCompleted},
		{"completed", StopStatusCompleted},
		{"success", StopStatusCompleted},
		{"ok", StopStatusCompleted},
		{"aborted", StopStatusAborted},
		{"cancelled", StopStatusAborted},
		{"canceled", StopStatusAborted},
		{"ABORTED", StopStatusAborted},
		{"error", StopStatusError},
		{"failed", StopStatusError},
		{"  ERROR  ", StopStatusError},
		{"unknown_value", StopStatusCompleted},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := resolveStopStatus(&StopInput{Status: tc.in})
			if got != tc.want {
				t.Errorf("resolveStopStatus(%q) = %v; want %v", tc.in, got, tc.want)
			}
		})
	}
	if got := resolveStopStatus(nil); got != StopStatusCompleted {
		t.Errorf("nil StopInput should resolve to completed; got %v", got)
	}
}

// Cursor infers the provider via the shared mapperutil.InferProvider helper.
// MapFragment must keep wiring it into Model.Provider for the documented cases;
// the helper's own edge cases are covered in internal/mapperutil.
func TestMapFragment_InfersProviderFromModel(t *testing.T) {
	cases := []struct{ model, want string }{
		{"claude-sonnet-4-6", "anthropic"},
		{"gpt-5", "openai"},
		{"o3-mini", "openai"},
		{"gemini-2.5-pro", "google"},
		{"some-random-model", "cursor"}, // no match → cursor fallback
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			frag := &fragment.Fragment{ConversationID: "c", GenerationID: "g", Model: tc.model}
			got := MapFragment(Inputs{Fragment: frag, ContentCapture: agento11y.ContentCaptureModeMetadataOnly, Now: fixedTime})
			if got.Generation.Model.Provider != tc.want {
				t.Errorf("provider for model %q = %q; want %q", tc.model, got.Generation.Model.Provider, tc.want)
			}
		})
	}
}

func TestResolveUserID(t *testing.T) {
	cases := []struct {
		name         string
		override     string
		payloadEmail string
		want         string
	}{
		{"override wins", "alice@example.com", "bob@example.com", "alice@example.com"},
		{"override trimmed", "  alice@example.com\t", "bob@example.com", "alice@example.com"},
		{"falls back to payload email", "", "bob@example.com", "bob@example.com"},
		{"payload email trimmed", "", "  bob@example.com  ", "bob@example.com"},
		{"whitespace override falls through", "   ", "bob@example.com", "bob@example.com"},
		{"both empty", "", "", ""},
		{"both whitespace-only", "  ", "\t", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveUserID(tc.override, tc.payloadEmail); got != tc.want {
				t.Fatalf("resolveUserID(%q, %q) = %q; want %q",
					tc.override, tc.payloadEmail, got, tc.want)
			}
		})
	}
}

func TestMapFragment_MissingModelAndProvider_FallsBackToCursor(t *testing.T) {
	frag := &fragment.Fragment{
		ConversationID: "conv",
		GenerationID:   "gen",
	}
	got := MapFragment(Inputs{
		Fragment:       frag,
		ContentCapture: agento11y.ContentCaptureModeMetadataOnly,
		Now:            fixedTime,
	})
	if got.Generation.Model.Provider != "cursor" {
		t.Errorf("Provider = %q; want cursor", got.Generation.Model.Provider)
	}
	if got.Generation.Model.Name != "unknown" {
		t.Errorf("Name = %q; want unknown", got.Generation.Model.Name)
	}
}

func TestMapFragment_ConversationTitleFromSession(t *testing.T) {
	got := MapFragment(Inputs{
		Fragment: basicFragment(t),
		Session: &fragment.Session{
			ConversationID:    "conv-1",
			ConversationTitle: "list go files",
		},
		ContentCapture: agento11y.ContentCaptureModeMetadataOnly,
		Now:            fixedTime,
	})
	if got.Start.ConversationTitle != "list go files" {
		t.Errorf("Start.ConversationTitle = %q; want %q", got.Start.ConversationTitle, "list go files")
	}
	if got.Generation.ConversationTitle != "list go files" {
		t.Errorf("Generation.ConversationTitle = %q; want %q", got.Generation.ConversationTitle, "list go files")
	}
}

func TestMapFragment_ConversationTitleEmptyWithoutSession(t *testing.T) {
	got := MapFragment(Inputs{
		Fragment:       basicFragment(t),
		ContentCapture: agento11y.ContentCaptureModeMetadataOnly,
		Now:            fixedTime,
	})
	if got.Start.ConversationTitle != "" {
		t.Errorf("Start.ConversationTitle = %q; want empty", got.Start.ConversationTitle)
	}
	if got.Generation.ConversationTitle != "" {
		t.Errorf("Generation.ConversationTitle = %q; want empty", got.Generation.ConversationTitle)
	}
}

func TestMapFragment_BuiltinTags(t *testing.T) {
	got := MapFragment(Inputs{
		Fragment: basicFragment(t),
		Session: &fragment.Session{
			ConversationID: "conv-1",
			WorkspaceRoots: []string{"/no-such-dir-without-git"},
		},
		ContentCapture: agento11y.ContentCaptureModeMetadataOnly,
		Now:            fixedTime,
	})
	// No real .git → git.branch absent.
	if _, ok := got.Generation.Tags["git.branch"]; ok {
		t.Errorf("git.branch should be absent when no .git resolves; got %q",
			got.Generation.Tags["git.branch"])
	}
	if got.Generation.Tags["cwd"] != "/repo" {
		t.Errorf("cwd should come from first tool record; got %q", got.Generation.Tags["cwd"])
	}
}

func TestMapFragment_TokenUsage(t *testing.T) {
	in, out := int64(100), int64(50)
	frag := basicFragment(t)
	frag.TokenUsage = &fragment.TokenCounts{InputTokens: &in, OutputTokens: &out}

	got := MapFragment(Inputs{
		Fragment:       frag,
		ContentCapture: agento11y.ContentCaptureModeMetadataOnly,
		Now:            fixedTime,
	})
	if got.Generation.Usage.InputTokens != 100 {
		t.Errorf("InputTokens = %d; want 100", got.Generation.Usage.InputTokens)
	}
	if got.Generation.Usage.OutputTokens != 50 {
		t.Errorf("OutputTokens = %d; want 50", got.Generation.Usage.OutputTokens)
	}
	if got.Generation.Usage.TotalTokens != 150 {
		t.Errorf("TotalTokens = %d; want 150", got.Generation.Usage.TotalTokens)
	}
}

func TestExtractCallError(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want string
	}{
		{"nil error", nil, "cursor_stop_error"},
		{"empty bytes", []byte(""), "cursor_stop_error"},
		{"json string", []byte(`"boom"`), "boom"},
		{"json object with message", []byte(`{"message":"timeout","code":"E1"}`), "timeout"},
		{"json object missing message", []byte(`{"code":"E1"}`), "cursor_stop_error"},
		{"unparseable", []byte("garbage"), "cursor_stop_error"},
		{
			"secret in string is redacted",
			[]byte(`"auth failed for glc_abcdefghijklmnopqrstuvwxyz"`),
			"auth failed for [REDACTED:grafana-cloud-token]",
		},
		{
			"secret in object message is redacted",
			[]byte(`{"message":"auth failed for glc_abcdefghijklmnopqrstuvwxyz","code":"E1"}`),
			"auth failed for [REDACTED:grafana-cloud-token]",
		},
		{
			// Tier 1 only: a message like `retry limit: 3` must survive.
			"tier 2 heuristic does not fire",
			[]byte(`"request failed, retry limit: 3"`),
			"request failed, retry limit: 3",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractCallError(&StopInput{Error: tc.in}, redact.New())
			if got.Error() != tc.want {
				t.Errorf("got %q; want %q", got.Error(), tc.want)
			}
		})
	}
}

func TestMapFragment_StopStatusError_PopulatesCallError(t *testing.T) {
	got := MapFragment(Inputs{
		Fragment:       basicFragment(t),
		Stop:           &StopInput{Status: "error", Error: []byte(`"network failure"`)},
		ContentCapture: agento11y.ContentCaptureModeMetadataOnly,
		Now:            fixedTime,
	})
	if got.StopStatus != StopStatusError {
		t.Errorf("StopStatus = %v; want error", got.StopStatus)
	}
	if got.CallError == nil || got.CallError.Error() != "network failure" {
		t.Errorf("CallError = %v; want 'network failure'", got.CallError)
	}
}

func TestBuildToolDefinitions_DedupAndSort(t *testing.T) {
	tools := []fragment.ToolRecord{
		{ToolName: "Write"},
		{ToolName: "Read"},
		{ToolName: "Read"}, // dup
		{ToolName: ""},     // skipped
		{ToolName: "Bash"},
	}
	got := buildToolDefinitions(tools)
	if len(got) != 3 {
		t.Fatalf("got %d defs; want 3 (got %+v)", len(got), got)
	}
	wantNames := []string{"Bash", "Read", "Write"}
	for i, def := range got {
		if def.Name != wantNames[i] {
			t.Errorf("got[%d].Name = %q; want %q", i, def.Name, wantNames[i])
		}
		if def.Type != "function" {
			t.Errorf("got[%d].Type = %q; want function", i, def.Type)
		}
	}
}

func TestMapFragment_EffectiveVersionStableAcrossToolSubsets(t *testing.T) {
	session := &fragment.Session{CursorVersion: "0.45.2"}

	fragA := basicFragment(t)
	fragA.Tools = []fragment.ToolRecord{{ToolName: "Read", ToolUseID: "t1"}}

	fragB := basicFragment(t)
	fragB.Tools = []fragment.ToolRecord{{ToolName: "Bash", ToolUseID: "t2"}}

	gotA := MapFragment(Inputs{Fragment: fragA, Session: session, ContentCapture: agento11y.ContentCaptureModeFull, Now: fixedTime})
	gotB := MapFragment(Inputs{Fragment: fragB, Session: session, ContentCapture: agento11y.ContentCaptureModeFull, Now: fixedTime})

	if gotA.Generation.EffectiveVersion == "" {
		t.Fatalf("EffectiveVersion is empty; expected raw cursorVersion")
	}
	if gotA.Generation.EffectiveVersion != gotB.Generation.EffectiveVersion {
		t.Fatalf("EffectiveVersion mismatch across turns: %q vs %q", gotA.Generation.EffectiveVersion, gotB.Generation.EffectiveVersion)
	}
	if gotA.Generation.EffectiveVersion != gotA.Generation.AgentVersion {
		t.Fatalf("EffectiveVersion %q should equal AgentVersion %q", gotA.Generation.EffectiveVersion, gotA.Generation.AgentVersion)
	}
	if gotA.Start.EffectiveVersion != gotA.Generation.EffectiveVersion {
		t.Fatalf("Start.EffectiveVersion %q != Generation.EffectiveVersion %q", gotA.Start.EffectiveVersion, gotA.Generation.EffectiveVersion)
	}
}

// TestMapFragmentAgentNameOverride pins the exported identity against
// Inputs.AgentName on both the start and the generation.
func TestMapFragmentAgentNameOverride(t *testing.T) {
	tests := []struct {
		name      string
		agentName string
		want      string
	}{
		{name: "blank keeps the product name", want: AgentName},
		{name: "override", agentName: "cursor-e2e", want: "cursor-e2e"},
		{name: "override is trimmed", agentName: "  spaced  ", want: "spaced"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MapFragment(Inputs{
				Fragment:       basicFragment(t),
				AgentName:      tt.agentName,
				ContentCapture: agento11y.ContentCaptureModeMetadataOnly,
				Now:            fixedTime,
			})
			if got.Generation.AgentName != tt.want || got.Start.AgentName != tt.want {
				t.Fatalf("AgentName = %q/%q, want %q", got.Start.AgentName, got.Generation.AgentName, tt.want)
			}
		})
	}
}
