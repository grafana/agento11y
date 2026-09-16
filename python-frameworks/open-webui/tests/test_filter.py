from __future__ import annotations

import asyncio
import copy
import json
import logging
import sys
import time
from datetime import timedelta
from pathlib import Path
from typing import Any
from uuid import UUID

import pytest
from agento11y import (
    Client,
    ClientConfig,
    ContentCaptureMode,
    GenerationExportConfig,
    SecretRedactionOptions,
    create_secret_redaction_sanitizer,
)
from agento11y.models import ExportGenerationResult, ExportGenerationsResponse, MessageRole
from agento11y_open_webui import Filter
from agento11y_open_webui import filter as filter_module
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.metrics.export import InMemoryMetricReader

_DEFAULT_USAGE = object()
_CALLBACK_FIXTURE = json.loads((Path(__file__).parent / "fixtures" / "callbacks.json").read_text())


class _CapturingExporter:
    def __init__(self) -> None:
        self.requests: list[Any] = []

    def export_generations(self, request: Any) -> ExportGenerationsResponse:
        self.requests.append(request)
        return ExportGenerationsResponse(
            results=[ExportGenerationResult(generation_id=g.id, accepted=True) for g in request.generations]
        )

    def shutdown(self) -> None:
        pass


class _FakeRecorder:
    def __init__(
        self,
        *,
        end_error: Exception | None = None,
        final_error: Exception | None = None,
        error_check_error: Exception | None = None,
    ) -> None:
        self.end_error = end_error
        self.final_error = final_error
        self.error_check_error = error_check_error
        self.results: list[Any] = []
        self.end_calls = 0

    def set_result(self, generation: Any) -> None:
        self.results.append(generation)

    def end(self) -> None:
        self.end_calls += 1
        if self.end_error is not None:
            raise self.end_error

    def err(self) -> Exception | None:
        if self.error_check_error is not None:
            raise self.error_check_error
        return self.final_error


class _FakeClient:
    def __init__(
        self,
        recorder: _FakeRecorder | None = None,
        *,
        start_error: Exception | None = None,
    ) -> None:
        self.recorder = recorder or _FakeRecorder()
        self.start_error = start_error
        self.starts: list[Any] = []
        self.streaming_starts: list[Any] = []
        self.flush_calls = 0
        self.shutdown_calls = 0

    def start_generation(self, start: Any) -> _FakeRecorder:
        if self.start_error is not None:
            raise self.start_error
        self.starts.append(start)
        return self.recorder

    def start_streaming_generation(self, start: Any) -> _FakeRecorder:
        if self.start_error is not None:
            raise self.start_error
        self.streaming_starts.append(start)
        return self.recorder

    def flush(self) -> None:
        self.flush_calls += 1

    def shutdown(self) -> None:
        self.shutdown_calls += 1


def _new_client(
    exporter: _CapturingExporter,
    *,
    content_capture: ContentCaptureMode = ContentCaptureMode.METADATA_ONLY,
    meter: Any = None,
    agent_name: str = "",
    sanitize: bool = True,
) -> Client:
    sanitizer = (
        create_secret_redaction_sanitizer(SecretRedactionOptions(redact_input_messages=True)) if sanitize else None
    )
    return Client(
        ClientConfig(
            content_capture=content_capture,
            generation_sanitizer=sanitizer,
            generation_export=GenerationExportConfig(
                batch_size=10,
                flush_interval=timedelta(seconds=60),
            ),
            generation_exporter=exporter,
            meter=meter,
            agent_name=agent_name,
        )
    )


def _outlet_body(
    *,
    chat_id: str = "chat-1",
    message_id: str = "assistant-1",
    model: str = "connection.gpt-test",
    user_content: Any = "Question",
    assistant_content: Any = "Answer",
    usage: Any = _DEFAULT_USAGE,
    messages: list[dict[str, Any]] | None = None,
) -> dict[str, Any]:
    if usage is _DEFAULT_USAGE:
        usage = {"input_tokens": 10, "output_tokens": 4, "total_tokens": 14}
    return {
        "id": message_id,
        "chat_id": chat_id,
        "model": model,
        "messages": messages
        or [
            {"id": "user-1", "role": "user", "content": user_content},
            {
                "id": message_id,
                "role": "assistant",
                "content": assistant_content,
                "usage": usage,
            },
        ],
    }


def _run_callback(coro):
    return asyncio.run(coro)


