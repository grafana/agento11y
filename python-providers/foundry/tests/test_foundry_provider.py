"""Microsoft Foundry provider helper tests."""

from __future__ import annotations

import asyncio
from datetime import timedelta

from sigil_sdk import Client, ClientConfig, GenerationExportConfig
from sigil_sdk.models import ExportGenerationResult, ExportGenerationsResponse
from sigil_sdk_foundry import FoundryOptions, responses


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


class _FakeResponses:
    def __init__(self) -> None:
        self.requests = []

    def create(self, **request):
        self.requests.append(request)
        if request.get("stream"):
            return iter(
                [
                    {"type": "response.output_text.delta", "delta": "hello "},
                    {"type": "response.output_text.delta", "delta": "foundry"},
                    {
                        "type": "response.completed",
                        "response": {
                            "id": "resp-stream",
                            "model": request["model"],
                            "status": "completed",
                            "output": [],
                            "usage": {"input_tokens": 1, "output_tokens": 2, "total_tokens": 3},
                        },
                    },
                ]
            )
        return {
            "id": "resp-sync",
            "model": request["model"],
            "status": "completed",
            "output": [
                {
                    "type": "message",
                    "role": "assistant",
                    "content": [{"type": "output_text", "text": "foundry response"}],
                }
            ],
            "usage": {"input_tokens": 3, "output_tokens": 4, "total_tokens": 7},
        }


class _FakeOpenAIClient:
    def __init__(self) -> None:
        self.responses = _FakeResponses()


class _AsyncStream:
    def __init__(self, events) -> None:
        self._events = list(events)

    def __aiter__(self):
        return self

    async def __anext__(self):
        if not self._events:
            raise StopAsyncIteration
        return self._events.pop(0)


class _AsyncFakeResponses:
    async def create(self, **request):
        if request.get("stream"):
            return _AsyncStream(
                [
                    {"type": "response.output_text.delta", "delta": "async "},
                    {"type": "response.output_text.delta", "delta": "foundry"},
                ]
            )
        return {
            "id": "resp-async",
            "model": request["model"],
            "status": "completed",
            "output": [
                {
                    "type": "message",
                    "role": "assistant",
                    "content": [{"type": "output_text", "text": "async foundry"}],
                }
            ],
        }


class _AsyncFakeOpenAIClient:
    def __init__(self) -> None:
        self.responses = _AsyncFakeResponses()


def _new_client(exporter):
    return Client(
        ClientConfig(
            generation_export=GenerationExportConfig(batch_size=10, flush_interval=timedelta(seconds=60)),
            generation_exporter=exporter,
        )
    )


def test_foundry_responses_create_records_azure_foundry_generation() -> None:
    exporter = _CapturingExporter()
    sigil = _new_client(exporter)
    foundry = _FakeOpenAIClient()

    try:
        response = responses.create(
            sigil,
            foundry,
            {"model": "gpt-5.2", "input": "hello"},
            FoundryOptions(
                conversation_id="conv-foundry",
                agent_name="foundry-agent",
                project_endpoint="https://example.services.ai.azure.com/api/projects/demo",
            ),
        )

        assert response["id"] == "resp-sync"
        assert foundry.responses.requests[0] == {"model": "gpt-5.2", "input": "hello"}

        sigil.flush()
        generation = exporter.requests[0].generations[0]
        assert generation.conversation_id == "conv-foundry"
        assert generation.agent_name == "foundry-agent"
        assert generation.model.provider == "azure_foundry"
        assert generation.model.name == "gpt-5.2"
        assert generation.metadata["azure.foundry.project_endpoint"] == (
            "https://example.services.ai.azure.com/api/projects/demo"
        )
    finally:
        sigil.shutdown()


def test_foundry_responses_stream_collects_openai_compatible_events() -> None:
    exporter = _CapturingExporter()
    sigil = _new_client(exporter)
    foundry = _FakeOpenAIClient()

    try:
        summary = responses.stream(
            sigil,
            foundry,
            {"model": "gpt-5.2", "input": "stream this"},
            FoundryOptions(conversation_id="conv-stream"),
        )

        assert summary.output_text == "hello foundry"
        assert foundry.responses.requests[0]["stream"] is True

        sigil.flush()
        generation = exporter.requests[0].generations[0]
        assert generation.mode.value == "STREAM"
        assert generation.model.provider == "azure_foundry"
        assert generation.output[0].parts[0].text == "hello foundry"
    finally:
        sigil.shutdown()


def test_foundry_responses_async_wrappers_record_generation() -> None:
    async def run() -> None:
        exporter = _CapturingExporter()
        sigil = _new_client(exporter)
        foundry = _AsyncFakeOpenAIClient()

        try:
            response = await responses.create_async(
                sigil,
                foundry,
                {"model": "gpt-5.2", "input": "hello"},
                FoundryOptions(conversation_id="conv-async"),
            )

            assert response["id"] == "resp-async"
            sigil.flush()
            generation = exporter.requests[0].generations[0]
            assert generation.conversation_id == "conv-async"
            assert generation.model.provider == "azure_foundry"
        finally:
            sigil.shutdown()

    asyncio.run(run())


def test_foundry_responses_async_stream_records_generation() -> None:
    async def run() -> None:
        exporter = _CapturingExporter()
        sigil = _new_client(exporter)
        foundry = _AsyncFakeOpenAIClient()

        try:
            summary = await responses.stream_async(
                sigil,
                foundry,
                {"model": "gpt-5.2", "input": "stream"},
                FoundryOptions(conversation_id="conv-async-stream"),
            )

            assert summary.output_text == "async foundry"
            sigil.flush()
            generation = exporter.requests[0].generations[0]
            assert generation.mode.value == "STREAM"
            assert generation.output[0].parts[0].text == "async foundry"
        finally:
            sigil.shutdown()

    asyncio.run(run())
