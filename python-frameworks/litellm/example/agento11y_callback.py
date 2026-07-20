"""Sigil callback for LiteLLM proxy."""

import os

from agento11y import Client
from agento11y.config import AuthConfig, ClientConfig, GenerationExportConfig
from agento11y_litellm import Agento11yLiteLLMLogger

_endpoint = os.environ["AGENTO11Y_ENDPOINT"]

client = Client(
    ClientConfig(
        generation_export=GenerationExportConfig(
            protocol="http",
            endpoint=_endpoint,
            auth=AuthConfig(
                mode="basic",
                tenant_id=os.environ.get("AGENTO11Y_AUTH_TENANT_ID", ""),
                basic_password=os.environ.get("AGENTO11Y_AUTH_TOKEN", ""),
            ),
        ),
    )
)

agento11y_handler = Agento11yLiteLLMLogger(
    client=client,
    agent_name="litellm-proxy-integration-test",
)
