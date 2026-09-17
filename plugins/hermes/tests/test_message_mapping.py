"""Turning hermes payloads into SDK messages.

Hermes hands over whatever the provider used, so the same tool call arrives as
a dict on one route and as an object with attributes on another, and its
arguments arrive as a JSON string about as often as a dict. This is the layer
that flattens that, and it has to drop what it cannot read instead of raising:
it runs inside the argument list of ``set_result``.
"""

from __future__ import annotations

import json
from typing import Any

import pytest
from agento11y import MessageRole

from grafana_agento11y_hermes import _client, _config, _hooks


class _Function:
    def __init__(self, name: str, arguments: Any) -> None:
        self.name = name
        self.arguments = arguments


class _ToolCall:
    """A provider SDK's tool call: attributes, not keys."""

    def __init__(self, id: str, function: Any) -> None:
        self.id = id
        self.function = function


class _AssistantObject:
    def __init__(self, content: Any, tool_calls: Any = None) -> None:
        self.content = content
        self.tool_calls = tool_calls


# --- tool call flattening ---


def test_a_dict_shaped_tool_call_is_read() -> None:
    calls = _hooks._serialize_tool_calls([{"id": "c1", "function": {"name": "bash", "arguments": {"command": "ls"}}}])
    assert calls == [{"id": "c1", "name": "bash", "arguments": {"command": "ls"}}]


def test_an_object_shaped_tool_call_is_read() -> None:
    calls = _hooks._serialize_tool_calls([_ToolCall("c1", _Function("bash", {"command": "ls"}))])
    assert calls == [{"id": "c1", "name": "bash", "arguments": {"command": "ls"}}]


def test_an_object_tool_call_without_a_function_is_kept_nameless() -> None:
    calls = _hooks._serialize_tool_calls([_ToolCall("c1", None)])
    assert calls == [{"id": "c1", "name": "", "arguments": None}]


@pytest.mark.parametrize(
    ("arguments", "expected"),
    (
        # OpenAI sends the arguments as a JSON string.
        ('{"command": "ls"}', {"command": "ls"}),
        # Anthropic sends them already decoded.
        ({"command": "ls"}, {"command": "ls"}),
        # A string that is not JSON stays a string rather than costing the call.
        ("not json at all", "not json at all"),
        ('{"unterminated": ', '{"unterminated": '),
        (None, None),
    ),
)
def test_tool_call_arguments_are_decoded_where_possible(arguments: Any, expected: Any) -> None:
    calls = _hooks._serialize_tool_calls([{"id": "c1", "function": {"name": "bash", "arguments": arguments}}])
    assert calls[0]["arguments"] == expected


def test_no_tool_calls_is_an_empty_list() -> None:
    assert _hooks._serialize_tool_calls(None) == []
    assert _hooks._serialize_tool_calls([]) == []


# --- message conversion ---


def test_a_user_message_becomes_one_text_part() -> None:
    msg = _hooks._to_sdk_message({"role": "user", "content": "hi"})
    assert msg is not None
    assert msg.role == MessageRole.USER
    assert len(msg.parts) == 1


def test_an_empty_user_message_carries_no_parts() -> None:
    msg = _hooks._to_sdk_message({"role": "user", "content": ""})
    assert msg is not None
    assert msg.parts == []


def test_a_tool_result_message_is_keyed_by_its_call_id() -> None:
    msg = _hooks._to_sdk_message({"role": "tool", "tool_call_id": "c1", "content": "a.txt"})
    assert msg is not None
    assert msg.role == MessageRole.TOOL
    assert len(msg.parts) == 1


def test_an_assistant_message_carries_text_and_tool_calls_together() -> None:
    msg = _hooks._to_sdk_message(
        {
            "role": "assistant",
            "content": "running it",
            "tool_calls": [{"id": "c1", "function": {"name": "bash", "arguments": {"command": "ls"}}}],
        }
    )
    assert msg is not None
    assert msg.role == MessageRole.ASSISTANT
    assert len(msg.parts) == 2, "one text part and one tool-call part"


