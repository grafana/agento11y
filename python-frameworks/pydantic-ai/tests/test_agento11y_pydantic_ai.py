"""pydantic-ai handler lifecycle and conversation-mapping tests."""

from __future__ import annotations

import asyncio
import json
from collections.abc import AsyncIterator, Sequence
from datetime import timedelta
from typing import Any
from uuid import UUID, uuid4

import pydantic_ai
import pytest
from agento11y import (
    Client,
    ClientConfig,
    GenerationExportConfig,
    Message,
    MessageRole,
    Part,
    PartKind,
    ToolCall,
    ToolResult,
)
from agento11y.models import ExportGenerationResult, ExportGenerationsResponse
from agento11y_pydantic_ai import (
    Agento11yPydanticAICapability,
    Agento11yPydanticAIHandler,
    create_agento11y_pydantic_ai_capability,
    create_agento11y_pydantic_ai_handler,
    with_agento11y_pydantic_ai_capability,
)
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.metrics.export import InMemoryMetricReader
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
from opentelemetry.trace import StatusCode
from packaging.version import Version
from pydantic import BaseModel
from pydantic_ai import Agent, ModelRetry, ToolOutput
from pydantic_ai.capabilities import Thinking
from pydantic_ai.messages import (
    ImageUrl,
    ModelMessage,
    ModelRequest,
    ModelResponse,
    TextPart,
    ThinkingPart,
    ToolCallPart,
    UserPromptPart,
)
from pydantic_ai.models import ModelRequestContext, ModelRequestParameters
from pydantic_ai.models.function import AgentInfo, DeltaThinkingPart, FunctionModel
from pydantic_ai.models.test import TestModel
from pydantic_ai.tools import RunContext
from pydantic_ai.tools import ToolDefinition as PydanticToolDefinition
from pydantic_ai.usage import RequestUsage, RunUsage


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


def _new_client(exporter: _CapturingExporter, tracer=None) -> Client:
    return Client(
        ClientConfig(
            tracer=tracer,
            generation_export=GenerationExportConfig(batch_size=10, flush_interval=timedelta(seconds=60)),
            generation_exporter=exporter,
        )
    )


def _run_context(run_id: str, *, agent: Agent[Any, Any] | None = None) -> RunContext[Any]:
    return RunContext(deps=None, model=TestModel(model_name="gpt-5"), usage=RunUsage(), run_id=run_id, agent=agent)


def _request_context(parameters: ModelRequestParameters | None = None) -> ModelRequestContext:
    return ModelRequestContext(
        model=TestModel(model_name="gpt-5"),
        messages=[ModelRequest(parts=[UserPromptPart(content="hello")])],
        model_settings=None,
        model_request_parameters=parameters or ModelRequestParameters(),
    )


def _weather_tool_call() -> tuple[ToolCallPart, PydanticToolDefinition]:
    call = ToolCallPart(tool_name="weather_tool", args={"city": "Paris"}, tool_call_id="call-1")
    return call, PydanticToolDefinition(name="weather_tool", description="Get weather info")


def test_agento11y_pydantic_ai_sync_lifecycle_sets_framework_metadata() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    try:
        run_id = uuid4()
        parent_run_id = uuid4()
        handler = Agento11yPydanticAIHandler(client=client, provider_resolver="auto")

        handler.on_chat_model_start(
            {"name": "ChatModel"},
            [[{"type": "human", "content": "hello"}]],
            run_id=run_id,
            parent_run_id=parent_run_id,
            tags=["prod"],
            invocation_params={"model": "gpt-5", "retry_attempt": 2, "session_id": "session-invocation"},
            metadata={
                "conversation_id": "framework-conversation-42",
                "thread_id": "framework-thread-42",
                "event_id": "framework-event-42",
            },
        )
        handler.on_llm_end(
            {
                "generations": [[{"text": "world"}]],
                "llm_output": {
                    "model_name": "gpt-5",
                    "finish_reason": "stop",
                },
            },
            run_id=run_id,
        )

        client.flush()
        generation = exporter.requests[0].generations[0]
        assert generation.tags["agento11y.framework.name"] == "pydantic-ai"
        assert generation.tags["agento11y.framework.source"] == "handler"
        assert generation.tags["agento11y.framework.language"] == "python"
        assert generation.conversation_id == "framework-conversation-42"
        assert generation.metadata["agento11y.framework.run_id"] == str(run_id)
        assert generation.metadata["agento11y.framework.parent_run_id"] == str(parent_run_id)
        assert generation.metadata["agento11y.framework.thread_id"] == "framework-thread-42"
        assert generation.metadata["agento11y.framework.event_id"] == "framework-event-42"
        assert generation.metadata["agento11y.framework.run_type"] == "chat"
        assert generation.metadata["agento11y.framework.retry_attempt"] == 2
        assert generation.output[0].parts[0].text == "world"
    finally:
        client.shutdown()


def test_agento11y_pydantic_ai_keeps_thread_metadata_when_ids_are_split_across_payloads() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    try:
        run_id = uuid4()
        handler = Agento11yPydanticAIHandler(client=client, provider_resolver="auto")

        handler.on_chat_model_start(
            {"name": "ChatModel"},
            [[{"type": "human", "content": "hello"}]],
            run_id=run_id,
            invocation_params={"model": "gpt-5"},
            metadata={
                "conversation_id": "framework-conversation-split-42",
                "event_id": "framework-event-split-42",
            },
            thread_id="framework-thread-split-42",
        )
        handler.on_llm_end(
            {
                "generations": [[{"text": "world"}]],
                "llm_output": {"model_name": "gpt-5"},
            },
            run_id=run_id,
        )

        client.flush()
        generation = exporter.requests[0].generations[0]
        assert generation.conversation_id == "framework-conversation-split-42"
        assert generation.metadata["agento11y.framework.thread_id"] == "framework-thread-split-42"
        assert generation.metadata["agento11y.framework.event_id"] == "framework-event-split-42"
    finally:
        client.shutdown()


