"""OpenAI provider helper tests."""

from __future__ import annotations

import asyncio
from datetime import timedelta

import pytest
from agento11y import Client, ClientConfig, GenerationExportConfig
from agento11y.models import ExportGenerationResult, ExportGenerationsResponse
from agento11y_openai import (
    ChatCompletionsStreamSummary,
    OpenAIOptions,
    ResponsesStreamSummary,
    chat,
    embeddings,
    responses,
)
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter


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


def _new_client(exporter, tracer=None):
    return Client(
        ClientConfig(
            tracer=tracer,
            generation_export=GenerationExportConfig(batch_size=10, flush_interval=timedelta(seconds=60)),
            generation_exporter=exporter,
        )
    )


def test_chat_sync_wrapper_sets_sync_mode_and_raw_artifacts_default_off() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    try:
        response = chat.completions.create(
            client,
            {
                "model": "gpt-5",
                "max_completion_tokens": 320,
                "reasoning": {"effort": "high", "max_output_tokens": 1024},
                "messages": [
                    {"role": "system", "content": "system"},
                    {"role": "user", "content": "hello"},
                ],
            },
            lambda _request: {
                "id": "resp-1",
                "model": "gpt-5",
                "object": "chat.completion",
                "created": 0,
                "choices": [
                    {
                        "index": 0,
                        "finish_reason": "stop",
                        "message": {"role": "assistant", "content": "world"},
                    }
                ],
                "usage": {
                    "prompt_tokens": 10,
                    "completion_tokens": 5,
                    "total_tokens": 15,
                    "prompt_tokens_details": {"cached_tokens": 1},
                    "completion_tokens_details": {"reasoning_tokens": 2},
                },
            },
        )
        assert response["id"] == "resp-1"

        client.flush()
        generation = exporter.requests[0].generations[0]
        assert generation.mode.value == "SYNC"
        assert generation.model.provider == "openai"
        assert generation.max_tokens == 320
        assert generation.metadata["sigil.gen_ai.request.thinking.budget_tokens"] == 1024
        assert generation.artifacts == []
    finally:
        client.shutdown()


def test_chat_stream_wrapper_sets_stream_mode_and_opt_in_artifacts() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    try:
        summary = chat.completions.stream(
            client,
            {
                "model": "gpt-5",
                "stream": True,
                "messages": [{"role": "user", "content": "hello"}],
            },
            lambda _request: ChatCompletionsStreamSummary(
                output_text="stream-output",
                events=[
                    {
                        "id": "chunk-1",
                        "object": "chat.completion.chunk",
                        "created": 0,
                        "model": "gpt-5",
                        "choices": [{"index": 0, "delta": {"content": "stream-output"}}],
                    }
                ],
            ),
            OpenAIOptions(raw_artifacts=True),
        )
        assert summary.output_text == "stream-output"

        client.flush()
        generation = exporter.requests[0].generations[0]
        assert generation.mode.value == "STREAM"
        assert [artifact.kind.value for artifact in generation.artifacts] == ["request", "provider_event"]
    finally:
        client.shutdown()


