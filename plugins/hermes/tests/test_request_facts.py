"""Reading the provider request that ``pre_api_request`` carries.

The bodies here are the literal provider payloads hermes builds per
``api_mode``, and the degraded ones are what its payload sanitizer leaves
behind once ``HERMES_PLUGIN_PAYLOAD_MAX_CHARS`` is crossed.
"""

from __future__ import annotations

import json
import logging
from typing import Any

import pytest

from grafana_agento11y_hermes import _request

ANTHROPIC_TOOL = {"name": "read_file", "description": "d", "input_schema": {"type": "object"}}
OPENAI_TOOL = {"type": "function", "function": {"name": "read_file", "parameters": {"type": "object"}}}
RESPONSES_TOOL = {"type": "function", "name": "read_file", "parameters": {"type": "object"}}


@pytest.mark.parametrize(
    ("body", "kwargs", "expected"),
    [
        # anthropic_messages, plain string.
        ({"system": "be helpful"}, {}, "be helpful"),
        # anthropic_messages under cache_control or OAuth: a content-block list.
        (
            {"system": [{"type": "text", "text": "be helpful", "cache_control": {"type": "ephemeral"}}]},
            {},
            "be helpful",
        ),
        # bedrock_converse: blocks with no ``type`` key.
        ({"system": [{"text": "be helpful"}]}, {}, "be helpful"),
        # codex_responses.
        ({"instructions": "be helpful"}, {}, "be helpful"),
        # chat_completions keeps it in the message list, not on the body.
        ({}, {"request_messages": [{"role": "system", "content": "be helpful"}]}, "be helpful"),
        # GPT-5 and Codex models take it as ``developer``.
        ({}, {"request_messages": [{"role": "developer", "content": "be helpful"}]}, "be helpful"),
        # A leading user message is not a system prompt.
        ({}, {"request_messages": [{"role": "user", "content": "hi"}]}, ""),
        # The post-0.20.1 kwarg is unclipped, so it beats the sanitized body.
        (
            {"system": "clipped...[truncated 900 chars]"},
            {"system_prompt": "full text"},
            "full text",
        ),
        ({}, {}, ""),
    ],
)
def test_system_prompt_is_read_from_every_api_mode_shape(body: dict, kwargs: dict[str, Any], expected: str) -> None:
    facts = _request.parse({"method": "POST", "body": body}, **kwargs)

    assert facts.system_prompt == expected
    assert not facts.truncated


@pytest.mark.parametrize(
    ("tools", "expected_names"),
    [
        ([ANTHROPIC_TOOL], ["read_file"]),
        ([OPENAI_TOOL], ["read_file"]),
        ([RESPONSES_TOOL], ["read_file"]),
        ([ANTHROPIC_TOOL, RESPONSES_TOOL], ["read_file", "read_file"]),
        # The sentinel a clipped list ends with has no name to attribute a call
        # to, so the SDK mapper drops it.
        ([ANTHROPIC_TOOL, {"_truncated_items": 3}], ["read_file"]),
        ([], []),
        (None, []),
    ],
)
def test_tool_definitions_cover_the_three_schema_shapes(tools: Any, expected_names: list[str]) -> None:
    facts = _request.parse({"method": "POST", "body": {"tools": tools}})

    assert [tool.name for tool in facts.tools] == expected_names


@pytest.mark.parametrize("tool", [ANTHROPIC_TOOL, OPENAI_TOOL, RESPONSES_TOOL])
def test_the_input_schema_survives_the_mapping(tool: dict) -> None:
    facts = _request.parse({"method": "POST", "body": {"tools": [tool]}})

    assert json.loads(facts.tools[0].input_schema_json) == {"type": "object"}


@pytest.mark.parametrize(
    "request_payload",
    [
        # The whole envelope replaced once the payload is still over the cap.
        {"_truncated": True, "original_type": "dict", "preview": "{'model': 'claude"},
        # No ``request`` kwarg at all.
        None,
        # A body that is not the provider mapping.
        {"method": "POST", "body": "clipped...[truncated 40000 chars]"},
        {"method": "POST"},
    ],
)
def test_a_degraded_payload_reports_itself_rather_than_raising(request_payload: Any) -> None:
    facts = _request.parse(request_payload)

    assert facts.truncated
    assert facts.system_prompt == ""
    assert facts.tools == []
    assert facts.max_tokens is None


