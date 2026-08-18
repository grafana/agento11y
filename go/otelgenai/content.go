package otelgenai

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"

	"go.opentelemetry.io/otel"
)

// EnvCaptureMessageContent is the conventions' switch for message content on
// spans and events. Values are NO_CONTENT, SPAN_ONLY, EVENT_ONLY, and
// SPAN_AND_EVENT. The package treats anything else as NO_CONTENT. It takes
// effect only with the opt-in EnvSemconvStabilityOptIn describes.
const EnvCaptureMessageContent = "OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT"

// EnvSemconvStabilityOptIn opts a process into semantic conventions that are
// not stable yet. It holds a comma-separated list of tokens, and the token
// gen_ai_latest_experimental covers the experimental GenAI signals. Message
// content is one of them, so without that token EnvCaptureMessageContent is
// ignored and no content is emitted.
//
// One variable carries the tokens of every signal, so a process that opts into
// another one keeps it: "http/dup,gen_ai_latest_experimental" enables content.
// Each token is trimmed and then matched exactly, because the tokens of the
// other signals are case-sensitive and the same variable is read by the other
// language SDKs in the same deployment.
const EnvSemconvStabilityOptIn = "OTEL_SEMCONV_STABILITY_OPT_IN"

// semconvOptInGenAILatestExperimental is the EnvSemconvStabilityOptIn token
// that turns the experimental GenAI signals on.
const semconvOptInGenAILatestExperimental = "gen_ai_latest_experimental"

// EnvEmitEvent accepts true or false to override the capture mode's
// event-emission default. The package reports an invalid value to the OTel
// error handler and leaves the capture mode in charge. WithEmitEvent
// overrides this process-wide setting.
const EnvEmitEvent = "OTEL_INSTRUMENTATION_GENAI_EMIT_EVENT"

// CaptureMode selects where message content is emitted.
type CaptureMode string

const (
	// CaptureUnset selects the handler's mode. It is the zero value, and it is
	// what an Invocation carries when it does not override the handler.
	CaptureUnset CaptureMode = ""
	// CaptureNoContent emits no message content. It is the default.
	CaptureNoContent CaptureMode = "NO_CONTENT"
	// CaptureSpanOnly emits message content as span attributes.
	CaptureSpanOnly CaptureMode = "SPAN_ONLY"
	// CaptureEventOnly emits message content on log events only.
	CaptureEventOnly CaptureMode = "EVENT_ONLY"
	// CaptureSpanAndEvent emits message content on spans and log events.
	CaptureSpanAndEvent CaptureMode = "SPAN_AND_EVENT"
)

// SpanContent reports whether the mode puts message content on the span.
//
// An end hook reads the mode to decide whether an attribute the hook adds may
// carry content. The handler cannot make that call, because it cannot see
// inside a hook's attributes.
func (m CaptureMode) SpanContent() bool {
	switch m {
	case CaptureSpanOnly, CaptureSpanAndEvent:
		return true
	case CaptureNoContent, CaptureEventOnly, CaptureUnset:
		return false
	default:
		return false
	}
}

// EventContent reports whether the mode puts message content on the
// operation-details event.
func (m CaptureMode) EventContent() bool {
	switch m {
	case CaptureEventOnly, CaptureSpanAndEvent:
		return true
	case CaptureNoContent, CaptureSpanOnly, CaptureUnset:
		return false
	default:
		return false
	}
}

// ParseCaptureMode maps a capture-mode string to a CaptureMode, reporting
// false for an unknown value. Parsing is case-insensitive.
func ParseCaptureMode(value string) (CaptureMode, bool) {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "NO_CONTENT":
		return CaptureNoContent, true
	case "SPAN_ONLY":
		return CaptureSpanOnly, true
	case "EVENT_ONLY":
		return CaptureEventOnly, true
	case "SPAN_AND_EVENT":
		return CaptureSpanAndEvent, true
	default:
		return CaptureNoContent, false
	}
}

