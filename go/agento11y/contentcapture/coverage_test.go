package contentcapture_test

// This file is the schema-drift guard for the strip. Strip is one algorithm
// over the Target interfaces, so adding a method to those interfaces breaks
// every adapter until it is handled; what the compiler cannot see is a new
// proto field or a new Part.payload variant that nobody classified as content
// or as retained structure. TestProtoFieldCoverage walks the descriptors and
// fails on it. TestMetadataKeyCoverage does the same for the metadata keys,
// which the descriptors cannot reach: metadata is one proto field holding a
// mix of content mirrors and retained keys.

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/grafana/agento11y/go/agento11y/contentcapture"
	"github.com/grafana/agento11y/go/agento11y/model"
	agento11yv1 "github.com/grafana/agento11y/go/proto/agento11y/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// contentClass is how one proto field survives, or does not survive, a
// metadata_only strip.
type contentClass string

const (
	// classContent is cleared by the strip: the fixture must populate it and
	// the stripped proto must not carry it.
	classContent contentClass = "content cleared by the strip"
	// classRetained survives: structure, identifiers, usage, timing, tags,
	// and metadata a reader needs to make sense of the stripped generation.
	classRetained contentClass = "structure retained by the strip"
	// classReplaced is call_error: the raw text is replaced by the classified
	// error category rather than cleared.
	classReplaced contentClass = "replaced with the error category"
)

// There is deliberately no "exempt" class. A field the fixture cannot populate
// still has to be classified, and an exemption would let a new Part.payload
// variant pass the coverage test having asserted nothing about the strip.

// generationFieldClasses classifies every proto field reachable from
// Generation, including each Part.payload oneof variant. A field missing here
// fails TestProtoFieldCoverage with its proto path, so a new content field
// cannot be added without someone deciding whether the strip has to clear it.
var generationFieldClasses = map[string]contentClass{
	"agento11y.v1.Generation.id":                    classRetained,
	"agento11y.v1.Generation.conversation_id":       classRetained,
	"agento11y.v1.Generation.operation_name":        classRetained,
	"agento11y.v1.Generation.mode":                  classRetained,
	"agento11y.v1.Generation.trace_id":              classRetained,
	"agento11y.v1.Generation.span_id":               classRetained,
	"agento11y.v1.Generation.model":                 classRetained,
	"agento11y.v1.Generation.response_id":           classRetained,
	"agento11y.v1.Generation.response_model":        classRetained,
	"agento11y.v1.Generation.system_prompt":         classContent,
	"agento11y.v1.Generation.input":                 classRetained,
	"agento11y.v1.Generation.output":                classRetained,
	"agento11y.v1.Generation.tools":                 classRetained,
	"agento11y.v1.Generation.usage":                 classRetained,
	"agento11y.v1.Generation.stop_reason":           classRetained,
	"agento11y.v1.Generation.started_at":            classRetained,
	"agento11y.v1.Generation.completed_at":          classRetained,
	"agento11y.v1.Generation.tags":                  classRetained,
	"agento11y.v1.Generation.metadata":              classRetained,
	"agento11y.v1.Generation.raw_artifacts":         classContent,
	"agento11y.v1.Generation.call_error":            classReplaced,
	"agento11y.v1.Generation.agent_name":            classRetained,
	"agento11y.v1.Generation.agent_version":         classRetained,
	"agento11y.v1.Generation.max_tokens":            classRetained,
	"agento11y.v1.Generation.temperature":           classRetained,
	"agento11y.v1.Generation.top_p":                 classRetained,
	"agento11y.v1.Generation.tool_choice":           classRetained,
	"agento11y.v1.Generation.thinking_enabled":      classRetained,
	"agento11y.v1.Generation.parent_generation_ids": classRetained,
	"agento11y.v1.Generation.effective_version":     classRetained,

	"agento11y.v1.ModelRef.provider": classRetained,
	"agento11y.v1.ModelRef.name":     classRetained,

	"agento11y.v1.Message.role":  classRetained,
	"agento11y.v1.Message.name":  classRetained,
	"agento11y.v1.Message.parts": classRetained,

	// Part.payload variants. A sixth variant belongs here or the coverage test
	// fails. That is the only guard against a new payload kind that no
	// PartTarget method clears.
	"agento11y.v1.Part.metadata":    classRetained,
	"agento11y.v1.Part.text":        classContent,
	"agento11y.v1.Part.thinking":    classContent,
	"agento11y.v1.Part.tool_call":   classRetained,
	"agento11y.v1.Part.tool_result": classRetained,
	"agento11y.v1.Part.media":       classRetained,

	"agento11y.v1.PartMetadata.provider_type": classRetained,

	"agento11y.v1.ToolCall.id":         classRetained,
	"agento11y.v1.ToolCall.name":       classRetained,
	"agento11y.v1.ToolCall.input_json": classContent,

	"agento11y.v1.ToolResult.tool_call_id": classRetained,
	"agento11y.v1.ToolResult.name":         classRetained,
	"agento11y.v1.ToolResult.content":      classContent,
	"agento11y.v1.ToolResult.content_json": classContent,
	"agento11y.v1.ToolResult.is_error":     classRetained,

	// Media.url can hold a data: URI with the file bytes inline, so it counts
	// as content. The fields around it are references and are retained.
	"agento11y.v1.Media.kind":      classRetained,
	"agento11y.v1.Media.url":       classContent,
	"agento11y.v1.Media.mime_type": classRetained,
	"agento11y.v1.Media.name":      classRetained,

	"agento11y.v1.ToolDefinition.name":              classRetained,
	"agento11y.v1.ToolDefinition.description":       classContent,
	"agento11y.v1.ToolDefinition.type":              classRetained,
	"agento11y.v1.ToolDefinition.input_schema_json": classContent,
	"agento11y.v1.ToolDefinition.deferred":          classRetained,

	"agento11y.v1.TokenUsage.input_tokens":             classRetained,
	"agento11y.v1.TokenUsage.output_tokens":            classRetained,
	"agento11y.v1.TokenUsage.total_tokens":             classRetained,
	"agento11y.v1.TokenUsage.cache_read_input_tokens":  classRetained,
	"agento11y.v1.TokenUsage.cache_write_input_tokens": classRetained,
	"agento11y.v1.TokenUsage.reasoning_tokens":         classRetained,

	// The whole raw_artifacts list is dropped, so no Artifact field survives.
	"agento11y.v1.Artifact.kind":         classContent,
	"agento11y.v1.Artifact.name":         classContent,
	"agento11y.v1.Artifact.content_type": classContent,
	"agento11y.v1.Artifact.payload":      classContent,
	"agento11y.v1.Artifact.record_id":    classContent,
	"agento11y.v1.Artifact.uri":          classContent,
}

