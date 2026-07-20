package mapper

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grafana/agento11y/go/agento11y"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/codex/codexlog"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/codex/fragment"
)

func TestMapMetadataOnlyStripsTextButKeepsToolStructure(t *testing.T) {
	f := &fragment.Fragment{
		SessionID: "sess",
		TurnID:    "turn",
		Model:     "gpt-5.5",
		Prompt:    "hello",
		Tools: []fragment.ToolRecord{{
			ToolName:  "Bash",
			ToolUseID: "tool-1",
		}},
		LastAssistantMessage: "done",
	}
	got := Map(Inputs{Fragment: f, ContentCapture: agento11y.ContentCaptureModeMetadataOnly, Now: time.Unix(1, 0)})
	if len(got.Generation.Input) != 0 {
		t.Fatalf("metadata_only input len = %d, want 0", len(got.Generation.Input))
	}
	if len(got.Generation.Output) != 1 || got.Generation.Output[0].Parts[0].ToolCall == nil {
		t.Fatalf("metadata_only should retain tool-call structure, got %+v", got.Generation.Output)
	}
}

func TestMapFullRedactsContent(t *testing.T) {
	secret := "glc_abcdefghijklmnopqrstuvwxyz"
	raw, _ := json.Marshal(map[string]string{"token": secret})
	f := &fragment.Fragment{
		SessionID:            "sess",
		TurnID:               "turn",
		Model:                "gpt-5.5",
		Prompt:               "token " + secret,
		LastAssistantMessage: "saw " + secret,
		Tools: []fragment.ToolRecord{{
			ToolName:     "Bash",
			ToolUseID:    "tool-1",
			ToolInput:    raw,
			ToolResponse: raw,
		}},
	}
	got := Map(Inputs{Fragment: f, ContentCapture: agento11y.ContentCaptureModeFull, Now: time.Unix(1, 0)})
	combined := got.Generation.Input[0].Parts[0].Text + got.Generation.Output[len(got.Generation.Output)-1].Parts[0].Text
	toolInput := string(got.Generation.Output[0].Parts[0].ToolCall.InputJSON)
	toolResult := string(got.Generation.Input[1].Parts[0].ToolResult.ContentJSON)
	if !json.Valid([]byte(toolInput)) || !json.Valid([]byte(toolResult)) {
		t.Fatalf("redacted tool JSON must remain valid: input=%s result=%s", toolInput, toolResult)
	}
	combined += toolInput + toolResult
	if strings.Contains(combined, secret) {
		t.Fatalf("unredacted secret in generation: %s", combined)
	}
	if !strings.Contains(combined, "[REDACTED:grafana-cloud-token]") {
		t.Fatalf("expected redaction marker in generation: %s", combined)
	}
}

func TestMapFullWithMetadataSpansPreservesStartModeAndFullPayload(t *testing.T) {
	f := &fragment.Fragment{
		SessionID:            "sess",
		TurnID:               "turn",
		Model:                "gpt-5.5",
		Prompt:               "hello",
		LastAssistantMessage: "done",
		Tools: []fragment.ToolRecord{{
			ToolName:     "Bash",
			ToolUseID:    "tool-1",
			ToolInput:    json.RawMessage(`{"cmd":"echo hi"}`),
			ToolResponse: json.RawMessage(`{"output":"hi"}`),
		}},
	}

	got := Map(Inputs{Fragment: f, ContentCapture: agento11y.ContentCaptureModeFullWithMetadataSpans, Now: time.Unix(1, 0)})
	if got.Start.ContentCapture != agento11y.ContentCaptureModeFullWithMetadataSpans {
		t.Fatalf("Start.ContentCapture = %v; want FullWithMetadataSpans", got.Start.ContentCapture)
	}
	if len(got.Generation.Input) == 0 || got.Generation.Input[0].Parts[0].Text == "" {
		t.Fatalf("full_with_metadata_spans should keep full gRPC input payload: %+v", got.Generation.Input)
	}
	if len(got.Generation.Output) == 0 || got.Generation.Output[0].Parts[0].ToolCall == nil || len(got.Generation.Output[0].Parts[0].ToolCall.InputJSON) == 0 {
		t.Fatalf("full_with_metadata_spans should keep full gRPC tool payload: %+v", got.Generation.Output)
	}
}