// CaptureModeFromEnv reads EnvCaptureMessageContent. An unset or unrecognized
// value is CaptureNoContent, which is the conventions' default.
//
// A mode other than NO_CONTENT also needs the gen_ai_latest_experimental
// token in EnvSemconvStabilityOptIn. Without it the mode is CaptureNoContent,
// and a mode the environment did ask for is reported to the OTel error
// handler, because a silent drop of content the operator enabled is hard to
// diagnose.
//
// The opt-in gates the environment only. WithCaptureMode and
// Invocation.Capture are the instrumentation's own decision and hold without
// it: code that sets a mode has taken responsibility for the experimental
// attributes, while the environment variable is set by an operator who may
// not know they are experimental.
func CaptureModeFromEnv() CaptureMode {
	value := os.Getenv(EnvCaptureMessageContent)
	if !genAILatestExperimental() {
		if mode, ok := ParseCaptureMode(value); ok && mode != CaptureNoContent {
			otel.Handle(fmt.Errorf("otelgenai: %s=%s needs %s=%s, emitting no content",
				EnvCaptureMessageContent, value,
				EnvSemconvStabilityOptIn, semconvOptInGenAILatestExperimental))
		}
		return CaptureNoContent
	}
	mode, _ := ParseCaptureMode(value)
	return mode
}

// genAILatestExperimental reports whether EnvSemconvStabilityOptIn opts into
// the experimental GenAI signals.
func genAILatestExperimental() bool {
	for token := range strings.SplitSeq(os.Getenv(EnvSemconvStabilityOptIn), ",") {
		if strings.TrimSpace(token) == semconvOptInGenAILatestExperimental {
			return true
		}
	}
	return false
}

func emitEventOverrideFromEnv() *bool {
	value := strings.TrimSpace(os.Getenv(EnvEmitEvent))
	switch strings.ToLower(value) {
	case "":
		return nil
	case "true":
		emit := true
		return &emit
	case "false":
		emit := false
		return &emit
	default:
		otel.Handle(fmt.Errorf("otelgenai: unrecognized %s value %q, using the capture mode", EnvEmitEvent, value))
		return nil
	}
}

// The JSON shapes below are the conventions' message schema. The field order
// is the encoded key order, and the golden fixtures pin that wire format, so
// do not reorder the fields.

// A pointer field lets the encoder omit a key on the part types that do not
// own it.

type wireMessage struct {
	Role string `json:"role"`
	Name string `json:"name,omitempty"`
	// Parts hold pre-encoded objects: each part may carry extension keys,
	// which marshalling a struct cannot express.
	Parts        []json.RawMessage `json:"parts"`
	FinishReason *string           `json:"finish_reason,omitempty"`
}

type wirePart struct {
	Type                   string          `json:"type"`
	Content                *string         `json:"content,omitempty"`
	ID                     string          `json:"id,omitempty"`
	Name                   *string         `json:"name,omitempty"`
	Arguments              json.RawMessage `json:"arguments,omitempty"`
	Response               json.RawMessage `json:"response,omitempty"`
	ServerToolCall         json.RawMessage `json:"server_tool_call,omitempty"`
	ServerToolCallResponse json.RawMessage `json:"server_tool_call_response,omitempty"`

	MimeType string  `json:"mime_type,omitempty"`
	Modality *string `json:"modality,omitempty"`
	URI      *string `json:"uri,omitempty"`
	FileID   *string `json:"file_id,omitempty"`
}

type wireToolDefinition struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// toolTypeFunction is the schema's default tool shape, and the only one the
// conventions name. A tool definition with no type of its own gets it,
// because the schema requires the key.
const toolTypeFunction = "function"

// wirePartKeys and wireToolDefinitionKeys are the JSON keys the two schema
// objects can emit. An extension key outside these sets can never collide
// with a schema key, so appendExtensions can skip re-reading the encoded
// object.
//
// jsonFieldNames reads the two sets off the struct tags rather than a written
// list, so a field added to either shape cannot leave the clash check stale.
var (
	wirePartKeys           = jsonFieldNames(reflect.TypeFor[wirePart]())
	wireToolDefinitionKeys = jsonFieldNames(reflect.TypeFor[wireToolDefinition]())
)