def test_agento11y_pydantic_ai_fallback_conversation_is_deterministic() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    try:
        run_id = uuid4()
        handler = Agento11yPydanticAIHandler(client=client)

        handler.on_llm_start(
            {"kwargs": {"model": "gpt-5"}},
            ["hello"],
            run_id=run_id,
            invocation_params={"model": "gpt-5"},
        )
        handler.on_llm_end({"generations": [[{"text": "ok"}]], "llm_output": {"model_name": "gpt-5"}}, run_id=run_id)

        client.flush()
        generation = exporter.requests[0].generations[0]
        assert generation.conversation_id == f"agento11y:framework:pydantic-ai:{run_id}"
    finally:
        client.shutdown()


def test_agento11y_pydantic_ai_stream_mode_uses_chunks_when_output_missing() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    try:
        run_id = uuid4()
        handler = Agento11yPydanticAIHandler(client=client)

        handler.on_llm_start(
            {"kwargs": {"model": "claude-sonnet-4-5"}},
            ["stream this"],
            run_id=run_id,
            invocation_params={"stream": True, "model": "claude-sonnet-4-5"},
        )
        handler.on_llm_new_token("hello", run_id=run_id)
        handler.on_llm_new_token(" world", run_id=run_id)
        handler.on_llm_end({"llm_output": {"model_name": "claude-sonnet-4-5"}}, run_id=run_id)

        client.flush()
        generation = exporter.requests[0].generations[0]
        assert generation.mode.value == "STREAM"
        assert generation.model.provider == "anthropic"
        assert generation.output[0].parts[0].text == "hello world"
    finally:
        client.shutdown()


def test_agento11y_pydantic_ai_generation_span_tracks_active_parent_span_and_export_lineage() -> None:
    exporter = _CapturingExporter()
    span_exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(span_exporter))
    tracer = provider.get_tracer("agento11y-framework-test")
    client = _new_client(exporter, tracer=tracer)

    try:
        run_id = uuid4()
        with tracer.start_as_current_span("framework.request"):
            handler = Agento11yPydanticAIHandler(client=client, provider_resolver="auto")
            handler.on_chat_model_start(
                {"name": "ChatModel"},
                [[{"type": "human", "content": "hello"}]],
                run_id=run_id,
                parent_run_id=uuid4(),
                invocation_params={"model": "gpt-5"},
                metadata={
                    "conversation_id": "framework-conversation-lineage-42",
                    "thread_id": "framework-thread-lineage-42",
                },
            )
            handler.on_llm_end(
                {"generations": [[{"text": "world"}]], "llm_output": {"model_name": "gpt-5", "finish_reason": "stop"}},
                run_id=run_id,
            )

        client.flush()
        generation = exporter.requests[0].generations[0]
        spans = span_exporter.get_finished_spans()
        parent_span = next(span for span in spans if span.name == "framework.request")
        generation_span = next(span for span in spans if span.attributes.get("gen_ai.operation.name") == "generateText")

        assert generation_span.parent is not None
        assert generation_span.parent.span_id == parent_span.context.span_id
        assert generation_span.context.trace_id == parent_span.context.trace_id
        assert generation.trace_id == generation_span.context.trace_id.to_bytes(16, "big").hex()
        assert generation.span_id == generation_span.context.span_id.to_bytes(8, "big").hex()
    finally:
        client.shutdown()
        provider.shutdown()


def test_agento11y_pydantic_ai_handler_records_generation_from_async_context() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    async def _run() -> None:
        run_id = uuid4()
        handler = Agento11yPydanticAIHandler(client=client)
        handler.on_llm_start({}, ["hello"], run_id=run_id, invocation_params={"model": "gpt-5"})
        handler.on_llm_end({"generations": [[{"text": "world"}]]}, run_id=run_id)

    try:
        asyncio.run(_run())
        client.flush()
        generation = exporter.requests[0].generations[0]
        assert generation.tags["agento11y.framework.name"] == "pydantic-ai"
        assert generation.model.provider == "openai"
    finally:
        client.shutdown()


def test_agento11y_pydantic_ai_capability_wrap_model_request() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    try:
        capability = create_agento11y_pydantic_ai_capability(client=client, provider_resolver="auto")

        async def handler(rc: ModelRequestContext) -> ModelResponse:
            return ModelResponse(
                parts=[TextPart(content="world")],
                model_name="gpt-5",
                finish_reason="stop",
                usage=RequestUsage(input_tokens=10, output_tokens=5, cache_read_tokens=3, cache_write_tokens=2),
            )

        asyncio.run(
            capability.wrap_model_request(
                _run_context("pydantic-run-42"), request_context=_request_context(), handler=handler
            )
        )
        client.flush()
        generation = exporter.requests[0].generations[0]
        assert generation.tags["agento11y.framework.name"] == "pydantic-ai"
        assert generation.model.name == "gpt-5"
        assert generation.model.provider == "openai"
        assert generation.conversation_id == "agento11y:framework:pydantic-ai:pydantic-run-42"
        assert generation.input == [_text(MessageRole.USER, "hello")]
        assert generation.output == [_text(MessageRole.ASSISTANT, "world")]
        assert generation.stop_reason == "stop"
        assert generation.usage.input_tokens == 10
        assert generation.usage.output_tokens == 5
        assert generation.usage.cache_read_input_tokens == 3
        assert generation.usage.cache_write_input_tokens == 2
        assert generation.usage.total_tokens == 15
    finally:
        client.shutdown()


