"""OpenTelemetry TracerProvider + MeterProvider auto-setup.

The SDK does not own OTel; applications must install providers. This plugin
acts as the application setup for hermes users who haven't wired OTel
themselves.

OTel exporter and resource configuration follow the OpenTelemetry env-var
schema (``OTEL_EXPORTER_OTLP_ENDPOINT``, ``OTEL_EXPORTER_OTLP_HEADERS``,
``OTEL_SERVICE_NAME``, ``OTEL_RESOURCE_ATTRIBUTES``). The OTLP HTTP exporters
read these themselves; the plugin only fills in ``service.name=hermes`` when
the user hasn't set one, the generations basic-auth headers derived in
``_config`` when no OTLP header env is set, and the endpoint when it was given
under the branded ``AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT`` name that the
exporters do not read.

If the host application has already installed a non-proxy provider, the plugin
leaves it untouched and uses the host's setup.
"""

from __future__ import annotations

import logging
import os
from typing import Any

from . import _config

logger = logging.getLogger(__name__)

_INSTALLED_TRACER_PROVIDER: Any = None
_INSTALLED_METER_PROVIDER: Any = None
_SETUP_DONE = False


def _is_proxy_tracer_provider(provider: Any) -> bool:
    from opentelemetry.trace import ProxyTracerProvider

    return isinstance(provider, ProxyTracerProvider)


def _is_proxy_meter_provider(provider: Any) -> bool:
    # Public default is the proxy provider. The class is in `_internal` —
    # reaching into a private name is brittle but the only correct check.
    try:
        from opentelemetry.metrics._internal import _ProxyMeterProvider
    except ImportError:
        return False
    return isinstance(provider, _ProxyMeterProvider)


def _auth_source(derived: bool) -> str:
    """Log suffix naming where the exporter's auth actually came from.

    ``derived`` is whether ``_exporter_headers`` handed the exporter our
    credentials-derived headers. It says nothing about whether those headers
    exist: with ``OTEL_EXPORTER_OTLP_HEADERS`` set, the env wins and the derived
    ones are never passed.
    """
    return " (auth from AGENTO11Y_AUTH_*)" if derived else ""


def _build_resource():
    from opentelemetry.sdk.resources import Resource

    # Resource.create() merges OTEL_SERVICE_NAME / OTEL_RESOURCE_ATTRIBUTES
    # from env, but explicit attrs win on key collision. Only set service.name
    # when the user hasn't, so OTEL_SERVICE_NAME still takes effect.
    attrs: dict[str, str] = {}
    if not os.environ.get("OTEL_SERVICE_NAME") and "service.name=" not in os.environ.get(
        "OTEL_RESOURCE_ATTRIBUTES", ""
    ):
        attrs["service.name"] = "hermes"
    return Resource.create(attrs)


def _exporter_headers(signal_env: str, fallback_headers: dict[str, str]) -> dict[str, str] | None:
    """Headers kwarg for an OTLP exporter, or ``None`` to let it read env itself.

    Returns the credentials-derived fallback only when the user set neither the
    generic ``OTEL_EXPORTER_OTLP_HEADERS`` nor the signal-specific override
    (``signal_env``). Passing ``headers=`` would otherwise clobber the user's
    explicit env config, since the exporter's kwarg wins over the env var.
    """
    if not fallback_headers:
        return None
    if os.environ.get("OTEL_EXPORTER_OTLP_HEADERS", "").strip():
        return None
    if os.environ.get(signal_env, "").strip():
        return None
    return dict(fallback_headers)


def _signal_endpoint(base: str, signal_path: str) -> str:
    """Append an OTLP signal path to a base endpoint, per the OTel spec.

    Only used for ``otel_endpoint_override``. The exporter's ``endpoint`` kwarg
    is the signal URL, not the base, so the path the exporter would have
    appended for ``OTEL_EXPORTER_OTLP_ENDPOINT`` has to be added here.
    """
    return base.rstrip("/") + signal_path


def _exporter_kwargs(cfg: _config.PluginConfig, signal_headers_env: str, signal_path: str) -> dict[str, Any]:
    """Exporter kwargs, empty when every value comes from the standard envs."""
    kwargs: dict[str, Any] = {}
    if cfg.otel_endpoint_override:
        kwargs["endpoint"] = _signal_endpoint(cfg.otel_endpoint_override, signal_path)
    headers = _exporter_headers(signal_headers_env, cfg.otel_auth_headers)
    if headers is not None:
        kwargs["headers"] = headers
    return kwargs


def _install_tracer_provider(cfg: _config.PluginConfig) -> tuple[Any, bool]:
    from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
    from opentelemetry.sdk.trace import TracerProvider
    from opentelemetry.sdk.trace.export import BatchSpanProcessor

    # OTLPSpanExporter() reads OTEL_EXPORTER_OTLP_ENDPOINT (appending
    # /v1/traces), OTEL_EXPORTER_OTLP_HEADERS, and OTEL_EXPORTER_OTLP_INSECURE
    # itself. We pass a value only where the standard env cannot supply it.
    kwargs = _exporter_kwargs(cfg, "OTEL_EXPORTER_OTLP_TRACES_HEADERS", "/v1/traces")
    exporter = OTLPSpanExporter(**kwargs)
    provider = TracerProvider(resource=_build_resource())
    provider.add_span_processor(BatchSpanProcessor(exporter))
    return provider, "headers" in kwargs


