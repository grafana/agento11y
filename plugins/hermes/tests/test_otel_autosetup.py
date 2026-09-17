from __future__ import annotations

from collections.abc import Iterator
from typing import Any

import pytest
from opentelemetry import metrics, trace
from opentelemetry.metrics._internal import _ProxyMeterProvider
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.trace import ProxyTracerProvider

from grafana_agento11y_hermes import _client, _config, _otel


@pytest.fixture(autouse=True)
def reset_otel(monkeypatch: pytest.MonkeyPatch) -> Iterator[None]:
    _client._reset_for_tests()
    # Force a fresh ProxyTracerProvider for isolation. Safe: opentelemetry
    # exposes the proxy as a no-op default that downstream code tolerates.
    monkeypatch.setattr(trace, "_TRACER_PROVIDER", None, raising=False)
    monkeypatch.setattr(
        trace,
        "_TRACER_PROVIDER_SET_ONCE",
        trace._TRACER_PROVIDER_SET_ONCE.__class__(),
        raising=False,
    )
    monkeypatch.setattr(metrics._internal, "_METER_PROVIDER", None, raising=False)
    monkeypatch.setattr(
        metrics._internal,
        "_METER_PROVIDER_SET_ONCE",
        metrics._internal._METER_PROVIDER_SET_ONCE.__class__(),
        raising=False,
    )
    yield
    _client._reset_for_tests()


def _proxy_tracer_active() -> bool:
    return isinstance(trace.get_tracer_provider(), ProxyTracerProvider)


def _proxy_meter_active() -> bool:
    return isinstance(metrics.get_meter_provider(), _ProxyMeterProvider)


@pytest.fixture
def otel_env(monkeypatch: pytest.MonkeyPatch) -> None:
    """Standard OTLP env — exporters read these themselves."""
    monkeypatch.setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost/otlp")
    monkeypatch.setenv(
        "OTEL_EXPORTER_OTLP_HEADERS",
        "Authorization=Basic c3RhY2stMTpnbGNfb3RscF9zZWNyZXQ=",
    )


def _make_cfg(*, otel_auto: bool = True, otel_configured: bool = True) -> _config.PluginConfig:
    return _config.PluginConfig(
        otel_auto=otel_auto,
        otel_configured=otel_configured,
    )


def test_auto_setup_installs_both_providers_when_proxies_are_global(otel_env) -> None:
    assert _proxy_tracer_active(), "test fixture should leave a ProxyTracerProvider in place"
    assert _proxy_meter_active(), "test fixture should leave a proxy MeterProvider in place"
    cfg = _make_cfg(otel_auto=True)
    ok = _otel.setup_if_needed(cfg)
    assert ok is True
    assert isinstance(trace.get_tracer_provider(), TracerProvider)
    assert isinstance(metrics.get_meter_provider(), MeterProvider)


def test_auto_setup_is_idempotent(otel_env) -> None:
    cfg = _make_cfg(otel_auto=True)
    _otel.setup_if_needed(cfg)
    first_tracer = trace.get_tracer_provider()
    first_meter = metrics.get_meter_provider()
    _otel.setup_if_needed(cfg)
    assert trace.get_tracer_provider() is first_tracer
    assert metrics.get_meter_provider() is first_meter


def test_auto_setup_skipped_when_user_has_both_providers(otel_env) -> None:
    custom_tracer = TracerProvider()
    custom_meter = MeterProvider()
    trace.set_tracer_provider(custom_tracer)
    metrics.set_meter_provider(custom_meter)
    cfg = _make_cfg(otel_auto=True)
    ok = _otel.setup_if_needed(cfg)
    assert ok is True
    assert trace.get_tracer_provider() is custom_tracer
    assert metrics.get_meter_provider() is custom_meter


def test_auto_setup_installs_only_missing_provider(otel_env) -> None:
    custom_tracer = TracerProvider()
    trace.set_tracer_provider(custom_tracer)
    cfg = _make_cfg(otel_auto=True)
    ok = _otel.setup_if_needed(cfg)
    assert ok is True
    assert trace.get_tracer_provider() is custom_tracer
    assert isinstance(metrics.get_meter_provider(), MeterProvider)


def test_auto_setup_disabled_returns_false_with_proxies(otel_env) -> None:
    cfg = _make_cfg(otel_auto=False)
    ok = _otel.setup_if_needed(cfg)
    assert ok is False
    assert _proxy_tracer_active(), "no provider must be installed when auto is disabled"
    assert _proxy_meter_active(), "no meter provider must be installed when auto is disabled"


