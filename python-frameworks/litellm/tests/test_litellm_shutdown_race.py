"""Generations from LiteLLM's deferred callbacks are exported by a shutdown right after the call."""

from __future__ import annotations

import asyncio
from datetime import timedelta
from typing import Any

import litellm
import pytest
from agento11y import Client, ClientConfig, GenerationExportConfig
from agento11y_litellm import Agento11yLiteLLMLogger
from test_litellm_handler import _CapturingExporter

_MESSAGES = [{"role": "user", "content": "hi"}]


def _exported(exporter: _CapturingExporter) -> int:
    return sum(len(r.generations) for r in exporter.requests)


@pytest.fixture
def wired() -> Any:
    exporter = _CapturingExporter()
    client = Client(
        ClientConfig(
            generation_export=GenerationExportConfig(batch_size=100, flush_interval=timedelta(seconds=60)),
            generation_exporter=exporter,
        )
    )
    handler = Agento11yLiteLLMLogger(client=client)
    # LiteLLM copies litellm.callbacks into its own success/failure lists, so reset those too.
    litellm.logging_callback_manager._reset_all_callbacks()
    litellm.callbacks = [handler]
    yield exporter, handler
    litellm.logging_callback_manager._reset_all_callbacks()


def test_shutdown_after_sync_completions_exports_all(wired: Any) -> None:
    exporter, handler = wired
    for _ in range(2):
        litellm.completion(model="gpt-4o-mini", messages=_MESSAGES, mock_response="ok")
    handler.shutdown()
    assert _exported(exporter) == 2


def test_ashutdown_inside_event_loop_exports_all(wired: Any) -> None:
    exporter, handler = wired

    async def run() -> None:
        for _ in range(2):
            await litellm.acompletion(model="gpt-4o-mini", messages=_MESSAGES, mock_response="ok")
        await handler.ashutdown()

    asyncio.run(run())
    assert _exported(exporter) == 2


def test_flush_inside_event_loop_points_to_aflush(wired: Any) -> None:
    _, handler = wired

    async def run() -> None:
        handler.flush()

    with pytest.raises(RuntimeError, match="aflush"):
        asyncio.run(run())
    handler.shutdown()
