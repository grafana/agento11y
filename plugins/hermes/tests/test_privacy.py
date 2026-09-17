"""Exercise privacy through real SDK recorders and in-memory exporters."""

from __future__ import annotations

import copy
from dataclasses import asdict
from typing import Any

import pytest
from agento11y import Client, ContentCaptureMode
from agento11y.models import ExportGenerationResult, ExportGenerationsResponse
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

from grafana_agento11y_hermes import _client, _compat, _config, _hooks, _redact

SECRET = "glc_abcdefghijklmnopqrstuvwxyz1234"


class MemoryExporter:
    def __init__(self) -> None:
        self.generations: list[Any] = []

    def export_generations(self, request: Any) -> ExportGenerationsResponse:
        self.generations.extend(copy.deepcopy(request.generations))
        return ExportGenerationsResponse(
            results=[ExportGenerationResult(generation_id=g.id, accepted=True) for g in request.generations]
        )

    def shutdown(self) -> None:
        pass


@pytest.mark.parametrize(
    ("canonical", "legacy", "expected"),
    [
        (None, None, "metadata_only"),
        ("", None, "metadata_only"),
        ("  ", None, "metadata_only"),
        ("default", None, "metadata_only"),
        ("invalid", None, "metadata_only"),
        (" FULL ", None, "full"),
        ("no_tool_content", None, "no_tool_content"),
        ("full_with_metadata_spans", None, "full_with_metadata_spans"),
        (None, "full", "full"),
        (" ", "full", "full"),
        ("metadata_only", "full", "metadata_only"),
        ("invalid", "full", "metadata_only"),
    ],
)
def test_capture_mode_table(monkeypatch, canonical, legacy, expected) -> None:
    for key, value in (("AGENTO11Y_CONTENT_CAPTURE_MODE", canonical), ("SIGIL_CONTENT_CAPTURE_MODE", legacy)):
        if value is not None:
            monkeypatch.setenv(key, value)
    assert _client._to_client_config(_config.PluginConfig()).content_capture == ContentCaptureMode(expected)


