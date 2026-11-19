"""LlamaIndex workflow agents: usage, stop reason, stream mode, tool calls, and tool spans."""

from __future__ import annotations

import asyncio
import json
from datetime import timedelta
from types import SimpleNamespace
from typing import Any

import pytest
from agento11y import Client, ClientConfig, GenerationExportConfig
from agento11y.models import ExportGenerationResult, ExportGenerationsResponse, PartKind
from agento11y_llamaindex import with_agento11y_llamaindex_callbacks
from llama_index.core.agent.workflow import FunctionAgent
from llama_index.core.base.llms.types import ChatMessage, ChatResponse, TextBlock, ToolCallBlock
from llama_index.core.llms.callbacks import llm_chat_callback
from llama_index.core.llms.mock import MockFunctionCallingLLM
from llama_index.core.tools import FunctionTool
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.metrics.export import InMemoryMetricReader
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


def _openai_raw(*, finish_reason: str | None, usage: dict[str, Any] | None) -> Any:
    """Shape of an OpenAI ChatCompletion(Chunk): finish_reason on choices, usage at the top."""
    choices = [] if finish_reason is None else [SimpleNamespace(finish_reason=finish_reason)]
    return SimpleNamespace(model="gpt-4o-mini", choices=choices, usage=usage)


def _anthropic_raw(*, stop_reason: str, input_tokens: int, output_tokens: int) -> dict[str, Any]:
    """Shape of the raw dict LlamaIndex's Anthropic integration puts on each streamed ChatResponse."""
    return {
        "stop_reason": stop_reason,
        "usage": {
            "input_tokens": input_tokens,
            "output_tokens": output_tokens,
            "cache_read_input_tokens": 30,
            "cache_creation_input_tokens": 7,
        },
    }


def _tool_turn_chunks(provider: str) -> list[ChatResponse]:
    calls = [
        ToolCallBlock(tool_call_id="call_weather_1", tool_name="get_weather", tool_kwargs={"city": "Paris"}),
        ToolCallBlock(tool_call_id="call_add_2", tool_name="add", tool_kwargs='{"a": 17, "b": 25}'),
    ]
    if provider == "openai":
        raws = [
            _openai_raw(finish_reason=None, usage=None),
            _openai_raw(finish_reason="tool_calls", usage=None),
            # stream_options={"include_usage": True}: a last chunk with no choices carries usage.
            _openai_raw(
                finish_reason=None,
                usage={"prompt_tokens": 100, "completion_tokens": 20, "prompt_tokens_details": {"cached_tokens": 30}},
            ),
        ]
    else:
        raws = [
            _anthropic_raw(stop_reason="", input_tokens=70, output_tokens=1),
            _anthropic_raw(stop_reason="", input_tokens=70, output_tokens=10),
            _anthropic_raw(stop_reason="tool_use", input_tokens=70, output_tokens=20),
        ]
    return [
        ChatResponse(
            message=ChatMessage(role="assistant", blocks=[TextBlock(text=""), *calls[: i + 1]]), delta="", raw=raw
        )
        for i, raw in enumerate(raws)
    ]


def _answer_turn_chunks(provider: str) -> list[ChatResponse]:
    if provider == "openai":
        last = _openai_raw(
            finish_reason="stop",
            usage={"prompt_tokens": 150, "completion_tokens": 8, "prompt_tokens_details": {"cached_tokens": 30}},
        )
        first = _openai_raw(finish_reason=None, usage=None)
    else:
        last = _anthropic_raw(stop_reason="end_turn", input_tokens=120, output_tokens=8)
        first = _anthropic_raw(stop_reason="", input_tokens=120, output_tokens=1)
    return [
        ChatResponse(message=ChatMessage(role="assistant", content="Sunny"), delta="Sunny", raw=first),
        ChatResponse(message=ChatMessage(role="assistant", content="Sunny, 42."), delta=", 42.", raw=last),
    ]


class _ScriptedLLM(MockFunctionCallingLLM):
    """Function-calling LLM that streams a fixed list of chunks per call."""

    def __init__(self, turns: list[list[ChatResponse]], **kwargs: Any) -> None:
        super().__init__(**kwargs)
        self._turns = list(turns)

    @llm_chat_callback()
    async def astream_chat(self, messages, **kwargs):
        chunks = self._turns.pop(0)

        async def gen():
            for chunk in chunks:
                yield chunk

        return gen()

    @llm_chat_callback()
    def chat(self, messages, **kwargs):
        return self._turns.pop(0)[-1]


def get_weather(city: str) -> str:
    """Get the weather for a city."""
    return f"Sunny in {city}"


def add(a: int, b: int) -> int:
    """Add two integers."""
    return a + b