func TestProtoFieldCoverage(t *testing.T) {
	const category = "rate_limit"

	universe := protoFieldUniverse((&agento11yv1.Generation{}).ProtoReflect().Descriptor())
	for name := range universe {
		if _, classified := generationFieldClasses[name]; !classified {
			t.Errorf("proto field %s is not classified in generationFieldClasses: decide whether it carries content, and if it does, give it a clear method on contentcapture.Target or contentcapture.PartTarget so Strip reaches it in every shape", name)
		}
	}
	for name := range generationFieldClasses {
		if !universe[name] {
			t.Errorf("generationFieldClasses classifies %s, which is no longer reachable from agento11y.v1.Generation", name)
		}
	}

	before := fullContentGeneration()
	after, ok := proto.Clone(before).(*agento11yv1.Generation)
	if !ok {
		t.Fatalf("clone fixture: unexpected type")
	}
	contentcapture.StripGeneration(after, category)

	populatedBefore := nonEmptyByField(walkProtoFields(before.ProtoReflect(), ""))
	afterOccurrences := walkProtoFields(after.ProtoReflect(), "")
	populatedAfter := nonEmptyByField(afterOccurrences)

	for _, name := range slices.Sorted(maps.Keys(generationFieldClasses)) {
		switch class := generationFieldClasses[name]; class {
		case classContent:
			if !populatedBefore[name] {
				t.Errorf("%s is classified as content but the fixture leaves it empty, so the strip is untested", name)
			}
			if populatedAfter[name] {
				t.Errorf("%s is classified as content but survives the strip: %s", name, occurrencesOf(afterOccurrences, name))
			}
		case classRetained:
			if !populatedBefore[name] {
				t.Errorf("%s is classified as retained but the fixture leaves it empty, so retention is unverified", name)
			}
			if !populatedAfter[name] {
				t.Errorf("%s is classified as retained but the strip cleared it", name)
			}
		case classReplaced:
			if name != "agento11y.v1.Generation.call_error" {
				t.Errorf("%s is classified as replaced, but only call_error has replacement semantics: extend this case before reusing the class", name)
				continue
			}
			if got := after.GetCallError(); got != category {
				t.Errorf("%s = %q, want the classified category %q", name, got, category)
			}
		default:
			t.Errorf("%s has unknown class %q", name, class)
		}
	}
}

