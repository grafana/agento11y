"""Request-scoped generation pairing, the path hermes v2026.6.5+ (PyPI 0.16.0) takes.

These tests use the kwarg names hermes actually sends, captured from the
``pre_api_request`` and ``post_api_request`` call sites in
``agent/conversation_loop.py`` (``:1357`` and ``:4486`` in hermes 0.19.0). The
older tests in ``test_hooks.py`` omit ``api_request_id`` and so exercise the
legacy fallback instead.
"""

from __future__ import annotations

import logging
import threading
import time
from typing import Any

import pytest
from opentelemetry import trace as otel_trace

from grafana_agento11y_hermes import _client, _errors, _hooks, _state

# No system message: hermes prepends the system prompt to the list that goes on
# the wire, not to the running conversation it hands the hooks.
CONVO = [
    {"role": "user", "content": "what is 2+2?"},
]

# The anthropic_messages body, which is where the system prompt and the tool
# schemas actually reach the plugin.
REQUEST = {
    "method": "POST",
    "body": {
        "model": "claude-sonnet-4-6",
        "max_tokens": 8192,
        "temperature": 0.7,
        "system": [{"type": "text", "text": "be helpful"}],
        "messages": [{"role": "user", "content": "what is 2+2?"}],
        "tools": [
            {"name": "read_file", "description": "read a file", "input_schema": {"type": "object"}},
            {"name": "shell", "description": "run a command", "input_schema": {"type": "object"}},
        ],
        "tool_choice": {"type": "auto"},
    },
}

# What the host's payload sanitizer leaves once the envelope is over its cap.
CLIPPED_REQUEST = {"_truncated": True, "original_type": "dict", "preview": "{'model': 'claude"}

# The pass before that one: the body still reads, but the prompt is cut short
# and the tool list is one entry plus the count of what it dropped.
CLIPPED_FIELDS_REQUEST = {
    "method": "POST",
    "body": {
        "model": "claude-sonnet-4-6",
        "max_tokens": 8192,
        "system": [{"type": "text", "text": "be help...[truncated 900 chars]"}],
        "tools": [
            {"name": "read_file", "description": "read a file", "input_schema": {"type": "object"}},
            {"_truncated_items": 1},
        ],
    },
}


def texts(messages: Any) -> list[str]:
    """Text of each SDK ``Message``, which stores content as typed parts."""
    return ["".join(p.text for p in m.parts if p.text) for m in messages]


def attributes(span: Any) -> dict[str, Any]:
    """Span attributes as a plain dict. The OTel SDK types them as optional."""
    return dict(span.attributes or {})


def _pre(client_unused: Any = None, **over: Any) -> None:
    kwargs: dict[str, Any] = {
        "task_id": "task-1",
        "turn_id": "turn-1",
        "api_request_id": "req-1",
        "session_id": "sess-1",
        "user_message": "what is 2+2?",
        "conversation_history": list(CONVO),
        "platform": "cli",
        "model": "claude-sonnet-4-6",
        "provider": "anthropic",
        "api_call_count": 1,
        "message_count": 1,
        "tool_count": 2,
        "request": REQUEST,
    }
    kwargs.update(over)
    _hooks.on_pre_api_request(**kwargs)


def _post(**over: Any) -> None:
    kwargs: dict[str, Any] = {
        "task_id": "task-1",
        "turn_id": "turn-1",
        "api_request_id": "req-1",
        "session_id": "sess-1",
        "model": "claude-sonnet-4-6",
        "provider": "anthropic",
        "api_call_count": 1,
        "api_duration": 1.5,
        "finish_reason": "stop",
        "response_model": "claude-sonnet-4-6",
        "usage": {"input_tokens": 10, "output_tokens": 4},
        "assistant_message": {"role": "assistant", "content": "4"},
        "assistant_content_chars": 1,
        "assistant_tool_call_count": 0,
    }
    kwargs.update(over)
    _hooks.on_post_api_request(**kwargs)


def test_generation_closes_in_post_api_request(patch_client: Any, env_creds: None) -> None:
    """No post_llm_call needed: the pair is exact and the output is in hand."""
    _pre()
    rec = patch_client._next_gen_recorder
    assert rec is not None and rec.entered
    assert not rec.exited

    _post()

    assert rec.exited
    assert len(rec.set_result_calls) == 1
    call = rec.set_result_calls[0]
    assert call["stop_reason"] == "stop"
    assert call["response_model"] == "claude-sonnet-4-6"


def test_output_comes_from_assistant_message(patch_client: Any, env_creds: None) -> None:
    _pre()
    _post(assistant_message={"role": "assistant", "content": "the answer is 4"})

    call = patch_client._next_gen_recorder.set_result_calls[0]
    assert texts(call["output"]) == ["the answer is 4"]
    assert call["input"], "input seeded at pre-time must survive the close"


def test_completed_at_reflects_api_duration(patch_client: Any, env_creds: None) -> None:
    """Span covers the LLM call, not the recorder lifetime."""
    _pre()
    _post(api_duration=2.0)

    call = patch_client._next_gen_recorder.set_result_calls[0]
    delta = call["completed_at"] - call["started_at"]
    assert delta.total_seconds() == pytest.approx(2.0)


def test_concurrent_requests_in_one_session_do_not_collide(patch_client: Any, env_creds: None) -> None:
    """MoA fan-out and subagents interleave requests with the same task/session and api_call_count."""
    _pre(api_request_id="req-a", conversation_history=[{"role": "user", "content": "A"}])
    rec_a = patch_client._next_gen_recorder
    _pre(api_request_id="req-b", conversation_history=[{"role": "user", "content": "B"}])
    rec_b = patch_client._next_gen_recorder

    assert rec_a is not rec_b

    _post(api_request_id="req-b", assistant_message={"role": "assistant", "content": "B-out"})
    _post(api_request_id="req-a", assistant_message={"role": "assistant", "content": "A-out"})

    assert texts(rec_a.set_result_calls[0]["output"]) == ["A-out"]
    assert texts(rec_b.set_result_calls[0]["output"]) == ["B-out"]