func jsonFieldNames(structType reflect.Type) map[string]struct{} {
	out := make(map[string]struct{}, structType.NumField())
	for i := range structType.NumField() {
		name, _, _ := strings.Cut(structType.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			out[name] = struct{}{}
		}
	}
	return out
}

// orEmpty returns a pointer to an empty string when value is nil, which emits
// a schema-required key the caller left unset.
func orEmpty(value *string) *string {
	if value != nil {
		return value
	}
	empty := ""
	return &empty
}

// encodeMessages renders messages as the JSON string carried by
// gen_ai.input.messages and gen_ai.output.messages. output selects the
// output-message schema, which requires a finish reason on every entry; input
// messages carry one only when the caller set it.
//
// encodeMessages leaves out a field it cannot represent and reports it: the
// returned payload is always usable, and the returned error names every field
// dropped from that payload.
func encodeMessages(messages []Message, output bool) (string, error) {
	var problems []error
	out := make([]json.RawMessage, 0, len(messages))
	for _, msg := range messages {
		parts := make([]json.RawMessage, 0, len(msg.Parts))
		for _, part := range msg.Parts {
			encoded, err := encodePart(part)
			if err != nil {
				problems = append(problems, err)
			}
			if encoded != nil {
				parts = append(parts, encoded)
			}
		}
		finishReason := msg.FinishReason
		if output {
			finishReason = orEmpty(finishReason)
		}
		wm := wireMessage{
			Role:         string(msg.Role),
			Name:         msg.Name,
			Parts:        parts,
			FinishReason: finishReason,
		}
		payload, err := json.Marshal(wm)
		if err != nil {
			problems = append(problems, fmt.Errorf("otelgenai: encode message: %w", err))
			continue
		}
		out = append(out, payload)
	}
	payload, err := marshalArray(out)
	problems = append(problems, err)
	return payload, errors.Join(problems...)
}

// encodeSystemInstructions renders parts as the JSON string carried by
// gen_ai.system_instructions, under the same partial-failure rule as
// encodeMessages.
func encodeSystemInstructions(parts []Part) (string, error) {
	var problems []error
	encoded := make([]json.RawMessage, 0, len(parts))
	for _, part := range parts {
		var payload json.RawMessage
		var err error
		if part.Type == PartTypeText {
			payload, err = encodePart(part)
		} else {
			payload, err = encodeGenericPart(part)
		}
		if err != nil {
			problems = append(problems, err)
		}
		if payload != nil {
			encoded = append(encoded, payload)
		}
	}
	payload, err := marshalArray(encoded)
	problems = append(problems, err)
	return payload, errors.Join(problems...)
}

// encodeToolDefinitions renders tools as the JSON string carried by
// gen_ai.tool.definitions, under the same partial-failure rule as
// encodeMessages.
func encodeToolDefinitions(tools []ToolDefinition) (string, error) {
	var problems []error
	encoded := make([]json.RawMessage, 0, len(tools))
	for _, tool := range tools {
		parameters, err := rawJSONField(tool.Parameters, "parameters")
		if err != nil {
			problems = append(problems, err)
		}
		toolType := tool.Type
		if toolType == "" {
			toolType = toolTypeFunction
		}
		wt := wireToolDefinition{
			Type:        toolType,
			Name:        tool.Name,
			Description: tool.Description,
			Parameters:  parameters,
		}
		payload, err := json.Marshal(wt)
		if err != nil {
			problems = append(problems, fmt.Errorf("otelgenai: encode tool definitions: %w", err))
			continue
		}
		payload, err = appendExtensions(payload, tool.Extensions, wireToolDefinitionKeys)
		if err != nil {
			problems = append(problems, err)
		}
		encoded = append(encoded, payload)
	}
	payload, err := marshalArray(encoded)
	problems = append(problems, err)
	return payload, errors.Join(problems...)
}

