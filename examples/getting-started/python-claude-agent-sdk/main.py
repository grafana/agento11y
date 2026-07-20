"""Minimal Sigil Cloud example using the Claude Agent SDK."""

from __future__ import annotations

import asyncio
import os

from claude_agent_sdk import ClaudeAgentOptions, ResultMessage
from dotenv import load_dotenv
from opentelemetry import metrics, trace
from opentelemetry.exporter.otlp.proto.http.metric_exporter import OTLPMetricExporter
from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.metrics.export import PeriodicExportingMetricReader
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor
from agento11y import AuthConfig, Client, ClientConfig, GenerationExportConfig
from agento11y_claude_agent import SigilClaudeSDKClient

load_dotenv()


def env(name: str, default: str) -> str:
    value = os.getenv(name, default).strip()
    return value or default


def required_env(name: str) -> str:
    value = os.getenv(name, "").strip()
    if not value:
        raise RuntimeError(f"{name} must be set (see .env.example).")
    return value


def otlp_metrics_endpoint() -> str:
    endpoint = os.getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "").strip()
    if endpoint:
        return endpoint

    endpoint = os.getenv("OTEL_EXPORTER_OTLP_ENDPOINT", "").strip()
    if not endpoint:
        raise RuntimeError(
            "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT or OTEL_EXPORTER_OTLP_ENDPOINT must be set (see .env.example)."
        )
    if endpoint.endswith("/v1/metrics"):
        return endpoint
    return f"{endpoint.rstrip('/')}/v1/metrics"


def setup_otel() -> tuple[TracerProvider, MeterProvider]:
    resource = Resource.create({"service.name": env("OTEL_SERVICE_NAME", "sigil-claude-agent-sdk-example")})

    tracer_provider = TracerProvider(resource=resource)
    tracer_provider.add_span_processor(BatchSpanProcessor(OTLPSpanExporter()))
    trace.set_tracer_provider(tracer_provider)

    exporter = OTLPMetricExporter(endpoint=otlp_metrics_endpoint())
    reader = PeriodicExportingMetricReader(
        exporter,
        export_interval_millis=int(env("OTEL_METRIC_EXPORT_INTERVAL_MILLIS", "60000")),
    )
    meter_provider = MeterProvider(resource=resource, metric_readers=[reader])
    metrics.set_meter_provider(meter_provider)
    return tracer_provider, meter_provider


async def main() -> None:
    tracer_provider, meter_provider = setup_otel()
    tenant_id = required_env("AGENTO11Y_AUTH_TENANT_ID")
    sigil = Client(
        ClientConfig(
            generation_export=GenerationExportConfig(
                protocol=os.getenv("AGENTO11Y_PROTOCOL", "http"),
                endpoint=required_env("AGENTO11Y_ENDPOINT"),
                auth=AuthConfig(
                    mode="basic",
                    tenant_id=tenant_id,
                    basic_user=tenant_id,
                    basic_password=required_env("AGENTO11Y_AUTH_TOKEN"),
                ),
            ),
            meter=meter_provider.get_meter("sigil-claude-agent-sdk-example"),
        )
    )

    try:
        async with SigilClaudeSDKClient(
            client=sigil,
            options=ClaudeAgentOptions(
                model=env("CLAUDE_MODEL", "claude-sonnet-4-5"),
                permission_mode="default",
                allowed_tools=["Read", "Glob", "Grep"],
            ),
            conversation_id=env("AGENTO11Y_CONVERSATION_ID", "sigil-claude-agent-sdk-demo"),
            agent_name="claude-agent-sdk-demo",
            agent_version=env("AGENTO11Y_AGENT_VERSION", "dev"),
        ) as claude:
            await claude.query("Use read-only tools if needed, then explain what this repository is in one sentence.")
            async for message in claude.receive_response():
                if isinstance(message, ResultMessage) and message.result:
                    print(message.result)
    finally:
        sigil.shutdown()
        tracer_provider.shutdown()
        meter_provider.shutdown()

    print("Done")


if __name__ == "__main__":
    asyncio.run(main())
