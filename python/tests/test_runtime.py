"""Core lifecycle and span behavior tests for Sigil Python SDK."""

from __future__ import annotations

import time
from datetime import timedelta

from agento11y import (
    Client,
    ClientConfig,
    EmbeddingCaptureConfig,
    EmbeddingResult,
    EmbeddingStart,
    EnqueueError,
    GenerationExportConfig,
    GenerationStart,
    Message,
    MessageRole,
    ModelRef,
    Part,
    PartKind,
    QueueFullError,
    ToolExecutionStart,
    ValidationError,
    with_agent_name,
    with_agent_version,
    with_conversation_id,
)
from conftest import CapturingGenerationExporter
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter


def _new_client(exporter: CapturingGenerationExporter, tracer=None, **overrides) -> Client:
    generation_export = GenerationExportConfig(
        batch_size=overrides.get("batch_size", 10),
        flush_interval=overrides.get("flush_interval", timedelta(seconds=60)),
        queue_size=overrides.get("queue_size", 10),
        max_retries=overrides.get("max_retries", 1),
        initial_backoff=overrides.get("initial_backoff", timedelta(milliseconds=1)),
        max_backoff=overrides.get("max_backoff", timedelta(milliseconds=1)),
        payload_max_bytes=overrides.get("payload_max_bytes", 4 << 20),
    )
    return Client(
        ClientConfig(
            tracer=tracer,
            generation_export=generation_export,
            embedding_capture=overrides.get("embedding_capture", EmbeddingCaptureConfig()),
            generation_exporter=exporter,
        )
    )


def _seed_generation(conversation_id: str) -> GenerationStart:
    return GenerationStart(
        conversation_id=conversation_id,
        model=ModelRef(provider="openai", name="gpt-5"),
    )


def _assistant_output(text: str) -> list[Message]:
    return [Message(role=MessageRole.ASSISTANT, parts=[Part(kind=PartKind.TEXT, text=text)])]


def test_flushes_generation_exports_by_batch_size() -> None:
    exporter = CapturingGenerationExporter()
    client = _new_client(exporter, batch_size=2)
    try:
        rec1 = client.start_generation(_seed_generation("conv-1"))
        rec1.set_result(output=_assistant_output("ok-1"))
        rec1.end()
        assert rec1.err() is None

        rec2 = client.start_generation(_seed_generation("conv-2"))
        rec2.set_result(output=_assistant_output("ok-2"))
        rec2.end()
        assert rec2.err() is None

        _wait_for(lambda: len(exporter.requests) == 1)
        assert len(exporter.requests[0].generations) == 2
    finally:
        client.shutdown()


def test_flushes_generation_exports_by_interval() -> None:
    exporter = CapturingGenerationExporter()
    client = _new_client(exporter, batch_size=10, flush_interval=timedelta(milliseconds=20))
    try:
        rec = client.start_generation(_seed_generation("conv-3"))
        rec.set_result(output=_assistant_output("ok-3"))
        rec.end()
        assert rec.err() is None

        _wait_for(lambda: len(exporter.requests) >= 1)
        assert len(exporter.requests[0].generations) == 1
    finally:
        client.shutdown()


def test_flush_retries_failed_exports_with_backoff() -> None:
    exporter = CapturingGenerationExporter(failures_before_success=2)
    client = _new_client(exporter, batch_size=10, max_retries=2)
    try:
        rec = client.start_generation(_seed_generation("conv-4"))
        rec.set_result(output=_assistant_output("ok-4"))
        rec.end()

        client.flush()
        assert exporter.attempts == 3
        assert len(exporter.requests) == 3
    finally:
        client.shutdown()


def test_shutdown_flushes_pending_generation() -> None:
    exporter = CapturingGenerationExporter()
    client = _new_client(exporter, batch_size=10)

    rec = client.start_generation(_seed_generation("conv-5"))
    rec.set_result(output=_assistant_output("ok-5"))
    rec.end()

    client.shutdown()

    assert len(exporter.requests) == 1
    assert len(exporter.requests[0].generations) == 1
    assert exporter.shutdown_calls == 1


