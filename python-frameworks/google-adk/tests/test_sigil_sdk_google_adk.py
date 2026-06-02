"""google-adk handler lifecycle and conversation-mapping tests."""

from __future__ import annotations

import asyncio
from datetime import timedelta
from uuid import UUID, uuid4

from google.adk.plugins import BasePlugin
from google.genai import types as genai_types
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
from sigil_sdk import Client, ClientConfig, GenerationExportConfig
from sigil_sdk.models import ExportGenerationResult, ExportGenerationsResponse
from sigil_sdk_google_adk import (
    SigilAsyncGoogleAdkHandler,
    SigilGoogleAdkCallbacks,
    SigilGoogleAdkHandler,
    SigilGoogleAdkPlugin,
    create_sigil_google_adk_handler,
    create_sigil_google_adk_plugin,
    with_sigil_google_adk_callbacks,
    with_sigil_google_adk_plugins,
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


def _new_client(exporter: _CapturingExporter, tracer=None) -> Client:
    return Client(
        ClientConfig(
            tracer=tracer,
            generation_export=GenerationExportConfig(batch_size=10, flush_interval=timedelta(seconds=60)),
            generation_exporter=exporter,
        )
    )


def test_sigil_sdk_google_adk_sync_lifecycle_sets_framework_metadata() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    try:
        run_id = uuid4()
        parent_run_id = uuid4()
        handler = SigilGoogleAdkHandler(client=client, provider_resolver="auto")

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
        assert generation.tags["sigil.framework.name"] == "google-adk"
        assert generation.tags["sigil.framework.source"] == "handler"
        assert generation.tags["sigil.framework.language"] == "python"
        assert generation.conversation_id == "framework-conversation-42"
        assert generation.metadata["sigil.framework.run_id"] == str(run_id)
        assert generation.metadata["sigil.framework.parent_run_id"] == str(parent_run_id)
        assert generation.metadata["sigil.framework.thread_id"] == "framework-thread-42"
        assert generation.metadata["sigil.framework.event_id"] == "framework-event-42"
        assert generation.metadata["sigil.framework.run_type"] == "chat"
        assert generation.metadata["sigil.framework.retry_attempt"] == 2
        assert generation.output[0].parts[0].text == "world"
    finally:
        client.shutdown()


def test_sigil_sdk_google_adk_keeps_thread_metadata_when_ids_are_split_across_payloads() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    try:
        run_id = uuid4()
        handler = SigilGoogleAdkHandler(client=client, provider_resolver="auto")

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
        assert generation.metadata["sigil.framework.thread_id"] == "framework-thread-split-42"
        assert generation.metadata["sigil.framework.event_id"] == "framework-event-split-42"
    finally:
        client.shutdown()


def test_sigil_sdk_google_adk_fallback_conversation_is_deterministic() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    try:
        run_id = uuid4()
        handler = SigilGoogleAdkHandler(client=client)

        handler.on_llm_start(
            {"kwargs": {"model": "gpt-5"}},
            ["hello"],
            run_id=run_id,
            invocation_params={"model": "gpt-5"},
        )
        handler.on_llm_end({"generations": [[{"text": "ok"}]], "llm_output": {"model_name": "gpt-5"}}, run_id=run_id)

        client.flush()
        generation = exporter.requests[0].generations[0]
        assert generation.conversation_id == f"sigil:framework:google-adk:{run_id}"
    finally:
        client.shutdown()


def test_sigil_sdk_google_adk_stream_mode_uses_chunks_when_output_missing() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    try:
        run_id = uuid4()
        handler = SigilGoogleAdkHandler(client=client)

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


def test_sigil_sdk_google_adk_generation_span_tracks_active_parent_span_and_export_lineage() -> None:
    exporter = _CapturingExporter()
    span_exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(span_exporter))
    tracer = provider.get_tracer("sigil-framework-test")
    client = _new_client(exporter, tracer=tracer)

    try:
        run_id = uuid4()
        with tracer.start_as_current_span("framework.request"):
            handler = SigilGoogleAdkHandler(client=client, provider_resolver="auto")
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


