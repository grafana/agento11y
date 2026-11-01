"""openai-agents handler lifecycle and conversation-mapping tests."""

from __future__ import annotations

import asyncio
import json
from datetime import timedelta
from types import SimpleNamespace
from unittest.mock import Mock
from uuid import uuid4

import pytest
from agento11y import Client, ClientConfig, ContentCaptureMode, GenerationExportConfig, with_content_capture_mode
from agento11y.models import ExportGenerationResult, ExportGenerationsResponse
from agento11y_openai_agents import (
    Agento11yAsyncOpenAIAgentsHandler,
    Agento11yOpenAIAgentsHandler,
    Agento11yOpenAIAgentsRunHooks,
    create_agento11y_openai_agents_handler,
    with_agento11y_openai_agents_hooks,
)
from agents import RunHooks
from agents.run_context import AgentHookContext, RunContextWrapper
from agents.tool_context import ToolContext
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


def _agent_hook_context(context: RunContextWrapper) -> AgentHookContext:
    hook_context = AgentHookContext(context=context.context, usage=context.usage)
    context._share_tool_state_with(hook_context)
    return hook_context


def test_agento11y_openai_agents_sync_lifecycle_sets_framework_metadata() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    try:
        run_id = uuid4()
        parent_run_id = uuid4()
        handler = Agento11yOpenAIAgentsHandler(client=client, provider_resolver="auto")

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
        assert generation.tags["agento11y.framework.name"] == "openai-agents"
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


def test_agento11y_openai_agents_keeps_thread_metadata_when_ids_are_split_across_payloads() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    try:
        run_id = uuid4()
        handler = Agento11yOpenAIAgentsHandler(client=client, provider_resolver="auto")

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


def test_agento11y_openai_agents_fallback_conversation_is_deterministic() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    try:
        run_id = uuid4()
        handler = Agento11yOpenAIAgentsHandler(client=client)

        handler.on_llm_start(
            {"kwargs": {"model": "gpt-5"}},
            ["hello"],
            run_id=run_id,
            invocation_params={"model": "gpt-5"},
        )
        handler.on_llm_end({"generations": [[{"text": "ok"}]], "llm_output": {"model_name": "gpt-5"}}, run_id=run_id)

        client.flush()
        generation = exporter.requests[0].generations[0]
        assert generation.conversation_id == f"agento11y:framework:openai-agents:{run_id}"
    finally:
        client.shutdown()


