from __future__ import annotations

import os

from agento11y import Client, ClientConfig
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.trace import TracerProvider


def setup_agento11y(
    *,
    tracer_provider: TracerProvider | None,
    meter_provider: MeterProvider | None,
) -> Client:
    config = ClientConfig(
        tracer=(tracer_provider.get_tracer("agento11y") if tracer_provider is not None else None),
        meter=(meter_provider.get_meter("agento11y") if meter_provider is not None else None),
        agent_name=os.getenv(
            "AGENTO11Y_AGENT_NAME",
            "agentcore-operations-demo",
        ),
        agent_version=os.getenv("AGENTO11Y_AGENT_VERSION", "0.1.0"),
    )
    return Client(config)
