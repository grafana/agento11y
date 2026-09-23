from __future__ import annotations

from typing import Any

import pytest
from agento11y import GenerationStart, MessageRole, PartKind, ToolExecutionStart

from grafana_agento11y_hermes import _hooks, _state


def _sample_messages() -> list[dict]:
    return [
        {"role": "system", "content": "You are concise."},
        {"role": "user", "content": "What's 2+2?"},
        {
            "role": "assistant",
            "content": None,
            "tool_calls": [
                {"id": "tc_1", "function": {"name": "calc", "arguments": '{"expr": "2+2"}'}},
            ],
        },
        {"role": "tool", "tool_call_id": "tc_1", "content": "4"},
    ]


def test_pre_api_request_calls_start_generation_with_expected_fields(patch_client) -> None:
    _hooks.on_pre_api_request(
        task_id="t1",
        session_id="s1",
        model="claude-sonnet-4-6",
        provider="anthropic",
        messages=_sample_messages(),
        api_call_count=1,
    )

    assert len(patch_client.start_generation_calls) == 1
    start: GenerationStart = patch_client.start_generation_calls[0]
    assert isinstance(start, GenerationStart)
    assert start.conversation_id == "s1"
    assert start.agent_name == "hermes"
    assert start.model.provider == "anthropic"
    assert start.model.name == "claude-sonnet-4-6"
    assert start.system_prompt == "You are concise."
    assert start.metadata.get("hermes.api_call_count") == 1
    assert start.metadata.get("hermes.task_id") == "t1"

    rec = patch_client._next_gen_recorder
    assert rec.entered
    assert not rec.exited
    # Input is stored on GenState; threaded into set_result at close-time.
    state = _state.gen_get(("t1", "s1", 1))
    assert state is not None
    assert any(m.role == MessageRole.USER for m in state.input_messages)


def test_the_legacy_path_reads_the_request_payload_too(patch_client) -> None:
    """``on_pre_api_request`` is shared, so a pre-0.16.0 hermes captures both."""
    _hooks.on_pre_api_request(
        task_id="t1",
        session_id="s1",
        model="claude-sonnet-4-6",
        provider="anthropic",
        messages=_sample_messages(),
        api_call_count=1,
        tool_count=1,
        request={
            "method": "POST",
            "body": {"system": "be helpful", "tools": [{"name": "calc", "input_schema": {"type": "object"}}]},
        },
    )

    start: GenerationStart = patch_client.start_generation_calls[0]
    assert [tool.name for tool in start.tools] == ["calc"]
    assert start.system_prompt == "be helpful", "the request body beats a system message in the history"
    assert start.metadata["hermes.tool_count"] == 1


def test_pre_post_api_request_round_trip(patch_client) -> None:
    _hooks.on_pre_api_request(
        task_id="t1",
        session_id="s1",
        model="gpt-4.1",
        provider="openai",
        messages=_sample_messages(),
        api_call_count=2,
    )
    rec = patch_client._next_gen_recorder
    assert rec.entered

    assistant_resp = {"role": "assistant", "content": "The answer is 4.", "tool_calls": []}
    _hooks.on_post_api_request(
        task_id="t1",
        session_id="s1",
        api_call_count=2,
        model="gpt-4.1",
        assistant_message=assistant_resp,
        usage={
            "input_tokens": 100,
            "output_tokens": 20,
            "cache_read_input_tokens": 30,
            "cache_creation_input_tokens": 5,
            "reasoning_tokens": 7,
        },
        finish_reason="stop",
        messages=_sample_messages(),
    )

    # Recorder is NOT yet closed — close is deferred to post_llm_call so we
    # can assign the assistant output from conversation_history.
    assert not rec.exited

    _hooks.on_post_llm_call(
        task_id="t1",
        session_id="s1",
        conversation_history=[
            {"role": "user", "content": "What's 2+2?"},
            assistant_resp,
        ],
        assistant_response="The answer is 4.",
    )

    assert rec.exited
    assert _state.gen_pop(("t1", "s1", 2)) is None

    final = rec.set_result_calls[-1]
    assert final["stop_reason"] == "stop"
    assert final["response_model"] == "gpt-4.1"
    usage = final["usage"]
    assert usage.input_tokens == 100
    assert usage.output_tokens == 20
    assert usage.cache_read_input_tokens == 30
    assert usage.cache_write_input_tokens == 5
    assert usage.reasoning_tokens == 7
    output_messages = final["output"]
    assert len(output_messages) == 1
    assert output_messages[0].role == MessageRole.ASSISTANT
    assert any(p.kind == PartKind.TEXT and "4" in p.text for p in output_messages[0].parts)