// encodePart renders one part object. It encodes only the fields the part's
// type owns, so a Part that carries leftovers from another shape cannot
// smuggle them onto the wire. It emits a required string key as empty when
// the caller leaves it unset, except modality, whose enum has no empty value:
// an unset modality is omitted and reported.
//
// A type the schema does not name is encoded as its generic part: the type
// and the extensions, nothing else. A part with no type is dropped, and
// reported with a nil payload.
func encodePart(part Part) (json.RawMessage, error) {
	if part.Type == "" {
		return nil, errors.New("otelgenai: drop message part with no type")
	}

	var problems []error
	wp := wirePart{Type: string(part.Type)}
	needsModality := false

	switch part.Type {
	case PartTypeText, PartTypeReasoning:
		wp.Content = orEmpty(part.Content)
	case PartTypeToolCall:
		wp.Name = orEmpty(&part.Name)
		wp.ID = part.ID
		arguments, err := rawJSONField(part.Arguments, "arguments")
		if err != nil {
			problems = append(problems, err)
		}
		if len(arguments) == 0 {
			arguments = json.RawMessage("null")
		}
		wp.Arguments = arguments
	case PartTypeServerToolCall:
		wp.Name = orEmpty(&part.Name)
		wp.ID = part.ID
		serverToolCall, err := rawJSONField(part.Arguments, "server_tool_call")
		if err != nil {
			problems = append(problems, err)
		}
		if len(serverToolCall) == 0 {
			serverToolCall = json.RawMessage("null")
		}
		wp.ServerToolCall = serverToolCall
	case PartTypeToolCallResponse:
		wp.ID = part.ID
		response, err := rawJSONField(part.Response, "response")
		if err != nil {
			problems = append(problems, err)
		}
		if len(response) == 0 {
			response = json.RawMessage("null")
		}
		wp.Response = response
	case PartTypeServerToolCallResponse:
		wp.ID = part.ID
		response, err := rawJSONField(part.Response, "server_tool_call_response")
		if err != nil {
			problems = append(problems, err)
		}
		if len(response) == 0 {
			response = json.RawMessage("null")
		}
		wp.ServerToolCallResponse = response
	case PartTypeCompaction:
		wp.ID = part.ID
		wp.Content = part.Content
	case PartTypeBlob:
		wp.Content = orEmpty(part.Content)
		wp.MimeType = part.MimeType
		needsModality = true
	case PartTypeFile:
		wp.FileID = orEmpty(part.FileID)
		wp.MimeType = part.MimeType
		needsModality = true
	case PartTypeURI:
		wp.URI = orEmpty(&part.URI)
		wp.MimeType = part.MimeType
		needsModality = true
	default:
		return encodeGenericPart(part)
	}

	if needsModality {
		if part.Modality != nil && *part.Modality != "" {
			wp.Modality = part.Modality
		} else {
			problems = append(problems, fmt.Errorf(
				"otelgenai: %s part has no modality; omitting the schema-required key",
				part.Type))
		}
	}

	payload, err := json.Marshal(wp)
	if err != nil {
		problems = append(problems, fmt.Errorf("otelgenai: encode message part: %w", err))
		return nil, errors.Join(problems...)
	}
	payload, err = appendExtensions(payload, part.Extensions, wirePartKeys)
	problems = append(problems, err)
	return payload, errors.Join(problems...)
}

func encodeGenericPart(part Part) (json.RawMessage, error) {
	if part.Type == "" {
		return nil, errors.New("otelgenai: drop message part with no type")
	}

	var problems []error
	if part.Content != nil || part.ID != "" || part.Name != "" ||
		len(part.Arguments) > 0 || len(part.Response) > 0 ||
		part.MimeType != "" || part.Modality != nil ||
		part.URI != "" || part.FileID != nil {
		problems = append(problems, fmt.Errorf(
			"otelgenai: message part of type %q keeps only its type and its extensions: the schema's generic part has no other field",
			part.Type))
	}

	payload, err := json.Marshal(wirePart{Type: string(part.Type)})
	if err != nil {
		problems = append(problems, fmt.Errorf("otelgenai: encode message part: %w", err))
		return nil, errors.Join(problems...)
	}
	payload, err = appendExtensions(payload, part.Extensions, wirePartKeys)
	problems = append(problems, err)
	return payload, errors.Join(problems...)
}

