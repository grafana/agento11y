"""pydantic-ai handler lifecycle and conversation-mapping tests."""

from __future__ import annotations

import asyncio
from datetime import timedelta
from typing import Any
from uuid import UUID, uuid4

import pytest
from agento11y import Client, ClientConfig, GenerationExportConfig
from agento11y.models import ExportGenerationResult, ExportGenerationsResponse
from agento11y_pydantic_ai import (
    Agento11yPydanticAICapability,
    Agento11yPydanticAIHandler,
    create_agento11y_pydantic_ai_capability,
    create_agento11y_pydantic_ai_handler,
    with_agento11y_pydantic_ai_capability,
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


def _new_client(exporter: _CapturingExporter, tracer=None) -> Client:
    return Client(
        ClientConfig(
            tracer=tracer,
            generation_export=GenerationExportConfig(batch_size=10, flush_interval=timedelta(seconds=60)),
            generation_exporter=exporter,
        )
    )


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

    class _RunContext:
        run_id = "pydantic-run-42"
        deps = None
        metadata = None
        model = None

    class _Usage:
        input_tokens = 10
        output_tokens = 5
        cache_read_tokens = 3
        cache_write_tokens = 2

    class _TextPart:
        part_kind = "text"
        content = "world"

    class _ModelResponse:
        parts = [_TextPart()]
        model_name = "gpt-5"
        finish_reason = "stop"
        usage = _Usage()

    class _UserPart:
        part_kind = "user-prompt"
        content = "hello"

    class _ModelRequest:
        kind = "request"
        parts = [_UserPart()]

    class _RequestContext:
        model = "gpt-5"
        messages = [_ModelRequest()]

    try:
        capability = create_agento11y_pydantic_ai_capability(client=client, provider_resolver="auto")

        async def _run() -> None:
            ctx = _RunContext()
            request_context = _RequestContext()

            async def handler(rc: Any) -> Any:
                return _ModelResponse()

            await capability.wrap_model_request(ctx, request_context=request_context, handler=handler)

        asyncio.run(_run())
        client.flush()
        generation = exporter.requests[0].generations[0]
        assert generation.tags["agento11y.framework.name"] == "pydantic-ai"
        assert generation.model.name == "gpt-5"
        assert generation.model.provider == "openai"
        assert generation.conversation_id == "agento11y:framework:pydantic-ai:pydantic-run-42"
        assert generation.output[0].parts[0].text == "world"
        # pydantic-ai reports a cache-inclusive input: the raw input (10) already
        # contains the 3 cache_read and 2 cache_write tokens, so raw_input + output
        # (10 + 5) equals the disjoint total (15). That justifies subtracting BOTH
        # cache buckets to recover fresh input (10 - 3 - 2 = 5).
        assert 10 + 5 == 15  # raw input + output == total (cache is inside input)
        assert generation.usage.input_tokens == 5
        assert generation.usage.output_tokens == 5
        assert generation.usage.cache_read_input_tokens == 3
        assert generation.usage.cache_write_input_tokens == 2
        assert generation.usage.total_tokens == 15
        # Disjoint contract: fresh input + all disjoint buckets reconstructs total.
        assert (
            generation.usage.input_tokens
            + generation.usage.output_tokens
            + generation.usage.cache_read_input_tokens
            + generation.usage.cache_write_input_tokens
            == generation.usage.total_tokens
        )
        assert generation.usage.input_is_disjoint is True
    finally:
        client.shutdown()


def test_agento11y_pydantic_ai_capability_wrap_model_request_propagates_settings_and_tools() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    class _RunContext:
        run_id = "pydantic-settings-42"
        deps = None
        metadata = None
        model = None
        agent = None

    class _ModelResponse:
        parts: list[Any] = []
        model_name = "gpt-5"
        finish_reason = "stop"
        usage = None

    class _ToolDef:
        name = "lookup_city"
        description = "Lookup a city"
        parameters_json_schema = {
            "type": "object",
            "properties": {"city": {"type": "string"}},
            "required": ["city"],
        }

    class _RequestParams:
        function_tools = [_ToolDef()]
        instruction_parts = [
            type("_IP", (), {"content": "You are a concise weather assistant."})(),
            type("_IP", (), {"content": "Always answer in English."})(),
        ]

    class _RequestContext:
        model = "gpt-5"
        messages: list[Any] = []
        model_settings = {"temperature": 0.2, "max_tokens": 256, "top_p": 0.9}
        model_request_parameters = _RequestParams()

    try:
        capability = create_agento11y_pydantic_ai_capability(client=client, provider_resolver="auto")

        async def _run() -> None:
            async def handler(rc: Any) -> Any:
                return _ModelResponse()

            await capability.wrap_model_request(_RunContext(), request_context=_RequestContext(), handler=handler)

        asyncio.run(_run())
        client.flush()
        generation = exporter.requests[0].generations[0]
        assert generation.temperature == 0.2
        assert generation.max_tokens == 256
        assert generation.top_p == 0.9
        assert generation.system_prompt == "You are a concise weather assistant.\n\nAlways answer in English."
        assert len(generation.tools) == 1
        assert generation.tools[0].name == "lookup_city"
        assert generation.tools[0].description == "Lookup a city"
        assert generation.tools[0].type == "function"
        assert b'"city"' in generation.tools[0].input_schema_json
    finally:
        client.shutdown()


def test_agento11y_pydantic_ai_capability_wrap_model_request_marks_streaming_handler() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    class _RunContext:
        run_id = "pydantic-stream-42"
        deps = None
        metadata = None
        model = None
        agent = None

    class _ModelResponse:
        parts: list[Any] = []
        model_name = "gpt-5"
        finish_reason = "stop"
        usage = None

    class _RequestContext:
        model = "gpt-5"
        messages: list[Any] = []
        model_settings = None
        model_request_parameters = None

    async def _streaming_handler(rc: Any) -> Any:
        return _ModelResponse()

    try:
        capability = create_agento11y_pydantic_ai_capability(client=client, provider_resolver="auto")

        async def _run() -> None:
            await capability.wrap_model_request(
                _RunContext(), request_context=_RequestContext(), handler=_streaming_handler
            )

        asyncio.run(_run())
        client.flush()
        generation = exporter.requests[0].generations[0]
        assert generation.mode.value == "STREAM"
    finally:
        client.shutdown()


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

    class _RunContext:
        run_id = "pydantic-tool-run-42"
        deps = None
        metadata = None
        model = None

    class _ToolCall:
        tool_name = "weather_tool"

    class _ToolDef:
        name = "weather_tool"
        description = "Get weather info"

    capture = _CapturingHandler()
    capability = Agento11yPydanticAICapability(capture)  # type: ignore[arg-type]

    async def _run() -> None:
        ctx = _RunContext()

        async def handler(args: Any) -> Any:
            return {"temperature": 22}

        await capability.wrap_tool_execute(
            ctx,
            call=_ToolCall(),
            tool_def=_ToolDef(),
            args={"city": "Paris"},
            handler=handler,
        )

    asyncio.run(_run())

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

    class _RunContext:
        run_id = "pydantic-tool-args-42"
        deps = None
        metadata = None
        model = None
        agent = None

    class _ToolCall:
        tool_name = "weather_tool"

    class _ToolDef:
        name = "weather_tool"
        description = "Get weather info"

    try:
        capability = create_agento11y_pydantic_ai_capability(client=client)

        async def _run() -> None:
            async def handler(args: Any) -> Any:
                return {"temperature": 22}

            await capability.wrap_tool_execute(
                ctx=_RunContext(),
                call=_ToolCall(),
                tool_def=_ToolDef(),
                args={"city": "Paris"},
                handler=handler,
            )

        asyncio.run(_run())
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

    class _RunContext:
        run_id = "pydantic-chain-run-42"
        deps = None
        metadata = None
        model = None

    class _Result:
        data = "ok"

    capture = _CapturingHandler()
    capability = Agento11yPydanticAICapability(capture)  # type: ignore[arg-type]

    async def _run() -> None:
        ctx = _RunContext()

        async def handler() -> Any:
            return _Result()

        await capability.wrap_run(ctx, handler=handler)

    asyncio.run(_run())

    assert len(capture.starts) == 1
    assert len(capture.ends) == 1
    start_kwargs = capture.starts[0]["kwargs"]
    assert start_kwargs["run_type"] == "agent"
    assert start_kwargs["run_name"] == "pydantic_ai_agent"


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
    client = _new_client(exporter)

    class _RunContext:
        run_id = "pydantic-control-flow-42"
        deps = None
        metadata = None
        model = None
        agent = None

    class _RequestContext:
        model = "gpt-5"
        messages = []

    class _ToolCall:
        tool_name = "cached_tool"

    class _ToolDef:
        name = "cached_tool"
        description = "Can be skipped"

    try:
        handler = Agento11yPydanticAIHandler(client=client)
        capability = Agento11yPydanticAICapability(handler)

        async def _run() -> None:
            ctx = _RunContext()

            async def run_handler() -> Any:
                raise ModelRetry("retry the agent")

            with pytest.raises(ModelRetry):
                await capability.wrap_run(ctx, handler=run_handler)

            async def model_handler(rc: Any) -> Any:
                raise SkipModelRequest(object())

            with pytest.raises(SkipModelRequest):
                await capability.wrap_model_request(ctx, request_context=_RequestContext(), handler=model_handler)

            async def tool_handler(args: Any) -> Any:
                raise SkipToolExecution("cached")

            with pytest.raises(SkipToolExecution):
                await capability.wrap_tool_execute(
                    ctx,
                    call=_ToolCall(),
                    tool_def=_ToolDef(),
                    args={},
                    handler=tool_handler,
                )

        asyncio.run(_run())
        client.flush()

        assert handler._chain_spans == {}
        assert handler._runs == {}
        assert handler._tool_runs == {}
        assert exporter.requests == []
    finally:
        client.shutdown()


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

    class _RunContext:
        run_id = "pydantic-error-run-42"
        deps = None
        metadata = None
        model = None
        agent = None

    class _RequestContext:
        model = "gpt-5"
        messages = []

    capture = _CapturingHandler()
    capability = Agento11yPydanticAICapability(capture)  # type: ignore[arg-type]

    async def _run() -> None:
        ctx = _RunContext()
        request_context = _RequestContext()

        async def handler(rc: Any) -> Any:
            raise ValueError("model failed")

        with pytest.raises(ValueError, match="model failed"):
            await capability.wrap_model_request(ctx, request_context=request_context, handler=handler)

    asyncio.run(_run())

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

    class _RunContext:
        run_id = "pydantic-tool-error-42"
        deps = None
        metadata = None
        model = None
        agent = None

    class _ToolCall:
        tool_name = "broken_tool"

    class _ToolDef:
        name = "broken_tool"
        description = "A tool that fails"

    capture = _CapturingHandler()
    capability = Agento11yPydanticAICapability(capture)  # type: ignore[arg-type]

    async def _run() -> None:
        ctx = _RunContext()

        async def handler(args: Any) -> Any:
            raise RuntimeError("tool exploded")

        with pytest.raises(RuntimeError, match="tool exploded"):
            await capability.wrap_tool_execute(
                ctx,
                call=_ToolCall(),
                tool_def=_ToolDef(),
                args={"x": 1},
                handler=handler,
            )

    asyncio.run(_run())

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

    class _RunContext:
        run_id = "pydantic-chain-error-42"
        deps = None
        metadata = None
        model = None
        agent = None

    capture = _CapturingHandler()
    capability = Agento11yPydanticAICapability(capture)  # type: ignore[arg-type]

    async def _run() -> None:
        ctx = _RunContext()

        async def handler() -> Any:
            raise RuntimeError("agent crashed")

        with pytest.raises(RuntimeError, match="agent crashed"):
            await capability.wrap_run(ctx, handler=handler)

    asyncio.run(_run())

    assert len(capture.starts) == 1
    assert len(capture.errors) == 1
    assert len(capture.ends) == 0
    assert capture.starts[0] == capture.errors[0][0]
    assert str(capture.errors[0][1]) == "agent crashed"


def test_agento11y_pydantic_ai_capability_agent_name_from_agent_attribute() -> None:
    class _Agent:
        name = "my_custom_agent"

    class _RunContext:
        run_id = "pydantic-agent-name-42"
        deps = None
        metadata = None
        model = None
        agent = _Agent()

    class _CapturingHandler:
        def __init__(self) -> None:
            self.run_names: list[str] = []

        def on_chain_start(self, *args, **kwargs) -> None:
            run_name = kwargs.get("run_name")
            if run_name:
                self.run_names.append(run_name)

        def on_chain_end(self, *args, **kwargs) -> None:
            pass

    capture = _CapturingHandler()
    capability = Agento11yPydanticAICapability(capture)  # type: ignore[arg-type]

    async def _run() -> None:
        ctx = _RunContext()

        async def handler() -> Any:
            return "ok"

        await capability.wrap_run(ctx, handler=handler)

    asyncio.run(_run())

    assert len(capture.run_names) == 1
    assert capture.run_names[0] == "my_custom_agent"


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