def test_post_tool_call_records_full_round_trip(patch_client) -> None:
    args = {"path": "/tmp/foo.txt", "limit": 100}
    result = {"content": "hello world", "total_lines": 1}

    _hooks.on_post_tool_call(
        tool_name="read_file",
        args=args,
        result=result,
        task_id="t1",
        session_id="s1",
        tool_call_id="tc_42",
        duration_ms=42,
    )

    assert len(patch_client.start_tool_execution_calls) == 1
    start: ToolExecutionStart = patch_client.start_tool_execution_calls[0]
    assert isinstance(start, ToolExecutionStart)
    assert start.tool_name == "read_file"
    assert start.tool_call_id == "tc_42"
    assert start.conversation_id == "s1"
    assert start.agent_name == "hermes"
    # Plugin must not pin include_content — the SDK derives it from the capture
    # mode, so no_tool_content can still strip tool args/results from the span.
    assert start.include_content is False
    assert start.started_at is not None

    rec = patch_client._next_tool_recorder
    assert rec.entered
    assert rec.exited
    final = rec.set_result_calls[-1]
    # Args/result pass through redactor — values are echoed since they're tiny
    assert final["arguments"] == {"path": "/tmp/foo.txt", "limit": 100}
    assert final["result"] == {"content": "hello world", "total_lines": 1}
    delta_ms = (final["completed_at"] - start.started_at).total_seconds() * 1000
    assert delta_ms == pytest.approx(42, abs=1)


def test_missing_credentials_makes_handlers_noop(monkeypatch: pytest.MonkeyPatch) -> None:
    for name in (
        "AGENTO11Y_ENDPOINT",
        "AGENTO11Y_PROTOCOL",
        "AGENTO11Y_AUTH_MODE",
        "AGENTO11Y_AUTH_TENANT_ID",
        "AGENTO11Y_AUTH_TOKEN",
        "OTEL_EXPORTER_OTLP_ENDPOINT",
    ):
        monkeypatch.delenv(name, raising=False)
    # Also delete any leftover legacy names from a host shell.
    for name in (
        "HERMES_SIGIL_ENDPOINT",
        "HERMES_SIGIL_INSTANCE_ID",
        "HERMES_SIGIL_API_KEY",
        "HERMES_SIGIL_OTLP_ENDPOINT",
        "HERMES_SIGIL_OTLP_INSTANCE_ID",
        "HERMES_SIGIL_OTLP_TOKEN",
    ):
        monkeypatch.delenv(name, raising=False)

    constructed: list[Any] = []

    import agento11y

    def boom(*_: Any, **__: Any) -> Any:
        constructed.append(True)
        raise AssertionError("Client should not be constructed when creds missing")

    monkeypatch.setattr(agento11y, "Client", boom)

    _hooks.on_pre_api_request(task_id="t", session_id="s", model="m", provider="p", messages=[], api_call_count=1)
    _hooks.on_post_api_request(task_id="t", session_id="s", api_call_count=1)
    _hooks.on_post_tool_call(tool_name="x", task_id="t", session_id="s", tool_call_id="tc")
    _hooks.on_session_end()
    assert constructed == []


def test_client_init_failure_is_cached(monkeypatch: pytest.MonkeyPatch, env_creds: None) -> None:
    """Construction error → handlers swallow, subsequent calls don't retry."""
    import agento11y

    from grafana_agento11y_hermes import _otel

    monkeypatch.setattr(_otel, "setup_if_needed", lambda cfg: True)

    call_count = {"n": 0}

    def boom(*_: Any, **__: Any) -> Any:
        call_count["n"] += 1
        raise RuntimeError("unreachable endpoint")

    monkeypatch.setattr(agento11y, "Client", boom)

    _hooks.on_pre_api_request(task_id="t", session_id="s", model="m", provider="p", messages=[], api_call_count=1)
    _hooks.on_post_tool_call(tool_name="x", task_id="t", session_id="s", tool_call_id="tc")
    _hooks.on_session_end()

    assert call_count["n"] == 1, "client construction must only be retried once after failure"


