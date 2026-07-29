"""Public exports for agento11y LiteLLM callback handler."""

from collections.abc import Sequence
from typing import Any

from agento11y import Client

from .guard import DEFAULT_GUARDRAIL_NAME, Agento11yLiteLLMGuardrail
from .handler import DEFAULT_AGENT_NAME_METADATA_KEYS, Agento11yLiteLLMLogger


def create_agento11y_litellm_logger(
    *,
    client: Client,
    capture_inputs: bool = True,
    capture_outputs: bool = True,
    agent_name: str = "",
    agent_name_metadata_keys: Sequence[str] = DEFAULT_AGENT_NAME_METADATA_KEYS,
    agent_version: str = "",
    conversation_id: str = "",
    extra_tags: dict[str, str] | None = None,
    extra_metadata: dict[str, Any] | None = None,
) -> Agento11yLiteLLMLogger:
    """Create a LiteLLM agento11y callback logger."""
    return Agento11yLiteLLMLogger(
        client=client,
        capture_inputs=capture_inputs,
        capture_outputs=capture_outputs,
        agent_name=agent_name,
        agent_name_metadata_keys=agent_name_metadata_keys,
        agent_version=agent_version,
        conversation_id=conversation_id,
        extra_tags=extra_tags,
        extra_metadata=extra_metadata,
    )


def create_agento11y_litellm_guardrail(
    *,
    client: Client,
    agent_name: str = "",
    agent_name_metadata_keys: Sequence[str] = DEFAULT_AGENT_NAME_METADATA_KEYS,
    agent_version: str = "",
    max_concurrent_evaluations: int = 32,
    request_timeout_seconds: float = 2.0,
    extra_tags: dict[str, str] | None = None,
    guardrail_name: str = DEFAULT_GUARDRAIL_NAME,
    default_on: bool = False,
) -> Agento11yLiteLLMGuardrail:
    """Create a LiteLLM guardrail that enforces agento11y preflight hook rules.

    Enforcement only happens behind the LiteLLM proxy; pre-call hooks are never
    invoked on the ``litellm.completion()`` SDK path.
    """
    return Agento11yLiteLLMGuardrail(
        client=client,
        agent_name=agent_name,
        agent_name_metadata_keys=agent_name_metadata_keys,
        agent_version=agent_version,
        max_concurrent_evaluations=max_concurrent_evaluations,
        request_timeout_seconds=request_timeout_seconds,
        extra_tags=extra_tags,
        guardrail_name=guardrail_name,
        default_on=default_on,
    )


__all__ = [
    "DEFAULT_AGENT_NAME_METADATA_KEYS",
    "DEFAULT_GUARDRAIL_NAME",
    "Agento11yLiteLLMGuardrail",
    "Agento11yLiteLLMLogger",
    "create_agento11y_litellm_guardrail",
    "create_agento11y_litellm_logger",
]
