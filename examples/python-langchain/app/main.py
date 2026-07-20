"""FastAPI entry point.

Bootstraps OpenTelemetry and the Sigil client on startup, exposes them
on `app.state` for handlers, and mounts the routes from `handlers.py`.
"""

from __future__ import annotations

import os
from contextlib import asynccontextmanager

from dotenv import load_dotenv
from fastapi import FastAPI
from opentelemetry.instrumentation.fastapi import FastAPIInstrumentor

from .agent import build_agent
from .handlers import router
from .agento11y_client import setup_agento11y
from .telemetry import setup_opentelemetry


load_dotenv()


@asynccontextmanager
async def lifespan(app: FastAPI):
    otel = setup_opentelemetry()
    agento11y = setup_agento11y(
        tracer_provider=otel.tracer_provider,
        meter_provider=otel.meter_provider,
    )

    app.state.otel = otel
    app.state.agento11y = agento11y
    app.state.agent = build_agent(os.getenv("AGENT_MODEL", "claude-sonnet-4-5"))
    app.state.classifier_model = os.getenv("CLASSIFIER_MODEL", "claude-haiku-4-5")

    FastAPIInstrumentor.instrument_app(app, tracer_provider=otel.tracer_provider)

    try:
        yield
    finally:
        # Shut down Sigil first so in-flight generations finish exporting
        # before OTel tears down the gRPC channels underneath.
        agento11y.shutdown()
        otel.shutdown()


app = FastAPI(title="Sigil + LangChain weather example", lifespan=lifespan)
app.include_router(router)