def test_sigil_sdk_google_adk_normalizes_extra_metadata() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    try:
        run_id = uuid4()
        handler = SigilGoogleAdkHandler(
            client=client,
            extra_metadata={
                "timestamp": "2026-02-20T00:00:00Z",
                "list": ["a", {"nested": True}, object()],
                "callable": lambda: "skip",
            },
        )

        handler.on_llm_start(
            {"kwargs": {"model": "gpt-5"}},
            ["hello"],
            run_id=run_id,
            invocation_params={"model": "gpt-5"},
        )
        handler.on_llm_end({"generations": [[{"text": "ok"}]], "llm_output": {"model_name": "gpt-5"}}, run_id=run_id)

        client.flush()
        generation = exporter.requests[0].generations[0]
        assert generation.metadata["timestamp"] == "2026-02-20T00:00:00Z"
        assert generation.metadata["list"] == ["a", {"nested": True}]
        assert "callable" not in generation.metadata
    finally:
        client.shutdown()


def test_sigil_sdk_google_adk_async_handler_records_generation() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    async def _run() -> None:
        run_id = uuid4()
        handler = SigilAsyncGoogleAdkHandler(client=client)
        await handler.on_llm_start({}, ["hello"], run_id=run_id, invocation_params={"model": "gpt-5"})
        await handler.on_llm_end({"generations": [[{"text": "world"}]]}, run_id=run_id)

    try:
        asyncio.run(_run())
        client.flush()
        generation = exporter.requests[0].generations[0]
        assert generation.tags["sigil.framework.name"] == "google-adk"
        assert generation.model.provider == "openai"
    finally:
        client.shutdown()


def test_sigil_sdk_google_adk_callback_helpers_attach_callback_fields() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    class _Session:
        id = "adk-session-42"
        state: dict[str, str] = {}

    class _CallbackContext:
        invocation_id = "adk-invocation-42"
        session = _Session()
        agent_name = "adk-helper-agent"

    class _Part:
        def __init__(self, text: str) -> None:
            self.text = text

    class _Content:
        def __init__(self, role: str, text: str) -> None:
            self.role = role
            self.parts = [_Part(text)]

    class _LlmRequest:
        model = "gemini-2.5-pro"
        contents = [_Content("user", "hello")]

    class _UsageMetadata:
        prompt_token_count = 3
        candidates_token_count = 5
        total_token_count = 8

    class _LlmResponse:
        content = _Content("assistant", "world")
        model_version = "gemini-2.5-pro"
        usage_metadata = _UsageMetadata()
        finish_reason = "stop"

    class _Agent:
        before_model_callback = None
        after_model_callback = None
        on_model_error_callback = None
        before_tool_callback = None
        after_tool_callback = None
        on_tool_error_callback = None

    async def _invoke_callback(callback: object, **kwargs) -> None:
        callbacks = callback if isinstance(callback, list) else [callback]
        for item in callbacks:
            result = item(**kwargs)  # type: ignore[misc]
            if asyncio.iscoroutine(result):
                await result

    try:
        created = create_sigil_google_adk_handler(client=client)
        assert isinstance(created, SigilGoogleAdkHandler)

        config = with_sigil_google_adk_callbacks(
            {"name": "adk-runner", "plugins": ["existing-plugin"]},
            client=client,
            agent_name="google-adk-helper",
        )

        assert config["name"] == "adk-runner"
        assert config["plugins"] == ["existing-plugin"]
        assert "before_model_callback" in config
        callback = config["before_model_callback"]
        callback_items = callback if isinstance(callback, list) else [callback]
        assert any(isinstance(getattr(item, "__self__", None), SigilGoogleAdkCallbacks) for item in callback_items)

        async def _run() -> None:
            callback_context = _CallbackContext()
            await _invoke_callback(
                config["before_model_callback"],
                callback_context=callback_context,
                llm_request=_LlmRequest(),
            )
            await _invoke_callback(
                config["after_model_callback"],
                callback_context=callback_context,
                llm_response=_LlmResponse(),
            )

        asyncio.run(_run())
        client.flush()
        generation = exporter.requests[0].generations[0]
        assert generation.conversation_id == "adk-session-42"
        assert generation.metadata["sigil.framework.event_id"] == "adk-invocation-42"

        agent = _Agent()
        returned_agent = with_sigil_google_adk_callbacks(agent, client=client)
        assert returned_agent is agent
        assert agent.before_model_callback is not None
        assert agent.after_model_callback is not None

    finally:
        client.shutdown()