def _run_turn(
    filter_: Filter,
    body: dict[str, Any],
    *,
    metadata: dict[str, Any] | None = None,
    user: dict[str, Any] | None = None,
    stream: bool = False,
) -> dict[str, Any]:
    request_metadata = metadata if metadata is not None else {}
    inlet_body = {"stream": stream, "messages": [{"role": "user", "content": "Question"}]}
    assert _run_callback(filter_.inlet(inlet_body, __metadata__=request_metadata)) is inlet_body
    assert _run_callback(filter_.outlet(body, __metadata__=request_metadata, __user__=user or {})) is body
    return request_metadata


def _exported_generations(client: Client, exporter: _CapturingExporter) -> list[Any]:
    client.flush()
    return [generation for request in exporter.requests for generation in request.generations]


def test_public_filter_import_is_instantiable() -> None:
    assert isinstance(Filter(client=_FakeClient()), Filter)


def test_documented_function_body_exposes_filter_without_open_webui() -> None:
    function_body = '''\
"""
title: Grafana Agent Observability
required_open_webui_version: 0.11.3
"""

from agento11y_open_webui import Filter
'''
    namespace: dict[str, Any] = {}

    exec(function_body, namespace)

    assert isinstance(namespace["Filter"](client=_FakeClient()), Filter)
    assert "open_webui" not in sys.modules


def test_injected_client_is_lazy_owner_independent(monkeypatch) -> None:
    registered: list[Any] = []
    monkeypatch.setattr(filter_module.atexit, "register", registered.append)
    client = _FakeClient()
    filter_ = Filter(client=client)

    _run_turn(filter_, _outlet_body())

    assert len(client.starts) == 1
    assert registered == []
    assert client.shutdown_calls == 0
    assert client.flush_calls == 0
    assert client.recorder.results[0].input == []
    assert client.recorder.results[0].output == []


def test_injected_default_client_keeps_metadata_only() -> None:
    exporter = _CapturingExporter()
    client = _new_client(
        exporter,
        content_capture=ContentCaptureMode.DEFAULT,
        sanitize=False,
    )
    secret = "sk-proj-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    try:
        _run_turn(
            Filter(client=client),
            _outlet_body(user_content=f"user {secret}", assistant_content=f"assistant {secret}"),
        )
        generation = _exported_generations(client, exporter)[0]

        assert generation.input == []
        assert generation.output == []
        assert generation.metadata["agento11y.sdk.content_capture_mode"] == "metadata_only"
        assert secret not in repr(generation)
    finally:
        client.shutdown()


def test_owned_client_is_created_once_with_private_defaults(monkeypatch) -> None:
    clients: list[_FakeClient] = []
    configs: list[ClientConfig] = []
    registered: list[Any] = []

    def new_client(config: ClientConfig) -> _FakeClient:
        configs.append(config)
        client = _FakeClient()
        clients.append(client)
        return client

    monkeypatch.setattr(filter_module, "Client", new_client)
    monkeypatch.setattr(filter_module.atexit, "register", registered.append)
    filter_ = Filter()
    assert clients == []

    _run_turn(filter_, _outlet_body(message_id="assistant-1"))
    _run_turn(filter_, _outlet_body(message_id="assistant-2"))

    assert len(clients) == 1
    assert configs[0].content_capture is ContentCaptureMode.METADATA_ONLY
    assert configs[0].agent_name == "open-webui"
    assert configs[0].generation_sanitizer is not None
    assert registered == [clients[0].shutdown]
    assert len(clients[0].starts) == 2


def test_explicit_capture_and_agent_name_survive_owned_client_defaults(monkeypatch) -> None:
    captured: list[ClientConfig] = []

    def from_env(cls) -> ClientConfig:
        return ClientConfig(content_capture=ContentCaptureMode.FULL, agent_name="support-assistant")

    def new_client(config: ClientConfig) -> _FakeClient:
        captured.append(config)
        return _FakeClient()

    monkeypatch.setattr(filter_module.ClientConfig, "from_env", classmethod(from_env))
    monkeypatch.setattr(filter_module, "Client", new_client)
    monkeypatch.setattr(filter_module.atexit, "register", lambda _callback: None)

    filter_ = Filter()
    _run_turn(filter_, _outlet_body())

    assert captured[0].content_capture is ContentCaptureMode.FULL
    assert captured[0].agent_name == "support-assistant"
    assert filter_._agent_name == "support-assistant"


