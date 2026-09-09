# GenAI span-name, kind, and operation-specific attribute rules that the
# registry cannot express. Expected attributes come from coverage-model.json.

package live_check_advice

import rego.v1

# Memory operations share gen_ai.memory.client rather than a span type derived
# from the operation name.
_memory_ops := {
	"search_memory",
	"create_memory",
	"update_memory",
	"upsert_memory",
	"delete_memory",
	"create_memory_store",
	"delete_memory_store",
}

# ─── Span name format (violation) ───────────────────────────────────────────

_span_name_keyed_attr["chat"]              := "gen_ai.request.model"
_span_name_keyed_attr["generate_content"]  := "gen_ai.request.model"
_span_name_keyed_attr["text_completion"]   := "gen_ai.request.model"
_span_name_keyed_attr["embeddings"]        := "gen_ai.request.model"
_span_name_keyed_attr["execute_tool"]      := "gen_ai.tool.name"
_span_name_keyed_attr["invoke_agent"]      := "gen_ai.agent.name"
_span_name_keyed_attr["create_agent"]      := "gen_ai.agent.name"
_span_name_keyed_attr["invoke_workflow"]   := "gen_ai.workflow.name"
_span_name_keyed_attr["retrieval"]         := "gen_ai.data_source.id"
_span_name_keyed_attr["plan"]              := "gen_ai.agent.name"

_operation_only_span_names := {"fetch_response"} | _memory_ops

# Span name SHOULD be `{op}` (when the keyed attribute is absent) or
# `{op} {value}` (when present). Mirrors the "SHOULD append when known"
# guidance in semconv.
#
# Avoid `%v ` patterns in sprintf: weaver 0.22.1's OPA-based sprintf
# consumes a single space character immediately following any verb (`%v`,
# `%s`, `%d`) — interpreting it as Go's space-flag — so `%v %v` produces
# `<a><b>` instead of `<a> <b>`. We use `concat` for the literal-space
# joins below.
deny contains _span_finding(
	"genai_span_name_format",
	"violation",
	input.sample.span,
	{
		"operation":     op,
		"keyed_attr":    keyed_attr,
		"expected_form": concat("", [op, " or '", op, " <", keyed_attr, ">'"]),
	},
	concat("", [
		op, " span name should be '",
		op, "' or '",
		op, " <value of ", keyed_attr, ">', got '",
		input.sample.span.name, "'",
	]),
) if {
	input.sample.span
	op := _attr_value(input.sample.span, "gen_ai.operation.name")
	keyed_attr := _span_name_keyed_attr[op]
	not _valid_op_and_attr_span_name(input.sample.span, op, keyed_attr)
}

deny contains _span_finding(
	"genai_span_name_format",
	"violation",
	input.sample.span,
	{"operation": op, "expected_form": op},
	sprintf("%v span name should be '%v', got '%v'", [op, op, input.sample.span.name]),
) if {
	input.sample.span
	op := _attr_value(input.sample.span, "gen_ai.operation.name")
	op in _operation_only_span_names
	input.sample.span.name != op
}

# ─── Per-operation expected attributes (violation) ──────────────────────────

_matching_span_type(op, _, "gen_ai.inference.client") if {
	op in {"chat", "generate_content", "text_completion"}
}

_matching_span_type(op, _, "gen_ai.memory.client") if {
	op in _memory_ops
}

_matching_span_type(op, kind, span_type) if {
	not op in {"chat", "generate_content", "text_completion"}
	not op in _memory_ops
	data["coverage-model"].spans[span_type]
	startswith(span_type, sprintf("gen_ai.%v", [op]))
	endswith(span_type, sprintf(".%v", [kind]))
}

# These recommended attributes have no value unless the request, response, or
# provider supplies one; absence is conformant even after a successful call.
_conditionally_available_recommended := {
	"gen_ai.request.temperature",
	"gen_ai.request.max_tokens",
	"gen_ai.request.top_p",
	"gen_ai.request.stop_sequences",
	"gen_ai.request.presence_penalty",
	"gen_ai.request.frequency_penalty",
	"gen_ai.request.encoding_formats",
	"gen_ai.usage.cache_creation.input_tokens",
	"gen_ai.usage.cache_read.input_tokens",
	"gen_ai.tool.description",
}

_level_expected(level, _) if {
	level == "required"
}

_level_expected(level, attr) if {
	level == "recommended"
	not _conditionally_available_recommended[attr]
}

_internal_client_span(op, kind) if {
	kind == "internal"
	op in {"chat", "generate_content", "text_completion"}
}

_internal_client_span(op, kind) if {
	kind == "internal"
	op in _memory_ops
}

_expected_for_op(op, kind) := expected if {
	data["coverage-model"].spans
	not _internal_client_span(op, kind)
	some span_type
	_matching_span_type(op, kind, span_type)
	attrs := data["coverage-model"].spans[span_type].attributes
	expected := { attr |
		some attr, level in attrs
		_level_expected(level, attr)
	}
}

# In-process inference follows the client attribute contract except for the
# remote endpoint address, which does not exist for an in-process model.
_expected_for_op(op, "internal") := expected if {
	op in {"chat", "generate_content", "text_completion"}
	attrs := data["coverage-model"].spans["gen_ai.inference.client"].attributes
	registry_expected := { attr |
		some attr, level in attrs
		_level_expected(level, attr)
	}
	expected := registry_expected - {"server.address"}
}

