package agento11y

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// The fixtures pin the generation span wire format. This test checks that OTel
// mode emits the matching span.
//
// OTel mode maps the SDK's generateText and streamText defaults to "chat". It
// also adds agento11y.record=true and the Go SDK identity. OTLP attribute order
// has no meaning, so this test compares attributes as a set.
//
// The test calls the OTel helpers directly because openai_sync contains a
// system-role message that normal client validation rejects.
func TestOTelModeMatchesGoldenWireFormat(t *testing.T) {
	for _, name := range []string{"openai_sync", "anthropic_stream", "gemini_sync"} {
		t.Run(name, func(t *testing.T) {
			generation := readGenerationFixture(t, name)
			want := readSpanFixture(t, name)

			client, recorder, _ := newOTelTestClient(t, nil)
			exportFixtureGeneration(client, generation)

			span := onlySpan(t, recorder)
			wantName := "chat " + generation.Model.Name
			if got := span.Name(); got != wantName {
				t.Errorf("span name = %q, want %q", got, wantName)
			}
			if !span.StartTime().Equal(generation.StartedAt) {
				t.Errorf("span start = %v, want %v", span.StartTime(), generation.StartedAt)
			}
			if !span.EndTime().Equal(generation.CompletedAt) {
				t.Errorf("span end = %v, want %v", span.EndTime(), generation.CompletedAt)
			}

			got := renderedAttributes(t, span)
			wantAttributes := want.attributes(t)
			wantAttributes["agento11y.record"] = "string:true"
			wantAttributes[sdkMetadataKeyName] = "string:" + sdkName
			for key, wantValue := range wantAttributes {
				gotValue, ok := got[key]
				if !ok {
					t.Errorf("missing attribute %s (want %s)", key, wantValue)
					continue
				}
				switch key {
				case "gen_ai.operation.name":
					// otel mode reports the spec operation, so the fixture's
					// proprietary name cannot match.
					wantValue = "string:chat"
				case "agento11y.generation.metadata":
					// The SDK stamps its own metadata keys on every export
					// path, so the fixture's entries are a subset.
					assertMetadataSuperset(t, gotValue, wantValue)
					continue
				}
				if gotValue != wantValue {
					t.Errorf("attribute %s =\n%s\nwant\n%s", key, gotValue, wantValue)
				}
			}
			wantAttributes["agento11y.generation.metadata"] = ""
			for key := range got {
				if _, ok := wantAttributes[key]; !ok {
					t.Errorf("unexpected attribute %s", key)
				}
			}
		})
	}
}

// assertMetadataSuperset checks that every entry of the fixture's metadata
// document is present in the emitted one.
func assertMetadataSuperset(t *testing.T, got, want string) {
	t.Helper()

	gotMap := decodeMetadataAttribute(t, got)
	for key, wantValue := range decodeMetadataAttribute(t, want) {
		gotValue, ok := gotMap[key]
		if !ok {
			t.Errorf("metadata is missing key %q", key)
			continue
		}
		if fmt.Sprint(gotValue) != fmt.Sprint(wantValue) {
			t.Errorf("metadata[%q] = %v, want %v", key, gotValue, wantValue)
		}
	}
}

func decodeMetadataAttribute(t *testing.T, rendered string) map[string]any {
	t.Helper()

	payload, ok := strings.CutPrefix(rendered, "string:")
	if !ok {
		t.Fatalf("metadata attribute %q is not a string", rendered)
	}
	out := map[string]any{}
	if err := json.Unmarshal([]byte(payload), &out); err != nil {
		t.Fatalf("unmarshal metadata %q: %v", payload, err)
	}
	return out
}

// exportFixtureGeneration emits one fixture generation as an otel-mode span,
// through the same client helpers GenerationRecorder.End uses.
func exportFixtureGeneration(client *Client, generation Generation) {
	ctx, _, invocation := client.startOTelGeneration(context.Background(), generation, generation.StartedAt)
	client.endOTelGeneration(ctx, invocation, generation,
		generationError{message: generation.CallError}, time.Time{}, ContentCaptureModeFull)
}