def test_unmatched_post_is_ignored(patch_client: Any, env_creds: None) -> None:
    _pre(api_request_id="req-1")
    _post(api_request_id="req-does-not-exist")

    assert not patch_client._next_gen_recorder.exited


def test_session_end_closes_a_request_that_never_completed(patch_client: Any, env_creds: None) -> None:
    """Interrupt safety: input and timing still export, output is empty."""
    _pre()
    rec = patch_client._next_gen_recorder

    _hooks.on_session_end(session_id="sess-1")

    assert rec.exited
    assert rec.set_result_calls[0]["output"] == []
    assert rec.set_result_calls[0]["input"]
    assert _state.req_pop("req-1") is None


def test_session_end_drops_the_link_state_it_owns(patch_client: Any, env_creds: None) -> None:
    """Nothing of this session can fire after it ends, so nothing should be kept."""
    _pre()
    _post()
    assert _state.gen_link_get("req-1") is not None
    assert _state.turn_last_gen_get("turn-1")

    _hooks.on_session_end(session_id="sess-1")

    assert _state.gen_link_get("req-1") is None
    assert _state.turn_last_gen_get("turn-1") == ""


def test_session_end_leaves_another_sessions_links_alone(patch_client: Any, env_creds: None) -> None:
    _pre(api_request_id="req-mine", session_id="sess-1", turn_id="turn-mine")
    _post(api_request_id="req-mine", session_id="sess-1", turn_id="turn-mine")
    _pre(api_request_id="req-other", session_id="sess-2", turn_id="turn-other")
    _post(api_request_id="req-other", session_id="sess-2", turn_id="turn-other")

    _hooks.on_session_end(session_id="sess-1")

    assert _state.gen_link_get("req-other") is not None
    assert _state.turn_last_gen_get("turn-other")


def test_session_end_only_drains_its_own_session(patch_client: Any, env_creds: None) -> None:
    _pre(api_request_id="req-mine", session_id="sess-1")
    mine = patch_client._next_gen_recorder
    _pre(api_request_id="req-other", session_id="sess-2")
    other = patch_client._next_gen_recorder

    _hooks.on_session_end(session_id="sess-1")

    assert mine.exited
    assert not other.exited


def test_legacy_hermes_warns_once(patch_client: Any, env_creds: None, caplog: pytest.LogCaptureFixture) -> None:
    """No api_request_id means an old hermes, and the user should hear so."""
    with caplog.at_level(logging.WARNING):
        _pre(api_request_id="", conversation_history=None, api_call_count=1)
        _pre(api_request_id="", conversation_history=None, api_call_count=2)

    warnings = [r for r in caplog.records if "api_request_id" in r.getMessage()]
    assert len(warnings) == 1
    assert "v2026.6.5" in warnings[0].getMessage()


def test_a_retry_reusing_the_request_id_closes_the_displaced_recorder(
    patch_client: Any,
    env_creds: None,
) -> None:
    """Hermes assigns api_request_id above its retry loop, so ids repeat."""
    _pre()
    first = patch_client._next_gen_recorder
    _pre()
    second = patch_client._next_gen_recorder

    assert first is not second
    assert first.exited, "the abandoned attempt must not outlive the request"
    assert not second.exited

    _post(assistant_message={"role": "assistant", "content": "kept"})

    assert texts(second.set_result_calls[0]["output"]) == ["kept"]


def test_a_displaced_attempt_is_marked_rather_than_exported_as_a_success(
    patch_client: Any,
    env_creds: None,
) -> None:
    _pre()
    first = patch_client._next_gen_recorder
    _pre()

    assert isinstance(first.set_call_error_calls[0], _errors.SupersededAttempt)
    assert first.set_result_calls[0]["output"] == []
    assert first.set_result_calls[0]["input"], "the attempt's input still exports"


def test_api_request_error_closes_the_attempt_once(patch_client: Any, env_creds: None) -> None:
    _pre()
    rec = patch_client._next_gen_recorder

    _hooks.on_api_request_error(
        api_request_id="req-1",
        error={"type": "RateLimitError", "message": "slow down"},
        status_code=429,
    )

    assert rec.exited
    assert len(rec.set_result_calls) == 1
    assert rec.set_result_calls[0]["call_error"] == "slow down"
    error = rec.set_call_error_calls[0]
    assert isinstance(error, _errors.ProviderCallError)
    assert error.status_code == 429


def test_a_retry_after_the_error_hook_displaces_nothing(patch_client: Any, env_creds: None) -> None:
    _pre()
    failed = patch_client._next_gen_recorder
    _hooks.on_api_request_error(api_request_id="req-1", error="boom", status_code=500)

    _pre()
    retry = patch_client._next_gen_recorder

    assert len(failed.set_call_error_calls) == 1, "closed once, not once per mechanism"
    assert not retry.exited
    assert retry.set_call_error_calls == []


def test_the_status_code_is_read_off_the_error_payload_too(patch_client: Any, env_creds: None) -> None:
    _pre()

    _hooks.on_api_request_error(api_request_id="req-1", error={"message": "slow down", "status_code": 429})

    assert patch_client._next_gen_recorder.set_call_error_calls[0].status_code == 429


