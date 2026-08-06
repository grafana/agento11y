package agento11y

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// stringFixtures mirrors redaction/fixtures/strings.json, the shared case file
// every redaction engine loads. Keeping the shape here rather than in a helper
// package keeps the SDK free of test-only dependencies.
type stringFixtures struct {
	Cases []stringCase `json:"cases"`
}

type stringCase struct {
	ID       string `json:"id"`
	Mode     string `json:"mode"`
	Emails   bool   `json:"emails"`
	Input    string `json:"input"`
	Expected string `json:"expected"`
}

func loadStringFixtures(t *testing.T) stringFixtures {
	t.Helper()
	path := filepath.Join("..", "..", "redaction", "fixtures", "strings.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var fixtures stringFixtures
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if len(fixtures.Cases) == 0 {
		t.Fatalf("%s has no cases", path)
	}
	return fixtures
}

func TestConformanceRedactionStrings(t *testing.T) {
	for _, tc := range loadStringFixtures(t).Cases {
		t.Run(tc.ID, func(t *testing.T) {
			var got string
			switch tc.Mode {
			case "full":
				got = redactFull(tc.Input, tc.Emails)
			case "light":
				got = redactLight(tc.Input, tc.Emails)
			default:
				t.Fatalf("unknown mode %q", tc.Mode)
			}
			if got != tc.Expected {
				t.Errorf("mode=%s emails=%v\n input: %q\n   got: %q\n  want: %q", tc.Mode, tc.Emails, tc.Input, got, tc.Expected)
			}
		})
	}
}

// TestConformanceRedactionFixtureCoverage keeps the shared fixture file honest:
// a pattern with no positive case can be mis-escaped in one language and still
// pass every suite.
func TestConformanceRedactionFixtureCoverage(t *testing.T) {
	cases := loadStringFixtures(t).Cases

	seen := map[string]map[string]bool{}
	for _, tc := range cases {
		for _, id := range markerIDs(tc.Expected) {
			if seen[id] == nil {
				seen[id] = map[string]bool{}
			}
			seen[id][tc.Mode] = true
		}
	}

	for _, p := range tier1Patterns {
		if !seen[p.id]["full"] || !seen[p.id]["light"] {
			t.Errorf("tier 1 pattern %q needs a positive fixture in both full and light mode", p.id)
		}
	}
	if !seen[emailPattern.id]["full"] && !seen[emailPattern.id]["light"] {
		t.Errorf("email pattern needs a positive fixture")
	}
	for _, p := range tier2Patterns {
		if !seen[p.id]["full"] {
			t.Errorf("tier 2 pattern %q needs a positive fixture in full mode", p.id)
		}
	}
}

var markerPattern = regexp.MustCompile(`\[REDACTED:([a-z0-9-]+)\]`)

// markerIDs extracts the pattern ids from the `[REDACTED:<id>]` markers in an
// expected value.
func markerIDs(expected string) []string {
	var ids []string
	for _, match := range markerPattern.FindAllStringSubmatch(expected, -1) {
		ids = append(ids, match[1])
	}
	return ids
}

// generationFixtures mirrors redaction/fixtures/generations.json.
type generationFixtures struct {
	Probe map[string]string `json:"probe"`
	Cases []generationCase  `json:"cases"`
}

type generationCase struct {
	ID                   string            `json:"id"`
	RedactInputMessages  bool              `json:"redactInputMessages"`
	RedactEmailAddresses bool              `json:"redactEmailAddresses"`
	Slots                map[string]string `json:"slots"`
}

func loadGenerationFixtures(t *testing.T) generationFixtures {
	t.Helper()
	path := filepath.Join("..", "..", "redaction", "fixtures", "generations.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var fixtures generationFixtures
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if len(fixtures.Cases) == 0 {
		t.Fatalf("%s has no cases", path)
	}
	return fixtures
}

// buildProbeGeneration fills every slot in the matrix with the same probe so
// the sanitizer's per-slot mode is what the assertions read back. The media
// part is Go-only and has no slot in the shared matrix; the test asserts on it
// directly.
func buildProbeGeneration(probe string) Generation {
	toolParts := func() []Part {
		return []Part{
			{Kind: PartKindText, Text: probe},
			{Kind: PartKindToolResult, ToolResult: &ToolResult{
				Name:        "bash",
				Content:     probe,
				ContentJSON: []byte(probe),
			}},
		}
	}
	assistantParts := func() []Part {
		return []Part{
			{Kind: PartKindText, Text: probe},
			{Kind: PartKindThinking, Thinking: probe},
			{Kind: PartKindToolCall, ToolCall: &ToolCall{Name: "bash", InputJSON: []byte(probe)}},
		}
	}

	return Generation{
		SystemPrompt:      probe,
		ConversationTitle: probe,
		CallError:         probe,
		Input: []Message{
			{Role: RoleUser, Parts: []Part{
				{Kind: PartKindText, Text: probe},
				{Kind: PartKindMedia, Media: &Media{Kind: "image", URL: probe}},
			}},
			{Role: RoleAssistant, Parts: assistantParts()},
			{Role: RoleTool, Parts: toolParts()},
		},
		Output: []Message{
			{Role: RoleAssistant, Parts: assistantParts()},
			{Role: RoleTool, Parts: toolParts()},
		},
	}
}

func generationSlotValues(g Generation) map[string]string {
	return map[string]string{
		"systemPrompt":                       g.SystemPrompt,
		"conversationTitle":                  g.ConversationTitle,
		"callError":                          g.CallError,
		"input.user.text":                    g.Input[0].Parts[0].Text,
		"input.assistant.text":               g.Input[1].Parts[0].Text,
		"input.assistant.thinking":           g.Input[1].Parts[1].Thinking,
		"input.assistant.toolCallInputJson":  string(g.Input[1].Parts[2].ToolCall.InputJSON),
		"input.tool.text":                    g.Input[2].Parts[0].Text,
		"input.tool.toolResultContent":       g.Input[2].Parts[1].ToolResult.Content,
		"input.tool.toolResultContentJson":   string(g.Input[2].Parts[1].ToolResult.ContentJSON),
		"output.assistant.text":              g.Output[0].Parts[0].Text,
		"output.assistant.thinking":          g.Output[0].Parts[1].Thinking,
		"output.assistant.toolCallInputJson": string(g.Output[0].Parts[2].ToolCall.InputJSON),
		"output.tool.text":                   g.Output[1].Parts[0].Text,
		"output.tool.toolResultContent":      g.Output[1].Parts[1].ToolResult.Content,
		"output.tool.toolResultContentJson":  string(g.Output[1].Parts[1].ToolResult.ContentJSON),
	}
}

func TestConformanceRedactionGenerationSlots(t *testing.T) {
	fixtures := loadGenerationFixtures(t)

	for _, tc := range fixtures.Cases {
		t.Run(tc.ID, func(t *testing.T) {
			sanitizer := newSecretRedactionSanitizer(
				func(string) (string, bool) { return "", false },
				SecretRedactionOptions{
					RedactInputMessages:  &tc.RedactInputMessages,
					RedactEmailAddresses: &tc.RedactEmailAddresses,
				},
			)

			sanitized := sanitizer(buildProbeGeneration(fixtures.Probe["input"]))
			got := generationSlotValues(sanitized)

			for slot, mode := range tc.Slots {
				want, ok := fixtures.Probe[mode]
				if !ok {
					t.Fatalf("slot %s: unknown mode %q", slot, mode)
				}
				actual, ok := got[slot]
				if !ok {
					t.Fatalf("slot %s is in the fixture but not built by the Go harness", slot)
				}
				if actual != want {
					t.Errorf("slot %s (mode %s)\n got: %q\nwant: %q", slot, mode, actual, want)
				}
			}

			for slot := range got {
				if _, ok := tc.Slots[slot]; !ok {
					t.Errorf("slot %s is built by the Go harness but missing from the fixture", slot)
				}
			}

			// Media is the one part kind the other three SDKs have no equivalent
			// for, so it stays out of the shared matrix. The sanitizer leaves it
			// alone; metadata-only capture strips media URLs instead.
			if url := sanitized.Input[0].Parts[1].Media.URL; url != fixtures.Probe["skip"] {
				t.Errorf("media url\n got: %q\nwant: %q", url, fixtures.Probe["skip"])
			}
		})
	}
}