// renderedAttributes renders a span's attributes as key -> "type:value", the
// same rendering the fixture values go through, so a type mismatch shows up as
// a value mismatch instead of passing silently.
func renderedAttributes(t *testing.T, span sdktrace.ReadOnlySpan) map[string]string {
	t.Helper()

	out := make(map[string]string, len(span.Attributes()))
	for _, kv := range span.Attributes() {
		out[string(kv.Key)] = renderAttributeValue(t, kv.Value)
	}
	return out
}

func renderAttributeValue(t *testing.T, value attribute.Value) string {
	t.Helper()

	switch value.Type() {
	case attribute.STRING:
		return "string:" + value.AsString()
	case attribute.BOOL:
		return "bool:" + strconv.FormatBool(value.AsBool())
	case attribute.INT64:
		return "int64:" + strconv.FormatInt(value.AsInt64(), 10)
	case attribute.FLOAT64:
		return "float64:" + strconv.FormatFloat(value.AsFloat64(), 'g', -1, 64)
	case attribute.STRINGSLICE:
		return "stringslice:" + fmt.Sprintf("%q", value.AsStringSlice())
	default:
		t.Fatalf("unsupported attribute type %s", value.Type())
		return ""
	}
}

// fixtureSpan mirrors the protojson rendering of an OTLP span, just enough to
// compare against the emitted attributes without pulling the OTLP protos into
// this module.
type fixtureSpan struct {
	Name       string        `json:"name"`
	Kind       string        `json:"kind"`
	Attributes []fixtureAttr `json:"attributes"`
}

type fixtureAttr struct {
	Key   string       `json:"key"`
	Value fixtureValue `json:"value"`
}

type fixtureValue struct {
	StringValue *string  `json:"string_value"`
	BoolValue   *bool    `json:"bool_value"`
	IntValue    *string  `json:"int_value"` // protojson renders int64 as a string
	DoubleValue *float64 `json:"double_value"`
	ArrayValue  *struct {
		Values []fixtureValue `json:"values"`
	} `json:"array_value"`
}

func (s fixtureSpan) attributes(t *testing.T) map[string]string {
	t.Helper()

	out := make(map[string]string, len(s.Attributes))
	for _, attr := range s.Attributes {
		out[attr.Key] = attr.Value.render(t)
	}
	return out
}

func (v fixtureValue) render(t *testing.T) string {
	t.Helper()

	switch {
	case v.StringValue != nil:
		return "string:" + *v.StringValue
	case v.BoolValue != nil:
		return "bool:" + strconv.FormatBool(*v.BoolValue)
	case v.IntValue != nil:
		parsed, err := strconv.ParseInt(*v.IntValue, 10, 64)
		if err != nil {
			t.Fatalf("fixture int value %q: %v", *v.IntValue, err)
		}
		return "int64:" + strconv.FormatInt(parsed, 10)
	case v.DoubleValue != nil:
		return "float64:" + strconv.FormatFloat(*v.DoubleValue, 'g', -1, 64)
	case v.ArrayValue != nil:
		values := make([]string, 0, len(v.ArrayValue.Values))
		for _, item := range v.ArrayValue.Values {
			if item.StringValue == nil {
				t.Fatal("fixture array value with a non-string element")
			}
			values = append(values, *item.StringValue)
		}
		return "stringslice:" + fmt.Sprintf("%q", values)
	default:
		t.Fatal("fixture attribute value with no recognized type")
		return ""
	}
}

func readSpanFixture(t *testing.T, name string) fixtureSpan {
	t.Helper()

	payload := readFixture(t, name+".span.json")
	var span fixtureSpan
	if err := json.Unmarshal(payload, &span); err != nil {
		t.Fatalf("unmarshal span fixture %s: %v", name, err)
	}
	if span.Kind != "SPAN_KIND_CLIENT" {
		t.Fatalf("fixture span kind = %q, want SPAN_KIND_CLIENT", span.Kind)
	}
	return span
}

