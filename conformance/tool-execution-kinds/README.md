# Tool execution kind cases

`cases.json` defines the shared tool-span contract for the five SDKs and Go `otelgenai`.
The definitions follow [merged GenAI #498](https://github.com/open-telemetry/semantic-conventions-genai/pull/498).
No case requires network access, file resolution, or actual command execution.

`execution_kind` identifies one of three kinds of tool execution:

- `load_skill`: loads a skill's instructions.
- `read_skill_resource`: reads a resource associated with a skill.
- `command`: executes a command, optionally associated with a skill or resource.

An absent or unknown execution kind means generic tool execution.

## Schema (version 1)

- `skill_process_attributes`: the complete set of skill and process attribute keys. Consumers must omit every key absent from a case's `attributes`.
- `private_attributes`: keys that must never reach a sampler. Reduced-content spans must omit them too.
- `cases`: independent executions, each producing exactly one INTERNAL `execute_tool` span.
- `id`: unique test name.
- `start`: snake-case public start fields, including optional `execution_kind`. Adapters map names to each language's API.
- `end`: optional completion fields. An absent exit code is unknown, not zero.
- `capture_mode`: SDK content mode; defaults to `no_tool_content`.
- `include_content`: legacy tool-content opt-in; defaults to false.
- `otel_capture`: optional low-level capture override. Otherwise `full` maps to `SPAN_ONLY`, and reduced/default cases map to `NO_CONTENT`.
- `error`: optional execution error, independent of exit code.
- `invalid_exit_code`: dynamic-language input that must be rejected or omitted, never converted to a successful zero. Statically typed consumers leave it unset.
- `dynamic_languages`: optional restriction on which dynamic consumers test the invalid input. Other consumers exercise an absent code.
- `span_name`: exact finished name, also expected at creation when all naming inputs are known.
- `attributes`: exact skill and process attributes on the finished span. Other existing tool attributes are not enumerated here.

Runtime fixture tests also check that `agento11y.skill.name` is absent, metrics have no skill/process dimensions, and creation attributes include applicable structural names.
They verify that samplers receive neither exit codes nor the three private fields.
The focused registry consumer validates recorded spans only.

Core `no_tool_content` retains the three private fields without the arguments/results opt-in.
The low-level `no_tool_content_private_fields` case uses `SPAN_ONLY` because `otelgenai` has independent capture modes.

The Go and Python experimental OTel transports resolve `full_with_metadata_spans` to `full`.
Consumers testing those transports must apply the effective mode, rather than the unresolved fixture mode.
The normal Python transport uses the fixture expectations unchanged.

Copy/clone tests supplement these cases. They verify start fields and absent, zero, and nonzero completion codes.
Java and JavaScript completed debug snapshots preserve the added scalar fields; their existing raw payloads are not sanitized by capture modes.
