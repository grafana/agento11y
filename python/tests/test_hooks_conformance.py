"""Hook wire conformance for the Python SDK.

Checks the request the SDK serializes and the responses it parses against the
shared fixtures in `conformance/hooks/`, which are the only contract for an
endpoint with no generated stubs.
"""

from __future__ import annotations

import base64
import copy
import json
from typing import Any

import pytest
from agento11y import (
    HookContext,
    HookEvaluateRequest,
    HookInput,
    HookModel,
    Message,
    MessageRole,
    Part,
    PartKind,
    ToolCall,
    ToolResult,
)
from agento11y.hooks import _parse_response, _serialize_request
from hooks_fixtures import (
    BASH_SCHEMA,
    diff_json,
    load_postflight_guard_request,
    load_preflight_request,
    load_responses,
    postflight_guard_request,
    preflight_request,
)


@pytest.mark.parametrize(
    ("fixture_name", "build", "load"),
    [
        pytest.param("request-preflight.json", preflight_request, load_preflight_request, id="request-preflight"),
        pytest.param(
            "request-postflight-guard.json",
            postflight_guard_request,
            load_postflight_guard_request,
            id="request-postflight-guard",
        ),
    ],
)
def test_hooks_request_conformance(fixture_name: str, build: Any, load: Any) -> None:
    serialized = json.loads(json.dumps(_serialize_request(build())))
    diffs = diff_json(serialized, load())
    assert diffs == [], f"request does not match conformance/hooks/{fixture_name}:\n" + "\n".join(diffs)


@pytest.mark.parametrize(
    ("name", "mutate", "want_path"),
    [
        pytest.param(
            "renamed discriminator",
            lambda body: _rename_kind(_part(body, 0, 0)),
            "input.messages[0].parts[0].kind",
            id="renamed-discriminator",
        ),
        pytest.param(
            "unknown discriminator value",
            lambda body: _part(body, 0, 0).update({"kind": "message"}),
            "input.messages[0].parts[0].kind",
            id="unknown-discriminator",
        ),
        pytest.param(
            "base64 tool call input",
            lambda body: _base64_encode_in_place(_part(body, 1, 1)["tool_call"], "input_json"),
            "input.messages[1].parts[1].tool_call.input_json",
            id="base64-tool-call-input",
        ),
        pytest.param(
            "base64 tool result content",
            lambda body: _base64_encode_in_place(_part(body, 2, 0)["tool_result"], "content_json"),
            "input.messages[2].parts[0].tool_result.content_json",
            id="base64-tool-result-content",
        ),
        pytest.param(
            "raw JSON tool schema",
            lambda body: _raw_json_schema(body["input"]["tools"][0]),
            "input.tools[0].input_schema_json",
            id="raw-json-tool-schema",
        ),
    ],
)
def test_hook_fixture_comparison_names_divergent_fields(name: str, mutate: Any, want_path: str) -> None:
    """Pins the comparator, not the serializer.

    ``test_hooks_request_conformance`` is what checks the SDK's own output. Each
    case here applies one divergence the server cannot read and asserts the diff
    names the offending path. Without this test, a comparator that accepted a
    renamed discriminator or a re-encoded payload would pass every conformance
    case while checking nothing.
    """

    fixture = load_preflight_request()
    mutated = copy.deepcopy(fixture)
    mutate(mutated)

    diffs = diff_json(mutated, fixture)
    assert diffs, f"comparison accepted a divergent payload ({name})"
    assert any(diff.startswith(want_path) for diff in diffs), f"diff did not name {want_path}: {diffs}"


def test_hooks_response_conformance_allow() -> None:
    parsed = _parse_response(load_responses()["allow"])

    assert parsed.action == "allow"
    assert not parsed.is_deny
    assert parsed.transformed_input is None
    assert len(parsed.evaluations) == 1
    evaluation = parsed.evaluations[0]
    assert evaluation.rule_id == "pii-detect"
    assert evaluation.evaluator_id == "evaluator-pii"
    assert evaluation.evaluator_kind == "regex"
    assert evaluation.passed is True
    assert evaluation.latency_ms == 12
    assert evaluation.explanation == "no PII matches"


def test_hooks_response_conformance_deny() -> None:
    parsed = _parse_response(load_responses()["deny"])

    assert parsed.is_deny
    assert parsed.rule_id == "block-destructive-bash"
    assert parsed.reason == "Bash(*rm*) is not allowed in this environment"
    assert len(parsed.evaluations) == 1
    assert parsed.evaluations[0].passed is False
    assert parsed.evaluations[0].reason == "blocked tool Bash"