def test_independent_replies_share_conversation_and_get_unique_generation_ids() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        filter_ = Filter(client=client)
        metadata_1 = _run_turn(filter_, _outlet_body(message_id="assistant-1"))
        metadata_2 = _run_turn(filter_, _outlet_body(message_id="assistant-2"))
        generations = _exported_generations(client, exporter)

        assert [generation.conversation_id for generation in generations] == ["chat-1", "chat-1"]
        assert len({generation.id for generation in generations}) == 2
        UUID(generations[0].id)
        UUID(generations[1].id)
        assert metadata_1["agento11y.open_webui"]["generation_id"] == generations[0].id
        assert metadata_2["agento11y.open_webui"]["generation_id"] == generations[1].id
    finally:
        client.shutdown()


def test_concurrent_conversations_keep_request_local_state() -> None:
    client = _FakeClient()
    filter_ = Filter(client=client)
    metadata_a: dict[str, Any] = {}
    metadata_b: dict[str, Any] = {}

    async def run() -> None:
        await asyncio.gather(
            filter_.inlet({"stream": False}, __metadata__=metadata_a),
            filter_.inlet({"stream": True}, __metadata__=metadata_b),
        )
        await asyncio.gather(
            filter_.outlet(_outlet_body(chat_id="chat-a"), __metadata__=metadata_a),
            filter_.outlet(_outlet_body(chat_id="chat-b"), __metadata__=metadata_b),
        )

    asyncio.run(run())

    starts = [*client.starts, *client.streaming_starts]
    assert {start.conversation_id for start in starts} == {"chat-a", "chat-b"}
    assert metadata_a["agento11y.open_webui"]["generation_id"] != metadata_b["agento11y.open_webui"]["generation_id"]


def test_duplicate_and_unmatched_outlets_do_not_export_twice() -> None:
    client = _FakeClient(recorder=_FakeRecorder(final_error=RuntimeError("queue secret")))
    filter_ = Filter(client=client)
    metadata = _run_turn(filter_, _outlet_body())
    body = _outlet_body()

    assert _run_callback(filter_.outlet(body, __metadata__=metadata)) is body
    assert _run_callback(filter_.outlet(body, __metadata__={})) is body

    assert len(client.starts) == 1
    assert metadata["agento11y.open_webui"]["consumed"] is True


def test_export_work_does_not_block_event_loop() -> None:
    class _SlowRecorder(_FakeRecorder):
        def end(self) -> None:
            time.sleep(0.1)
            super().end()

    client = _FakeClient(recorder=_SlowRecorder())
    filter_ = Filter(client=client)

    async def run() -> None:
        metadata: dict[str, Any] = {}
        await filter_.inlet({}, __metadata__=metadata)
        task = asyncio.create_task(filter_.outlet(_outlet_body(), __metadata__=metadata))

        await asyncio.sleep(0.01)
        assert not task.done()
        await task

    asyncio.run(run())


def test_inlet_state_is_json_compatible_and_contains_no_messages() -> None:
    filter_ = Filter(client=_FakeClient())
    metadata: dict[str, Any] = {"chat_id": "chat-1"}
    raw_message = "do not copy this"

    _run_callback(
        filter_.inlet(
            {"stream": True, "messages": [{"role": "user", "content": raw_message}]},
            __metadata__=metadata,
        )
    )

    state = metadata["agento11y.open_webui"]
    assert set(state) == {"generation_id", "started_at", "streaming", "consumed"}
    assert state["streaming"] is True
    assert state["consumed"] is False
    assert raw_message not in repr(state)


def test_selected_assistant_and_last_preceding_user_are_exported() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter, content_capture=ContentCaptureMode.FULL)
    messages = [
        {"id": "user-1", "role": "user", "content": "First question"},
        {"id": "assistant-1", "role": "assistant", "content": "First answer"},
        {"id": "user-2", "role": "user", "content": "Current question"},
        {
            "id": "assistant-2",
            "role": "assistant",
            "content": "Current answer",
            "usage": {"input_tokens": 9, "output_tokens": 3},
        },
        {"id": "assistant-3", "role": "assistant", "content": "Later answer"},
    ]
    try:
        _run_turn(Filter(client=client), _outlet_body(message_id="assistant-2", messages=messages))
        generation = _exported_generations(client, exporter)[0]

        assert generation.input[0].role is MessageRole.USER
        assert generation.input[0].parts[0].text == "Current question"
        assert generation.output[0].role is MessageRole.ASSISTANT
        assert generation.output[0].parts[0].text == "Current answer"
        assert {
            key: generation.metadata[key]
            for key in ("capture_scope", "selected_message_id", "selected_model_id", "usage_status")
        } == {
            "capture_scope": "response_snapshot",
            "selected_message_id": "assistant-2",
            "selected_model_id": "connection.gpt-test",
            "usage_status": "reported",
        }
    finally:
        client.shutdown()


