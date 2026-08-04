"""agento11y callback for LiteLLM proxy.

Registers two objects with the proxy:

- ``agento11y_handler`` exports one generation per request.
- ``agento11y_guards`` evaluates Agent Observability guards on the request path
  and fails a request a rule denies.

``config.yaml`` lists both by dotted path.
"""

import os

from agento11y import Client
from agento11y.config import ApiConfig, AuthConfig, ClientConfig, GenerationExportConfig, HooksConfig
from agento11y_litellm import Agento11yLiteLLMGuardrail, Agento11yLiteLLMLogger

_endpoint = os.environ["AGENTO11Y_ENDPOINT"]
# Used only for a request that names no agent of its own. See the README section
# "Attributing generations to the calling agent".
_agent_name = os.environ.get("AGENTO11Y_AGENT_NAME", "litellm-proxy")

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
        # Guards call the Agent Observability API, which the export endpoint does
        # not cover.
        api=ApiConfig(endpoint=_endpoint),
        hooks=HooksConfig(
            enabled=True,
            # 15s (the default) is too long to hold a proxy worker thread; keep
            # it just above the guardrail's own timeout.
            timeout_seconds=3.0,
            # Drop "postflight" to evaluate request rules only. Postflight sends
            # response content to the hooks API, on a deployment that keeps
            # content out of its logs too.
            phases=["preflight", "postflight"],
        ),
    )
)

agento11y_handler = Agento11yLiteLLMLogger(
    client=client,
    agent_name=_agent_name,
)

# Fails a denied request before it reaches the provider (preflight) and before a
# non-streamed response reaches the caller (postflight). Only active through the
# proxy: LiteLLM never runs call hooks on the direct SDK path.
agento11y_guards = Agento11yLiteLLMGuardrail(
    client=client,
    agent_name=_agent_name,
    request_timeout_seconds=2.0,
    default_on=True,
    # "pre_call" alone evaluates preflight rules only, and is the default.
    event_hook=["pre_call", "post_call"],
)
