"""Tests for cache diagnostics metadata."""

from __future__ import annotations

from datetime import timedelta

from agento11y import (
    CACHE_DIAGNOSTICS_MISS_REASON_KEY,
    CACHE_DIAGNOSTICS_MISSED_INPUT_TOKENS_KEY,
    CACHE_DIAGNOSTICS_PREVIOUS_MESSAGE_ID_KEY,
    Client,
    ClientConfig,
    EmbeddingCaptureConfig,
    GenerationExportConfig,
    GenerationStart,
    Message,
    MessageRole,
    ModelRef,
    Part,
    PartKind,
    set_cache_diagnostics,
)
from conftest import CapturingGenerationExporter


def _client(exporter: CapturingGenerationExporter) -> Client:
    return Client(
        ClientConfig(
            generation_export=GenerationExportConfig(
                batch_size=10,
                flush_interval=timedelta(seconds=60),
                queue_size=10,
                max_retries=1,
                initial_backoff=timedelta(milliseconds=1),
                max_backoff=timedelta(milliseconds=1),
                payload_max_bytes=4 << 20,
            ),
            embedding_capture=EmbeddingCaptureConfig(),
            generation_exporter=exporter,
        )
    )


def test_set_cache_diagnostics_module_function() -> None:
    exporter = CapturingGenerationExporter()
    client = _client(exporter)
    try:
        rec = client.start_generation(
            GenerationStart(model=ModelRef(provider="anthropic", name="claude-3-5-sonnet-latest"))
        )
        set_cache_diagnostics(
            rec,
            "tools_changed",
            missed_input_tokens=100,
            previous_message_id="msg_prev",
        )
        rec.set_result(output=[Message(role=MessageRole.ASSISTANT, parts=[Part(kind=PartKind.TEXT, text="ok")])])
        rec.end()
        assert rec.err() is None
        md = rec.last_generation.metadata
        assert md[CACHE_DIAGNOSTICS_MISS_REASON_KEY] == "tools_changed"
        assert md[CACHE_DIAGNOSTICS_MISSED_INPUT_TOKENS_KEY] == "100"
        assert md[CACHE_DIAGNOSTICS_PREVIOUS_MESSAGE_ID_KEY] == "msg_prev"
    finally:
        client.shutdown()


def test_set_cache_diagnostics_empty_reason_noop() -> None:
    exporter = CapturingGenerationExporter()
    client = _client(exporter)
    try:
        rec = client.start_generation(
            GenerationStart(model=ModelRef(provider="anthropic", name="claude-3-5-sonnet-latest"))
        )
        set_cache_diagnostics(rec, "   ")
        rec.set_result(output=[Message(role=MessageRole.ASSISTANT, parts=[Part(kind=PartKind.TEXT, text="ok")])])
        rec.end()
        assert CACHE_DIAGNOSTICS_MISS_REASON_KEY not in rec.last_generation.metadata
    finally:
        client.shutdown()


def test_set_cache_diagnostics_none_recorder() -> None:
    set_cache_diagnostics(None, "system_changed")


def test_set_cache_diagnostics_after_end_noop() -> None:
    exporter = CapturingGenerationExporter()
    client = _client(exporter)
    try:
        rec = client.start_generation(
            GenerationStart(model=ModelRef(provider="anthropic", name="claude-3-5-sonnet-latest"))
        )
        rec.set_result(output=[Message(role=MessageRole.ASSISTANT, parts=[Part(kind=PartKind.TEXT, text="ok")])])
        rec.end()

        set_cache_diagnostics(rec, "system_changed", missed_input_tokens=42)

        assert CACHE_DIAGNOSTICS_MISS_REASON_KEY not in rec.last_generation.metadata
    finally:
        client.shutdown()
