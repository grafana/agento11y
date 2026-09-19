"""Hook failures must not interrupt the hermes loop."""

from __future__ import annotations

import logging
from collections.abc import Callable
from typing import Any

import pytest

from grafana_agento11y_hermes import _hooks, _state
from tests.conftest import FakeClient

# Every SDK call the plugin makes across a full turn. Each one is a place the
# real client can raise: a closed client, a full queue, a recorder the SDK
# changed the shape of.
SDK_CALLS = (
    "start_generation",
    "start_tool_execution",
    "flush",
    "__enter__",
    "__exit__",
    "set_result",
    "set_call_error",
    "set_exec_error",
)


def _drive_a_turn(*, session: str = "s1", request: str = "req-1") -> None:
    """Fire the hooks of one tool-calling turn, in the order hermes fires them."""
    _hooks.on_pre_llm_call(session_id=session, conversation_history=[], user_message="hi")
    _hooks.on_pre_api_request(
        session_id=session,
        api_request_id=request,
        turn_id="turn-1",
        model="claude-opus-5",
        provider="anthropic",
        conversation_history=[{"role": "user", "content": "hi"}],
        tool_count=1,
    )
    _hooks.on_post_tool_call(
        session_id=session,
        api_request_id=request,
        tool_name="bash",
        tool_call_id="call-1",
        args={"command": "ls"},
        result="a.txt",
        duration_ms=12,
    )
    _hooks.on_post_api_request(
        session_id=session,
        api_request_id=request,
        model="claude-opus-5",
        assistant_message={"role": "assistant", "content": "done"},
        usage={"input_tokens": 10, "output_tokens": 2},
        api_duration=0.5,
    )
    _hooks.on_post_llm_call(session_id=session, turn_id="turn-1", conversation_history=[])
    _hooks.on_session_end(session_id=session)
    _hooks.on_session_finalize(session_id=session)


@pytest.mark.parametrize("failing_call", SDK_CALLS)
def test_a_raising_sdk_call_never_reaches_the_hermes_loop(
    failing_call: str,
    failing_client: Callable[..., FakeClient],
) -> None:
    failing_client(failing_call)
    _drive_a_turn()


@pytest.mark.parametrize("failing_call", SDK_CALLS)
def test_a_raising_sdk_call_on_the_error_path_is_contained(
    failing_call: str,
    failing_client: Callable[..., FakeClient],
) -> None:
    failing_client(failing_call)
    _hooks.on_pre_api_request(session_id="s1", api_request_id="req-1", model="m", provider="p")
    _hooks.on_api_request_error(api_request_id="req-1", error={"type": "RateLimit"}, status_code=429)
    _hooks.on_session_end(session_id="s1")


def test_every_sdk_call_failing_at_once_still_completes_a_turn(
    failing_client: Callable[..., FakeClient],
) -> None:
    failing_client(*SDK_CALLS)
    _drive_a_turn()


def test_a_failure_is_logged_rather_than_swallowed_silently(
    failing_client: Callable[..., FakeClient],
    caplog: pytest.LogCaptureFixture,
) -> None:
    failing_client("start_generation")
    with caplog.at_level(logging.WARNING):
        _hooks.on_pre_api_request(session_id="s1", api_request_id="req-1", model="m", provider="p")
    assert [r for r in caplog.records if "on_pre_api_request failed" in r.getMessage()]


# Each hook paired with a ``_state`` function it reaches on the way through.
# Poisoning that function is what proves the handler's own guard is there, as
# opposed to the narrower ones around the SDK calls.
HOOK_POISON: tuple[tuple[str, dict[str, Any], str], ...] = (
    ("on_pre_llm_call", {"session_id": "s1", "conversation_history": []}, "convo_set"),
    ("on_post_llm_call", {"session_id": "s1", "turn_id": "t1"}, "gen_pop_session"),
    ("on_pre_api_request", {"session_id": "s1", "api_request_id": "r1"}, "session_facts_get"),
    ("on_post_api_request", {"session_id": "s1", "api_request_id": "r1"}, "req_pop"),
    ("on_api_request_error", {"api_request_id": "r1"}, "req_pop"),
    ("on_post_tool_call", {"session_id": "s1", "tool_name": "bash"}, "session_model_get"),
    ("on_session_end", {"session_id": "s1"}, "req_pop_session"),
    ("on_session_finalize", {"session_id": "s1"}, "req_pop_session"),
)


@pytest.mark.parametrize(("hook_name", "kwargs", "poisoned"), HOOK_POISON, ids=[c[0] for c in HOOK_POISON])
def test_a_raising_state_layer_never_reaches_the_hermes_loop(
    hook_name: str,
    kwargs: dict[str, Any],
    poisoned: str,
    patch_client: FakeClient,
    monkeypatch: pytest.MonkeyPatch,
    caplog: pytest.LogCaptureFixture,
) -> None:
    def explode(*_: Any, **__: Any) -> Any:
        raise RuntimeError(f"{poisoned} exploded")

    monkeypatch.setattr(_state, poisoned, explode)
    with caplog.at_level(logging.WARNING):
        getattr(_hooks, hook_name)(**kwargs)
    assert [r for r in caplog.records if " failed: " in r.getMessage()], "the failure has to be logged"


