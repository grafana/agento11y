package hook

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/grafana/agento11y/go/agento11y"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/config"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/fragment"
)

func TestThoughtStepID(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"75945372-0ac1-4187-a87c-465c20ee3eba-10-sc6k", "75945372-0ac1-4187-a87c-465c20ee3eba-10-sc6k"},
		{"75945372-0ac1-4187-a87c-465c20ee3eba-0-75wi", "75945372-0ac1-4187-a87c-465c20ee3eba-0-75wi"},
		{"75945372-0ac1-4187-a87c-465c20ee3eba", ""},
		{"gen-cursor-1", ""},
		{"", ""},
		{"  ", ""},
	}
	for _, tc := range cases {
		if got := thoughtStepID(tc.in); got != tc.want {
			t.Errorf("thoughtStepID(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

func TestTurnGenerationID(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"75945372-0ac1-4187-a87c-465c20ee3eba-10-sc6k", "75945372-0ac1-4187-a87c-465c20ee3eba"},
		{"75945372-0ac1-4187-a87c-465c20ee3eba-0-75wi", "75945372-0ac1-4187-a87c-465c20ee3eba"},
		{"75945372-0ac1-4187-a87c-465c20ee3eba", "75945372-0ac1-4187-a87c-465c20ee3eba"},
		{"gen-cursor-1", "gen-cursor-1"},
		{"", ""},
		{"  ", ""},
	}
	for _, tc := range cases {
		if got := turnGenerationID(tc.in); got != tc.want {
			t.Errorf("turnGenerationID(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

func TestResolvedModel(t *testing.T) {
	cases := []struct {
		name string
		p    Payload
		want string
	}{
		{name: "model wins", p: Payload{Model: "claude-opus-4-7-thinking-max", ModelID: "claude-opus-4-7"}, want: "claude-opus-4-7-thinking-max"},
		{name: "model_id fallback", p: Payload{ModelID: "claude-opus-4-7"}, want: "claude-opus-4-7"},
		{name: "blank", p: Payload{}, want: ""},
		{name: "whitespace model falls through", p: Payload{Model: "  ", ModelID: "gpt-5"}, want: "gpt-5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolvedModel(tc.p); got != tc.want {
				t.Errorf("resolvedModel = %q; want %q", got, tc.want)
			}
		})
	}
}

func TestAfterAgentThought(t *testing.T) {
	cases := []struct {
		name    string
		seed    func(t *testing.T, logger *log.Logger)
		payload Payload
		verify  func(t *testing.T, logBuf *bytes.Buffer)
	}{
		{
			name: "first call sets flag",
			payload: Payload{
				HookEventName:  "afterAgentThought",
				ConversationID: "conv",
				GenerationID:   "gen",
				Timestamp:      "2026-04-28T12:00:00Z",
			},
			verify: func(t *testing.T, _ *bytes.Buffer) {
				got := fragment.LoadTolerant("conv", "gen", log.New(&bytes.Buffer{}, "", 0))
				if got == nil || !got.ThinkingPresent {
					t.Fatalf("ThinkingPresent should be set; got %+v", got)
				}
			},
		},
		{
			name: "suffixed generation_id collapses onto parent",
			seed: func(t *testing.T, logger *log.Logger) {
				if err := fragment.Update("conv", "75945372-0ac1-4187-a87c-465c20ee3eba", logger, func(f *fragment.Fragment) bool {
					f.UserPrompt = "hello"
					return true
				}); err != nil {
					t.Fatalf("seed: %v", err)
				}
			},
			payload: Payload{
				HookEventName:  "afterAgentThought",
				ConversationID: "conv",
				GenerationID:   "75945372-0ac1-4187-a87c-465c20ee3eba-10-sc6k",
				Model:          "claude-opus-4-7-thinking-max",
				Timestamp:      "2026-04-28T12:00:00Z",
			},
			verify: func(t *testing.T, _ *bytes.Buffer) {
				parent := fragment.LoadTolerant("conv", "75945372-0ac1-4187-a87c-465c20ee3eba", log.New(&bytes.Buffer{}, "", 0))
				if parent == nil || !parent.ThinkingPresent {
					t.Fatalf("parent ThinkingPresent unset; got %+v", parent)
				}
				if parent.Model != "claude-opus-4-7-thinking-max" {
					t.Errorf("parent Model = %q; want claude-opus-4-7-thinking-max", parent.Model)
				}
				if parent.UserPrompt != "hello" {
					t.Errorf("parent UserPrompt = %q; want hello", parent.UserPrompt)
				}
				siblingPath := fragment.FragmentFilePath("conv", "75945372-0ac1-4187-a87c-465c20ee3eba-10-sc6k")
				if _, err := os.Stat(siblingPath); !os.IsNotExist(err) {
					t.Errorf("sibling fragment should not exist at %s (err=%v)", siblingPath, err)
				}
			},
		},
		{
			name: "model_id fills empty model when thinking already set",
			seed: func(t *testing.T, logger *log.Logger) {
				if err := fragment.Update("conv", "gen", logger, func(f *fragment.Fragment) bool {
					f.ThinkingPresent = true
					return true
				}); err != nil {
					t.Fatalf("seed: %v", err)
				}
			},
			payload: Payload{
				HookEventName:  "afterAgentThought",
				ConversationID: "conv",
				GenerationID:   "gen",
				ModelID:        "gpt-5.1-codex",
				Timestamp:      "2026-04-28T12:00:01Z",
			},
			verify: func(t *testing.T, _ *bytes.Buffer) {
				got := fragment.LoadTolerant("conv", "gen", log.New(&bytes.Buffer{}, "", 0))
				if got == nil {
					t.Fatal("fragment missing")
				}
				if got.Model != "gpt-5.1-codex" {
					t.Errorf("Model = %q; want gpt-5.1-codex", got.Model)
				}
			},
		},
		{
			name: "already true skips rewrite when model already set",
			seed: func(t *testing.T, logger *log.Logger) {
				if err := fragment.Update("conv", "gen", logger, func(f *fragment.Fragment) bool {
					f.ThinkingPresent = true
					f.Model = "claude-sonnet-4"
					f.FirstTokenAt = "2026-04-28T12:00:00Z"
					return true
				}); err != nil {
					t.Fatalf("seed: %v", err)
				}
				path := fragment.FragmentFilePath("conv", "gen")
				old := time.Now().Add(-time.Hour)
				if err := os.Chtimes(path, old, old); err != nil {
					t.Fatalf("chtimes: %v", err)
				}
			},
			payload: Payload{
				HookEventName:  "afterAgentThought",
				ConversationID: "conv",
				GenerationID:   "gen",
				Model:          "claude-sonnet-4",
				Timestamp:      "2026-04-28T12:00:01Z",
			},
			verify: func(t *testing.T, _ *bytes.Buffer) {
				path := fragment.FragmentFilePath("conv", "gen")
				stat, err := os.Stat(path)
				if err != nil {
					t.Fatalf("stat after: %v", err)
				}
				if time.Since(stat.ModTime()) < 30*time.Minute {
					t.Errorf("file was rewritten: mtime=%v (expected ~1h old)", stat.ModTime())
				}
			},
		},
		{
			name:    "missing ids logs",
			payload: Payload{HookEventName: "afterAgentThought"},
			verify: func(t *testing.T, logBuf *bytes.Buffer) {
				if !bytes.Contains(logBuf.Bytes(), []byte("missing")) {
					t.Errorf("expected 'missing' log; got %q", logBuf.String())
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			var logBuf bytes.Buffer
			logger := log.New(&logBuf, "", 0)

			if tc.seed != nil {
				tc.seed(t, logger)
			}
			AfterAgentThought(tc.payload, config.Config{}, logger)
			tc.verify(t, &logBuf)
		})
	}
}

func TestAfterAgentThoughtStoresThinkingText(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	logger := log.New(&bytes.Buffer{}, "", 0)
	cfg := config.Config{ContentCapture: agento11y.ContentCaptureModeFull}
	firstDur := 2000.0
	laterDur := 5000.0

	AfterAgentThought(Payload{
		HookEventName:  "afterAgentThought",
		ConversationID: "conv",
		GenerationID:   "gen-0-ab12",
		Timestamp:      "2026-04-28T12:00:01Z",
		Text:           "weigh the options",
		DurationMs:     &firstDur,
	}, cfg, logger)
	AfterAgentThought(Payload{
		HookEventName:  "afterAgentThought",
		ConversationID: "conv",
		GenerationID:   "gen-0-ab12",
		Timestamp:      "2026-04-28T12:00:04Z",
		Text:           "weigh the options, then pick one",
		DurationMs:     &laterDur,
	}, cfg, logger)

	got := fragment.LoadTolerant("conv", "gen", logger)
	if got == nil {
		t.Fatal("fragment not written")
	}
	if !got.ThinkingPresent {
		t.Fatal("ThinkingPresent unset")
	}
	if got.FirstTokenAt != "" {
		t.Errorf("FirstTokenAt = %q; want empty because the hook timestamp is completion time", got.FirstTokenAt)
	}
	if len(got.Thinking) != 1 || got.Thinking[0].Text != "weigh the options, then pick one" {
		t.Fatalf("Thinking = %+v; want the same step's longer aggregation to replace the prefix", got.Thinking)
	}
	if got.Thinking[0].StepID != "gen-0-ab12" {
		t.Errorf("StepID = %q; want gen-0-ab12", got.Thinking[0].StepID)
	}
	if got.Thinking[0].DurationMs == nil || *got.Thinking[0].DurationMs != laterDur {
		t.Errorf("duration = %v; want %v", got.Thinking[0].DurationMs, laterDur)
	}
	if _, err := os.Stat(fragment.FragmentFilePath("conv", "gen-0-ab12")); !os.IsNotExist(err) {
		t.Fatalf("step id should collapse onto gen, stat err=%v", err)
	}
}

func TestAfterAgentThoughtPairsBareTurnIDResend(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	logger := log.New(&bytes.Buffer{}, "", 0)
	cfg := config.Config{ContentCapture: agento11y.ContentCaptureModeFull}
	firstDur := 2000.0
	laterDur := 5000.0

	AfterAgentThought(Payload{
		HookEventName:  "afterAgentThought",
		ConversationID: "conv",
		GenerationID:   "gen-0-ab12",
		Timestamp:      "2026-04-28T12:00:01Z",
		Text:           "weigh the options",
		DurationMs:     &firstDur,
	}, cfg, logger)
	AfterAgentThought(Payload{
		HookEventName:  "afterAgentThought",
		ConversationID: "conv",
		GenerationID:   "gen",
		Timestamp:      "2026-04-28T12:00:04Z",
		Text:           "weigh the options, then pick one",
		DurationMs:     &laterDur,
	}, cfg, logger)

	got := fragment.LoadTolerant("conv", "gen", logger)
	if got == nil {
		t.Fatal("fragment not written")
	}
	if len(got.Thinking) != 1 || got.Thinking[0].Text != "weigh the options, then pick one" {
		t.Fatalf("Thinking = %+v; want the bare turn-id event to update the stepped block", got.Thinking)
	}
	if got.Thinking[0].StepID != "gen-0-ab12" {
		t.Errorf("StepID = %q; want the original step id kept", got.Thinking[0].StepID)
	}
	if got.Thinking[0].DurationMs == nil || *got.Thinking[0].DurationMs != laterDur {
		t.Errorf("duration = %v; want %v", got.Thinking[0].DurationMs, laterDur)
	}
}

func TestAfterAgentThoughtKeepsDistinctSteps(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	logger := log.New(&bytes.Buffer{}, "", 0)
	cfg := config.Config{ContentCapture: agento11y.ContentCaptureModeFull}
	dur := 1000.0

	AfterAgentThought(Payload{
		HookEventName:  "afterAgentThought",
		ConversationID: "conv",
		GenerationID:   "gen-0-ab12",
		Text:           "Let me check",
		DurationMs:     &dur,
	}, cfg, logger)
	AfterAgentThought(Payload{
		HookEventName:  "afterAgentThought",
		ConversationID: "conv",
		GenerationID:   "gen-1-cd34",
		Text:           "Let me check",
		DurationMs:     &dur,
	}, cfg, logger)

	got := fragment.LoadTolerant("conv", "gen", logger)
	if got == nil {
		t.Fatal("fragment not written")
	}
	if len(got.Thinking) != 2 {
		t.Fatalf("Thinking = %+v; want one block per model step", got.Thinking)
	}
	if got.Thinking[0].StepID != "gen-0-ab12" || got.Thinking[1].StepID != "gen-1-cd34" {
		t.Errorf("step ids = %q, %q", got.Thinking[0].StepID, got.Thinking[1].StepID)
	}
}

func TestAfterAgentThoughtDropsTextInMetadataOnly(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	logger := log.New(&bytes.Buffer{}, "", 0)
	first := 1000.0
	second := 2000.0
	cfg := config.Config{ContentCapture: agento11y.ContentCaptureModeMetadataOnly}
	AfterAgentThought(Payload{
		HookEventName:  "afterAgentThought",
		ConversationID: "conv",
		GenerationID:   "gen-0-ab12",
		Text:           "secret reasoning",
		DurationMs:     &first,
	}, cfg, logger)
	AfterAgentThought(Payload{
		HookEventName:  "afterAgentThought",
		ConversationID: "conv",
		GenerationID:   "gen-1-cd34",
		Text:           "more secret reasoning",
		DurationMs:     &second,
	}, cfg, logger)
	AfterAgentThought(Payload{
		HookEventName:  "afterAgentThought",
		ConversationID: "conv",
		GenerationID:   "gen-2-ef56",
		Text:           "same duration as first is still another block",
		DurationMs:     &first,
	}, cfg, logger)

	got := fragment.LoadTolerant("conv", "gen", logger)
	if got == nil || !got.ThinkingPresent {
		t.Fatalf("expected thinking flag; got %+v", got)
	}
	if got.ThinkingDurationMs != nil {
		t.Errorf("legacy ThinkingDurationMs = %v; want per-block segments only", got.ThinkingDurationMs)
	}
	if len(got.Thinking) != 3 {
		t.Fatalf("Thinking = %+v; want one duration per block", got.Thinking)
	}
	var sum float64
	for i, seg := range got.Thinking {
		if seg.Text != "" {
			t.Errorf("thinking text leaked on block %d: %+v", i, seg)
		}
		if seg.DurationMs == nil {
			t.Fatalf("block %d missing duration: %+v", i, seg)
		}
		sum += *seg.DurationMs
	}
	if sum != 4000 {
		t.Errorf("duration sum = %v; want 4000", sum)
	}
}

func TestAfterAgentThoughtBareFirstDoesNotOverwritePriorStep(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	logger := log.New(&bytes.Buffer{}, "", 0)
	cfg := config.Config{ContentCapture: agento11y.ContentCaptureModeMetadataOnly}
	first := 1000.0
	second := 2000.0

	// Cursor often sends the bare turn id first, then the stepped id.
	AfterAgentThought(Payload{
		HookEventName:  "afterAgentThought",
		ConversationID: "conv",
		GenerationID:   "gen",
		Text:           "first block",
		DurationMs:     &first,
	}, cfg, logger)
	AfterAgentThought(Payload{
		HookEventName:  "afterAgentThought",
		ConversationID: "conv",
		GenerationID:   "gen-0-ab12",
		Text:           "first block",
		DurationMs:     &first,
	}, cfg, logger)
	AfterAgentThought(Payload{
		HookEventName:  "afterAgentThought",
		ConversationID: "conv",
		GenerationID:   "gen",
		Text:           "second block",
		DurationMs:     &second,
	}, cfg, logger)
	AfterAgentThought(Payload{
		HookEventName:  "afterAgentThought",
		ConversationID: "conv",
		GenerationID:   "gen-1-cd34",
		Text:           "second block",
		DurationMs:     &second,
	}, cfg, logger)

	got := fragment.LoadTolerant("conv", "gen", logger)
	if got == nil {
		t.Fatal("fragment not written")
	}
	if len(got.Thinking) != 2 {
		t.Fatalf("Thinking = %+v; want the second bare event to start a new block", got.Thinking)
	}
	if got.Thinking[0].DurationMs == nil || *got.Thinking[0].DurationMs != first {
		t.Errorf("first duration = %v; want %v", got.Thinking[0].DurationMs, first)
	}
	if got.Thinking[1].DurationMs == nil || *got.Thinking[1].DurationMs != second {
		t.Errorf("second duration = %v; want %v", got.Thinking[1].DurationMs, second)
	}
}

func TestAfterAgentThoughtPairsBareResendInMetadataOnly(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	logger := log.New(&bytes.Buffer{}, "", 0)
	cfg := config.Config{ContentCapture: agento11y.ContentCaptureModeMetadataOnly}
	first := 1000.0
	second := 2000.0

	AfterAgentThought(Payload{
		HookEventName:  "afterAgentThought",
		ConversationID: "conv",
		GenerationID:   "gen-0-ab12",
		Text:           "secret reasoning",
		DurationMs:     &first,
	}, cfg, logger)
	AfterAgentThought(Payload{
		HookEventName:  "afterAgentThought",
		ConversationID: "conv",
		GenerationID:   "gen",
		Text:           "secret reasoning",
		DurationMs:     &first,
	}, cfg, logger)
	AfterAgentThought(Payload{
		HookEventName:  "afterAgentThought",
		ConversationID: "conv",
		GenerationID:   "gen-1-cd34",
		Text:           "next step",
		DurationMs:     &second,
	}, cfg, logger)
	AfterAgentThought(Payload{
		HookEventName:  "afterAgentThought",
		ConversationID: "conv",
		GenerationID:   "gen",
		Text:           "next step",
		DurationMs:     &second,
	}, cfg, logger)

	got := fragment.LoadTolerant("conv", "gen", logger)
	if got == nil {
		t.Fatal("fragment not written")
	}
	if len(got.Thinking) != 2 {
		t.Fatalf("Thinking = %+v; want one block per step after pairing bare resends", got.Thinking)
	}
	var sum float64
	for _, seg := range got.Thinking {
		if seg.Text != "" {
			t.Errorf("thinking text leaked: %+v", seg)
		}
		if seg.DurationMs != nil {
			sum += *seg.DurationMs
		}
	}
	if sum != 3000 {
		t.Errorf("duration sum = %v; want 3000", sum)
	}
}

func TestParseModelParamsToleratesNonStringValues(t *testing.T) {
	raw := []byte(`{"model_params":[{"id":"optimize_for","value":"cost"},{"id":"thinking","value":true},{"id":"max_tokens","value":128},{"id":"","value":"skip"},{"id":"nested","value":{"a":1}}]}`)
	var p Payload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := parseModelParams(p.ModelParams)
	if got["optimize_for"] != "cost" || got["thinking"] != "true" || got["max_tokens"] != "128" {
		t.Fatalf("params = %#v", got)
	}
	if _, ok := got["nested"]; !ok {
		t.Fatal("object value should be kept as JSON")
	}
	if _, ok := got[""]; ok {
		t.Fatal("empty id should be dropped")
	}
	if parseModelParams([]byte(`{"id":"thinking"}`)) != nil {
		t.Fatal("a non-array model_params should not fail the caller; it should yield nothing")
	}
}

func TestApplyStopModelPrefersSlug(t *testing.T) {
	frag := &fragment.Fragment{Model: "claude-opus-4-7"}
	applyStopModel(frag, Payload{Model: "claude-opus-4-7-thinking-max", ModelID: "claude-opus-4-7"})
	if frag.Model != "claude-opus-4-7-thinking-max" {
		t.Fatalf("Model = %q; want composer slug from stop", frag.Model)
	}

	frag2 := &fragment.Fragment{}
	applyStopModel(frag2, Payload{ModelID: "gpt-5.1"})
	if frag2.Model != "gpt-5.1" {
		t.Fatalf("Model = %q; want model_id fallback", frag2.Model)
	}
}

func TestIsThoughtOnlyFragment(t *testing.T) {
	in := int64(10)
	cases := []struct {
		name string
		frag *fragment.Fragment
		want bool
	}{
		{name: "nil", frag: nil, want: false},
		{
			name: "thinking only on bare turn id kept",
			frag: &fragment.Fragment{GenerationID: "75945372-0ac1-4187-a87c-465c20ee3eba", ThinkingPresent: true},
			want: false,
		},
		{
			name: "thinking only on step id dropped",
			frag: &fragment.Fragment{GenerationID: "75945372-0ac1-4187-a87c-465c20ee3eba-0-75wi", ThinkingPresent: true},
			want: true,
		},
		{
			name: "empty step id dropped",
			frag: &fragment.Fragment{GenerationID: "75945372-0ac1-4187-a87c-465c20ee3eba-10-sc6k"},
			want: true,
		},
		{name: "with prompt", frag: &fragment.Fragment{GenerationID: "g-1-abcd", UserPrompt: "hi"}, want: false},
		{name: "with tools", frag: &fragment.Fragment{GenerationID: "g-1-abcd", Tools: []fragment.ToolRecord{{ToolName: "Read"}}}, want: false},
		{name: "with assistant", frag: &fragment.Fragment{GenerationID: "g-1-abcd", Assistant: []fragment.AssistantSegment{{Text: "ok"}}}, want: false},
		{name: "metadata_only with model on step id", frag: &fragment.Fragment{GenerationID: "g-1-abcd", ThinkingPresent: true, Model: "claude-opus-4-7"}, want: false},
		{name: "pending stop retry", frag: &fragment.Fragment{GenerationID: "g-1-abcd", PendingStop: &fragment.PendingStop{Status: "completed"}}, want: false},
		{name: "token usage only", frag: &fragment.Fragment{GenerationID: "g-1-abcd", TokenUsage: &fragment.TokenCounts{InputTokens: &in}}, want: false},
		{name: "provider only", frag: &fragment.Fragment{GenerationID: "g-1-abcd", Provider: "anthropic"}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isThoughtOnlyFragment(tc.frag); got != tc.want {
				t.Errorf("isThoughtOnlyFragment = %v; want %v", got, tc.want)
			}
		})
	}
}

func TestEmitOneStrandedDropsThoughtOnly(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	logger := log.New(&bytes.Buffer{}, "", 0)

	const conv = "conv-drop"
	const thoughtGID = "75945372-0ac1-4187-a87c-465c20ee3eba-0-75wi"
	const realGID = "75945372-0ac1-4187-a87c-465c20ee3eba"

	if err := fragment.Update(conv, thoughtGID, logger, func(f *fragment.Fragment) bool {
		f.ThinkingPresent = true
		return true
	}); err != nil {
		t.Fatalf("seed thought: %v", err)
	}
	if err := fragment.Update(conv, realGID, logger, func(f *fragment.Fragment) bool {
		f.UserPrompt = "investigate unknown"
		f.Tools = []fragment.ToolRecord{{ToolName: "Read"}}
		f.Model = "claude-opus-4-7"
		return true
	}); err != nil {
		t.Fatalf("seed real: %v", err)
	}

	// No credentials / client: call the helper directly. Thought-only path
	// returns before emitGeneration, so a nil client is fine for that branch.
	if emitted, ok := emitOneStranded(t.Context(), nil, nil, conv, thoughtGID, config.Config{}, logger); emitted || !ok {
		t.Fatal("thought-only emitOneStranded should succeed (drop)")
	}
	if _, err := os.Stat(fragment.FragmentFilePath(conv, thoughtGID)); !os.IsNotExist(err) {
		t.Fatalf("thought-only fragment should be deleted; err=%v", err)
	}
	// Real fragment must still be on disk — we did not call emit for it here.
	if _, err := os.Stat(fragment.FragmentFilePath(conv, realGID)); err != nil {
		t.Fatalf("real fragment should remain: %v", err)
	}
	// Directory should still contain the real fragment file.
	entries, err := os.ReadDir(filepath.Dir(fragment.FragmentFilePath(conv, realGID)))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("conversation dir emptied unexpectedly")
	}
}