def test_responses_sync_wrapper_sets_sync_mode_and_maps_contract_fields() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    try:
        response = responses.create(
            client,
            {
                "model": "gpt-5",
                "instructions": "be concise",
                "input": [
                    {
                        "type": "message",
                        "role": "user",
                        "content": [{"type": "input_text", "text": "hello"}],
                    }
                ],
                "max_output_tokens": 256,
                "temperature": 0.2,
                "top_p": 0.8,
                "tool_choice": {"type": "function", "name": "weather"},
                "reasoning": {"effort": "high", "max_output_tokens": 512},
            },
            lambda _request: {
                "id": "resp-sync",
                "object": "response",
                "model": "gpt-5",
                "status": "completed",
                "output": [
                    {
                        "id": "msg-1",
                        "type": "message",
                        "role": "assistant",
                        "status": "completed",
                        "content": [{"type": "output_text", "text": "world", "annotations": []}],
                    }
                ],
                "parallel_tool_calls": False,
                "temperature": 1,
                "top_p": 1,
                "tools": [],
                "created_at": 0,
                "incomplete_details": None,
                "metadata": {},
                "error": None,
                "usage": {
                    "input_tokens": 20,
                    "output_tokens": 5,
                    "total_tokens": 25,
                    "input_tokens_details": {"cached_tokens": 2},
                    "output_tokens_details": {"reasoning_tokens": 3},
                },
            },
        )

        assert response["id"] == "resp-sync"
        client.flush()
        generation = exporter.requests[0].generations[0]
        assert generation.mode.value == "SYNC"
        assert generation.model.provider == "openai"
        assert generation.max_tokens == 256
        assert generation.temperature == 0.2
        assert generation.top_p == 0.8
        assert generation.stop_reason == "stop"
        assert generation.metadata["sigil.gen_ai.request.thinking.budget_tokens"] == 512
        assert generation.artifacts == []
    finally:
        client.shutdown()


def test_responses_stream_wrapper_maps_events_and_opt_in_artifacts() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    try:
        summary = responses.stream(
            client,
            {
                "model": "gpt-5",
                "stream": True,
                "input": "stream this",
                "max_output_tokens": 128,
            },
            lambda _request: ResponsesStreamSummary(
                events=[
                    {
                        "type": "response.output_text.delta",
                        "sequence_number": 1,
                        "output_index": 0,
                        "item_id": "msg-1",
                        "content_index": 0,
                        "delta": "hello",
                    },
                    {
                        "type": "response.output_text.delta",
                        "sequence_number": 2,
                        "output_index": 0,
                        "item_id": "msg-1",
                        "content_index": 0,
                        "delta": " world",
                    },
                ]
            ),
            OpenAIOptions(raw_artifacts=True),
        )

        assert len(summary.events) == 2
        client.flush()
        generation = exporter.requests[0].generations[0]
        assert generation.mode.value == "STREAM"
        assert generation.output[0].parts[0].text == "hello world"
        assert [artifact.kind.value for artifact in generation.artifacts] == ["request", "provider_event"]
    finally:
        client.shutdown()


def test_openai_wrappers_propagate_provider_error_and_set_call_error() -> None:
    for run in (
        lambda client: chat.completions.create(
            client,
            {"model": "gpt-5", "messages": [{"role": "user", "content": "hello"}]},
            lambda _request: (_ for _ in ()).throw(RuntimeError("provider failure chat")),
        ),
        lambda client: responses.create(
            client,
            {"model": "gpt-5", "input": "hello"},
            lambda _request: (_ for _ in ()).throw(RuntimeError("provider failure responses")),
        ),
    ):
        exporter = _CapturingExporter()
        client = _new_client(exporter)

        try:
            with pytest.raises(RuntimeError, match="provider failure"):
                run(client)

            client.flush()
            generation = exporter.requests[0].generations[0]
            assert generation.model.provider == "openai"
            assert "provider failure" in generation.call_error
        finally:
            client.shutdown()


def test_openai_wrappers_tolerate_missing_provider_payload_fields() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    try:
        chat.completions.create(
            client,
            {"model": "gpt-5", "messages": [{"role": "user", "content": "hello"}]},
            lambda _request: {
                "id": "resp-chat-malformed",
                "model": "gpt-5",
                "object": "chat.completion",
                "created": 0,
                "choices": [],
            },
        )
        responses.stream(
            client,
            {"model": "gpt-5", "stream": True, "input": "stream this"},
            lambda _request: ResponsesStreamSummary(events=[{"type": "response.output_text.delta", "delta": 42}]),
        )

        client.flush()
        generations = exporter.requests[0].generations
        assert len(generations) == 2

        chat_generation = generations[0]
        assert chat_generation.mode.value == "SYNC"
        assert chat_generation.response_id == "resp-chat-malformed"
        assert chat_generation.response_model == "gpt-5"
        assert chat_generation.output == []
        assert chat_generation.stop_reason == ""

        stream_generation = generations[1]
        assert stream_generation.mode.value == "STREAM"
        assert stream_generation.response_model == "gpt-5"
        assert stream_generation.output[0].parts[0].text == "42"
    finally:
        client.shutdown()


