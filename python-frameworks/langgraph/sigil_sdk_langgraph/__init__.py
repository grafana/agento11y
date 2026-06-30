"""Public exports for Sigil LangGraph callback handlers."""

from typing import Any

from sigil_sdk import Client

from .handler import SigilAsyncLangGraphHandler, SigilLangGraphHandler


def create_sigil_langgraph_handler(
    *,
    client: Client,
    async_handler: bool = False,
    **handler_kwargs: Any,
) -> SigilLangGraphHandler | SigilAsyncLangGraphHandler:
    """Create a LangGraph Sigil callback handler for sync or async flows."""
    if async_handler:
        return SigilAsyncLangGraphHandler(client=client, **handler_kwargs)
    return SigilLangGraphHandler(client=client, **handler_kwargs)


def with_sigil_langgraph_callbacks(
    config: dict[str, Any] | None,
    *,
    client: Client,
    async_handler: bool = False,
    **handler_kwargs: Any,
) -> dict[str, Any]:
    """Append a Sigil callback handler to a LangGraph invocation config."""
    merged = dict(config or {})
    existing = merged.get("callbacks")
    if isinstance(existing, list):
        callbacks = list(existing)
    elif existing is None:
        callbacks = []
    else:
        callbacks = [existing]
    if not any(isinstance(item, (SigilLangGraphHandler, SigilAsyncLangGraphHandler)) for item in callbacks):
        callbacks.append(create_sigil_langgraph_handler(client=client, async_handler=async_handler, **handler_kwargs))
    merged["callbacks"] = callbacks
    return merged


__all__ = [
    "SigilLangGraphHandler",
    "SigilAsyncLangGraphHandler",
    "create_sigil_langgraph_handler",
    "with_sigil_langgraph_callbacks",
]