def test_auto_setup_disabled_uses_existing_user_providers(otel_env) -> None:
    custom_tracer = TracerProvider()
    custom_meter = MeterProvider()
    trace.set_tracer_provider(custom_tracer)
    metrics.set_meter_provider(custom_meter)
    cfg = _make_cfg(otel_auto=False)
    ok = _otel.setup_if_needed(cfg)
    assert ok is True
    assert trace.get_tracer_provider() is custom_tracer
    assert metrics.get_meter_provider() is custom_meter


def test_no_otel_env_is_no_op_for_otel() -> None:
    cfg = _make_cfg(otel_configured=False)
    ok = _otel.setup_if_needed(cfg)
    assert ok is False
    assert _proxy_tracer_active()
    assert _proxy_meter_active()


def test_default_service_name_is_hermes(otel_env, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("OTEL_SERVICE_NAME", raising=False)
    monkeypatch.delenv("OTEL_RESOURCE_ATTRIBUTES", raising=False)

    cfg = _make_cfg(otel_auto=True)
    _otel.setup_if_needed(cfg)

    provider = trace.get_tracer_provider()
    assert isinstance(provider, TracerProvider)
    assert provider.resource.attributes.get("service.name") == "hermes"


def test_otel_service_name_env_wins(otel_env, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("OTEL_SERVICE_NAME", "my-app")

    cfg = _make_cfg(otel_auto=True)
    _otel.setup_if_needed(cfg)

    provider = trace.get_tracer_provider()
    assert isinstance(provider, TracerProvider)
    assert provider.resource.attributes.get("service.name") == "my-app"


# --- OTLP auth-header fallback ---

_FALLBACK = {"Authorization": "Basic c3RhY2stMTpnbGNfc2VjcmV0", "X-Scope-OrgID": "stack-1"}


def test_exporter_headers_none_without_fallback(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("OTEL_EXPORTER_OTLP_HEADERS", raising=False)
    monkeypatch.delenv("OTEL_EXPORTER_OTLP_TRACES_HEADERS", raising=False)
    assert _otel._exporter_headers("OTEL_EXPORTER_OTLP_TRACES_HEADERS", {}) is None


def test_exporter_headers_returns_copy_of_fallback(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("OTEL_EXPORTER_OTLP_HEADERS", raising=False)
    monkeypatch.delenv("OTEL_EXPORTER_OTLP_TRACES_HEADERS", raising=False)
    out = _otel._exporter_headers("OTEL_EXPORTER_OTLP_TRACES_HEADERS", _FALLBACK)
    assert out == _FALLBACK
    assert out is not _FALLBACK


def test_exporter_headers_suppressed_by_generic_env(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("OTEL_EXPORTER_OTLP_HEADERS", "Authorization=Basic xyz")
    monkeypatch.delenv("OTEL_EXPORTER_OTLP_TRACES_HEADERS", raising=False)
    assert _otel._exporter_headers("OTEL_EXPORTER_OTLP_TRACES_HEADERS", _FALLBACK) is None


def test_exporter_headers_suppressed_by_signal_env(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("OTEL_EXPORTER_OTLP_HEADERS", raising=False)
    monkeypatch.setenv("OTEL_EXPORTER_OTLP_TRACES_HEADERS", "Authorization=Basic xyz")
    assert _otel._exporter_headers("OTEL_EXPORTER_OTLP_TRACES_HEADERS", _FALLBACK) is None


def _capture_exporters(monkeypatch: pytest.MonkeyPatch) -> dict[str, Any]:
    """Patch the OTLP exporters with subclasses that record their kwargs."""
    import opentelemetry.exporter.otlp.proto.http.metric_exporter as me
    import opentelemetry.exporter.otlp.proto.http.trace_exporter as te

    captured: dict[str, Any] = {}

    class CapSpan(te.OTLPSpanExporter):
        def __init__(self, *a: Any, **kw: Any) -> None:
            captured["span"] = kw.get("headers")
            captured["span_endpoint"] = kw.get("endpoint")
            super().__init__(*a, **kw)

    class CapMetric(me.OTLPMetricExporter):
        def __init__(self, *a: Any, **kw: Any) -> None:
            captured["metric"] = kw.get("headers")
            captured["metric_endpoint"] = kw.get("endpoint")
            super().__init__(*a, **kw)

    monkeypatch.setattr(te, "OTLPSpanExporter", CapSpan)
    monkeypatch.setattr(me, "OTLPMetricExporter", CapMetric)
    return captured


def test_fallback_headers_passed_to_exporters(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost/otlp")
    for var in (
        "OTEL_EXPORTER_OTLP_HEADERS",
        "OTEL_EXPORTER_OTLP_TRACES_HEADERS",
        "OTEL_EXPORTER_OTLP_METRICS_HEADERS",
    ):
        monkeypatch.delenv(var, raising=False)
    captured = _capture_exporters(monkeypatch)

    cfg = _config.PluginConfig(otel_auto=True, otel_configured=True, otel_auth_headers=dict(_FALLBACK))
    assert _otel.setup_if_needed(cfg) is True
    assert captured["span"] == _FALLBACK
    assert captured["metric"] == _FALLBACK


def test_user_headers_env_suppresses_fallback(otel_env, monkeypatch: pytest.MonkeyPatch) -> None:
    """otel_env sets OTEL_EXPORTER_OTLP_HEADERS, so the fallback must not apply."""
    captured = _capture_exporters(monkeypatch)

    cfg = _config.PluginConfig(otel_auto=True, otel_configured=True, otel_auth_headers=dict(_FALLBACK))
    assert _otel.setup_if_needed(cfg) is True
    assert captured["span"] is None
    assert captured["metric"] is None


# --- branded OTLP endpoint ---


def test_standard_endpoint_is_left_to_the_exporters(otel_env, monkeypatch: pytest.MonkeyPatch) -> None:
    captured = _capture_exporters(monkeypatch)

    assert _otel.setup_if_needed(_make_cfg()) is True
    assert captured["span_endpoint"] is None
    assert captured["metric_endpoint"] is None


def test_branded_endpoint_is_passed_per_signal(monkeypatch: pytest.MonkeyPatch) -> None:
    """The branded alias is the one name the exporters cannot read themselves."""
    monkeypatch.delenv("OTEL_EXPORTER_OTLP_ENDPOINT", raising=False)
    monkeypatch.setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", "https://otlp.example/otlp")
    captured = _capture_exporters(monkeypatch)

    cfg = _config.load()
    assert cfg.otel_configured is True
    assert _otel.setup_if_needed(cfg) is True
    assert captured["span_endpoint"] == "https://otlp.example/otlp/v1/traces"
    assert captured["metric_endpoint"] == "https://otlp.example/otlp/v1/metrics"


@pytest.mark.parametrize(
    ("base", "expected"),
    [
        ("https://otlp.example/otlp", "https://otlp.example/otlp/v1/traces"),
        ("https://otlp.example/otlp/", "https://otlp.example/otlp/v1/traces"),
        ("https://otlp.example/otlp///", "https://otlp.example/otlp/v1/traces"),
    ],
)
def test_signal_endpoint_normalizes_trailing_slashes(base: str, expected: str) -> None:
    assert _otel._signal_endpoint(base, "/v1/traces") == expected


def test_auth_source_reflects_what_was_passed() -> None:
    """The log suffix tracks the headers kwarg, not whether credentials exist."""
    assert _otel._auth_source(True) == " (auth from AGENTO11Y_AUTH_*)"
    assert _otel._auth_source(False) == ""


def test_install_reports_derived_auth_only_when_headers_are_used(otel_env, monkeypatch: pytest.MonkeyPatch) -> None:
    """otel_env sets OTEL_EXPORTER_OTLP_HEADERS, so the derived headers lose."""
    _capture_exporters(monkeypatch)
    cfg = _config.PluginConfig(otel_auto=True, otel_configured=True, otel_auth_headers=dict(_FALLBACK))

    _, derived = _otel._install_tracer_provider(cfg)
    assert derived is False

    monkeypatch.delenv("OTEL_EXPORTER_OTLP_HEADERS", raising=False)
    _, derived = _otel._install_tracer_provider(cfg)
    assert derived is True


def test_a_provider_that_cannot_be_built_disables_the_channel(
    otel_env, monkeypatch: pytest.MonkeyPatch, caplog: pytest.LogCaptureFixture
) -> None:
    """An unbuildable exporter is a no-op channel, never a raise into hermes."""
    import logging

    def explode(*_: Any, **__: Any) -> Any:
        raise RuntimeError("exporter refused to build")

    monkeypatch.setattr(_otel, "_install_tracer_provider", explode)

    with caplog.at_level(logging.WARNING):
        assert _otel.setup_if_needed(_make_cfg()) is False

    assert [r for r in caplog.records if "failed to set up OTel providers" in r.getMessage()]
    assert _otel._INSTALLED_TRACER_PROVIDER is None
    assert _proxy_tracer_active(), "a failed install must not leave a half-wired provider"


def test_a_failed_setup_is_not_retried(otel_env, monkeypatch: pytest.MonkeyPatch) -> None:
    attempts: list[int] = []

    def explode(*_: Any, **__: Any) -> Any:
        attempts.append(1)
        raise RuntimeError("exporter refused to build")

    monkeypatch.setattr(_otel, "_install_tracer_provider", explode)

    _otel.setup_if_needed(_make_cfg())
    _otel.setup_if_needed(_make_cfg())

    assert len(attempts) == 1, "setup is idempotent, including after a failure"
