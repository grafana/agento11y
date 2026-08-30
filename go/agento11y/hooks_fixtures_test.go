package agento11y

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// Shared helpers for the hook conformance and integration suites: the fixture
// loader, the preflight request builder, the one allowed normalization, and a
// path-reporting JSON diff.

var bashToolSchema = json.RawMessage(
	`{"type":"object","properties":{"command":{"type":"string","description":"Shell command to run."}},"required":["command"]}`,
)

var readFileToolSchema = json.RawMessage(
	`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`,
)

// hookFixturePath resolves a fixture from the working directory, which `go test`
// sets to the package directory: go/agento11y is two levels below the repository
// root.
func hookFixturePath(t *testing.T, name string) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("resolve working directory: %v", err)
	}
	return filepath.Join(wd, "..", "..", "conformance", "hooks", name)
}

func loadHookFixture(t *testing.T, name string) any {
	t.Helper()
	data, err := os.ReadFile(hookFixturePath(t, name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("decode fixture %s: %v", name, err)
	}
	return out
}

func loadHookResponseFixture(t *testing.T, name string) json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(hookFixturePath(t, "responses.json"))
	if err != nil {
		t.Fatalf("read responses fixture: %v", err)
	}
	var fixtures map[string]json.RawMessage
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatalf("decode responses fixture: %v", err)
	}
	body, ok := fixtures[name]
	if !ok {
		t.Fatalf("responses.json has no %q scenario", name)
	}
	return body
}

func parseHookFixtureResponse(t *testing.T, name string) HookEvaluateResponse {
	t.Helper()
	var wire hookWireResponse
	if err := json.Unmarshal(loadHookResponseFixture(t, name), &wire); err != nil {
		t.Fatalf("parse %s response: %v", name, err)
	}
	return wire.toResponse()
}

func reparseHookJSON(t *testing.T, value any) any {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal value: %v", err)
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal value: %v", err)
	}
	return out
}

func hookFixtureInput(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	input, ok := body["input"].(map[string]any)
	if !ok {
		t.Fatalf("fixture has no input object")
	}
	return input
}

func hookFixturePart(t *testing.T, body map[string]any, message, part int) map[string]any {
	t.Helper()
	messages, ok := hookFixtureInput(t, body)["messages"].([]any)
	if !ok || len(messages) <= message {
		t.Fatalf("fixture has no message %d", message)
	}
	msg, ok := messages[message].(map[string]any)
	if !ok {
		t.Fatalf("fixture message %d is not an object", message)
	}
	parts, ok := msg["parts"].([]any)
	if !ok || len(parts) <= part {
		t.Fatalf("fixture message %d has no part %d", message, part)
	}
	out, ok := parts[part].(map[string]any)
	if !ok {
		t.Fatalf("fixture message %d part %d is not an object", message, part)
	}
	return out
}

// stripEmptyHookMetadata removes `metadata` keys whose value is an empty object.
// model.Part.Metadata is a value-type struct, so `omitempty` cannot apply and
// every Go part carries `"metadata":{}`. This is the only normalization the
// contract allows; see conformance/hooks/README.md.
func stripEmptyHookMetadata(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			if key == "metadata" {
				if nested, ok := item.(map[string]any); ok && len(nested) == 0 {
					continue
				}
			}
			out[key] = stripEmptyHookMetadata(item)
		}
		return out
	case []any:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			out = append(out, stripEmptyHookMetadata(item))
		}
		return out
	default:
		return value
	}
}

// diffJSON reports every structural difference between got and want as a
// dotted JSON path plus the two values, so a failure names the offending field
// instead of dumping two payloads.
func diffJSON(path string, got, want any) []string {
	switch wantTyped := want.(type) {
	case map[string]any:
		gotTyped, ok := got.(map[string]any)
		if !ok {
			return []string{fmt.Sprintf("%s: got %s, want an object", hookDiffPath(path), hookDiffValue(got))}
		}
		var diffs []string
		for _, key := range sortedHookKeys(wantTyped, gotTyped) {
			gotValue, gotHas := gotTyped[key]
			wantValue, wantHas := wantTyped[key]
			childPath := key
			if path != "" {
				childPath = path + "." + key
			}
			switch {
			case !gotHas:
				diffs = append(diffs, fmt.Sprintf("%s: missing, want %s", childPath, hookDiffValue(wantValue)))
			case !wantHas:
				diffs = append(diffs, fmt.Sprintf("%s: unexpected %s", childPath, hookDiffValue(gotValue)))
			default:
				diffs = append(diffs, diffJSON(childPath, gotValue, wantValue)...)
			}
		}
		return diffs
	case []any:
		gotTyped, ok := got.([]any)
		if !ok {
			return []string{fmt.Sprintf("%s: got %s, want an array", hookDiffPath(path), hookDiffValue(got))}
		}
		if len(gotTyped) != len(wantTyped) {
			return []string{fmt.Sprintf("%s: got %d items, want %d", hookDiffPath(path), len(gotTyped), len(wantTyped))}
		}
		var diffs []string
		for i := range wantTyped {
			diffs = append(diffs, diffJSON(fmt.Sprintf("%s[%d]", path, i), gotTyped[i], wantTyped[i])...)
		}
		return diffs
	default:
		if !reflect.DeepEqual(got, want) {
			return []string{fmt.Sprintf("%s: got %s, want %s", hookDiffPath(path), hookDiffValue(got), hookDiffValue(want))}
		}
		return nil
	}
}