def test_agento11y_pydantic_ai_agent_run_records_model_settings_and_function_tools() -> None:
    def lookup_city(city: str) -> str:
        """Lookup a city."""
        return city

    def model(messages: list[ModelMessage], info: AgentInfo) -> ModelResponse:
        return ModelResponse(parts=[TextPart(content="Done.")])

    generations, _ = _record_agent_run(
        model,
        "Go.",
        tools=[lookup_city],
        model_settings={"temperature": 0.2, "max_tokens": 256, "top_p": 0.9},
    )

    (generation,) = generations
    assert (generation.temperature, generation.max_tokens, generation.top_p) == (0.2, 256, 0.9)
    (tool,) = generation.tools
    assert (tool.name, tool.description, tool.type) == ("lookup_city", "Lookup a city.", "function")
    assert json.loads(tool.input_schema_json)["required"] == ["city"]


def test_agento11y_pydantic_ai_capability_wrap_tool_execute() -> None:
    class _CapturingHandler:
        def __init__(self) -> None:
            self.started: list[UUID] = []
            self.ended: list[UUID] = []

        def on_tool_start(self, *args, **kwargs) -> None:
            del args
            run_id = kwargs.get("run_id")
            if isinstance(run_id, UUID):
                self.started.append(run_id)

        def on_tool_end(self, *args, **kwargs) -> None:
            del args
            run_id = kwargs.get("run_id")
            if isinstance(run_id, UUID):
                self.ended.append(run_id)

    capture = _CapturingHandler()
    capability = Agento11yPydanticAICapability(capture)  # type: ignore[arg-type]
    call, tool_def = _weather_tool_call()

    async def handler(args: Any) -> Any:
        return {"temperature": 22}

    asyncio.run(
        capability.wrap_tool_execute(
            _run_context("pydantic-tool-run-42"), call=call, tool_def=tool_def, args={"city": "Paris"}, handler=handler
        )
    )

    assert len(capture.started) == 1
    assert len(capture.ended) == 1
    assert capture.started[0] == capture.ended[0]


def test_agento11y_pydantic_ai_capability_wrap_tool_execute_records_arguments() -> None:
    exporter = _CapturingExporter()
    span_exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(span_exporter))
    tracer = provider.get_tracer("agento11y-pydantic-ai-tool-args")
    client = _new_client(exporter, tracer=tracer)
    call, tool_def = _weather_tool_call()

    try:
        capability = create_agento11y_pydantic_ai_capability(client=client)

        async def handler(args: Any) -> Any:
            return {"temperature": 22}

        asyncio.run(
            capability.wrap_tool_execute(
                _run_context("pydantic-tool-args-42"),
                call=call,
                tool_def=tool_def,
                args={"city": "Paris"},
                handler=handler,
            )
        )
        client.flush()

        spans = span_exporter.get_finished_spans()
        tool_span = next(span for span in spans if span.attributes.get("gen_ai.operation.name") == "execute_tool")
        assert tool_span.attributes.get("gen_ai.tool.name") == "weather_tool"
        arguments = tool_span.attributes.get("gen_ai.tool.call.arguments")
        assert isinstance(arguments, str)
        assert "Paris" in arguments
        assert "city" in arguments
    finally:
        client.shutdown()
        provider.shutdown()


def test_agento11y_pydantic_ai_capability_wrap_run_chain_spans() -> None:
    class _CapturingHandler:
        def __init__(self) -> None:
            self.starts: list[dict[str, object]] = []
            self.ends: list[dict[str, object]] = []

        def on_chain_start(self, *args, **kwargs) -> None:
            self.starts.append({"args": args, "kwargs": kwargs})

        def on_chain_end(self, *args, **kwargs) -> None:
            self.ends.append({"args": args, "kwargs": kwargs})

    capture = _CapturingHandler()
    capability = Agento11yPydanticAICapability(capture)  # type: ignore[arg-type]

    async def handler() -> Any:
        return "ok"

    asyncio.run(capability.wrap_run(_run_context("pydantic-chain-run-42"), handler=handler))

    assert len(capture.starts) == 1
    assert len(capture.ends) == 1
    start_kwargs = capture.starts[0]["kwargs"]
    assert start_kwargs["run_type"] == "agent"
    # A run of an agent without a name is recorded under its model's.
    assert start_kwargs["run_name"] == "gpt-5"


def test_agento11y_pydantic_ai_factory_function_return_types() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    try:
        handler = create_agento11y_pydantic_ai_handler(client=client)
        assert isinstance(handler, Agento11yPydanticAIHandler)

        capability = create_agento11y_pydantic_ai_capability(client=client)
        assert isinstance(capability, Agento11yPydanticAICapability)
    finally:
        client.shutdown()


def test_agento11y_pydantic_ai_double_injection_guard() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    try:
        caps = with_agento11y_pydantic_ai_capability(None, client=client)
        assert len(caps) == 1
        assert isinstance(caps[0], Agento11yPydanticAICapability)

        caps2 = with_agento11y_pydantic_ai_capability(caps, client=client)
        assert len(caps2) == 1
        assert caps2[0] is caps[0]
    finally:
        client.shutdown()


def test_agento11y_pydantic_ai_capability_for_run_returns_isolated_capability() -> None:
    class _CapturingHandler:
        pass

    capability = Agento11yPydanticAICapability(_CapturingHandler())  # type: ignore[arg-type]

    async def _run() -> Agento11yPydanticAICapability:
        return await capability.for_run(object())

    run_capability = asyncio.run(_run())

    assert isinstance(run_capability, Agento11yPydanticAICapability)
    assert run_capability is not capability
    assert run_capability._agento11y_handler is capability._agento11y_handler