def test_on_session_end_flushes_without_closing_client(patch_client) -> None:
    """on_session_end must flush, not shutdown — the client is a process-wide singleton."""
    _hooks.on_session_end()
    assert patch_client.flush_calls == 1
    assert patch_client.shutdown_calls == 0


def test_session_end_lets_subsequent_session_record(patch_client) -> None:
    _hooks.on_pre_api_request(
        task_id="t1",
        session_id="s1",
        model="m",
        provider="p",
        messages=[{"role": "user", "content": "hi"}],
        api_call_count=1,
    )
    _hooks.on_session_end()
    _hooks.on_pre_api_request(
        task_id="t2",
        session_id="s2",
        model="m",
        provider="p",
        messages=[{"role": "user", "content": "again"}],
        api_call_count=1,
    )
    assert len(patch_client.start_generation_calls) == 2


def test_session_end_force_flushes_installed_providers(
    monkeypatch: pytest.MonkeyPatch,
    env_creds: None,
) -> None:
    import agento11y

    from grafana_agento11y_hermes import _client, _otel
    from tests.conftest import FakeClient

    class FakeProvider:
        def __init__(self) -> None:
            self.flush_calls = 0

        def force_flush(self, *_: Any, **__: Any) -> None:
            self.flush_calls += 1

    fake_tracer = FakeProvider()
    fake_meter = FakeProvider()
    monkeypatch.setattr(_otel, "_INSTALLED_TRACER_PROVIDER", fake_tracer, raising=False)
    monkeypatch.setattr(_otel, "_INSTALLED_METER_PROVIDER", fake_meter, raising=False)
    monkeypatch.setattr(_otel, "setup_if_needed", lambda cfg: True)
    monkeypatch.setattr(agento11y, "Client", lambda *a, **k: FakeClient())

    assert _client._get_client() is not None
    _hooks.on_session_end()
    assert fake_tracer.flush_calls == 1
    assert fake_meter.flush_calls == 1


def test_session_end_does_not_flush_user_owned_providers(
    monkeypatch: pytest.MonkeyPatch,
    env_creds: None,
) -> None:
    import agento11y
    from opentelemetry import trace

    from grafana_agento11y_hermes import _client, _otel
    from tests.conftest import FakeClient

    class FakeProvider:
        def __init__(self) -> None:
            self.flush_calls = 0

        def force_flush(self, *_: Any, **__: Any) -> None:
            self.flush_calls += 1

    # Globally installed, but by the host: _INSTALLED_*_PROVIDER stay None, so
    # the plugin has no claim on it. Reaching for the module globals rather
    # than set_tracer_provider, which is once-per-process.
    fake_provider = FakeProvider()
    monkeypatch.setattr(trace, "_TRACER_PROVIDER", fake_provider, raising=False)
    monkeypatch.setattr(_otel, "setup_if_needed", lambda cfg: True)
    monkeypatch.setattr(agento11y, "Client", lambda *a, **k: FakeClient())
    assert _client._get_client() is not None
    assert trace.get_tracer_provider() is fake_provider

    _hooks.on_session_end()
    assert fake_provider.flush_calls == 0


def test_on_session_end_does_not_initialize_client(monkeypatch: pytest.MonkeyPatch, env_creds: None) -> None:
    """on_session_end must use create_if_missing=False and not trigger init."""
    import agento11y

    from grafana_agento11y_hermes import _otel

    monkeypatch.setattr(_otel, "setup_if_needed", lambda cfg: True)

    constructed = {"n": 0}

    def factory(*_: Any, **__: Any) -> Any:
        constructed["n"] += 1
        return object()

    monkeypatch.setattr(agento11y, "Client", factory)

    _hooks.on_session_end()
    assert constructed["n"] == 0


def test_post_api_request_without_pre_is_safe(patch_client) -> None:
    _hooks.on_post_api_request(task_id="t", session_id="s", api_call_count=999, assistant_message={"content": "x"})


def test_post_tool_call_with_unknown_id_is_safe(patch_client) -> None:
    _hooks.on_post_tool_call(tool_name="x", task_id="t", session_id="s", tool_call_id="ghost")


