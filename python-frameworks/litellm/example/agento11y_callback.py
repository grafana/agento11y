"""agento11y callback for LiteLLM proxy."""

import os

from agento11y import Client
from agento11y.config import ApiConfig, AuthConfig, ClientConfig, GenerationExportConfig, HooksConfig
from agento11y_litellm import Agento11yLiteLLMGuardrail, Agento11yLiteLLMLogger

_endpoint = os.environ["AGENTO11Y_ENDPOINT"]
_agent_name = "litellm-proxy-integration-test"

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
        api=ApiConfig(endpoint=_endpoint),
        # Preflight rule enforcement. 15s (the default) is too long to hold a
        # proxy worker thread; keep it just above the guardrail's own timeout.
        hooks=HooksConfig(enabled=True, timeout_seconds=3.0),
    )
)

agento11y_handler = Agento11yLiteLLMLogger(
    client=client,
    agent_name=_agent_name,
)

# Blocks denied requests before they reach the provider. Only active through the
# proxy: LiteLLM never runs pre-call hooks on the direct SDK path.
agento11y_guards = Agento11yLiteLLMGuardrail(
    client=client,
    agent_name=_agent_name,
    request_timeout_seconds=2.0,
    default_on=True,
)