func TestMapFullRedactsInvalidJSONAsJSONString(t *testing.T) {
	secret := "glc_abcdefghijklmnopqrstuvwxyz"
	f := &fragment.Fragment{
		SessionID: "sess",
		TurnID:    "turn",
		Model:     "gpt-5.5",
		Tools: []fragment.ToolRecord{{
			ToolName:     "Bash",
			ToolUseID:    "tool-1",
			ToolResponse: json.RawMessage(`token=` + secret),
		}},
	}
	got := Map(Inputs{Fragment: f, ContentCapture: agento11y.ContentCaptureModeFull, Now: time.Unix(1, 0)})
	toolResult := got.Generation.Input[0].Parts[0].ToolResult.ContentJSON
	if !json.Valid(toolResult) {
		t.Fatalf("redacted invalid JSON fallback must be valid JSON: %s", string(toolResult))
	}
	if strings.Contains(string(toolResult), secret) {
		t.Fatalf("unredacted secret in generation: %s", string(toolResult))
	}
}

func TestMapFullRedactsSensitiveJSONKeys(t *testing.T) {
	raw := json.RawMessage(`{
		"password": "hunter2",
		"client_secret": "short-secret",
		"token": "short-token",
		"api_key": "short-api-key",
		"clientSecret": "short-camel-secret",
		"headers": {
			"Authorization": "Bearer short"
		},
		"nested": [
			{"access_key": "short-access-key"}
		],
		"safe": "visible"
	}`)
	f := &fragment.Fragment{
		SessionID: "sess",
		TurnID:    "turn",
		Model:     "gpt-5.5",
		Tools: []fragment.ToolRecord{{
			ToolName:     "Bash",
			ToolUseID:    "tool-1",
			ToolResponse: raw,
		}},
	}
	got := Map(Inputs{Fragment: f, ContentCapture: agento11y.ContentCaptureModeFull, Now: time.Unix(1, 0)})
	toolResult := string(got.Generation.Input[0].Parts[0].ToolResult.ContentJSON)
	for _, secret := range []string{"hunter2", "short-secret", "short-token", "short-api-key", "short-camel-secret", "Bearer short", "short-access-key"} {
		if strings.Contains(toolResult, secret) {
			t.Fatalf("secret %q leaked in redacted JSON: %s", secret, toolResult)
		}
	}
	if !strings.Contains(toolResult, "[REDACTED:json-secret-field]") {
		t.Fatalf("expected sensitive JSON field redaction marker: %s", toolResult)
	}
	if !strings.Contains(toolResult, "visible") {
		t.Fatalf("safe value should remain visible: %s", toolResult)
	}
	if !json.Valid([]byte(toolResult)) {
		t.Fatalf("redacted JSON must remain valid: %s", toolResult)
	}
}

func TestMapResolvesGitBranchFromCwd(t *testing.T) {
	// Fragment cwd points at a temp dir containing a `.git/HEAD` symbolic
	// ref, so the gitbranch resolver finds the branch without shelling
	// out. Mirrors the cursor tags-package fixture.
	cases := []struct {
		name    string
		headRaw string
		want    string
	}{
		{name: "regular branch", headRaw: "ref: refs/heads/feature/codex\n", want: "feature/codex"},
		{name: "detached HEAD", headRaw: "abcdef0123456789abcdef0123456789abcdef01\n", want: "abcdef012345"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			gitHead := filepath.Join(root, ".git", "HEAD")
			if err := os.MkdirAll(filepath.Dir(gitHead), 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.WriteFile(gitHead, []byte(tc.headRaw), 0o644); err != nil {
				t.Fatalf("write head: %v", err)
			}
			f := &fragment.Fragment{
				SessionID: "sess",
				TurnID:    "turn",
				Model:     "gpt-5.5",
				Cwd:       root,
			}
			got := Map(Inputs{Fragment: f, ContentCapture: agento11y.ContentCaptureModeMetadataOnly, Now: time.Unix(1, 0)})
			if got.Generation.Tags["git.branch"] != tc.want {
				t.Fatalf("git.branch = %q, want %q", got.Generation.Tags["git.branch"], tc.want)
			}
			if got.Generation.Tags["cwd"] != root {
				t.Fatalf("cwd = %q, want %q", got.Generation.Tags["cwd"], root)
			}
		})
	}
}

func TestMapOmitsGitBranchWhenNoCheckout(t *testing.T) {
	root := t.TempDir()
	f := &fragment.Fragment{
		SessionID: "sess",
		TurnID:    "turn",
		Model:     "gpt-5.5",
		Cwd:       root,
	}
	got := Map(Inputs{Fragment: f, ContentCapture: agento11y.ContentCaptureModeMetadataOnly, Now: time.Unix(1, 0)})
	if _, ok := got.Generation.Tags["git.branch"]; ok {
		t.Fatalf("git.branch should be absent when no .git found; got tags=%+v", got.Generation.Tags)
	}
	if got.Generation.Tags["cwd"] != root {
		t.Fatalf("cwd should still be present; got %q", got.Generation.Tags["cwd"])
	}
}

