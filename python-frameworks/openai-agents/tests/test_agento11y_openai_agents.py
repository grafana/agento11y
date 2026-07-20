"""openai-agents handler lifecycle and conversation-mapping tests."""

from __future__ import annotations

import asyncio
from datetime import timedelta
from uuid import uuid4

import pytest
from agento11y import Client, ClientConfig, GenerationExportConfig
from agento11y.models import ExportGenerationResult, ExportGenerationsResponse
from agento11y_openai_agents import (
    SigilAsyncOpenAIAgentsHandler,
    SigilOpenAIAgentsHandler,
    SigilOpenAIAgentsRunHooks,
    create_sigil_openai_agents_handler,
    with_sigil_openai_agents_hooks,
)
from agents import RunHooks
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


def test_agento11y_openai_agents_sync_lifecycle_sets_framework_metadata() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    try:
        run_id = uuid4()
        parent_run_id = uuid4()
        handler = SigilOpenAIAgentsHandler(client=client, provider_resolver="auto")

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
        assert generation.tags["sigil.framework.name"] == "openai-agents"
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


def test_agento11y_openai_agents_keeps_thread_metadata_when_ids_are_split_across_payloads() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    try:
        run_id = uuid4()
        handler = SigilOpenAIAgentsHandler(client=client, provider_resolver="auto")

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


def test_agento11y_openai_agents_fallback_conversation_is_deterministic() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    try:
        run_id = uuid4()
        handler = SigilOpenAIAgentsHandler(client=client)

        handler.on_llm_start(
            {"kwargs": {"model": "gpt-5"}},
            ["hello"],
            run_id=run_id,
            invocation_params={"model": "gpt-5"},
        )
        handler.on_llm_end({"generations": [[{"text": "ok"}]], "llm_output": {"model_name": "gpt-5"}}, run_id=run_id)

        client.flush()
        generation = exporter.requests[0].generations[0]
        assert generation.conversation_id == f"sigil:framework:openai-agents:{run_id}"
    finally:
        client.shutdown()


def test_agento11y_openai_agents_stream_mode_uses_chunks_when_output_missing() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    try:
        run_id = uuid4()
        handler = SigilOpenAIAgentsHandler(client=client)

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


def test_agento11y_openai_agents_generation_span_tracks_active_parent_span_and_export_lineage() -> None:
    exporter = _CapturingExporter()
    span_exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(span_exporter))
    tracer = provider.get_tracer("sigil-framework-test")
    client = _new_client(exporter, tracer=tracer)

    try:
        run_id = uuid4()
        with tracer.start_as_current_span("framework.request"):
            handler = SigilOpenAIAgentsHandler(client=client, provider_resolver="auto")
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


def test_agento11y_openai_agents_normalizes_extra_metadata() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    try:
        run_id = uuid4()
        handler = SigilOpenAIAgentsHandler(
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


def test_agento11y_openai_agents_async_handler_records_generation() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    async def _run() -> None:
        run_id = uuid4()
        handler = SigilAsyncOpenAIAgentsHandler(client=client)
        await handler.on_llm_start({}, ["hello"], run_id=run_id, invocation_params={"model": "gpt-5"})
        await handler.on_llm_end({"generations": [[{"text": "world"}]]}, run_id=run_id)

    try:
        asyncio.run(_run())
        client.flush()
        generation = exporter.requests[0].generations[0]
        assert generation.tags["sigil.framework.name"] == "openai-agents"
        assert generation.model.provider == "openai"
    finally:
        client.shutdown()


def test_agento11y_openai_agents_hook_helpers_append_handler() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    class _ExistingHooks:
        def __init__(self) -> None:
            self.started = 0
            self.ended = 0

        async def on_agent_start(self, _context, _agent) -> None:
            self.started += 1

        async def on_agent_end(self, _context, _agent, _output) -> None:
            self.ended += 1

    class _Context:
        def __init__(self) -> None:
            self.context = {"conversation_id": "helper-conversation-42", "thread_id": "helper-thread-42"}
            self.tool_input = {"arg": "value"}

    class _Agent:
        name = "helper-agent"
        model = "gpt-5"

    try:
        created = create_sigil_openai_agents_handler(client=client)
        assert isinstance(created, SigilOpenAIAgentsHandler)

        existing = _ExistingHooks()
        run_options = with_sigil_openai_agents_hooks(
            {"hooks": existing, "max_turns": 3},
            client=client,
            agent_name="openai-agent-helper",
        )

        assert run_options["max_turns"] == 3
        hooks = run_options["hooks"]
        assert isinstance(hooks, SigilOpenAIAgentsRunHooks)
        run_hooks_origin = getattr(RunHooks, "__origin__", None)
        assert run_hooks_origin is not None
        assert isinstance(hooks, run_hooks_origin)

        async def _run() -> None:
            context = _Context()
            agent = _Agent()
            await hooks.on_agent_start(context, agent)
            await hooks.on_llm_start(context, agent, None, [{"role": "user", "content": "hello"}])
            await hooks.on_llm_end(
                context,
                agent,
                {
                    "output": [{"role": "assistant", "content": "world"}],
                    "usage": {"input_tokens": 2, "output_tokens": 1, "total_tokens": 3},
                },
            )
            await hooks.on_agent_end(context, agent, {"content": "world"})

        asyncio.run(_run())
        client.flush()
        generation = exporter.requests[0].generations[0]
        assert generation.conversation_id == "helper-conversation-42"
        assert generation.metadata["sigil.framework.thread_id"] == "helper-thread-42"
        assert existing.started == 1
        assert existing.ended == 1

        with pytest.raises(TypeError):
            with_sigil_openai_agents_hooks({"hooks": [existing]}, client=client)
    finally:
        client.shutdown()


def test_agento11y_openai_agents_handler_explicitly_has_no_embedding_lifecycle() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        handler = SigilOpenAIAgentsHandler(client=client)
        assert not hasattr(handler, "on_embedding_start")
        assert not hasattr(handler, "on_embedding_end")
        assert not hasattr(handler, "on_embedding_error")
    finally:
        client.shutdown()