def test_sigil_sdk_google_adk_handler_explicitly_has_no_embedding_lifecycle() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        handler = SigilGoogleAdkHandler(client=client)
        assert not hasattr(handler, "on_embedding_start")
        assert not hasattr(handler, "on_embedding_end")
        assert not hasattr(handler, "on_embedding_error")
    finally:
        client.shutdown()


def test_sigil_sdk_google_adk_callbacks_close_tool_runs_without_function_call_id() -> None:
    class _CapturingHandler:
        def __init__(self) -> None:
            self.started: list[UUID] = []
            self.ended: list[UUID] = []

        async def on_tool_start(self, *args, **kwargs) -> None:
            del args
            run_id = kwargs.get("run_id")
            if isinstance(run_id, UUID):
                self.started.append(run_id)

        async def on_tool_end(self, *args, **kwargs) -> None:
            del args
            run_id = kwargs.get("run_id")
            if isinstance(run_id, UUID):
                self.ended.append(run_id)

    class _ToolContext:
        def __init__(self, invocation_id: str) -> None:
            self.invocation_id = invocation_id
            self.function_call_id = None

    capture = _CapturingHandler()
    callbacks = SigilGoogleAdkCallbacks(capture)  # type: ignore[arg-type]
    context_start = _ToolContext("adk-tool-invocation-42")
    context_end = _ToolContext("adk-tool-invocation-42")

    async def _run() -> None:
        await callbacks.before_tool_callback({"name": "weather_tool"}, {"city": "Paris"}, context_start)
        await callbacks.after_tool_callback({"name": "weather_tool"}, {"city": "Paris"}, context_end, {"ok": True})

    asyncio.run(_run())

    assert len(capture.started) == 1
    assert len(capture.ended) == 1
    assert capture.started[0] == capture.ended[0]


def test_sigil_sdk_google_adk_plugin_helpers_attach_plugin_list() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    class _Session:
        id = "adk-plugin-session-42"
        state: dict[str, str] = {}

    class _InvocationContext:
        invocation_id = "adk-plugin-invocation-42"
        session = _Session()
        agent_name = "adk-plugin-agent"

    class _CallbackContext:
        invocation_id = "adk-plugin-invocation-42"
        session = _Session()
        agent_name = "adk-plugin-agent"

    class _Part:
        def __init__(self, text: str) -> None:
            self.text = text

    class _Content:
        def __init__(self, role: str, text: str) -> None:
            self.role = role
            self.parts = [_Part(text)]

    class _LlmRequest:
        model = "gemini-2.5-pro"
        contents = [_Content("user", "hello")]

    class _LlmResponse:
        content = _Content("assistant", "world")
        model_version = "gemini-2.5-pro"

    try:
        created = create_sigil_google_adk_plugin(client=client)
        assert isinstance(created, SigilGoogleAdkPlugin)

        config = with_sigil_google_adk_plugins({"name": "adk-runner"}, client=client)
        assert config["name"] == "adk-runner"
        assert "plugins" in config
        assert isinstance(config["plugins"], list)
        assert isinstance(config["plugins"][0], SigilGoogleAdkPlugin)
        assert isinstance(config["plugins"][0], BasePlugin)

        plugin = config["plugins"][0]

        async def _run() -> None:
            invocation_context = _InvocationContext()
            callback_context = _CallbackContext()
            await plugin.before_run_callback(invocation_context=invocation_context)
            await plugin.before_model_callback(callback_context=callback_context, llm_request=_LlmRequest())
            await plugin.after_model_callback(callback_context=callback_context, llm_response=_LlmResponse())
            await plugin.after_run_callback(invocation_context=invocation_context)

        asyncio.run(_run())
        client.flush()
        generation = exporter.requests[0].generations[0]
        assert generation.conversation_id == "adk-plugin-session-42"
        assert generation.tags["sigil.framework.name"] == "google-adk"
    finally:
        client.shutdown()


