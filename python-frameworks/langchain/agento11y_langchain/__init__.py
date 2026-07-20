"""Public exports for Sigil LangChain callback handlers."""

from typing import Any

from agento11y import Client

from .handler import SigilAsyncLangChainHandler, SigilLangChainHandler


def create_sigil_langchain_handler(
    *,
    client: Client,
    async_handler: bool = False,
    **handler_kwargs: Any,
) -> SigilLangChainHandler | SigilAsyncLangChainHandler:
    """Create a LangChain Sigil callback handler for sync or async flows."""
    if async_handler:
        return SigilAsyncLangChainHandler(client=client, **handler_kwargs)
    return SigilLangChainHandler(client=client, **handler_kwargs)


def with_sigil_langchain_callbacks(
    config: dict[str, Any] | None,
    *,
    client: Client,
    async_handler: bool = False,
    **handler_kwargs: Any,
) -> dict[str, Any]:
    """Append a Sigil callback handler to a LangChain runnable config."""
    merged = dict(config or {})
    existing = merged.get("callbacks")
    if isinstance(existing, list):
        callbacks = list(existing)
    elif existing is None:
        callbacks = []
    else:
        callbacks = [existing]
    if not any(isinstance(item, (SigilLangChainHandler, SigilAsyncLangChainHandler)) for item in callbacks):
        callbacks.append(create_sigil_langchain_handler(client=client, async_handler=async_handler, **handler_kwargs))
    merged["callbacks"] = callbacks
    return merged


__all__ = [
    "SigilLangChainHandler",
    "SigilAsyncLangChainHandler",
    "create_sigil_langchain_handler",
    "with_sigil_langchain_callbacks",
]
