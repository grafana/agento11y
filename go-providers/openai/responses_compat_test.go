package openai

import (
	"encoding/json"
	"testing"

	oresponses "github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
)

func TestResponsesFromStreamReadsFunctionNameFromRawArgumentsDoneEvent(t *testing.T) {
	var event oresponses.ResponseStreamEventUnion
	if err := json.Unmarshal([]byte(`{"type":"response.function_call_arguments.done","item_id":"fc_1","output_index":0,"name":"weather","arguments":"{\"city\":\"Paris\"}","sequence_number":3}`), &event); err != nil {
		t.Fatalf("decode event: %v", err)
	}

	generation, err := ResponsesFromStream(oresponses.ResponseNewParams{Model: shared.ResponsesModel("gpt-5")}, ResponsesStreamSummary{
		Events: []oresponses.ResponseStreamEventUnion{event, {Type: "response.completed"}},
	})
	if err != nil {
		t.Fatalf("responses from stream: %v", err)
	}
	if len(generation.Output) != 1 || generation.Output[0].Parts[0].ToolCall == nil {
		t.Fatalf("expected one tool call output, got %#v", generation.Output)
	}
	call := generation.Output[0].Parts[0].ToolCall
	if call.Name != "weather" || string(call.InputJSON) != `{"city":"Paris"}` {
		t.Fatalf("unexpected tool call %#v", call)
	}
}

func TestResponsesFromRequestResponseMapsOutputItemError(t *testing.T) {
	cases := map[string]string{
		"object": `{"id":"mcp_1","type":"mcp_call","name":"lookup","server_label":"docs","arguments":"{}","error":{"type":"http_error","code":500,"message":"upstream failed"}}`,
		"string": `{"id":"mcp_1","type":"mcp_call","name":"lookup","server_label":"docs","arguments":"{}","error":"upstream failed"}`,
	}
	for name, rawItem := range cases {
		t.Run(name, func(t *testing.T) {
			var resp oresponses.Response
			if err := json.Unmarshal([]byte(`{"id":"resp_1","model":"gpt-5","status":"completed","output":[`+rawItem+`]}`), &resp); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			generation, err := ResponsesFromRequestResponse(oresponses.ResponseNewParams{Model: shared.ResponsesModel("gpt-5")}, &resp)
			if err != nil {
				t.Fatalf("responses from response: %v", err)
			}
			if len(generation.Output) != 1 || generation.Output[0].Parts[0].Text != "upstream failed" {
				t.Fatalf("expected output item error text, got %#v", generation.Output)
			}
		})
	}
}
