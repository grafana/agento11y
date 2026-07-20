"""Sigil framework handler for the Pydantic AI integration."""

from __future__ import annotations

from typing import Any
from uuid import UUID

from agento11y import Client
from agento11y.framework_handler import Agento11yFrameworkHandlerBase, ProviderResolver, merge_framework_callback_kwargs
from opentelemetry.trace import Status, StatusCode

try:
    from agento11y.context import _pop_capture_mode
except ImportError:

    def _pop_capture_mode(recorder_id: int) -> None:
        return


_framework_name = "pydantic-ai"
_framework_source = "handler"
_framework_language = "python"
_framework_instrumentation_name = "github.com/grafana/sigil/sdks/python-frameworks/pydantic-ai"


def _discard_recorder(recorder: Any, *, pop_capture_mode: bool = False) -> None:
    lock = getattr(recorder, "_lock", None)
    if lock is not None:
        with lock:
            if getattr(recorder, "_ended", False):
                return
            recorder._ended = True
    elif getattr(recorder, "_ended", False):
        return
    else:
        recorder._ended = True

    try:
        span = getattr(recorder, "span", None)
        if span is not None:
            span.set_status(Status(StatusCode.OK))
            span.end()
    finally:
        if pop_capture_mode:
            _pop_capture_mode(id(recorder))


class Agento11yPydanticAIHandler(Agento11yFrameworkHandlerBase):
    """Sigil framework handler for the Pydantic AI integration."""

    def __init__(
        self,
        *,
        client: Client,
        agent_name: str = "",
        agent_version: str = "",
        provider_resolver: ProviderResolver = "auto",
        provider: str = "",
        capture_inputs: bool = True,
        capture_outputs: bool = True,
        extra_tags: dict[str, str] | None = None,
        extra_metadata: dict[str, Any] | None = None,
    ) -> None:
        super().__init__(
            client=client,
            framework_name=_framework_name,
            framework_source=_framework_source,
            framework_language=_framework_language,
            framework_instrumentation_name=_framework_instrumentation_name,
            agent_name=agent_name,
            agent_version=agent_version,
            provider_resolver=provider_resolver,
            provider=provider,
            capture_inputs=capture_inputs,
            capture_outputs=capture_outputs,
            extra_tags=extra_tags,
            extra_metadata=extra_metadata,
        )

    def on_llm_start(
        self,
        serialized: dict[str, Any] | None,
        prompts: list[str],
        *,
        run_id: UUID,
        parent_run_id: UUID | None = None,
        tags: list[str] | None = None,
        metadata: dict[str, Any] | None = None,
        invocation_params: dict[str, Any] | None = None,
        run_name: str | None = None,
        **kwargs: Any,
    ) -> None:
        self._on_llm_start(
            serialized=serialized,
            prompts=prompts,
            run_id=run_id,
            parent_run_id=parent_run_id,
            invocation_params=invocation_params,
            callback_kwargs=merge_framework_callback_kwargs(kwargs, tags=tags, metadata=metadata, run_name=run_name),
        )

    def on_chat_model_start(
        self,
        serialized: dict[str, Any] | None,
        messages: list[list[Any]],
        *,
        run_id: UUID,
        parent_run_id: UUID | None = None,
        tags: list[str] | None = None,
        metadata: dict[str, Any] | None = None,
        invocation_params: dict[str, Any] | None = None,
        run_name: str | None = None,
        **kwargs: Any,
    ) -> None:
        self._on_chat_model_start(
            serialized=serialized,
            messages=messages,
            run_id=run_id,
            parent_run_id=parent_run_id,
            invocation_params=invocation_params,
            callback_kwargs=merge_framework_callback_kwargs(kwargs, tags=tags, metadata=metadata, run_name=run_name),
        )

    def on_llm_new_token(self, token: str, *, run_id: UUID, **_kwargs: Any) -> None:
        self._on_llm_new_token(token=token, run_id=run_id)

    def on_llm_end(self, response: Any, *, run_id: UUID, **_kwargs: Any) -> None:
        self._on_llm_end(response=response, run_id=run_id)

    def on_llm_error(self, error: BaseException, *, run_id: UUID, **_kwargs: Any) -> None:
        self._on_llm_error(error=error, run_id=run_id)

    def on_tool_start(
        self,
        serialized: dict[str, Any] | None,
        input_str: str,
        *,
        run_id: UUID,
        parent_run_id: UUID | None = None,
        tags: list[str] | None = None,
        metadata: dict[str, Any] | None = None,
        run_name: str | None = None,
        **kwargs: Any,
    ) -> None:
        self._on_tool_start(
            serialized=serialized,
            input_str=input_str,
            run_id=run_id,
            parent_run_id=parent_run_id,
            callback_kwargs=merge_framework_callback_kwargs(kwargs, tags=tags, metadata=metadata, run_name=run_name),
        )

    def on_tool_end(self, output: Any, *, run_id: UUID, **_kwargs: Any) -> None:
        self._on_tool_end(output=output, run_id=run_id)

    def on_tool_error(self, error: BaseException, *, run_id: UUID, **_kwargs: Any) -> None:
        self._on_tool_error(error=error, run_id=run_id)

    def on_chain_start(
        self,
        serialized: dict[str, Any] | None,
        _inputs: dict[str, Any] | None,
        *,
        run_id: UUID,
        parent_run_id: UUID | None = None,
        tags: list[str] | None = None,
        metadata: dict[str, Any] | None = None,
        run_type: str | None = None,
        run_name: str | None = None,
        **kwargs: Any,
    ) -> None:
        self._on_chain_start(
            serialized=serialized,
            run_id=run_id,
            parent_run_id=parent_run_id,
            run_type=run_type or "chain",
            callback_kwargs=merge_framework_callback_kwargs(kwargs, tags=tags, metadata=metadata, run_name=run_name),
        )

    def on_chain_end(self, _outputs: dict[str, Any] | None, *, run_id: UUID, **_kwargs: Any) -> None:
        self._on_chain_end(run_id=run_id)

    def on_chain_error(self, error: BaseException, *, run_id: UUID, **_kwargs: Any) -> None:
        self._on_chain_error(error=error, run_id=run_id)

    def _discard_llm_run(self, *, run_id: UUID) -> None:
        run_state = self._runs.pop(str(run_id), None)
        if run_state is None:
            return
        _discard_recorder(run_state.recorder, pop_capture_mode=True)

    def _discard_tool_run(self, *, run_id: UUID) -> None:
        run_state = self._tool_runs.pop(str(run_id), None)
        if run_state is None:
            return
        _discard_recorder(run_state.recorder)

    def _discard_chain_run(self, *, run_id: UUID) -> None:
        self._end_framework_span(self._chain_spans, run_id=run_id, error=None)