// fixtureGeneration mirrors the protojson rendering of an agento11y.v1
// Generation. It is read instead of protojson-decoding into the SDK proto
// because the proto's role enum has no system member, while the SDK model
// carries the role as a free string.
type fixtureGeneration struct {
	ID                  string            `json:"id"`
	ConversationID      string            `json:"conversation_id"`
	OperationName       string            `json:"operation_name"`
	Mode                string            `json:"mode"`
	Model               fixtureModel      `json:"model"`
	ResponseID          string            `json:"response_id"`
	ResponseModel       string            `json:"response_model"`
	SystemPrompt        string            `json:"system_prompt"`
	Input               []fixtureMessage  `json:"input"`
	Output              []fixtureMessage  `json:"output"`
	Usage               fixtureUsage      `json:"usage"`
	StopReason          string            `json:"stop_reason"`
	StartedAt           time.Time         `json:"started_at"`
	CompletedAt         time.Time         `json:"completed_at"`
	AgentName           string            `json:"agent_name"`
	AgentVersion        string            `json:"agent_version"`
	ParentGenerationIDs []string          `json:"parent_generation_ids"`
	Tags                map[string]string `json:"tags"`
	Metadata            map[string]any    `json:"metadata"`
	Tools               []fixtureToolDef  `json:"tools"`
	CallError           string            `json:"call_error"`
	MaxTokens           *fixtureInt       `json:"max_tokens"`
	Temperature         *float64          `json:"temperature"`
	TopP                *float64          `json:"top_p"`
	ToolChoice          *string           `json:"tool_choice"`
	ThinkingEnabled     *bool             `json:"thinking_enabled"`
	EffectiveVersion    string            `json:"effective_version"`
}

type fixtureModel struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
}

type fixtureMessage struct {
	Role  string        `json:"role"`
	Name  string        `json:"name"`
	Parts []fixturePart `json:"parts"`
}

type fixturePart struct {
	Text     string `json:"text"`
	Thinking string `json:"thinking"`
	ToolCall *struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		InputJSON string `json:"input_json"`
	} `json:"tool_call"`
	ToolResult *struct {
		ToolCallID  string `json:"tool_call_id"`
		Name        string `json:"name"`
		IsError     bool   `json:"is_error"`
		Content     string `json:"content"`
		ContentJSON string `json:"content_json"`
	} `json:"tool_result"`
	Metadata *struct {
		ProviderType string `json:"provider_type"`
	} `json:"metadata"`
}

type fixtureToolDef struct {
	Name            string `json:"name"`
	Description     string `json:"description"`
	Type            string `json:"type"`
	InputSchemaJSON string `json:"input_schema_json"`
	Deferred        bool   `json:"deferred"`
}

// fixtureInt is an int64 rendered by protojson as a JSON string.
type fixtureInt int64

func (v *fixtureInt) UnmarshalJSON(payload []byte) error {
	text := strings.Trim(string(payload), `"`)
	if text == "" || text == "null" {
		return nil
	}
	parsed, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return err
	}
	*v = fixtureInt(parsed)
	return nil
}

type fixtureUsage struct {
	InputTokens           fixtureInt `json:"input_tokens"`
	OutputTokens          fixtureInt `json:"output_tokens"`
	TotalTokens           fixtureInt `json:"total_tokens"`
	CacheReadInputTokens  fixtureInt `json:"cache_read_input_tokens"`
	CacheWriteInputTokens fixtureInt `json:"cache_write_input_tokens"`
	ReasoningTokens       fixtureInt `json:"reasoning_tokens"`
}