def test_agento11y_openai_agents_stream_mode_uses_chunks_when_output_missing() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    try:
        run_id = uuid4()
        handler = Agento11yOpenAIAgentsHandler(client=client)

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
    tracer = provider.get_tracer("agento11y-framework-test")
    client = _new_client(exporter, tracer=tracer)

    try:
        run_id = uuid4()
        with tracer.start_as_current_span("framework.request"):
            handler = Agento11yOpenAIAgentsHandler(client=client, provider_resolver="auto")
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
        handler = Agento11yOpenAIAgentsHandler(
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
        handler = Agento11yAsyncOpenAIAgentsHandler(client=client)
        await handler.on_llm_start({}, ["hello"], run_id=run_id, invocation_params={"model": "gpt-5"})
        await handler.on_llm_end({"generations": [[{"text": "world"}]]}, run_id=run_id)

    try:
        asyncio.run(_run())
        client.flush()
        generation = exporter.requests[0].generations[0]
        assert generation.tags["agento11y.framework.name"] == "openai-agents"
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
        created = create_agento11y_openai_agents_handler(client=client)
        assert isinstance(created, Agento11yOpenAIAgentsHandler)

        existing = _ExistingHooks()
        run_options = with_agento11y_openai_agents_hooks(
            {"hooks": existing, "max_turns": 3},
            client=client,
            agent_name="openai-agent-helper",
        )

        assert run_options["max_turns"] == 3
        hooks = run_options["hooks"]
        assert isinstance(hooks, Agento11yOpenAIAgentsRunHooks)
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
        assert generation.metadata["agento11y.framework.thread_id"] == "helper-thread-42"
        assert existing.started == 1
        assert existing.ended == 1

        with pytest.raises(TypeError):
            with_agento11y_openai_agents_hooks({"hooks": [existing]}, client=client)
    finally:
        client.shutdown()


@pytest.mark.parametrize("payload", [None, {"conversation_id": "shared-conversation"}])
def test_distinct_callback_wrappers_share_lineage_and_keep_runs_separate(payload) -> None:
    exporter = _CapturingExporter()
    span_exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(span_exporter))
    client = _new_client(exporter, tracer=provider.get_tracer("wrapper-lineage-test"))
    hooks = with_agento11y_openai_agents_hooks(None, client=client)["hooks"]
    agent = SimpleNamespace(name="support", model="gpt-4o-mini")
    tool = SimpleNamespace(name="lookup_order")
    # Independent Runner.run calls may reuse the same application context.
    contexts = [RunContextWrapper(context=payload), RunContextWrapper(context=payload)]

    async def run() -> None:
        for context in contexts:
            await hooks.on_agent_start(_agent_hook_context(context), agent)
            await hooks.on_llm_start(context, agent, None, [{"role": "user", "content": "Order status?"}])
        for index, context in enumerate(contexts):
            await hooks.on_llm_end(
                context,
                agent,
                {"output": [{"content": "Shipped."}], "usage": {"input_tokens": 2, "output_tokens": 1}},
            )
            tool_context = ToolContext.from_agent_context(
                context,
                tool_name="lookup_order",
                tool_call_id=f"call-{index}",
                tool_arguments="{}",
            )
            await hooks.on_tool_start(tool_context, agent, tool)
            await hooks.on_tool_end(tool_context, agent, tool, "Shipped.")
            await hooks.on_agent_end(_agent_hook_context(context), agent, "Shipped.")

    try:
        asyncio.run(run())
        client.flush()
        generations = [generation for request in exporter.requests for generation in request.generations]
        spans = span_exporter.get_finished_spans()
        agent_spans = [span for span in spans if span.attributes.get("agento11y.framework.run_type") == "agent"]
        tool_spans = [span for span in spans if span.attributes.get("gen_ai.operation.name") == "execute_tool"]
        assert len(generations) == len(agent_spans) == len(tool_spans) == 2
        parent_ids = []
        for generation, agent_span in zip(generations, agent_spans, strict=True):
            parent_id = agent_span.attributes["agento11y.framework.run_id"]
            assert generation.metadata["agento11y.framework.parent_run_id"] == parent_id
            parent_ids.append(parent_id)
        assert parent_ids[0] != parent_ids[1]
        assert not hooks._agent_run_stacks
        assert not hooks._llm_run_stacks
        assert not hooks._tool_run_stacks
        assert not hooks._agent_runs_by_usage
        assert not hooks._tool_runs_by_usage
    finally:
        client.shutdown()
        provider.shutdown()


@pytest.mark.parametrize("end_order", [(0, 1), (1, 0)])
@pytest.mark.parametrize("active_llm", [False, True])
def test_overlapping_calls_of_same_tool_close_with_their_own_results(end_order, active_llm) -> None:
    exporter = _CapturingExporter()
    span_exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(span_exporter))
    client = _new_client(exporter, tracer=provider.get_tracer("overlapping-tools-test"))
    handler = Mock(wraps=Agento11yOpenAIAgentsHandler(client=client))
    hooks = Agento11yOpenAIAgentsRunHooks(agento11y_handler=handler)
    context = RunContextWrapper(context=None)
    agent = SimpleNamespace(name="support", model="gpt-4o-mini")
    tool = SimpleNamespace(name="lookup_order")
    tool_contexts = [
        ToolContext.from_agent_context(context, tool_call_id=f"call-{index}", tool_name=tool.name, tool_arguments="{}")
        for index in range(2)
    ]
    for index, tool_context in enumerate(tool_contexts):
        tool_context.tool_input = {"order": index}

    async def run() -> None:
        await hooks.on_agent_start(_agent_hook_context(context), agent)
        if active_llm:
            await hooks.on_llm_start(context, agent, None, [])
        for tool_context in tool_contexts:
            await hooks.on_tool_start(tool_context, agent, tool)
        for index in end_order:
            await hooks.on_tool_end(tool_contexts[index], agent, tool, f"result-{index}")
        if active_llm:
            await hooks.on_llm_end(context, agent, {"output": [{"content": "Done."}]})
        await hooks.on_agent_end(_agent_hook_context(context), agent, "Done.")

    try:
        with with_content_capture_mode(ContentCaptureMode.FULL):
            asyncio.run(run())
        starts = handler.on_tool_start.call_args_list
        assert starts[0].kwargs["run_id"] != starts[1].kwargs["run_id"]
        parent = handler.on_llm_start.call_args if active_llm else handler.on_chain_start.call_args
        assert all(start.kwargs["parent_run_id"] == parent.kwargs["run_id"] for start in starts)
        for end, index in zip(handler.on_tool_end.call_args_list, end_order, strict=True):
            assert end.kwargs["run_id"] == starts[index].kwargs["run_id"]
            assert end.args == (f"result-{index}",)
        tool_spans = [
            span
            for span in span_exporter.get_finished_spans()
            if span.attributes.get("gen_ai.operation.name") == "execute_tool"
        ]
        assert len(tool_spans) == 2
        assert {
            json.loads(span.attributes["gen_ai.tool.call.arguments"])["order"]: json.loads(
                span.attributes["gen_ai.tool.call.result"]
            )
            for span in tool_spans
        } == {0: "result-0", 1: "result-1"}
        assert not hooks._agent_run_stacks
        assert not hooks._llm_run_stacks
        assert not hooks._tool_run_stacks
    finally:
        client.shutdown()
        provider.shutdown()