@pytest.fixture
def recording():
    exporter = _CapturingExporter()
    spans = InMemorySpanExporter()
    tracer_provider = TracerProvider()
    tracer_provider.add_span_processor(SimpleSpanProcessor(spans))
    metrics = InMemoryMetricReader()
    meter_provider = MeterProvider(metric_readers=[metrics])
    client = Client(
        ClientConfig(
            tracer_provider=tracer_provider,
            meter_provider=meter_provider,
            generation_export=GenerationExportConfig(batch_size=10, flush_interval=timedelta(seconds=60)),
            generation_exporter=exporter,
        )
    )
    yield SimpleNamespace(client=client, exporter=exporter, spans=spans, metrics=metrics)
    client.shutdown()


def _generations(exporter: _CapturingExporter) -> list[Any]:
    return [generation for request in exporter.requests for generation in request.generations]


def _ttft_count(metrics: InMemoryMetricReader) -> int:
    data = metrics.get_metrics_data()
    count = 0
    for resource_metrics in data.resource_metrics if data else []:
        for scope_metrics in resource_metrics.scope_metrics:
            for metric in scope_metrics.metrics:
                if metric.name == "gen_ai.client.time_to_first_token":
                    count += sum(point.count for point in metric.data.data_points)
    return count


@pytest.mark.parametrize("provider", ["openai", "anthropic"])
def test_function_agent_records_usage_stop_reason_stream_tool_calls_and_tool_spans(recording, provider: str) -> None:
    llm = _ScriptedLLM([_tool_turn_chunks(provider), _answer_turn_chunks(provider)])
    config = with_agento11y_llamaindex_callbacks(
        None, client=recording.client, agent_name="llamaindex-test", provider=provider
    )
    llm.callback_manager = config["callback_manager"]
    agent = FunctionAgent(
        tools=[FunctionTool.from_defaults(get_weather), FunctionTool.from_defaults(add)],
        llm=llm,
    )

    async def run() -> None:
        await agent.run("Weather in Paris and 17 + 25?")

    asyncio.run(run())
    recording.client.flush()

    tool_turn, answer_turn = _generations(recording.exporter)
    assert tool_turn.mode.value == "STREAM"
    assert answer_turn.mode.value == "STREAM"
    assert _ttft_count(recording.metrics) == 2

    if provider == "openai":
        assert tool_turn.stop_reason == "tool_calls"
        assert (tool_turn.usage.input_tokens, tool_turn.usage.output_tokens) == (100, 20)
        assert tool_turn.usage.cache_read_input_tokens == 30
        assert answer_turn.stop_reason == "stop"
    else:
        assert tool_turn.stop_reason == "tool_use"
        # Anthropic input_tokens exclude the cache buckets; the recorded input includes them.
        assert (tool_turn.usage.input_tokens, tool_turn.usage.output_tokens) == (107, 20)
        assert tool_turn.usage.cache_read_input_tokens == 30
        assert tool_turn.usage.cache_write_input_tokens == 7
        assert answer_turn.stop_reason == "end_turn"

    calls = [part.tool_call for part in tool_turn.output[0].parts if part.kind == PartKind.TOOL_CALL]
    assert [(call.id, call.name) for call in calls] == [("call_weather_1", "get_weather"), ("call_add_2", "add")]
    assert json.loads(calls[1].input_json) == {"a": 17, "b": 25}
    assert answer_turn.output[0].parts[0].text == "Sunny, 42."

    results = [
        part.tool_result for message in answer_turn.input for part in message.parts if part.kind == PartKind.TOOL_RESULT
    ]
    assert sorted(result.tool_call_id for result in results) == ["call_add_2", "call_weather_1"]

    tool_spans = [span for span in recording.spans.get_finished_spans() if span.name.startswith("execute_tool")]
    tool_span_ids = sorted(
        (span.attributes["gen_ai.tool.name"], span.attributes["gen_ai.tool.call.id"]) for span in tool_spans
    )
    assert tool_span_ids == [
        ("add", "call_add_2"),
        ("get_weather", "call_weather_1"),
    ]


def test_sync_chat_records_usage_and_stop_reason_in_sync_mode(recording) -> None:
    raw = _openai_raw(finish_reason="stop", usage={"prompt_tokens": 12, "completion_tokens": 3})
    llm = _ScriptedLLM([[ChatResponse(message=ChatMessage(role="assistant", content="hi"), raw=raw)]])
    config = with_agento11y_llamaindex_callbacks(None, client=recording.client, provider="openai")
    llm.callback_manager = config["callback_manager"]

    llm.chat([ChatMessage(role="user", content="hello")])
    recording.client.flush()

    (generation,) = _generations(recording.exporter)
    assert generation.mode.value == "SYNC"
    assert generation.stop_reason == "stop"
    assert (generation.usage.input_tokens, generation.usage.output_tokens) == (12, 3)
    assert _ttft_count(recording.metrics) == 0
