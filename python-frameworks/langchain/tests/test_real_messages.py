"""Regression coverage using LangChain's public message and tool APIs."""

from uuid import uuid4

import pytest
from agento11y_langchain import Agento11yLangChainHandler
from langchain_core.messages import AIMessage, AIMessageChunk, HumanMessage, ToolMessage
from langchain_core.outputs import ChatGeneration, ChatGenerationChunk, LLMResult
from langchain_core.tools import tool
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
from test_langchain_handler import _CapturingExporter, _new_client


@pytest.fixture
def recording():
    spans = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(spans))
    exporter = _CapturingExporter()
    client = _new_client(exporter, provider.get_tracer(__name__))
    handler = Agento11yLangChainHandler(client=client, conversation_id="conversation")
    handler.raise_error = True
    yield handler, client, exporter, spans, provider
    client.shutdown()
    provider.shutdown()


def test_tool_call_id_from_real_tool_invocation(recording):
    handler, _, _, spans, _ = recording

    @tool
    def add(a: int, b: int) -> int:
        """Add two integers."""
        return a + b

    result = add.invoke(
        {"type": "tool_call", "id": "call_add", "name": "add", "args": {"a": 1, "b": 2}},
        config={"callbacks": [handler]},
    )
    assert isinstance(result, ToolMessage)
    (span,) = spans.get_finished_spans()
    assert span.attributes["gen_ai.tool.name"] == "add"
    assert span.attributes["gen_ai.tool.call.id"] == result.tool_call_id


def test_callback_parenting_without_active_span(recording):
    handler, client, _, spans, _ = recording
    root, child, model, tool_run = [uuid4() for _ in range(4)]
    handler.on_chain_start({"name": "agent"}, {}, run_id=root)
    handler.on_chain_start({"name": "model"}, {}, run_id=child, parent_run_id=root)
    handler.on_chat_model_start(
        {"name": "ChatOpenAI"},
        [[HumanMessage(content="hello")]],
        run_id=model,
        parent_run_id=child,
        invocation_params={"model": "gpt-4.1-mini"},
    )
    handler.on_llm_end(LLMResult(generations=[[ChatGeneration(message=AIMessage(content="hello"))]]), run_id=model)
    handler.on_tool_start({"name": "add"}, "{}", run_id=tool_run, parent_run_id=root)
    handler.on_tool_end(ToolMessage(content="3", tool_call_id="call_add"), run_id=tool_run)
    handler.on_chain_end({}, run_id=child)
    handler.on_chain_end({}, run_id=root)
    client.flush()
    recorded = spans.get_finished_spans()
    assert len({s.context.trace_id for s in recorded}) == 1
    by_name = {s.name: s for s in recorded}
    root_span = by_name["langchain.chain agent"]
    child_span = by_name["langchain.chain model"]
    assert child_span.parent.span_id == root_span.context.span_id
    assert (
        next(s for s in recorded if s.attributes.get("gen_ai.request.model") == "gpt-4.1-mini").parent.span_id
        == child_span.context.span_id
    )
    assert next(s for s in recorded if s.name.startswith("execute_tool ")).parent.span_id == root_span.context.span_id


@pytest.mark.parametrize("call_id", [None, "", "  "])
def test_missing_tool_call_id_is_not_invented(recording, call_id):
    handler, _, _, spans, _ = recording
    run = uuid4()
    handler.on_tool_start({"name": "add"}, "{}", run_id=run, tool_call_id=call_id)
    handler.on_tool_end("3", run_id=run)
    (span,) = spans.get_finished_spans()
    assert "gen_ai.tool.call.id" not in span.attributes


def test_completed_text_chunk_keeps_assistant_role(recording):
    handler, client, exporter, _, _ = recording
    run = uuid4()
    handler.on_chat_model_start(
        {"name": "ChatOpenAI"},
        [[HumanMessage(content="hello")]],
        run_id=run,
        invocation_params={"model": "gpt-4.1-mini"},
    )
    handler.on_llm_end(
        LLMResult(generations=[[ChatGenerationChunk(message=AIMessageChunk(content="hello"))]]), run_id=run
    )
    client.flush()
    assert exporter.requests[0].generations[0].output[0].role.value == "assistant"


def test_real_tool_messages_and_anthropic_blocks(recording):
    handler, client, exporter, _, _ = recording
    message = AIMessage(
        content=[
            {"type": "text", "text": "Using add."},
            {"type": "tool_use", "id": "call_add", "name": "add", "input": {"a": 1, "b": 2}},
        ],
        tool_calls=[{"id": "call_add", "name": "add", "args": {"a": 1, "b": 2}}],
    )
    run = uuid4()
    handler.on_chat_model_start(
        {"name": "ChatAnthropic"},
        [
            [
                HumanMessage(content="add"),
                message,
                ToolMessage(content="3", name="add", tool_call_id="call_add"),
            ]
        ],
        run_id=run,
        invocation_params={"model": "claude-haiku-4-5"},
    )
    handler.on_llm_end(LLMResult(generations=[[ChatGeneration(message=message)]]), run_id=run)
    client.flush()
    generation = exporter.requests[0].generations[0]
    calls = [p.tool_call for p in generation.output[0].parts if p.tool_call]
    assert len(calls) == 1
    assert calls[0].id == "call_add"
    assert generation.input[-1].parts[0].tool_result.tool_call_id == "call_add"