def test_builtin_noop_generation_exporter_supports_instrumentation_only_mode() -> None:
    provider = TracerProvider()
    tracer = provider.get_tracer("agento11y-test")
    client = Client(
        ClientConfig(
            tracer=tracer,
            generation_export=GenerationExportConfig(
                protocol="none",
                batch_size=1,
                flush_interval=timedelta(hours=1),
            ),
        )
    )
    try:
        rec = client.start_generation(_seed_generation("conv-noop"))
        rec.set_result(output=_assistant_output("ok-noop"))
        rec.end()

        assert rec.err() is None
        assert rec.last_generation is not None

        client.flush()
    finally:
        client.shutdown()
        provider.shutdown()


def test_queue_full_error_is_exposed_as_local_recorder_error() -> None:
    exporter = CapturingGenerationExporter()
    client = _new_client(exporter, batch_size=10, queue_size=1)
    try:
        rec1 = client.start_generation(_seed_generation("conv-6"))
        rec1.set_result(output=_assistant_output("ok-6"))
        rec1.end()
        assert rec1.err() is None

        rec2 = client.start_generation(_seed_generation("conv-7"))
        rec2.set_result(output=_assistant_output("full"))
        rec2.end()

        assert isinstance(rec2.err(), QueueFullError)
        assert isinstance(rec2.err(), EnqueueError)
    finally:
        client.shutdown()


def test_default_operation_name_is_mode_aware_and_mode_not_emitted_as_span_attribute() -> None:
    exporter = CapturingGenerationExporter()
    span_exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(span_exporter))
    tracer = provider.get_tracer("agento11y-test")

    client = _new_client(exporter, tracer=tracer)
    try:
        sync_rec = client.start_generation(_seed_generation("conv-sync"))
        sync_rec.set_result(output=_assistant_output("sync"))
        sync_rec.end()

        stream_rec = client.start_streaming_generation(_seed_generation("conv-stream"))
        stream_rec.set_result(output=_assistant_output("stream"))
        stream_rec.end()

        spans = span_exporter.get_finished_spans()
        names = {span.name for span in spans if span.attributes.get("gen_ai.request.model") == "gpt-5"}
        assert "generateText gpt-5" in names
        assert "streamText gpt-5" in names

        for span in spans:
            assert "agento11y.generation.mode" not in span.attributes
    finally:
        client.shutdown()
        provider.shutdown()


def test_call_error_sets_span_error_and_does_not_set_local_error() -> None:
    exporter = CapturingGenerationExporter()
    span_exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(span_exporter))
    tracer = provider.get_tracer("agento11y-test")

    client = _new_client(exporter, tracer=tracer)
    try:
        rec = client.start_generation(_seed_generation("conv-call-error"))
        rec.set_call_error(RuntimeError("provider unavailable"))
        rec.end()

        assert rec.err() is None
        assert rec.last_generation is not None
        assert rec.last_generation.call_error == "provider unavailable"
        assert rec.last_generation.metadata["agento11y.sdk.name"] == "sdk-python"

        span = span_exporter.get_finished_spans()[-1]
        assert span.status.status_code.name == "ERROR"
        assert span.attributes.get("error.type") == "provider_call_error"
        assert span.attributes.get("agento11y.sdk.name") == "sdk-python"
    finally:
        client.shutdown()
        provider.shutdown()