def test_hooks_response_conformance_transformed_input() -> None:
    parsed = _parse_response(load_responses()["allow_with_transformed_input"])

    assert parsed.action == "allow"
    transformed = parsed.transformed_input
    assert transformed is not None
    assert transformed.system_prompt == "You are a careful assistant."
    assert transformed.conversation_preview == "user: Delete the cache directory under [REDACTED]."
    assert len(transformed.messages) == 3

    user = transformed.messages[0]
    assert user.role == MessageRole.USER
    assert [p.kind for p in user.parts] == [PartKind.TEXT]
    assert user.parts[0].text == "Delete the cache directory under [REDACTED]."

    assistant = transformed.messages[1]
    assert assistant.role == MessageRole.ASSISTANT
    assert [p.kind for p in assistant.parts] == [PartKind.THINKING, PartKind.TOOL_CALL]
    assert assistant.parts[0].thinking == "The request is destructive, so inspect the directory first."
    call = assistant.parts[1].tool_call
    assert call is not None
    assert call.id == "call-bash"
    assert call.name == "Bash"
    assert call.input_json == b'{"command":"rm -rf /tmp/cache"}'

    tool = transformed.messages[2]
    assert tool.role == MessageRole.TOOL
    assert tool.name == "Bash"
    assert [p.kind for p in tool.parts] == [PartKind.TOOL_RESULT]
    result = tool.parts[0].tool_result
    assert result is not None
    assert result.tool_call_id == "call-bash"
    assert result.name == "Bash"
    assert result.is_error is True
    assert result.content == "rm: cannot remove '/tmp/cache': Permission denied"
    assert result.content_json == b'{"exit_code":1}'

    assert len(transformed.tools) == 1
    definition = transformed.tools[0]
    assert definition.name == "Bash"
    assert definition.description == "Run a shell command."
    assert definition.type == "function"
    assert definition.input_schema_json == BASH_SCHEMA


def test_hooks_response_conformance_accepts_proto_json_roles() -> None:
    """Any emitter that marshals the proto enum directly sends integer roles."""

    parsed = _parse_response(
        {
            "action": "allow",
            "transformed_input": {
                "messages": [
                    {"role": 2, "parts": [{"text": "hello"}]},
                    {
                        "role": 3,
                        "parts": [{"kind": "tool_result", "tool_result": {"tool_call_id": "call-1", "content": "ok"}}],
                    },
                ]
            },
            "evaluations": [],
        }
    )

    transformed = parsed.transformed_input
    assert transformed is not None
    assert transformed.messages[0].role == MessageRole.ASSISTANT
    assert transformed.messages[0].parts[0].kind == PartKind.TEXT
    assert transformed.messages[0].parts[0].text == "hello"
    assert transformed.messages[1].role == MessageRole.TOOL
    result = transformed.messages[1].parts[0].tool_result
    assert result is not None
    assert result.tool_call_id == "call-1"
    assert result.content == "ok"


@pytest.mark.parametrize(
    ("case", "value", "expected"),
    [
        pytest.param(
            "base64 of JSON",
            base64.b64encode(b'{"command":"ls /tmp"}').decode("ascii"),
            b'{"command":"ls /tmp"}',
            id="base64-of-json",
        ),
        pytest.param(
            "base64 of plain text",
            base64.b64encode(b"plain text tool output").decode("ascii"),
            b'"plain text tool output"',
            id="base64-of-plain-text",
        ),
        pytest.param(
            "embedded JSON text",
            '{"command":"ls /tmp"}',
            b'{"command":"ls /tmp"}',
            id="embedded-json-text",
        ),
        pytest.param(
            "neither base64 nor JSON",
            "not base64 either",
            b'"not base64 either"',
            id="neither-base64-nor-json",
        ),
    ],
)
def test_hooks_response_payload_decoding_is_always_json(case: str, value: str, expected: bytes) -> None:
    """Response payloads are base64 of whatever bytes the proto field held.

    Nothing guarantees those bytes are JSON, and the parsed value has to stay a
    JSON document so a transform can be re-exported or re-sent. Go and JS resolve
    these four cases the same way; see `conformance/hooks/README.md`.
    """

    parsed = _parse_response(
        {
            "action": "allow",
            "transformed_input": {
                "messages": [
                    {
                        "role": "assistant",
                        "parts": [
                            {
                                "kind": "tool_call",
                                "tool_call": {"id": "call-1", "name": "Bash", "input_json": value},
                            }
                        ],
                    }
                ]
            },
            "evaluations": [],
        }
    )

    transformed = parsed.transformed_input
    assert transformed is not None, case
    call = transformed.messages[0].parts[0].tool_call
    assert call is not None
    assert call.input_json == expected
    json.loads(call.input_json)