@pytest.mark.parametrize("normalized", [False, True])
def test_thinking_and_tool_use_blocks(recording, normalized):
    handler, client, exporter, _, _ = recording
    message = AIMessage(
        content=[
            {"type": "thinking", "thinking": "Use addition.", "signature": "opaque"},
            {"type": "text", "text": "Adding."},
            {"type": "tool_use", "id": "call_add", "name": "add", "input": {"a": 1, "b": 2}},
        ],
        tool_calls=[{"id": "call_add", "name": "add", "args": {"a": 1, "b": 2}}] if normalized else [],
    )
    run = uuid4()
    handler.on_chat_model_start(
        {"name": "ChatAnthropic"}, [[message]], run_id=run, invocation_params={"model": "claude-haiku-4-5"}
    )
    handler.on_llm_end(LLMResult(generations=[[ChatGeneration(message=message)]]), run_id=run)
    client.flush()
    generation = exporter.requests[0].generations[0]
    for mapped in [generation.input[0], generation.output[0]]:
        assert [p.kind.value for p in mapped.parts] == ["thinking", "text", "tool_call"]
        assert [p.thinking for p in mapped.parts if p.kind.value == "thinking"] == ["Use addition."]
        calls = [p.tool_call for p in mapped.parts if p.tool_call]
        assert len(calls) == 1
        assert calls[0].id == "call_add"
        assert calls[0].input_json == b'{"a": 1, "b": 2}'


def test_interleaved_blocks_preserve_order_and_normalized_tool_arguments(recording):
    handler, client, exporter, _, _ = recording
    message = AIMessage(
        content=[
            {"type": "thinking", "thinking": "First step."},
            {"type": "text", "text": "Before."},
            {"type": "tool_use", "id": "call_add", "name": "add", "input": {"a": 0}},
            {"type": "text", "text": "After."},
            {"type": "thinking", "thinking": "Next step."},
        ],
        tool_calls=[{"id": "call_add", "name": "add", "args": {"a": 1, "b": 2}}],
    )
    run = uuid4()
    handler.on_chat_model_start(
        {"name": "ChatAnthropic"}, [[message]], run_id=run, invocation_params={"model": "claude-haiku-4-5"}
    )
    handler.on_llm_end(LLMResult(generations=[[ChatGeneration(message=message)]]), run_id=run)
    client.flush()
    generation = exporter.requests[0].generations[0]
    for mapped in [generation.input[0], generation.output[0]]:
        assert [p.kind.value for p in mapped.parts] == ["thinking", "text", "tool_call", "text", "thinking"]
        assert [p.text for p in mapped.parts if p.kind.value == "text"] == ["Before.", "After."]
        assert mapped.parts[2].tool_call.input_json == b'{"a": 1, "b": 2}'


def test_streamed_content_blocks(recording):
    handler, client, exporter, _, _ = recording
    run = uuid4()
    handler.on_chat_model_start(
        {"name": "ChatAnthropic"},
        [[HumanMessage(content="add")]],
        run_id=run,
        invocation_params={"model": "claude-haiku-4-5", "streaming": True},
    )
    chunks = [
        AIMessageChunk(content=[{"type": "text", "text": "Adding.", "index": 0}]),
        AIMessageChunk(content=[], tool_call_chunks=[{"name": "add", "id": "call_add", "args": '{"a":', "index": 0}]),
        AIMessageChunk(content=[], tool_call_chunks=[{"name": None, "id": None, "args": '1,"b":2}', "index": 0}]),
    ]
    for chunk in chunks:
        handler.on_llm_new_token(chunk.content, run_id=run, chunk=ChatGenerationChunk(message=chunk))
    combined = chunks[0] + chunks[1] + chunks[2]
    handler.on_llm_end(LLMResult(generations=[[ChatGenerationChunk(message=combined)]]), run_id=run)
    client.flush()
    generation = exporter.requests[0].generations[0]
    assert generation.output[0].parts[0].text == "Adding."
    assert generation.output[0].parts[1].tool_call.id == "call_add"


@pytest.mark.parametrize(
    "details,expected_read,expected_write",
    [
        ({"cache_read": 70, "cache_creation": 0}, 70, 0),
        ({"cache_read": 0, "cache_creation": 70}, 0, 70),
        (
            {"cache_read": 0, "cache_creation": 0, "ephemeral_5m_input_tokens": 50, "ephemeral_1h_input_tokens": 20},
            0,
            70,
        ),
    ],
)
def test_standardized_usage_precedes_provider_usage(recording, details, expected_read, expected_write):
    handler, client, exporter, _, _ = recording
    run = uuid4()
    handler.on_chat_model_start(
        {"name": "ChatAnthropic"},
        [[HumanMessage(content="hello")]],
        run_id=run,
        invocation_params={"model": "claude-haiku-4-5"},
    )
    message = AIMessage(
        content="hello",
        usage_metadata={
            "input_tokens": 100,
            "output_tokens": 10,
            "total_tokens": 110,
            "input_token_details": details,
            "output_token_details": {"reasoning": 5},
        },
    )
    handler.on_llm_end(
        LLMResult(
            generations=[[ChatGeneration(message=message)]],
            llm_output={
                "usage": {
                    "input_tokens": 30,
                    "output_tokens": 10,
                    "cache_read_input_tokens": expected_read,
                    "output_tokens_details": {"thinking_tokens": 5},
                },
            },
        ),
        run_id=run,
    )
    client.flush()
    usage = exporter.requests[0].generations[0].usage
    assert usage.input_tokens == 100
    assert usage.total_tokens == 110
    assert usage.cache_read_input_tokens == expected_read
    assert usage.cache_write_input_tokens == expected_write
    assert usage.reasoning_tokens == 5