def _install_meter_provider(cfg: _config.PluginConfig) -> tuple[Any, bool]:
    from opentelemetry.exporter.otlp.proto.http.metric_exporter import OTLPMetricExporter
    from opentelemetry.sdk.metrics import MeterProvider
    from opentelemetry.sdk.metrics.export import PeriodicExportingMetricReader

    kwargs = _exporter_kwargs(cfg, "OTEL_EXPORTER_OTLP_METRICS_HEADERS", "/v1/metrics")
    exporter = OTLPMetricExporter(**kwargs)
    reader = PeriodicExportingMetricReader(exporter)
    return MeterProvider(resource=_build_resource(), metric_readers=[reader]), "headers" in kwargs


def setup_if_needed(plugin_cfg: _config.PluginConfig) -> bool:
    """Install TracerProvider and MeterProvider when missing.

    Returns True when at least one provider is in place after the call (host
    already had one, or this function installed one). Returns False when
    auto-setup is disabled and no provider exists.

    Idempotent — safe to call repeatedly. Each provider is considered
    independently: the host can own one and let the plugin install the other.
    """
    global _INSTALLED_TRACER_PROVIDER, _INSTALLED_METER_PROVIDER, _SETUP_DONE

    if _SETUP_DONE:
        return _INSTALLED_TRACER_PROVIDER is not None or _INSTALLED_METER_PROVIDER is not None or _has_any_provider()

    if not plugin_cfg.otel_configured:
        # No OTel endpoint env → only set up if the host has its own provider.
        _SETUP_DONE = True
        return _has_any_provider()

    from opentelemetry import metrics, trace

    current_tracer = trace.get_tracer_provider()
    current_meter = metrics.get_meter_provider()

    needs_tracer = _is_proxy_tracer_provider(current_tracer)
    needs_meter = _is_proxy_meter_provider(current_meter)

    if not (needs_tracer or needs_meter):
        _SETUP_DONE = True
        return True

    if not plugin_cfg.otel_auto:
        if needs_tracer or needs_meter:
            logger.warning(
                "grafana-agento11y-hermes: AGENTO11Y_HERMES_OTEL_AUTO=false and no provider is configured "
                "for %s — telemetry is disabled.",
                "TracerProvider+MeterProvider"
                if (needs_tracer and needs_meter)
                else ("TracerProvider" if needs_tracer else "MeterProvider"),
            )
        _SETUP_DONE = True
        return not (needs_tracer and needs_meter)

    try:
        if needs_tracer:
            provider, derived_auth = _install_tracer_provider(plugin_cfg)
            trace.set_tracer_provider(provider)
            _INSTALLED_TRACER_PROVIDER = provider
            logger.info(
                "grafana-agento11y-hermes: installed TracerProvider with OTLP HTTP exporter%s",
                _auth_source(derived_auth),
            )
        if needs_meter:
            provider, derived_auth = _install_meter_provider(plugin_cfg)
            metrics.set_meter_provider(provider)
            _INSTALLED_METER_PROVIDER = provider
            logger.info(
                "grafana-agento11y-hermes: installed MeterProvider with OTLP HTTP exporter%s",
                _auth_source(derived_auth),
            )
    except Exception as exc:
        logger.warning("grafana-agento11y-hermes: failed to set up OTel providers: %s", exc)
        _SETUP_DONE = True
        return False

    _SETUP_DONE = True
    return True


def _has_any_provider() -> bool:
    from opentelemetry import metrics, trace

    return not _is_proxy_tracer_provider(trace.get_tracer_provider()) or not _is_proxy_meter_provider(
        metrics.get_meter_provider()
    )


def force_flush(timeout_millis: int | None = None) -> None:
    """Flush the providers we installed. Skip ones owned by the host.

    ``timeout_millis`` bounds each provider's flush. Left unset, both use the
    OTel SDK default of 30s.
    """
    kwargs = {} if timeout_millis is None else {"timeout_millis": timeout_millis}
    if _INSTALLED_TRACER_PROVIDER is not None:
        try:
            _INSTALLED_TRACER_PROVIDER.force_flush(**kwargs)
        except Exception as exc:
            logger.warning("grafana-agento11y-hermes: TracerProvider force_flush failed: %s", exc)
    if _INSTALLED_METER_PROVIDER is not None:
        try:
            _INSTALLED_METER_PROVIDER.force_flush(**kwargs)
        except Exception as exc:
            logger.warning("grafana-agento11y-hermes: MeterProvider force_flush failed: %s", exc)


def _reset_for_tests() -> None:
    """Clear cached install state and shut down any providers we installed.

    Shutdown is needed even in tests — the providers' background export threads
    otherwise keep firing retries against localhost long after the test ended.
    """
    global _INSTALLED_TRACER_PROVIDER, _INSTALLED_METER_PROVIDER, _SETUP_DONE
    if _INSTALLED_TRACER_PROVIDER is not None:
        try:
            _INSTALLED_TRACER_PROVIDER.shutdown()
        except Exception:
            pass
    if _INSTALLED_METER_PROVIDER is not None:
        try:
            _INSTALLED_METER_PROVIDER.shutdown()
        except Exception:
            pass
    _INSTALLED_TRACER_PROVIDER = None
    _INSTALLED_METER_PROVIDER = None
    _SETUP_DONE = False