@pytest.mark.parametrize(
    "schema",
    [
        pytest.param({"type": "object"}, id="raw-json-object"),
        pytest.param("eyJhIjoxfQ", id="unpadded-base64"),
        pytest.param("not base64 either", id="plain-text"),
    ],
)
def test_hooks_response_malformed_tool_schema_keeps_the_verdict(schema: Any) -> None:
    """A malformed tool schema must not cost the caller the deny it has to enforce."""

    parsed = _parse_response(
        {
            "action": "deny",
            "rule_id": "block-destructive-bash",
            "reason": "denied",
            "transformed_input": {"tools": [{"name": "Bash", "input_schema_json": schema}]},
            "evaluations": [],
        }
    )

    assert parsed.is_deny
    assert parsed.rule_id == "block-destructive-bash"


def test_hooks_request_keeps_an_unparsable_payload() -> None:
    """Streaming providers accumulate tool arguments without validating them.

    A truncated payload has to reach the server as text. Failing the request
    instead would give the caller a fail-open allow for what is only a payload
    problem. Go and JS send the same text.
    """

    body = _serialize_request(
        HookEvaluateRequest(
            phase="preflight",
            context=HookContext(model=HookModel(provider="anthropic", name="claude-sonnet-4")),
            input=HookInput(
                messages=[
                    Message(
                        role=MessageRole.ASSISTANT,
                        parts=[
                            Part(
                                kind=PartKind.TOOL_CALL,
                                tool_call=ToolCall(id="call-1", name="Bash", input_json=b'{"command":"truncat'),
                            ),
                            Part(
                                kind=PartKind.TOOL_RESULT,
                                tool_result=ToolResult(tool_call_id="call-1", content_json=b'{"exit_code'),
                            ),
                        ],
                    )
                ]
            ),
        )
    )

    parts = body["input"]["messages"][0]["parts"]
    assert parts[0]["tool_call"]["input_json"] == '{"command":"truncat'
    assert parts[1]["tool_result"]["content_json"] == '{"exit_code'


def test_hooks_request_serializes_a_part_less_message_as_an_empty_array() -> None:
    """All three SDKs send `"parts": []` rather than null for a message with no parts."""

    body = _serialize_request(
        HookEvaluateRequest(
            phase="preflight",
            context=HookContext(model=HookModel(provider="anthropic", name="claude-sonnet-4")),
            input=HookInput(messages=[Message(role=MessageRole.USER, parts=[])]),
        )
    )

    assert body["input"]["messages"] == [{"role": "user", "parts": []}]


def test_hooks_response_empty_transformed_input_is_no_transform() -> None:
    """The server emits `transformed_input:{}` for a rule that returns an empty input.

    Reporting a transform for that body would let a caller replace the prompt with
    nothing. Go and JS also report no transform.
    """

    parsed = _parse_response({"action": "allow", "transformed_input": {}, "evaluations": []})

    assert parsed.transformed_input is None


def _part(body: dict[str, Any], message: int, part: int) -> dict[str, Any]:
    return body["input"]["messages"][message]["parts"][part]


def _rename_kind(part: dict[str, Any]) -> None:
    part["type"] = part.pop("kind")


def _base64_encode_in_place(payload: dict[str, Any], key: str) -> None:
    payload[key] = base64.b64encode(json.dumps(payload[key], separators=(",", ":")).encode()).decode("ascii")


def _raw_json_schema(tool: dict[str, Any]) -> None:
    tool["input_schema"] = json.loads(base64.b64decode(tool.pop("input_schema_json")))