@pytest.mark.parametrize("end_order", [(0, 1), (1, 0)])
def test_overlapping_nested_agents_sharing_usage_close_their_own_spans(end_order) -> None:
    exporter = _CapturingExporter()
    span_exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(span_exporter))
    client = _new_client(exporter, tracer=provider.get_tracer("overlapping-agents-test"))
    handler = Mock(wraps=Agento11yOpenAIAgentsHandler(client=client))
    hooks = Agento11yOpenAIAgentsRunHooks(agento11y_handler=handler)
    parent = SimpleNamespace(name="parent", model="gpt-4o-mini")
    # Agent tools share Usage with the outer run, even across distinct wrappers.
    context = RunContextWrapper(context=None)
    agents = [SimpleNamespace(name=f"child-{index}", model="gpt-4o-mini") for index in range(2)]
    contexts = [RunContextWrapper(context=None, usage=context.usage) for _ in agents]

    async def run() -> None:
        await hooks.on_agent_start(_agent_hook_context(context), parent)
        for child_context, agent in zip(contexts, agents, strict=True):
            await hooks.on_agent_start(_agent_hook_context(child_context), agent)
        # Both children are active when the first LLM callback arrives.
        for index in end_order:
            await hooks.on_llm_start(contexts[index], agents[index], None, [])
            await hooks.on_llm_end(contexts[index], agents[index], {"output": [{"content": f"result-{index}"}]})
            await hooks.on_agent_end(_agent_hook_context(contexts[index]), agents[index], f"result-{index}")
        await hooks.on_llm_start(context, parent, None, [])
        await hooks.on_llm_end(context, parent, {"output": [{"content": "Done."}]})
        await hooks.on_agent_end(_agent_hook_context(context), parent, "Done.")

    try:
        asyncio.run(run())
        client.flush()
        starts = {call.kwargs["run_name"]: call.kwargs["run_id"] for call in handler.on_chain_start.call_args_list}
        for end, index in zip(handler.on_chain_end.call_args_list[:2], end_order, strict=True):
            assert end.kwargs["run_id"] == starts[f"child-{index}"]
            assert end.args == ({"output": f"result-{index}"},)
        assert handler.on_chain_end.call_args.kwargs["run_id"] == starts["parent"]
        generations = [generation for request in exporter.requests for generation in request.generations]
        assert len(generations) == 3
        for generation, name in zip(generations, [f"child-{index}" for index in end_order] + ["parent"], strict=True):
            assert generation.metadata["agento11y.framework.parent_run_id"] == str(starts[name])
        agent_spans = [
            span
            for span in span_exporter.get_finished_spans()
            if span.attributes.get("agento11y.framework.run_type") == "agent"
        ]
        assert {span.attributes["agento11y.framework.run_id"] for span in agent_spans} == {
            str(run_id) for run_id in starts.values()
        }
        assert not hooks._agent_run_stacks
        assert not hooks._llm_run_stacks
    finally:
        client.shutdown()
        provider.shutdown()


