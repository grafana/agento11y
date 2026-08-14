# Validates the JSON payload of GenAI content attributes against the semconv
# JSON schemas. The schemas ship with the semconv-genai registry
# (model/gen-ai/*.json) and are loaded into rego data by weaver's `--advice-data`
# flag, keyed by the schema file's stem (e.g. gen-ai-input-messages.json ->
# data["gen-ai-input-messages"]), so this policy references them as data
# documents rather than inlining them.

package live_check_advice

import rego.v1

# Content attribute name -> advice-data key (the schema file's stem).
_genai_content_schema_keys := {
	"gen_ai.input.messages":      "gen-ai-input-messages",
	"gen_ai.output.messages":     "gen-ai-output-messages",
	"gen_ai.system_instructions": "gen-ai-system-instructions",
	"gen_ai.tool.definitions":    "gen-ai-tool-definitions",
	"gen_ai.retrieval.documents": "gen-ai-retrieval-documents",
}

deny contains result if {
	input.sample.attribute
	attr_name := input.sample.attribute.name
	attr_value := input.sample.attribute.value
	is_string(attr_value)

	key := _genai_content_schema_keys[attr_name]
	# Undefined (so the rule skips this attribute) when the schema isn't loaded
	# for the pinned semconv version yet — e.g. forward-looking attributes like
	# `gen_ai.tool.definitions` before upstream ships the schema.
	schema := data[key]

	parsed := json.unmarshal(attr_value)

	[matched, errors] := json.match_schema(parsed, schema)
	not matched

	# PolicyFinding format per
	# https://github.com/open-telemetry/weaver/blob/main/crates/weaver_live_check/README.md#policyfinding
	# (id / level / context / message; signal_* omitted because the sample
	# is attribute-level and weaver doesn't surface the parent span here).
	result := {
		"id":    "genai_content_schema",
		"level": "violation",
		"context": {
			"attribute": attr_name,
			"errors":    errors,
		},
		"message": sprintf(
			"Attribute '%v' value does not conform to the GenAI schema: %v",
			[attr_name, errors],
		),
	}
}
