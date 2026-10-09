package history

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// Regression: the Claude Code, Codex and Pi importers cut the conversation
// title to 100 bytes before the Sanitizer redacted it. A secret the cut split
// no longer matched its pattern, so its first part was exported in the title
// while the same prompt's input was redacted.
func TestImportedTitlesRedactASecretTheCutSplits(t *testing.T) {
	// The token starts at byte 71 and ends past byte 100.
	prompt := strings.Repeat("x", 70) + " ghp_" + strings.Repeat("A", 36) + " please"

	tests := []struct {
		name  string
		turns func(t *testing.T) []HistoricalGeneration
	}{
		{
			name: "claude-code",
			turns: func(t *testing.T) []HistoricalGeneration {
				root, sid := t.TempDir(), "sess-title-secret"
				path := writeFile(t, filepath.Join(root, "-work-repo", sid+".jsonl"),
					claudeUserLine(sid, "/work/repo", "2026-01-10T12:00:00Z", prompt)+
						claudeAssistantLine(sid, "/work/repo", "2026-01-10T12:00:10Z", "req-1", "ok", nil))
				imp := claudeImporterAt(root)
				preview, _, err := imp.Preview(context.Background(), path)
				if err != nil {
					t.Fatal(err)
				}
				return collectTurns(t, imp, preview)
			},
		},
		{
			name: "codex",
			turns: func(t *testing.T) []HistoricalGeneration {
				root, sid := t.TempDir(), "019a0000-0000-7000-8000-000000000001"
				path := writeFile(t, filepath.Join(root, "2026", "01", "10", "rollout-2026-01-10T12-00-00-"+sid+".jsonl"),
					codexRecord("2026-01-10T12:00:00Z", "session_meta", map[string]any{"id": sid, "cwd": "/work/repo"})+
						codexRecord("2026-01-10T12:00:01Z", "turn_context", map[string]any{"turn_id": "turn-1", "cwd": "/work/repo", "model": "gpt-5.5"})+
						codexRecord("2026-01-10T12:00:02Z", "event_msg", map[string]any{"type": "user_message", "message": prompt})+
						codexRecord("2026-01-10T12:00:05Z", "response_item", map[string]any{
							"type": "message", "role": "assistant",
							"content": []map[string]any{{"type": "output_text", "text": "ok"}},
						})+
						codexRecord("2026-01-10T12:00:06Z", "event_msg", map[string]any{
							"type": "token_count",
							"info": map[string]any{
								"total_token_usage": map[string]any{"input_tokens": 10, "output_tokens": 2, "total_tokens": 12},
								"last_token_usage":  map[string]any{"input_tokens": 10, "output_tokens": 2, "total_tokens": 12},
							},
						}))
				imp := codexImporterAt(root)
				return collectTurns(t, imp, codexPreview(t, imp, path))
			},
		},
		{
			name: "pi",
			turns: func(t *testing.T) []HistoricalGeneration {
				root, sid := t.TempDir(), "019ead07-cfbf-78d3-8b03-875769426583"
				path := writeFile(t, filepath.Join(root, "--work-repo--", "2026-06-09T15-37-10-848Z_"+sid+".jsonl"),
					piHeader(t, sid, "/work/repo", "2026-06-09T15:37:10.848Z")+
						piUserEntry(t, "u1", "", "2026-06-09T15:37:20.000Z", prompt, 1781019439000)+
						piAssistantEntry(t, "a1", "u1", "2026-06-09T15:37:24.000Z", 1781019441000,
							[]map[string]any{piTextBlock("ok")}, nil))
				imp := piImporterAt(root)
				return collectTurns(t, imp, piPreview(t, imp, path))
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			turns := tt.turns(t)
			if len(turns) == 0 {
				t.Fatal("no turns imported")
			}
			// The import pipeline: the Sanitizer runs on every turn before export.
			gen := turns[0]
			Sanitizer{}.Sanitize(&gen)
			title := gen.Gen.ConversationTitle
			if strings.Contains(title, "ghp_") {
				t.Errorf("title keeps part of the token: %q", title)
			}
			if !strings.Contains(title, "[REDACTED:github-pat]") {
				t.Errorf("title = %q, want the token redacted", title)
			}
		})
	}
}