def test_missing_selected_assistant_does_not_substitute_an_earlier_message() -> None:
    client = _FakeClient()
    filter_ = Filter(client=client)
    body = _outlet_body(
        message_id="missing",
        messages=[
            {"id": "user-1", "role": "user", "content": "Question"},
            {"id": "assistant-1", "role": "assistant", "content": "Old answer"},
        ],
    )

    _run_turn(filter_, body)

    assert client.starts == []


def test_text_extraction_uses_only_allowlisted_top_level_blocks() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter, content_capture=ContentCaptureMode.FULL)
    user_content = [
        {"type": "text", "text": "Question"},
        {"type": "image_url", "image_url": {"url": "private-image"}},
        {"type": "tool_result", "content": {"type": "text", "text": "nested user secret"}},
        {"type": "thinking", "text": "private thought"},
    ]
    assistant_content = [
        {"type": "output_text", "text": "Answer"},
        {"type": "attachment", "text": "attachment secret"},
        {"type": "tool_result", "content": [{"type": "output_text", "text": "nested output secret"}]},
    ]
    try:
        _run_turn(
            Filter(client=client),
            _outlet_body(user_content=user_content, assistant_content=assistant_content),
        )
        generation = _exported_generations(client, exporter)[0]

        assert generation.input[0].parts[0].text == "Question"
        assert generation.output[0].parts[0].text == "Answer"
        assert "secret" not in repr(generation)
        assert "private thought" not in repr(generation)
    finally:
        client.shutdown()


def test_model_and_user_identity_use_only_the_export_contract() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter, agent_name="configured-agent")
    try:
        _run_turn(
            Filter(client=client),
            _outlet_body(model="private-connection.model-7"),
            user={"id": "opaque-user-7", "name": "Private Name", "email": "private@example.com", "role": "admin"},
        )
        generation = _exported_generations(client, exporter)[0]

        assert generation.model.provider == "custom"
        assert generation.model.name == "open-webui-response-summary"
        assert generation.agent_name == "configured-agent"
        assert generation.user_id == "opaque-user-7"
        assert generation.metadata["selected_model_id"] == "private-connection.model-7"
        assert "Private Name" not in repr(generation)
        assert "private@example.com" not in repr(generation)
        assert "admin" not in repr(generation)
    finally:
        client.shutdown()


@pytest.mark.parametrize(
    ("usage", "metadata", "expected", "status"),
    [
        (
            {
                "input_tokens": 30,
                "output_tokens": 12,
                "total_tokens": 42,
                "prompt_tokens": 8,
                "completion_tokens": 3,
                "input_tokens_details": {"cached_tokens": 20},
                "output_tokens_details": {"reasoning_tokens": 7},
            },
            {},
            (30, 12, 42, 0, 0, 0),
            "reported",
        ),
        ({"input_tokens": 5, "output_tokens": 2}, {}, (5, 2, 7, 0, 0, 0), "reported"),
        (None, {}, (0, 0, 0, 0, 0, 0), "unavailable"),
        ({"prompt_tokens": 5, "completion_tokens": 2}, {}, (0, 0, 0, 0, 0, 0), "unavailable"),
        ({"input_tokens": True, "output_tokens": 2}, {}, (0, 0, 0, 0, 0, 0), "unavailable"),
        ({"input_tokens": -1, "output_tokens": 2}, {}, (0, 0, 0, 0, 0, 0), "unavailable"),
        (
            {"input_tokens": 30, "output_tokens": 12, "total_tokens": 42},
            {"assistant_message_id": "assistant-1"},
            (0, 0, 0, 0, 0, 0),
            "continuation_omitted",
        ),
    ],
)
def test_usage_contract(usage, metadata, expected, status) -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        _run_turn(Filter(client=client), _outlet_body(usage=usage), metadata=metadata)
        generation = _exported_generations(client, exporter)[0]
        actual = (
            generation.usage.input_tokens,
            generation.usage.output_tokens,
            generation.usage.total_tokens,
            generation.usage.cache_read_input_tokens,
            generation.usage.cache_write_input_tokens,
            generation.usage.reasoning_tokens,
        )

        assert actual == expected
        assert generation.metadata["usage_status"] == status
    finally:
        client.shutdown()


