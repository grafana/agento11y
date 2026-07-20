"""LangChain handler lifecycle and provider-mapping tests."""

from __future__ import annotations

import asyncio
from datetime import timedelta
from uuid import uuid4

from agento11y import Client, ClientConfig, GenerationExportConfig
from agento11y.client import GenerationRecorder
from agento11y.models import ExportGenerationResult, ExportGenerationsResponse
from agento11y_langchain import (
    SigilAsyncLangChainHandler,
    SigilLangChainHandler,
    create_sigil_langchain_handler,
    with_sigil_langchain_callbacks,
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


def test_langchain_sync_lifecycle_sets_framework_tags_and_metadata() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    try:
        run_id = uuid4()
        parent_run_id = uuid4()
        handler = SigilLangChainHandler(
            client=client,
            agent_name="agent-langchain",
            agent_version="v1",
            provider_resolver="auto",
            extra_tags={"env": "test", "sigil.framework.name": "override"},
            extra_metadata={
                "seed": 7,
                "sigil.framework.run_id": "override-run",
                "sigil.framework.thread_id": "override-thread",
            },
        )

        handler.on_chat_model_start(
            {"name": "ChatOpenAI"},
            [[{"type": "human", "content": "hello"}]],
            run_id=run_id,
            parent_run_id=parent_run_id,
            tags=["prod", "blue"],
            invocation_params={"model": "gpt-5", "retry_attempt": 2},
            metadata={"thread_id": "chain-thread-42"},
        )
        handler.on_llm_end(
            {
                "generations": [[{"text": "world"}]],
                "llm_output": {
                    "model_name": "gpt-5",
                    "finish_reason": "stop",
                    "token_usage": {
                        "prompt_tokens": 10,
                        "completion_tokens": 5,
                        "total_tokens": 15,
                    },
                },
            },
            run_id=run_id,
        )

        client.flush()
        generation = exporter.requests[0].generations[0]
        assert generation.mode.value == "SYNC"
        assert generation.model.provider == "openai"
        assert generation.model.name == "gpt-5"
        assert generation.tags["sigil.framework.name"] == "langchain"
        assert generation.tags["sigil.framework.source"] == "handler"
        assert generation.tags["sigil.framework.language"] == "python"
        assert generation.tags["env"] == "test"
        assert generation.conversation_id == "chain-thread-42"
        assert generation.metadata["sigil.framework.run_id"] == str(run_id)
        assert generation.metadata["sigil.framework.thread_id"] == "chain-thread-42"
        assert generation.metadata["sigil.framework.parent_run_id"] == str(parent_run_id)
        assert generation.metadata["sigil.framework.component_name"] == "ChatOpenAI"
        assert generation.metadata["sigil.framework.run_type"] == "chat"
        assert generation.metadata["sigil.framework.retry_attempt"] == 2
        assert generation.metadata["sigil.framework.tags"] == ["prod", "blue"]
        assert generation.metadata["seed"] == 7
        assert generation.usage.input_tokens == 10
        assert generation.usage.output_tokens == 5
        assert generation.usage.total_tokens == 15
        assert generation.stop_reason == "stop"
        assert generation.output[0].parts[0].text == "world"
    finally:
        client.shutdown()


def test_langchain_backfills_model_from_response_when_request_model_unknown() -> None:
    """Regression: LangChain/LangGraph with a Bedrock inference profile does not
    surface the model at request start, so it resolves to "unknown"/"custom".
    The real model (a Bedrock inference-profile ARN) only arrives on the
    response. It must be backfilled onto the generation model so the
    token-usage metric (which drives cost) carries a resolvable model instead
    of "unknown"."""
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    arn = "arn:aws:bedrock:ap-south-1:500440857146:inference-profile/global.anthropic.claude-sonnet-4-6"

    try:
        run_id = uuid4()
        handler = SigilLangChainHandler(
            client=client,
            agent_name="agent-langchain",
            agent_version="v1",
            provider_resolver="auto",
        )

        # No "model" key in invocation_params — mirrors ChatBedrock with an
        # auto-provisioner default config, where the model is only known on
        # the response.
        handler.on_chat_model_start(
            {"name": "ChatBedrock"},
            [[{"type": "human", "content": "hello"}]],
            run_id=run_id,
            invocation_params={},
        )
        handler.on_llm_end(
            {
                "generations": [[{"text": "world"}]],
                "llm_output": {
                    "model_name": arn,
                    "finish_reason": "stop",
                    "token_usage": {
                        "prompt_tokens": 17011,
                        "completion_tokens": 217,
                        "total_tokens": 17228,
                    },
                },
            },
            run_id=run_id,
        )

        client.flush()
        generation = exporter.requests[0].generations[0]
        # Model is backfilled from the response so cost can resolve.
        assert generation.model.name == arn
        assert generation.model.provider == "anthropic"
        # response_model is still recorded.
        assert generation.response_model == arn
        assert generation.usage.input_tokens == 17011
        assert generation.usage.output_tokens == 217
    finally:
        client.shutdown()


def test_langchain_does_not_override_explicit_request_model() -> None:
    """When the request model IS known, an ARN response model must not clobber
    it — the explicit request model wins."""
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    try:
        run_id = uuid4()
        handler = SigilLangChainHandler(
            client=client,
            agent_name="agent-langchain",
            agent_version="v1",
            provider_resolver="auto",
        )

        handler.on_chat_model_start(
            {"name": "ChatAnthropic"},
            [[{"type": "human", "content": "hello"}]],
            run_id=run_id,
            invocation_params={"model": "claude-haiku-4-5-20251001"},
        )
        handler.on_llm_end(
            {
                "generations": [[{"text": "world"}]],
                "llm_output": {
                    "model_name": "some-other-response-model",
                    "finish_reason": "stop",
                    "token_usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
                },
            },
            run_id=run_id,
        )

        client.flush()
        generation = exporter.requests[0].generations[0]
        assert generation.model.name == "claude-haiku-4-5-20251001"
        assert generation.model.provider == "anthropic"
        assert generation.response_model == "some-other-response-model"
    finally:
        client.shutdown()


def test_langchain_infers_bedrock_provider_at_request_start() -> None:
    """When a Bedrock-style model id IS surfaced at request start (no explicit
    provider), the provider fallback must classify it from the vendor segment
    instead of falling back to "custom", so the start span is resolvable too.
    Uses positional vendor parsing that mirrors the backend: a dotted custom
    name whose vendor word is not in the vendor position stays "custom"."""
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    try:
        # Bedrock inference-profile id surfaced at start -> provider inferred.
        run_id = uuid4()
        handler = SigilLangChainHandler(
            client=client,
            agent_name="agent-langchain",
            agent_version="v1",
            provider_resolver="auto",
        )
        handler.on_chat_model_start(
            {"name": "ChatBedrock"},
            [[{"type": "human", "content": "hello"}]],
            run_id=run_id,
            invocation_params={"model": "us.anthropic.claude-sonnet-4-6"},
        )
        handler.on_llm_end(
            {
                "generations": [[{"text": "world"}]],
                "llm_output": {
                    "finish_reason": "stop",
                    "token_usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
                },
            },
            run_id=run_id,
        )

        # A dotted custom name with a vendor word out of position stays "custom".
        custom_run_id = uuid4()
        handler.on_chat_model_start(
            {"name": "ChatCustom"},
            [[{"type": "human", "content": "hello"}]],
            run_id=custom_run_id,
            invocation_params={"model": "my-team.anthropic.internal-model"},
        )
        handler.on_llm_end(
            {
                "generations": [[{"text": "world"}]],
                "llm_output": {
                    "finish_reason": "stop",
                    "token_usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
                },
            },
            run_id=custom_run_id,
        )

        client.flush()
        generations = exporter.requests[0].generations
        bedrock_gen = next(g for g in generations if g.model.name == "us.anthropic.claude-sonnet-4-6")
        assert bedrock_gen.model.provider == "anthropic"
        custom_gen = next(g for g in generations if g.model.name == "my-team.anthropic.internal-model")
        assert custom_gen.model.provider == "custom"
    finally:
        client.shutdown()


def test_langchain_non_canonical_provider_hint_does_not_block_bedrock_inference() -> None:
    """LangChain reports Bedrock as ls_provider="amazon_bedrock" (and sometimes
    "provider"). That hint normalizes to "custom" and must NOT short-circuit the
    model-name inference that recovers the real vendor from the Bedrock id. A canonical
    hint still wins, and a genuinely custom model keeps "custom"."""
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    cases = [
        # (run label, invocation_params, expected provider)
        ("ls", {"model": "us.anthropic.claude-sonnet-4-6-v1:0", "ls_provider": "amazon_bedrock"}, "anthropic"),
        ("prov", {"model": "us.anthropic.claude-sonnet-4-6-v1:0", "provider": "amazon_bedrock"}, "anthropic"),
        ("canonical", {"model": "claude-sonnet-4-5", "ls_provider": "openai"}, "openai"),
        ("genuine_custom", {"model": "my-inhouse-model", "ls_provider": "my_platform"}, "custom"),
    ]

    try:
        handler = SigilLangChainHandler(client=client, provider_resolver="auto")
        run_ids = {}
        for label, invocation_params, _ in cases:
            run_id = uuid4()
            run_ids[label] = run_id
            handler.on_chat_model_start(
                {"name": "ChatBedrock"},
                [[{"type": "human", "content": "hello"}]],
                run_id=run_id,
                invocation_params=invocation_params,
            )
            handler.on_llm_end(
                {
                    "generations": [[{"text": "world"}]],
                    "llm_output": {"model_name": invocation_params["model"]},
                },
                run_id=run_id,
            )

        client.flush()
        generations = exporter.requests[0].generations
        by_model = {g.model.name: g for g in generations}
        for label, invocation_params, expected in cases:
            gen = by_model[invocation_params["model"]]
            assert gen.model.provider == expected, f"{label}: got {gen.model.provider}, want {expected}"
    finally:
        client.shutdown()


def test_langchain_sync_lifecycle_extracts_anthropic_style_usage_and_stop_reason() -> None:
    """ChatAnthropic puts token usage under 'usage' (not 'token_usage') and
    stop reason under 'stop_reason' (not 'finish_reason')."""
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    try:
        run_id = uuid4()
        handler = SigilLangChainHandler(
            client=client,
            agent_name="agent-langchain",
            agent_version="v1",
            provider_resolver="auto",
        )

        handler.on_chat_model_start(
            {"name": "ChatAnthropic"},
            [[{"type": "human", "content": "hello"}]],
            run_id=run_id,
            invocation_params={"model": "claude-haiku-4-5-20251001"},
        )
        handler.on_llm_end(
            {
                "generations": [[{"text": "world"}]],
                "llm_output": {
                    "id": "msg_01ABC",
                    "model": "claude-haiku-4-5-20251001",
                    "model_name": "claude-haiku-4-5-20251001",
                    "stop_reason": "end_turn",
                    "usage": {
                        "input_tokens": 42,
                        "output_tokens": 17,
                    },
                },
            },
            run_id=run_id,
        )

        client.flush()
        generation = exporter.requests[0].generations[0]
        assert generation.model.provider == "anthropic"
        assert generation.model.name == "claude-haiku-4-5-20251001"
        assert generation.usage.input_tokens == 42
        assert generation.usage.output_tokens == 17
        assert generation.usage.total_tokens == 59
        assert generation.stop_reason == "end_turn"
    finally:
        client.shutdown()


def test_langchain_gemini_tool_calls_map_from_message_fields() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    try:
        run_id = uuid4()
        handler = SigilLangChainHandler(
            client=client,
            agent_name="agent-langchain",
            provider_resolver="auto",
        )

        handler.on_chat_model_start(
            {"name": "ChatGoogleGenerativeAI"},
            [[{"type": "human", "content": "Use the weather tool."}]],
            run_id=run_id,
            invocation_params={
                "model": "gemini-2.5-flash",
                "tool_choice": "get_weather",
                "tools": [
                    {
                        "type": "function",
                        "function": {
                            "name": "get_weather",
                            "description": "Get current weather for a city.",
                            "parameters": {
                                "type": "object",
                                "properties": {"city": {"type": "string"}},
                                "required": ["city"],
                            },
                        },
                    }
                ],
            },
        )
        handler.on_llm_end(
            {
                "generations": [
                    [
                        {
                            "message": {
                                "type": "ai",
                                "content": "",
                                "tool_calls": [
                                    {
                                        "name": "get_weather",
                                        "args": {"city": "Paris"},
                                        "id": "call-weather",
                                        "type": "tool_call",
                                    }
                                ],
                                "usage_metadata": {
                                    "input_tokens": 49,
                                    "output_tokens": 51,
                                    "total_tokens": 100,
                                    "input_token_details": {"cache_read": 7},
                                    "output_token_details": {"reasoning": 36},
                                },
                                "response_metadata": {
                                    "finish_reason": "STOP",
                                    "model_name": "gemini-2.5-flash",
                                    "model_provider": "google_genai",
                                },
                            }
                        }
                    ]
                ],
                "llm_output": {},
            },
            run_id=run_id,
        )

        client.flush()
        generation = exporter.requests[0].generations[0]
        assert generation.model.provider == "gemini"
        assert generation.model.name == "gemini-2.5-flash"
        assert generation.response_model == "gemini-2.5-flash"
        assert generation.stop_reason == "STOP"
        assert generation.tool_choice == "get_weather"
        assert [(tool.name, tool.type) for tool in generation.tools] == [("get_weather", "function")]
        assert b'"city"' in generation.tools[0].input_schema_json

        assert generation.output[0].role.value == "assistant"
        output_tool_call = generation.output[0].parts[0].tool_call
        assert output_tool_call is not None
        assert output_tool_call.id == "call-weather"
        assert output_tool_call.name == "get_weather"
        assert b'"Paris"' in output_tool_call.input_json

        assert generation.usage.input_tokens == 49
        assert generation.usage.output_tokens == 51
        assert generation.usage.total_tokens == 100
        assert generation.usage.cache_read_input_tokens == 7
        assert generation.usage.reasoning_tokens == 36
    finally:
        client.shutdown()


def test_langchain_stream_lifecycle_uses_stream_mode_and_chunk_fallback() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    try:
        run_id = uuid4()
        handler = SigilLangChainHandler(client=client)

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


def test_langchain_stream_records_first_token_timestamp_once() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    first_token_calls = 0
    original_set_first_token_at = GenerationRecorder.set_first_token_at

    def _tracking_set_first_token_at(self, first_token_at):
        nonlocal first_token_calls
        first_token_calls += 1
        return original_set_first_token_at(self, first_token_at)

    GenerationRecorder.set_first_token_at = _tracking_set_first_token_at

    try:
        run_id = uuid4()
        handler = SigilLangChainHandler(client=client)

        handler.on_llm_start(
            {"kwargs": {"model": "gpt-5"}},
            ["stream this"],
            run_id=run_id,
            invocation_params={"stream": True, "model": "gpt-5"},
        )
        handler.on_llm_new_token("hello", run_id=run_id)
        handler.on_llm_new_token(" world", run_id=run_id)
        handler.on_llm_end({"llm_output": {"model_name": "gpt-5"}}, run_id=run_id)

        assert first_token_calls == 1
    finally:
        GenerationRecorder.set_first_token_at = original_set_first_token_at
        client.shutdown()


def test_langchain_generation_span_tracks_active_parent_span_and_export_lineage() -> None:
    exporter = _CapturingExporter()
    span_exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(span_exporter))
    tracer = provider.get_tracer("sigil-framework-test")
    client = _new_client(exporter, tracer=tracer)

    try:
        run_id = uuid4()
        with tracer.start_as_current_span("framework.request"):
            handler = SigilLangChainHandler(client=client)
            handler.on_chat_model_start(
                {"name": "ChatOpenAI"},
                [[{"type": "human", "content": "hello"}]],
                run_id=run_id,
                parent_run_id=uuid4(),
                invocation_params={"model": "gpt-5"},
                metadata={"thread_id": "chain-thread-lineage-42"},
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


def test_langchain_provider_resolution_supports_known_models_and_fallback() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    try:
        handler = SigilLangChainHandler(client=client)

        run_openai = uuid4()
        handler.on_llm_start({}, ["x"], run_id=run_openai, invocation_params={"model": "gpt-5"})
        handler.on_llm_end({"generations": [[{"text": "ok"}]]}, run_id=run_openai)

        run_anthropic = uuid4()
        handler.on_llm_start({}, ["x"], run_id=run_anthropic, invocation_params={"model": "claude-sonnet-4-5"})
        handler.on_llm_end({"generations": [[{"text": "ok"}]]}, run_id=run_anthropic)

        run_gemini = uuid4()
        handler.on_llm_start({}, ["x"], run_id=run_gemini, invocation_params={"model": "gemini-2.5-pro"})
        handler.on_llm_end({"generations": [[{"text": "ok"}]]}, run_id=run_gemini)

        run_custom = uuid4()
        handler.on_llm_start({}, ["x"], run_id=run_custom, invocation_params={"model": "mistral-large"})
        handler.on_llm_end({"generations": [[{"text": "ok"}]]}, run_id=run_custom)

        client.flush()
        providers = [generation.model.provider for request in exporter.requests for generation in request.generations]
        assert providers == ["openai", "anthropic", "gemini", "custom"]
    finally:
        client.shutdown()


def test_langchain_error_sets_call_error_and_preserves_framework_tags() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    try:
        run_id = uuid4()
        handler = SigilLangChainHandler(client=client)

        handler.on_llm_start({}, ["x"], run_id=run_id, invocation_params={"model": "gpt-5"})
        handler.on_llm_error(RuntimeError("provider unavailable"), run_id=run_id)

        client.flush()
        generation = exporter.requests[0].generations[0]
        assert "provider unavailable" in generation.call_error
        assert generation.tags["sigil.framework.name"] == "langchain"
    finally:
        client.shutdown()


def test_langchain_async_handler_records_generation() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    async def _run() -> None:
        run_id = uuid4()
        handler = SigilAsyncLangChainHandler(client=client)
        await handler.on_llm_start({}, ["hello"], run_id=run_id, invocation_params={"model": "gpt-5"})
        await handler.on_llm_end({"generations": [[{"text": "world"}]]}, run_id=run_id)

    try:
        asyncio.run(_run())
        client.flush()
        generation = exporter.requests[0].generations[0]
        assert generation.tags["sigil.framework.name"] == "langchain"
        assert generation.model.provider == "openai"
    finally:
        client.shutdown()


def test_langchain_tool_chain_and_retriever_callbacks_emit_spans() -> None:
    exporter = _CapturingExporter()
    span_exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(span_exporter))
    tracer = provider.get_tracer("sigil-test")
    client = _new_client(exporter, tracer=tracer)

    try:
        handler = SigilLangChainHandler(client=client)
        parent_run_id = uuid4()

        tool_run_id = uuid4()
        handler.on_tool_start(
            {"name": "weather", "description": "Get weather"},
            '{"city":"Paris"}',
            run_id=tool_run_id,
            parent_run_id=parent_run_id,
            metadata={"thread_id": "chain-thread-42"},
        )
        handler.on_tool_end({"temp_c": 18}, run_id=tool_run_id)

        chain_run_id = uuid4()
        handler.on_chain_start(
            {"name": "PlanChain"},
            {},
            run_id=chain_run_id,
            parent_run_id=parent_run_id,
            tags=["workflow"],
            metadata={"thread_id": "chain-thread-42"},
            run_type="chain",
        )
        handler.on_chain_end({}, run_id=chain_run_id)

        retriever_run_id = uuid4()
        handler.on_retriever_start(
            {"name": "VectorRetriever"},
            "where is my data",
            run_id=retriever_run_id,
            parent_run_id=parent_run_id,
            metadata={"thread_id": "chain-thread-42"},
        )
        handler.on_retriever_error(RuntimeError("retriever failed"), run_id=retriever_run_id)

        spans = span_exporter.get_finished_spans()
        tool_span = next(span for span in spans if span.attributes.get("gen_ai.operation.name") == "execute_tool")
        chain_span = next(span for span in spans if span.attributes.get("gen_ai.operation.name") == "framework_chain")
        retriever_span = next(
            span for span in spans if span.attributes.get("gen_ai.operation.name") == "framework_retriever"
        )

        assert tool_span.attributes.get("gen_ai.tool.name") == "weather"
        assert tool_span.attributes.get("gen_ai.conversation.id") == "chain-thread-42"

        assert chain_span.attributes.get("sigil.framework.run_type") == "chain"
        assert chain_span.attributes.get("sigil.framework.component_name") == "PlanChain"
        assert chain_span.attributes.get("sigil.framework.parent_run_id") == str(parent_run_id)
        assert chain_span.status.status_code.name == "OK"

        assert retriever_span.attributes.get("sigil.framework.run_type") == "retriever"
        assert retriever_span.attributes.get("sigil.framework.component_name") == "VectorRetriever"
        assert retriever_span.status.status_code.name == "ERROR"
        assert retriever_span.attributes.get("error.type") == "framework_error"
    finally:
        client.shutdown()
        provider.shutdown()