func TestMapAddsStopHookActiveTag(t *testing.T) {
	f := &fragment.Fragment{
		SessionID:      "sess",
		TurnID:         "turn",
		Model:          "gpt-5.5",
		StopHookActive: true,
	}
	got := Map(Inputs{Fragment: f, ContentCapture: agento11y.ContentCaptureModeMetadataOnly, Now: time.Unix(1, 0)})
	if got.Generation.Tags["codex.stop_hook_active"] != "true" {
		t.Fatalf("stop hook tag = %q, want true", got.Generation.Tags["codex.stop_hook_active"])
	}
}

func TestMapDoesNotShareTagOrMetadataMaps(t *testing.T) {
	f := &fragment.Fragment{
		SessionID: "child",
		TurnID:    "child-turn",
		Model:     "gpt-5.5",
	}
	link := &fragment.SubagentLink{
		ChildSessionID:     "child",
		ParentSessionID:    "parent",
		ParentGenerationID: "parent-gen",
	}

	got := Map(Inputs{Fragment: f, SubagentLink: link, ContentCapture: agento11y.ContentCaptureModeMetadataOnly, Now: time.Unix(1, 0)})
	got.Start.Tags["start-only"] = "true"
	got.Start.Metadata["start-only"] = "true"

	if _, ok := got.Generation.Tags["start-only"]; ok {
		t.Fatalf("Generation.Tags shares Start.Tags: %+v", got.Generation.Tags)
	}
	if _, ok := got.Generation.Metadata["start-only"]; ok {
		t.Fatalf("Generation.Metadata shares Start.Metadata: %+v", got.Generation.Metadata)
	}
}

func TestMapResolvedSubagentLink(t *testing.T) {
	f := &fragment.Fragment{
		SessionID: "child",
		TurnID:    "child-turn",
		Model:     "gpt-5.5",
	}
	link := &fragment.SubagentLink{
		ChildSessionID:     "child",
		ParentSessionID:    "parent",
		ParentTurnID:       "parent-turn",
		ParentGenerationID: "parent-gen",
		SpawnCallID:        "call_1",
		AgentRole:          "reviewer",
		AgentNickname:      "Dalton",
		AgentDepth:         1,
		Source:             "transcript.session_meta",
	}

	got := Map(Inputs{Fragment: f, SubagentLink: link, ContentCapture: agento11y.ContentCaptureModeMetadataOnly, Now: time.Unix(1, 0)})

	if got.Generation.AgentName != SubagentAgentName || got.Start.AgentName != SubagentAgentName {
		t.Fatalf("AgentName = %q/%q, want %q", got.Start.AgentName, got.Generation.AgentName, SubagentAgentName)
	}
	if got.Generation.ConversationID != "parent" || got.Start.ConversationID != "parent" {
		t.Fatalf("ConversationID = %q/%q, want parent", got.Start.ConversationID, got.Generation.ConversationID)
	}
	if len(got.Generation.ParentGenerationIDs) != 1 || got.Generation.ParentGenerationIDs[0] != "parent-gen" {
		t.Fatalf("ParentGenerationIDs = %+v, want parent-gen", got.Generation.ParentGenerationIDs)
	}
	if got.Start.ParentGenerationIDs[0] != "parent-gen" {
		t.Fatalf("Start.ParentGenerationIDs = %+v", got.Start.ParentGenerationIDs)
	}
	if got.Generation.Tags["subagent"] != "true" || got.Generation.Tags["codex.link_source"] != "transcript" || got.Generation.Tags["codex.agent_role"] != "reviewer" {
		t.Fatalf("unexpected tags: %+v", got.Generation.Tags)
	}
	if got.Generation.Metadata["codex.child_session_id"] != "child" ||
		got.Generation.Metadata["codex.parent_session_id"] != "parent" ||
		got.Generation.Metadata["codex.parent_turn_id"] != "parent-turn" ||
		got.Generation.Metadata["codex.spawn_call_id"] != "call_1" ||
		got.Generation.Metadata["codex.agent_nickname"] != "Dalton" ||
		got.Generation.Metadata["codex.agent_depth"] != 1 {
		t.Fatalf("unexpected metadata: %+v", got.Generation.Metadata)
	}
}