@pytest.mark.parametrize("end_order", [(0, 1), (1, 0)])
@pytest.mark.parametrize("parent_tool", [False, True])
def test_parallel_runs_of_same_agent_tool_keep_their_own_lineage(end_order, parent_tool) -> None:
    exporter = _CapturingExporter()
    span_exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(span_exporter))
    client = _new_client(exporter, tracer=provider.get_tracer("parallel-agent-tools-test"))
    handler = Mock(wraps=Agento11yOpenAIAgentsHandler(client=client))
    hooks = Agento11yOpenAIAgentsRunHooks(agento11y_handler=handler)
    parent = SimpleNamespace(name="parent", model="gpt-4o-mini")
    agent = SimpleNamespace(name="child", model="gpt-4o-mini")
    tool = SimpleNamespace(name="child_tool", _is_agent_tool=True, _agent_instance=agent)
    inner_tool = SimpleNamespace(name="lookup")
    context = RunContextWrapper(context=None)
    tool_contexts = [
        ToolContext.from_agent_context(context, tool_call_id=f"call-{index}", tool_name=tool.name, tool_arguments="{}")
        for index in range(2)
    ]
    # Agent.as_tool copies these fields into a fresh wrapper for each nested run.
    # Usage is shared, but tool state is separate from the parent and sibling.
    contexts = [
        ToolContext(
            context=tool_context.context,
            usage=tool_context.usage,
            tool_call_id=tool_context.tool_call_id,
            tool_name=tool_context.tool_name,
            tool_arguments=tool_context.tool_arguments,
        )
        for tool_context in tool_contexts
    ]
    inner_contexts = [
        ToolContext.from_agent_context(child, tool_call_id="lookup", tool_name=inner_tool.name, tool_arguments="{}")
        for child in contexts
    ]

    async def run() -> None:
        await hooks.on_agent_start(_agent_hook_context(context), parent)
        if parent_tool:
            for tool_context in tool_contexts:
                await hooks.on_tool_start(tool_context, parent, tool)
        for child in contexts:
            await hooks.on_agent_start(_agent_hook_context(child), agent)
        # Both LLM calls overlap, and tool call IDs may repeat across nested runs.
        for child, inner_context in zip(contexts, inner_contexts, strict=True):
            await hooks.on_llm_start(child, agent, None, [])
            await hooks.on_tool_start(inner_context, agent, inner_tool)
        for index in end_order:
            await hooks.on_tool_end(inner_contexts[index], agent, inner_tool, f"lookup-{index}")
            await hooks.on_llm_end(contexts[index], agent, {"output": [{"content": f"result-{index}"}]})
            # A later LLM callback must still find this run after its sibling ends.
            await hooks.on_llm_start(contexts[index], agent, None, [])
            await hooks.on_llm_end(contexts[index], agent, {"output": [{"content": f"followup-{index}"}]})
            await hooks.on_agent_end(_agent_hook_context(contexts[index]), agent, f"result-{index}")
            if parent_tool:
                await hooks.on_tool_end(tool_contexts[index], parent, tool, f"result-{index}")
        await hooks.on_agent_end(_agent_hook_context(context), parent, "Done.")

    try:
        asyncio.run(run())
        client.flush()
        starts = handler.on_chain_start.call_args_list
        parent_id = starts[0].kwargs["run_id"]
        child_ids = [start.kwargs["run_id"] for start in starts[1:]]
        assert len(set(child_ids)) == 2
        tool_starts = handler.on_tool_start.call_args_list
        expected_parents = [start.kwargs["run_id"] for start in tool_starts[:2]] if parent_tool else [parent_id] * 2
        for start, expected_parent in zip(starts[1:], expected_parents, strict=True):
            assert start.kwargs["parent_run_id"] == expected_parent
        for end, index in zip(handler.on_chain_end.call_args_list[:2], end_order, strict=True):
            assert end.kwargs["run_id"] == child_ids[index]
            assert end.args == ({"output": f"result-{index}"},)
        llm_starts = handler.on_llm_start.call_args_list
        assert [start.kwargs["parent_run_id"] for start in llm_starts[:2]] == child_ids
        inner_starts = tool_starts[2:] if parent_tool else tool_starts
        for start, llm_start in zip(inner_starts, llm_starts[:2], strict=True):
            assert start.kwargs["parent_run_id"] == llm_start.kwargs["run_id"]
        inner_ends = [end for end in handler.on_tool_end.call_args_list if end.args[0].startswith("lookup-")]
        for end, index in zip(inner_ends, end_order, strict=True):
            assert end.kwargs["run_id"] == inner_starts[index].kwargs["run_id"]
        generations = [generation for request in exporter.requests for generation in request.generations]
        assert len(generations) == 4
        spans = {
            span.attributes["agento11y.framework.run_id"]: span
            for span in span_exporter.get_finished_spans()
            if "agento11y.framework.run_id" in span.attributes
        }
        assert len(spans) == 7  # Three agent spans and four LLM spans all closed.
        for index in range(2):
            child_span = spans[str(child_ids[index])]
            assert child_span.attributes["agento11y.framework.parent_run_id"] == str(expected_parents[index])
            for prefix in ("result", "followup"):
                generation = next(g for g in generations if g.output[0].parts[0].text == f"{prefix}-{index}")
                assert generation.metadata["agento11y.framework.parent_run_id"] == str(child_ids[index])
                llm_span = spans[generation.metadata["agento11y.framework.run_id"]]
                assert llm_span.attributes["agento11y.framework.parent_run_id"] == str(child_ids[index])
        assert not hooks._agent_run_stacks
        assert not hooks._llm_run_stacks
        assert not hooks._tool_run_stacks
        assert not hooks._agent_runs_by_usage
        assert not hooks._tool_runs_by_usage
    finally:
        client.shutdown()
        provider.shutdown()


