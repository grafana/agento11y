package live_check_advice

import rego.v1

# The _otel_ prefix avoids helper name collisions with the GenAI policies.

# The trace API reserves OK status for application code that explicitly
# verifies success; instrumentation leaves successful spans UNSET.
# https://opentelemetry.io/docs/specs/otel/trace/api/#set-status

deny contains _otel_span_finding(
	"span_status_ok_set_by_instrumentation",
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

# The registry cannot express that error.type is conditional on span status.

deny contains _otel_span_finding(
	"error_type_missing_on_error",
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
	not _otel_has_attr(input.sample.span, "error.type")
}

deny contains _otel_span_finding(
	"error_type_without_error_status",
	"violation",
	input.sample.span,
	{"status_code": input.sample.span.status.code},
	sprintf(
		"Span '%v' sets 'error.type'='%v' but status.code is '%v', not 'error'.",
		[input.sample.span.name, _otel_attr_value(input.sample.span, "error.type"), input.sample.span.status.code],
	),
) if {
	input.sample.span
	_otel_has_attr(input.sample.span, "error.type")
	input.sample.span.status.code != "error"
}

# Weaver represents span attributes as a list of {name, value, type} objects.
_otel_has_attr(span, name) if {
	some attr in span.attributes
	attr.name == name
}

_otel_attr_value(span, name) := value if {
	some attr in span.attributes
	attr.name == name
	value := attr.value
}

# PolicyFinding format per
# https://github.com/open-telemetry/weaver/blob/main/crates/weaver_live_check/README.md#policyfinding
_otel_span_finding(id, level, span, context, message) := {
	"id":          id,
	"level":       level,
	"signal_type": "span",
	"signal_name": span.name,
	"context":     context,
	"message":     message,
}
