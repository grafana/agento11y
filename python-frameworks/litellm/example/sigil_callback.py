"""Sigil callback for LiteLLM proxy."""

import os

from agento11y import Client
from agento11y.config import AuthConfig, ClientConfig, GenerationExportConfig
from agento11y_litellm import SigilLiteLLMLogger

_endpoint = os.environ["SIGIL_ENDPOINT"]

client = Client(
    ClientConfig(
        generation_export=GenerationExportConfig(
            protocol="http",
            endpoint=_endpoint,
            auth=AuthConfig(
                mode="basic",
                tenant_id=os.environ.get("SIGIL_AUTH_TENANT_ID", ""),
                basic_password=os.environ.get("SIGIL_AUTH_TOKEN", ""),
            ),
        ),
    )
)

sigil_handler = SigilLiteLLMLogger(
    client=client,
    agent_name="litellm-proxy-integration-test",
)
