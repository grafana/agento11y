"""OpenTelemetry bootstrap.

Sets up a TracerProvider + MeterProvider using OTLP/gRPC exporters and
registers them globally. The exporters read standard OTel env vars
(OTEL_EXPORTER_OTLP_ENDPOINT, OTEL_EXPORTER_OTLP_HEADERS, etc.)
automatically, so no endpoint/auth is hardcoded here.

Send traces and metrics to Grafana Cloud either:
  A) Direct — set OTEL_EXPORTER_OTLP_ENDPOINT to your Cloud OTLP gateway
     URL (from Cloud portal → stack Details) and OTEL_EXPORTER_OTLP_HEADERS
     with Basic auth.
  B) Via Alloy or an OTel Collector — set OTEL_EXPORTER_OTLP_ENDPOINT to
     your collector endpoint and let the collector forward to Cloud.
"""

from __future__ import annotations

import os
from dataclasses import dataclass

from opentelemetry import metrics, trace
from opentelemetry.exporter.otlp.proto.grpc.metric_exporter import OTLPMetricExporter
from opentelemetry.exporter.otlp.proto.grpc.trace_exporter import OTLPSpanExporter
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.metrics.export import PeriodicExportingMetricReader
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor


@dataclass
class OpenTelemetry:
    tracer_provider: TracerProvider
    meter_provider: MeterProvider

    def shutdown(self) -> None:
        self.tracer_provider.shutdown()
        self.meter_provider.shutdown()


def setup_opentelemetry() -> OpenTelemetry:
    service_name = os.getenv("OTEL_SERVICE_NAME", "sigil-langchain-weather-example")

    resource = Resource.create({"service.name": service_name})

    tracer_provider = TracerProvider(resource=resource)
    tracer_provider.add_span_processor(BatchSpanProcessor(OTLPSpanExporter()))
    trace.set_tracer_provider(tracer_provider)

    meter_provider = MeterProvider(
        resource=resource,
        metric_readers=[PeriodicExportingMetricReader(OTLPMetricExporter())],
    )
    metrics.set_meter_provider(meter_provider)

    return OpenTelemetry(
        tracer_provider=tracer_provider,
        meter_provider=meter_provider,
    )
