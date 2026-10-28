"""The same order-support agent, tools, and conversation for both variants."""

from __future__ import annotations

import os

from agents import Agent, function_tool
from opentelemetry import metrics, trace
from opentelemetry.exporter.otlp.proto.http.metric_exporter import OTLPMetricExporter
from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.metrics.export import PeriodicExportingMetricReader
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import SpanProcessor, TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor

AGENT_NAME = "order-support"
PROMPTS = (
    "Use lookup_order to check the status of order DEMO-1001.",
    "For that same order, use shipping_eta to check how many business days remain. Answer in one sentence.",
)
ORDERS = {
    "DEMO-1001": {"status": "shipped", "business_days_remaining": 2},
    "DEMO-1002": {"status": "processing", "business_days_remaining": 5},
}


def env(name: str, default: str) -> str:
    value = os.getenv(name, default).strip()
    return value or default


def required_env(name: str) -> str:
    value = os.getenv(name, "").strip()
    if not value:
        raise RuntimeError(f"{name} must be set (see .env.example).")
    return value


@function_tool
def lookup_order(order_id: str) -> str:
    """Look up the status of a demo order by its order ID."""
    order = ORDERS.get(order_id)
    return f"{order_id}: {order['status']}" if order else "Order not found."


@function_tool
def shipping_eta(order_id: str) -> str:
    """Return the estimated business days remaining for a demo order."""
    order = ORDERS.get(order_id)
    return f"{order_id}: {order['business_days_remaining']} business days remaining." if order else "Order not found."


def create_agent() -> Agent:
    return Agent(
        name=AGENT_NAME,
        model=env("OPENAI_MODEL", "gpt-4o-mini"),
        instructions=(
            "You are a concise order-support assistant. Use the tools for order facts. "
            "Use the order ID from the conversation for follow-up questions. "
            "Never invent an order status or delivery estimate."
        ),
        tools=[lookup_order, shipping_eta],
    )


def setup_otel(span_processor: SpanProcessor | None = None) -> tuple[TracerProvider, MeterProvider]:
    # Both exporters read OTEL_EXPORTER_OTLP_ENDPOINT / OTEL_EXPORTER_OTLP_HEADERS,
    # and honor the signal-specific endpoint/header overrides when present.
    required_env("OTEL_EXPORTER_OTLP_ENDPOINT")
    resource = Resource.create({"service.name": env("OTEL_SERVICE_NAME", "agento11y-openai-agents-example")})
    tracer_provider = TracerProvider(resource=resource)
    if span_processor is not None:
        tracer_provider.add_span_processor(span_processor)
    tracer_provider.add_span_processor(BatchSpanProcessor(OTLPSpanExporter()))
    trace.set_tracer_provider(tracer_provider)

    reader = PeriodicExportingMetricReader(
        OTLPMetricExporter(),
        export_interval_millis=int(env("OTEL_METRIC_EXPORT_INTERVAL_MILLIS", "1000")),
    )
    meter_provider = MeterProvider(resource=resource, metric_readers=[reader])
    metrics.set_meter_provider(meter_provider)
    return tracer_provider, meter_provider
