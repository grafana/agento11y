package hook

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/config"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/fragment"
)

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
			AfterAgentThought(tc.payload, logger)
			tc.verify(t, &logBuf)
		})
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
	if !emitOneStranded(t.Context(), nil, nil, conv, thoughtGID, config.Config{}, logger) {
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