def test_an_assistant_message_with_only_tool_calls_has_no_text_part() -> None:
    msg = _hooks._to_sdk_message(
        {"role": "assistant", "content": None, "tool_calls": [{"id": "c1", "function": {"name": "bash"}}]}
    )
    assert msg is not None
    assert len(msg.parts) == 1


def test_circular_tool_call_arguments_stop_at_the_depth_limit() -> None:
    circular: dict[str, Any] = {}
    circular["self"] = circular
    msg = _hooks._to_sdk_message(
        {"role": "assistant", "tool_calls": [{"id": "c1", "function": {"name": "bash", "arguments": circular}}]}
    )
    assert msg is not None
    assert json.loads(msg.parts[0].tool_call.input_json) == {
        "self": {"self": {"self": {"self": {"self": "<max-depth>"}}}}
    }


def test_tool_call_arguments_that_cannot_be_sanitized_become_empty_input(monkeypatch: pytest.MonkeyPatch) -> None:
    def fail(*args: Any, **kwargs: Any) -> Any:
        raise ValueError("cannot sanitize")

    monkeypatch.setattr(_hooks._redact, "safe_value", fail)
    msg = _hooks._to_sdk_message(
        {"role": "assistant", "tool_calls": [{"id": "c1", "function": {"name": "bash", "arguments": {}}}]}
    )

    assert msg.parts[0].tool_call.input_json == b""


@pytest.mark.parametrize("max_chars", [None, 80])
@pytest.mark.parametrize("encoded", [False, True])
def test_generation_tool_payloads_respect_the_string_limit(
    monkeypatch: pytest.MonkeyPatch, max_chars: int | None, encoded: bool
) -> None:
    if max_chars is not None:
        monkeypatch.setattr(_client, "_CONFIG", _config.PluginConfig(max_chars=max_chars))
    limit = max_chars if max_chars is not None else 12000
    payload = "x" * (limit + 100)
    arguments = {"value": payload}
    msg = _hooks._to_sdk_message(
        {
            "role": "assistant",
            "tool_calls": [
                {"id": "c1", "function": {"name": "bash", "arguments": json.dumps(arguments) if encoded else arguments}}
            ],
        }
    )
    result = _hooks._to_sdk_message({"role": "tool", "tool_call_id": "c1", "content": payload})

    expected = "x" * limit + "... [truncated 100 chars]"
    assert json.loads(msg.parts[0].tool_call.input_json) == {"value": expected}
    assert result.parts[0].tool_result.content == expected
    assert arguments == {"value": payload}


def test_generation_tool_arguments_respect_structural_limits() -> None:
    arguments = {"items": list(range(100)), "nested": {"a": {"b": {"c": {"d": "deep"}}}}}
    msg = _hooks._to_sdk_message(
        {"role": "assistant", "tool_calls": [{"function": {"name": "bash", "arguments": arguments}}]}
    )

    recorded = json.loads(msg.parts[0].tool_call.input_json)
    assert recorded["items"] == list(range(50))
    assert recorded["nested"]["a"]["b"]["c"]["d"] == "<max-depth>"


def test_an_unknown_role_is_dropped() -> None:
    assert _hooks._to_sdk_message({"role": "developer", "content": "x"}) is None
    assert _hooks._to_sdk_message({"content": "no role at all"}) is None


@pytest.mark.parametrize("messages", (None, "a string", {"role": "user"}, 7))
def test_a_message_list_that_is_not_a_list_maps_to_nothing(messages: Any) -> None:
    assert _hooks._to_sdk_messages(messages) == []


def test_non_dict_entries_and_system_messages_are_skipped() -> None:
    out = _hooks._to_sdk_messages(
        [
            "a raw string",
            7,
            None,
            {"role": "system", "content": "you are helpful"},
            {"role": "user", "content": "hi"},
            {"role": "developer", "content": "dropped as unknown"},
        ]
    )
    assert len(out) == 1
    assert out[0].role == MessageRole.USER