def test_api_request_error_for_an_unknown_id_is_a_no_op(patch_client: Any, env_creds: None) -> None:
    _pre()

    _hooks.on_api_request_error(api_request_id="req-other", error="boom")

    assert not patch_client._next_gen_recorder.exited


def test_an_unreadable_error_payload_neither_raises_nor_orphans_the_recorder(
    patch_client: Any,
    env_creds: None,
) -> None:
    """``error`` is whatever hermes built, and reading it must not escape."""

    class Unreadable:
        def __str__(self) -> str:
            raise RuntimeError("unreadable")

    _pre()
    rec = patch_client._next_gen_recorder

    _hooks.on_api_request_error(api_request_id="req-1", error=Unreadable())

    assert not rec.exited, "the state stays in the map for a later sweep"
    _hooks.on_session_end(session_id="sess-1")
    assert rec.exited


def test_an_oversized_error_message_is_truncated(patch_client: Any, env_creds: None) -> None:
    _pre()

    _hooks.on_api_request_error(api_request_id="req-1", error={"message": "x" * 5000})

    message = patch_client._next_gen_recorder.set_result_calls[0]["call_error"]
    assert message.startswith("x" * 2000)
    assert "truncated 3000 chars" in message


def test_the_sdk_derives_the_category_from_our_sentinel() -> None:
    """Dashboards group failures by ``error.category``, which the SDK reads from the exception."""
    from agento11y.client import _error_category_from_exception

    cases = {429: "rate_limit", 401: "auth_error", 403: "auth_error", 503: "server_error"}
    for status_code, expected in cases.items():
        error = _errors.ProviderCallError("api_request_error", status_code)
        assert _error_category_from_exception(error, fallback_sdk=True) == expected


def test_a_non_numeric_token_count_does_not_discard_the_generation(
    patch_client: Any,
    env_creds: None,
) -> None:
    """One unusable token value used to abort the close before set_result."""
    _pre()
    _post(usage={"input_tokens": "lots"}, assistant_message={"role": "assistant", "content": "4"})

    call = patch_client._next_gen_recorder.set_result_calls[0]
    assert call["input"]
    assert texts(call["output"]) == ["4"]
    assert call["response_model"] == "claude-sonnet-4-6"
    usage = call["usage"]
    assert usage.input_tokens == 0
    assert usage.output_tokens == 0
    assert usage.total_tokens == 0


@pytest.mark.parametrize(
    ("sent", "body", "recorded"),
    [
        # The body is what hermes put on the wire, and the kwarg beside it
        # arrives as None on every supported release.
        (None, {"max_tokens": 8192}, 8192),
        ("8192", {"max_tokens": 4096}, 4096),
        # Nothing readable on the body: the kwarg is all there is.
        (4096, None, 4096),
        (None, None, None),
        ("8192", None, 8192),
        ("none", None, None),
    ],
)
def test_max_tokens_is_recorded(
    patch_client: Any, env_creds: None, sent: Any, body: dict | None, recorded: int | None
) -> None:
    _pre(max_tokens=sent, request={"method": "POST", "body": body} if body else None)

    assert patch_client.start_generation_calls[0].max_tokens == recorded


# --- what the request payload puts on the generation ---


def test_the_seed_carries_the_tools_and_sampling_params(patch_client: Any, env_creds: None) -> None:
    _pre()

    start = patch_client.start_generation_calls[0]
    assert [tool.name for tool in start.tools] == ["read_file", "shell"]
    assert start.system_prompt == "be helpful"
    assert start.max_tokens == 8192
    assert start.temperature == 0.7
    assert start.tool_choice == "auto"
    assert start.metadata["hermes.tool_count"] == 2


@pytest.mark.parametrize(
    ("over", "expected"),
    [
        # anthropic_messages puts it on the body.
        ({}, "be helpful"),
        # chat_completions puts no ``system`` on the body at all, so the
        # message list is the only copy.
        (
            {
                "request": {"method": "POST", "body": {"model": "gpt-5"}},
                "request_messages": [{"role": "system", "content": "from the messages"}],
            },
            "from the messages",
        ),
        # Hermes past 0.20.1 passes the unclipped text as its own kwarg, which
        # beats the body the sanitizer may have clipped.
        ({"system_prompt": "from the kwarg"}, "from the kwarg"),
    ],
)
def test_the_seed_system_prompt_follows_the_api_mode(
    patch_client: Any, env_creds: None, over: dict[str, Any], expected: str
) -> None:
    _pre(**over)

    assert patch_client.start_generation_calls[0].system_prompt == expected


def test_a_clipped_request_reuses_the_session_capture(patch_client: Any, env_creds: None) -> None:
    """The common case: hermes clips the payload as the conversation grows."""
    _pre()
    _pre(api_request_id="req-2", request=CLIPPED_REQUEST)

    start = patch_client.start_generation_calls[1]
    assert [tool.name for tool in start.tools] == ["read_file", "shell"]
    assert start.system_prompt == "be helpful"
    assert start.max_tokens == 8192


def test_a_model_switch_drops_the_carried_sampling_params(patch_client: Any, env_creds: None) -> None:
    """A cap and a temperature come from the model's own profile.

    The prompt and the toolset are the agent's, so a fallback to another
    provider keeps those and resolves its own sampling params.
    """
    _pre()
    _pre(api_request_id="req-2", model="claude-opus-4-1", request=CLIPPED_REQUEST)

    start = patch_client.start_generation_calls[1]
    assert start.max_tokens is None
    assert start.temperature is None
    assert start.system_prompt == "be helpful"
    assert [tool.name for tool in start.tools] == ["read_file", "shell"]


