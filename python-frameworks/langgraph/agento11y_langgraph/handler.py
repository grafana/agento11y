"""LangGraph callback handlers for Sigil generation recording."""

from __future__ import annotations

from typing import Any
from uuid import UUID

from agento11y import Client
from agento11y.framework_handler import ProviderResolver, SigilFrameworkHandlerBase

try:
    from langchain_core.callbacks import AsyncCallbackHandler, BaseCallbackHandler
except ModuleNotFoundError:  # pragma: no cover - handled by package dependency in normal installs

    class BaseCallbackHandler:  # type: ignore[no-redef]
        """Fallback base class when langchain-core is unavailable."""

    class AsyncCallbackHandler:  # type: ignore[no-redef]
        """Fallback async base class when langchain-core is unavailable."""


_framework_name = "langgraph"
_framework_source = "handler"
_framework_language = "python"
_framework_instrumentation_name = "github.com/grafana/sigil/sdks/python-frameworks/langgraph"


def _merge_callback_kwargs(
    kwargs: dict[str, Any],
    *,
    tags: list[str] | None = None,
    metadata: dict[str, Any] | None = None,
    run_name: str | None = None,
) -> dict[str, Any]:
    callback_kwargs = dict(kwargs)
    if tags is not None:
        callback_kwargs["tags"] = tags
    if metadata is not None:
        callback_kwargs["metadata"] = metadata
    if run_name is not None and run_name.strip() != "":
        callback_kwargs["run_name"] = run_name
    return callback_kwargs


class _SigilLangGraphBase(SigilFrameworkHandlerBase):
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
        capture_workflow_steps: bool = False,
        conversation_title: str = "",
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
            capture_workflow_steps=capture_workflow_steps,
            conversation_title=conversation_title,
            extra_tags=extra_tags,
            extra_metadata=extra_metadata,
        )


class SigilLangGraphHandler(_SigilLangGraphBase, BaseCallbackHandler):
    """Sync LangGraph callback handler that records Sigil generations."""

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
            callback_kwargs=_merge_callback_kwargs(kwargs, tags=tags, metadata=metadata, run_name=run_name),
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
            callback_kwargs=_merge_callback_kwargs(kwargs, tags=tags, metadata=metadata, run_name=run_name),
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
            callback_kwargs=_merge_callback_kwargs(kwargs, tags=tags, metadata=metadata, run_name=run_name),
        )

    def on_tool_end(self, output: Any, *, run_id: UUID, **_kwargs: Any) -> None:
        self._on_tool_end(output=output, run_id=run_id)

    def on_tool_error(self, error: BaseException, *, run_id: UUID, **_kwargs: Any) -> None:
        self._on_tool_error(error=error, run_id=run_id)

    def on_chain_start(
        self,
        serialized: dict[str, Any] | None,
        inputs: dict[str, Any] | None,
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
            callback_kwargs=_merge_callback_kwargs(kwargs, tags=tags, metadata=metadata, run_name=run_name),
            inputs=inputs,
        )

    def on_chain_end(self, outputs: dict[str, Any] | None, *, run_id: UUID, **_kwargs: Any) -> None:
        self._on_chain_end(run_id=run_id, outputs=outputs)

    def on_chain_error(self, error: BaseException, *, run_id: UUID, **_kwargs: Any) -> None:
        self._on_chain_error(error=error, run_id=run_id)

    def on_retriever_start(
        self,
        serialized: dict[str, Any] | None,
        _query: str,
        *,
        run_id: UUID,
        parent_run_id: UUID | None = None,
        tags: list[str] | None = None,
        metadata: dict[str, Any] | None = None,
        run_name: str | None = None,
        **kwargs: Any,
    ) -> None:
        self._on_retriever_start(
            serialized=serialized,
            run_id=run_id,
            parent_run_id=parent_run_id,
            callback_kwargs=_merge_callback_kwargs(kwargs, tags=tags, metadata=metadata, run_name=run_name),
        )

    def on_retriever_end(self, _documents: list[Any], *, run_id: UUID, **_kwargs: Any) -> None:
        self._on_retriever_end(run_id=run_id)

    def on_retriever_error(self, error: BaseException, *, run_id: UUID, **_kwargs: Any) -> None:
        self._on_retriever_error(error=error, run_id=run_id)


class SigilAsyncLangGraphHandler(_SigilLangGraphBase, AsyncCallbackHandler):
    """Async LangGraph callback handler that records Sigil generations."""

    async def on_llm_start(
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
            callback_kwargs=_merge_callback_kwargs(kwargs, tags=tags, metadata=metadata, run_name=run_name),
        )

    async def on_chat_model_start(
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
            callback_kwargs=_merge_callback_kwargs(kwargs, tags=tags, metadata=metadata, run_name=run_name),
        )

    async def on_llm_new_token(self, token: str, *, run_id: UUID, **_kwargs: Any) -> None:
        self._on_llm_new_token(token=token, run_id=run_id)

    async def on_llm_end(self, response: Any, *, run_id: UUID, **_kwargs: Any) -> None:
        self._on_llm_end(response=response, run_id=run_id)

    async def on_llm_error(self, error: BaseException, *, run_id: UUID, **_kwargs: Any) -> None:
        self._on_llm_error(error=error, run_id=run_id)

    async def on_tool_start(
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
            callback_kwargs=_merge_callback_kwargs(kwargs, tags=tags, metadata=metadata, run_name=run_name),
        )

    async def on_tool_end(self, output: Any, *, run_id: UUID, **_kwargs: Any) -> None:
        self._on_tool_end(output=output, run_id=run_id)

    async def on_tool_error(self, error: BaseException, *, run_id: UUID, **_kwargs: Any) -> None:
        self._on_tool_error(error=error, run_id=run_id)

    async def on_chain_start(
        self,
        serialized: dict[str, Any] | None,
        inputs: dict[str, Any] | None,
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
            callback_kwargs=_merge_callback_kwargs(kwargs, tags=tags, metadata=metadata, run_name=run_name),
            inputs=inputs,
        )

    async def on_chain_end(self, outputs: dict[str, Any] | None, *, run_id: UUID, **_kwargs: Any) -> None:
        self._on_chain_end(run_id=run_id, outputs=outputs)

    async def on_chain_error(self, error: BaseException, *, run_id: UUID, **_kwargs: Any) -> None:
        self._on_chain_error(error=error, run_id=run_id)

    async def on_retriever_start(
        self,
        serialized: dict[str, Any] | None,
        _query: str,
        *,
        run_id: UUID,
        parent_run_id: UUID | None = None,
        tags: list[str] | None = None,
        metadata: dict[str, Any] | None = None,
        run_name: str | None = None,
        **kwargs: Any,
    ) -> None:
        self._on_retriever_start(
            serialized=serialized,
            run_id=run_id,
            parent_run_id=parent_run_id,
            callback_kwargs=_merge_callback_kwargs(kwargs, tags=tags, metadata=metadata, run_name=run_name),
        )

    async def on_retriever_end(self, _documents: list[Any], *, run_id: UUID, **_kwargs: Any) -> None:
        self._on_retriever_end(run_id=run_id)

    async def on_retriever_error(self, error: BaseException, *, run_id: UUID, **_kwargs: Any) -> None:
        self._on_retriever_error(error=error, run_id=run_id)