// rawJSONField returns a raw JSON document for a schema field that carries one.
// rawJSONField drops invalid JSON rather than emitting it, because the
// enclosing marshal would fail on it and lose the whole attribute.
func rawJSONField(raw json.RawMessage, field string) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("otelgenai: drop %s: not valid JSON", field)
	}
	return raw, nil
}

// appendExtensions splices extension keys into an encoded JSON object, sorted
// by key so the output is deterministic. Extension keys are appended after the
// schema's own keys.
//
// appendExtensions drops and reports two kinds of extension entry. A key the
// schema already used would produce a duplicate key, and decoders resolve a
// duplicate differently. A value that does not marshal would leave no object
// at all.
//
// schemaKeys are the keys the enclosing object can hold. appendExtensions
// reads the payload back only when an extension key is one of them, because
// the clash test needs the keys the object actually holds, and an extension
// is free to reuse a key the encoder omitted.
func appendExtensions(payload json.RawMessage, extensions map[string]any, schemaKeys map[string]struct{}) (json.RawMessage, error) {
	if len(extensions) == 0 {
		return payload, nil
	}

	taken := map[string]struct{}{}
	for key := range extensions {
		if _, candidate := schemaKeys[key]; candidate {
			var err error
			if taken, err = objectKeys(payload); err != nil {
				return payload, err
			}
			break
		}
	}

	// Encoding the keys one at a time and joining them here gives byte for
	// byte what marshalling a map of them produces: json.Marshal sorts map
	// keys and escapes both halves the same way the enclosing marshal does.
	var problems []error
	var encoded []byte
	for _, key := range slices.Sorted(maps.Keys(extensions)) {
		if _, clash := taken[key]; clash {
			problems = append(problems, fmt.Errorf("otelgenai: drop extension key %q: the message schema already uses it", key))
			continue
		}
		value, err := json.Marshal(extensions[key])
		if err != nil {
			problems = append(problems, fmt.Errorf("otelgenai: drop extension key %q: %w", key, err))
			continue
		}
		name, err := json.Marshal(key)
		if err != nil {
			problems = append(problems, fmt.Errorf("otelgenai: drop extension key %q: %w", key, err))
			continue
		}
		if len(encoded) > 0 {
			encoded = append(encoded, ',')
		}
		encoded = append(encoded, name...)
		encoded = append(encoded, ':')
		encoded = append(encoded, value...)
	}
	if len(encoded) == 0 {
		return payload, errors.Join(problems...)
	}

	object := bytes.TrimSpace(payload)
	if len(object) < 2 || object[0] != '{' || object[len(object)-1] != '}' {
		problems = append(problems, errors.New("otelgenai: cannot add extension keys to non-object JSON"))
		return payload, errors.Join(problems...)
	}
	out := make([]byte, 0, len(object)+len(encoded)+1)
	out = append(out, object[:len(object)-1]...)
	if len(object) > 2 {
		out = append(out, ',')
	}
	out = append(out, encoded...)
	out = append(out, '}')
	return out, errors.Join(problems...)
}

// objectKeys returns the top-level keys of an encoded JSON object.
func objectKeys(payload json.RawMessage) (map[string]struct{}, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(payload, &object); err != nil {
		return nil, fmt.Errorf("otelgenai: read encoded object keys: %w", err)
	}
	out := make(map[string]struct{}, len(object))
	for key := range object {
		out[key] = struct{}{}
	}
	return out, nil
}

func marshalArray(items []json.RawMessage) (string, error) {
	payload, err := json.Marshal(items)
	if err != nil {
		return "[]", fmt.Errorf("otelgenai: encode array: %w", err)
	}
	return string(payload), nil
}
