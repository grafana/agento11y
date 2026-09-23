"""Print spans from a trace exported as JSON by a trace-store CLI.

Feed it the JSON of a single trace (OTLP/JSON shape: ``{"trace": {"resourceSpans":
[...]}}`` or a bare ``{"resourceSpans": [...]}``). Shows resource attributes, span
names, ids, durations, status and every attribute, which is how the generation
and tool spans get checked attribute by attribute.

The span and parent ids are printed because a tool span is started inside the
context of the generation that asked for it, so the parent of an
``execute_tool`` span should be the ``generateText`` span above it. The parent
span has already ended by then, which is legal and leaves the child's window
reaching past its parent's end.

usage: show-spans.py <trace.json>
"""

from __future__ import annotations

import json
import sys


def attrs(items: list | None) -> dict[str, object]:
    out: dict[str, object] = {}
    for item in items or []:
        value = item.get("value") or {}
        for key in ("stringValue", "intValue", "doubleValue", "boolValue"):
            if key in value:
                out[item["key"]] = value[key]
                break
        else:
            out[item["key"]] = json.dumps(value)
    return out


document = json.load(open(sys.argv[1]))
root = document.get("trace", document)

for resource_spans in root.get("resourceSpans", []):
    resource = attrs(resource_spans.get("resource", {}).get("attributes"))
    print("RESOURCE:", {key: resource[key] for key in sorted(resource)})
    for scope_spans in resource_spans.get("scopeSpans", []):
        print("  scope:", scope_spans.get("scope", {}).get("name"))
        for span in scope_spans.get("spans", []):
            duration = (int(span["endTimeUnixNano"]) - int(span["startTimeUnixNano"])) / 1e9
            print(f"  SPAN {span['name']} kind={span.get('kind')} dur={duration:.3f}s status={span.get('status')}")
            print(f"    span_id={span.get('spanId')} parent_span_id={span.get('parentSpanId') or '(root)'}")
            span_attrs = attrs(span.get("attributes"))
            for key in sorted(span_attrs):
                print(f"    {key} = {str(span_attrs[key])[:300]}")
            for event in span.get("events") or []:
                print("    EVENT", event.get("name"), json.dumps(attrs(event.get("attributes")))[:300])
