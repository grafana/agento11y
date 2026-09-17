from __future__ import annotations

import logging
from typing import Any

import pytest

from grafana_agento11y_hermes import _client, _config, _hooks, _state
from tests.conftest import FakeClient, FakeRecorder

# --- sampling ---


@pytest.mark.parametrize(
    ("sample_rate", "roll", "expected"),
    (
        # At or above 1.0 nothing is rolled at all.
        (1.0, 0.99, True),
        (2.0, 0.99, True),
        # At or below zero everything is dropped, also without a roll.
        (0.0, 0.0, False),
        (-1.0, 0.0, False),
        (0.5, 0.49, True),
        (0.5, 0.5, False),
        (0.5, 0.51, False),
    ),
)
def test_the_sample_rate_decides_what_is_recorded(
    monkeypatch: pytest.MonkeyPatch, sample_rate: float, roll: float, expected: bool
) -> None:
    monkeypatch.setattr(_client, "_CONFIG", _config.PluginConfig(sample_rate=sample_rate))
    monkeypatch.setattr(_hooks.random, "random", lambda: roll)
    assert _hooks._should_sample() is expected


def test_sampling_defaults_to_on_before_the_client_exists() -> None:
    assert _hooks._should_sample() is True


def test_a_sampled_out_request_records_nothing_and_closes_cleanly(
    monkeypatch: pytest.MonkeyPatch, patch_client: FakeClient
) -> None:
    monkeypatch.setattr(_client, "_CONFIG", _config.PluginConfig(sample_rate=0.0))

    _hooks.on_pre_api_request(session_id="s1", api_request_id="r1", model="m", provider="p")
    _hooks.on_post_tool_call(session_id="s1", api_request_id="r1", tool_name="bash")
    _hooks.on_post_api_request(session_id="s1", api_request_id="r1", assistant_message={"content": "hi"})
    _hooks.on_session_end(session_id="s1")

    assert patch_client.start_generation_calls == []
    assert patch_client.start_tool_execution_calls == []


def test_the_request_capture_is_read_even_when_the_request_is_sampled_out(
    monkeypatch: pytest.MonkeyPatch, patch_client: FakeClient
) -> None:
    """The readable payloads are the earliest of a session, so the gate is after the read."""
    monkeypatch.setattr(_client, "_CONFIG", _config.PluginConfig(sample_rate=0.0))

    _hooks.on_pre_api_request(
        session_id="s1",
        api_request_id="r1",
        model="m",
        provider="p",
        request={"body": {"system": "be brief", "max_tokens": 900}},
    )

    entry = _state.session_facts_get("s1")
    assert entry is not None
    assert entry[1].system_prompt == "be brief"
    assert entry[1].max_tokens == 900


# --- the error hook ---


def test_an_error_without_a_request_id_is_ignored(patch_client: FakeClient) -> None:
    """There is nothing to close, and the legacy path has no id to match on."""
    _hooks.on_api_request_error(error={"type": "RateLimit"}, status_code=429)
    assert patch_client.flush_calls == 0


def test_an_error_for_an_unknown_request_closes_nothing(patch_client: FakeClient) -> None:
    _hooks.on_api_request_error(api_request_id="never-opened", error="boom")
    assert patch_client.start_generation_calls == []


# --- linking a tool span to its generation ---


def test_a_span_that_refuses_the_attribute_still_leaves_a_tool_execution(
    caplog: pytest.LogCaptureFixture,
) -> None:
    class HostileSpan:
        def set_attribute(self, key: str, value: Any) -> None:
            raise RuntimeError("attribute rejected")

    recorder = FakeRecorder()
    recorder.span = HostileSpan()  # ty: ignore[invalid-assignment]
    link = _state.GenLink(generation_id="gen_1", span_context=None, session_id="s1", turn_id="t1")

    with caplog.at_level(logging.DEBUG):
        _hooks._stamp_parent_generation(recorder, link)

    assert [r for r in caplog.records if "parent generation attribute" in r.getMessage()]


def test_a_recorder_without_a_span_is_left_alone() -> None:
    """``NoopToolExecutionRecorder``, which an empty tool name produces, has none."""
    link = _state.GenLink(generation_id="gen_1", span_context=None, session_id="s1", turn_id="t1")
    _hooks._stamp_parent_generation(object(), link)


def test_no_link_means_no_attribute() -> None:
    recorder = FakeRecorder()
    _hooks._stamp_parent_generation(recorder, None)
    assert recorder.span.attributes == {}


def test_a_tool_without_a_usable_parent_is_still_recorded(patch_client: FakeClient) -> None:
    """No generation to hang it off, so it becomes its own root span."""
    _hooks.on_post_tool_call(session_id="s1", api_request_id="never-opened", tool_name="bash", tool_call_id="c1")
    assert len(patch_client.start_tool_execution_calls) == 1


# --- LEGACY: the running conversation, for hermes with no api_request_id ---


def test_arguments_that_will_not_serialize_are_recorded_as_empty(patch_client: FakeClient) -> None:
    # convo_append only extends a bucket pre_llm_call opened, which is the hook
    # order on the hermes releases that need this path.
    _hooks.on_pre_llm_call(session_id="s1", conversation_history=[])

    _hooks.on_post_tool_call(session_id="s1", tool_name="bash", tool_call_id="c1", args={1, 2}, result="ok")

    convo = _state.convo_get(("", "s1"))
    assert convo[0]["tool_calls"][0]["function"]["arguments"] == "{}"


def test_a_result_that_will_not_serialize_falls_back_to_its_repr(patch_client: FakeClient) -> None:
    circular: list[Any] = []
    circular.append(circular)
    _hooks.on_pre_llm_call(session_id="s1", conversation_history=[])

    _hooks.on_post_tool_call(session_id="s1", tool_name="bash", tool_call_id="c1", args={}, result=circular)

    convo = _state.convo_get(("", "s1"))
    assert convo[1]["content"] == repr(circular)


def test_the_convo_bookkeeping_stops_once_a_request_id_has_been_seen(patch_client: FakeClient) -> None:
    _hooks.on_pre_api_request(session_id="s1", api_request_id="r1", model="m", provider="p")
    _hooks.on_post_tool_call(session_id="s1", api_request_id="r1", tool_name="bash", tool_call_id="c1")

    assert _state.convo_get(("", "s1")) == [], "current hermes carries its own messages"


# --- state keys the layer refuses ---


def test_an_empty_session_or_request_id_is_never_stored() -> None:
    """Empty keys would collide across sessions, so they are dropped at the door."""
    _state.gen_link_put("", _state.GenLink(generation_id="g", span_context=None, session_id="s", turn_id="t"))
    _state.session_model_put("", "model", "provider")
    _state.session_facts_put("", "model", _hooks._request.RequestFacts())

    assert _state.gen_link_get("") is None
    assert _state.session_model_get("") == ("", "")
    assert _state.session_facts_get("") is None