def test_agento11y_pydantic_ai_capability_control_flow_exceptions_do_not_record_errors() -> None:
    from pydantic_ai.exceptions import ModelRetry, SkipModelRequest, SkipToolExecution

    exporter = _CapturingExporter()
    span_exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(span_exporter))
    client = _new_client(exporter, tracer=provider.get_tracer("agento11y-pydantic-ai-control-flow"))
    ctx = _run_context("pydantic-control-flow-42")
    call, tool_def = _weather_tool_call()

    try:
        handler = Agento11yPydanticAIHandler(client=client)
        capability = Agento11yPydanticAICapability(handler)

        async def model_handler(rc: ModelRequestContext) -> ModelResponse:
            raise SkipModelRequest(ModelResponse(parts=[TextPart(content="cached")]))

        async def tool_handler(args: Any) -> Any:
            raise SkipToolExecution("cached")

        async def run_handler() -> Any:
            with pytest.raises(SkipModelRequest):
                await capability.wrap_model_request(ctx, request_context=_request_context(), handler=model_handler)
            with pytest.raises(SkipToolExecution):
                await capability.wrap_tool_execute(ctx, call=call, tool_def=tool_def, args={}, handler=tool_handler)
            raise ModelRetry("retry the agent")

        with pytest.raises(ModelRetry):
            asyncio.run(capability.wrap_run(ctx, handler=run_handler))
        client.flush()

        assert handler._chain_spans == {}
        assert handler._runs == {}
        assert handler._tool_runs == {}
        assert handler._run_to_graph_key == {}
        assert exporter.requests == []
        assert [span.status.status_code for span in span_exporter.get_finished_spans()] == [StatusCode.OK] * 3
    finally:
        client.shutdown()
        provider.shutdown()


def test_agento11y_pydantic_ai_capability_wrap_model_request_error_records_llm_error() -> None:
    class _CapturingHandler:
        def __init__(self) -> None:
            self.started: list[UUID] = []
            self.errors: list[tuple[UUID, BaseException]] = []
            self.ended: list[UUID] = []

        def on_chat_model_start(self, *args, **kwargs) -> None:
            run_id = kwargs.get("run_id")
            if isinstance(run_id, UUID):
                self.started.append(run_id)

        def on_llm_end(self, *args, **kwargs) -> None:
            run_id = kwargs.get("run_id")
            if isinstance(run_id, UUID):
                self.ended.append(run_id)

        def on_llm_error(self, error, *args, **kwargs) -> None:
            run_id = kwargs.get("run_id")
            if isinstance(run_id, UUID):
                self.errors.append((run_id, error))

    capture = _CapturingHandler()
    capability = Agento11yPydanticAICapability(capture)  # type: ignore[arg-type]

    async def handler(rc: ModelRequestContext) -> ModelResponse:
        raise ValueError("model failed")

    with pytest.raises(ValueError, match="model failed"):
        asyncio.run(
            capability.wrap_model_request(
                _run_context("pydantic-error-run-42"), request_context=_request_context(), handler=handler
            )
        )

    assert len(capture.started) == 1
    assert len(capture.errors) == 1
    assert len(capture.ended) == 0
    assert capture.started[0] == capture.errors[0][0]
    assert str(capture.errors[0][1]) == "model failed"


def test_agento11y_pydantic_ai_capability_wrap_tool_execute_error_records_tool_error() -> None:
    class _CapturingHandler:
        def __init__(self) -> None:
            self.started: list[UUID] = []
            self.errors: list[tuple[UUID, BaseException]] = []
            self.ended: list[UUID] = []

        def on_tool_start(self, *args, **kwargs) -> None:
            run_id = kwargs.get("run_id")
            if isinstance(run_id, UUID):
                self.started.append(run_id)

        def on_tool_end(self, *args, **kwargs) -> None:
            run_id = kwargs.get("run_id")
            if isinstance(run_id, UUID):
                self.ended.append(run_id)

        def on_tool_error(self, error, *args, **kwargs) -> None:
            run_id = kwargs.get("run_id")
            if isinstance(run_id, UUID):
                self.errors.append((run_id, error))

    capture = _CapturingHandler()
    capability = Agento11yPydanticAICapability(capture)  # type: ignore[arg-type]
    call, tool_def = _weather_tool_call()

    async def handler(args: Any) -> Any:
        raise RuntimeError("tool exploded")

    with pytest.raises(RuntimeError, match="tool exploded"):
        asyncio.run(
            capability.wrap_tool_execute(
                _run_context("pydantic-tool-error-42"), call=call, tool_def=tool_def, args={"x": 1}, handler=handler
            )
        )

    assert len(capture.started) == 1
    assert len(capture.errors) == 1
    assert len(capture.ended) == 0
    assert capture.started[0] == capture.errors[0][0]
    assert str(capture.errors[0][1]) == "tool exploded"