def test_sample_rate_zero_skips_recording(monkeypatch: pytest.MonkeyPatch, patch_client) -> None:
    """AGENTO11Y_HERMES_SAMPLE_RATE=0 → pre-hooks short-circuit, no recorder created."""
    from grafana_agento11y_hermes import _client, _config

    monkeypatch.setattr(
        _client,
        "_CONFIG",
        _config.PluginConfig(sample_rate=0.0),
        raising=False,
    )

    _hooks.on_pre_api_request(
        task_id="t",
        session_id="s",
        model="m",
        provider="p",
        messages=[{"role": "user", "content": "hi"}],
        api_call_count=1,
    )
    _hooks.on_post_tool_call(tool_name="x", args={}, result="ok", task_id="t", session_id="s", tool_call_id="tc1")

    assert patch_client.start_generation_calls == []
    assert patch_client.start_tool_execution_calls == []


def test_pre_llm_call_seeds_input_for_pre_api_request(patch_client) -> None:
    """Hermes does not pass messages to pre_api_request — input must come from pre_llm_call."""
    _hooks.on_pre_llm_call(
        task_id="t1",
        session_id="s1",
        conversation_history=[
            {"role": "user", "content": "hey"},
        ],
    )
    _hooks.on_pre_api_request(
        task_id="t1",
        session_id="s1",
        model="m",
        provider="p",
        api_call_count=1,
        # messages NOT passed — matches real hermes
    )

    rec = patch_client._next_gen_recorder
    assert rec is not None
    state = _state.gen_get(("t1", "s1", 1))
    assert state is not None
    assert len(state.input_messages) == 1
    assert state.input_messages[0].role == MessageRole.USER


def test_post_llm_call_assigns_outputs_to_pending_recorders(patch_client) -> None:
    """Tool loop: 2 LLM calls, post_llm_call assigns each call's assistant output."""
    _hooks.on_pre_llm_call(
        task_id="t1",
        session_id="s1",
        conversation_history=[{"role": "user", "content": "search for X"}],
    )
    # LLM call 1 — emits a tool call
    _hooks.on_pre_api_request(task_id="t1", session_id="s1", model="m", provider="p", api_call_count=1)
    rec1 = patch_client._next_gen_recorder
    _hooks.on_post_api_request(
        task_id="t1",
        session_id="s1",
        api_call_count=1,
        model="m",
        usage={"input_tokens": 10, "output_tokens": 5},
        finish_reason="tool_calls",
    )
    assert not rec1.exited  # deferred close
    # LLM call 2 — final answer
    _hooks.on_pre_api_request(task_id="t1", session_id="s1", model="m", provider="p", api_call_count=2)
    rec2 = patch_client._next_gen_recorder
    _hooks.on_post_api_request(
        task_id="t1",
        session_id="s1",
        api_call_count=2,
        model="m",
        usage={"input_tokens": 20, "output_tokens": 8},
        finish_reason="stop",
    )
    assert not rec2.exited

    asst1 = {
        "role": "assistant",
        "content": None,
        "tool_calls": [{"id": "tc_1", "function": {"name": "search", "arguments": '{"q":"X"}'}}],
    }
    asst2 = {"role": "assistant", "content": "found 3 results"}
    _hooks.on_post_llm_call(
        task_id="t1",
        session_id="s1",
        conversation_history=[
            {"role": "user", "content": "search for X"},
            asst1,
            {"role": "tool", "tool_call_id": "tc_1", "content": "results"},
            asst2,
        ],
        assistant_response="found 3 results",
    )

    assert rec1.exited and rec2.exited
    final1 = rec1.set_result_calls[-1]
    final2 = rec2.set_result_calls[-1]
    assert final1["stop_reason"] == "tool_calls"
    assert any(p.kind == PartKind.TOOL_CALL for p in final1["output"][0].parts)
    assert final2["stop_reason"] == "stop"
    assert any(p.kind == PartKind.TEXT and "found 3 results" in p.text for p in final2["output"][0].parts)


def test_completed_at_uses_api_duration_not_recorder_close_time(patch_client) -> None:
    """Span end + duration metric must reflect the LLM call, not the close time.

    If we used wallclock at close, the first call in a tool loop would report
    a span/histogram covering the full turn (LLM + tool + later calls).
    """
    _hooks.on_pre_api_request(
        task_id="t1",
        session_id="s1",
        model="m",
        provider="p",
        messages=[{"role": "user", "content": "hi"}],
        api_call_count=1,
    )
    rec = patch_client._next_gen_recorder
    state = _state.gen_get(("t1", "s1", 1))
    assert state is not None and state.started_at is not None
    started_at = state.started_at

    _hooks.on_post_api_request(
        task_id="t1",
        session_id="s1",
        api_call_count=1,
        model="m",
        usage={},
        finish_reason="stop",
        api_duration=2.5,
    )
    _hooks.on_post_llm_call(task_id="t1", session_id="s1", conversation_history=[])

    final = rec.set_result_calls[-1]
    assert final["started_at"] == started_at
    completed_at = final["completed_at"]
    assert completed_at is not None
    delta = (completed_at - started_at).total_seconds()
    assert delta == pytest.approx(2.5)


