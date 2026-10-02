package kiro

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/mapperutil"
)

func TestMapTurnIdentity(t *testing.T) {
	for _, tc := range []struct{ name, primary, legacy, want string }{
		{name: "unset"},
		{name: "legacy", legacy: "1.2.3", want: "1.2.3"},
		{name: "primary wins", primary: "3.1.0", legacy: "1.2.3", want: "3.1.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENTO11Y_AGENT_VERSION", tc.primary)
			t.Setenv("SIGIL_AGENT_VERSION", tc.legacy)
			t.Setenv("AGENTO11Y_AGENT_NAME", "custom-agent")
			start, gen := mapTurn("session", turn{ID: "turn", CWD: "/workspace"}, agento11y.ContentCaptureModeFull)
			if start.Tags["entrypoint"] != "kiro" || gen.Tags["entrypoint"] != "kiro" || gen.Tags["cwd"] != "/workspace" {
				t.Fatalf("missing source tags: start=%v gen=%v", start.Tags, gen.Tags)
			}
			if start.AgentVersion != tc.want || start.EffectiveVersion != tc.want || gen.AgentVersion != tc.want || gen.EffectiveVersion != tc.want {
				t.Fatalf("version mismatch: start=%q/%q gen=%q/%q", start.AgentVersion, start.EffectiveVersion, gen.AgentVersion, gen.EffectiveVersion)
			}
			if start.AgentName != "custom-agent" || gen.AgentName != "custom-agent" {
				t.Fatal("source tag changed agent override")
			}
		})
	}
}

func TestToolExportBounds(t *testing.T) {
	// This models persisted payloads from before the fix, not just new hooks.
	large, _ := json.Marshal(map[string]string{"value": strings.Repeat("é", 9<<20)})
	small := json.RawMessage(`{"value":"small"}`)
	for _, mode := range []agento11y.ContentCaptureMode{agento11y.ContentCaptureModeFull, agento11y.ContentCaptureModeFullWithMetadataSpans, agento11y.ContentCaptureModeNoToolContent, agento11y.ContentCaptureModeMetadataOnly} {
		t.Run(mode.String(), func(t *testing.T) {
			tr := turn{ID: "turn", Tools: []tool{{Name: "large", Input: large, Response: large}, {Name: "small", Input: small, Response: small}}}
			_, gen := mapTurn("session", tr, mode)
			full := mapperutil.NormalizePayloadContentMode(mode) == agento11y.ContentCaptureModeFull
			input := gen.Output[0].Parts[0].ToolCall.InputJSON
			if full {
				for _, check := range []struct {
					raw   json.RawMessage
					limit int
				}{{input, mapperutil.MaxToolInputBytes}, {gen.Input[0].Parts[0].ToolResult.ContentJSON, mapperutil.MaxToolResultBytes}} {
					var text string
					if err := json.Unmarshal(check.raw, &text); err != nil {
						t.Fatal(err)
					}
					if !utf8.ValidString(text) || !strings.HasSuffix(text, " [truncated]") || len(text) > check.limit+len(" [truncated]") {
						t.Fatal("invalid or unbounded truncated JSON")
					}
				}
				if string(gen.Output[1].Parts[0].ToolCall.InputJSON) != string(small) || string(gen.Input[1].Parts[0].ToolResult.ContentJSON) != string(small) {
					t.Fatal("small payload changed")
				}
			} else if len(input) > 0 {
				t.Fatal("tool content leaked")
			}
			payload, err := json.Marshal(gen)
			if err != nil {
				t.Fatal(err)
			}
			if len(payload) >= 16<<20 {
				t.Fatal("generation still exceeds SDK cap")
			}
			span := toolResult(tool{Input: large, Response: large}, mode)
			if full {
				if len(span.Arguments.(string)) > mapperutil.MaxToolInputBytes+len(" [truncated]") || len(span.Result.(string)) > mapperutil.MaxToolResultBytes+len(" [truncated]") {
					t.Fatal("span content unbounded")
				}
			} else if span.Arguments != nil || span.Result != nil {
				t.Fatal("span content leaked")
			}
		})
	}
}
