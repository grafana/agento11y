"""Public exports for Sigil LangGraph callback handlers."""

from typing import Any

from agento11y import Client

from .handler import Agento11yAsyncLangGraphHandler, Agento11yLangGraphHandler


def create_agento11y_langgraph_handler(
    *,
    client: Client,
    async_handler: bool = False,
    **handler_kwargs: Any,
) -> Agento11yLangGraphHandler | Agento11yAsyncLangGraphHandler:
    """Create a LangGraph Sigil callback handler for sync or async flows."""
    if async_handler:
        return Agento11yAsyncLangGraphHandler(client=client, **handler_kwargs)
    return Agento11yLangGraphHandler(client=client, **handler_kwargs)


def with_agento11y_langgraph_callbacks(
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
    if not any(isinstance(item, (Agento11yLangGraphHandler, Agento11yAsyncLangGraphHandler)) for item in callbacks):
        callbacks.append(
            create_agento11y_langgraph_handler(client=client, async_handler=async_handler, **handler_kwargs)
        )
    merged["callbacks"] = callbacks
    return merged


__all__ = [
    "Agento11yLangGraphHandler",
    "Agento11yAsyncLangGraphHandler",
    "create_agento11y_langgraph_handler",
    "with_agento11y_langgraph_callbacks",
]