# --- the assistant response on post_api_request ---


def test_no_assistant_message_maps_to_no_output() -> None:
    assert _hooks._assistant_to_sdk_messages(None) == []


def test_an_object_shaped_assistant_message_is_read() -> None:
    out = _hooks._assistant_to_sdk_messages(_AssistantObject("done"))
    assert len(out) == 1
    assert out[0].role == MessageRole.ASSISTANT


def test_an_object_shaped_assistant_message_keeps_its_tool_calls() -> None:
    out = _hooks._assistant_to_sdk_messages(
        _AssistantObject(None, [_ToolCall("c1", _Function("bash", '{"command": "ls"}'))])
    )
    assert len(out) == 1
    assert len(out[0].parts) == 1


def test_an_assistant_message_of_an_unreadable_type_maps_to_no_output() -> None:
    """``getattr`` finds nothing, so the message has no content and no calls."""
    out = _hooks._assistant_to_sdk_messages(object())
    assert len(out) == 1
    assert out[0].parts == []


# --- token usage ---


@pytest.mark.parametrize(
    ("usage", "expected_input", "expected_output"),
    (
        ({"input_tokens": 10, "output_tokens": 3}, 10, 3),
        # OpenAI names them differently.
        ({"prompt_tokens": 10, "completion_tokens": 3}, 10, 3),
        ({}, 0, 0),
        # A count reported as text must not abort the close.
        ({"input_tokens": "ten"}, 0, 0),
        (None, 0, 0),
        ("not a dict", 0, 0),
    ),
)
def test_token_usage_reads_every_provider_spelling(usage: Any, expected_input: int, expected_output: int) -> None:
    built = _hooks._build_token_usage(usage)
    assert built.input_tokens == expected_input
    assert built.output_tokens == expected_output


@pytest.mark.parametrize(
    ("key", "attribute"),
    (
        ("cache_read_tokens", "cache_read_input_tokens"),
        ("cache_read_input_tokens", "cache_read_input_tokens"),
        ("cache_write_tokens", "cache_write_input_tokens"),
        ("cache_creation_input_tokens", "cache_write_input_tokens"),
        ("cache_write_input_tokens", "cache_write_input_tokens"),
        ("reasoning_tokens", "reasoning_tokens"),
        ("total_tokens", "total_tokens"),
    ),
)
def test_cache_and_reasoning_counts_map_to_one_field_each(key: str, attribute: str) -> None:
    assert getattr(_hooks._build_token_usage({key: 7}), attribute) == 7


# --- system prompt splitting ---


@pytest.mark.parametrize("messages", (None, "a string", 7))
def test_splitting_a_non_list_yields_nothing(messages: Any) -> None:
    assert _hooks._split_system_prompt(messages) == ("", [])


def test_multiple_system_messages_join_into_one_prompt() -> None:
    prompt, rest = _hooks._split_system_prompt(
        [
            {"role": "system", "content": "first"},
            "not a dict",
            {"role": "system", "content": ""},
            {"role": "system", "content": "second"},
            {"role": "user", "content": "hi"},
        ]
    )
    assert prompt == "first\n\nsecond"
    assert rest == [{"role": "user", "content": "hi"}]


# --- span context lookup ---


def test_a_recorder_without_a_span_has_no_context() -> None:
    assert _hooks._span_context_of(object()) is None


def test_a_span_that_cannot_answer_loses_the_link_and_not_the_generation() -> None:
    class Hostile:
        @property
        def span(self) -> Any:
            raise RuntimeError("no span for you")

    assert _hooks._span_context_of(Hostile()) is None


# --- json round trips used by the legacy convo path ---


def test_serialized_arguments_survive_a_round_trip() -> None:
    """The legacy path re-encodes tool arguments before storing them."""
    calls = _hooks._serialize_tool_calls([{"id": "c1", "function": {"name": "bash", "arguments": '{"a": 1}'}}])
    assert json.dumps(calls[0]["arguments"]) == '{"a": 1}'
