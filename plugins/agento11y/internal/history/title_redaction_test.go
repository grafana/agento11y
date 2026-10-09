package history

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/chatstore/chatstoretest"
)

// Regression: the Claude Code, Codex and Pi importers cut the conversation
// title to 100 bytes before the Sanitizer redacted it, and Cursor's import
// redacted only tier 1 before its cut, for a prompt and for a chat name alike.
// A secret the cut split no longer matched its pattern, so its first part was
// exported in the title while the same prompt's input was redacted.
func TestImportedTitlesRedactASecretTheCutSplits(t *testing.T) {
	prompts := []struct {
		name string
		text string
		// leak is a part of the secret the title must not keep.
		leak string
	}{
		{
			// The token ends past byte 100, but its marker fits.
			name: "a token across the cut",
			text: strings.Repeat("x", 70) + " ghp_" + strings.Repeat("A", 36) + " please",
			leak: "ghp_",
		},
		{
			// Redacted, the marker itself crosses byte 100.
			name: "a token whose marker crosses the cut",
			text: strings.Repeat("x", 85) + " ghp_" + strings.Repeat("A", 36) + " please",
			leak: "ghp_",
		},
		{
			// A tier-2 key-value secret, matched only with its closing quote.
			name: "a key-value secret across the cut",
			text: strings.Repeat("x", 80) + ` "api_key": "supersecretvalue123" please`,
			leak: "superse",
		},
		{
			// The key only matches once the cut gives it a word boundary, so
			// the Sanitizer's pass over the cut title found it.
			name: "a key the cut makes match",
			text: strings.Repeat("x", 79) + " AKIA" + strings.Repeat("B", 16) + "XYZ please",
			leak: "AKIA",
		},
		{
			// One pass redacts the key and leaves the token after it.
			name: "a token only a second pass finds",
			text: strings.Repeat("x", 20) + " SK" + strings.Repeat("0a", 16) + "ghp_" + strings.Repeat("A", 36) + " please",
			leak: "ghp_",
		},
	}
	importers := []struct {
		name  string
		turns func(t *testing.T, prompt string) []HistoricalGeneration
	}{
		{name: "claude-code", turns: claudeTurnsForPrompt},
		{name: "codex", turns: codexTurnsForPrompt},
		{name: "pi", turns: piTurnsForPrompt},
		{name: "cursor transcript", turns: cursorTurnsForPrompt},
		{name: "cursor store", turns: cursorStoreTurnsForPrompt},
		{name: "cursor store chat name", turns: cursorStoreTurnsForChatName},
	}
	for _, imp := range importers {
		for _, p := range prompts {
			t.Run(imp.name+"/"+p.name, func(t *testing.T) {
				turns := imp.turns(t, p.text)
				if len(turns) == 0 {
					t.Fatal("no turns imported")
				}
				// The import pipeline: the Sanitizer runs on every turn before export.
				gen := turns[0]
				Sanitizer{}.Sanitize(&gen)
				title := gen.Gen.ConversationTitle
				// Redacted as the Sanitizer redacts, the title leaves it nothing to do.
				if before := turns[0].Gen.ConversationTitle; title != before {
					t.Errorf("the Sanitizer changed the title from %q to %q", before, title)
				}
				if strings.Contains(title, p.leak) {
					t.Errorf("title keeps part of the secret: %q", title)
				}
				if at := strings.LastIndex(title, "[REDACT"); at >= 0 && !strings.Contains(title[at:], "]") {
					t.Errorf("title ends in part of a redaction marker: %q", title)
				}
				if !strings.HasPrefix(title, "xxxxxxxxxx") {
					t.Errorf("title = %q, want the prompt's start", title)
				}
			})
		}
	}
}

// Regression: Codex collapses a prompt's whitespace for its title, and it did
// so after redacting. A separator the patterns do not take as space, such as \v
// after "Bearer", collapsed into one they do, and the cut had already split the
// token, so the Sanitizer could no longer match it either.
func TestCodexTitleRedactsASecretCollapsingWhitespaceReveals(t *testing.T) {
	turns := codexTurnsForPrompt(t, strings.Repeat("x", 80)+" Bearer\v"+strings.Repeat("A", 40)+" please")
	if len(turns) == 0 {
		t.Fatal("no turns imported")
	}
	gen := turns[0]
	Sanitizer{}.Sanitize(&gen)
	if title := gen.Gen.ConversationTitle; title != strings.Repeat("x", 80) {
		t.Errorf("title = %q, want the prompt up to the token", title)
	}
}

func claudeTurnsForPrompt(t *testing.T, prompt string) []HistoricalGeneration {
	t.Helper()
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
}

func codexTurnsForPrompt(t *testing.T, prompt string) []HistoricalGeneration {
	t.Helper()
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
}

func piTurnsForPrompt(t *testing.T, prompt string) []HistoricalGeneration {
	t.Helper()
	root, sid := t.TempDir(), "019ead07-cfbf-78d3-8b03-875769426583"
	path := writeFile(t, filepath.Join(root, "--work-repo--", "2026-06-09T15-37-10-848Z_"+sid+".jsonl"),
		piHeader(t, sid, "/work/repo", "2026-06-09T15:37:10.848Z")+
			piUserEntry(t, "u1", "", "2026-06-09T15:37:20.000Z", prompt, 1781019439000)+
			piAssistantEntry(t, "a1", "u1", "2026-06-09T15:37:24.000Z", 1781019441000,
				[]map[string]any{piTextBlock("ok")}, nil))
	imp := piImporterAt(root)
	return collectTurns(t, imp, piPreview(t, imp, path))
}

func cursorTurnsForPrompt(t *testing.T, prompt string) []HistoricalGeneration {
	t.Helper()
	line := func(entry map[string]any) string {
		data, err := json.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	imp := &cursorImporter{}
	path := writeCursorTranscriptLines(t, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		line(map[string]any{"role": "user", "message": map[string]any{"content": []map[string]any{{"type": "text", "text": "<user_query>\n" + prompt + "\n</user_query>"}}}}),
		line(map[string]any{"role": "assistant", "message": map[string]any{"content": []map[string]any{{"type": "text", "text": "ok"}}}}))
	turns, _ := cursorWalk(t, imp, cursorPreview(t, imp, path))
	return turns
}

func cursorStoreTurnsForPrompt(t *testing.T, prompt string) []HistoricalGeneration {
	t.Helper()
	imp, path := cursorBuild(t, "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d", func(b *chatstoretest.Builder) {
		b.AddPrompt("<user_query>" + prompt + "</user_query>")
		b.AddAssistantText("ok")
	})
	turns, _ := cursorWalk(t, imp, cursorPreview(t, imp, path))
	return turns
}

// cursorStoreTurnsForChatName names the store's chat with name. The session
// types nothing beside an image, so the chat name is its title.
func cursorStoreTurnsForChatName(t *testing.T, name string) []HistoricalGeneration {
	t.Helper()
	imp, path := cursorBuild(t, "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d", func(b *chatstoretest.Builder) {
		b.Meta.Name = name
		b.AddPrompt("<image_files>\n1. /work/repo/a.png\n</image_files>\n<user_query>\n</user_query>")
		b.AddAssistantText("ok")
	})
	turns, _ := cursorWalk(t, imp, cursorPreview(t, imp, path))
	return turns
}