@pytest.mark.parametrize("start_second_tool_early", [False, True])
def test_sibling_agent_tools_keep_their_parents_when_inner_tools_overlap(start_second_tool_early) -> None:
    handler = Mock()
    hooks = Agento11yOpenAIAgentsRunHooks(agento11y_handler=handler)
    context = RunContextWrapper(context=None)
    parent = SimpleNamespace(name="parent")
    agents = [SimpleNamespace(name=f"child-{index}") for index in range(2)]
    tools = [
        SimpleNamespace(name=f"child_tool_{index}", _is_agent_tool=True, _agent_instance=agent)
        for index, agent in enumerate(agents)
    ]
    tool_contexts = [
        ToolContext.from_agent_context(context, tool_call_id=f"call-{index}", tool_name=tool.name, tool_arguments="{}")
        for index, tool in enumerate(tools)
    ]
    contexts = [RunContextWrapper(context=None, usage=context.usage) for _ in agents]
    first, second = (1, 0) if start_second_tool_early else (0, 1)
    inner_tool = SimpleNamespace(name="lookup")
    inner_context = ToolContext.from_agent_context(
        contexts[first], tool_call_id="lookup", tool_name=inner_tool.name, tool_arguments="{}"
    )

    async def run() -> None:
        await hooks.on_agent_start(_agent_hook_context(context), parent)
        await hooks.on_tool_start(tool_contexts[0], parent, tools[0])
        if start_second_tool_early:
            await hooks.on_tool_start(tool_contexts[1], parent, tools[1])
        await hooks.on_agent_start(_agent_hook_context(contexts[first]), agents[first])
        await hooks.on_tool_start(inner_context, agents[first], inner_tool)
        if not start_second_tool_early:
            await hooks.on_tool_start(tool_contexts[1], parent, tools[1])
        # The sibling starts while its cousin's ordinary tool is still active.
        await hooks.on_agent_start(_agent_hook_context(contexts[second]), agents[second])
        await hooks.on_tool_end(inner_context, agents[first], inner_tool, "Found.")
        for index in (first, second):
            await hooks.on_agent_end(_agent_hook_context(contexts[index]), agents[index], "Done.")
            await hooks.on_tool_end(tool_contexts[index], parent, tools[index], "Done.")
        await hooks.on_agent_end(_agent_hook_context(context), parent, "Done.")

    asyncio.run(run())
    tool_starts = {call.kwargs["run_name"]: call.kwargs["run_id"] for call in handler.on_tool_start.call_args_list}
    agent_starts = {call.kwargs["run_name"]: call for call in handler.on_chain_start.call_args_list}
    for index in range(2):
        assert agent_starts[f"child-{index}"].kwargs["parent_run_id"] == tool_starts[f"child_tool_{index}"]
    inner_start = next(call for call in handler.on_tool_start.call_args_list if call.kwargs["run_name"] == "lookup")
    assert inner_start.kwargs["parent_run_id"] == agent_starts[f"child-{first}"].kwargs["run_id"]
    assert not hooks._agent_run_stacks
    assert not hooks._llm_run_stacks
    assert not hooks._tool_run_stacks
    assert not hooks._agent_runs_by_usage
    assert not hooks._tool_runs_by_usage


