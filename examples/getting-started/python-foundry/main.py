"""Minimal Sigil example using Microsoft Foundry's Python SDK."""

from __future__ import annotations

import os

from dotenv import load_dotenv
from opentelemetry import metrics, trace
from opentelemetry.exporter.otlp.proto.http.metric_exporter import OTLPMetricExporter
from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.metrics.export import PeriodicExportingMetricReader
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor
from sigil_sdk import AuthConfig, Client, ClientConfig, GenerationExportConfig
from sigil_sdk_foundry import FoundryOptions, responses

load_dotenv()


def env(name: str, default: str) -> str:
    value = os.getenv(name, default).strip()
    return value or default


def required_env(name: str) -> str:
    value = os.getenv(name, "").strip()
    if not value:
        raise RuntimeError(f"{name} must be set (see .env.example).")
    return value


def setup_otel() -> tuple[TracerProvider, MeterProvider]:
    resource = Resource.create({"service.name": env("OTEL_SERVICE_NAME", "sigil-foundry-example")})

    tracer_provider = TracerProvider(resource=resource)
    tracer_provider.add_span_processor(BatchSpanProcessor(OTLPSpanExporter()))
    trace.set_tracer_provider(tracer_provider)

    reader = PeriodicExportingMetricReader(
        OTLPMetricExporter(),
        export_interval_millis=int(env("OTEL_METRIC_EXPORT_INTERVAL_MILLIS", "1000")),
    )
    meter_provider = MeterProvider(resource=resource, metric_readers=[reader])
    metrics.set_meter_provider(meter_provider)

    return tracer_provider, meter_provider


def create_sigil_client(tracer_provider: TracerProvider, meter_provider: MeterProvider) -> Client:
    auth_mode = env("SIGIL_AUTH_MODE", "none")
    tenant_id = os.getenv("SIGIL_AUTH_TENANT_ID", "").strip()
    token = os.getenv("SIGIL_AUTH_TOKEN", "").strip()

    return Client(
        ClientConfig(
            generation_export=GenerationExportConfig(
                protocol=env("SIGIL_PROTOCOL", "http"),
                endpoint=required_env("SIGIL_ENDPOINT"),
                auth=AuthConfig(
                    mode=auth_mode,
                    tenant_id=tenant_id,
                    basic_user=tenant_id,
                    basic_password=token,
                    bearer_token=token,
                ),
            ),
            tracer=tracer_provider.get_tracer("sigil-foundry-example"),
            meter=meter_provider.get_meter("sigil-foundry-example"),
            tags={"example": "python-foundry"},
        )
    )


def main() -> None:
    endpoint = required_env("AZURE_FOUNDRY_PROJECT_ENDPOINT")
    model = env("AZURE_FOUNDRY_MODEL", "gpt-5.2")
    conversation_id = env("SIGIL_CONVERSATION_ID", "sigil-foundry-demo")

    tracer_provider, meter_provider = setup_otel()
    sigil = create_sigil_client(tracer_provider, meter_provider)

    try:
        response = responses.create_from_project(
            sigil,
            endpoint,
            {
                "model": model,
                "instructions": "You are concise and practical.",
                "input": "Give one concrete reason to instrument AI agents in production.",
            },
            options=FoundryOptions(
                conversation_id=conversation_id,
                agent_name="foundry-demo-agent",
                agent_version=env("SIGIL_AGENT_VERSION", "dev"),
                tags={"provider": "azure_foundry"},
            ),
        )

        print(f"Conversation: {conversation_id}")
        print(f"Response: {response.output_text}")
    finally:
        sigil.shutdown()
        tracer_provider.shutdown()
        meter_provider.shutdown()

    print("Done")


if __name__ == "__main__":
    main()