def test_completed_at_is_none_when_api_duration_missing(patch_client) -> None:
    """No api_duration → leave completed_at unset; SDK falls back to its clock."""
    _hooks.on_pre_api_request(
        task_id="t1",
        session_id="s1",
        model="m",
        provider="p",
        messages=[{"role": "user", "content": "hi"}],
        api_call_count=1,
    )
    rec = patch_client._next_gen_recorder
    _hooks.on_post_api_request(
        task_id="t1",
        session_id="s1",
        api_call_count=1,
        model="m",
        usage={},
        finish_reason="stop",
    )
    _hooks.on_post_llm_call(task_id="t1", session_id="s1", conversation_history=[])

    final = rec.set_result_calls[-1]
    assert final["completed_at"] is None


def test_close_pending_handles_discarded_retry(patch_client) -> None:
    """Discarded retry iterations must not steal a prior turn's assistant.

    Hermes increments ``api_call_count`` on every iteration, including ones
    whose response is discarded (incomplete <REASONING_SCRATCHPAD>, invalid-
    response retries). The discarded
    iteration's recorder is still in ``_GEN_STATE`` when ``post_llm_call``
    fires, but no assistant message was appended for it. End-anchored
    pairing closes the discarded recorder with empty output rather than
    pulling an assistant from a successful call or an earlier turn.
    """
    prior_history = [
        {"role": "user", "content": "first turn"},
        {"role": "assistant", "content": "first turn answer"},
        {"role": "user", "content": "second turn"},
    ]
    _hooks.on_pre_llm_call(
        task_id="t1",
        session_id="s1",
        conversation_history=prior_history,
    )
    # Iteration 1 — response discarded by hermes (`continue` without append).
    _hooks.on_pre_api_request(task_id="t1", session_id="s1", model="m", provider="p", api_call_count=1)
    rec1 = patch_client._next_gen_recorder
    _hooks.on_post_api_request(
        task_id="t1",
        session_id="s1",
        api_call_count=1,
        model="m",
        usage={"input_tokens": 10, "output_tokens": 5},
        finish_reason="stop",
    )
    # Iteration 2 — kept; produces final_response.
    _hooks.on_pre_api_request(task_id="t1", session_id="s1", model="m", provider="p", api_call_count=2)
    rec2 = patch_client._next_gen_recorder
    _hooks.on_post_api_request(
        task_id="t1",
        session_id="s1",
        api_call_count=2,
        model="m",
        usage={"input_tokens": 12, "output_tokens": 6},
        finish_reason="stop",
    )

    asst_for_iter_2 = {"role": "assistant", "content": "real answer"}
    _hooks.on_post_llm_call(
        task_id="t1",
        session_id="s1",
        conversation_history=[*prior_history, asst_for_iter_2],
        assistant_response="real answer",
    )

    # Discarded iter 1 closes empty — must NOT have stolen "first turn answer".
    final1 = rec1.set_result_calls[-1]
    assert final1["output"] == [], f"discarded iteration must close with empty output, got {final1['output']}"
    final2 = rec2.set_result_calls[-1]
    assert any(p.kind == PartKind.TEXT and "real answer" in p.text for p in final2["output"][0].parts)


def test_session_end_closes_pending_recorders_on_interrupt(patch_client) -> None:
    """If post_llm_call never fires (interrupt), on_session_end must still close recorders."""
    _hooks.on_pre_api_request(
        task_id="t1",
        session_id="s1",
        model="m",
        provider="p",
        messages=[{"role": "user", "content": "hi"}],
        api_call_count=1,
    )
    rec = patch_client._next_gen_recorder
    _hooks.on_post_api_request(
        task_id="t1",
        session_id="s1",
        api_call_count=1,
        model="m",
        usage={},
        finish_reason="length",
    )
    assert not rec.exited
    # session_end fires before post_llm_call (interrupt path)
    _hooks.on_session_end(session_id="s1")
    assert rec.exited
    # Output is empty (no conversation_history to derive it from), but the
    # recorder was closed and partial state (input, usage) was set.
    final = rec.set_result_calls[-1]
    assert final["output"] == []