def test_embedding_span_sets_attributes_and_does_not_enqueue_generation() -> None:
    exporter = CapturingGenerationExporter()
    span_exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(span_exporter))
    tracer = provider.get_tracer("agento11y-test")

    client = _new_client(exporter, tracer=tracer)
    try:
        rec = client.start_embedding(
            EmbeddingStart(
                model=ModelRef(provider="openai", name="text-embedding-3-small"),
                agent_name="agent-embed",
                agent_version="v-embed",
                dimensions=256,
                encoding_format="float",
            )
        )
        rec.set_result(
            EmbeddingResult(
                input_count=2,
                input_tokens=64,
                input_texts=["first", "second"],
                response_model="text-embedding-3-small",
                dimensions=512,
            )
        )
        rec.end()

        assert rec.err() is None
        assert len(exporter.requests) == 0

        span = span_exporter.get_finished_spans()[-1]
        assert span.name == "embeddings text-embedding-3-small"
        assert span.attributes.get("gen_ai.operation.name") == "embeddings"
        assert span.attributes.get("gen_ai.provider.name") == "openai"
        assert span.attributes.get("gen_ai.request.model") == "text-embedding-3-small"
        assert span.attributes.get("gen_ai.agent.name") == "agent-embed"
        assert span.attributes.get("gen_ai.agent.version") == "v-embed"
        assert span.attributes.get("gen_ai.embeddings.dimension.count") == 512
        assert span.attributes.get("gen_ai.request.encoding_formats") == ("float",)
        assert span.attributes.get("gen_ai.embeddings.input_count") == 2
        assert span.attributes.get("gen_ai.usage.input_tokens") == 64
        assert span.attributes.get("gen_ai.response.model") == "text-embedding-3-small"
        assert "gen_ai.embeddings.input_texts" not in span.attributes
    finally:
        client.shutdown()
        provider.shutdown()


def test_embedding_input_capture_is_opt_in_with_truncation() -> None:
    exporter = CapturingGenerationExporter()
    span_exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(span_exporter))
    tracer = provider.get_tracer("agento11y-test")

    client = _new_client(
        exporter,
        tracer=tracer,
        embedding_capture=EmbeddingCaptureConfig(
            capture_input=True,
            max_input_items=2,
            max_text_length=8,
        ),
    )
    try:
        rec = client.start_embedding(
            EmbeddingStart(
                model=ModelRef(provider="openai", name="text-embedding-3-small"),
            )
        )
        rec.set_result(
            EmbeddingResult(
                input_count=3,
                input_texts=["12345678", "123456789", "dropped"],
            )
        )
        rec.end()

        assert rec.err() is None
        span = span_exporter.get_finished_spans()[-1]
        assert span.attributes.get("gen_ai.embeddings.input_texts") == ("12345678", "12345...")
    finally:
        client.shutdown()
        provider.shutdown()


def test_embedding_call_error_marks_provider_error_without_local_error() -> None:
    exporter = CapturingGenerationExporter()
    span_exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(span_exporter))
    tracer = provider.get_tracer("agento11y-test")

    client = _new_client(exporter, tracer=tracer)
    try:
        rec = client.start_embedding(
            EmbeddingStart(
                model=ModelRef(provider="openai", name="text-embedding-3-small"),
            )
        )
        rec.set_call_error(RuntimeError("provider unavailable"))
        rec.end()

        assert rec.err() is None
        span = span_exporter.get_finished_spans()[-1]
        assert span.status.status_code.name == "ERROR"
        assert span.attributes.get("error.type") == "provider_call_error"
    finally:
        client.shutdown()
        provider.shutdown()


def test_embedding_validation_error_sets_local_error() -> None:
    exporter = CapturingGenerationExporter()
    span_exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(span_exporter))
    tracer = provider.get_tracer("agento11y-test")

    client = _new_client(exporter, tracer=tracer)
    try:
        rec = client.start_embedding(
            EmbeddingStart(
                model=ModelRef(provider="", name="text-embedding-3-small"),
            )
        )
        rec.end()

        assert isinstance(rec.err(), ValidationError)
        span = span_exporter.get_finished_spans()[-1]
        assert span.status.status_code.name == "ERROR"
        assert span.attributes.get("error.type") == "validation_error"
        assert span.attributes.get("error.category") == "sdk_error"
    finally:
        client.shutdown()
        provider.shutdown()


def test_embedding_context_defaults_apply_for_agent_fields() -> None:
    exporter = CapturingGenerationExporter()
    span_exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(span_exporter))
    tracer = provider.get_tracer("agento11y-test")

    client = _new_client(exporter, tracer=tracer)
    try:
        with with_agent_name("agent-from-ctx"), with_agent_version("v-ctx"):
            rec = client.start_embedding(
                EmbeddingStart(
                    model=ModelRef(provider="openai", name="text-embedding-3-small"),
                )
            )
            rec.end()

        span = span_exporter.get_finished_spans()[-1]
        assert span.attributes.get("gen_ai.agent.name") == "agent-from-ctx"
        assert span.attributes.get("gen_ai.agent.version") == "v-ctx"
    finally:
        client.shutdown()
        provider.shutdown()


