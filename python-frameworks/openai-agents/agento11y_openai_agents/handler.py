"""OpenAI Agents callback handlers for agento11y generation recording."""

from __future__ import annotations

from typing import Any
from uuid import UUID

from agento11y import Client
from agento11y.framework_handler import Agento11yFrameworkHandlerBase, ProviderResolver, merge_framework_callback_kwargs

_framework_name = "openai-agents"
_framework_source = "handler"
_framework_language = "python"
_framework_instrumentation_name = "github.com/grafana/sigil/sdks/python-frameworks/openai-agents"


class _Agento11yOpenAIAgentsBase(Agento11yFrameworkHandlerBase):
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


class Agento11yOpenAIAgentsHandler(_Agento11yOpenAIAgentsBase):
    """Sync OpenAI Agents callback handler that records agento11y generations."""

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
            callback_kwargs=merge_framework_callback_kwargs(kwargs, tags=tags, metadata=metadata, run_name=run_name),
        )

    def on_retriever_end(self, _documents: list[Any], *, run_id: UUID, **_kwargs: Any) -> None:
        self._on_retriever_end(run_id=run_id)

    def on_retriever_error(self, error: BaseException, *, run_id: UUID, **_kwargs: Any) -> None:
        self._on_retriever_error(error=error, run_id=run_id)


class Agento11yAsyncOpenAIAgentsHandler(_Agento11yOpenAIAgentsBase):
    """Async OpenAI Agents callback handler that records agento11y generations."""

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
            callback_kwargs=merge_framework_callback_kwargs(kwargs, tags=tags, metadata=metadata, run_name=run_name),
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
            callback_kwargs=merge_framework_callback_kwargs(kwargs, tags=tags, metadata=metadata, run_name=run_name),
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
            callback_kwargs=merge_framework_callback_kwargs(kwargs, tags=tags, metadata=metadata, run_name=run_name),
        )

    async def on_tool_end(self, output: Any, *, run_id: UUID, **_kwargs: Any) -> None:
        self._on_tool_end(output=output, run_id=run_id)

    async def on_tool_error(self, error: BaseException, *, run_id: UUID, **_kwargs: Any) -> None:
        self._on_tool_error(error=error, run_id=run_id)

    async def on_chain_start(
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

    async def on_chain_end(self, _outputs: dict[str, Any] | None, *, run_id: UUID, **_kwargs: Any) -> None:
        self._on_chain_end(run_id=run_id)

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
            callback_kwargs=merge_framework_callback_kwargs(kwargs, tags=tags, metadata=metadata, run_name=run_name),
        )

    async def on_retriever_end(self, _documents: list[Any], *, run_id: UUID, **_kwargs: Any) -> None:
        self._on_retriever_end(run_id=run_id)

    async def on_retriever_error(self, error: BaseException, *, run_id: UUID, **_kwargs: Any) -> None:
        self._on_retriever_error(error=error, run_id=run_id)
