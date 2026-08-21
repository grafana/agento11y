package agento11y

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// Hook wire conformance: the request the SDK serializes and the responses it
// parses are checked against the shared fixtures in conformance/hooks/, which
// are the only contract for an endpoint with no generated stubs.

func TestHooksRequestConformance(t *testing.T) {
	for _, tc := range []struct {
		fixture string
		request HookEvaluateRequest
	}{
		{fixture: "request-preflight.json", request: preflightHookRequest()},
		{fixture: "request-postflight-guard.json", request: postflightGuardHookRequest()},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			payload, err := json.Marshal(newHookWireRequest(tc.request))
			if err != nil {
				t.Fatalf("marshal hook request: %v", err)
			}

			var got any
			if err := json.Unmarshal(payload, &got); err != nil {
				t.Fatalf("decode serialized request: %v", err)
			}

			for _, diff := range diffJSON("", stripEmptyHookMetadata(got), loadHookFixture(t, tc.fixture)) {
				t.Errorf("request does not match conformance/hooks/%s: %s", tc.fixture, diff)
			}
		})
	}
}

// TestHookFixtureComparisonNamesDivergentFields pins the comparator, not the
// serializer. TestHooksRequestConformance is what checks the SDK's own output.
// Each case takes the fixture, applies one divergence the server cannot read,
// and asserts the diff names the offending path. Without this test, a comparator
// that accepted a renamed discriminator or a re-encoded payload would pass every
// conformance case while checking nothing.
func TestHookFixtureComparisonNamesDivergentFields(t *testing.T) {
	want := loadHookFixture(t, "request-preflight.json")

	tests := []struct {
		name     string
		mutate   func(map[string]any)
		wantPath string
	}{
		{
			name: "renamed part discriminator",
			mutate: func(body map[string]any) {
				part := hookFixturePart(t, body, 0, 0)
				delete(part, "kind")
				part["type"] = "text"
			},
			wantPath: "input.messages[0].parts[0].kind",
		},
		{
			name: "unknown part discriminator value",
			mutate: func(body map[string]any) {
				hookFixturePart(t, body, 0, 0)["kind"] = "message"
			},
			wantPath: "input.messages[0].parts[0].kind",
		},
		{
			name: "base64 tool call input",
			mutate: func(body map[string]any) {
				toolCall, ok := hookFixturePart(t, body, 1, 1)["tool_call"].(map[string]any)
				if !ok {
					t.Fatalf("fixture tool_call part is not an object")
				}
				raw, err := json.Marshal(toolCall["input_json"])
				if err != nil {
					t.Fatalf("marshal input_json: %v", err)
				}
				toolCall["input_json"] = base64.StdEncoding.EncodeToString(raw)
			},
			wantPath: "input.messages[1].parts[1].tool_call.input_json",
		},
		{
			name: "base64 tool result content",
			mutate: func(body map[string]any) {
				toolResult, ok := hookFixturePart(t, body, 2, 0)["tool_result"].(map[string]any)
				if !ok {
					t.Fatalf("fixture tool_result part is not an object")
				}
				raw, err := json.Marshal(toolResult["content_json"])
				if err != nil {
					t.Fatalf("marshal content_json: %v", err)
				}
				toolResult["content_json"] = base64.StdEncoding.EncodeToString(raw)
			},
			wantPath: "input.messages[2].parts[0].tool_result.content_json",
		},
		{
			name: "raw JSON tool schema",
			mutate: func(body map[string]any) {
				tools, ok := hookFixtureInput(t, body)["tools"].([]any)
				if !ok || len(tools) == 0 {
					t.Fatalf("fixture has no tools")
				}
				tool, ok := tools[0].(map[string]any)
				if !ok {
					t.Fatalf("fixture tool is not an object")
				}
				delete(tool, "input_schema_json")
				tool["input_schema"] = map[string]any{"type": "object"}
			},
			wantPath: "input.tools[0].input_schema_json",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mutated, ok := reparseHookJSON(t, want).(map[string]any)
			if !ok {
				t.Fatalf("fixture is not a JSON object")
			}
			tc.mutate(mutated)
			diffs := diffJSON("", mutated, want)
			if len(diffs) == 0 {
				t.Fatalf("comparison accepted a divergent payload")
			}
			found := false
			for _, diff := range diffs {
				if len(diff) >= len(tc.wantPath) && diff[:len(tc.wantPath)] == tc.wantPath {
					found = true
				}
			}
			if !found {
				t.Errorf("diff did not name %s: %v", tc.wantPath, diffs)
			}
		})
	}
}

