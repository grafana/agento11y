package local

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/guardeval"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEncodeHookEvaluateResponseUsesSnakeCaseAndBase64Payloads(t *testing.T) {
	req, err := decodeHookEvaluateRequest([]byte(`{"phase":"postflight","context":{"agentName":"pi"},"input":{"output":[{"role":"assistant","parts":[{"type":"tool_call","toolCall":{"id":"c1","name":"Bash","inputJSON":"{\"command\":\"echo safe\"}"}}]}]}}`))
	require.NoError(t, err)
	require.Len(t, req.Input.Output, 1)
	require.Len(t, req.Input.Output[0].Parts, 1)

	req.Input.Output[0].Parts = append(req.Input.Output[0].Parts, agento11y.Part{
		Kind: agento11y.PartKindToolResult,
		ToolResult: &agento11y.ToolResult{
			ToolCallID:  "c1",
			ContentJSON: json.RawMessage(`{"ok":true}`),
		},
	})
	req.Input.Tools = []agento11y.ToolDefinition{{
		Name:        "Bash",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}}

	raw, err := json.Marshal(encodeHookEvaluateResponse(guardeval.Response{
		Action:           agento11y.HookActionAllow,
		TransformedInput: &req.Input,
		Evaluations:      []guardeval.Evaluation{},
	}))
	require.NoError(t, err)
	assert.NotContains(t, string(raw), `"type":"tool_call"`)
	assert.NotContains(t, string(raw), `"toolCall"`)
	assert.NotContains(t, string(raw), `"inputJSON"`)
	assert.NotContains(t, string(raw), `"input_schema"`)

	var out struct {
		TransformedInput struct {
			Output []struct {
				Parts []struct {
					Kind     string `json:"kind"`
					ToolCall *struct {
						InputJSON string `json:"input_json"`
					} `json:"tool_call"`
					ToolResult *struct {
						ContentJSON string `json:"content_json"`
					} `json:"tool_result"`
				} `json:"parts"`
			} `json:"output"`
			Tools []struct {
				InputSchemaJSON string `json:"input_schema_json"`
			} `json:"tools"`
		} `json:"transformed_input"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	parts := out.TransformedInput.Output[0].Parts
	assert.Equal(t, "tool_call", parts[0].Kind)
	require.NotNil(t, parts[0].ToolCall)
	assertBase64JSONEq(t, `{"command":"echo safe"}`, parts[0].ToolCall.InputJSON)
	require.NotNil(t, parts[1].ToolResult)
	assertBase64JSONEq(t, `{"ok":true}`, parts[1].ToolResult.ContentJSON)
	require.Len(t, out.TransformedInput.Tools, 1)
	assertBase64JSONEq(t, `{"type":"object"}`, out.TransformedInput.Tools[0].InputSchemaJSON)
}

func TestDecodeHookEvaluateResponseNormalizesPayloadsOnce(t *testing.T) {
	const (
		input   = `{"command":"echo safe"}`
		content = `{"ok":true}`
		schema  = `{"type":"object"}`
	)
	body := []byte(`{"action":"allow","transformed_input":{"output":[{"role":"assistant","parts":[` +
		`{"kind":"tool_call","tool_call":{"id":"c1","name":"Bash","input_json":"` + base64.StdEncoding.EncodeToString([]byte(input)) + `"}},` +
		`{"kind":"tool_result","tool_result":{"tool_call_id":"c1","content_json":"` + base64.StdEncoding.EncodeToString([]byte(content)) + `"}}]}],` +
		`"tools":[{"name":"Bash","input_schema_json":"` + base64.StdEncoding.EncodeToString([]byte(schema)) + `"}]}}`)

	resp, err := decodeHookEvaluateResponse(body)
	require.NoError(t, err)
	require.NotNil(t, resp.TransformedInput)
	parts := resp.TransformedInput.Output[0].Parts
	require.Len(t, parts, 2)
	assert.JSONEq(t, input, string(parts[0].ToolCall.InputJSON))
	assert.JSONEq(t, content, string(parts[1].ToolResult.ContentJSON))
	require.Len(t, resp.TransformedInput.Tools, 1)
	assert.JSONEq(t, schema, string(resp.TransformedInput.Tools[0].InputSchema))

	reencoded, err := json.Marshal(encodeHookEvaluateResponse(guardeval.FromHookResponse(resp)))
	require.NoError(t, err)
	var wire struct {
		TransformedInput struct {
			Output []struct {
				Parts []struct {
					ToolCall *struct {
						InputJSON string `json:"input_json"`
					} `json:"tool_call"`
					ToolResult *struct {
						ContentJSON string `json:"content_json"`
					} `json:"tool_result"`
				} `json:"parts"`
			} `json:"output"`
			Tools []struct {
				InputSchemaJSON string `json:"input_schema_json"`
			} `json:"tools"`
		} `json:"transformed_input"`
	}
	require.NoError(t, json.Unmarshal(reencoded, &wire))
	assert.Equal(t, base64.StdEncoding.EncodeToString([]byte(input)), wire.TransformedInput.Output[0].Parts[0].ToolCall.InputJSON)
	assert.Equal(t, base64.StdEncoding.EncodeToString([]byte(content)), wire.TransformedInput.Output[0].Parts[1].ToolResult.ContentJSON)
	assert.Equal(t, base64.StdEncoding.EncodeToString([]byte(schema)), wire.TransformedInput.Tools[0].InputSchemaJSON)
}

func TestDecodeResponseWireJSONAlwaysReturnsJSON(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{name: "base64_JSON", raw: `"eyJvayI6dHJ1ZX0="`, want: `{"ok":true}`},
		{name: "base64_text", raw: `"aGVsbG8="`, want: `"hello"`},
		{name: "JSON_in_string", raw: `"{\"ok\":true}"`, want: `{"ok":true}`},
		{name: "plain_string", raw: `"not base64!"`, want: `"not base64!"`},
		{name: "embedded_JSON", raw: `{"ok":true}`, want: `{"ok":true}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decodeResponseWireJSON(json.RawMessage(tc.raw))
			require.True(t, json.Valid(got))
			assert.JSONEq(t, tc.want, string(got))
		})
	}
}