func readGenerationFixture(t *testing.T, name string) Generation {
	t.Helper()

	payload := readFixture(t, name+".generation.json")
	var fixture fixtureGeneration
	if err := json.Unmarshal(payload, &fixture); err != nil {
		t.Fatalf("unmarshal generation fixture %s: %v", name, err)
	}

	generation := Generation{
		ID:                  fixture.ID,
		ConversationID:      fixture.ConversationID,
		AgentName:           fixture.AgentName,
		AgentVersion:        fixture.AgentVersion,
		OperationName:       fixture.OperationName,
		Mode:                fixtureMode(fixture.Mode),
		Model:               ModelRef{Provider: fixture.Model.Provider, Name: fixture.Model.Name},
		ResponseID:          fixture.ResponseID,
		ResponseModel:       fixture.ResponseModel,
		SystemPrompt:        fixture.SystemPrompt,
		Input:               fixtureMessages(t, fixture.Input),
		Output:              fixtureMessages(t, fixture.Output),
		StopReason:          fixture.StopReason,
		StartedAt:           fixture.StartedAt,
		CompletedAt:         fixture.CompletedAt,
		ParentGenerationIDs: fixture.ParentGenerationIDs,
		Tags:                fixture.Tags,
		Metadata:            fixture.Metadata,
		EffectiveVersion:    fixture.EffectiveVersion,
		CallError:           fixture.CallError,
		Temperature:         fixture.Temperature,
		TopP:                fixture.TopP,
		ToolChoice:          fixture.ToolChoice,
		ThinkingEnabled:     fixture.ThinkingEnabled,
		Usage: TokenUsage{
			InputTokens:           int64(fixture.Usage.InputTokens),
			OutputTokens:          int64(fixture.Usage.OutputTokens),
			TotalTokens:           int64(fixture.Usage.TotalTokens),
			CacheReadInputTokens:  int64(fixture.Usage.CacheReadInputTokens),
			CacheWriteInputTokens: int64(fixture.Usage.CacheWriteInputTokens),
			ReasoningTokens:       int64(fixture.Usage.ReasoningTokens),
		},
	}
	if fixture.MaxTokens != nil {
		maxTokens := int64(*fixture.MaxTokens)
		generation.MaxTokens = &maxTokens
	}
	for _, tool := range fixture.Tools {
		generation.Tools = append(generation.Tools, ToolDefinition{
			Name:        tool.Name,
			Description: tool.Description,
			Type:        tool.Type,
			InputSchema: fixtureBytes(t, tool.InputSchemaJSON),
			Deferred:    tool.Deferred,
		})
	}
	return generation
}

func fixtureMode(mode string) GenerationMode {
	if mode == "GENERATION_MODE_STREAM" {
		return GenerationModeStream
	}
	return GenerationModeSync
}

func fixtureMessages(t *testing.T, messages []fixtureMessage) []Message {
	t.Helper()

	out := make([]Message, 0, len(messages))
	for _, message := range messages {
		converted := Message{
			Role: Role(strings.ToLower(strings.TrimPrefix(message.Role, "MESSAGE_ROLE_"))),
			Name: message.Name,
		}
		for _, part := range message.Parts {
			converted.Parts = append(converted.Parts, fixturePartToPart(t, part))
		}
		out = append(out, converted)
	}
	return out
}

func fixturePartToPart(t *testing.T, part fixturePart) Part {
	t.Helper()

	out := Part{}
	if part.Metadata != nil {
		out.Metadata = PartMetadata{ProviderType: part.Metadata.ProviderType}
	}
	switch {
	case part.ToolCall != nil:
		out.Kind = PartKindToolCall
		out.ToolCall = &ToolCall{
			ID:        part.ToolCall.ID,
			Name:      part.ToolCall.Name,
			InputJSON: fixtureBytes(t, part.ToolCall.InputJSON),
		}
	case part.ToolResult != nil:
		out.Kind = PartKindToolResult
		out.ToolResult = &ToolResult{
			ToolCallID:  part.ToolResult.ToolCallID,
			Name:        part.ToolResult.Name,
			IsError:     part.ToolResult.IsError,
			Content:     part.ToolResult.Content,
			ContentJSON: fixtureBytes(t, part.ToolResult.ContentJSON),
		}
	case part.Thinking != "":
		out.Kind = PartKindThinking
		out.Thinking = part.Thinking
	default:
		out.Kind = PartKindText
		out.Text = part.Text
	}
	return out
}

// fixtureBytes decodes a protojson bytes field, which is standard base64.
func fixtureBytes(t *testing.T, value string) []byte {
	t.Helper()

	if value == "" {
		return nil
	}
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		t.Fatalf("decode fixture bytes %q: %v", value, err)
	}
	return decoded
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()

	payload, err := os.ReadFile(filepath.Join("testdata", "otlpwire", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return payload
}
