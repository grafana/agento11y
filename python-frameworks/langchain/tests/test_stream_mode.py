"""`.stream()` / `.astream()` on a default chat model must record STREAM with TTFT."""

import asyncio
from datetime import timedelta

import pytest
from agento11y import Client, ClientConfig, GenerationExportConfig
from agento11y.models import GenerationMode
from agento11y_langchain import Agento11yAsyncLangChainHandler, Agento11yLangChainHandler
from langchain_core.language_models.fake_chat_models import GenericFakeChatModel
from langchain_core.messages import AIMessage
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.metrics.export import InMemoryMetricReader
from test_langchain_handler import _CapturingExporter


@pytest.fixture
def setup():
    reader = InMemoryMetricReader()
    meters = MeterProvider(metric_readers=[reader])
    exporter = _CapturingExporter()
    client = Client(
        ClientConfig(
            meter=meters.get_meter("test"),
            generation_export=GenerationExportConfig(batch_size=10, flush_interval=timedelta(seconds=60)),
            generation_exporter=exporter,
        )
    )
    yield client, exporter, reader
    client.shutdown()
    meters.shutdown()


def _model() -> GenericFakeChatModel:
    # Default flags: neither `stream` nor `streaming` appears in invocation_params.
    return GenericFakeChatModel(messages=iter([AIMessage(content="hello streamed world")]))


def _ttft_points(reader: InMemoryMetricReader) -> int:
    data = reader.get_metrics_data()
    return sum(
        p.count
        for rm in (data.resource_metrics if data else [])
        for sm in rm.scope_metrics
        for m in sm.metrics
        if m.name == "gen_ai.client.time_to_first_token"
        for p in m.data.data_points
    )


def _only_generation(client: Client, exporter: _CapturingExporter):
    client.flush()
    (generation,) = [g for r in exporter.requests for g in r.generations]
    return generation


def _assert_stream(generation, reader) -> None:
    assert generation.mode == GenerationMode.STREAM
    assert generation.operation_name == "streamText"
    assert _ttft_points(reader) == 1


def test_sync_stream_records_stream_mode_and_ttft(setup):
    client, exporter, reader = setup
    handler = Agento11yLangChainHandler(client=client)
    chunks = list(_model().stream("hi", config={"callbacks": [handler]}))
    assert len(chunks) > 1
    _assert_stream(_only_generation(client, exporter), reader)


def test_async_stream_records_stream_mode_and_ttft(setup):
    client, exporter, reader = setup
    handler = Agento11yAsyncLangChainHandler(client=client)

    async def run() -> list:
        return [c async for c in _model().astream("hi", config={"callbacks": [handler]})]

    assert len(asyncio.run(run())) > 1
    _assert_stream(_only_generation(client, exporter), reader)


def test_invoke_stays_sync(setup):
    client, exporter, reader = setup
    handler = Agento11yLangChainHandler(client=client)
    _model().invoke("hi", config={"callbacks": [handler]})
    generation = _only_generation(client, exporter)
    assert generation.mode == GenerationMode.SYNC
    assert generation.operation_name == "generateText"
    assert _ttft_points(reader) == 0