def test_embeddings_wrapper_records_span_and_skips_generation_export() -> None:
    exporter = _CapturingExporter()
    span_exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(span_exporter))
    tracer = provider.get_tracer("sigil-test")
    client = _new_client(exporter, tracer=tracer)

    try:
        response = embeddings.create(
            client,
            {
                "model": "text-embedding-3-small",
                "input": ["hello", "world"],
                "dimensions": 256,
                "encoding_format": "float",
            },
            lambda _request: {
                "model": "text-embedding-3-small",
                "usage": {"prompt_tokens": 12},
                "data": [{"embedding": [0.1, 0.2, 0.3]}],
            },
        )
        assert response["model"] == "text-embedding-3-small"

        client.flush()
        assert exporter.requests == []
        span = span_exporter.get_finished_spans()[-1]
        assert span.attributes["gen_ai.operation.name"] == "embeddings"
        assert span.attributes["gen_ai.provider.name"] == "openai"
        assert span.attributes["gen_ai.request.model"] == "text-embedding-3-small"
        assert span.attributes["gen_ai.embeddings.input_count"] == 2
        assert span.attributes["gen_ai.usage.input_tokens"] == 12
        assert span.attributes["gen_ai.embeddings.dimension.count"] == 3
        assert span.attributes["gen_ai.request.encoding_formats"] == ("float",)
    finally:
        client.shutdown()
        provider.shutdown()


def test_embeddings_wrapper_counts_tokenized_single_input_as_one_item() -> None:
    exporter = _CapturingExporter()
    span_exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(span_exporter))
    tracer = provider.get_tracer("sigil-test")
    client = _new_client(exporter, tracer=tracer)

    try:
        embeddings.create(
            client,
            {
                "model": "text-embedding-3-small",
                "input": [101, 102, 103, 104],
            },
            lambda _request: {
                "model": "text-embedding-3-small",
                "usage": {"prompt_tokens": 4},
                "data": [{"embedding": [0.1, 0.2]}],
            },
        )

        client.flush()
        assert exporter.requests == []
        span = span_exporter.get_finished_spans()[-1]
        assert span.attributes["gen_ai.embeddings.input_count"] == 1
        assert span.attributes["gen_ai.usage.input_tokens"] == 4
    finally:
        client.shutdown()
        provider.shutdown()


def test_embeddings_wrapper_propagates_provider_error_and_sets_span_error() -> None:
    exporter = _CapturingExporter()
    span_exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(span_exporter))
    tracer = provider.get_tracer("sigil-test")
    client = _new_client(exporter, tracer=tracer)

    try:
        with pytest.raises(RuntimeError, match="provider failure embedding"):
            embeddings.create(
                client,
                {
                    "model": "text-embedding-3-small",
                    "input": "hello",
                },
                lambda _request: (_ for _ in ()).throw(RuntimeError("provider failure embedding")),
            )

        span = span_exporter.get_finished_spans()[-1]
        assert span.status.status_code.name == "ERROR"
        assert span.attributes.get("error.type") == "provider_call_error"
    finally:
        client.shutdown()
        provider.shutdown()