def test_request_controls_result_overrides_seed_and_sets_span_attrs() -> None:
    exporter = CapturingGenerationExporter()
    span_exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(span_exporter))
    tracer = provider.get_tracer("agento11y-test")

    client = _new_client(exporter, tracer=tracer)
    try:
        rec = client.start_generation(
            GenerationStart(
                conversation_id="conv-controls",
                model=ModelRef(provider="openai", name="gpt-5"),
                max_tokens=1024,
                temperature=0.7,
                top_p=0.9,
                tool_choice="auto",
                thinking_enabled=True,
            )
        )
        rec.set_result(
            max_tokens=256,
            temperature=0.2,
            top_p=0.8,
            tool_choice="required",
            thinking_enabled=False,
            metadata={
                "agento11y.gen_ai.request.thinking.budget_tokens": 4096,
                "agento11y.framework.run_id": "framework-run-1",
                "agento11y.framework.thread_id": "framework-thread-1",
                "agento11y.framework.parent_run_id": "framework-parent-1",
                "agento11y.framework.component_name": "ChatOpenAI",
                "agento11y.framework.run_type": "chat",
                "agento11y.framework.retry_attempt": 3,
                "agento11y.framework.langgraph.node": "answer_node",
                "agento11y.sdk.name": "user-value",
            },
            stop_reason="end_turn",
            output=_assistant_output("ok"),
        )
        rec.end()

        assert rec.err() is None
        assert rec.last_generation is not None
        assert rec.last_generation.max_tokens == 256
        assert rec.last_generation.temperature == 0.2
        assert rec.last_generation.top_p == 0.8
        assert rec.last_generation.tool_choice == "required"
        assert rec.last_generation.thinking_enabled is False
        assert rec.last_generation.metadata["agento11y.gen_ai.request.thinking.budget_tokens"] == 4096
        assert rec.last_generation.metadata["agento11y.framework.run_id"] == "framework-run-1"
        assert rec.last_generation.metadata["agento11y.framework.thread_id"] == "framework-thread-1"
        assert rec.last_generation.metadata["agento11y.framework.parent_run_id"] == "framework-parent-1"
        assert rec.last_generation.metadata["agento11y.framework.component_name"] == "ChatOpenAI"
        assert rec.last_generation.metadata["agento11y.framework.run_type"] == "chat"
        assert rec.last_generation.metadata["agento11y.framework.retry_attempt"] == 3
        assert rec.last_generation.metadata["agento11y.framework.langgraph.node"] == "answer_node"
        assert rec.last_generation.metadata["agento11y.sdk.name"] == "sdk-python"

        span = span_exporter.get_finished_spans()[-1]
        assert span.attributes.get("gen_ai.request.max_tokens") == 256
        assert span.attributes.get("gen_ai.request.temperature") == 0.2
        assert span.attributes.get("gen_ai.request.top_p") == 0.8
        assert span.attributes.get("agento11y.gen_ai.request.tool_choice") == "required"
        assert span.attributes.get("agento11y.gen_ai.request.thinking.enabled") is False
        assert span.attributes.get("agento11y.gen_ai.request.thinking.budget_tokens") == 4096
        assert span.attributes.get("agento11y.framework.run_id") == "framework-run-1"
        assert span.attributes.get("agento11y.framework.thread_id") == "framework-thread-1"
        assert span.attributes.get("agento11y.framework.parent_run_id") == "framework-parent-1"
        assert span.attributes.get("agento11y.framework.component_name") == "ChatOpenAI"
        assert span.attributes.get("agento11y.framework.run_type") == "chat"
        assert span.attributes.get("agento11y.framework.retry_attempt") == 3
        assert span.attributes.get("agento11y.framework.langgraph.node") == "answer_node"
        assert span.attributes.get("agento11y.sdk.name") == "sdk-python"
        assert span.attributes.get("gen_ai.response.finish_reasons") == ("end_turn",)
    finally:
        client.shutdown()
        provider.shutdown()