@pytest.mark.parametrize("mode", [None, "full", "no_tool_content", "full_with_metadata_spans"])
@pytest.mark.parametrize(
    ("redact_setting", "redact_inputs"),
    [(None, True), ("", True), ("invalid", True), ("true", True), ("false", False), (" OFF ", False)],
)
@pytest.mark.parametrize("secret", [SECRET, "ghp_" + "a" * 36])
def test_real_sdk_privacy(monkeypatch, mode, redact_setting, redact_inputs, secret) -> None:
    if mode is not None:
        monkeypatch.setenv("AGENTO11Y_CONTENT_CAPTURE_MODE", mode)
    if redact_setting is not None:
        monkeypatch.setenv("AGENTO11Y_REDACT_INPUT_MESSAGES", redact_setting)
    spans = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(spans))
    exports = MemoryExporter()
    plugin_config = _config.PluginConfig(generations_configured=True, error_flush_timeout=0)
    config = _client._to_client_config(plugin_config)
    config.tracer = provider.get_tracer("hermes-privacy-test")
    config.generation_exporter = exports
    client = Client(config)
    monkeypatch.setattr(_client, "_CLIENT", client)
    monkeypatch.setattr(_client, "_CONFIG", plugin_config)
    history = [
        {"role": "user", "content": f"user {secret}"},
        {
            "role": "assistant",
            "tool_calls": [{"id": secret, "function": {"name": secret, "arguments": {"secret": secret}}}],
        },
        {"role": "tool", "tool_call_id": secret, "content": secret},
    ]
    request = {"body": {"tools": [{"name": "read", "description": secret, "input_schema": {"default": secret}}]}}
    original = copy.deepcopy(request)
    try:
        _hooks.on_pre_api_request(
            api_request_id="request-1",
            session_id="session",
            task_id=secret,
            model="test-model",
            provider="test-provider",
            conversation_history=history,
            system_prompt=f"system {secret}",
            request=request,
        )
        _hooks.on_post_api_request(
            api_request_id="request-1",
            assistant_message={"role": "assistant", "content": f"assistant {secret}; sort key: name"},
            usage={"input_tokens": 10, "output_tokens": 2},
        )
        _hooks.on_post_tool_call(
            api_request_id="request-1",
            session_id="session",
            tool_name="read",
            tool_call_id="call-1",
            args={"token": "unstructured-credential", "value": secret},
            result={"password": "unstructured-credential", "value": secret},
            status="error",
            error_message=f"{secret}; sort key: name",
        )
        _hooks.on_pre_api_request(api_request_id="request-2", session_id="session", model="test-model")
        _hooks.on_api_request_error(
            api_request_id="request-2", error={"type": secret, "message": f"{secret}; sort key: name"}
        )
        client.flush()
        assert len(exports.generations) == 2
        generation = exports.generations[0]
        assert generation.usage.input_tokens == 10
        assert generation.metadata["hermes.task_id"] != secret
        serialized = repr(asdict(generation))
        if mode is None:
            assert all(not part.text for message in generation.input + generation.output for part in message.parts)
            assert all(not tool.description and not tool.input_schema_json for tool in generation.tools)
            assert not generation.system_prompt
            assert secret not in serialized
        else:
            assert generation.output and generation.system_prompt
            assert secret not in repr(generation.output)
            assert "sort key: name" in repr(generation.output)
            assert "sort key: name" in exports.generations[1].call_error
            assert secret not in repr(generation.tools)
            assert (secret not in repr(generation.input)) == redact_inputs
        assert secret not in repr(asdict(exports.generations[1]))
        finished = spans.get_finished_spans()
        assert len(finished) == 3
        if redact_inputs or mode in (None, "full_with_metadata_spans"):
            for span in finished:
                assert secret not in repr(dict(span.attributes or {}))
                assert secret not in repr([dict(event.attributes or {}) for event in span.events])
                assert secret not in str(span.status.description)
        tool = next(span for span in finished if span.name.startswith("execute_tool"))
        assert secret not in repr(dict(tool.attributes or {}))
        assert "unstructured-credential" not in repr(dict(tool.attributes or {}))
        assert secret not in repr([dict(event.attributes or {}) for event in tool.events])
        assert secret not in str(tool.status.description)
        if mode == "full":
            assert "[REDACTED" in repr(dict(tool.attributes or {}))
            assert "sort key: name" in str(tool.status.description)
        else:
            assert "gen_ai.tool.call.arguments" not in (tool.attributes or {})
        assert request == original
        assert history[0]["content"] == f"user {secret}"
    finally:
        client.shutdown()
        provider.shutdown()


def test_redact_before_truncation_and_copy() -> None:
    original = {SECRET: [SECRET, {"password": "DATABASE_PASSWORD=long-secret-value"}]}
    result = _redact.safe_value(original, max_chars=12)
    assert SECRET[:12] not in repr(result)
    assert SECRET in original
    assert _redact.redact_record(b"token=" + SECRET.encode()) != b"token=" + SECRET.encode()


@pytest.mark.parametrize("new", ["", " ", "new"])
def test_legacy_alias_precedence(new) -> None:
    env = {"SIGIL_ENDPOINT": "primary", "SIGIL_API_ENDPOINT": "secondary", "AGENTO11Y_ENDPOINT": new}
    _compat.apply_legacy_env(env)
    assert env["AGENTO11Y_ENDPOINT"] == (new if new.strip() else "primary")
    assert env["SIGIL_ENDPOINT"] == "primary"


@pytest.mark.parametrize("suffix", ["AUTH_TOKEN", "CONTENT_CAPTURE_MODE", "HEADERS", "HERMES_ERROR_FLUSH_TIMEOUT"])
def test_legacy_values_are_not_logged(suffix, caplog) -> None:
    env = {f"SIGIL_{suffix}": SECRET}
    _compat.apply_legacy_env(env)
    assert SECRET not in caplog.text
    assert env[f"AGENTO11Y_{suffix}"] == SECRET