def test_sigil_sdk_google_adk_plugin_maps_current_request_config_tools_and_usage() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    class _Session:
        id = "adk-current-session-42"
        state: dict[str, str] = {}

    class _InvocationContext:
        invocation_id = "adk-current-invocation-42"
        session = _Session()
        agent_name = "adk-current-agent"

    class _CallbackContext:
        invocation_context = _InvocationContext()

    class _LlmRequest:
        model = "gemini-2.5-pro"
        contents = [
            genai_types.Content(role="user", parts=[genai_types.Part(text="weather in Paris?")]),
            genai_types.Content(
                role="model",
                parts=[
                    genai_types.Part(
                        function_call=genai_types.FunctionCall(
                            id="call-weather-0",
                            name="get_weather",
                            args={"city": "Paris"},
                        )
                    )
                ],
            ),
            genai_types.Content(
                role="user",
                parts=[
                    genai_types.Part(
                        function_response=genai_types.FunctionResponse(
                            id="call-weather-0",
                            name="get_weather",
                            response={"forecast": "sunny"},
                        )
                    )
                ],
            ),
        ]
        config = genai_types.GenerateContentConfig(
            system_instruction=genai_types.Content(role="system", parts=[genai_types.Part(text="Be concise.")]),
            temperature=0.2,
            max_output_tokens=256,
            top_p=0.95,
            tool_config=genai_types.ToolConfig(
                function_calling_config=genai_types.FunctionCallingConfig(
                    mode=genai_types.FunctionCallingConfigMode.ANY,
                    allowed_function_names=["get_weather"],
                )
            ),
            thinking_config=genai_types.ThinkingConfig(include_thoughts=True, thinking_budget=1536),
            tools=[
                genai_types.Tool(
                    function_declarations=[
                        genai_types.FunctionDeclaration(
                            name="get_weather",
                            description="Gets the weather for a city.",
                            parameters_json_schema={
                                "type": "object",
                                "properties": {"city": {"type": "string"}},
                                "required": ["city"],
                            },
                        )
                    ]
                )
            ],
        )
        tools_dict = {}

    class _LlmResponse:
        content = genai_types.Content(
            role="model",
            parts=[
                genai_types.Part(
                    function_call=genai_types.FunctionCall(
                        id="call-weather-1",
                        name="get_weather",
                        args={"city": "Paris"},
                    )
                )
            ],
        )
        model_version = "gemini-2.5-pro-001"
        usage_metadata = genai_types.GenerateContentResponseUsageMetadata(
            prompt_token_count=10,
            candidates_token_count=5,
            cached_content_token_count=3,
            thoughts_token_count=2,
            tool_use_prompt_token_count=4,
            total_token_count=21,
        )
        finish_reason = "STOP"

    try:
        plugin = create_sigil_google_adk_plugin(client=client)

        async def _run() -> None:
            invocation_context = _InvocationContext()
            callback_context = _CallbackContext()
            await plugin.before_run_callback(invocation_context=invocation_context)
            await plugin.before_agent_callback(agent=object(), callback_context=callback_context)
            await plugin.before_model_callback(callback_context=callback_context, llm_request=_LlmRequest())
            await plugin.after_model_callback(callback_context=callback_context, llm_response=_LlmResponse())
            await plugin.after_agent_callback(agent=object(), callback_context=callback_context)
            await plugin.after_run_callback(invocation_context=invocation_context)

        asyncio.run(_run())
        client.flush()

        generation = exporter.requests[0].generations[0]
        assert generation.system_prompt == "Be concise."
        assert generation.max_tokens == 256
        assert generation.temperature == 0.2
        assert generation.top_p == 0.95
        assert generation.tool_choice == "get_weather"
        assert generation.thinking_enabled is True
        assert generation.metadata["sigil.gen_ai.request.thinking.budget_tokens"] == 1536
        assert len(generation.tools) == 1
        assert generation.tools[0].name == "get_weather"
        assert generation.tools[0].description == "Gets the weather for a city."
        assert b'"city"' in generation.tools[0].input_schema_json

        assert generation.input[1].role.value == "assistant"
        history_tool_call = generation.input[1].parts[0].tool_call
        assert history_tool_call is not None
        assert history_tool_call.id == "call-weather-0"
        assert history_tool_call.name == "get_weather"

        assert generation.input[2].role.value == "tool"
        history_tool_result = generation.input[2].parts[0].tool_result
        assert history_tool_result is not None
        assert history_tool_result.tool_call_id == "call-weather-0"
        assert history_tool_result.name == "get_weather"
        assert b'"forecast"' in history_tool_result.content_json

        assert generation.output[0].role.value == "assistant"
        tool_call = generation.output[0].parts[0].tool_call
        assert tool_call is not None
        assert tool_call.id == "call-weather-1"
        assert tool_call.name == "get_weather"
        assert b'"Paris"' in tool_call.input_json

        assert generation.usage.input_tokens == 14
        assert generation.usage.output_tokens == 7
        assert generation.usage.cache_read_input_tokens == 3
        assert generation.usage.reasoning_tokens == 2
        assert generation.usage.total_tokens == 21
    finally:
        client.shutdown()