_expected_for_op(op, "internal") := expected if {
	op in _memory_ops
	attrs := data["coverage-model"].spans["gen_ai.memory.client"].attributes
	registry_expected := { attr |
		some attr, level in attrs
		_level_expected(level, attr)
	}
	expected := registry_expected - {"server.address"}
}

_response_attributes_unavailable_on_failure(op) := {
	"gen_ai.response.finish_reasons",
	"gen_ai.response.id",
	"gen_ai.response.model",
	"gen_ai.usage.input_tokens",
	"gen_ai.usage.output_tokens",
} if {
	op != "fetch_response"
}

# A fetch requires the requested response ID even when no response arrives.
_response_attributes_unavailable_on_failure("fetch_response") := {
	"gen_ai.response.finish_reasons",
	"gen_ai.response.model",
	"gen_ai.response.status",
}

_expected_for_span(span, op) := expected if {
	span.status.code == "error"
	expected := _expected_for_op(op, span.kind) - _response_attributes_unavailable_on_failure(op)
}

_expected_for_span(span, op) := expected if {
	span.status.code != "error"
	expected := _expected_for_op(op, span.kind)
}

# Per expected attribute, one violation if missing.
deny contains _span_finding(
	"genai_expected_attribute_missing",
	"violation",
	input.sample.span,
	{
		"operation":         op,
		"missing_attribute": attr_name,
	},
	sprintf(
		"Span '%v' (operation '%v') is missing expected attribute '%v'",
		[input.sample.span.name, op, attr_name],
	),
) if {
	input.sample.span
	op := _attr_value(input.sample.span, "gen_ai.operation.name")
	expected := _expected_for_span(input.sample.span, op)
	some attr_name in expected
	not _has_attr(input.sample.span, attr_name)
}

# ─── Per-operation span kind (violation) ────────────────────────────────────
#
# Declared kinds come from the coverage model. Inference and memory client
# conventions additionally allow INTERNAL for same-process calls.
_op_allowed_kind(op, span_type, span_def) := kind if {
	startswith(span_type, sprintf("gen_ai.%v", [op]))
	kind := span_def.kind
}

_expected_kinds_for_op[op] := {"client", "internal"} if {
	some op in {"chat", "generate_content", "text_completion"}
}

_expected_kinds_for_op[op] := {"client", "internal"} if {
	some op in _memory_ops
}

_expected_kinds_for_op[op] := kinds if {
	some op in data["coverage-model"].enums["gen_ai.operation.name"]
	not op in {"chat", "generate_content", "text_completion"}
	not op in _memory_ops
	kinds := { kind |
		some span_type, span_def in data["coverage-model"].spans
		kind := _op_allowed_kind(op, span_type, span_def)
	}
	count(kinds) > 0
}

deny contains _span_finding(
	"genai_span_kind_unexpected",
	"violation",
	input.sample.span,
	{
		"operation": op,
		"kind":      input.sample.span.kind,
	},
	sprintf(
		"Span '%v' (operation '%v') has kind '%v'; semconv expects one of %v",
		[input.sample.span.name, op, input.sample.span.kind, expected_list],
	),
) if {
	input.sample.span
	op := _attr_value(input.sample.span, "gen_ai.operation.name")
	expected_kinds := _expected_kinds_for_op[op]
	not expected_kinds[input.sample.span.kind]

	# Keep the comprehension in the body to avoid Weaver's OPA scheduling error.
	expected_list := sort([kind | some kind in expected_kinds])
}

# ─── Helpers ────────────────────────────────────────────────────────────────

# Span attributes arrive as `[{"name": ..., "value": ..., "type": ...}]`.

# True when the span has an attribute named `name`.
_has_attr(span, name) if {
	some attr in span.attributes
	attr.name == name
}

# Returns the value of the named attribute. Undefined (rule body fails) when
# the attribute isn't present — callers must guard with `_has_attr` first if
# they need to distinguish "absent" from "set to a falsy value".
_attr_value(span, name) := value if {
	some attr in span.attributes
	attr.name == name
	value := attr.value
}

# A valid span name is either exactly `{op}` (when the keyed attribute is
# absent) or `{op} {value}` (when present).
_valid_op_and_attr_span_name(span, op, attr_key) if {
	span.name == op
	not _has_attr(span, attr_key)
}

_valid_op_and_attr_span_name(span, op, attr_key) if {
	value := _attr_value(span, attr_key)
	# concat requires strings; let the registry report a non-string keyed attribute.
	is_string(value)
	# concat (not sprintf): see the note above the deny rule. sprintf("%v %v", ...)
	# silently produces "<a><b>" with no space, so every span with a `{op} {value}`
	# name would be reported as a violation.
	span.name == concat(" ", [op, value])
}

# PolicyFinding format per
# https://github.com/open-telemetry/weaver/blob/main/crates/weaver_live_check/README.md#policyfinding
_span_finding(id, level, span, context, message) := {
	"id":          id,
	"level":       level,
	"signal_type": "span",
	"signal_name": span.name,
	"context":     context,
	"message":     message,
}