@pytest.mark.parametrize("fallback_body", [None, {"max_tokens": 32000}, {"temperature": 0.2}])
def test_a_one_turn_fallback_keeps_the_params_of_the_model_it_left(
    patch_client: Any, env_creds: None, fallback_body: dict[str, Any] | None
) -> None:
    """Hermes restores the primary runtime at the top of every turn.

    So the model a failure moved the session to holds it for one turn, and the
    request that comes back to the first model is deep enough in the session to
    arrive clipped. Retiring the params on the way out would empty every
    generation after that.
    """
    _pre()
    _pre(
        api_request_id="req-2",
        model="claude-opus-4-1",
        request={"body": fallback_body} if fallback_body is not None else CLIPPED_REQUEST,
    )
    _pre(api_request_id="req-3", request=CLIPPED_REQUEST)

    fallback = patch_client.start_generation_calls[1]
    assert fallback.max_tokens == (fallback_body or {}).get("max_tokens")
    assert fallback.temperature == (fallback_body or {}).get("temperature")
    start = patch_client.start_generation_calls[2]
    assert start.max_tokens == 8192
    assert start.temperature == 0.7

    _pre(api_request_id="req-4", model="claude-opus-4-1", request=CLIPPED_REQUEST)
    restored_fallback = patch_client.start_generation_calls[3]
    assert restored_fallback.max_tokens == fallback.max_tokens
    assert restored_fallback.temperature == fallback.temperature
    assert restored_fallback.system_prompt == "be helpful"
    assert [tool.name for tool in restored_fallback.tools] == ["read_file", "shell"]


def test_a_real_model_switch_takes_the_capture_over(patch_client: Any, env_creds: None) -> None:
    _pre()
    _pre(
        api_request_id="req-2",
        model="claude-opus-4-1",
        request={"method": "POST", "body": {"max_tokens": 32000}},
    )
    _pre(api_request_id="req-3", model="claude-opus-4-1", request=CLIPPED_REQUEST)

    assert patch_client.start_generation_calls[2].max_tokens == 32000


def test_same_model_partial_params_preserve_missing_fields(patch_client: Any, env_creds: None) -> None:
    _pre(request={"body": {"max_tokens": 8192, "temperature": 0.7, "top_p": 0.9, "tool_choice": "auto"}})
    _pre(api_request_id="req-2", request={"body": {"temperature": 0.0}})
    _pre(api_request_id="req-3", request=CLIPPED_REQUEST)

    for start in patch_client.start_generation_calls[1:]:
        assert start.max_tokens == 8192
        assert start.temperature == 0.0
        assert start.top_p == 0.9
        assert start.tool_choice == "auto"
        assert start.metadata["hermes.request_facts_reused"] is True


def test_a_body_of_only_sampling_params_still_carries_forward(patch_client: Any, env_creds: None) -> None:
    _pre(request={"method": "POST", "body": {"max_tokens": 64000, "temperature": 1}}, tool_count=0)
    _pre(api_request_id="req-2", request=CLIPPED_REQUEST, tool_count=0)

    start = patch_client.start_generation_calls[1]
    assert start.max_tokens == 64000
    assert start.temperature == 1.0


def test_a_capture_never_crosses_into_another_session(patch_client: Any, env_creds: None) -> None:
    _pre()
    _hooks.on_session_end(session_id="sess-1")

    _pre(api_request_id="req-2", session_id="sess-2", request=CLIPPED_REQUEST)

    start = patch_client.start_generation_calls[-1]
    assert start.tools == []
    assert start.system_prompt == ""


def test_a_capture_outlives_the_turn_that_made_it(patch_client: Any, env_creds: None) -> None:
    """``on_session_end`` fires once per user message, not once per session.

    Hermes calls ``run_conversation`` per turn and finalizes it at the end of
    each (``agent/turn_finalizer.py`` in 0.19.0). Turn 2's first request
    already carries the grown history and so arrives clipped, which is exactly
    when the capture has to still be there.
    """
    _pre()
    _hooks.on_session_end(session_id="sess-1")

    _pre(api_request_id="req-2", request=CLIPPED_REQUEST)

    start = patch_client.start_generation_calls[-1]
    assert [tool.name for tool in start.tools] == ["read_file", "shell"]
    assert start.system_prompt == "be helpful"


def test_a_clipped_field_does_not_overwrite_the_capture_it_borrows_from(patch_client: Any, env_creds: None) -> None:
    """The first two sanitizer passes leave a value that reads as present.

    A prompt cut to its first line and a tool list cut to one entry are worse
    than the complete copy already held, so neither is exported nor stored.
    """
    _pre()
    _pre(api_request_id="req-2", request=CLIPPED_FIELDS_REQUEST)
    _pre(api_request_id="req-3", request=CLIPPED_REQUEST)

    for start in patch_client.start_generation_calls[1:]:
        assert [tool.name for tool in start.tools] == ["read_file", "shell"]
        assert start.system_prompt == "be helpful"


def test_a_shorter_clip_does_not_replace_a_longer_one(patch_client: Any, env_creds: None) -> None:
    """Pass 2 cuts a string to 1000 chars where pass 1 cut it to 8000."""

    def clipped_to(kept: int) -> dict[str, Any]:
        return {"method": "POST", "body": {"system": "S" * kept + f"...[truncated {12000 - kept} chars]"}}

    _pre(request=clipped_to(8000))
    _pre(api_request_id="req-2", request=clipped_to(1000))

    assert patch_client.start_generation_calls[1].system_prompt.startswith("S" * 8000)


def test_the_record_says_when_a_field_came_from_an_earlier_request(patch_client: Any, env_creds: None) -> None:
    """A swapped toolset makes a reused field stale, so the record admits it."""
    _pre()
    _pre(api_request_id="req-2", request=CLIPPED_REQUEST)

    first, second = patch_client.start_generation_calls
    assert first.metadata["hermes.request_facts_reused"] is False
    assert second.metadata["hermes.request_facts_reused"] is True