@pytest.mark.parametrize(
    "metadata",
    [{"assistant_message_id": "assistant-1"}, {"assistant_message_id": "approval-resume"}],
)
def test_continue_and_approval_resume_get_fresh_generation_ids(metadata) -> None:
    client = _FakeClient()
    filter_ = Filter(client=client)

    first = _run_turn(filter_, _outlet_body(), metadata=dict(metadata))
    second = _run_turn(filter_, _outlet_body(), metadata=dict(metadata))

    assert client.starts[0].id != client.starts[1].id
    assert first["agento11y.open_webui"]["generation_id"] == client.starts[0].id
    assert second["agento11y.open_webui"]["generation_id"] == client.starts[1].id
    assert client.recorder.results[0].usage.total_tokens == 0
    assert client.recorder.results[1].usage.total_tokens == 0


def test_missing_usage_emits_no_token_histogram_observations() -> None:
    reader = InMemoryMetricReader()
    provider = MeterProvider(metric_readers=[reader])
    exporter = _CapturingExporter()
    client = _new_client(exporter, meter=provider.get_meter("open-webui-test"))
    try:
        _run_turn(Filter(client=client), _outlet_body(usage={"prompt_tokens": 5}))
        client.flush()
        metric_names = {
            metric.name
            for resource_metrics in reader.get_metrics_data().resource_metrics
            for scope_metrics in resource_metrics.scope_metrics
            for metric in scope_metrics.metrics
        }

        assert "gen_ai.client.operation.duration" in metric_names
        assert "gen_ai.client.token.usage" not in metric_names
    finally:
        client.shutdown()
        provider.shutdown()


def test_metadata_only_default_exports_no_message_text() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    raw_user = "raw user secret"
    raw_assistant = "raw assistant secret"
    try:
        metadata = _run_turn(
            Filter(client=client),
            _outlet_body(user_content=raw_user, assistant_content=raw_assistant),
        )
        generation = _exported_generations(client, exporter)[0]

        assert generation.input == []
        assert generation.output == []
        assert raw_user not in repr(generation.metadata)
        assert raw_assistant not in repr(generation.metadata)
        assert raw_user not in repr(metadata["agento11y.open_webui"])
        assert raw_assistant not in repr(metadata["agento11y.open_webui"])
    finally:
        client.shutdown()


def test_opt_in_message_capture_redacts_input_and_output() -> None:
    exporter = _CapturingExporter()
    client = _new_client(
        exporter,
        content_capture=ContentCaptureMode.FULL,
        sanitize=False,
    )
    secret = "sk-proj-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    try:
        _run_turn(
            Filter(client=client),
            _outlet_body(user_content=f"user {secret}", assistant_content=f"assistant {secret}"),
        )
        generation = _exported_generations(client, exporter)[0]

        assert secret not in repr(generation)
        assert "[REDACTED:openai-project-key]" in generation.input[0].parts[0].text
        assert "[REDACTED:openai-project-key]" in generation.output[0].parts[0].text
        assert secret not in repr(generation.metadata)
    finally:
        client.shutdown()


def test_sanitizer_failure_falls_back_to_metadata_only(monkeypatch, caplog) -> None:
    exporter = _CapturingExporter()
    client = _new_client(
        exporter,
        content_capture=ContentCaptureMode.FULL,
        sanitize=False,
    )
    filter_ = Filter(client=client)
    secret = "sk-proj-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

    def fail_sanitization(_generation):
        raise RuntimeError("raw sanitizer secret")

    monkeypatch.setattr(filter_, "_sanitizer", fail_sanitization)
    try:
        with caplog.at_level(logging.WARNING, logger="agento11y_open_webui.filter"):
            _run_turn(
                filter_,
                _outlet_body(user_content=f"user {secret}", assistant_content=f"assistant {secret}"),
            )
        generation = _exported_generations(client, exporter)[0]

        assert generation.input == []
        assert generation.output == []
        assert generation.metadata["agento11y.sdk.content_capture_mode"] == "metadata_only"
        assert "content_sanitization" in caplog.text
        assert "raw sanitizer secret" not in caplog.text
        assert secret not in repr(generation)
    finally:
        client.shutdown()


