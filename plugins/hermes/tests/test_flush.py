"""A flush that raises or hangs in ``on_session_end`` can disrupt the hermes loop."""

from __future__ import annotations

import threading
from typing import Any

import pytest

from grafana_agento11y_hermes import _client, _hooks, _otel
from tests.conftest import FakeClient


class RaisingProvider:
    """A provider whose flush and shutdown both fail, as an unreachable one does."""

    def __init__(self) -> None:
        self.force_flush_calls: list[dict[str, Any]] = []
        self.shutdown_calls = 0

    def force_flush(self, **kwargs: Any) -> None:
        self.force_flush_calls.append(kwargs)
        raise RuntimeError("exporter is unreachable")

    def shutdown(self) -> None:
        self.shutdown_calls += 1
        raise RuntimeError("already shut down")


class RecordingProvider:
    def __init__(self) -> None:
        self.force_flush_calls: list[dict[str, Any]] = []

    def force_flush(self, **kwargs: Any) -> None:
        self.force_flush_calls.append(kwargs)


@pytest.mark.parametrize("failing", ("tracer", "meter", "both"))
def test_a_provider_that_cannot_flush_does_not_stop_the_other(monkeypatch: pytest.MonkeyPatch, failing: str) -> None:
    tracer = RaisingProvider() if failing in ("tracer", "both") else RecordingProvider()
    meter = RaisingProvider() if failing in ("meter", "both") else RecordingProvider()
    monkeypatch.setattr(_otel, "_INSTALLED_TRACER_PROVIDER", tracer)
    monkeypatch.setattr(_otel, "_INSTALLED_METER_PROVIDER", meter)

    _otel.force_flush()

    assert tracer.force_flush_calls, "the tracer provider was asked"
    assert meter.force_flush_calls, "and so was the meter provider, whichever failed"


def test_a_flush_timeout_is_passed_to_both_providers(monkeypatch: pytest.MonkeyPatch) -> None:
    tracer, meter = RecordingProvider(), RecordingProvider()
    monkeypatch.setattr(_otel, "_INSTALLED_TRACER_PROVIDER", tracer)
    monkeypatch.setattr(_otel, "_INSTALLED_METER_PROVIDER", meter)

    _otel.force_flush(1500)

    assert tracer.force_flush_calls == [{"timeout_millis": 1500}]
    assert meter.force_flush_calls == [{"timeout_millis": 1500}]


def test_no_timeout_leaves_the_otel_default_in_place(monkeypatch: pytest.MonkeyPatch) -> None:
    tracer = RecordingProvider()
    monkeypatch.setattr(_otel, "_INSTALLED_TRACER_PROVIDER", tracer)
    monkeypatch.setattr(_otel, "_INSTALLED_METER_PROVIDER", None)

    _otel.force_flush()

    assert tracer.force_flush_calls == [{}]


def test_host_owned_providers_are_never_flushed(monkeypatch: pytest.MonkeyPatch) -> None:
    """Only providers this plugin installed are tracked, so this is a no-op."""
    monkeypatch.setattr(_otel, "_INSTALLED_TRACER_PROVIDER", None)
    monkeypatch.setattr(_otel, "_INSTALLED_METER_PROVIDER", None)
    _otel.force_flush(1000)


def test_a_provider_that_cannot_shut_down_does_not_break_the_reset(monkeypatch: pytest.MonkeyPatch) -> None:
    tracer, meter = RaisingProvider(), RaisingProvider()
    monkeypatch.setattr(_otel, "_INSTALLED_TRACER_PROVIDER", tracer)
    monkeypatch.setattr(_otel, "_INSTALLED_METER_PROVIDER", meter)

    _otel._reset_for_tests()

    assert tracer.shutdown_calls == 1
    assert meter.shutdown_calls == 1
    assert _otel._INSTALLED_TRACER_PROVIDER is None
    assert _otel._INSTALLED_METER_PROVIDER is None


def test_a_client_flush_that_raises_still_drains_otel(monkeypatch: pytest.MonkeyPatch, failing_client: Any) -> None:
    client = failing_client("flush")
    tracer = RecordingProvider()
    monkeypatch.setattr(_otel, "_INSTALLED_TRACER_PROVIDER", tracer)

    _client._flush_channels()

    assert client.flush_calls == 1
    assert tracer.force_flush_calls, "the OTel pipeline is a separate channel and still needs draining"


def test_flushing_before_the_client_exists_does_not_build_one(monkeypatch: pytest.MonkeyPatch) -> None:
    import agento11y

    def explode(*_: Any, **__: Any) -> Any:
        raise AssertionError("Client must not be constructed by a flush")

    monkeypatch.setattr(agento11y, "Client", explode)
    _client._flush_channels()


def test_a_zero_timeout_skips_the_bounded_flush(patch_client: FakeClient) -> None:
    assert _client.flush_bounded(0) is False
    assert patch_client.flush_calls == 0


def test_a_bounded_flush_reports_success(patch_client: FakeClient) -> None:
    assert _client.flush_bounded(5.0) is True
    assert patch_client.flush_calls == 1


def test_a_bounded_flush_gives_up_on_a_hanging_exporter(
    monkeypatch: pytest.MonkeyPatch, patch_client: FakeClient
) -> None:
    """A blocking flush must not stall the hermes loop past the timeout."""
    release = threading.Event()

    def hang(*_: Any, **__: Any) -> None:
        release.wait(timeout=5)

    monkeypatch.setattr(_client, "_flush_channels", hang)
    try:
        assert _client.flush_bounded(0.05) is False
    finally:
        release.set()


def test_session_end_flushes_without_shutting_the_client_down(patch_client: FakeClient) -> None:
    """The client is a process-wide singleton; the next session reuses it."""
    _hooks.on_session_end(session_id="s1")
    _hooks.on_session_end(session_id="s2")

    assert patch_client.flush_calls == 2
    assert patch_client.shutdown_calls == 0
    assert _client._get_client() is patch_client
