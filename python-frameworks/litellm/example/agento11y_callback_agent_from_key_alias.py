"""agento11y callback for LiteLLM proxy that names agents after the calling key.

The handler already resolves ``agent_name`` from per-request metadata and from
LiteLLM's own ``agent_id``, so callers that identify themselves need nothing
extra. This variant covers the remaining case: callers that send no agent
identity at all, which would otherwise collapse into one proxy-wide name. It
appends the virtual key's alias to the keys the handler consults, so each key
shows up as its own agent.

Only worth it when your keys map one-to-one onto agents. A key alias names a
credential, not an agent, so rotating a key renames the agent and a shared key
merges unrelated callers.
"""

import os

from agento11y import Client
from agento11y.config import AuthConfig, ClientConfig, GenerationExportConfig
from agento11y_litellm import DEFAULT_AGENT_NAME_METADATA_KEYS, Agento11yLiteLLMLogger

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
    agent_name_metadata_keys=(
        *DEFAULT_AGENT_NAME_METADATA_KEYS,
        "user_api_key_alias",
        "user_api_key_team_alias",
    ),
    # Used only when a request matches none of the keys above.
    agent_name="litellm-proxy-integration-test",
)