def test_langchain_attach_helpers_preserve_existing_callbacks() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        created = create_sigil_langchain_handler(client=client)
        assert isinstance(created, SigilLangChainHandler)

        existing = object()
        config = with_sigil_langchain_callbacks(
            {"callbacks": [existing], "retry": 2},
            client=client,
            agent_name="langchain-helper",
        )

        assert config["retry"] == 2
        callbacks = config["callbacks"]
        assert isinstance(callbacks, list)
        assert callbacks[0] is existing
        assert isinstance(callbacks[1], SigilLangChainHandler)
    finally:
        client.shutdown()


def test_langchain_handler_explicitly_has_no_embedding_lifecycle() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        handler = SigilLangChainHandler(client=client)
        assert not hasattr(handler, "on_embedding_start")
        assert not hasattr(handler, "on_embedding_end")
        assert not hasattr(handler, "on_embedding_error")
    finally:
        client.shutdown()


def test_langchain_attach_helpers_do_not_duplicate_existing_sigil_handler() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        existing = SigilLangChainHandler(client=client)
        config = with_sigil_langchain_callbacks({"callbacks": [existing]}, client=client)
        callbacks = config["callbacks"]
        assert isinstance(callbacks, list)
        assert len(callbacks) == 1
        assert callbacks[0] is existing
    finally:
        client.shutdown()
