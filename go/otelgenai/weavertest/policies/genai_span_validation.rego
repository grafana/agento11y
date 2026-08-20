# Validates GenAI span shape beyond what weaver's semconv-registry-driven
# checks already enforce. The registry validates per-attribute requirements
# (name, type, presence) for spans matching its definitions; this file adds
# cross-cutting span-level invariants the registry can't easily express.
#
# Three classes of rules, all keyed on `gen_ai.operation.name`:
#
#   1. Span name format → `violation`
#      (`{operation_name} {request_model}` for inference / embeddings,
#      `{operation_name} {agent_name}` for invoke_agent / create_agent,
#      `{operation_name} {tool_name}` for execute_tool).
#
#   2. Per-operation expected attributes → `violation`
#      Combines `Required` (always must be set) and the always-emit subset
#      of `Recommended` (e.g. response model/id, token usage on inference)
#      into one manifest per operation. Sourced from the rendered tables in
#      semantic-conventions/docs/gen-ai/gen-ai-spans.md and
#      gen-ai-agent-spans.md (the MD flattens the YAML inheritance chain
#      via `extends:`, so it's the right place to source from).
#      `invoke_agent` is the one operation whose manifest also depends on
#      span kind: semconv defines separate internal (same-process) and
#      client (remote) spans, and only the client span carries server.*.
#
# The "set when known" Recommended subset (sampling parameters like
# `frequency_penalty`, `max_tokens`; provider-side caches; conditionally-
# emitted things like `gen_ai.response.time_to_first_chunk` for streaming)
# is deliberately NOT flagged here — those depend on user input or on the
# request shape and would produce noisy false positives. Cross-attribute
# conditional rules (e.g. "if streaming, response.time_to_first_chunk
# SHOULD be set") would also belong here.
#
# Required attributes are also flagged by weaver's registry-driven
# validation. Listing them here too is intentional: rego rules give us
# stable advice ids to grep for in reports and let us tighten the check
# regardless of how the registry classifies the gap.
#
# Attribute access: weaver hands rego a span sample where `attributes` is a
# **list** of `{name, value, type}` objects, not a dict — `_attr(name)`
# walks that list and returns the value (or `null` if absent).

package live_check_advice

import rego.v1

# ─── Operation classification ───────────────────────────────────────────────
#
# Mirrors the semconv `gen_ai.operation.name` enum
# (model/gen-ai/registry.yaml). When semconv adds a new operation, append it
# to the matching set below — or leave it out if the new operation has its
# own span definition with different conventions.

_inference_ops := {"chat", "generate_content", "text_completion"}

_embeddings_ops := {"embeddings"}

_tool_ops := {"execute_tool"}

_invoke_agent_ops := {"invoke_agent"}

_create_agent_ops := {"create_agent"}

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

# ─── Per-operation expected attributes (violation) ──────────────────────────

# `_expected_for_op(op, kind)` returns the expected-attribute manifest for a
# span given its `gen_ai.operation.name` and span kind. Most operations ignore
# kind (second arg is a wildcard); invoke_agent dispatches on it because
# semconv splits it into separate internal and client spans. Undefined (→ no
# violations) for an unmapped op or an unexpected agent span kind.
_expected_for_op("chat", _) := _inference_expected

_expected_for_op("generate_content", _) := _inference_expected

_expected_for_op("text_completion", _) := _inference_expected

_expected_for_op("embeddings", _) := _embeddings_expected

_expected_for_op("execute_tool", _) := _execute_tool_expected

_expected_for_op("invoke_agent", kind) := _invoke_agent_expected[kind]

_expected_for_op("create_agent", _) := _create_agent_expected

_expected_for_op("retrieval", _) := _retrieval_expected

_expected_for_op("invoke_workflow", _) := _invoke_workflow_expected

# Inference (chat / generate_content / text_completion).
# Required: gen_ai.operation.name, gen_ai.provider.name.
# Always-emit Recommended: response model/id, finish reasons, token usage,
# server.address. Sampling parameters (frequency_penalty, max_tokens, …),
# cache counters, and `gen_ai.response.time_to_first_chunk` (streaming-only)
# are conditional and not flagged here.
_inference_expected := {
	"gen_ai.operation.name",
	"gen_ai.provider.name",
	"gen_ai.response.model",
	"gen_ai.response.id",
	"gen_ai.response.finish_reasons",
	"gen_ai.usage.input_tokens",
	"gen_ai.usage.output_tokens",
	"server.address"
}

# Embeddings.
# Required: gen_ai.operation.name, gen_ai.provider.name.
# Always-emit Recommended: dimension.count, response.model, input tokens,
# server.address. (`gen_ai.request.encoding_formats` is conditional.)
_embeddings_expected := {
	"gen_ai.operation.name",
	"gen_ai.provider.name",
	"gen_ai.embeddings.dimension.count",
	"gen_ai.response.model",
	"gen_ai.usage.input_tokens",
	"server.address"
}

# Tool execution.
# Required: gen_ai.operation.name, gen_ai.tool.name.
# Recommended-when-available: gen_ai.tool.call.id, gen_ai.tool.type. (Tool
# description is genuinely optional per provider — not flagged.)
_execute_tool_expected := {
	"gen_ai.operation.name",
	"gen_ai.tool.name",
	"gen_ai.tool.call.id",
	"gen_ai.tool.type",
}

