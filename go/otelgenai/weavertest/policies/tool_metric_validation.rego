package live_check_advice

import rego.v1

# Weaver 0.26.1 checks enum membership but does not reject a numeric value for
# a string-valued open enum. Retain the scalar type from the resolved registry.
deny contains {
	"id": "type_mismatch",
	"level": "violation",
	"signal_type": "metric",
	"signal_name": metric.name,
	"context": {"attribute_key": attr.name, "attribute_type": attr.type, "expected": "string"},
	"message": concat("", ["Attribute '", attr.name, "' must have type 'string'."]),
} if {
	metric := input.sample.metric
	definition := data["coverage-model"].tool_metric_base
	metric.name == definition.name
	some point in metric.data_points
	some attr in point.attributes
	rule := definition.attributes[attr.name]
	count(rule.type.members) > 0
	every member in rule.type.members { is_string(member.value) }
	attr.type != "string"
}
