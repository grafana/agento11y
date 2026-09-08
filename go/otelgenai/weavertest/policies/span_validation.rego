package live_check_advice

import rego.v1

# The trace API reserves OK status for application code that explicitly
# verifies success; instrumentation leaves successful spans UNSET.
# https://opentelemetry.io/docs/specs/otel/trace/api/#set-status

deny contains _span_finding(
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

deny contains _span_finding(
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
	not _has_attr(input.sample.span, "error.type")
}

deny contains _span_finding(
	"error_type_without_error_status",
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
