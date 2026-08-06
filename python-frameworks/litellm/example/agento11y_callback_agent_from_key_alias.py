"""agento11y callback for LiteLLM proxy that names agents after the calling key.

A drop-in replacement for ``agento11y_callback``: it exports the same two names
from one client, so ``config.yaml`` swaps both lines and the proxy still runs a
single SDK client.

The handler already resolves ``agent_name`` from per-request metadata and from
LiteLLM's own ``agent_id``, so callers that identify themselves need nothing
extra. This variant covers the remaining case: callers that send no agent
identity at all, which would otherwise collapse into one proxy-wide name. It
appends the virtual key's alias to the keys the handler consults, so each key
shows up as its own agent.

Only worth it when your keys map one-to-one onto agents. A key alias names a
credential, not an agent, so rotating a key renames the agent and a shared key
merges unrelated callers.

Virtual keys are a database feature: this file resolves nothing until the proxy
runs with ``DATABASE_URL`` and ``LITELLM_MASTER_KEY`` set and callers
authenticate with a key that has an alias. Without them every request still
lands on the fallback name below.
"""

import os

from agento11y import Client
from agento11y.config import ApiConfig, AuthConfig, ClientConfig, GenerationExportConfig, HooksConfig
from agento11y_litellm import (
    DEFAULT_AGENT_NAME_METADATA_KEYS,
    Agento11yLiteLLMGuardrail,
    Agento11yLiteLLMLogger,
)

_endpoint = os.environ["AGENTO11Y_ENDPOINT"]
_fallback_agent_name = os.environ.get("AGENTO11Y_AGENT_NAME", "litellm-proxy")

# Consulted in order, so a caller that names itself still wins over its key.
_agent_name_keys = (
    *DEFAULT_AGENT_NAME_METADATA_KEYS,
    "user_api_key_alias",
    "user_api_key_team_alias",
)

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
        hooks=HooksConfig(enabled=True, timeout_seconds=3.0, phases=["preflight", "postflight"]),
    )
)

agento11y_handler = Agento11yLiteLLMLogger(
    client=client,
    agent_name_metadata_keys=_agent_name_keys,
    # Used only when a request matches none of the keys above.
    agent_name=_fallback_agent_name,
)

# Given the same keys, so a rule matching on agent_name selects the same traffic
# the exported generations are filed under.
agento11y_guards = Agento11yLiteLLMGuardrail(
    client=client,
    agent_name_metadata_keys=_agent_name_keys,
    agent_name=_fallback_agent_name,
    request_timeout_seconds=2.0,
    default_on=True,
    event_hook=["pre_call", "post_call"],
)