// generationMetadataKeyClasses classifies the metadata keys a generation
// carries into the strip. generationFieldClasses classifies
// agento11y.v1.Generation.metadata as one retained field, which cannot express
// that some keys inside it are content: the call-error mirror and the
// conversation title under both spellings are deleted, while user keys and the
// content-capture stamp survive.
var generationMetadataKeyClasses = map[string]contentClass{
	contentcapture.MetadataKeyCallError:       classContent,
	contentcapture.ConversationTitleKey:       classContent,
	contentcapture.LegacyConversationTitleKey: classContent,
	model.MetadataKeyContentCaptureMode:       classRetained,
	"agento11y.sdk.content_capture":           classRetained,
	"user.key":                                classRetained,
}

func TestMetadataKeyCoverage(t *testing.T) {
	before := fullContentGeneration()
	after, ok := proto.Clone(before).(*agento11yv1.Generation)
	if !ok {
		t.Fatalf("clone fixture: unexpected type")
	}
	contentcapture.StripGeneration(after, "rate_limit")

	for _, key := range slices.Sorted(maps.Keys(before.GetMetadata().GetFields())) {
		if _, classified := generationMetadataKeyClasses[key]; !classified {
			t.Errorf("metadata key %q is not classified in generationMetadataKeyClasses: decide whether it mirrors content, and if it does, delete it in contentcapture.Strip", key)
		}
	}

	for _, key := range slices.Sorted(maps.Keys(generationMetadataKeyClasses)) {
		_, inFixture := before.GetMetadata().GetFields()[key]
		value, survived := after.GetMetadata().GetFields()[key]

		switch class := generationMetadataKeyClasses[key]; class {
		case classContent:
			if !inFixture {
				t.Errorf("metadata key %q is classified as content but the fixture does not set it, so the strip is untested", key)
			}
			if survived {
				t.Errorf("metadata key %q is classified as content but survives the strip: %v", key, value.AsInterface())
			}
		case classRetained:
			if !inFixture {
				t.Errorf("metadata key %q is classified as retained but the fixture does not set it, so retention is unverified", key)
			}
			if !survived {
				t.Errorf("metadata key %q is classified as retained but the strip deleted it", key)
			}
		default:
			t.Errorf("metadata key %q has class %q, which has no meaning for a metadata key: a key is either deleted or kept", key, class)
		}
	}
}

// protoFieldOccurrence is one populated field found in a message tree.
type protoFieldOccurrence struct {
	// path locates the value in the instance, e.g. input[0].parts[1].text.
	path string
	// field is the descriptor's full name, e.g. agento11y.v1.Part.text.
	field string
	// nonEmpty is false for a field that is set but holds its zero value,
	// which is how a cleared oneof member (Part.text = "") looks.
	nonEmpty bool
	rendered string
}

// protoFieldUniverse returns every field reachable from md, keyed by
// descriptor full name. Oneof members are ordinary fields, so a new
// Part.payload variant shows up here. Well-known types are leaves: their
// internals are not agento11y's schema. Map entries are leaves too; the
// synthetic key/value pair carries no classification decision.
func protoFieldUniverse(md protoreflect.MessageDescriptor) map[string]bool {
	out := map[string]bool{}
	seen := map[protoreflect.FullName]bool{}

	var visit func(protoreflect.MessageDescriptor)
	visit = func(md protoreflect.MessageDescriptor) {
		if seen[md.FullName()] {
			return
		}
		seen[md.FullName()] = true

		fields := md.Fields()
		for i := range fields.Len() {
			fd := fields.Get(i)
			out[string(fd.FullName())] = true
			if fd.IsMap() {
				continue
			}
			if msg := fd.Message(); msg != nil && !isWellKnownProto(msg) {
				visit(msg)
			}
		}
	}
	visit(md)

	return out
}

func isWellKnownProto(md protoreflect.MessageDescriptor) bool {
	return md.FullName().Parent() == "google.protobuf"
}

// walkProtoFields records every populated field in a message tree. Unset
// fields are skipped: in proto3 a zero scalar is indistinguishable from an
// absent one, so "populated" is the only usable signal, and a set-but-zero
// oneof member is recorded with nonEmpty false.
func walkProtoFields(m protoreflect.Message, prefix string) []protoFieldOccurrence {
	var out []protoFieldOccurrence
	if m == nil || !m.IsValid() {
		return out
	}

	fields := m.Descriptor().Fields()
	for i := range fields.Len() {
		fd := fields.Get(i)
		if !m.Has(fd) {
			continue
		}
		value := m.Get(fd)
		path := prefix + string(fd.Name())

		switch {
		case fd.IsMap():
			out = append(out, protoFieldOccurrence{
				path:     path,
				field:    string(fd.FullName()),
				nonEmpty: value.Map().Len() > 0,
				rendered: renderProtoMap(value.Map()),
			})
		case fd.IsList():
			list := value.List()
			out = append(out, protoFieldOccurrence{
				path:     path,
				field:    string(fd.FullName()),
				nonEmpty: list.Len() > 0,
				rendered: fmt.Sprintf("len=%d", list.Len()),
			})
			for j := range list.Len() {
				out = append(out, walkProtoValue(fd, list.Get(j), fmt.Sprintf("%s[%d]", path, j))...)
			}
		default:
			out = append(out, walkProtoValue(fd, value, path)...)
		}
	}

	return out
}