def test_the_capture_survives_a_request_the_sampler_dropped(
    patch_client: Any, env_creds: None, monkeypatch: pytest.MonkeyPatch
) -> None:
    """Sampling must not eat the readable payloads and keep the clipped ones.

    Only the earliest requests of a session arrive complete, so gating the read
    on the sample rate leaves every sampled-in request with nothing behind it.
    """
    cfg = _client._get_plugin_config()
    assert cfg is not None
    monkeypatch.setattr(cfg, "sample_rate", 0.0)
    _pre()
    assert patch_client.start_generation_calls == [], "the generation itself is skipped"

    monkeypatch.setattr(cfg, "sample_rate", 1.0)
    _pre(api_request_id="req-2", request=CLIPPED_REQUEST)

    start = patch_client.start_generation_calls[0]
    assert [tool.name for tool in start.tools] == ["read_file", "shell"]
    assert start.system_prompt == "be helpful"


def test_the_capture_map_is_bounded(patch_client: Any, env_creds: None) -> None:
    """Nothing clears an entry, so the bound is what keeps a long process flat."""
    for n in range(_state._SESSION_FACTS_MAX + 1):
        _pre(api_request_id=f"req-{n}", session_id=f"sess-{n}")

    assert _state.session_facts_get("sess-0") is None
    assert _state.session_facts_get(f"sess-{_state._SESSION_FACTS_MAX}") is not None


def test_the_capture_map_evicts_the_least_recently_used_session(patch_client: Any, env_creds: None) -> None:
    for n in range(_state._SESSION_FACTS_MAX):
        _pre(api_request_id=f"req-{n}", session_id=f"sess-{n}")
    assert _state.session_facts_get("sess-0") is not None
    _pre(api_request_id="req-new", session_id="sess-new")

    assert _state.session_facts_get("sess-1") is None
    assert _state.session_facts_get("sess-0") is not None


def test_model_cache_eviction_keeps_shared_facts(patch_client: Any, env_creds: None) -> None:
    _pre(model="model-0")
    for n in range(1, _state._SESSION_FACTS_MODELS_MAX):
        _pre(api_request_id=f"req-{n}", model=f"model-{n}", request={"body": {"max_tokens": n}})
    assert _state.session_facts_get("sess-1", "model-0") is not None
    _pre(api_request_id="req-new", model="model-new", request=CLIPPED_REQUEST)

    entry = _state._SESSION_REQUEST_FACTS["sess-1"]
    assert len(entry.models) == _state._SESSION_FACTS_MODELS_MAX
    assert "model-1" not in entry.models
    assert "model-0" in entry.models

    _pre(api_request_id="req-return", model="model-0", request=CLIPPED_REQUEST)
    assert patch_client.start_generation_calls[-1].max_tokens == 8192
    _pre(api_request_id="req-evicted", model="model-1", request=CLIPPED_REQUEST)
    start = patch_client.start_generation_calls[-1]
    assert start.max_tokens is None
    assert start.temperature is None
    assert start.system_prompt == "be helpful"
    assert [tool.name for tool in start.tools] == ["read_file", "shell"]
    assert start.metadata["hermes.request_facts_reused"] is True
    assert len(entry.models) == _state._SESSION_FACTS_MODELS_MAX


def test_shared_facts_improve_across_models(patch_client: Any, env_creds: None) -> None:
    _pre(request=CLIPPED_FIELDS_REQUEST)
    _pre(api_request_id="req-2", model="other-model")
    _pre(api_request_id="req-3", request=CLIPPED_REQUEST)

    start = patch_client.start_generation_calls[-1]
    assert start.system_prompt == "be helpful"
    assert [tool.name for tool in start.tools] == ["read_file", "shell"]


def test_the_history_system_prompt_is_the_last_resort(patch_client: Any, env_creds: None) -> None:
    """No request payload at all: a pre-0.16.0 hermes, or a collapsed envelope.

    Current releases put no system message in ``conversation_history``, so this
    path is a fallback rather than the normal source.
    """
    _pre(request=None, conversation_history=[{"role": "system", "content": "from the history"}, *CONVO])

    assert patch_client.start_generation_calls[0].system_prompt == "from the history"


def test_a_clipped_request_still_records_the_tool_count(patch_client: Any, env_creds: None) -> None:
    """``tools: []`` beside a non-zero count reads as lost schemas, not no tools."""
    _pre(request=CLIPPED_REQUEST, tool_count=17)

    start = patch_client.start_generation_calls[0]
    assert start.tools == []
    assert start.metadata["hermes.tool_count"] == 17


def test_the_truncation_note_names_the_host_knob_once(
    patch_client: Any, env_creds: None, caplog: pytest.LogCaptureFixture
) -> None:
    with caplog.at_level(logging.DEBUG):
        for n in range(3):
            _pre(api_request_id=f"req-{n}", request=CLIPPED_REQUEST)

    notes = [r for r in caplog.records if "HERMES_PLUGIN_PAYLOAD_MAX_CHARS" in r.getMessage()]
    assert len(notes) == 1


def test_generations_carry_the_builtin_tags(patch_client: Any, env_creds: None) -> None:
    """The cross-plugin tags, so hermes filters like cursor and codex do.

    The SDK merges the client tags underneath the seed tags, so the export sees
    the union whichever side a tag rides on.
    """
    _pre()

    cfg = _client._get_plugin_config()
    assert cfg is not None
    tags = {**_client._to_client_config(cfg).tags, **patch_client.start_generation_calls[0].tags}
    assert tags["entrypoint"] == "hermes"
    assert "cwd" not in tags
    assert "git.branch" not in tags
    assert tags["agento11y.framework.name"] == "hermes"
    assert tags["agento11y.framework.source"] == "plugin"
    assert tags["agento11y.framework.language"] == "python"