def test_sdk_metadata_overrides_conflicting_seed_and_result_values() -> None:
    exporter = CapturingGenerationExporter()
    client = _new_client(exporter)
    try:
        rec = client.start_generation(
            GenerationStart(
                conversation_id="conv-sdk-metadata",
                model=ModelRef(provider="openai", name="gpt-5"),
                metadata={
                    "agento11y.sdk.name": "seed-value",
                },
            )
        )
        rec.set_result(
            metadata={
                "agento11y.sdk.name": "result-value",
            },
            output=_assistant_output("ok"),
        )
        rec.end()

        assert rec.err() is None
        assert rec.last_generation is not None
        assert rec.last_generation.metadata["agento11y.sdk.name"] == "sdk-python"
    finally:
        client.shutdown()


def test_validation_error_sets_local_error() -> None:
    exporter = CapturingGenerationExporter()
    client = _new_client(exporter)
    try:
        rec = client.start_generation(_seed_generation("conv-validation"))
        rec.set_result(
            input=[Message(role=MessageRole.USER, parts=[])],
            output=_assistant_output("ok"),
        )
        rec.end()

        assert isinstance(rec.err(), ValidationError)
    finally:
        client.shutdown()


def test_context_defaults_apply_and_explicit_fields_override() -> None:
    exporter = CapturingGenerationExporter()
    client = _new_client(exporter)
    try:
        with with_conversation_id("conv-from-ctx"), with_agent_name("agent-from-ctx"), with_agent_version("v-ctx"):
            rec = client.start_generation(
                GenerationStart(
                    model=ModelRef(provider="anthropic", name="claude-sonnet-4-5"),
                )
            )
            rec.end()

        assert rec.last_generation is not None
        assert rec.last_generation.conversation_id == "conv-from-ctx"
        assert rec.last_generation.agent_name == "agent-from-ctx"
        assert rec.last_generation.agent_version == "v-ctx"

        with with_conversation_id("ctx-id"):
            rec2 = client.start_generation(
                GenerationStart(
                    conversation_id="explicit-id",
                    model=ModelRef(provider="anthropic", name="claude-sonnet-4-5"),
                )
            )
            rec2.end()

        assert rec2.last_generation is not None
        assert rec2.last_generation.conversation_id == "explicit-id"
    finally:
        client.shutdown()


def test_tool_execution_attributes_and_content_capture() -> None:
    exporter = CapturingGenerationExporter()
    span_exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(span_exporter))
    tracer = provider.get_tracer("agento11y-test")

    client = _new_client(exporter, tracer=tracer)
    try:
        with client.start_tool_execution(
            ToolExecutionStart(
                tool_name="weather",
                tool_call_id="call_weather",
                tool_type="function",
                tool_description="Get weather",
                conversation_id="conv-tool",
                agent_name="agent-tools",
                agent_version="2026.02.12",
                request_provider="openai",
                request_model="gpt-5",
                include_content=True,
            )
        ) as rec:
            rec.set_result(arguments={"city": "Paris"}, result={"temp_c": 18})

        span = span_exporter.get_finished_spans()[-1]
        assert span.name == "execute_tool weather"
        assert span.attributes.get("gen_ai.operation.name") == "execute_tool"
        assert span.attributes.get("gen_ai.tool.name") == "weather"
        assert span.attributes.get("gen_ai.tool.call.id") == "call_weather"
        assert span.attributes.get("gen_ai.tool.call.arguments") is not None
        assert span.attributes.get("gen_ai.tool.call.result") is not None
        assert span.attributes.get("gen_ai.provider.name") == "openai"
        assert span.attributes.get("gen_ai.request.model") == "gpt-5"
        assert span.attributes.get("agento11y.sdk.name") == "sdk-python"
    finally:
        client.shutdown()
        provider.shutdown()


def _wait_for(predicate, timeout_s: float = 1.0) -> None:
    deadline = time.time() + timeout_s
    while time.time() < deadline:
        if predicate():
            return
        time.sleep(0.01)
    raise AssertionError("timed out waiting for condition")