def test_chat_mapper_filters_system_messages_and_supports_raw_artifacts() -> None:
    request = {
        "model": "gpt-5",
        "max_completion_tokens": 320,
        "max_tokens": 999,
        "temperature": 0.2,
        "top_p": 0.85,
        "tool_choice": {"type": "function", "function": {"name": "weather"}},
        "reasoning": {"effort": "high", "max_output_tokens": 2048},
        "messages": [
            {"role": "system", "content": "system"},
            {"role": "developer", "content": "developer"},
            {"role": "user", "content": "hello"},
            {"role": "tool", "tool_call_id": "call_weather", "content": '{"ok":true}', "name": "tool-weather"},
        ],
        "tools": [
            {
                "type": "function",
                "function": {
                    "name": "weather",
                    "description": "lookup weather",
                    "parameters": {"type": "object"},
                },
            }
        ],
    }
    response = {
        "id": "resp-openai",
        "model": "gpt-5",
        "object": "chat.completion",
        "created": 0,
        "choices": [
            {
                "index": 0,
                "finish_reason": "tool_calls",
                "message": {
                    "role": "assistant",
                    "content": "world",
                    "tool_calls": [
                        {
                            "id": "call_weather",
                            "type": "function",
                            "function": {"name": "weather", "arguments": '{"city":"Paris"}'},
                        }
                    ],
                },
            }
        ],
    }

    mapped_default = chat.completions.from_request_response(request, response)
    assert mapped_default.response_model == "gpt-5"
    assert len(mapped_default.input) == 2
    assert mapped_default.input[0].role.value == "user"
    assert mapped_default.input[1].role.value == "tool"
    assert mapped_default.input[1].parts[0].kind.value == "tool_result"
    assert mapped_default.input[1].parts[0].tool_result.tool_call_id == "call_weather"
    assert mapped_default.input[1].parts[0].tool_result.name == "tool-weather"
    assert mapped_default.input[1].parts[0].tool_result.content == '{"ok":true}'
    assert mapped_default.max_tokens == 320
    assert mapped_default.temperature == 0.2
    assert mapped_default.top_p == 0.85
    assert mapped_default.thinking_enabled is True
    assert mapped_default.metadata["sigil.gen_ai.request.thinking.budget_tokens"] == 2048
    assert mapped_default.artifacts == []

    mapped_with_artifacts = chat.completions.from_request_response(
        request,
        response,
        OpenAIOptions(raw_artifacts=True),
    )
    assert [artifact.kind.value for artifact in mapped_with_artifacts.artifacts] == ["request", "response", "tools"]


def test_responses_mapper_maps_output_and_stream_fallback() -> None:
    request = {
        "model": "gpt-5",
        "instructions": "Be concise",
        "input": [
            {
                "type": "message",
                "role": "user",
                "content": [{"type": "input_text", "text": "hello"}],
            },
            {
                "type": "function_call_output",
                "call_id": "call_weather",
                "name": "weather",
                "output": {"temp_c": 18},
            },
        ],
        "max_output_tokens": 300,
        "reasoning": {"effort": "medium", "max_output_tokens": 640},
    }
    response = {
        "id": "resp-1",
        "object": "response",
        "model": "gpt-5",
        "status": "completed",
        "output": [
            {
                "id": "msg-1",
                "type": "message",
                "role": "assistant",
                "status": "completed",
                "content": [{"type": "output_text", "text": "world", "annotations": []}],
            },
            {
                "id": "call-1",
                "type": "function_call",
                "call_id": "call_weather",
                "name": "weather",
                "arguments": '{"city":"Paris"}',
            },
            {
                "id": "result-1",
                "type": "function_call_output",
                "call_id": "call_weather",
                "name": "weather",
                "output": {"temp_c": 18},
            },
        ],
        "parallel_tool_calls": False,
        "temperature": 1,
        "top_p": 1,
        "tools": [],
        "created_at": 0,
        "incomplete_details": None,
        "metadata": {},
        "error": None,
        "usage": {
            "input_tokens": 80,
            "output_tokens": 20,
            "total_tokens": 100,
            "input_tokens_details": {"cached_tokens": 2},
            "output_tokens_details": {"reasoning_tokens": 3},
        },
    }

    mapped = responses.from_request_response(request, response)
    assert mapped.response_model == "gpt-5"
    assert len(mapped.input) == 2
    assert mapped.input[1].role.value == "tool"
    assert mapped.input[1].parts[0].kind.value == "tool_result"
    assert mapped.input[1].parts[0].tool_result.tool_call_id == "call_weather"
    assert mapped.input[1].parts[0].tool_result.content_json == b'{"temp_c":18}'
    assert mapped.max_tokens == 300
    assert mapped.stop_reason == "stop"
    assert mapped.thinking_enabled is True
    assert mapped.metadata["sigil.gen_ai.request.thinking.budget_tokens"] == 640
    assert mapped.usage.total_tokens == 100
    assert len(mapped.output) == 3
    assert mapped.output[2].role.value == "tool"
    assert mapped.output[2].parts[0].kind.value == "tool_result"
    assert mapped.output[2].parts[0].tool_result.tool_call_id == "call_weather"
    assert mapped.output[2].parts[0].tool_result.content_json == b'{"temp_c":18}'

    streamed = responses.from_stream(
        {**request, "stream": True},
        ResponsesStreamSummary(
            events=[
                {
                    "type": "response.output_text.delta",
                    "sequence_number": 1,
                    "output_index": 0,
                    "item_id": "msg-1",
                    "content_index": 0,
                    "delta": "delta-one",
                },
                {
                    "type": "response.output_text.delta",
                    "sequence_number": 2,
                    "output_index": 0,
                    "item_id": "msg-1",
                    "content_index": 0,
                    "delta": " delta-two",
                },
            ]
        ),
        OpenAIOptions(raw_artifacts=True),
    )

    assert streamed.response_model == "gpt-5"
    assert streamed.output[0].parts[0].text == "delta-one delta-two"
    assert [artifact.kind.value for artifact in streamed.artifacts] == ["request", "provider_event"]