def test_the_identity_tags_ride_on_the_client_config(patch_client: Any, env_creds: None) -> None:
    """Only client tags reach spans and metrics, as agento11y.tag.<key>."""
    cfg = _client._get_plugin_config()
    assert cfg is not None

    tags = _client._to_client_config(cfg).tags

    assert tags["agento11y.framework.name"] == "hermes"
    assert tags["agento11y.framework.source"] == "plugin"
    assert tags["agento11y.framework.language"] == "python"
    assert tags["entrypoint"] == "hermes"
    assert "git.branch" not in tags
    assert "cwd" not in tags, "one metric series per working directory is not worth the label"


def test_a_tool_execution_is_typed_as_a_function(patch_client: Any, env_creds: None) -> None:
    _hooks.on_post_tool_call(tool_name="read_file", session_id="sess-1", tool_call_id="call-1")

    assert patch_client.start_tool_execution_calls[0].tool_type == "function"


def test_effective_version_reads_the_shared_name(patch_client: Any, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("AGENTO11Y_AGENT_VERSION", "1.2.3")
    _pre()

    assert patch_client.start_generation_calls[0].effective_version == "1.2.3"


def test_the_deprecated_version_name_still_works_and_warns(
    patch_client: Any,
    monkeypatch: pytest.MonkeyPatch,
    caplog: pytest.LogCaptureFixture,
) -> None:
    monkeypatch.delenv("AGENTO11Y_AGENT_VERSION", raising=False)
    monkeypatch.setenv("AGENTO11Y_HERMES_AGENT_VERSION", "0.9")

    with caplog.at_level(logging.WARNING):
        _pre()
        _pre(api_request_id="req-2")

    assert patch_client.start_generation_calls[0].effective_version == "0.9"
    deprecations = [r for r in caplog.records if "AGENTO11Y_HERMES_AGENT_VERSION" in r.getMessage()]
    assert len(deprecations) == 1


def test_a_failed_tool_sets_the_exec_error_before_the_result(patch_client: Any, env_creds: None) -> None:
    _hooks.on_post_tool_call(
        tool_name="read_file",
        args={"path": "/nope"},
        result="",
        session_id="sess-1",
        tool_call_id="call-1",
        status="error",
        error_type="FileNotFoundError",
        error_message="boom",
    )

    rec = patch_client._next_tool_recorder
    assert rec.calls == ["set_exec_error", "set_result"]
    assert str(rec.set_exec_error_calls[0]) == "boom"


def test_a_failed_tool_without_a_message_falls_back(patch_client: Any, env_creds: None) -> None:
    _hooks.on_post_tool_call(
        tool_name="read_file",
        session_id="sess-1",
        tool_call_id="call-1",
        status="error",
        error_type="FileNotFoundError",
    )

    assert str(patch_client._next_tool_recorder.set_exec_error_calls[0]) == "FileNotFoundError"


def test_a_successful_tool_sets_no_exec_error(patch_client: Any, env_creds: None) -> None:
    _hooks.on_post_tool_call(
        tool_name="read_file",
        result="contents",
        session_id="sess-1",
        tool_call_id="call-1",
        status="ok",
    )

    assert patch_client._next_tool_recorder.set_exec_error_calls == []


@pytest.mark.parametrize("status", ["blocked", "cancelled", "BLOCKED", "CANCELLED"])
def test_a_blocked_or_cancelled_tool_has_no_execution(patch_client: Any, status: str) -> None:
    _hooks.on_post_tool_call(tool_name="read_file", session_id="sess-1", status=status)

    assert patch_client.start_tool_execution_calls == []


@pytest.mark.parametrize("sample_request", [True, False])
@pytest.mark.parametrize("previous_model", [False, True])
def test_a_tool_execution_carries_the_requesting_model(
    patch_client: Any, env_creds: None, monkeypatch: pytest.MonkeyPatch, sample_request: bool, previous_model: bool
) -> None:
    """post_tool_call carries no model, so the metric needs the cached one."""
    if previous_model:
        _pre(api_request_id="previous", model="old-model", provider="old-provider")
    samples = iter([sample_request, True])
    monkeypatch.setattr(_hooks, "_should_sample", lambda: next(samples))
    _pre(model="claude-sonnet-4-6", provider="anthropic")

    _hooks.on_post_tool_call(tool_name="read_file", session_id="sess-1", tool_call_id="call-1")

    start = patch_client.start_tool_execution_calls[0]
    assert start.request_model == "claude-sonnet-4-6"
    assert start.request_provider == "anthropic"


# --- the generations of one turn form a chain ---
#
# A tool loop is a DAG: call N+1's input is call N's output plus the tool
# results. Chained per turn_id, so a session is a set of chains rather than one
# long line. MoA fan-out puts concurrent requests in one turn and will chain
# them in an arbitrary order; that is left as it is.


def test_the_second_call_of_a_turn_names_the_first(patch_client: Any, env_creds: None) -> None:
    _pre(api_request_id="req-1")
    _post(api_request_id="req-1")
    _pre(api_request_id="req-2")

    first, second = patch_client.start_generation_calls
    assert first.parent_generation_ids == []
    assert second.parent_generation_ids == [first.id]


def test_a_superseded_attempt_is_not_the_next_calls_parent(patch_client: Any, env_creds: None) -> None:
    """The chain is written at close time, and a displaced attempt never closes clean."""
    _pre(api_request_id="req-1")
    _pre(api_request_id="req-1")
    _post(api_request_id="req-1")
    _pre(api_request_id="req-2")

    abandoned, kept, following = patch_client.start_generation_calls
    assert abandoned.id != kept.id
    assert following.parent_generation_ids == [kept.id]


def test_a_new_turn_starts_a_new_chain(patch_client: Any, env_creds: None) -> None:
    _pre(api_request_id="req-1", turn_id="turn-1")
    _post(api_request_id="req-1", turn_id="turn-1")

    _pre(api_request_id="req-2", turn_id="turn-2")

    assert patch_client.start_generation_calls[1].parent_generation_ids == []


def test_post_llm_call_ends_the_turns_chain(patch_client: Any, env_creds: None) -> None:
    _pre()
    _post()

    _hooks.on_post_llm_call(task_id="task-1", session_id="sess-1", turn_id="turn-1")

    assert _state.turn_last_gen_get("turn-1") == ""


def test_a_failed_call_is_not_a_parent(patch_client: Any, env_creds: None) -> None:
    _pre(api_request_id="req-1")
    _hooks.on_api_request_error(api_request_id="req-1", error="boom", status_code=500)

    _pre(api_request_id="req-2")

    assert patch_client.start_generation_calls[1].parent_generation_ids == []


def test_the_link_maps_drop_their_oldest_entries(patch_client: Any, env_creds: None) -> None:
    """A process that runs for days must not grow them without limit."""
    overflow = _state._MAX_ENTRIES + 10
    for index in range(overflow):
        _state.gen_link_put(f"req-{index}", _state.GenLink(generation_id=f"gen-{index}"))
        _state.turn_last_gen_put(f"turn-{index}", f"gen-{index}", "sess-1")

    assert _state.gen_link_get("req-0") is None
    assert _state.turn_last_gen_get("turn-0") == ""
    assert _state.gen_link_get(f"req-{overflow - 1}") is not None
    assert _state.turn_last_gen_get(f"turn-{overflow - 1}") == f"gen-{overflow - 1}"


# --- tool executions linked to the call that requested them ---


def test_a_tool_span_names_the_generation_that_requested_it(patch_client: Any, env_creds: None) -> None:
    _pre()
    generation_id = patch_client.start_generation_calls[0].id
    assert generation_id

    _hooks.on_post_tool_call(
        tool_name="read_file",
        session_id="sess-1",
        tool_call_id="call-1",
        api_request_id="req-1",
    )

    attributes = patch_client._next_tool_recorder.span.attributes
    assert attributes["agento11y.generation.parent_generation_ids"] == [generation_id]


def test_a_tool_span_is_started_inside_the_generations_trace(
    patch_client: Any,
    env_creds: None,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """The SDK parents the span off the ambient context, so that is what we set."""
    _pre()
    generation_context = patch_client._next_gen_recorder.span.get_span_context()
    ambient: list[Any] = []
    original = patch_client.start_tool_execution

    def capture(start: Any) -> Any:
        ambient.append(otel_trace.get_current_span().get_span_context())
        return original(start)

    monkeypatch.setattr(patch_client, "start_tool_execution", capture)

    _hooks.on_post_tool_call(
        tool_name="read_file",
        session_id="sess-1",
        tool_call_id="call-1",
        api_request_id="req-1",
    )

    assert ambient[0].trace_id == generation_context.trace_id
    assert ambient[0].span_id == generation_context.span_id
    assert not otel_trace.get_current_span().get_span_context().is_valid, "the context must be detached again"


def test_a_tool_of_an_unknown_request_is_still_recorded(patch_client: Any, env_creds: None) -> None:
    """Fail open: no link resolves, so the tool becomes its own root span."""
    _pre()

    _hooks.on_post_tool_call(
        tool_name="read_file",
        args={"path": "/tmp/x"},
        result="contents",
        session_id="sess-1",
        tool_call_id="call-1",
        api_request_id="req-does-not-exist",
    )

    rec = patch_client._next_tool_recorder
    assert rec.exited
    assert rec.set_result_calls[0]["arguments"] == {"path": "/tmp/x"}
    assert rec.set_result_calls[0]["result"] == "contents"
    assert "agento11y.generation.parent_generation_ids" not in rec.span.attributes


def test_a_tool_without_a_request_id_is_still_recorded(patch_client: Any, env_creds: None) -> None:
    _pre()

    _hooks.on_post_tool_call(tool_name="read_file", session_id="sess-1", tool_call_id="call-1")

    rec = patch_client._next_tool_recorder
    assert rec.exited
    assert "agento11y.generation.parent_generation_ids" not in rec.span.attributes


def test_a_recorder_without_a_span_does_not_break_the_hook(
    patch_client: Any,
    env_creds: None,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """``NoopToolExecutionRecorder`` has no span, and neither does a stubbed one."""
    _pre()
    original = patch_client.start_tool_execution

    def spanless(start: Any) -> Any:
        recorder = original(start)
        del recorder.span
        return recorder

    monkeypatch.setattr(patch_client, "start_tool_execution", spanless)

    _hooks.on_post_tool_call(
        tool_name="read_file",
        session_id="sess-1",
        tool_call_id="call-1",
        api_request_id="req-1",
    )

    assert patch_client._next_tool_recorder.exited


@pytest.mark.parametrize(
    ("shared_version", "legacy_version", "expected_version", "expected_effective_version"),
    [("1.2.3", "0.9", "1.2.3", "1.2.3"), ("", "0.9", "", "0.9"), ("", "", "", "")],
)
def test_the_exported_spans_share_a_trace_and_carry_the_client_tags(
    monkeypatch: pytest.MonkeyPatch,
    request: pytest.FixtureRequest,
    env_creds: None,
    shared_version: str,
    legacy_version: str,
    expected_version: str,
    expected_effective_version: str,
) -> None:
    """Use the real SDK because the fake client starts no spans."""
    import agento11y
    from opentelemetry.sdk.trace import TracerProvider
    from opentelemetry.sdk.trace.export import SimpleSpanProcessor
    from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

    from grafana_agento11y_hermes import _otel

    exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(exporter))

    monkeypatch.setenv("AGENTO11Y_AGENT_VERSION", shared_version)
    monkeypatch.setenv("AGENTO11Y_HERMES_AGENT_VERSION", legacy_version)
    real_client = agento11y.Client
    generations: list[Any] = []

    def factory(config: Any) -> Any:
        config.tracer = provider.get_tracer("test")
        config.generation_export.protocol = "none"
        client = real_client(config)
        monkeypatch.setattr(client, "_enqueue_generation", generations.append)
        # The only real client the suite builds, and nothing else closes it:
        # reset_module_state drops the reference without shutting it down, and
        # Client.__init__ has already started a flush timer thread that would
        # then wake once a second for the rest of the run. Zeroing the interval
        # is not the way out, because the SDK clamps a non-positive one to 1ms.
        request.addfinalizer(client.shutdown)
        return client

    monkeypatch.setattr(agento11y, "Client", factory)
    monkeypatch.setattr(_otel, "setup_if_needed", lambda cfg: True)

    _pre()
    _hooks.on_post_tool_call(
        tool_name="read_file",
        session_id="sess-1",
        tool_call_id="call-1",
        api_request_id="req-1",
    )
    _post()

    assert len(generations) == 1
    assert generations[0].agent_version == expected_version
    assert generations[0].effective_version == expected_effective_version
    spans = {span.name.split()[0]: span for span in exporter.get_finished_spans()}
    generation, tool = spans["generateText"], spans["execute_tool"]
    assert tool.context.trace_id == generation.context.trace_id
    assert tool.parent is not None and tool.parent.span_id == generation.context.span_id
    assert attributes(tool)["agento11y.generation.parent_generation_ids"] == (
        attributes(generation)["agento11y.generation.id"],
    )
    for span in (generation, tool):
        assert attributes(span).get("gen_ai.agent.version", "") == expected_version
        assert attributes(span)["agento11y.tag.agento11y.framework.name"] == "hermes"
        assert attributes(span)["agento11y.tag.entrypoint"] == "hermes"
    # mode stays SYNC, which is what makes the operation generateText. See
    # "mode stays SYNC" in CLAUDE.md for why streaming is not detectable here.
    assert attributes(generation)["gen_ai.operation.name"] == "generateText"


def test_current_hermes_stops_maintaining_the_legacy_convo(patch_client: Any, env_creds: None) -> None:
    """Bookkeeping only the pre-v2026.6.5 path reads."""
    _pre()

    _hooks.on_pre_llm_call(session_id="sess-1", conversation_history=list(CONVO))
    _hooks.on_post_tool_call(tool_name="read_file", session_id="sess-1", tool_call_id="call-1")

    assert _state.convo_get(("", "sess-1")) == []


# --- flushing the failure path ---
#
# Hermes one-shot fires no session hook when a turn dies on a provider error,
# and exits via os._exit (hermes_cli/main.py:_exit_after_oneshot), which skips
# the SDK's atexit flush. The error hook is the last chance to export.


def test_api_request_error_flushes(patch_client: Any, env_creds: None) -> None:
    _pre()

    _hooks.on_api_request_error(api_request_id="req-1", error="boom", status_code=500)

    assert patch_client.flush_calls == 1


def test_api_request_error_does_not_flush_for_an_unknown_id(patch_client: Any, env_creds: None) -> None:
    """Nothing was closed, so there is nothing new to export."""
    _pre()

    _hooks.on_api_request_error(api_request_id="req-other", error="boom")

    assert patch_client.flush_calls == 0


def test_error_flush_timeout_zero_skips_the_flush(
    patch_client: Any, env_creds: None, monkeypatch: pytest.MonkeyPatch
) -> None:
    cfg = _client._get_plugin_config()
    assert cfg is not None
    monkeypatch.setattr(cfg, "error_flush_timeout", 0.0)
    _pre()

    _hooks.on_api_request_error(api_request_id="req-1", error="boom", status_code=500)

    assert patch_client._next_gen_recorder.exited, "the generation still closes"
    assert patch_client.flush_calls == 0


def test_a_hanging_flush_does_not_block_the_hook(
    patch_client: Any, env_creds: None, monkeypatch: pytest.MonkeyPatch
) -> None:
    """Fail open: a stuck exporter must not stall the hermes loop."""
    release = threading.Event()

    def hanging_flush() -> None:
        release.wait(30)

    monkeypatch.setattr(patch_client, "flush", hanging_flush)
    cfg = _client._get_plugin_config()
    assert cfg is not None
    monkeypatch.setattr(cfg, "error_flush_timeout", 0.05)
    _pre()

    started = time.monotonic()
    _hooks.on_api_request_error(api_request_id="req-1", error="boom", status_code=500)
    elapsed = time.monotonic() - started
    release.set()

    assert elapsed < 5, f"hook waited {elapsed}s on a hanging flush"


def test_session_finalize_flushes(patch_client: Any, env_creds: None) -> None:
    """The only session hook the interactive failure path gets."""
    _hooks.on_session_finalize(session_id="sess-1")

    assert patch_client.flush_calls == 1


def test_session_finalize_closes_a_still_open_generation(patch_client: Any, env_creds: None) -> None:
    _pre()
    rec = patch_client._next_gen_recorder

    _hooks.on_session_finalize(session_id="sess-1")

    assert rec.exited
