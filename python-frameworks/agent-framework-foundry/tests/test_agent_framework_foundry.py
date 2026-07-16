"""Microsoft Agent Framework Foundry middleware tests."""

from __future__ import annotations

import asyncio
from datetime import timedelta

from agent_framework import ChatContext, ChatResponse, FunctionInvocationContext, Message, UsageDetails
from sigil_sdk import Client, ClientConfig, GenerationExportConfig
from sigil_sdk.models import ExportGenerationResult, ExportGenerationsResponse
from sigil_sdk_agent_framework_foundry import create_sigil_foundry_middleware, with_sigil_foundry_middleware
from sigil_sdk_agent_framework_foundry.handler import (
    SigilAgentFrameworkFoundryChatMiddleware,
    SigilAgentFrameworkFoundryFunctionMiddleware,
)


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


class _FakeFoundryClient:
    model = "gpt-5.2"
    project_endpoint = "https://example.services.ai.azure.com/api/projects/demo"


class _FakeFunction:
    name = "lookup_status"
    description = "Look up deployment status."


def _new_client(exporter):
    return Client(
        ClientConfig(
            generation_export=GenerationExportConfig(batch_size=10, flush_interval=timedelta(seconds=60)),
            generation_exporter=exporter,
        )
    )


def _chat_middleware(middleware):
    return next(item for item in middleware if isinstance(item, SigilAgentFrameworkFoundryChatMiddleware))


def _function_middleware(middleware):
    return next(item for item in middleware if isinstance(item, SigilAgentFrameworkFoundryFunctionMiddleware))


def test_foundry_chat_middleware_records_generation() -> None:
    async def run() -> None:
        exporter = _CapturingExporter()
        sigil = _new_client(exporter)
        middleware = create_sigil_foundry_middleware(
            client=sigil,
            conversation_id="conv-foundry-agent",
            agent_name="foundry-agent",
        )
        chat = _chat_middleware(middleware)
        context = ChatContext(
            client=_FakeFoundryClient(),
            messages=[Message("user", ["hello foundry"])],
            options={"temperature": 0.2},
        )

        async def call_next() -> None:
            context.result = ChatResponse(
                messages=[Message("assistant", ["hello from foundry"])],
                response_id="resp-foundry",
                model="gpt-5.2",
                finish_reason="stop",
                usage_details=UsageDetails(input_token_count=3, output_token_count=4, total_token_count=7),
            )

        try:
            await chat.process(context, call_next)
            sigil.flush()
            generation = exporter.requests[0].generations[0]
            assert generation.conversation_id == "conv-foundry-agent"
            assert generation.agent_name == "foundry-agent"
            assert generation.model.provider == "azure_foundry"
            assert generation.model.name == "gpt-5.2"
            assert generation.output[0].parts[0].text == "hello from foundry"
            assert generation.usage.input_tokens == 3
            assert generation.tags["sigil.framework.name"] == "agent-framework-foundry"
        finally:
            sigil.shutdown()

    asyncio.run(run())


def test_foundry_function_middleware_records_tool_execution() -> None:
    async def run() -> None:
        exporter = _CapturingExporter()
        sigil = _new_client(exporter)
        middleware = create_sigil_foundry_middleware(client=sigil, conversation_id="conv-tool")
        function = _function_middleware(middleware)
        context = FunctionInvocationContext(_FakeFunction(), {"service": "checkout"})

        async def call_next() -> None:
            context.result = {"status": "green"}

        try:
            await function.process(context, call_next)
            sigil.flush()
            assert exporter.requests == []
        finally:
            sigil.shutdown()

    asyncio.run(run())


def test_with_sigil_foundry_middleware_is_idempotent() -> None:
    exporter = _CapturingExporter()
    sigil = _new_client(exporter)
    try:
        first = with_sigil_foundry_middleware([], client=sigil)
        second = with_sigil_foundry_middleware(first, client=sigil)
        assert second == first
        assert len(first) == 3
    finally:
        sigil.shutdown()


def test_foundry_chat_middleware_records_stream_result() -> None:
    async def run() -> None:
        exporter = _CapturingExporter()
        sigil = _new_client(exporter)
        middleware = create_sigil_foundry_middleware(client=sigil, conversation_id="conv-stream")
        chat = _chat_middleware(middleware)
        context = ChatContext(
            client=_FakeFoundryClient(),
            messages=[Message("user", ["stream"])],
            options={},
            stream=True,
        )

        async def call_next() -> None:
            for hook in context.stream_transform_hooks:
                hook(type("Update", (), {"text": "streamed "})())
                hook(type("Update", (), {"text": "answer"})())
            response = ChatResponse(messages=[Message("assistant", ["streamed answer"])], model="gpt-5.2")
            for hook in context.stream_result_hooks:
                hook(response)
            context.result = response

        try:
            await chat.process(context, call_next)
            sigil.flush()
            generation = exporter.requests[0].generations[0]
            assert generation.mode.value == "STREAM"
            assert generation.output[0].parts[0].text == "streamed answer"
        finally:
            sigil.shutdown()

    asyncio.run(run())
