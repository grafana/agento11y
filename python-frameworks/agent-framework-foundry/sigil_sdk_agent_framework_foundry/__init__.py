"""Microsoft Agent Framework Foundry middleware for Sigil Python SDK."""

from __future__ import annotations

from collections.abc import Sequence
from typing import Any

from sigil_sdk import Client

from .handler import (
    SigilAgentFrameworkFoundryAgentMiddleware,
    SigilAgentFrameworkFoundryChatMiddleware,
    SigilAgentFrameworkFoundryFunctionMiddleware,
    SigilAgentFrameworkFoundryHandler,
)


def create_sigil_foundry_middleware(
    *,
    client: Client,
    agent_name: str = "",
    agent_version: str = "",
    conversation_id: str = "",
    capture_inputs: bool = True,
    capture_outputs: bool = True,
    extra_tags: dict[str, str] | None = None,
    extra_metadata: dict[str, Any] | None = None,
) -> list[Any]:
    """Create Agent Framework middleware for Foundry agents, chat calls, and tools."""

    handler = SigilAgentFrameworkFoundryHandler(
        client=client,
        agent_name=agent_name,
        agent_version=agent_version,
        conversation_id=conversation_id,
        capture_inputs=capture_inputs,
        capture_outputs=capture_outputs,
        extra_tags=extra_tags,
        extra_metadata=extra_metadata,
    )
    return [
        SigilAgentFrameworkFoundryAgentMiddleware(handler),
        SigilAgentFrameworkFoundryChatMiddleware(handler),
        SigilAgentFrameworkFoundryFunctionMiddleware(handler),
    ]


def with_sigil_foundry_middleware(
    middleware: Sequence[Any] | None,
    *,
    client: Client,
    **handler_kwargs: Any,
) -> list[Any]:
    """Append Sigil Foundry middleware to an Agent Framework middleware list."""

    existing = list(middleware or [])
    if any(_is_sigil_foundry_middleware(item) for item in existing):
        return existing
    return [*existing, *create_sigil_foundry_middleware(client=client, **handler_kwargs)]


def instrument_foundry_agent(agent: Any, *, client: Client, **handler_kwargs: Any) -> Any:
    """Append Sigil middleware to an existing Agent or FoundryAgent instance."""

    current = getattr(agent, "middleware", None)
    agent.middleware = with_sigil_foundry_middleware(current, client=client, **handler_kwargs)
    return agent


def instrument_foundry_chat_client(chat_client: Any, *, client: Client, **handler_kwargs: Any) -> Any:
    """Append Sigil chat/function middleware to an existing FoundryChatClient instance."""

    middleware = create_sigil_foundry_middleware(client=client, **handler_kwargs)
    chat_and_function = [
        item
        for item in middleware
        if isinstance(
            item,
            (SigilAgentFrameworkFoundryChatMiddleware, SigilAgentFrameworkFoundryFunctionMiddleware),
        )
    ]
    current = getattr(chat_client, "chat_middleware", None)
    if current is not None:
        merged = list(current)
        for item in chat_and_function:
            if not any(type(existing) is type(item) for existing in merged):
                merged.append(item)
        chat_client.chat_middleware = merged
        return chat_client

    current = getattr(chat_client, "middleware", None)
    chat_client.middleware = [*(current or []), *chat_and_function]
    return chat_client


def _is_sigil_foundry_middleware(item: Any) -> bool:
    return isinstance(
        item,
        (
            SigilAgentFrameworkFoundryAgentMiddleware,
            SigilAgentFrameworkFoundryChatMiddleware,
            SigilAgentFrameworkFoundryFunctionMiddleware,
        ),
    )


__all__ = [
    "SigilAgentFrameworkFoundryAgentMiddleware",
    "SigilAgentFrameworkFoundryChatMiddleware",
    "SigilAgentFrameworkFoundryFunctionMiddleware",
    "SigilAgentFrameworkFoundryHandler",
    "create_sigil_foundry_middleware",
    "instrument_foundry_agent",
    "instrument_foundry_chat_client",
    "with_sigil_foundry_middleware",
]
