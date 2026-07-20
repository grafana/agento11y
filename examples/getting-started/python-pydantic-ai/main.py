"""Minimal AI Observability getting-started example — Python + Pydantic AI."""

import os
import uuid
from dataclasses import dataclass
from datetime import datetime, timezone

from dotenv import load_dotenv
from opentelemetry import metrics, trace
from opentelemetry.exporter.otlp.proto.http.metric_exporter import OTLPMetricExporter
from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.metrics.export import PeriodicExportingMetricReader
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor
from pydantic_ai import Agent, RunContext
from agento11y import (
    AuthConfig,
    Client,
    ClientConfig,
    GenerationExportConfig,
    with_conversation_title,
)
from agento11y_pydantic_ai import with_sigil_pydantic_ai_capability

load_dotenv()


@dataclass
class Deps:
    conversation_id: str


# Register OTel providers so SDK-emitted traces and metrics are exported.
# OTLPSpanExporter / OTLPMetricExporter pick up OTEL_EXPORTER_OTLP_ENDPOINT and
# OTEL_EXPORTER_OTLP_HEADERS automatically.
resource = Resource.create({"service.name": "getting-started-python-pydantic-ai"})

tp = TracerProvider(resource=resource)
tp.add_span_processor(BatchSpanProcessor(OTLPSpanExporter()))
trace.set_tracer_provider(tp)

mp = MeterProvider(
    resource=resource,
    metric_readers=[PeriodicExportingMetricReader(OTLPMetricExporter())],
)
metrics.set_meter_provider(mp)

sigil = Client(
    ClientConfig(
        generation_export=GenerationExportConfig(
            protocol="http",
            endpoint=os.environ["AGENTO11Y_ENDPOINT"],
            auth=AuthConfig(
                mode="basic",
                tenant_id=os.environ["AGENTO11Y_AUTH_TENANT_ID"],
                basic_password=os.environ["AGENTO11Y_AUTH_TOKEN"],
            ),
        ),
    )
)

agent = Agent(
    "anthropic:claude-haiku-4-5",
    deps_type=Deps,
    capabilities=with_sigil_pydantic_ai_capability(None, client=sigil),
)


@agent.tool
def get_current_time(ctx: RunContext[Deps]) -> str:
    """Return the current UTC time in ISO 8601 format."""
    return datetime.now(timezone.utc).isoformat()


deps = Deps(
    conversation_id=f"getting-started-python-pydantic-ai-{uuid.uuid4().hex[:12]}"
)

with with_conversation_title("getting-started-python-pydantic-ai"):
    first = agent.run_sync("What is the current UTC time?", deps=deps)
    print(f"Reply 1: {first.output}\n")

    second = agent.run_sync(
        "Now explain what LLM observability is in one sentence.",
        deps=deps,
        message_history=first.all_messages(),
    )
    print(f"Reply 2: {second.output}\n")

# Shut down Sigil first so in-flight generations finish exporting before OTel stops.
sigil.shutdown()
tp.shutdown()
mp.shutdown()
print("Done — check the AI Observability plugin in your Grafana Cloud stack.")