# Invoke agent (internal)
# Required: gen_ai.operation.name
_invoke_agent_expected["internal"] := {
	"gen_ai.operation.name",
}

# Invoke agent (client)
# Required: gen_ai.operation.name, gen_ai.provider.name.
# Always-emit Recommended: server.address.
_invoke_agent_expected["client"] := {
	"gen_ai.operation.name",
	"gen_ai.provider.name",
	"server.address",
}

# Create agent. After creation completes the provider returns an agent.id;
# flag it as always-emit on create_agent.
_create_agent_expected := {
	"gen_ai.operation.name",
	"gen_ai.provider.name",
	"gen_ai.agent.id",
}

# Retrieval. Only gen_ai.operation.name is unconditionally required.
_retrieval_expected := {
	"gen_ai.operation.name",
	"server.address",
}

# Invoke workflow. Only gen_ai.operation.name is unconditionally required;
# gen_ai.workflow.name is conditionally required "when available" but is
# effectively always known where a workflow is instrumented, so flag it.
_invoke_workflow_expected := {
	"gen_ai.operation.name",
	"gen_ai.workflow.name",
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
	expected := _expected_for_op(op, input.sample.span.kind)
	some attr_name in expected
	not _has_attr(input.sample.span, attr_name)
}

# ─── Per-operation span kind (violation) ────────────────────────────────────
#
# Semconv pins the span kind for each operation. `_expected_kinds_for_op`
# returns the set of kinds semconv allows; a span whose kind is outside that
# set is flagged. Single-element sets are the unambiguous cases (inference and
# embeddings are remote calls → CLIENT; tool execution runs in-process →
# INTERNAL). `invoke_agent` / `create_agent` may be same-process or remote, so
# both kinds are allowed. Undefined (→ no violation) for unmapped ops.
_expected_kinds_for_op["chat"]             := {"client"}
_expected_kinds_for_op["generate_content"] := {"client"}
_expected_kinds_for_op["text_completion"]  := {"client"}
_expected_kinds_for_op["embeddings"]       := {"client"}
_expected_kinds_for_op["execute_tool"]     := {"internal"}
_expected_kinds_for_op["invoke_workflow"]  := {"internal"}
_expected_kinds_for_op["retrieval"]        := {"client"}
_expected_kinds_for_op["invoke_agent"]     := {"internal", "client"}
_expected_kinds_for_op["create_agent"]     := {"internal", "client"}

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
		[input.sample.span.name, op, input.sample.span.kind, sorted_kinds],
	),
) if {
	input.sample.span
	op := _attr_value(input.sample.span, "gen_ai.operation.name")
	expected_kinds := _expected_kinds_for_op[op]
	not expected_kinds[input.sample.span.kind]

	# The comprehension and the sort stay in the body. Evaluating them
	# inside the sprintf arguments of the head makes OPA fail the whole
	# query with "statements not scheduled in query", which Weaver
	# reports as a fatal advisor error rather than a finding, so one span
	# with an unexpected kind aborts the run.
	sorted_kinds := sort([k | expected_kinds[k]])
}

# ─── Span status (violation) ────────────────────────────────────────────────
#
# Per the OpenTelemetry trace spec, instrumentation libraries MUST NOT set
# span status to OK — that value is reserved for application code that has
# explicitly verified the call succeeded. Instrumentations should leave
# status UNSET on success and set ERROR on failure.
# https://opentelemetry.io/docs/specs/otel/trace/api/#set-status

deny contains _span_finding(
	"genai_span_status_ok_set_by_instrumentation",
	"violation",
	input.sample.span,
	{"status_code": input.sample.span.status.code},
	sprintf(
		"Span '%v' has status.code='ok'; instrumentations must leave status UNSET on success (OK is reserved for application code).",
		[input.sample.span.name],
	),
) if {
	input.sample.span
	input.sample.span.status.code == "ok"
}

# ─── error.type on failure (violation) ──────────────────────────────────────
#
# Semconv requires `error.type` to be set when an operation fails. The
# registry can't express this — it's conditional on span status — so check
# both directions: an error span MUST carry error.type, and error.type MUST
# NOT appear on a non-error span.

deny contains _span_finding(
	"genai_error_type_missing_on_error",
	"violation",
	input.sample.span,
	{"status_code": input.sample.span.status.code},
	sprintf(
		"Span '%v' has status.code='error' but is missing 'error.type'; it MUST be set when the operation fails.",
		[input.sample.span.name],
	),
) if {
	input.sample.span
	input.sample.span.status.code == "error"
	not _has_attr(input.sample.span, "error.type")
}

deny contains _span_finding(
	"genai_error_type_without_error_status",
	"violation",
	input.sample.span,
	{"status_code": input.sample.span.status.code},
	sprintf(
		"Span '%v' sets 'error.type'='%v' but status.code is '%v', not 'error'.",
		[input.sample.span.name, _attr_value(input.sample.span, "error.type"), input.sample.span.status.code],
	),
) if {
	input.sample.span
	_has_attr(input.sample.span, "error.type")
	input.sample.span.status.code != "error"
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