def test_agento11y_pydantic_ai_capability_wrap_run_error_records_chain_error() -> None:
    class _CapturingHandler:
        def __init__(self) -> None:
            self.starts: list[UUID] = []
            self.errors: list[tuple[UUID, BaseException]] = []
            self.ends: list[UUID] = []

        def on_chain_start(self, *args, **kwargs) -> None:
            run_id = kwargs.get("run_id")
            if isinstance(run_id, UUID):
                self.starts.append(run_id)

        def on_chain_end(self, *args, **kwargs) -> None:
            run_id = kwargs.get("run_id")
            if isinstance(run_id, UUID):
                self.ends.append(run_id)

        def on_chain_error(self, error, *args, **kwargs) -> None:
            run_id = kwargs.get("run_id")
            if isinstance(run_id, UUID):
                self.errors.append((run_id, error))

    capture = _CapturingHandler()
    capability = Agento11yPydanticAICapability(capture)  # type: ignore[arg-type]

    async def handler() -> Any:
        raise RuntimeError("agent crashed")

    with pytest.raises(RuntimeError, match="agent crashed"):
        asyncio.run(capability.wrap_run(_run_context("pydantic-chain-error-42"), handler=handler))

    assert len(capture.starts) == 1
    assert len(capture.errors) == 1
    assert len(capture.ends) == 0
    assert capture.starts[0] == capture.errors[0][0]
    assert str(capture.errors[0][1]) == "agent crashed"


def test_agento11y_pydantic_ai_handler_explicitly_has_no_embedding_lifecycle() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        handler = Agento11yPydanticAIHandler(client=client)
        assert not hasattr(handler, "on_embedding_start")
        assert not hasattr(handler, "on_embedding_end")
        assert not hasattr(handler, "on_embedding_error")
    finally:
        client.shutdown()


def _record_agent_run(
    model_function: Any,
    prompt: Any,
    *,
    capabilities: Sequence[Any] = (),
    run: dict[str, Any] | None = None,
    **agent_kwargs: Any,
) -> tuple[list[Any], list[Any]]:
    """Run a real agent on a FunctionModel; return its exported generations and finished spans.

    ``capabilities`` come ahead of the agento11y one, and ``run`` holds keyword arguments of the
    run itself, such as its ``conversation_id``.
    """
    exporter = _CapturingExporter()
    span_exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(span_exporter))
    client = _new_client(exporter, tracer=provider.get_tracer("agento11y-pydantic-ai-agent-run"))
    try:
        agent = Agent(
            FunctionModel(model_function),
            capabilities=[*capabilities, create_agento11y_pydantic_ai_capability(client=client)],
            **agent_kwargs,
        )
        agent.run_sync(prompt, **(run or {}))
        client.flush()
    finally:
        client.shutdown()
        provider.shutdown()
    generations = [generation for request in exporter.requests for generation in request.generations]
    return generations, list(span_exporter.get_finished_spans())


def _text(role: MessageRole, text: str) -> Message:
    return Message(role=role, parts=[Part(kind=PartKind.TEXT, text=text)])


def _tool_call(part: Part) -> tuple[str, str, Any]:
    assert part.kind == PartKind.TOOL_CALL and part.tool_call is not None
    return part.tool_call.id, part.tool_call.name, json.loads(part.tool_call.input_json)


def test_agento11y_pydantic_ai_agent_run_records_tool_calls_results_and_thinking() -> None:
    def get_weather(city: str) -> str:
        return f"Sunny in {city}"

    def model(messages: list[ModelMessage], info: AgentInfo) -> ModelResponse:
        if len(messages) == 1:
            return ModelResponse(
                parts=[
                    ThinkingPart(content="The user wants Paris weather."),
                    ToolCallPart(tool_name="get_weather", args={"city": "Paris"}, tool_call_id="call-1"),
                ]
            )
        return ModelResponse(parts=[TextPart(content="It is sunny in Paris.")])

    generations, spans = _record_agent_run(model, "Weather in Paris?", tools=[get_weather])

    assert [generation.mode.value for generation in generations] == ["SYNC", "SYNC"]
    calling, answering = generations
    assistant_turn = Message(
        role=MessageRole.ASSISTANT,
        parts=[
            Part(kind=PartKind.THINKING, thinking="The user wants Paris weather."),
            Part(
                kind=PartKind.TOOL_CALL,
                tool_call=ToolCall(
                    id="call-1", name="get_weather", input_json=calling.output[0].parts[1].tool_call.input_json
                ),
            ),
        ],
    )
    assert calling.output == [assistant_turn]
    assert _tool_call(calling.output[0].parts[1]) == ("call-1", "get_weather", {"city": "Paris"})
    assert answering.input == [
        _text(MessageRole.USER, "Weather in Paris?"),
        assistant_turn,
        Message(
            role=MessageRole.TOOL,
            parts=[
                Part(
                    kind=PartKind.TOOL_RESULT,
                    tool_result=ToolResult(tool_call_id="call-1", name="get_weather", content="Sunny in Paris"),
                )
            ],
        ),
    ]
    assert answering.output == [_text(MessageRole.ASSISTANT, "It is sunny in Paris.")]
    tool_span = next(span for span in spans if span.attributes.get("gen_ai.operation.name") == "execute_tool")
    assert tool_span.attributes.get("gen_ai.tool.call.id") == "call-1"


def test_agento11y_pydantic_ai_agent_run_records_tool_retry_as_error_result() -> None:
    def get_weather(city: str) -> str:
        if city == "Atlantis":
            raise ModelRetry("Unknown city: Atlantis")
        return f"Sunny in {city}"

    def model(messages: list[ModelMessage], info: AgentInfo) -> ModelResponse:
        city, call_id = ("Atlantis", "call-1") if len(messages) == 1 else ("Paris", "call-2")
        if len(messages) == 5:
            return ModelResponse(parts=[TextPart(content="It is sunny in Paris.")])
        return ModelResponse(parts=[ToolCallPart(tool_name="get_weather", args={"city": city}, tool_call_id=call_id)])

    generations, _ = _record_agent_run(model, "Weather in Atlantis?", tools=[get_weather])

    assert len(generations) == 3
    retried = generations[1].input[-1]
    assert retried.role == MessageRole.TOOL
    result = retried.parts[0].tool_result
    assert (result.tool_call_id, result.name, result.is_error) == ("call-1", "get_weather", True)
    assert "Unknown city: Atlantis" in result.content
    succeeded = generations[2].input[-1].parts[0].tool_result
    assert (succeeded.tool_call_id, succeeded.content, succeeded.is_error) == ("call-2", "Sunny in Paris", False)


