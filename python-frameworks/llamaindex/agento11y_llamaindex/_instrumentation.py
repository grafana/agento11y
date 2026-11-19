"""Bridge LlamaIndex instrumentation events and spans to agento11y callback handlers.

The legacy callback manager reports one start and one end per LLM call. It does
not say whether the call streams, it hides every chunk but the last, and
workflow agents such as ``FunctionAgent`` never emit ``FUNCTION_CALL`` events
for their tool calls. The instrumentation dispatcher covers all three:

* ``LLMChatInProgressEvent`` carries each streamed chunk, keyed by the span id
  of the LLM call that the callback handler saw start.
* The ``<Agent>.call_tool`` workflow step span carries the ``ToolCall`` event
  (name, arguments, tool call id) on entry and the ``ToolCallResult`` on exit.

One event handler and one span handler are registered on the root dispatcher,
once per process. They forward to every live callback handler; each handler
only acts on the LLM calls it started and the tool calls those LLM calls made.
"""

from __future__ import annotations

import threading
import weakref
from typing import Any

_bridges: weakref.WeakSet[Any] = weakref.WeakSet()
_registered = False
_lock = threading.Lock()


def register_bridge(bridge: Any) -> None:
    """Route instrumentation events to ``bridge``; installs the root handlers on first use."""
    global _registered
    with _lock:
        _bridges.add(bridge)
        if _registered:
            return
        try:
            from llama_index.core.instrumentation import get_dispatcher
        except Exception:  # pragma: no cover - llama-index missing
            return
        dispatcher = get_dispatcher()
        dispatcher.add_event_handler(_build_event_handler())
        dispatcher.add_span_handler(_build_span_handler())
        _registered = True


def current_span_id() -> str:
    """Return the active LlamaIndex instrumentation span id, or ``""``."""
    try:
        from llama_index.core.instrumentation.span import active_span_id
    except Exception:  # pragma: no cover - llama-index missing
        return ""
    value = active_span_id.get()
    return value if isinstance(value, str) else ""


def _live_bridges() -> list[Any]:
    with _lock:
        return list(_bridges)


def _build_event_handler() -> Any:
    from llama_index.core.instrumentation.event_handlers import BaseEventHandler

    class _Agento11yEventHandler(BaseEventHandler):
        @classmethod
        def class_name(cls) -> str:
            return "Agento11yEventHandler"

        def handle(self, event: Any, **_kwargs: Any) -> None:
            if event.class_name() != "LLMChatInProgressEvent":
                return
            for bridge in _live_bridges():
                bridge._on_llm_stream_chunk(event.span_id or "", event.response)

    return _Agento11yEventHandler()


def _is_tool_step_span(id_: str) -> bool:
    return id_.split("-", 1)[0].endswith(".call_tool")


def _build_span_handler() -> Any:
    from llama_index.core.instrumentation.span_handlers import BaseSpanHandler

    class _Agento11ySpanHandler(BaseSpanHandler[Any]):
        @classmethod
        def class_name(cls) -> str:
            return "Agento11ySpanHandler"

        def new_span(
            self,
            id_: str,
            bound_args: Any,
            instance: Any | None = None,
            parent_span_id: str | None = None,
            tags: dict[str, Any] | None = None,
            **_kwargs: Any,
        ) -> None:
            if not _is_tool_step_span(id_):
                return None
            tool_call = bound_args.arguments.get("ev")
            for bridge in _live_bridges():
                bridge._on_tool_step_start(id_, tool_call)
            return None

        def prepare_to_exit_span(
            self,
            id_: str,
            bound_args: Any,
            instance: Any | None = None,
            result: Any | None = None,
            **_kwargs: Any,
        ) -> None:
            if not _is_tool_step_span(id_):
                return None
            for bridge in _live_bridges():
                bridge._on_tool_step_end(id_, result=result)
            return None

        def prepare_to_drop_span(
            self,
            id_: str,
            bound_args: Any,
            instance: Any | None = None,
            err: BaseException | None = None,
            **_kwargs: Any,
        ) -> None:
            if not _is_tool_step_span(id_):
                return None
            for bridge in _live_bridges():
                bridge._on_tool_step_end(id_, error=err)
            return None

    return _Agento11ySpanHandler()