// deferred is Go/Python only, so it is not part of the shared fixtures. Keep the
// hook-side encoding covered here.
func TestHooksRequestSerializesDeferredToolDefinitions(t *testing.T) {
	payload, err := json.Marshal(newHookWireRequest(HookEvaluateRequest{
		Phase: HookPhasePreflight,
		Input: HookInput{Tools: []ToolDefinition{
			{Name: "search", Type: "function", InputSchema: readFileToolSchema},
			{Name: "approve_refund", Type: "function", Deferred: true},
		}},
	}))
	if err != nil {
		t.Fatalf("marshal hook request: %v", err)
	}
	var body struct {
		Input struct {
			Tools []map[string]any `json:"tools"`
		} `json:"input"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("decode serialized request: %v", err)
	}
	if len(body.Input.Tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(body.Input.Tools))
	}
	if _, ok := body.Input.Tools[0]["deferred"]; ok {
		t.Errorf("non-deferred tool should omit deferred: %v", body.Input.Tools[0])
	}
	if body.Input.Tools[1]["deferred"] != true {
		t.Errorf("deferred tool lost its flag: %v", body.Input.Tools[1])
	}
	schema, ok := body.Input.Tools[0]["input_schema_json"].(string)
	if !ok {
		t.Fatalf("tool schema is not a base64 string: %v", body.Input.Tools[0])
	}
	decoded, err := base64.StdEncoding.DecodeString(schema)
	if err != nil {
		t.Fatalf("tool schema is not base64: %v", err)
	}
	if string(decoded) != string(readFileToolSchema) {
		t.Errorf("tool schema round-trip failed: %s", decoded)
	}
}

func TestHooksResponseConformanceAllow(t *testing.T) {
	resp := parseHookFixtureResponse(t, "allow")

	if resp.Action != HookActionAllow {
		t.Fatalf("expected allow, got %q", resp.Action)
	}
	if len(resp.Evaluations) != 1 {
		t.Fatalf("expected 1 evaluation, got %d", len(resp.Evaluations))
	}
	eval := resp.Evaluations[0]
	if eval.RuleID != "pii-detect" || eval.EvaluatorID != "evaluator-pii" || eval.EvaluatorKind != "regex" {
		t.Errorf("unexpected evaluation identity: %#v", eval)
	}
	if !eval.Passed || eval.LatencyMs != 12 || eval.Explanation != "no PII matches" {
		t.Errorf("unexpected evaluation outcome: %#v", eval)
	}
	if resp.TransformedInput != nil {
		t.Errorf("allow response should carry no transformed input: %#v", resp.TransformedInput)
	}
}

func TestHooksResponseConformanceDeny(t *testing.T) {
	resp := parseHookFixtureResponse(t, "deny")

	if resp.Action != HookActionDeny {
		t.Fatalf("expected deny, got %q", resp.Action)
	}
	if resp.RuleID != "block-destructive-bash" {
		t.Errorf("unexpected rule id: %q", resp.RuleID)
	}
	if resp.Reason != "Bash(*rm*) is not allowed in this environment" {
		t.Errorf("unexpected reason: %q", resp.Reason)
	}
	err := HookDeniedFromResponse(&resp)
	if err == nil {
		t.Fatal("expected a HookDeniedError for a deny response")
	}
	denied, ok := err.(*HookDeniedError)
	if !ok {
		t.Fatalf("unexpected error type %T", err)
	}
	if denied.RuleID != "block-destructive-bash" || denied.Reason != resp.Reason {
		t.Errorf("denied error lost rule identity: %#v", denied)
	}
}

func TestHooksResponseConformanceTransformedInput(t *testing.T) {
	resp := parseHookFixtureResponse(t, "allow_with_transformed_input")

	if resp.Action != HookActionAllow {
		t.Fatalf("expected allow, got %q", resp.Action)
	}
	input := resp.TransformedInput
	if input == nil {
		t.Fatal("transformed input was dropped")
	}
	if input.SystemPrompt != "You are a careful assistant." {
		t.Errorf("unexpected system prompt: %q", input.SystemPrompt)
	}
	if input.ConversationPreview != "user: Delete the cache directory under [REDACTED]." {
		t.Errorf("unexpected conversation preview: %q", input.ConversationPreview)
	}
	if len(input.Messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(input.Messages))
	}

	if input.Messages[0].Role != RoleUser || len(input.Messages[0].Parts) != 1 {
		t.Fatalf("unexpected first message: %#v", input.Messages[0])
	}
	if part := input.Messages[0].Parts[0]; part.Kind != PartKindText ||
		part.Text != "Delete the cache directory under [REDACTED]." {
		t.Errorf("text part was not preserved: %#v", part)
	}

	assistant := input.Messages[1]
	if assistant.Role != RoleAssistant || len(assistant.Parts) != 2 {
		t.Fatalf("unexpected assistant message: %#v", assistant)
	}
	if assistant.Parts[0].Kind != PartKindThinking ||
		assistant.Parts[0].Thinking != "The request is destructive, so inspect the directory first." {
		t.Errorf("thinking part was not preserved: %#v", assistant.Parts[0])
	}
	callPart := assistant.Parts[1]
	if callPart.Kind != PartKindToolCall || callPart.ToolCall == nil {
		t.Fatalf("tool call part was dropped: %#v", callPart)
	}
	if callPart.ToolCall.ID != "call-bash" || callPart.ToolCall.Name != "Bash" {
		t.Errorf("tool call identity lost: %#v", callPart.ToolCall)
	}
	if got := string(callPart.ToolCall.InputJSON); got != `{"command":"rm -rf /tmp/cache"}` {
		t.Errorf("tool call arguments not base64-decoded: %s", got)
	}

	toolMsg := input.Messages[2]
	if toolMsg.Role != RoleTool || toolMsg.Name != "Bash" || len(toolMsg.Parts) != 1 {
		t.Fatalf("unexpected tool message: %#v", toolMsg)
	}
	resultPart := toolMsg.Parts[0]
	if resultPart.Kind != PartKindToolResult || resultPart.ToolResult == nil {
		t.Fatalf("tool result part was dropped: %#v", resultPart)
	}
	result := resultPart.ToolResult
	if result.ToolCallID != "call-bash" || result.Name != "Bash" || !result.IsError {
		t.Errorf("tool result identity lost: %#v", result)
	}
	if result.Content != "rm: cannot remove '/tmp/cache': Permission denied" {
		t.Errorf("tool result content lost: %q", result.Content)
	}
	if got := string(result.ContentJSON); got != `{"exit_code":1}` {
		t.Errorf("tool result content not base64-decoded: %s", got)
	}

	if len(input.Tools) != 1 {
		t.Fatalf("expected 1 tool definition, got %d", len(input.Tools))
	}
	tool := input.Tools[0]
	if tool.Name != "Bash" || tool.Description != "Run a shell command." || tool.Type != "function" {
		t.Errorf("tool definition lost fields: %#v", tool)
	}
	if got := string(tool.InputSchema); got != string(bashToolSchema) {
		t.Errorf("tool schema not base64-decoded: %s", got)
	}
}

func TestHooksResponseConformanceAcceptsProtoJSONRoles(t *testing.T) {
	// Any emitter that marshals the proto enum directly sends integer roles.
	var wire hookWireResponse
	body := []byte(`{"action":"allow","transformed_input":{"messages":[
		{"role":2,"parts":[{"text":"hello"}]},
		{"role":3,"parts":[{"kind":"tool_result","tool_result":{"tool_call_id":"call-1","content":"ok"}}]}
	]},"evaluations":[]}`)
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	resp := wire.toResponse()
	if resp.TransformedInput == nil || len(resp.TransformedInput.Messages) != 2 {
		t.Fatalf("unexpected transformed input: %#v", resp.TransformedInput)
	}
	first := resp.TransformedInput.Messages[0]
	if first.Role != RoleAssistant {
		t.Errorf("numeric assistant role not mapped: %q", first.Role)
	}
	if len(first.Parts) != 1 || first.Parts[0].Kind != PartKindText || first.Parts[0].Text != "hello" {
		t.Errorf("kindless text part not recovered: %#v", first.Parts)
	}
	second := resp.TransformedInput.Messages[1]
	if second.Role != RoleTool || len(second.Parts) != 1 || second.Parts[0].ToolResult == nil {
		t.Errorf("numeric tool role or tool result lost: %#v", second)
	}
}

func TestHooksResponsePayloadDecodingIsAlwaysJSON(t *testing.T) {
	// Response payloads are base64 of whatever bytes the proto field held, and
	// nothing guarantees those bytes are JSON. Whatever comes back has to stay a
	// valid JSON document: ToolCall.InputJSON is a json.RawMessage, so invalid
	// bytes there fail every later marshal of that part. Python and JS resolve
	// these four cases the same way; see conformance/hooks/README.md.
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{
			name:  "base64 of JSON",
			value: base64.StdEncoding.EncodeToString([]byte(`{"command":"ls /tmp"}`)),
			want:  `{"command":"ls /tmp"}`,
		},
		{
			name:  "base64 of plain text",
			value: base64.StdEncoding.EncodeToString([]byte("plain text tool output")),
			want:  `"plain text tool output"`,
		},
		{
			name:  "embedded JSON text",
			value: `{"command":"ls /tmp"}`,
			want:  `{"command":"ls /tmp"}`,
		},
		{
			name:  "neither base64 nor JSON",
			value: "not base64 either",
			want:  `"not base64 either"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{
				"action": "allow",
				"transformed_input": map[string]any{
					"messages": []any{map[string]any{
						"role": "assistant",
						"parts": []any{map[string]any{
							"kind":      "tool_call",
							"tool_call": map[string]any{"id": "call-1", "name": "Bash", "input_json": tc.value},
						}},
					}},
				},
			})
			if err != nil {
				t.Fatalf("build response: %v", err)
			}
			var wire hookWireResponse
			if err := json.Unmarshal(body, &wire); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			resp := wire.toResponse()
			if resp.TransformedInput == nil || len(resp.TransformedInput.Messages) != 1 {
				t.Fatalf("transformed input was dropped: %#v", resp.TransformedInput)
			}
			part := resp.TransformedInput.Messages[0].Parts[0]
			if part.ToolCall == nil {
				t.Fatalf("tool call part was dropped: %#v", part)
			}
			if got := string(part.ToolCall.InputJSON); got != tc.want {
				t.Errorf("decoded payload: got %s, want %s", got, tc.want)
			}
			// A transformed part a caller cannot re-export or re-send is not a
			// usable transform.
			if _, err := json.Marshal(part); err != nil {
				t.Errorf("transformed part cannot be marshalled again: %v", err)
			}
		})
	}
}