func TestMapPartialSubagentLink(t *testing.T) {
	f := &fragment.Fragment{
		SessionID: "child",
		TurnID:    "child-turn",
		Model:     "gpt-5.5",
	}
	link := &fragment.SubagentLink{
		ChildSessionID:  "child",
		ParentSessionID: "parent",
		AgentRole:       "default",
	}

	got := Map(Inputs{Fragment: f, SubagentLink: link, ContentCapture: agento11y.ContentCaptureModeMetadataOnly, Now: time.Unix(1, 0)})

	if got.Generation.AgentName != SubagentAgentName {
		t.Fatalf("AgentName = %q, want %q", got.Generation.AgentName, SubagentAgentName)
	}
	if got.Generation.ConversationID != "child" {
		t.Fatalf("ConversationID = %q, want child for partial link", got.Generation.ConversationID)
	}
	if got.Generation.ParentGenerationIDs != nil {
		t.Fatalf("ParentGenerationIDs = %+v, want nil", got.Generation.ParentGenerationIDs)
	}
	if got.Generation.Tags["codex.link_source"] != "partial" {
		t.Fatalf("link source tag = %q, want partial", got.Generation.Tags["codex.link_source"])
	}
	if got.Generation.Metadata["codex.parent_session_id"] != "parent" {
		t.Fatalf("unexpected metadata: %+v", got.Generation.Metadata)
	}
}

func TestMapSetsUsageFromTokenSnapshot(t *testing.T) {
	f := &fragment.Fragment{
		SessionID: "sess",
		TurnID:    "turn",
		Model:     "gpt-5.5",
	}
	snapshot := &codexlog.TokenSnapshot{
		TurnID: "turn",
		TurnUsage: codexlog.TokenUsage{
			InputTokens:           160,
			CachedInputTokens:     120,
			OutputTokens:          30,
			ReasoningOutputTokens: 9,
			TotalTokens:           190,
		},
		TotalUsage: codexlog.TokenUsage{
			InputTokens:           260,
			CachedInputTokens:     140,
			OutputTokens:          40,
			ReasoningOutputTokens: 12,
			TotalTokens:           300,
		},
		ModelContextWindow: 258400,
		Source:             "turn_context_delta",
	}

	got := Map(Inputs{Fragment: f, TokenSnapshot: snapshot, ContentCapture: agento11y.ContentCaptureModeMetadataOnly, Now: time.Unix(1, 0)})

	if got.Generation.Usage.InputTokens != 160 ||
		got.Generation.Usage.CacheReadInputTokens != 120 ||
		got.Generation.Usage.OutputTokens != 30 ||
		got.Generation.Usage.ReasoningTokens != 9 ||
		got.Generation.Usage.TotalTokens != 190 {
		t.Fatalf("unexpected usage: %+v", got.Generation.Usage)
	}
	if got.Generation.Metadata["codex.token_usage.total.input_tokens"] != int64(260) ||
		got.Generation.Metadata["codex.token_usage.total.cached_input_tokens"] != int64(140) ||
		got.Generation.Metadata["codex.token_usage.total.output_tokens"] != int64(40) ||
		got.Generation.Metadata["codex.token_usage.total.reasoning_output_tokens"] != int64(12) ||
		got.Generation.Metadata["codex.token_usage.total.total_tokens"] != int64(300) ||
		got.Generation.Metadata["codex.token_usage.context_window"] != int64(258400) ||
		got.Generation.Metadata["codex.token_usage.source"] != "turn_context_delta" {
		t.Fatalf("unexpected metadata: %+v", got.Generation.Metadata)
	}
	if _, ok := got.Generation.Tags["codex.token_usage.total.total_tokens"]; ok {
		t.Fatalf("token counts should not be tags: %+v", got.Generation.Tags)
	}
}

func TestMapWithoutTokenSnapshotPreservesExistingBehavior(t *testing.T) {
	f := &fragment.Fragment{
		SessionID: "sess",
		TurnID:    "turn",
		Model:     "gpt-5.5",
	}
	got := Map(Inputs{Fragment: f, ContentCapture: agento11y.ContentCaptureModeMetadataOnly, Now: time.Unix(1, 0)})

	if got.Generation.Usage != (agento11y.TokenUsage{}) {
		t.Fatalf("Usage = %+v, want zero", got.Generation.Usage)
	}
	if got.Generation.Metadata != nil {
		t.Fatalf("Metadata = %+v, want nil", got.Generation.Metadata)
	}
}
