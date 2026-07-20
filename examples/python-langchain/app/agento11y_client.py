"""Sigil client bootstrap.

Builds an `agento11y.Client` that reuses the app's OTel providers, so the
`gen_ai.*` spans and metrics it emits flow through the same pipeline as
everything else. Config comes from environment variables (see `.env.example`).
"""

from __future__ import annotations

import os
from urllib.parse import urlparse

from opentelemetry.metrics import MeterProvider
from opentelemetry.trace import TracerProvider

from agento11y import ApiConfig, AuthConfig, Client, ClientConfig, GenerationExportConfig


def _required_env(name: str) -> str:
    value = os.getenv(name, "").strip()
    if not value:
        raise RuntimeError(f"{name} must be set (see .env.example).")
    return value


def setup_agento11y(
    *,
    tracer_provider: TracerProvider,
    meter_provider: MeterProvider,
) -> Client:
    endpoint = _required_env("AGENTO11Y_ENDPOINT")
    parsed = urlparse(endpoint)
    api_endpoint = f"{parsed.scheme}://{parsed.netloc}"
    tenant_id = _required_env("AGENTO11Y_AUTH_TENANT_ID")
    auth_token = os.getenv("AGENTO11Y_AUTH_TOKEN", "").strip()

    if auth_token:
        # Grafana Cloud: basic auth (user = instance ID, password = access token).
        auth = AuthConfig(
            mode="basic",
            tenant_id=tenant_id,
            basic_user=tenant_id,
            basic_password=auth_token,
        )
    else:
        # Self-hosted / local dev: just X-Scope-OrgID, no credentials.
        auth = AuthConfig(mode="tenant", tenant_id=tenant_id)

    return Client(
        ClientConfig(
            generation_export=GenerationExportConfig(
                protocol="http",
                endpoint=endpoint,
                auth=auth,
            ),
            api=ApiConfig(endpoint=api_endpoint),
            tracer=tracer_provider.get_tracer("agento11y"),
            meter=meter_provider.get_meter("agento11y"),
        )
    )
