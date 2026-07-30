from __future__ import annotations

import os
from dataclasses import dataclass
from typing import Any
from urllib.parse import unquote, urlparse

from opentelemetry import metrics, trace
from opentelemetry.exporter.otlp.proto.grpc.metric_exporter import (
    OTLPMetricExporter as GrpcMetricExporter,
)
from opentelemetry.exporter.otlp.proto.grpc.trace_exporter import (
    OTLPSpanExporter as GrpcSpanExporter,
)
from opentelemetry.exporter.otlp.proto.http.metric_exporter import (
    OTLPMetricExporter as HttpMetricExporter,
)
from opentelemetry.exporter.otlp.proto.http.trace_exporter import (
    OTLPSpanExporter as HttpSpanExporter,
)
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.metrics.export import PeriodicExportingMetricReader
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor


@dataclass
class Telemetry:
    tracer_provider: TracerProvider | None
    meter_provider: MeterProvider | None
    tracer: Any
    requests: Any
    duration: Any

    def shutdown(self) -> None:
        if self.tracer_provider is not None:
            self.tracer_provider.shutdown()
        if self.meter_provider is not None:
            self.meter_provider.shutdown()


def setup_opentelemetry() -> Telemetry:
    endpoint, insecure, headers, protocol = _otlp_exporter_options()
    service_name = os.getenv("OTEL_SERVICE_NAME", "agento11y-bedrock-agentcore")
    resource = Resource.create(
        {
            "service.name": service_name,
            "service.version": os.getenv("AGENTO11Y_AGENT_VERSION", "0.1.0"),
            "deployment.environment": os.getenv("DEPLOYMENT_ENVIRONMENT", "dev"),
            **_parse_resource_attributes(os.getenv("OTEL_RESOURCE_ATTRIBUTES", "")),
        }
    )

    if endpoint:
        tracer_provider: TracerProvider | None = TracerProvider(resource=resource)
        if protocol == "http/protobuf":
            traces_endpoint, metrics_endpoint = _http_signal_endpoints(endpoint)
            span_exporter = HttpSpanExporter(endpoint=traces_endpoint, headers=headers)
            metric_exporter = HttpMetricExporter(endpoint=metrics_endpoint, headers=headers)
        else:
            grpc_endpoint = _grpc_endpoint(endpoint)
            span_exporter = GrpcSpanExporter(
                endpoint=grpc_endpoint,
                insecure=insecure,
                headers=headers,
            )
            metric_exporter = GrpcMetricExporter(
                endpoint=grpc_endpoint,
                insecure=insecure,
                headers=headers,
            )

        tracer_provider.add_span_processor(BatchSpanProcessor(span_exporter))
        trace.set_tracer_provider(tracer_provider)

        meter_provider: MeterProvider | None = MeterProvider(
            resource=resource,
            metric_readers=[PeriodicExportingMetricReader(metric_exporter)],
        )
        metrics.set_meter_provider(meter_provider)
    else:
        tracer_provider = None
        meter_provider = None

    tracer = trace.get_tracer(service_name)
    meter = metrics.get_meter(service_name)
    return Telemetry(
        tracer_provider=tracer_provider,
        meter_provider=meter_provider,
        tracer=tracer,
        requests=meter.create_counter(
            "agentcore_demo.agent.requests",
            description="Agent invocation count.",
        ),
        duration=meter.create_histogram(
            "agentcore_demo.agent.duration",
            unit="s",
            description="Agent invocation duration.",
        ),
    )


def _otlp_exporter_options() -> tuple[str, bool, dict[str, str] | None, str]:
    endpoint = os.getenv("OTEL_EXPORTER_OTLP_ENDPOINT", "").strip()
    raw_headers = os.getenv("OTEL_EXPORTER_OTLP_HEADERS", "").strip()
    protocol = os.getenv("OTEL_EXPORTER_OTLP_PROTOCOL", "grpc").strip().lower()
    insecure = os.getenv("OTEL_EXPORTER_OTLP_INSECURE", "").strip().lower() in {
        "1",
        "true",
        "yes",
        "on",
    }
    headers = _parse_otlp_headers(raw_headers)

    if not endpoint:
        return "", insecure, headers, protocol

    parsed = urlparse(endpoint if "://" in endpoint else f"//{endpoint}")
    is_local = parsed.hostname in {"localhost", "127.0.0.1", "0.0.0.0", "::1"}
    if parsed.scheme == "http" or is_local:
        insecure = True
    if "/otlp" in parsed.path or protocol in {"http/protobuf", "http/json"}:
        protocol = "http/protobuf"
    return endpoint, insecure, headers, protocol


def _parse_otlp_headers(raw: str) -> dict[str, str] | None:
    headers: dict[str, str] = {}
    for item in raw.split(","):
        item = item.strip()
        if not item or "=" not in item:
            continue
        key, value = item.split("=", 1)
        key = unquote(key.strip())
        value = unquote(value.strip().strip('"'))
        if key:
            headers[key] = value
    return headers or None


def _http_signal_endpoints(endpoint: str) -> tuple[str, str]:
    base = endpoint.rstrip("/")
    if base.endswith("/v1/traces"):
        return base, f"{base.rsplit('/', 1)[0]}/metrics"
    if base.endswith("/v1/metrics"):
        return f"{base.rsplit('/', 1)[0]}/traces", base
    return f"{base}/v1/traces", f"{base}/v1/metrics"


def _grpc_endpoint(endpoint: str) -> str:
    parsed = urlparse(endpoint if "://" in endpoint else f"//{endpoint}")
    if not parsed.hostname:
        return endpoint
    port = parsed.port
    if port is None:
        port = 443 if parsed.scheme == "https" else 80
    return f"{parsed.hostname}:{port}"


def _parse_resource_attributes(raw: str) -> dict[str, str]:
    attributes: dict[str, str] = {}
    for item in raw.split(","):
        item = item.strip()
        if not item or "=" not in item:
            continue
        key, value = item.split("=", 1)
        if key.strip():
            attributes[key.strip()] = value.strip()
    return attributes
