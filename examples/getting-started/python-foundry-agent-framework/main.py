"""Minimal Sigil example using Microsoft Agent Framework with Foundry."""

from __future__ import annotations

import asyncio
import os

from agent_framework import Agent, tool
from agent_framework.foundry import FoundryAgent, FoundryChatClient
from azure.identity import AzureCliCredential
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
from sigil_sdk_agent_framework_foundry import create_sigil_foundry_middleware

load_dotenv()


@tool
def deployment_status(service: str) -> str:
    """Return the deployment status for a service."""
    return f"{service} is green in production."


def env(name: str, default: str) -> str:
    value = os.getenv(name, default).strip()
    return value or default


def required_env(name: str) -> str:
    value = os.getenv(name, "").strip()
    if not value:
        raise RuntimeError(f"{name} must be set (see .env.example).")
    return value


def setup_otel() -> tuple[TracerProvider, MeterProvider]:
    resource = Resource.create({"service.name": env("OTEL_SERVICE_NAME", "sigil-foundry-agent-framework-example")})

    tracer_provider = TracerProvider(resource=resource)
    tracer_provider.add_span_processor(BatchSpanProcessor(OTLPSpanExporter()))
    trace.set_tracer_provider(tracer_provider)

    meter_provider = MeterProvider(
        resource=resource,
        metric_readers=[
            PeriodicExportingMetricReader(
                OTLPMetricExporter(),
                export_interval_millis=int(env("OTEL_METRIC_EXPORT_INTERVAL_MILLIS", "1000")),
            )
        ],
    )
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
            tracer=tracer_provider.get_tracer("sigil-foundry-agent-framework-example"),
            meter=meter_provider.get_meter("sigil-foundry-agent-framework-example"),
            tags={"example": "python-foundry-agent-framework"},
        )
    )


def build_agent(sigil: Client):
    endpoint = required_env("AZURE_FOUNDRY_PROJECT_ENDPOINT")
    credential = AzureCliCredential()
    middleware = create_sigil_foundry_middleware(
        client=sigil,
        conversation_id=env("SIGIL_CONVERSATION_ID", "sigil-foundry-agent-framework-demo"),
        agent_name="foundry-agent-framework-demo",
        agent_version=env("SIGIL_AGENT_VERSION", "dev"),
        extra_tags={"provider": "azure_foundry"},
    )

    if env("FOUNDRY_AGENT_MODE", "chat").lower() == "hosted":
        return FoundryAgent(
            project_endpoint=endpoint,
            agent_name=required_env("AZURE_FOUNDRY_AGENT_NAME"),
            agent_version=os.getenv("AZURE_FOUNDRY_AGENT_VERSION") or None,
            credential=credential,
            middleware=middleware,
        )

    return Agent(
        client=FoundryChatClient(
            project_endpoint=endpoint,
            model=env("AZURE_FOUNDRY_MODEL", "gpt-5.2"),
            credential=credential,
        ),
        name="foundry-agent-framework-demo",
        instructions="You are concise and practical.",
        tools=[deployment_status],
        middleware=middleware,
    )


async def main() -> None:
    tracer_provider, meter_provider = setup_otel()
    sigil = create_sigil_client(tracer_provider, meter_provider)

    try:
        agent = build_agent(sigil)
        response = await agent.run("Use the deployment_status tool for checkout, then summarize in one sentence.")
        print(f"Conversation: {env('SIGIL_CONVERSATION_ID', 'sigil-foundry-agent-framework-demo')}")
        print(f"Response: {response.text}")
    finally:
        sigil.shutdown()
        tracer_provider.shutdown()
        meter_provider.shutdown()

    print("Done")


if __name__ == "__main__":
    asyncio.run(main())