# One response part, and the part the SDK must report for it.
#
# ``kind`` names the field the parser reads, and the parser commits to it: a part
# that lost that field is dropped rather than rebuilt from a leftover field, which
# would report a part the rule never wrote. An empty payload carries no content
# either, and keeping it would append an empty part that the request serializer
# drops on the way back out. A tool call without a name goes too, because the
# caller can neither route nor re-send it. A part with no ``kind`` at all is still
# read from whichever payload field is set, because the server always sets
# ``kind``, so that shape only reaches the SDK from a hand-written or
# protobuf-JSON body.
#
# Go and JS resolve every case here the same way; see conformance/hooks/README.md.
_RESPONSE_PART_CASES = [
    pytest.param({"kind": "text", "text": "kept"}, Part(kind=PartKind.TEXT, text="kept"), id="text"),
    pytest.param({"kind": "text", "text": ""}, None, id="text-without-text"),
    # The shape the server emits for an empty text part: its encoder omits the field
    # rather than sending "".
    pytest.param({"kind": "text"}, None, id="text-without-a-text-field"),
    pytest.param(
        {"kind": "thinking", "thinking": "planning"},
        Part(kind=PartKind.THINKING, thinking="planning"),
        id="thinking",
    ),
    pytest.param({"kind": "thinking", "thinking": ""}, None, id="thinking-without-thinking"),
    pytest.param({"kind": "thinking"}, None, id="thinking-without-a-thinking-field"),
    pytest.param(
        {"kind": "tool_call", "tool_call": {"id": "call-1", "name": "Bash"}},
        Part(kind=PartKind.TOOL_CALL, tool_call=ToolCall(name="Bash", id="call-1")),
        id="tool-call",
    ),
    pytest.param({"kind": "tool_call"}, None, id="tool-call-without-payload"),
    pytest.param({"kind": "tool_call", "tool_call": {"id": "call-1"}}, None, id="tool-call-without-name"),
    pytest.param(
        {"kind": "tool_result", "tool_result": {"tool_call_id": "call-1", "content": "ok"}},
        Part(kind=PartKind.TOOL_RESULT, tool_result=ToolResult(tool_call_id="call-1", content="ok")),
        id="tool-result",
    ),
    pytest.param({"kind": "tool_result"}, None, id="tool-result-without-payload"),
    pytest.param({"kind": "image"}, None, id="unknown-kind"),
    pytest.param(
        {"kind": "image", "text": "described by the server as text"},
        Part(kind=PartKind.TEXT, text="described by the server as text"),
        id="unknown-kind-with-text",
    ),
    pytest.param({"kind": "image", "tool_call": {"name": "Bash"}}, None, id="unknown-kind-with-tool-call"),
    pytest.param({"kind": "tool_call", "text": "not a tool call"}, None, id="tool-call-kind-with-text"),
    pytest.param({"kind": "tool_result", "text": "not a tool result"}, None, id="tool-result-kind-with-text"),
    pytest.param(
        {"kind": "thinking", "thinking": "", "text": "not thinking"},
        None,
        id="thinking-kind-with-text",
    ),
    pytest.param(
        {"kind": "text", "text": "", "thinking": "not text"},
        None,
        id="text-kind-with-thinking",
    ),
    pytest.param(
        {"kind": "text", "text": "", "tool_call": {"name": "Bash"}},
        None,
        id="text-kind-with-tool-call",
    ),
    pytest.param({"text": "recovered text"}, Part(kind=PartKind.TEXT, text="recovered text"), id="no-kind-text"),
    pytest.param(
        {"thinking": "recovered thinking"},
        Part(kind=PartKind.THINKING, thinking="recovered thinking"),
        id="no-kind-thinking",
    ),
    pytest.param(
        {"tool_call": {"id": "call-1", "name": "Bash"}},
        Part(kind=PartKind.TOOL_CALL, tool_call=ToolCall(name="Bash", id="call-1")),
        id="no-kind-tool-call",
    ),
    pytest.param(
        {"tool_result": {"tool_call_id": "call-1", "content": "ok"}},
        Part(kind=PartKind.TOOL_RESULT, tool_result=ToolResult(tool_call_id="call-1", content="ok")),
        id="no-kind-tool-result",
    ),
    pytest.param({}, None, id="empty-part"),
]


@pytest.mark.parametrize(("part", "want"), _RESPONSE_PART_CASES)
def test_hooks_response_part_parsing(part: dict[str, Any], want: Part | None) -> None:
    parsed = _parse_response(
        {
            "action": "allow",
            "transformed_input": {"messages": [{"role": "assistant", "parts": [part]}]},
            "evaluations": [],
        }
    )

    transformed = parsed.transformed_input
    assert transformed is not None
    assert transformed.messages[0].parts == ([] if want is None else [want])