def test_agento11y_pydantic_ai_agent_run_records_output_tool_and_plain_text_retry() -> None:
    class Verdict(BaseModel):
        approved: bool

    def model(messages: list[ModelMessage], info: AgentInfo) -> ModelResponse:
        if len(messages) == 1:
            return ModelResponse(parts=[TextPart(content="Looks fine to me.")])
        output_tool = info.output_tools[0].name
        return ModelResponse(parts=[ToolCallPart(tool_name=output_tool, args={"approved": True}, tool_call_id="out-1")])

    generations, _ = _record_agent_run(model, "Review this.", output_type=ToolOutput(Verdict))

    assert len(generations) == 2
    prompt, prose, feedback = generations[1].input
    assert prompt == _text(MessageRole.USER, "Review this.")
    assert prose == _text(MessageRole.ASSISTANT, "Looks fine to me.")
    assert feedback.role == MessageRole.USER
    assert "tool" in feedback.parts[0].text.lower()
    assert _tool_call(generations[1].output[0].parts[0]) == ("out-1", "final_result", {"approved": True})


def test_agento11y_pydantic_ai_agent_run_records_output_tools_beside_function_tools() -> None:
    class Verdict(BaseModel):
        approved: bool

    def lookup_city(city: str) -> str:
        """Lookup a city."""
        return city

    def model(messages: list[ModelMessage], info: AgentInfo) -> ModelResponse:
        output_tool = info.output_tools[0].name
        return ModelResponse(parts=[ToolCallPart(tool_name=output_tool, args={"approved": True}, tool_call_id="out-1")])

    generations, _ = _record_agent_run(model, "Review this.", tools=[lookup_city], output_type=ToolOutput(Verdict))

    (generation,) = generations
    tools = {tool.name: tool for tool in generation.tools}
    assert list(tools) == ["lookup_city", "final_result"]
    assert "approved" in json.loads(tools["final_result"].input_schema_json)["properties"]


@pytest.mark.skipif(
    not hasattr(ModelRequestParameters, "visibility_of"),
    reason="this pydantic-ai cannot withhold a tool from a request",
)
def test_agento11y_pydantic_ai_capability_records_no_tool_withheld_from_the_request() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    request_context = _request_context(
        ModelRequestParameters(
            function_tools=[
                PydanticToolDefinition(name="lookup_city"),
                PydanticToolDefinition(name="archive_city", defer_loading=True),
            ]
        )
    )

    try:
        capability = create_agento11y_pydantic_ai_capability(client=client)

        async def handler(rc: ModelRequestContext) -> ModelResponse:
            return ModelResponse(parts=[TextPart(content="Done.")])

        asyncio.run(
            capability.wrap_model_request(
                _run_context("pydantic-withheld-1"), request_context=request_context, handler=handler
            )
        )
        client.flush()
        assert [tool.name for tool in exporter.requests[0].generations[0].tools] == ["lookup_city"]
    finally:
        client.shutdown()


def test_agento11y_pydantic_ai_agent_run_joins_system_prompt_and_instructions() -> None:
    def model(messages: list[ModelMessage], info: AgentInfo) -> ModelResponse:
        return ModelResponse(parts=[TextPart(content="Done.")])

    generations, _ = _record_agent_run(
        model, "Go.", system_prompt="You review code.", instructions="Answer in one sentence."
    )

    assert generations[0].system_prompt == "You review code.\n\nAnswer in one sentence."
    assert generations[0].input == [_text(MessageRole.USER, "Go.")]


def test_agento11y_pydantic_ai_agent_run_records_text_items_of_a_multimodal_prompt() -> None:
    def model(messages: list[ModelMessage], info: AgentInfo) -> ModelResponse:
        return ModelResponse(parts=[TextPart(content="A cat.")])

    generations, _ = _record_agent_run(model, ["Describe this.", ImageUrl(url="https://example.invalid/cat.png")])

    assert generations[0].input == [_text(MessageRole.USER, "Describe this.")]


def test_agento11y_pydantic_ai_agent_run_stream_records_time_to_first_token() -> None:
    async def stream_model(messages: list[ModelMessage], info: AgentInfo) -> AsyncIterator[str]:
        yield "Hel"
        yield "lo."

    exporter = _CapturingExporter()
    reader = InMemoryMetricReader()
    meter_provider = MeterProvider(metric_readers=[reader])
    client = Client(
        ClientConfig(
            meter=meter_provider.get_meter("agento11y-pydantic-ai-stream"),
            generation_export=GenerationExportConfig(batch_size=10, flush_interval=timedelta(seconds=60)),
            generation_exporter=exporter,
        )
    )
    agent = Agent(
        FunctionModel(stream_function=stream_model),
        capabilities=[create_agento11y_pydantic_ai_capability(client=client)],
    )

    async def _run() -> None:
        async with agent.run_stream("Hi") as result:
            await result.get_output()

    try:
        asyncio.run(_run())
        client.flush()
        metrics = reader.get_metrics_data()
    finally:
        client.shutdown()
        meter_provider.shutdown()

    generation = exporter.requests[0].generations[0]
    assert generation.mode.value == "STREAM"
    assert generation.output == [_text(MessageRole.ASSISTANT, "Hello.")]
    ttft_points = [
        point
        for resource_metrics in metrics.resource_metrics
        for scope_metrics in resource_metrics.scope_metrics
        for metric in scope_metrics.metrics
        if metric.name == "gen_ai.client.time_to_first_token"
        for point in metric.data.data_points
    ]
    assert [point.count for point in ttft_points] == [1]


