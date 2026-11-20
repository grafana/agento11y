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


def test_drain_waits_for_running_task_without_growing_pool(monkeypatch: pytest.MonkeyPatch) -> None:
    import threading
    import time

    from agento11y_litellm import handler as handler_mod
    from litellm import utils as litellm_utils
    from litellm.litellm_core_utils.thread_pool_executor import BoundedLoggingThreadPoolExecutor

    executor = BoundedLoggingThreadPoolExecutor(max_workers=50, max_pending_tasks=100)
    monkeypatch.setattr(litellm_utils, "executor", executor)
    done = threading.Event()
    executor.submit(lambda: (time.sleep(0.1), done.set()))
    handler_mod._drain_sync_executor(5.0)
    assert done.is_set()
    assert len(executor._threads) == 1
    executor.shutdown()


def test_drain_times_out_on_stuck_task(monkeypatch: pytest.MonkeyPatch, caplog: pytest.LogCaptureFixture) -> None:
    import threading

    from agento11y_litellm import handler as handler_mod
    from litellm import utils as litellm_utils
    from litellm.litellm_core_utils.thread_pool_executor import BoundedLoggingThreadPoolExecutor

    executor = BoundedLoggingThreadPoolExecutor(max_workers=2, max_pending_tasks=10)
    monkeypatch.setattr(litellm_utils, "executor", executor)
    release = threading.Event()
    executor.submit(release.wait)
    handler_mod._drain_sync_executor(0.05)
    assert "timed out" in caplog.text
    release.set()
    executor.shutdown()


def test_drain_falls_back_to_barrier_on_plain_pool(monkeypatch: pytest.MonkeyPatch) -> None:
    import threading
    import time
    from concurrent.futures import ThreadPoolExecutor

    from agento11y_litellm import handler as handler_mod
    from litellm import utils as litellm_utils

    executor = ThreadPoolExecutor(max_workers=2)
    monkeypatch.setattr(litellm_utils, "executor", executor)
    done = threading.Event()
    executor.submit(lambda: (time.sleep(0.1), done.set()))
    handler_mod._drain_sync_executor(5.0)
    assert done.is_set()
    executor.shutdown()


def test_adrain_waits_for_spawned_enqueue_task() -> None:
    from agento11y_litellm import handler as handler_mod
    from litellm.litellm_core_utils.logging_worker import GLOBAL_LOGGING_WORKER
    from litellm.responses.streaming_iterator import spawn_logging_task

    ran: list[bool] = []

    async def callback() -> None:
        ran.append(True)

    async def enqueue_later() -> None:
        await asyncio.sleep(0.05)
        GLOBAL_LOGGING_WORKER.ensure_initialized_and_enqueue(async_coroutine=callback())

    async def main() -> None:
        spawn_logging_task(asyncio.get_running_loop(), enqueue_later(), task_name="test enqueue")
        await handler_mod.adrain_litellm_callbacks(5.0)
        assert ran == [True]

    asyncio.run(main())


def test_ashutdown_still_shuts_down_client_when_task_drain_raises(monkeypatch: pytest.MonkeyPatch, wired: Any) -> None:
    from litellm.responses import streaming_iterator

    async def boom(timeout: float = 5.0) -> None:
        raise ValueError("boom")

    monkeypatch.setattr(streaming_iterator, "drain_logging_tasks", boom)
    _, handler = wired
    shut: list[bool] = []
    monkeypatch.setattr(handler._client, "shutdown", lambda: shut.append(True))
    asyncio.run(handler.ashutdown())
    assert shut == [True]


def test_shutdown_continues_when_worker_flush_raises(monkeypatch: pytest.MonkeyPatch, wired: Any) -> None:
    import threading

    from agento11y_litellm import handler as handler_mod

    class _BrokenWorker:
        _queue = object()

        async def flush(self) -> None:
            raise RuntimeError("queue bound to a different event loop")

    monkeypatch.setattr(handler_mod, "_async_logging_worker", lambda: _BrokenWorker())
    sync_drained = threading.Event()
    monkeypatch.setattr(handler_mod, "_drain_sync_executor", lambda timeout: sync_drained.set())
    _, handler = wired
    shut: list[bool] = []
    monkeypatch.setattr(handler._client, "shutdown", lambda: shut.append(True))
    handler.shutdown()
    assert sync_drained.is_set()
    assert shut == [True]


def test_sync_shutdown_inside_event_loop_leaves_client_open(monkeypatch: pytest.MonkeyPatch, wired: Any) -> None:
    _, handler = wired
    calls: list[str] = []
    monkeypatch.setattr(handler._client, "shutdown", lambda: calls.append("shutdown"))
    monkeypatch.setattr(handler._client, "flush", lambda: calls.append("flush"))

    async def run() -> None:
        for method in (handler.shutdown, handler.flush):
            with pytest.raises(RuntimeError, match="ashutdown"):
                method()

    asyncio.run(run())
    assert calls == []


@pytest.mark.parametrize("flush_first", [False, True])
def test_client_shutdown_after_sync_completions_exports_all(wired: Any, flush_first: bool) -> None:
    exporter, handler = wired
    for _ in range(2):
        litellm.completion(model="gpt-4o-mini", messages=_MESSAGES, mock_response="ok")
    if flush_first:
        handler._client.flush()
    handler._client.shutdown()
    assert _exported(exporter) == 2


def test_client_shutdown_waits_for_success_callback_that_lands_late(
    wired: Any, monkeypatch: pytest.MonkeyPatch, caplog: pytest.LogCaptureFixture
) -> None:
    import threading
    import time

    exporter, handler = wired
    original = handler.log_success_event
    shutdown_called = threading.Event()
    landed_after_shutdown: list[bool] = []

    def late_success(*args: Any, **kwargs: Any) -> None:
        shutdown_called.wait(2)
        time.sleep(0.05)
        landed_after_shutdown.append(shutdown_called.is_set())
        original(*args, **kwargs)

    monkeypatch.setattr(handler, "log_success_event", late_success)
    client = handler._client
    real_shutdown = client.shutdown

    def shutdown() -> None:
        shutdown_called.set()
        real_shutdown()

    for _ in range(2):
        litellm.completion(model="gpt-4o-mini", messages=_MESSAGES, mock_response="ok")
    shutdown()
    assert landed_after_shutdown == [True, True]
    assert _exported(exporter) == 2
    assert "client is shutting down" not in caplog.text


def test_handler_shutdown_drains_once_with_caller_timeout(wired: Any, monkeypatch: pytest.MonkeyPatch) -> None:
    from agento11y_litellm import handler as handler_mod

    _, handler = wired
    timeouts: list[float] = []
    monkeypatch.setattr(handler_mod, "drain_litellm_callbacks", lambda timeout=10.0: timeouts.append(timeout))
    monkeypatch.setattr(handler_mod, "_drain_sync_executor", lambda timeout: timeouts.append(timeout))
    handler.flush(timeout=0.5)
    handler.shutdown(timeout=0.25)
    assert timeouts == [0.5, 0.25]
