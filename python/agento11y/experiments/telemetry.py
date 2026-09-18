"""Optional one-call OTel setup using the SDK's existing OTLP dependencies."""

from __future__ import annotations

import os
from dataclasses import dataclass, field
from typing import Any


@dataclass(slots=True)
class EvalTelemetry:
    """Flush/shut down only providers created by setup_evals, never application-owned ones."""

    _owned: list[Any] = field(default_factory=list)

    def flush(self) -> None:
        for provider in self._owned:
            provider.force_flush()

    def shutdown(self) -> None:
        for provider in self._owned:
            provider.shutdown()
        self._owned.clear()

    def __enter__(self) -> EvalTelemetry:
        return self

    def __exit__(self, *_: Any) -> None:
        self.shutdown()


def setup_evals(*, service_name: str = "grafana-evals", otlp_endpoint: str = "") -> EvalTelemetry:
    """Call once before building instrumented clients; reuse installed providers.

    Uses HTTP/protobuf OTLP and standard OTEL_EXPORTER_OTLP_* credentials.
    Call shutdown after clients/runs finish, at process exit. Existing providers
    are not replaced or reconfigured. An endpoint is required for new exporters.
    """
    from opentelemetry import metrics, trace
    from opentelemetry.exporter.otlp.proto.http.metric_exporter import OTLPMetricExporter
    from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
    from opentelemetry.sdk.metrics import MeterProvider
    from opentelemetry.sdk.metrics.export import PeriodicExportingMetricReader
    from opentelemetry.sdk.resources import Resource
    from opentelemetry.sdk.trace import TracerProvider
    from opentelemetry.sdk.trace.export import BatchSpanProcessor

    result = EvalTelemetry()
    needs_trace = isinstance(trace.get_tracer_provider(), trace.ProxyTracerProvider)
    needs_metrics = type(metrics.get_meter_provider()).__name__ == "_ProxyMeterProvider"
    if not needs_trace and not needs_metrics:
        return result
    for needed, signal in ((needs_trace, "TRACES"), (needs_metrics, "METRICS")):
        if needed and not (
            otlp_endpoint
            or os.getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
            or os.getenv(f"OTEL_EXPORTER_OTLP_{signal}_ENDPOINT")
        ):
            raise ValueError(f"Set OTEL_EXPORTER_OTLP_ENDPOINT (or the {signal} endpoint) before setup_evals")
    resource = Resource.create({"service.name": service_name})
    if needs_trace:
        provider = TracerProvider(resource=resource)
        exporter = OTLPSpanExporter(**({"endpoint": otlp_endpoint.rstrip("/") + "/v1/traces"} if otlp_endpoint else {}))
        provider.add_span_processor(BatchSpanProcessor(exporter))
        trace.set_tracer_provider(provider)
        result._owned.append(provider)
    if needs_metrics:
        exporter = OTLPMetricExporter(
            **({"endpoint": otlp_endpoint.rstrip("/") + "/v1/metrics"} if otlp_endpoint else {})
        )
        meter = MeterProvider(resource=resource, metric_readers=[PeriodicExportingMetricReader(exporter)])
        metrics.set_meter_provider(meter)
        result._owned.append(meter)
    return result