def test_agento11y_pydantic_ai_failed_stream_keeps_the_text_it_streamed() -> None:
    async def stream_model(messages: list[ModelMessage], info: AgentInfo) -> AsyncIterator[Any]:
        yield {0: DeltaThinkingPart(content="Plan")}
        yield {0: DeltaThinkingPart(content=" the reply.")}
        yield "Hello"
        yield " world"
        raise RuntimeError("connection reset")

    exporter = _CapturingExporter()
    client = _new_client(exporter)
    agent = Agent(
        FunctionModel(stream_function=stream_model),
        capabilities=[create_agento11y_pydantic_ai_capability(client=client)],
    )

    async def _drain(ctx: Any, events: AsyncIterator[Any]) -> None:
        async for _ in events:
            pass

    try:
        with pytest.raises(RuntimeError, match="connection reset"):
            agent.run_sync("Hi", event_stream_handler=_drain)
        client.flush()
    finally:
        client.shutdown()

    assert exporter.requests[0].generations[0].output == [_text(MessageRole.ASSISTANT, "Hello world")]


def _done(messages: list[ModelMessage], info: AgentInfo) -> ModelResponse:
    return ModelResponse(parts=[TextPart(content="Done.")])


@pytest.mark.parametrize(
    ("thinking", "enabled", "level"),
    [(True, True, None), ("high", True, "high"), (False, False, None)],
)
def test_agento11y_pydantic_ai_agent_run_records_the_thinking_setting(
    thinking: bool | str, enabled: bool, level: str | None
) -> None:
    generations, _ = _record_agent_run(_done, "Go.", capabilities=[Thinking(thinking)])

    (generation,) = generations
    assert generation.thinking_enabled is enabled
    assert generation.metadata.get("agento11y.gen_ai.request.thinking.level") == level


def test_agento11y_pydantic_ai_agent_run_leaves_thinking_unset_without_the_setting() -> None:
    generations, _ = _record_agent_run(_done, "Go.")

    assert generations[0].thinking_enabled is None


@pytest.mark.parametrize(("agent_name", "run_name"), [("my_custom_agent", "my_custom_agent"), ("  ", "gpt-5")])
def test_agento11y_pydantic_ai_capability_names_a_run_after_its_agent_else_its_model(
    agent_name: str, run_name: str
) -> None:
    run_names: list[str] = []

    class _CapturingHandler:
        def on_chain_start(self, *args, **kwargs) -> None:
            run_names.append(kwargs["run_name"])

        def on_chain_end(self, *args, **kwargs) -> None:
            pass

    capability = Agento11yPydanticAICapability(_CapturingHandler())  # type: ignore[arg-type]
    ctx = _run_context("pydantic-agent-name-42", agent=Agent(TestModel(), name=agent_name))

    async def handler() -> Any:
        return "ok"

    asyncio.run(capability.wrap_run(ctx, handler=handler))

    assert run_names == [run_name]


class _Deps(BaseModel):
    conversation_id: str | None = None
    threadId: str | None = None


@pytest.mark.parametrize(
    ("deps", "metadata", "conversation_id", "thread_id"),
    [
        pytest.param(_Deps(conversation_id="deps-conversation"), None, "deps-conversation", None, id="deps attribute"),
        pytest.param({"sessionId": "deps-session"}, None, "deps-session", None, id="deps mapping, camel case"),
        pytest.param(None, {"session_id": "metadata-session"}, "metadata-session", None, id="run metadata"),
        pytest.param(
            _Deps(conversation_id="deps-conversation"),
            {"conversation_id": "metadata-conversation"},
            "deps-conversation",
            None,
            id="deps win over run metadata",
        ),
        pytest.param(_Deps(threadId="deps-thread"), None, "deps-thread", "deps-thread", id="thread alone"),
    ],
)
def test_agento11y_pydantic_ai_agent_run_takes_its_conversation_from_deps_then_run_metadata(
    deps: Any, metadata: dict[str, str] | None, conversation_id: str, thread_id: str | None
) -> None:
    generations, _ = _record_agent_run(_done, "Go.", run={"deps": deps, "metadata": metadata})

    (generation,) = generations
    assert generation.conversation_id == conversation_id
    assert generation.metadata.get("agento11y.framework.thread_id") == thread_id


@pytest.mark.skipif(
    "conversation_id" not in RunContext.__dataclass_fields__,
    reason="a run of this pydantic-ai has no conversation id",
)
@pytest.mark.parametrize(
    ("metadata", "recorded"),
    [
        pytest.param(None, "run-conversation", id="the run's own conversation id"),
        pytest.param({"conversation_id": "caller-conversation"}, "caller-conversation", id="the caller's id wins"),
    ],
)
def test_agento11y_pydantic_ai_agent_run_falls_back_to_its_own_conversation_id(
    metadata: dict[str, str] | None, recorded: str
) -> None:
    generations, spans = _record_agent_run(
        _done, "Go.", run={"conversation_id": "run-conversation", "metadata": metadata}
    )

    assert [generation.conversation_id for generation in generations] == [recorded]
    run_span = next(span for span in spans if span.attributes.get("agento11y.framework.run_type") == "agent")
    assert run_span.attributes.get("gen_ai.conversation.id") == recorded