@pytest.mark.parametrize("tool_metadata", ["legacy", "marker_only", "instance_only", "ended"])
def test_nested_agent_falls_back_when_no_agent_tool_is_claimable(tool_metadata) -> None:
    handler = Mock()
    hooks = Agento11yOpenAIAgentsRunHooks(agento11y_handler=handler)
    context = RunContextWrapper(context=None)
    child_context = RunContextWrapper(context=None, usage=context.usage)
    parent = SimpleNamespace(name="parent")
    child = SimpleNamespace(name="child")
    tool = SimpleNamespace(name="child_tool")
    if tool_metadata in ("marker_only", "ended"):
        tool._is_agent_tool = True
    if tool_metadata in ("instance_only", "ended"):
        tool._agent_instance = child

    async def run() -> None:
        await hooks.on_agent_start(_agent_hook_context(context), parent)
        await hooks.on_tool_start(context, parent, tool)
        if tool_metadata == "ended":
            await hooks.on_tool_end(context, parent, tool, "Done.")
        assert not hooks._tool_runs_by_usage
        await hooks.on_agent_start(_agent_hook_context(child_context), child)
        await hooks.on_agent_end(_agent_hook_context(child_context), child, "Done.")
        if tool_metadata != "ended":
            await hooks.on_tool_end(context, parent, tool, "Done.")
        # Unmatched end callbacks must also be harmless.
        await hooks.on_tool_end(context, parent, tool, "Done.")
        await hooks.on_agent_end(_agent_hook_context(context), parent, "Done.")

    asyncio.run(run())
    starts = handler.on_chain_start.call_args_list
    assert starts[1].kwargs["parent_run_id"] == starts[0].kwargs["run_id"]
    assert not hooks._agent_run_stacks
    assert not hooks._tool_run_stacks
    assert not hooks._agent_runs_by_usage
    assert not hooks._tool_runs_by_usage


@pytest.mark.parametrize("shared_usage", [False, True])
def test_callbacks_without_tool_state_keep_legacy_correlation(shared_usage) -> None:
    handler = Mock()
    hooks = Agento11yOpenAIAgentsRunHooks(agento11y_handler=handler)
    context = SimpleNamespace(context=None)
    end_context = context
    if shared_usage:
        context.usage = object()
        end_context = SimpleNamespace(context=None, usage=context.usage)
    agent = SimpleNamespace(name="agent", model="gpt-4o-mini")
    tool = SimpleNamespace(name="lookup")

    async def run() -> None:
        await hooks.on_agent_start(context, agent)
        await hooks.on_llm_start(context, agent, None, [])
        await hooks.on_llm_end(end_context, agent, {"output": [{"content": "Done."}]})
        await hooks.on_tool_start(context, agent, tool)
        await hooks.on_tool_end(end_context, agent, tool, "Done.")
        await hooks.on_agent_end(end_context, agent, "Done.")

    asyncio.run(run())
    agent_id = handler.on_chain_start.call_args.kwargs["run_id"]
    assert handler.on_chain_end.call_args.kwargs["run_id"] == agent_id
    assert handler.on_llm_start.call_args.kwargs["parent_run_id"] == agent_id
    assert handler.on_llm_end.call_args.kwargs["run_id"] == handler.on_llm_start.call_args.kwargs["run_id"]
    assert handler.on_tool_start.call_args.kwargs["parent_run_id"] == agent_id
    assert handler.on_tool_end.call_args.kwargs["run_id"] == handler.on_tool_start.call_args.kwargs["run_id"]
    assert not hooks._agent_run_stacks
    assert not hooks._llm_run_stacks
    assert not hooks._tool_run_stacks
    assert not hooks._agent_runs_by_usage
    assert not hooks._tool_runs_by_usage


def test_agento11y_openai_agents_handler_explicitly_has_no_embedding_lifecycle() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        handler = Agento11yOpenAIAgentsHandler(client=client)
        assert not hasattr(handler, "on_embedding_start")
        assert not hasattr(handler, "on_embedding_end")
        assert not hasattr(handler, "on_embedding_error")
    finally:
        client.shutdown()