GARBAGE_PAYLOADS: tuple[tuple[str, dict[str, Any]], ...] = (
    ("api_call_count as text", {"hook": "on_post_api_request", "api_call_count": "seven"}),
    ("api_call_count as text at open", {"hook": "on_pre_api_request", "api_call_count": "seven"}),
    ("usage as a string", {"hook": "on_post_api_request", "api_request_id": "r1", "usage": "none"}),
    ("assistant_message as a list", {"hook": "on_post_api_request", "api_request_id": "r1", "assistant_message": []}),
    ("api_duration as text", {"hook": "on_post_api_request", "api_request_id": "r1", "api_duration": "slow"}),
    ("conversation_history as an object", {"hook": "on_pre_api_request", "conversation_history": object()}),
    ("request as a string", {"hook": "on_pre_api_request", "request": "clipped"}),
    ("tool_count as text", {"hook": "on_pre_api_request", "tool_count": "many"}),
    ("duration_ms as text", {"hook": "on_post_tool_call", "tool_name": "bash", "duration_ms": "fast"}),
    ("args that will not serialize", {"hook": "on_post_tool_call", "tool_name": "bash", "args": {1, 2}}),
    ("result that will not serialize", {"hook": "on_post_tool_call", "tool_name": "bash", "result": object()}),
    ("status as a number", {"hook": "on_post_tool_call", "tool_name": "bash", "status": 500}),
    ("error as an object", {"hook": "on_api_request_error", "api_request_id": "r1", "error": object()}),
    ("status_code as text", {"hook": "on_api_request_error", "api_request_id": "r1", "status_code": "429"}),
    ("session_id as None", {"hook": "on_session_end", "session_id": None}),
    ("conversation_history as text", {"hook": "on_post_llm_call", "conversation_history": "nope"}),
    ("max_tokens as text", {"hook": "on_pre_api_request", "max_tokens": "lots"}),
    ("model as None", {"hook": "on_pre_api_request", "model": None, "provider": None}),
    ("request_messages as a dict", {"hook": "on_pre_api_request", "request_messages": {"role": "user"}}),
    ("system_prompt as a list", {"hook": "on_pre_api_request", "system_prompt": ["be nice"]}),
    ("turn_id as an object", {"hook": "on_pre_api_request", "turn_id": object()}),
    (
        "messages holding non-dicts",
        {"hook": "on_pre_api_request", "conversation_history": ["raw", 7, None, {"role": "user"}]},
    ),
    (
        "tool_calls of the wrong shape",
        {
            "hook": "on_post_api_request",
            "api_request_id": "r1",
            "assistant_message": {"role": "assistant", "tool_calls": ["not-a-call", 3]},
        },
    ),
    ("usage counts as text", {"hook": "on_post_api_request", "api_request_id": "r1", "usage": {"input_tokens": "ten"}}),
    ("finish_reason as a number", {"hook": "on_post_api_request", "api_request_id": "r1", "finish_reason": 5}),
    ("tool_call_id as a number", {"hook": "on_post_tool_call", "tool_name": "bash", "tool_call_id": 7}),
    ("tool_name as None", {"hook": "on_post_tool_call", "tool_name": None}),
    (
        "error_message as a dict",
        {"hook": "on_post_tool_call", "tool_name": "bash", "status": "error", "error_message": {"why": "boom"}},
    ),
    ("negative duration_ms", {"hook": "on_post_tool_call", "tool_name": "bash", "duration_ms": -5}),
    ("error as a bare exception", {"hook": "on_api_request_error", "api_request_id": "r1", "error": ValueError("x")}),
    (
        "error dict with a text status",
        {"hook": "on_api_request_error", "api_request_id": "r1", "error": {"status": "?"}},
    ),
)


@pytest.mark.parametrize(("label", "payload"), GARBAGE_PAYLOADS, ids=[c[0] for c in GARBAGE_PAYLOADS])
def test_a_malformed_payload_is_coerced_rather_than_caught(
    label: str,
    payload: dict[str, Any],
    patch_client: FakeClient,
    caplog: pytest.LogCaptureFixture,
) -> None:
    """A bad field should be absorbed by the read that touches it.

    Asserting on the absence of the backstop log is what makes this stronger
    than "no exception escaped": ``_fail_open`` would hide an unguarded
    conversion either way, and the handler that took the backstop abandoned
    everything after the bad field, which usually means a leaked recorder.
    """
    kwargs = dict(payload)
    hook = getattr(_hooks, kwargs.pop("hook"))
    kwargs.setdefault("session_id", "s1")
    with caplog.at_level(logging.WARNING):
        hook(**kwargs)
    rescued = [r.getMessage() for r in caplog.records if " failed: " in r.getMessage()]
    assert not rescued, f"{label} fell through to the fail-open backstop: {rescued}"
    _hooks.on_session_end(session_id="s1")


def test_an_unknown_kwarg_is_ignored(patch_client: FakeClient) -> None:
    """Hermes adds hook kwargs between releases; the handlers take ``**_``."""
    for name in (
        "on_pre_llm_call",
        "on_post_llm_call",
        "on_pre_api_request",
        "on_post_api_request",
        "on_api_request_error",
        "on_post_tool_call",
        "on_session_end",
        "on_session_finalize",
    ):
        getattr(_hooks, name)(session_id="s1", api_request_id="r1", a_field_from_a_future_hermes=object())


def test_a_client_that_cannot_be_built_leaves_every_hook_a_no_op(
    monkeypatch: pytest.MonkeyPatch,
    env_creds: None,
) -> None:
    import agento11y

    from grafana_agento11y_hermes import _otel

    def explode(*_: Any, **__: Any) -> Any:
        raise RuntimeError("no client for you")

    monkeypatch.setattr(agento11y, "Client", explode)
    monkeypatch.setattr(_otel, "setup_if_needed", lambda cfg: True)
    _drive_a_turn()