func walkProtoValue(fd protoreflect.FieldDescriptor, value protoreflect.Value, path string) []protoFieldOccurrence {
	name := string(fd.FullName())
	if fd.Message() == nil {
		return []protoFieldOccurrence{{
			path:     path,
			field:    name,
			nonEmpty: !isZeroScalar(fd, value),
			rendered: renderProtoScalar(fd, value),
		}}
	}
	if isWellKnownProto(fd.Message()) {
		return walkWellKnownProto(fd, value, path)
	}
	return append(
		[]protoFieldOccurrence{{path: path, field: name, nonEmpty: true, rendered: "set"}},
		walkProtoFields(value.Message(), path+".")...,
	)
}

// walkWellKnownProto renders a well-known type without descending into its
// generated schema. Struct is expanded one level so a metadata key shows up as
// its own occurrence rather than as part of the whole map.
func walkWellKnownProto(fd protoreflect.FieldDescriptor, value protoreflect.Value, path string) []protoFieldOccurrence {
	name := string(fd.FullName())
	switch message := value.Message().Interface().(type) {
	case *timestamppb.Timestamp:
		return []protoFieldOccurrence{{
			path:     path,
			field:    name,
			nonEmpty: !message.AsTime().IsZero(),
			rendered: message.AsTime().UTC().Format(time.RFC3339Nano),
		}}
	case *structpb.Struct:
		out := []protoFieldOccurrence{{
			path:     path,
			field:    name,
			nonEmpty: len(message.GetFields()) > 0,
			rendered: fmt.Sprintf("fields=%d", len(message.GetFields())),
		}}
		for _, key := range slices.Sorted(maps.Keys(message.GetFields())) {
			encoded, err := json.Marshal(message.GetFields()[key].AsInterface())
			if err != nil {
				encoded = fmt.Appendf(nil, "%q", err.Error())
			}
			out = append(out, protoFieldOccurrence{
				path:     path + "." + key,
				field:    name,
				nonEmpty: true,
				rendered: string(encoded),
			})
		}
		return out
	default:
		return []protoFieldOccurrence{{
			path:     path,
			field:    name,
			nonEmpty: true,
			rendered: fmt.Sprintf("%v", message),
		}}
	}
}

func isZeroScalar(fd protoreflect.FieldDescriptor, value protoreflect.Value) bool {
	switch fd.Kind() {
	case protoreflect.StringKind:
		return value.String() == ""
	case protoreflect.BytesKind:
		return len(value.Bytes()) == 0
	case protoreflect.BoolKind:
		return !value.Bool()
	case protoreflect.EnumKind:
		return value.Enum() == 0
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return value.Int() == 0
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return value.Uint() == 0
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		return value.Float() == 0
	default:
		return false
	}
}

func renderProtoScalar(fd protoreflect.FieldDescriptor, value protoreflect.Value) string {
	switch fd.Kind() {
	case protoreflect.StringKind:
		return fmt.Sprintf("%q", value.String())
	case protoreflect.BytesKind:
		return fmt.Sprintf("%q", string(value.Bytes()))
	case protoreflect.EnumKind:
		if vd := fd.Enum().Values().ByNumber(value.Enum()); vd != nil {
			return string(vd.Name())
		}
		return fmt.Sprintf("enum(%d)", value.Enum())
	default:
		return fmt.Sprintf("%v", value.Interface())
	}
}

func renderProtoMap(m protoreflect.Map) string {
	entries := make([]string, 0, m.Len())
	m.Range(func(k protoreflect.MapKey, v protoreflect.Value) bool {
		entries = append(entries, fmt.Sprintf("%s=%v", k.String(), v.Interface()))
		return true
	})
	slices.Sort(entries)
	return fmt.Sprintf("{%s}", strings.Join(entries, ", "))
}

// nonEmptyByField reports, per descriptor field name, whether any occurrence
// of that field in the tree holds a non-zero value.
func nonEmptyByField(occurrences []protoFieldOccurrence) map[string]bool {
	out := map[string]bool{}
	for _, occ := range occurrences {
		out[occ.field] = out[occ.field] || occ.nonEmpty
	}
	return out
}

func occurrencesOf(occurrences []protoFieldOccurrence, field string) string {
	var matches []string
	for _, occ := range occurrences {
		if occ.field == field && occ.nonEmpty {
			matches = append(matches, occ.path+"="+occ.rendered)
		}
	}
	return strings.Join(matches, ", ")
}