func TestHooksResponseMalformedToolSchemaKeepsTheVerdict(t *testing.T) {
	// input_schema_json is base64 on the response path. A value that is not
	// decodable must not fail the unmarshal of the whole body. That error routes to
	// failOpenOrError, and the caller loses a deny it has to enforce.
	tests := []struct {
		name   string
		schema string
	}{
		{name: "raw JSON object", schema: `{"type":"object"}`},
		{name: "unpadded base64", schema: `"eyJhIjoxfQ"`},
		{name: "plain text", schema: `"not base64 either"`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(
				`{"action":"deny","rule_id":"block-destructive-bash","reason":"denied",
				  "transformed_input":{"tools":[{"name":"Bash","input_schema_json":%s}]},
				  "evaluations":[]}`, tc.schema)
			var wire hookWireResponse
			if err := json.Unmarshal([]byte(body), &wire); err != nil {
				t.Fatalf("a malformed tool schema must not fail the response decode: %v", err)
			}
			resp := wire.toResponse()
			if resp.Action != HookActionDeny || resp.RuleID != "block-destructive-bash" {
				t.Fatalf("deny was lost: %#v", resp)
			}
			if HookDeniedFromResponse(&resp) == nil {
				t.Error("deny response must still produce a denial")
			}
		})
	}
}