func TestEncodeRelayBodyUsesRequestPartsAndWireTools(t *testing.T) {
	const schema = `{"type":"object"}`
	body := []byte(`{"phase":"postflight","context":{"agent_name":"claude-code","tags":{"route":"keep"}},"input":{"tools":[{"name":"Bash","input_schema_json":"` + base64.StdEncoding.EncodeToString([]byte(schema)) + `","deferred":true}],"output":[{"role":"assistant","parts":[{"kind":"tool_call","tool_call":{"id":"c1","name":"Bash","input_json":{"command":"echo safe"}}}]}]}}`)
	req, err := decodeHookEvaluateRequest(body)
	require.NoError(t, err)
	require.Len(t, req.Input.Tools, 1)
	assert.True(t, req.Input.Tools[0].Deferred)

	raw, err := encodeRelayBody(req.Phase, req.Context, req.Input)
	require.NoError(t, err)
	var out struct {
		Context agento11y.HookContext `json:"context"`
		Input   struct {
			Output []struct {
				Parts []struct {
					ToolCall struct {
						InputJSON json.RawMessage `json:"input_json"`
					} `json:"tool_call"`
				} `json:"parts"`
			} `json:"output"`
			Tools []struct {
				InputSchemaJSON string `json:"input_schema_json"`
				Deferred        bool   `json:"deferred"`
			} `json:"tools"`
		} `json:"input"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	assert.Equal(t, "keep", out.Context.Tags["route"])
	assert.JSONEq(t, `{"command":"echo safe"}`, string(out.Input.Output[0].Parts[0].ToolCall.InputJSON))
	require.Len(t, out.Input.Tools, 1)
	assert.True(t, out.Input.Tools[0].Deferred)
	assertBase64JSONEq(t, schema, out.Input.Tools[0].InputSchemaJSON)
	assert.NotContains(t, string(raw), `"input_schema":`)
}

func assertBase64JSONEq(t *testing.T, want, encoded string) {
	t.Helper()
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	require.NoError(t, err)
	assert.JSONEq(t, want, string(decoded))
}