def test_embeddings_mapper_extracts_input_counts_tokens_and_dimensions() -> None:
    mapped = embeddings.from_request_response(
        {
            "model": "text-embedding-3-small",
            "input": ["hello", {"text": "world"}, [1, 2, 3]],
        },
        {
            "model": "text-embedding-3-small",
            "usage": {"prompt_tokens": 30},
            "data": [{"embedding": [0.1, 0.2, 0.3]}],
        },
    )

    assert mapped.input_count == 3
    assert mapped.input_texts == ["hello", "world"]
    assert mapped.input_tokens == 30
    assert mapped.response_model == "text-embedding-3-small"
    assert mapped.dimensions == 3

    tokenized_single = embeddings.from_request_response(
        {
            "model": "text-embedding-3-small",
            "input": [101, 102, 103],
        },
        {
            "model": "text-embedding-3-small",
            "usage": {"prompt_tokens": 3},
            "data": [{"embedding": [0.1, 0.2]}],
        },
    )
    assert tokenized_single.input_count == 1
    assert tokenized_single.input_texts == []
    assert tokenized_single.input_tokens == 3


def test_async_wrappers_record_generation() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    async def run() -> None:
        await chat.completions.create_async(
            client,
            {"model": "gpt-5", "messages": [{"role": "user", "content": "hello"}]},
            lambda _request: _async_value(
                {
                    "id": "async-chat",
                    "model": "gpt-5",
                    "object": "chat.completion",
                    "created": 0,
                    "choices": [
                        {
                            "index": 0,
                            "finish_reason": "stop",
                            "message": {"role": "assistant", "content": "ok"},
                        }
                    ],
                }
            ),
        )

        await responses.stream_async(
            client,
            {"model": "gpt-5", "stream": True, "input": "hello"},
            lambda _request: _async_value(
                ResponsesStreamSummary(
                    events=[
                        {
                            "type": "response.output_text.delta",
                            "sequence_number": 1,
                            "output_index": 0,
                            "item_id": "msg-1",
                            "content_index": 0,
                            "delta": "ok",
                        }
                    ]
                )
            ),
        )

    try:
        asyncio.run(run())
        client.flush()
        generations = exporter.requests[0].generations
        assert len(generations) == 2
        assert generations[0].mode.value == "SYNC"
        assert generations[1].mode.value == "STREAM"
    finally:
        client.shutdown()


async def _async_value(value):
    return value
