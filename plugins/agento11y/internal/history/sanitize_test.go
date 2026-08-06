package history

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"
)

func TestTruncate(t *testing.T) {
	tests := []struct {
		name          string
		in            string
		max           int
		wantOut       string
		wantTruncated bool
	}{
		{name: "short text is untouched", in: "hello", max: 10, wantOut: "hello"},
		{name: "text at the cap is untouched", in: "hello", max: 5, wantOut: "hello"},
		{name: "long text is cut", in: "hello world", max: 5, wantOut: "hello", wantTruncated: true},
		{name: "a zero cap disables truncation", in: "hello", max: 0, wantOut: "hello"},
		{name: "a negative cap disables truncation", in: "hello", max: -1, wantOut: "hello"},
		{name: "a cut inside a rune backs off", in: "aa\u00e9bb", max: 3, wantOut: "aa", wantTruncated: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, info := Truncate(tc.in, tc.max)
			if got != tc.wantOut {
				t.Fatalf("Truncate() = %q, want %q", got, tc.wantOut)
			}
			if info.Truncated != tc.wantTruncated {
				t.Fatalf("Truncated = %v, want %v", info.Truncated, tc.wantTruncated)
			}
			if info.OriginalBytes != len(tc.in) {
				t.Fatalf("OriginalBytes = %d, want %d", info.OriginalBytes, len(tc.in))
			}
			if info.KeptBytes != len(got) {
				t.Fatalf("KeptBytes = %d, want %d", info.KeptBytes, len(got))
			}
		})
	}
}

func TestSanitizeRedactsEveryTextField(t *testing.T) {
	secret := "glc_0123456789abcdefghijklmnopqrstuvwxyz"
	g := HistoricalGeneration{
		Gen: agento11y.Generation{
			SystemPrompt:      "system " + secret,
			ConversationTitle: "title " + secret,
			CallError:         "error " + secret,
			Input: []agento11y.Message{{
				Role: agento11y.RoleUser,
				Parts: []agento11y.Part{{
					Kind: agento11y.PartKindText,
					Text: "prompt " + secret,
				}},
			}},
			Output: []agento11y.Message{{
				Role: agento11y.RoleAssistant,
				Parts: []agento11y.Part{
					{Kind: agento11y.PartKindThinking, Thinking: "thinking " + secret},
					{Kind: agento11y.PartKindToolCall, ToolCall: &agento11y.ToolCall{
						ID:        "call-1",
						Name:      "Bash",
						InputJSON: json.RawMessage(`{"token":"` + secret + `"}`),
					}},
					{Kind: agento11y.PartKindToolResult, ToolResult: &agento11y.ToolResult{
						ToolCallID:  "call-1",
						Content:     "result " + secret,
						ContentJSON: json.RawMessage(`{"token":"` + secret + `"}`),
					}},
				},
			}},
			Tools: []agento11y.ToolDefinition{{
				Name:        "Bash",
				Description: "runs " + secret,
				InputSchema: json.RawMessage(`{"token":"` + secret + `"}`),
			}},
		},
	}

	Sanitizer{}.Sanitize(&g)

	encoded, err := json.Marshal(g.Gen)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("the secret survived sanitization:\n%s", encoded)
	}
	if g.Gen.Tools[0].Name != "Bash" {
		t.Fatalf("tool name was redacted: %q", g.Gen.Tools[0].Name)
	}
	if g.Gen.Output[0].Parts[1].ToolCall.ID != "call-1" {
		t.Fatal("tool call ID was redacted")
	}
	for _, raw := range []json.RawMessage{
		g.Gen.Output[0].Parts[1].ToolCall.InputJSON,
		g.Gen.Output[0].Parts[2].ToolResult.ContentJSON,
		g.Gen.Tools[0].InputSchema,
	} {
		if !json.Valid(raw) {
			t.Fatalf("redaction produced invalid JSON: %s", raw)
		}
	}
}

func TestSanitizeTruncatesAndFlagsQuality(t *testing.T) {
	long := strings.Repeat("a", 100)
	g := HistoricalGeneration{
		Gen: agento11y.Generation{
			Input: []agento11y.Message{{
				Role:  agento11y.RoleUser,
				Parts: []agento11y.Part{{Kind: agento11y.PartKindText, Text: long}},
			}},
		},
	}
	Sanitizer{MaxFieldBytes: 10}.Sanitize(&g)

	if got := g.Gen.Input[0].Parts[0].Text; len(got) != 10 {
		t.Fatalf("text length = %d, want 10", len(got))
	}
	if !g.Quality.Truncated {
		t.Fatal("Quality.Truncated was not set")
	}
}

func TestSanitizeReplacesOversizedJSONWithValidPlaceholder(t *testing.T) {
	g := HistoricalGeneration{
		Gen: agento11y.Generation{
			Output: []agento11y.Message{{
				Role: agento11y.RoleAssistant,
				Parts: []agento11y.Part{{
					Kind: agento11y.PartKindToolCall,
					ToolCall: &agento11y.ToolCall{
						ID:        "call-1",
						InputJSON: json.RawMessage(`{"a":"` + strings.Repeat("b", 100) + `"}`),
					},
				}},
			}},
		},
	}
	Sanitizer{MaxFieldBytes: 20}.Sanitize(&g)

	got := g.Gen.Output[0].Parts[0].ToolCall.InputJSON
	if !json.Valid(got) {
		t.Fatalf("placeholder is not valid JSON: %s", got)
	}
	if string(got) != string(jsonTruncatedPlaceholder) {
		t.Fatalf("InputJSON = %s, want %s", got, jsonTruncatedPlaceholder)
	}
	if !g.Quality.Truncated {
		t.Fatal("Quality.Truncated was not set for an oversized JSON field")
	}
}

func TestSanitizeLeavesUnaffectedTurnsAlone(t *testing.T) {
	g := HistoricalGeneration{
		Gen: agento11y.Generation{
			ConversationTitle: "add a retry",
			Input: []agento11y.Message{{
				Role:  agento11y.RoleUser,
				Parts: []agento11y.Part{{Kind: agento11y.PartKindText, Text: "add a retry to the exporter"}},
			}},
		},
	}
	Sanitizer{}.Sanitize(&g)

	if g.Gen.ConversationTitle != "add a retry" {
		t.Fatalf("title = %q", g.Gen.ConversationTitle)
	}
	if got := g.Gen.Input[0].Parts[0].Text; got != "add a retry to the exporter" {
		t.Fatalf("text = %q", got)
	}
	if g.Quality.Truncated {
		t.Fatal("Quality.Truncated was set for a turn that fits")
	}
}