func hookDiffPath(path string) string {
	if path == "" {
		return "<root>"
	}
	return path
}

func hookDiffValue(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	return string(data)
}

func sortedHookKeys(maps ...map[string]any) []string {
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

// postflightGuardHookRequest builds
// conformance/hooks/request-postflight-guard.json from the public Go SDK types.
// The shipped guards evaluate a tool call under input.output, and the server's
// tool filter scans that field before input.messages.
func postflightGuardHookRequest() HookEvaluateRequest {
	return HookEvaluateRequest{
		Phase: HookPhasePostflight,
		Context: HookContext{
			AgentName:      "conformance-guard",
			AgentVersion:   "1.2.3",
			ConversationID: "conv-hooks-conformance",
			Model:          &HookModel{Provider: "anthropic", Name: "claude-sonnet-4"},
		},
		Input: HookInput{
			Output: []Message{{
				Role: RoleAssistant,
				Parts: []Part{ToolCallPart(ToolCall{
					ID:        "call-bash",
					Name:      "Bash",
					InputJSON: json.RawMessage(`{"command":"rm -rf /tmp/cache"}`),
				})},
			}},
		},
	}
}

// preflightHookRequest builds conformance/hooks/request-preflight.json from the
// public Go SDK types.
func preflightHookRequest() HookEvaluateRequest {
	return HookEvaluateRequest{
		Phase: HookPhasePreflight,
		Context: HookContext{
			AgentName:      "conformance-agent",
			AgentVersion:   "1.2.3",
			Model:          &HookModel{Provider: "anthropic", Name: "claude-sonnet-4"},
			Tags:           map[string]string{"env": "test", "team": "agent-observability"},
			ConversationID: "conv-hooks-conformance",
			TraceID:        "0123456789abcdef0123456789abcdef",
			SpanID:         "0123456789abcdef",
		},
		Input: HookInput{
			SystemPrompt: "You are a careful assistant.",
			Messages: []Message{
				{
					Role:  RoleUser,
					Parts: []Part{TextPart("Delete the cache directory under /tmp.")},
				},
				{
					Role: RoleAssistant,
					Parts: []Part{
						ThinkingPart("The request is destructive, so inspect the directory first."),
						ToolCallPart(ToolCall{
							ID:        "call-read",
							Name:      "read_file",
							InputJSON: json.RawMessage(`{"path":"/tmp/cache/manifest.json"}`),
						}),
						ToolCallPart(ToolCall{
							ID:        "call-bash",
							Name:      "Bash",
							InputJSON: json.RawMessage(`{"command":"rm -rf /tmp/cache"}`),
						}),
					},
				},
				{
					Role: RoleTool,
					Name: "read_file",
					Parts: []Part{ToolResultPart(ToolResult{
						ToolCallID:  "call-read",
						Name:        "read_file",
						Content:     "3 entries",
						ContentJSON: json.RawMessage(`{"entries":3}`),
					})},
				},
				{
					Role: RoleTool,
					Name: "Bash",
					Parts: []Part{ToolResultPart(ToolResult{
						ToolCallID:  "call-bash",
						Name:        "Bash",
						IsError:     true,
						Content:     "rm: cannot remove '/tmp/cache': Permission denied",
						ContentJSON: json.RawMessage(`{"exit_code":1}`),
					})},
				},
			},
			Tools: []ToolDefinition{
				{
					Name:        "Bash",
					Description: "Run a shell command.",
					Type:        "function",
					InputSchema: bashToolSchema,
				},
				{
					Name:        "read_file",
					Description: "Read a file from disk.",
					Type:        "function",
					InputSchema: readFileToolSchema,
				},
			},
			ConversationPreview: "user: Delete the cache directory under /tmp.",
		},
	}
}