@pytest.mark.parametrize(
    ("failure", "category"),
    [
        ("mapping", "snapshot_mapping"),
        ("recorder_initialization", "recorder_initialization"),
        ("finalization", "recorder_finalization"),
        ("queueing", "snapshot_queueing"),
        ("queue_check", "snapshot_queueing"),
    ],
)
def test_telemetry_failures_return_original_body_without_exception_text(caplog, failure, category) -> None:
    recorder = _FakeRecorder(
        end_error=RuntimeError("raw finalization secret") if failure == "finalization" else None,
        final_error=RuntimeError("raw queue secret") if failure == "queueing" else None,
        error_check_error=RuntimeError("raw queue check secret") if failure == "queue_check" else None,
    )
    client = _FakeClient(
        recorder=recorder,
        start_error=RuntimeError("raw recorder initialization secret")
        if failure == "recorder_initialization"
        else None,
    )
    filter_ = Filter(client=client)
    metadata: dict[str, Any] = {}
    inlet_body = {"stream": False}
    _run_callback(filter_.inlet(inlet_body, __metadata__=metadata))
    if failure == "mapping":
        metadata["agento11y.open_webui"]["started_at"] = "not-a-timestamp raw mapping secret"
    body = _outlet_body()

    with caplog.at_level(logging.WARNING, logger="agento11y_open_webui.filter"):
        result = _run_callback(filter_.outlet(body, __metadata__=metadata))

    assert result is body
    assert category in caplog.text
    assert "raw" not in caplog.text
    assert client.flush_calls == 0


def test_client_initialization_failure_is_fail_open(monkeypatch, caplog) -> None:
    def fail_client(_config):
        raise RuntimeError("raw initialization secret")

    monkeypatch.setattr(filter_module, "Client", fail_client)
    filter_ = Filter()
    metadata: dict[str, Any] = {}
    _run_callback(filter_.inlet({}, __metadata__=metadata))
    body = _outlet_body()

    with caplog.at_level(logging.WARNING, logger="agento11y_open_webui.filter"):
        result = _run_callback(filter_.outlet(body, __metadata__=metadata))

    assert result is body
    assert "client_initialization" in caplog.text
    assert "raw initialization secret" not in caplog.text
    assert metadata["agento11y.open_webui"]["consumed"] is True


def test_streaming_request_uses_streaming_recorder_without_ttft() -> None:
    client = _FakeClient()

    _run_turn(Filter(client=client), _outlet_body(), stream=True)

    assert client.starts == []
    assert len(client.streaming_starts) == 1
    assert client.streaming_starts[0].mode is None


@pytest.mark.parametrize("case", _CALLBACK_FIXTURE["cases"], ids=lambda case: case["name"])
def test_recorded_open_webui_callback_sequences(case) -> None:
    client = _FakeClient()
    filter_ = Filter(client=client)
    metadata = copy.deepcopy(case["inlet"]["metadata"])
    user = copy.deepcopy(case["inlet"]["user"])
    inlet_body = copy.deepcopy(case["inlet"]["body"])
    outlet_body = copy.deepcopy(case["outlet"]["body"])

    assert _run_callback(filter_.inlet(inlet_body, __metadata__=metadata)) is inlet_body
    assert "agento11y.open_webui" not in repr(case["provider_body"])
    assert _run_callback(filter_.outlet(outlet_body, __metadata__=metadata, __user__=user)) is outlet_body
    if case["outlet"]["repeat"]:
        assert _run_callback(filter_.outlet(outlet_body, __metadata__=metadata, __user__=user)) is outlet_body

    starts = [*client.starts, *client.streaming_starts]
    assert len(starts) == 1
    start = starts[0]
    expected_conversation = case["expected"]["conversation_id"]
    if expected_conversation == "invocation":
        expected_conversation = start.id
    assert start.conversation_id == expected_conversation
    assert start.metadata["usage_status"] == case["expected"]["usage_status"]
    assert bool(client.streaming_starts) is case["expected"]["streaming"]
    assert metadata["agento11y.open_webui"]["consumed"] is True
    if start.metadata["usage_status"] == "continuation_omitted":
        assert client.recorder.results[0].usage.total_tokens == 0


def test_callback_fixture_records_pinned_upstream_source() -> None:
    assert _CALLBACK_FIXTURE["upstream"] == {
        "repository": "https://github.com/open-webui/open-webui",
        "release": "v0.11.3",
        "release_revision": "2a960a59fe1dbbd35282f0556b3666d81102e781",
        "verified_revision": "0a7c15832fb30b1903753e83f81dc7d27e5b0944",
        "backend_diff_from_release": "none",
        "method": (
            "source-isolated process_filter_functions and outlet_filter_handler harnesses with a local fake provider"
        ),
    }
