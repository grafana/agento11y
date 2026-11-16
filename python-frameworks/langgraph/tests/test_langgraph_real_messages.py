"""Real message coverage for async graph callbacks and concurrent roots."""

import asyncio
from uuid import uuid4

from agento11y_langgraph import Agento11yAsyncLangGraphHandler
from langchain_core.messages import AIMessageChunk, HumanMessage, ToolMessage
from langchain_core.outputs import ChatGenerationChunk, LLMResult
from opentelemetry import trace
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
from test_langgraph_handler import _CapturingExporter, _new_client


def test_async_graph_chunks_tool_ids_and_isolated_parents():
    spans = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(spans))
    exporter = _CapturingExporter()
    client = _new_client(exporter, provider.get_tracer(__name__))
    handler = Agento11yAsyncLangGraphHandler(client=client, conversation_id="conversation")

    async def run():
        roots = [uuid4(), uuid4()]
        for root in roots:
            await handler.on_chain_start({"name": "graph"}, {}, run_id=root)

        async def turn(root):
            run_id, tool_id = uuid4(), uuid4()
            call_id = str(tool_id)
            await handler.on_chat_model_start(
                {"name": "ChatAnthropic"},
                [[HumanMessage(content="add")]],
                run_id=run_id,
                parent_run_id=root,
                invocation_params={"model": "claude-haiku-4-5", "streaming": True},
            )
            message = AIMessageChunk(
                content=[{"type": "text", "text": "Adding."}],
                tool_calls=[{"id": call_id, "name": "add", "args": {"a": 1, "b": 2}}],
            )
            await handler.on_llm_new_token(message.content, run_id=run_id)
            await asyncio.sleep(0)
            await handler.on_llm_end(LLMResult(generations=[[ChatGenerationChunk(message=message)]]), run_id=run_id)
            await handler.on_tool_start({"name": "add"}, "{}", run_id=tool_id, parent_run_id=root, tool_call_id=call_id)
            await handler.on_tool_end(ToolMessage(content="3", tool_call_id=call_id), run_id=tool_id)
            await handler.on_chain_end({}, run_id=root)
            assert not trace.get_current_span().get_span_context().is_valid

        await asyncio.gather(*(turn(root) for root in roots))

    try:
        asyncio.run(run())
        client.flush()
        recorded = spans.get_finished_spans()
        assert len(recorded) == 6
        assert len({s.context.trace_id for s in recorded}) == 2
        for generation in exporter.requests[0].generations:
            assert generation.output[0].role.value == "assistant"
            call = generation.output[0].parts[1].tool_call
            tool_span = next(s for s in recorded if s.attributes.get("gen_ai.tool.call.id") == call.id)
            model_span = next(
                s
                for s in recorded
                if s.context.trace_id == tool_span.context.trace_id and s.attributes.get("gen_ai.request.model")
            )
            assert model_span.parent.span_id == tool_span.parent.span_id
    finally:
        client.shutdown()
        provider.shutdown()