def test_running_convo_includes_assistant_and_tool_results(patch_client) -> None:
    """Tool loop: conversation grows across calls so api_call_count=2 has full input."""
    _hooks.on_pre_llm_call(
        task_id="t1",
        session_id="s1",
        conversation_history=[{"role": "user", "content": "search for X"}],
    )
    # Call #1 — model decides to call a tool
    _hooks.on_pre_api_request(
        task_id="t1",
        session_id="s1",
        model="m",
        provider="p",
        api_call_count=1,
    )
    _hooks.on_post_api_request(
        task_id="t1",
        session_id="s1",
        api_call_count=1,
        model="m",
        usage={},
        finish_reason="tool_calls",
    )
    # Tool runs — post_tool_call synthesizes the assistant tool_call message
    # and appends the tool result, both into the running convo.
    _hooks.on_post_tool_call(
        tool_name="search",
        args={"q": "X"},
        task_id="t1",
        session_id="s1",
        tool_call_id="tc_1",
        result="found 3 results",
    )
    # Call #2 — model gets to see user msg + asst tool call + tool result
    _hooks.on_pre_api_request(
        task_id="t1",
        session_id="s1",
        model="m",
        provider="p",
        api_call_count=2,
    )
    rec2 = patch_client._next_gen_recorder
    assert rec2 is not None
    state2 = _state.gen_get(("t1", "s1", 2))
    assert state2 is not None
    roles = [m.role for m in state2.input_messages]
    assert MessageRole.USER in roles
    assert MessageRole.ASSISTANT in roles
    assert MessageRole.TOOL in roles


def test_post_llm_call_clears_running_convo(patch_client) -> None:
    _hooks.on_pre_llm_call(
        task_id="t1",
        session_id="s1",
        conversation_history=[{"role": "user", "content": "hi"}],
    )
    from grafana_agento11y_hermes import _state

    # Convo is keyed by session_id only — task_id is not passed to pre_llm_call.
    assert _state.convo_get(("", "s1")) != []
    _hooks.on_post_llm_call(task_id="t1", session_id="s1")
    assert _state.convo_get(("", "s1")) == []


def test_sample_rate_one_records_everything(patch_client) -> None:
    """AGENTO11Y_HERMES_SAMPLE_RATE=1.0 (default) → every call recorded."""
    _hooks.on_pre_api_request(
        task_id="t",
        session_id="s",
        model="m",
        provider="p",
        messages=[{"role": "user", "content": "hi"}],
        api_call_count=1,
    )
    assert len(patch_client.start_generation_calls) == 1


def test_client_called_with_content_capture_override_when_generations_configured(
    monkeypatch: pytest.MonkeyPatch,
    env_creds: None,
) -> None:
    """Capture defaults to metadata-only without overriding SDK transport resolution."""
    import agento11y
    from agento11y import ContentCaptureMode

    from grafana_agento11y_hermes import _client, _otel

    monkeypatch.delenv("AGENTO11Y_CONTENT_CAPTURE_MODE", raising=False)
    captured: list[Any] = []

    def factory(*args: Any, **kwargs: Any) -> Any:
        captured.append(args[0] if args else None)
        from tests.conftest import FakeClient

        return FakeClient()

    monkeypatch.setattr(agento11y, "Client", factory)
    monkeypatch.setattr(_otel, "setup_if_needed", lambda cfg: True)

    assert _client._get_client() is not None
    assert len(captured) == 1
    cfg = captured[0]
    # The override is content_capture only; transport is left to env resolution.
    assert cfg.content_capture == ContentCaptureMode.METADATA_ONLY
    # The plugin must not pin protocol="none" when generations are configured —
    # that switch is reserved for OTel-only mode.
    assert cfg.generation_export.protocol != "none"
    # Generations get a plugin User-Agent so the backend can attribute the traffic.
    ua = cfg.generation_export.headers["User-Agent"]
    assert ua.startswith("agento11y-plugin-hermes/")
    assert "agento11y-sdk-python/" in ua