def test_sigil_sdk_google_adk_plugin_streams_partial_event_tokens() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    class _Session:
        id = "adk-stream-session-42"
        state: dict[str, str] = {}

    class _InvocationContext:
        invocation_id = "adk-stream-invocation-42"
        session = _Session()
        agent_name = "adk-stream-agent"

    class _CallbackContext:
        invocation_context = _InvocationContext()

    class _Part:
        def __init__(self, text: str) -> None:
            self.text = text

    class _Content:
        def __init__(self, role: str, text: str) -> None:
            self.role = role
            self.parts = [_Part(text)]

    class _LlmRequest:
        model = "gemini-2.5-pro"
        contents = [_Content("user", "hello")]

    class _LlmResponse:
        model_version = "gemini-2.5-pro"

    try:
        plugin = create_sigil_google_adk_plugin(client=client)

        async def _run() -> None:
            invocation_context = _InvocationContext()
            callback_context = _CallbackContext()
            await plugin.before_run_callback(invocation_context=invocation_context)
            await plugin.before_model_callback(callback_context=callback_context, llm_request=_LlmRequest())
            await plugin.on_event_callback(
                invocation_context=invocation_context,
                event={"partial": True, "text": "hello"},
            )
            await plugin.on_event_callback(
                invocation_context=invocation_context,
                event={"partial": True, "text": "world"},
            )
            await plugin.after_model_callback(callback_context=callback_context, llm_response=_LlmResponse())
            await plugin.after_run_callback(invocation_context=invocation_context)

        asyncio.run(_run())
        client.flush()
        generation = exporter.requests[0].generations[0]
        assert generation.output[0].parts[0].text == "helloworld"
    finally:
        client.shutdown()


def test_sigil_sdk_google_adk_plugin_tracks_nested_agent_chain_runs() -> None:
    class _CapturingHandler:
        def __init__(self) -> None:
            self.starts: list[dict[str, object]] = []
            self.ends: list[dict[str, object]] = []

        async def on_chain_start(self, *args, **kwargs) -> None:
            self.starts.append({"args": args, "kwargs": kwargs})

        async def on_chain_end(self, *args, **kwargs) -> None:
            self.ends.append({"args": args, "kwargs": kwargs})

    class _Session:
        id = "adk-agent-session-42"
        state: dict[str, str] = {}

    class _InvocationContext:
        invocation_id = "adk-agent-invocation-42"
        session = _Session()
        agent_name = "top-agent"

    class _CallbackContext:
        def __init__(self, agent_name: str) -> None:
            self.agent_name = agent_name
            self.invocation_context = _InvocationContext()

    capture = _CapturingHandler()
    plugin = SigilGoogleAdkPlugin(capture)  # type: ignore[arg-type]

    async def _run() -> None:
        invocation_context = _InvocationContext()
        parent_context = _CallbackContext("coordinator")
        child_context = _CallbackContext("worker")

        await plugin.before_run_callback(invocation_context=invocation_context)
        await plugin.before_agent_callback(callback_context=parent_context)
        await plugin.before_agent_callback(callback_context=child_context)
        await plugin.after_agent_callback(callback_context=child_context)
        await plugin.after_agent_callback(callback_context=parent_context)
        await plugin.after_run_callback(invocation_context=invocation_context)

    asyncio.run(_run())

    assert len(capture.starts) == 3
    invocation_start = capture.starts[0]["kwargs"]
    parent_agent_start = capture.starts[1]["kwargs"]
    child_agent_start = capture.starts[2]["kwargs"]

    invocation_run_id = invocation_start["run_id"]
    parent_agent_run_id = parent_agent_start["run_id"]
    child_agent_run_id = child_agent_start["run_id"]

    assert parent_agent_start["parent_run_id"] == invocation_run_id
    assert child_agent_start["parent_run_id"] == parent_agent_run_id
    assert parent_agent_start["run_type"] == "agent"
    assert child_agent_start["run_type"] == "agent"

    end_run_ids = [entry["kwargs"]["run_id"] for entry in capture.ends]
    assert end_run_ids == [child_agent_run_id, parent_agent_run_id, invocation_run_id]
