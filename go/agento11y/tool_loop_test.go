package agento11y

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/codes"
)

func TestExecuteToolCallsHappyPath(t *testing.T) {
	client, recorder, _ := newTestClient(t, Config{})
	ctx := context.Background()

	msgs := []Message{
		{
			Role: RoleAssistant,
			Parts: []Part{
				ToolCallPart(ToolCall{ID: "c1", Name: "weather", InputJSON: json.RawMessage(`{"city":"Paris"}`)}),
				ToolCallPart(ToolCall{ID: "c2", Name: "math", InputJSON: json.RawMessage(`{"a":1,"b":2}`)}),
			},
		},
	}

	_, out := client.ExecuteToolCalls(ctx, msgs, func(_ context.Context, name string, args json.RawMessage) (any, error) {
		if name == "weather" {
			return map[string]any{"temp_c": 18}, nil
		}
		var m map[string]any
		_ = json.Unmarshal(args, &m)
		return m, nil
	}, ExecuteToolCallsOptions{
		ConversationID:  "conv-loop",
		AgentName:       "agent-x",
		AgentVersion:    "1.0.0",
		RequestModel:    "gpt-test",
		RequestProvider: "openai",
	})

	if len(out) != 2 {
		t.Fatalf("want 2 tool messages, got %d", len(out))
	}
	if out[0].Role != RoleTool || out[0].Name != "weather" {
		t.Fatalf("first message: %#v", out[0])
	}
	tr0 := out[0].Parts[0].ToolResult
	if tr0.ToolCallID != "c1" || tr0.Name != "weather" {
		t.Fatalf("tool result: %#v", tr0)
	}
	if string(tr0.ContentJSON) != `{"temp_c":18}` {
		t.Fatalf("content json: %s", tr0.ContentJSON)
	}

	countToolSpans := func(substr string) int {
		n := 0
		for _, s := range recorder.Ended() {
			if strings.Contains(s.Name(), substr) {
				n++
			}
		}
		return n
	}
	if countToolSpans("execute_tool weather") != 1 || countToolSpans("execute_tool math") != 1 {
		t.Fatalf("unexpected spans")
	}
}

func TestExecuteToolCallsExecutorError(t *testing.T) {
	client, recorder, _ := newTestClient(t, Config{})
	msgs := []Message{
		{
			Role: RoleAssistant,
			Parts: []Part{
				ToolCallPart(ToolCall{ID: "c1", Name: "boom", InputJSON: json.RawMessage(`{}`)}),
			},
		},
	}
	_, out := client.ExecuteToolCalls(context.Background(), msgs, func(context.Context, string, json.RawMessage) (any, error) {
		return nil, errors.New("tool failed")
	}, ExecuteToolCallsOptions{})
	if len(out) != 1 {
		t.Fatalf("want 1 message")
	}
	tr := out[0].Parts[0].ToolResult
	if !tr.IsError || !strings.Contains(tr.Content, "tool failed") {
		t.Fatalf("tool result: %#v", tr)
	}
	var boom *codes.Code
	for _, s := range recorder.Ended() {
		if strings.HasPrefix(s.Name(), "execute_tool boom") {
			c := s.Status().Code
			boom = &c
			break
		}
	}
	if boom == nil || *boom != codes.Error {
		t.Fatalf("expected error span")
	}
}

func TestExecuteToolCallsNoTools(t *testing.T) {
	client, recorder, _ := newTestClient(t, Config{})
	before := len(recorder.Ended())
	_, out := client.ExecuteToolCalls(context.Background(), []Message{
		{Role: RoleAssistant, Parts: []Part{TextPart("hi")}},
	}, func(context.Context, string, json.RawMessage) (any, error) { return nil, nil }, ExecuteToolCallsOptions{})
	if len(out) != 0 {
		t.Fatalf("want empty out")
	}
	for _, s := range recorder.Ended()[before:] {
		if strings.HasPrefix(s.Name(), "execute_tool ") {
			t.Fatalf("unexpected tool span %q", s.Name())
		}
	}
}

func TestExecuteToolCallsSingleTool(t *testing.T) {
	client, _, _ := newTestClient(t, Config{})
	_, out := client.ExecuteToolCalls(context.Background(), []Message{
		{Role: RoleAssistant, Parts: []Part{ToolCallPart(ToolCall{ID: "id1", Name: "echo", InputJSON: json.RawMessage(`{"x":1}`)})}},
	}, func(_ context.Context, _ string, args json.RawMessage) (any, error) {
		var m map[string]any
		_ = json.Unmarshal(args, &m)
		return m, nil
	}, ExecuteToolCallsOptions{})
	if len(out) != 1 || out[0].Parts[0].ToolResult.ToolCallID != "id1" {
		t.Fatalf("out: %#v", out)
	}
}

func TestExecuteToolCallsEmptyMessages(t *testing.T) {
	client, recorder, _ := newTestClient(t, Config{})
	before := len(recorder.Ended())
	_, out := client.ExecuteToolCalls(context.Background(), nil, func(context.Context, string, json.RawMessage) (any, error) {
		return nil, nil
	}, ExecuteToolCallsOptions{})
	if len(out) != 0 {
		t.Fatalf("want empty")
	}
	for _, s := range recorder.Ended()[before:] {
		if strings.HasPrefix(s.Name(), "execute_tool ") {
			t.Fatalf("unexpected span")
		}
	}
}

func TestExecuteToolCallsSkipsBlankToolName(t *testing.T) {
	client, recorder, _ := newTestClient(t, Config{})
	before := len(recorder.Ended())
	_, out := client.ExecuteToolCalls(context.Background(), []Message{
		{Role: RoleAssistant, Parts: []Part{ToolCallPart(ToolCall{ID: "x", Name: "   ", InputJSON: json.RawMessage(`{}`)})}},
	}, func(context.Context, string, json.RawMessage) (any, error) { return 1, nil }, ExecuteToolCallsOptions{})
	if len(out) != 0 {
		t.Fatalf("want empty")
	}
	for _, s := range recorder.Ended()[before:] {
		if strings.HasPrefix(s.Name(), "execute_tool ") {
			t.Fatalf("unexpected span")
		}
	}
}

func TestExecuteToolCallsNilClient(t *testing.T) {
	var c *Client
	ctx, out := c.ExecuteToolCalls(context.Background(), nil, func(context.Context, string, json.RawMessage) (any, error) {
		return nil, nil
	}, ExecuteToolCallsOptions{})
	if ctx == nil || out != nil {
		t.Fatalf("ctx=%v out=%v", ctx, out)
	}
}

func TestExecuteToolCallsNilExecutor(t *testing.T) {
	client, _, _ := newTestClient(t, Config{})
	ctx, out := client.ExecuteToolCalls(context.Background(), []Message{
		{Role: RoleAssistant, Parts: []Part{ToolCallPart(ToolCall{Name: "x", InputJSON: json.RawMessage(`{}`)})}},
	}, nil, ExecuteToolCallsOptions{})
	if len(out) != 0 {
		t.Fatalf("want empty")
	}
	if ctx == nil {
		t.Fatal("ctx nil")
	}
}
