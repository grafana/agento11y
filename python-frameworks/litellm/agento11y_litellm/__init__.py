"""Public exports for Sigil LiteLLM callback handler."""

from typing import Any

from agento11y import Client

from .handler import SigilLiteLLMLogger


def create_sigil_litellm_logger(
    *,
    client: Client,
    capture_inputs: bool = True,
    capture_outputs: bool = True,
    agent_name: str = "",
    agent_version: str = "",
    conversation_id: str = "",
    extra_tags: dict[str, str] | None = None,
    extra_metadata: dict[str, Any] | None = None,
) -> SigilLiteLLMLogger:
    """Create a LiteLLM Sigil callback logger."""
    return SigilLiteLLMLogger(
        client=client,
        capture_inputs=capture_inputs,
        capture_outputs=capture_outputs,
        agent_name=agent_name,
        agent_version=agent_version,
        conversation_id=conversation_id,
        extra_tags=extra_tags,
        extra_metadata=extra_metadata,
    )


__all__ = [
    "SigilLiteLLMLogger",
    "create_sigil_litellm_logger",
]