def test_client_sends_plugin_user_agent_when_content_capture_mode_set(
    monkeypatch: pytest.MonkeyPatch,
    env_creds: None,
) -> None:
    """Transport and auth stay env-resolved with an explicit capture mode."""
    import agento11y

    from grafana_agento11y_hermes import _client, _otel

    monkeypatch.setenv("AGENTO11Y_CONTENT_CAPTURE_MODE", "no_tool_content")
    captured: list[Any] = []

    def factory(*args: Any, **kwargs: Any) -> Any:
        captured.append(args[0] if args else kwargs.get("config"))
        from tests.conftest import FakeClient

        return FakeClient()

    monkeypatch.setattr(agento11y, "Client", factory)
    monkeypatch.setattr(_otel, "setup_if_needed", lambda cfg: True)

    assert _client._get_client() is not None
    assert len(captured) == 1
    cfg = captured[0]
    assert cfg.content_capture.value == "no_tool_content"
    assert cfg.generation_export.protocol != "none"
    assert cfg.generation_export.headers["User-Agent"].startswith("agento11y-plugin-hermes/")


def test_export_headers_preserved_and_user_agent_override_wins(
    monkeypatch: pytest.MonkeyPatch,
    env_creds: None,
) -> None:
    import agento11y

    from grafana_agento11y_hermes import _client, _otel

    monkeypatch.setenv("AGENTO11Y_HEADERS", "X-Custom=1,User-Agent=my-agent/9")
    captured: list[Any] = []

    def factory(*args: Any, **kwargs: Any) -> Any:
        captured.append(args[0] if args else kwargs.get("config"))
        from tests.conftest import FakeClient

        return FakeClient()

    monkeypatch.setattr(agento11y, "Client", factory)
    monkeypatch.setattr(_otel, "setup_if_needed", lambda cfg: True)

    assert _client._get_client() is not None
    headers = captured[0].generation_export.headers
    assert headers["X-Custom"] == "1"
    assert headers["User-Agent"] == "my-agent/9"


def test_legacy_hermes_sigil_names_are_ignored(monkeypatch: pytest.MonkeyPatch) -> None:
    for name in (
        "AGENTO11Y_ENDPOINT",
        "AGENTO11Y_PROTOCOL",
        "AGENTO11Y_AUTH_MODE",
        "AGENTO11Y_AUTH_TENANT_ID",
        "AGENTO11Y_AUTH_TOKEN",
        "OTEL_EXPORTER_OTLP_ENDPOINT",
    ):
        monkeypatch.delenv(name, raising=False)
    monkeypatch.setenv("HERMES_SIGIL_ENDPOINT", "http://legacy/api")
    monkeypatch.setenv("HERMES_SIGIL_INSTANCE_ID", "stack-1")
    monkeypatch.setenv("HERMES_SIGIL_API_KEY", "glc_secret")
    monkeypatch.setenv("HERMES_SIGIL_OTLP_ENDPOINT", "http://legacy/otlp")

    import agento11y

    # Counted out here, not asserted inside the factory: _client swallows every
    # exception the constructor raises, an AssertionError included.
    constructed: list[Any] = []

    def factory(*args: Any, **kwargs: Any) -> Any:
        constructed.append(args or kwargs)
        from tests.conftest import FakeClient

        return FakeClient()

    monkeypatch.setattr(agento11y, "Client", factory)

    _hooks.on_pre_api_request(task_id="t", session_id="s", model="m", provider="p", messages=[], api_call_count=1)

    assert constructed == []


def test_client_config_uses_protocol_none_when_only_otel_configured(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """OTel-only mode: no AGENTO11Y_AUTH_TOKEN/MODE → SDK's HTTP exporter is disabled."""
    import agento11y

    from grafana_agento11y_hermes import _client, _otel

    for name in (
        "AGENTO11Y_AUTH_TOKEN",
        "AGENTO11Y_AUTH_MODE",
        "AGENTO11Y_AUTH_TENANT_ID",
        "AGENTO11Y_ENDPOINT",
        "AGENTO11Y_PROTOCOL",
    ):
        monkeypatch.delenv(name, raising=False)
    monkeypatch.setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://otlp")

    captured: list[Any] = []

    def factory(*args: Any, **kwargs: Any) -> Any:
        captured.append(args[0] if args else kwargs.get("config"))
        from tests.conftest import FakeClient

        return FakeClient()

    monkeypatch.setattr(agento11y, "Client", factory)
    monkeypatch.setattr(_otel, "setup_if_needed", lambda cfg: True)

    assert _client._get_client() is not None
    assert len(captured) == 1
    assert captured[0].generation_export.protocol == "none"