def test_the_unsanitized_kwargs_still_read_through_a_collapsed_envelope() -> None:
    """Hermes clips the body but passes these two as it built them."""
    facts = _request.parse(
        {"_truncated": True, "preview": "..."},
        request_messages=[{"role": "system", "content": "be helpful"}],
    )

    assert facts.system_prompt == "be helpful"
    assert facts.truncated, "the tool schemas are still lost"


@pytest.mark.parametrize(
    ("body", "expected"),
    [
        # anthropic_messages.
        ({"max_tokens": 8192, "temperature": 0.7, "top_p": 0.95}, (8192, 0.7, 0.95)),
        ({"max_tokens": "8192", "temperature": "0.7"}, (8192, 0.7, None)),
        # The OpenAI routes that reject ``max_tokens``: direct OpenAI, Azure,
        # GitHub Copilot, and every gpt-4o / gpt-4.1 / gpt-5 / o-series model.
        ({"max_completion_tokens": 8192}, (8192, None, None)),
        # codex_responses.
        ({"max_output_tokens": 4096}, (4096, None, None)),
        # bedrock_converse nests all three.
        ({"inferenceConfig": {"maxTokens": 4096, "temperature": 0.2, "topP": 0.9}}, (4096, 0.2, 0.9)),
        # Zero is a setting, not an absence.
        ({"temperature": 0.0}, (None, 0.0, None)),
        ({"inferenceConfig": {"temperature": 0.0}}, (None, 0.0, None)),
        # A cap is the exception: zero would cap the response at nothing, and
        # hermes's own reader skips it rather than reporting it.
        ({"max_tokens": 0}, (None, None, None)),
        ({"max_tokens": -1}, (None, None, None)),
        # An unusable cap falls through to the next name, as it does in hermes.
        ({"max_tokens": 0, "max_completion_tokens": 8192}, (8192, None, None)),
        ({"max_tokens": "warm", "max_output_tokens": 4096}, (4096, None, None)),
        ({}, (None, None, None)),
        ({"max_tokens": None, "temperature": "warm", "top_p": []}, (None, None, None)),
    ],
)
def test_sampling_params_are_read_under_every_route_name(body: dict, expected: tuple) -> None:
    facts = _request.parse({"method": "POST", "body": body})

    assert (facts.max_tokens, facts.temperature, facts.top_p) == expected


def test_bedrock_tools_are_read_through_the_toolspec_envelope() -> None:
    """Converse wraps the Anthropic shape twice; unwrapped, the mapper reads it."""
    body = {
        "toolConfig": {
            "tools": [
                {
                    "toolSpec": {
                        "name": "read_file",
                        "description": "read a file",
                        "inputSchema": {"json": {"type": "object"}},
                    }
                },
                {"_truncated_items": 3},
            ]
        }
    }

    facts = _request.parse({"method": "POST", "body": body})

    assert [tool.name for tool in facts.tools] == ["read_file"]
    assert json.loads(facts.tools[0].input_schema_json) == {"type": "object"}
    assert facts.tools_clipped, "the sentinel is read before the mapper drops it"


@pytest.mark.parametrize(
    ("body", "kwargs", "clipped_prompt", "clipped_tools"),
    [
        # Pass 1 and pass 2 both leave a readable value with a marker on it.
        ({"system": "be helpful...[truncated 11000 chars]"}, {}, True, False),
        ({"system": [{"type": "text", "text": "be...[truncated 4 chars]"}]}, {}, True, False),
        ({"tools": [ANTHROPIC_TOOL, {"_truncated_items": 30}]}, {}, False, True),
        # A marker anywhere under the tool list counts: a clipped description
        # or schema is a tool definition we would export wrong.
        ({"tools": [{"name": "read_file", "description": "d...[truncated 900 chars]"}]}, {}, False, True),
        ({"system": "be helpful", "tools": [ANTHROPIC_TOOL]}, {}, False, False),
        # The unsanitized kwargs never carry a marker.
        ({}, {"request_messages": [{"role": "system", "content": "be helpful"}]}, False, False),
    ],
)
def test_a_field_hermes_shortened_in_place_is_flagged(
    body: dict, kwargs: dict[str, Any], clipped_prompt: bool, clipped_tools: bool
) -> None:
    """The three sanitizer passes are not one state.

    Only the third loses the body. The first two leave a value that reads as
    present, which is why a clipped field has to announce itself.
    """
    facts = _request.parse({"method": "POST", "body": body}, **kwargs)

    assert facts.system_prompt_clipped is clipped_prompt
    assert facts.tools_clipped is clipped_tools
    assert not facts.truncated, "the envelope survived; only a field was shortened"