func TestHooksRequestKeepsAnUnparsablePayload(t *testing.T) {
	// Streaming providers accumulate input_json_delta without validating it, so a
	// truncated payload reaches the hook path. Failing the marshal would turn a
	// payload problem into a fail-open allow or a fail-closed error, and Python
	// and JS both send the text instead.
	payload, err := json.Marshal(newHookWireRequest(HookEvaluateRequest{
		Phase: HookPhasePreflight,
		Input: HookInput{Messages: []Message{{
			Role: RoleAssistant,
			Parts: []Part{
				ToolCallPart(ToolCall{ID: "call-1", Name: "Bash", InputJSON: json.RawMessage(`{"command":"truncat`)}),
				ToolResultPart(ToolResult{ToolCallID: "call-1", ContentJSON: json.RawMessage(`{"exit_code`)}),
			},
		}}},
	}))
	if err != nil {
		t.Fatalf("an unparsable payload must not fail the request marshal: %v", err)
	}

	var body struct {
		Input struct {
			Messages []struct {
				Parts []map[string]any `json:"parts"`
			} `json:"messages"`
		} `json:"input"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("decode serialized request: %v", err)
	}
	parts := body.Input.Messages[0].Parts
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(parts))
	}
	call, ok := parts[0]["tool_call"].(map[string]any)
	if !ok {
		t.Fatalf("tool call part lost its payload: %v", parts[0])
	}
	if got := call["input_json"]; got != `{"command":"truncat` {
		t.Errorf("unparsable tool arguments: got %#v, want the original text as a JSON string", got)
	}
	result, ok := parts[1]["tool_result"].(map[string]any)
	if !ok {
		t.Fatalf("tool result part lost its payload: %v", parts[1])
	}
	if got := result["content_json"]; got != `{"exit_code` {
		t.Errorf("unparsable tool result content: got %#v, want the original text as a JSON string", got)
	}
}

func TestHooksRequestDropsPartsTheServerCannotRead(t *testing.T) {
	// The server has no media kind, and its default branch would decode a media part
	// as an empty text part. The hook serializer therefore drops any part without a
	// payload the server can read, which is what Python and JS do. Go and Python can
	// both hold a media part; the JS MessagePart union has no media member, so JS has
	// none to drop. A kind-less part is Go-only. A message left with no parts
	// serializes as [] in all three SDKs.
	payload, err := json.Marshal(newHookWireRequest(HookEvaluateRequest{
		Phase: HookPhasePreflight,
		Input: HookInput{Messages: []Message{
			{Role: RoleUser, Parts: []Part{
				{Kind: PartKindMedia, Media: &Media{Kind: "image", URL: "https://example.test/a.png"}},
				TextPart(""),
				ThinkingPart(""),
				{Kind: PartKindToolCall},
				{Kind: PartKindToolResult},
				TextPart("describe this"),
			}},
			{Role: RoleUser, Parts: []Part{
				{Kind: PartKindMedia, Media: &Media{Kind: "image", URL: "https://example.test/b.png"}},
			}},
		}},
	}))
	if err != nil {
		t.Fatalf("marshal hook request: %v", err)
	}

	var body struct {
		Input struct {
			Messages []map[string]any `json:"messages"`
		} `json:"input"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("decode serialized request: %v", err)
	}
	if len(body.Input.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(body.Input.Messages))
	}
	first, ok := body.Input.Messages[0]["parts"].([]any)
	if !ok || len(first) != 1 {
		t.Fatalf("expected only the non-empty text part to survive, got %v", body.Input.Messages[0]["parts"])
	}
	if kind := first[0].(map[string]any)["kind"]; kind != string(PartKindText) {
		t.Errorf("surviving part is not text: %v", first[0])
	}
	second, ok := body.Input.Messages[1]["parts"].([]any)
	if !ok {
		t.Fatalf("a part-less message must serialize parts as [], got %v", body.Input.Messages[1]["parts"])
	}
	if len(second) != 0 {
		t.Errorf("expected no parts, got %v", second)
	}
}

func TestHooksResponseEmptyTransformedInputIsNoTransform(t *testing.T) {
	// The server emits transformed_input:{} whenever a rule returns a non-nil but
	// empty HookGenerationInput. A caller checking only for nil would replace the
	// prompt with nothing. Python and JS report no transform for this body.
	var wire hookWireResponse
	if err := json.Unmarshal([]byte(`{"action":"allow","transformed_input":{},"evaluations":[]}`), &wire); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp := wire.toResponse(); resp.TransformedInput != nil {
		t.Errorf("an empty transformed_input is not a transform: %#v", resp.TransformedInput)
	}
}

func TestHooksResponsePartParsing(t *testing.T) {
	// kind names the field the parser reads, and the parser commits to it: a part
	// that lost that field is dropped rather than rebuilt from a leftover field,
	// which would report a part the rule never wrote. An empty payload carries no
	// content either, and keeping it would append an empty part that the request
	// serializer drops on the way back out. A tool call without a name goes too,
	// because the caller can neither route nor re-send it. A part with no kind at
	// all is still read from whichever payload field is set, because the server
	// always sets kind, so that shape only reaches the SDK from a hand-written or
	// protobuf-JSON body.
	//
	// Python and JS resolve every case here the same way.
	tests := []struct {
		name string
		part string
		want *Part
	}{
		{name: "text", part: `{"kind":"text","text":"kept"}`, want: &Part{Kind: PartKindText, Text: "kept"}},
		{name: "text without text", part: `{"kind":"text","text":""}`},
		// The shape the server emits for an empty text part: its encoder omits the
		// field rather than sending "".
		{name: "text without a text field", part: `{"kind":"text"}`},
		{
			name: "thinking",
			part: `{"kind":"thinking","thinking":"planning"}`,
			want: &Part{Kind: PartKindThinking, Thinking: "planning"},
		},
		{name: "thinking without thinking", part: `{"kind":"thinking","thinking":""}`},
		{name: "thinking without a thinking field", part: `{"kind":"thinking"}`},
		{
			name: "tool call",
			part: `{"kind":"tool_call","tool_call":{"id":"call-1","name":"Bash"}}`,
			want: &Part{Kind: PartKindToolCall, ToolCall: &ToolCall{ID: "call-1", Name: "Bash"}},
		},
		{name: "tool call without payload", part: `{"kind":"tool_call"}`},
		{name: "tool call without name", part: `{"kind":"tool_call","tool_call":{"id":"call-1"}}`},
		{
			name: "tool result",
			part: `{"kind":"tool_result","tool_result":{"tool_call_id":"call-1","content":"ok"}}`,
			want: &Part{Kind: PartKindToolResult, ToolResult: &ToolResult{ToolCallID: "call-1", Content: "ok"}},
		},
		{name: "tool result without payload", part: `{"kind":"tool_result"}`},
		{name: "unknown kind", part: `{"kind":"image"}`},
		{
			name: "unknown kind with text",
			part: `{"kind":"image","text":"described by the server as text"}`,
			want: &Part{Kind: PartKindText, Text: "described by the server as text"},
		},
		{name: "unknown kind with a tool call", part: `{"kind":"image","tool_call":{"name":"Bash"}}`},
		{name: "tool call kind with leftover text", part: `{"kind":"tool_call","text":"not a tool call"}`},
		{name: "tool result kind with leftover text", part: `{"kind":"tool_result","text":"not a tool result"}`},
		{name: "thinking kind with leftover text", part: `{"kind":"thinking","thinking":"","text":"not thinking"}`},
		{name: "text kind with leftover thinking", part: `{"kind":"text","text":"","thinking":"not text"}`},
		{name: "text kind with a leftover tool call", part: `{"kind":"text","text":"","tool_call":{"name":"Bash"}}`},
		{name: "no kind with text", part: `{"text":"recovered text"}`, want: &Part{Kind: PartKindText, Text: "recovered text"}},
		{
			name: "no kind with thinking",
			part: `{"thinking":"recovered thinking"}`,
			want: &Part{Kind: PartKindThinking, Thinking: "recovered thinking"},
		},
		{
			name: "no kind with a tool call",
			part: `{"tool_call":{"id":"call-1","name":"Bash"}}`,
			want: &Part{Kind: PartKindToolCall, ToolCall: &ToolCall{ID: "call-1", Name: "Bash"}},
		},
		{
			name: "no kind with a tool result",
			part: `{"tool_result":{"tool_call_id":"call-1","content":"ok"}}`,
			want: &Part{Kind: PartKindToolResult, ToolResult: &ToolResult{ToolCallID: "call-1", Content: "ok"}},
		},
		{name: "empty part", part: `{}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(
				`{"action":"allow","transformed_input":{"messages":[{"role":"assistant","parts":[%s]}]},"evaluations":[]}`,
				tc.part)
			var wire hookWireResponse
			if err := json.Unmarshal([]byte(body), &wire); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			resp := wire.toResponse()
			if resp.TransformedInput == nil || len(resp.TransformedInput.Messages) != 1 {
				t.Fatalf("unexpected transformed input: %#v", resp.TransformedInput)
			}
			want := make([]Part, 0, 1)
			if tc.want != nil {
				want = append(want, *tc.want)
			}
			got := resp.TransformedInput.Messages[0].Parts
			if !reflect.DeepEqual(got, want) {
				t.Errorf("got %s, want %s", formatParts(got), formatParts(want))
			}
		})
	}
}

// formatParts renders parts with their payloads dereferenced, which %#v does not.
func formatParts(parts []Part) string {
	rendered := make([]string, 0, len(parts))
	for _, part := range parts {
		switch {
		case part.ToolCall != nil:
			rendered = append(rendered, fmt.Sprintf("{%s %#v}", part.Kind, *part.ToolCall))
		case part.ToolResult != nil:
			rendered = append(rendered, fmt.Sprintf("{%s %#v}", part.Kind, *part.ToolResult))
		default:
			rendered = append(rendered, fmt.Sprintf("{%s %q %q}", part.Kind, part.Text, part.Thinking))
		}
	}
	return "[" + strings.Join(rendered, " ") + "]"
}