def test_agento11y_pydantic_ai_agent_run_records_the_providers_response_id() -> None:
    def model(messages: list[ModelMessage], info: AgentInfo) -> ModelResponse:
        return ModelResponse(parts=[TextPart(content="Done.")], provider_response_id="resp_42")

    generations, _ = _record_agent_run(model, "Go.")

    assert generations[0].response_id == "resp_42"


def test_agento11y_pydantic_ai_agent_run_parents_model_requests_and_tools_on_the_run() -> None:
    def get_weather(city: str) -> str:
        return f"Sunny in {city}"

    def model(messages: list[ModelMessage], info: AgentInfo) -> ModelResponse:
        if len(messages) == 1:
            return ModelResponse(parts=[ToolCallPart(tool_name="get_weather", args={"city": "Paris"})])
        return ModelResponse(parts=[TextPart(content="Sunny.")])

    generations, spans = _record_agent_run(model, "Weather in Paris?", tools=[get_weather])

    run_span = next(span for span in spans if span.attributes.get("agento11y.framework.run_type") == "agent")
    run_id = run_span.attributes["agento11y.framework.run_id"]
    assert [generation.metadata["agento11y.framework.parent_run_id"] for generation in generations] == [run_id] * 2
    children = [span for span in spans if span is not run_span]
    assert len(children) == 3
    assert {span.parent.span_id for span in children} == {run_span.context.span_id}


def test_agento11y_pydantic_ai_agent_run_survives_a_response_that_cannot_be_recorded(
    caplog: pytest.LogCaptureFixture,
) -> None:
    class _FailsToRecordResponses(Agento11yPydanticAIHandler):
        def on_llm_end(self, response: Any, *, run_id: UUID, **kwargs: Any) -> None:
            raise RuntimeError("export queue rejected the generation")

    client = _new_client(_CapturingExporter())
    agent = Agent(
        FunctionModel(_done), capabilities=[Agento11yPydanticAICapability(_FailsToRecordResponses(client=client))]
    )

    try:
        with caplog.at_level("WARNING", logger="agento11y_pydantic_ai.capability"):
            assert agent.run_sync("Go.").output == "Done."
    finally:
        client.shutdown()

    assert [record.getMessage() for record in caplog.records] == ["agento11y: model response not recorded"]


# Pydantic AI 2.x has a field for reasoning tokens; 1.x model classes leave the count in `details`.
_REASONING_USAGE: list[dict[str, Any]] = (
    [{"output_reasoning_tokens": 7}]
    if "output_reasoning_tokens" in RequestUsage.__dataclass_fields__
    else [{"details": {"reasoning_tokens": 7}}, {"details": {"thoughts_tokens": 7}}]
)


@pytest.mark.parametrize("reasoning", _REASONING_USAGE, ids=lambda usage: next(iter(usage.get("details", usage))))
def test_agento11y_pydantic_ai_agent_run_records_reasoning_tokens(reasoning: dict[str, Any]) -> None:
    def model(messages: list[ModelMessage], info: AgentInfo) -> ModelResponse:
        return ModelResponse(
            parts=[TextPart(content="Done.")], usage=RequestUsage(input_tokens=10, output_tokens=20, **reasoning)
        )

    generations, _ = _record_agent_run(model, "Go.")

    usage = generations[0].usage
    assert (usage.input_tokens, usage.output_tokens, usage.reasoning_tokens) == (10, 20, 7)


def _record_streamed_run(stream_function: Any, drive: Any) -> list[Any]:
    """Drive a streamed run of a real agent with ``drive``; return its exported generations."""
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    agent = Agent(
        FunctionModel(stream_function=stream_function),
        capabilities=[create_agento11y_pydantic_ai_capability(client=client)],
    )
    try:
        asyncio.run(drive(agent))
        client.flush()
    finally:
        client.shutdown()
    return [generation for request in exporter.requests for generation in request.generations]


async def _stream_that_fails_to_open(messages: list[ModelMessage], info: AgentInfo) -> AsyncIterator[str]:
    raise RuntimeError("open failed")
    yield ""


async def _run_stream_expecting_open_failure(agent: Agent[Any, Any]) -> None:
    with pytest.raises(RuntimeError, match="open failed"):
        async with agent.run_stream("Hi") as result:
            await result.get_output()


def test_agento11y_pydantic_ai_agent_run_records_a_stream_that_fails_to_open_as_a_stream() -> None:
    (generation,) = _record_streamed_run(_stream_that_fails_to_open, _run_stream_expecting_open_failure)

    assert generation.mode.value == "STREAM"


@pytest.mark.skipif(
    Version("2.0.0") < Version(pydantic_ai.__version__) < Version("2.64.0"),
    reason="Pydantic AI 2.x before 2.64 (2.54, 2.62, 2.63 checked) cancels a request whose stream fails to open",
)
def test_agento11y_pydantic_ai_agent_run_records_the_error_of_a_stream_that_fails_to_open() -> None:
    (generation,) = _record_streamed_run(_stream_that_fails_to_open, _run_stream_expecting_open_failure)

    assert generation.call_error == "open failed"


def test_agento11y_pydantic_ai_agent_run_records_a_node_streamed_by_its_caller_as_a_stream() -> None:
    async def stream_model(messages: list[ModelMessage], info: AgentInfo) -> AsyncIterator[str]:
        yield "Hel"
        yield "lo."

    async def drive(agent: Agent[Any, Any]) -> None:
        async with agent.iter("Hi") as run:
            async for node in run:
                if Agent.is_model_request_node(node):
                    async with node.stream(run.ctx) as events:
                        async for _ in events:
                            pass

    (generation,) = _record_streamed_run(stream_model, drive)

    assert generation.mode.value == "STREAM"
    assert generation.output == [_text(MessageRole.ASSISTANT, "Hello.")]