def test_a_complete_prompt_wins_over_a_clipped_one_ahead_of_it() -> None:
    """Preference order yields to fidelity.

    ``request_messages`` skips the sanitizer, so where both hold the prompt the
    unclipped copy is the one to export.
    """
    facts = _request.parse(
        {"method": "POST", "body": {"system": "be help...[truncated 900 chars]"}},
        request_messages=[{"role": "system", "content": "be helpful, at length"}],
    )

    assert facts.system_prompt == "be helpful, at length"
    assert not facts.system_prompt_clipped


@pytest.mark.parametrize(
    ("tool_choice", "expected"),
    [
        ({"type": "auto"}, "auto"),
        # Which tool was forced is the content of the choice, so it is kept.
        ({"type": "tool", "name": "read_file"}, "tool:read_file"),
        ({"type": "function", "function": {"name": "read_file"}}, "function:read_file"),
        ("required", "required"),
        ({}, None),
        (None, None),
        (7, None),
    ],
)
def test_tool_choice_is_coerced_to_the_string_the_seed_takes(tool_choice: Any, expected: str | None) -> None:
    facts = _request.parse({"method": "POST", "body": {"tool_choice": tool_choice}})

    assert facts.tool_choice == expected


def test_an_unreadable_tool_list_does_not_cost_the_rest_of_the_body(caplog: pytest.LogCaptureFixture) -> None:
    """Fail open, and fail narrow: the tool mapping is the fragile read.

    It runs through a private SDK path, so it gets its own guard rather than
    taking the sampling params down with it.
    """

    class Hostile:
        def __iter__(self) -> Any:
            raise RuntimeError("boom")

    with caplog.at_level(logging.DEBUG, logger="grafana_agento11y_hermes._request"):
        facts = _request.parse(
            {"method": "POST", "body": {"system": "be helpful", "max_tokens": 8192, "tools": Hostile()}}
        )

    assert "could not read the request tools" in caplog.text
    assert facts.tools == []
    assert facts.system_prompt == "be helpful"
    assert facts.max_tokens == 8192, "a broken tool mapping must not cost the fields beside it"


def test_an_unreadable_body_never_reaches_the_hook(caplog: pytest.LogCaptureFixture) -> None:
    """``parse`` never raises, whatever hermes reshaped the body into."""

    class HostileBody(dict):
        def get(self, *_: Any, **__: Any) -> Any:
            raise RuntimeError("body refused to answer")

    with caplog.at_level(logging.DEBUG, logger="grafana_agento11y_hermes._request"):
        facts = _request.parse({"method": "POST", "body": HostileBody(system="be helpful")})

    assert "could not read the request body" in caplog.text
    assert facts.system_prompt == ""
    assert facts.max_tokens is None


def test_an_unreadable_envelope_never_reaches_the_hook(caplog: pytest.LogCaptureFixture) -> None:
    """The read that picks the body out is guarded too, being upstream of the rest."""

    class HostileRequest(dict):
        def get(self, *_: Any, **__: Any) -> Any:
            raise RuntimeError("envelope refused to answer")

    with caplog.at_level(logging.DEBUG, logger="grafana_agento11y_hermes._request"):
        facts = _request.parse(HostileRequest(body={"system": "be helpful"}))

    assert "could not read the request envelope" in caplog.text
    assert facts.truncated is True
    assert facts.tools == []


def test_the_clip_marker_scan_stops_at_its_depth_limit() -> None:
    """A tool schema can nest arbitrarily; the scan for a clip marker cannot."""
    marker = {_request._CLIP_SENTINEL: 5}
    within: Any = marker
    for _ in range(_request._MAX_SCAN_DEPTH):
        within = {"properties": within}
    assert _request._carries_clip_marker(within) is True

    beyond = {"properties": within}
    assert _request._carries_clip_marker(beyond) is False, "past the limit it reports nothing rather than recursing"
