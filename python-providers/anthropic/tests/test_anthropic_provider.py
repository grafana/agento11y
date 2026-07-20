"""Anthropic provider helper tests."""

from __future__ import annotations

from datetime import timedelta

import agento11y_anthropic
import pytest
from agento11y import Client, ClientConfig, GenerationExportConfig
from agento11y.models import ExportGenerationResult, ExportGenerationsResponse
from agento11y_anthropic import AnthropicOptions, AnthropicStreamSummary, messages


class _CapturingExporter:
    def __init__(self) -> None:
        self.requests = []

    def export_generations(self, request):
        self.requests.append(request)
        return ExportGenerationsResponse(
            results=[
                ExportGenerationResult(generation_id=generation.id, accepted=True) for generation in request.generations
            ]
        )

    def shutdown(self) -> None:
        return


def _new_client(exporter):
    return Client(
        ClientConfig(
            generation_export=GenerationExportConfig(batch_size=10, flush_interval=timedelta(seconds=60)),
            generation_exporter=exporter,
        )
    )


def _request() -> dict:
    return {
        "model": "claude-sonnet-4-5",
        "max_tokens": 512,
        "temperature": 0.3,
        "top_p": 0.75,
        "tool_choice": {"type": "tool", "name": "weather"},
        "thinking": {"type": "adaptive", "budget_tokens": 2048},
        "system": "be concise",
        "tools": [
            {
                "name": "weather",
                "description": "Get weather",
                "input_schema": {"type": "object"},
            }
        ],
        "messages": [
            {"role": "user", "content": "hello"},
            {
                "role": "user",
                "content": [
                    {
                        "type": "tool_result",
                        "tool_use_id": "call-weather",
                        "content": {"ok": True},
                    }
                ],
            },
        ],
    }


def _response() -> dict:
    return {
        "id": "resp-1",
        "model": "claude-sonnet-4-5-20260210",
        "role": "assistant",
        "content": [
            {"type": "tool_use", "id": "call-weather", "name": "weather", "input": {"city": "Paris"}},
            {"type": "text", "text": "world"},
        ],
        "stop_reason": "end_turn",
        "usage": {
            "input_tokens": 120,
            "output_tokens": 40,
            "cache_read_input_tokens": 12,
            "cache_creation_input_tokens": 4,
        },
    }


def test_anthropic_sync_wrapper_sets_sync_mode_and_raw_artifacts_default_off() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        messages.create(client, _request(), lambda _request: _response())

        client.flush()
        generation = exporter.requests[0].generations[0]
        assert generation.mode.value == "SYNC"
        assert generation.model.provider == "anthropic"
        assert generation.artifacts == []
    finally:
        client.shutdown()


def test_anthropic_stream_wrapper_sets_stream_mode_and_opt_in_artifacts() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        messages.stream(
            client,
            _request(),
            lambda _request: AnthropicStreamSummary(
                output_text="stream-output",
                events=[{"type": "content_block_delta", "delta": {"type": "text_delta", "text": "stream-output"}}],
            ),
            AnthropicOptions(raw_artifacts=True),
        )

        client.flush()
        generation = exporter.requests[0].generations[0]
        assert generation.mode.value == "STREAM"
        assert [artifact.kind.value for artifact in generation.artifacts] == ["request", "tools", "provider_event"]
    finally:
        client.shutdown()


def test_anthropic_wrapper_propagates_provider_error_and_sets_call_error() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    try:
        with pytest.raises(RuntimeError, match="provider failure"):
            messages.create(
                client,
                _request(),
                lambda _request: (_ for _ in ()).throw(RuntimeError("provider failure")),
            )

        client.flush()
        generation = exporter.requests[0].generations[0]
        assert generation.model.provider == "anthropic"
        assert generation.call_error == "provider failure"
    finally:
        client.shutdown()


def test_anthropic_wrappers_tolerate_missing_provider_payload_fields() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        messages.create(
            client,
            _request(),
            lambda _request: {
                "id": "resp-malformed",
                "model": "claude-sonnet-4-5-20260210",
                "role": "assistant",
                "content": [],
            },
        )
        messages.stream(
            client,
            _request(),
            lambda _request: AnthropicStreamSummary(
                events=[{"type": "content_block_delta", "delta": {"type": "text_delta"}}]
            ),
        )

        client.flush()
        generations = exporter.requests[0].generations
        assert len(generations) == 2

        sync_generation = generations[0]
        assert sync_generation.mode.value == "SYNC"
        assert sync_generation.response_id == "resp-malformed"
        assert sync_generation.response_model == "claude-sonnet-4-5-20260210"
        assert sync_generation.output == []
        assert sync_generation.stop_reason == ""

        stream_generation = generations[1]
        assert stream_generation.mode.value == "STREAM"
        assert stream_generation.response_model == "claude-sonnet-4-5"
        assert stream_generation.output == []
    finally:
        client.shutdown()


def test_anthropic_mappers_use_strict_payloads_and_support_raw_artifacts() -> None:
    request = _request()
    response = _response()

    mapped_default = messages.from_request_response(request, response)
    assert mapped_default.response_model == "claude-sonnet-4-5-20260210"
    assert len(mapped_default.input) == 2
    assert mapped_default.input[0].role.value == "user"
    assert mapped_default.input[1].role.value == "tool"
    assert mapped_default.max_tokens == 512
    assert mapped_default.temperature == 0.3
    assert mapped_default.top_p == 0.75
    assert mapped_default.tool_choice == '{"name":"weather","type":"tool"}'
    assert mapped_default.thinking_enabled is True
    assert mapped_default.metadata["sigil.gen_ai.request.thinking.budget_tokens"] == 2048
    assert mapped_default.usage.cache_write_input_tokens == 4
    assert mapped_default.usage.cache_read_input_tokens == 12
    assert mapped_default.artifacts == []

    mapped_with_artifacts = messages.from_request_response(request, response, AnthropicOptions(raw_artifacts=True))
    assert [artifact.kind.value for artifact in mapped_with_artifacts.artifacts] == ["request", "response", "tools"]

    stream_mapped = messages.from_stream(
        request,
        AnthropicStreamSummary(
            output_text="stream-output",
            events=[{"type": "content_block_delta", "delta": {"type": "text_delta", "text": "stream-output"}}],
        ),
        AnthropicOptions(raw_artifacts=True),
    )
    assert stream_mapped.response_model == "claude-sonnet-4-5"
    assert stream_mapped.output[0].parts[0].text == "stream-output"
    assert stream_mapped.max_tokens == 512
    assert stream_mapped.tool_choice == '{"name":"weather","type":"tool"}'
    assert stream_mapped.thinking_enabled is True
    assert stream_mapped.metadata["sigil.gen_ai.request.thinking.budget_tokens"] == 2048
    assert [artifact.kind.value for artifact in stream_mapped.artifacts] == ["request", "tools", "provider_event"]


def test_anthropic_mapper_maps_thinking_disabled() -> None:
    request = {
        "model": "claude-sonnet-4-5",
        "max_tokens": 128,
        "thinking": "disabled",
        "messages": [{"role": "user", "content": "hello"}],
    }
    response = {
        "id": "resp-1",
        "model": "claude-sonnet-4-5",
        "role": "assistant",
        "content": [{"type": "text", "text": "ok"}],
    }
    mapped = messages.from_request_response(request, response)

    assert mapped.thinking_enabled is False


def test_anthropic_provider_explicitly_has_no_embeddings_surface() -> None:
    assert "messages" in agento11y_anthropic.__all__
    assert "embeddings" not in agento11y_anthropic.__all__
    assert not hasattr(agento11y_anthropic, "embeddings")
