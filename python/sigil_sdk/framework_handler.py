"""Shared callback handler runtime for Python framework integrations."""

from __future__ import annotations

import json
import logging
import math
import secrets
from collections.abc import Callable
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any
from uuid import UUID

from opentelemetry import trace as otel_trace
from opentelemetry.trace import Span, SpanKind, Status, StatusCode

from sigil_sdk import (
    Client,
    Generation,
    GenerationMode,
    GenerationStart,
    Message,
    MessageRole,
    ModelRef,
    Part,
    PartKind,
    ToolCall,
    ToolDefinition,
    ToolExecutionStart,
    WorkflowStep,
    user_text_message,
)
from sigil_sdk.usage import map_usage

logger = logging.getLogger("sigil_sdk")

ProviderResolver = str | Callable[[str, dict[str, Any] | None, dict[str, Any] | None], str]


def _extract_tool_output(output: Any) -> Any:
    """Unwrap BaseMessage subclasses (e.g. ToolMessage) to their ``.content``.

    Callback-based frameworks such as LangChain and LangGraph may deliver a
    ``BaseMessage`` object to ``on_tool_end``.  These are not JSON-serialisable,
    so we extract the ``.content`` attribute when present.  Plain strings,
    ``None``, and other already-serialisable values pass through unchanged.
    """
    if output is None:
        return output
    if isinstance(output, str):
        return output
    if hasattr(output, "content"):
        return output.content
    return output


_default_framework_source = "handler"
_default_framework_language = "python"
_default_framework_instrumentation_name = "github.com/grafana/sigil/sdks/python/frameworks"
_span_attr_operation_name = "gen_ai.operation.name"
_span_attr_conversation_id = "gen_ai.conversation.id"
_span_attr_generation_id = "sigil.generation.id"
_span_attr_parent_generation_ids = "sigil.generation.parent_generation_ids"
_span_attr_framework_name = "sigil.framework.name"
_span_attr_framework_source = "sigil.framework.source"
_span_attr_framework_language = "sigil.framework.language"
_span_attr_framework_run_id = "sigil.framework.run_id"
_span_attr_framework_thread_id = "sigil.framework.thread_id"
_span_attr_framework_parent_run_id = "sigil.framework.parent_run_id"
_span_attr_framework_component_name = "sigil.framework.component_name"
_span_attr_framework_run_type = "sigil.framework.run_type"
_span_attr_framework_tags = "sigil.framework.tags"
_span_attr_framework_retry_attempt = "sigil.framework.retry_attempt"
_span_attr_framework_langgraph_node = "sigil.framework.langgraph.node"
_span_attr_framework_event_id = "sigil.framework.event_id"
_span_attr_error_type = "error.type"
_span_attr_error_category = "error.category"
_max_framework_metadata_depth = 5
_metadata_drop = object()


@dataclass(slots=True)
class _RunState:
    recorder: Any
    input_messages: list[Message]
    capture_outputs: bool
    output_chunks: list[str] = field(default_factory=list)
    first_token_recorded: bool = False


@dataclass(slots=True)
class _ToolRunState:
    recorder: Any
    arguments: Any
    capture_outputs: bool


@dataclass(slots=True)
class _WorkflowStepState:
    step_id: str
    conversation_id: str
    step_name: str
    started_at: datetime
    tags: dict[str, str]
    metadata: dict[str, Any]
    parent_step_ids: list[str]
    graph_root_key: str
    capture_outputs: bool
    input_state: dict[str, Any] = field(default_factory=dict)
    linked_generation_ids: list[str] = field(default_factory=list)


def merge_framework_callback_kwargs(
    callback_kwargs: dict[str, Any],
    *,
    tags: list[str] | None = None,
    metadata: dict[str, Any] | None = None,
    run_name: str | None = None,
) -> dict[str, Any]:
    merged = dict(callback_kwargs)
    if tags is not None:
        merged["tags"] = tags
    if metadata is not None:
        merged["metadata"] = metadata
    if run_name is not None and run_name.strip() != "":
        merged["run_name"] = run_name
    return merged


class SigilFrameworkHandlerBase:
    def __init__(
        self,
        *,
        client: Client,
        framework_name: str,
        agent_name: str = "",
        agent_version: str = "",
        framework_source: str = _default_framework_source,
        framework_language: str = _default_framework_language,
        framework_instrumentation_name: str = _default_framework_instrumentation_name,
        provider_resolver: ProviderResolver = "auto",
        provider: str = "",
        capture_inputs: bool = True,
        capture_outputs: bool = True,
        capture_workflow_steps: bool = False,
        conversation_title: str = "",
        extra_tags: dict[str, str] | None = None,
        extra_metadata: dict[str, Any] | None = None,
    ) -> None:
        self._client = client
        self._agent_name = agent_name
        self._agent_version = agent_version
        self._conversation_title = conversation_title.strip()
        self._framework_name = framework_name.strip()
        self._framework_source = framework_source.strip() or _default_framework_source
        self._framework_language = framework_language.strip() or _default_framework_language
        self._framework_instrumentation_name = (
            framework_instrumentation_name.strip() or _default_framework_instrumentation_name
        )
        self._provider_resolver = provider_resolver
        self._provider = provider
        self._capture_inputs = capture_inputs
        self._capture_outputs = capture_outputs
        self._capture_workflow_steps = capture_workflow_steps
        self._extra_tags = dict(extra_tags or {})
        self._extra_metadata = dict(extra_metadata or {})
        self._runs: dict[str, _RunState] = {}
        self._tool_runs: dict[str, _ToolRunState] = {}
        self._chain_spans: dict[str, Span] = {}
        self._retriever_spans: dict[str, Span] = {}
        self._workflow_step_runs: dict[str, _WorkflowStepState] = {}

        # Graph-level state for workflow step wiring. Keyed by the run_key of
        # each graph root so multiple concurrent graph invocations on the same
        # handler stay isolated.
        self._graph_root_run_keys: set[str] = set()
        self._graph_run_conversation_id: dict[str, str] = {}
        self._graph_run_last_step_id: dict[str, str] = {}
        # parent_run_id chain for any active run (chain/llm/chat). Used to walk
        # upwards from an LLM run to find the enclosing workflow step.
        self._run_to_graph_key: dict[str, str] = {}

    def _find_graph_root_key(self, run_key: str) -> str:
        """Walk the parent chain starting at run_key until we reach a known
        graph root run_key. Returns the root key, or "" if none is found.

        Uses a visited set as a defensive guard against pathological cycles
        in the parent map (which shouldn't happen but we don't want to spin)."""
        visited: set[str] = set()
        current = run_key
        while current and current not in visited:
            if current in self._graph_root_run_keys:
                return current
            visited.add(current)
            current = self._run_to_graph_key.get(current, "")
        return ""

    def _find_enclosing_workflow_step(self, run_key: str) -> _WorkflowStepState | None:
        """Walk the parent chain to find the closest ancestor whose run_key is
        registered as a workflow step. Returns None if none is found.

        This is what makes ``linked_generation_ids`` work for LLM runs nested
        inside sub-chains (LCEL composition, conditional edges, etc.) — the
        immediate parent is often a sub-chain, but the workflow step is one
        or two levels above."""
        visited: set[str] = set()
        current = self._run_to_graph_key.get(run_key, "")
        while current and current not in visited:
            visited.add(current)
            state = self._workflow_step_runs.get(current)
            if state is not None:
                return state
            current = self._run_to_graph_key.get(current, "")
        return None

    def _resolve_graph_conversation_id(self, conversation_id: str, parent_run_id: UUID | None) -> str:
        """When capture_workflow_steps is on, ensure all nodes under the same
        graph invocation share a single conversation_id."""
        if not self._capture_workflow_steps or parent_run_id is None:
            return conversation_id
        root = self._find_graph_root_key(str(parent_run_id))
        if root == "":
            return conversation_id
        cached = self._graph_run_conversation_id.get(root)
        if cached:
            return cached
        self._graph_run_conversation_id[root] = conversation_id
        return conversation_id

    def _track_run_generation_id(self, run_key: str, generation_id: str) -> None:
        """Register a generation id against the enclosing workflow step (if any)
        so it ends up in ``linked_generation_ids``."""
        if generation_id == "":
            return
        state = self._find_enclosing_workflow_step(run_key)
        if state is not None:
            state.linked_generation_ids.append(generation_id)

    def _is_graph_node(self, parent_run_id: UUID | None) -> bool:
        """Return True if parent_run_id is one of the active graph roots
        (i.e. this chain is a direct child of a compiled graph, not a
        nested sub-chain)."""
        if parent_run_id is None:
            return False
        return str(parent_run_id) in self._graph_root_run_keys

    def _cleanup_graph_state_for_root(self, root_run_key: str) -> None:
        """Remove all per-graph state once a graph root chain ends, including
        any leftover ``_run_to_graph_key`` entries whose parent chain reached
        this root, so the maps don't leak across long-lived handlers."""
        if not root_run_key:
            return
        descendants = [
            run_key for run_key in list(self._run_to_graph_key.keys()) if self._reaches_run(run_key, root_run_key)
        ]
        for run_key in descendants:
            self._run_to_graph_key.pop(run_key, None)
            orphan = self._workflow_step_runs.pop(run_key, None)
            if orphan is not None:
                logger.warning(
                    "sigil: dropping in-flight workflow step %s (node %s) — on_chain_end callback never fired",
                    orphan.step_id,
                    orphan.step_name,
                )
        self._graph_root_run_keys.discard(root_run_key)
        self._graph_run_conversation_id.pop(root_run_key, None)
        self._graph_run_last_step_id.pop(root_run_key, None)

    def _reaches_run(self, start_key: str, target_key: str) -> bool:
        """Return True if walking ``_run_to_graph_key`` from start_key
        eventually reaches target_key."""
        visited: set[str] = set()
        current = self._run_to_graph_key.get(start_key, "")
        while current and current not in visited:
            if current == target_key:
                return True
            visited.add(current)
            current = self._run_to_graph_key.get(current, "")
        return False

    def _on_llm_start(
        self,
        *,
        serialized: dict[str, Any] | None,
        prompts: list[str],
        run_id: UUID,
        parent_run_id: UUID | None,
        invocation_params: dict[str, Any] | None,
        callback_kwargs: dict[str, Any] | None = None,
    ) -> None:
        run_key = str(run_id)
        if run_key in self._runs:
            return

        model_name = _resolve_model_name(serialized, invocation_params)
        provider_name = _resolve_provider(
            explicit_provider=self._provider,
            provider_resolver=self._provider_resolver,
            model_name=model_name,
            serialized=serialized,
            invocation_params=invocation_params,
        )

        mode = GenerationMode.STREAM if _is_streaming(invocation_params) else GenerationMode.SYNC
        input_messages = (
            [user_text_message(prompt) for prompt in prompts if prompt.strip() != ""] if self._capture_inputs else []
        )

        tags = dict(self._extra_tags)
        tags["sigil.framework.name"] = self._framework_name
        tags["sigil.framework.source"] = self._framework_source
        tags["sigil.framework.language"] = self._framework_language

        conversation_id, thread_id = _resolve_framework_conversation_id(
            framework_name=self._framework_name,
            run_key=run_key,
            serialized=serialized,
            invocation_params=invocation_params,
            callback_kwargs=callback_kwargs,
        )
        conversation_id = self._resolve_graph_conversation_id(conversation_id, parent_run_id)
        parent_run_key = _normalize_run_key(parent_run_id)
        component_name = _resolve_component_name(serialized, callback_kwargs)
        callback_tags = _normalize_framework_tags(_read(callback_kwargs, "tags"))
        retry_attempt = _resolve_framework_retry_attempt(callback_kwargs, invocation_params, serialized)
        langgraph_node = _resolve_langgraph_node(callback_kwargs, invocation_params, serialized)
        event_id = _resolve_framework_event_id(callback_kwargs, invocation_params, serialized)
        generation_id = _resolve_generation_id(callback_kwargs, invocation_params, serialized)
        parent_generation_ids = _resolve_parent_generation_ids(callback_kwargs, invocation_params, serialized)

        metadata: dict[str, Any] = dict(self._extra_metadata)
        metadata[_span_attr_framework_run_id] = run_key
        metadata[_span_attr_framework_run_type] = "llm"
        if thread_id != "":
            metadata[_span_attr_framework_thread_id] = thread_id
        if parent_run_key != "":
            metadata[_span_attr_framework_parent_run_id] = parent_run_key
        if component_name != "":
            metadata[_span_attr_framework_component_name] = component_name
        if callback_tags:
            metadata[_span_attr_framework_tags] = callback_tags
        if retry_attempt is not None:
            metadata[_span_attr_framework_retry_attempt] = retry_attempt
        if langgraph_node != "":
            metadata[_span_attr_framework_langgraph_node] = langgraph_node
        if event_id != "":
            metadata[_span_attr_framework_event_id] = event_id
        metadata = _normalize_framework_metadata(metadata)

        if parent_run_id is not None:
            self._run_to_graph_key[run_key] = str(parent_run_id)

        start = GenerationStart(
            id=generation_id,
            conversation_id=conversation_id,
            conversation_title=self._conversation_title,
            agent_name=self._agent_name,
            agent_version=self._agent_version,
            mode=mode,
            model=ModelRef(provider=provider_name, name=model_name),
            tags=tags,
            metadata=metadata,
            parent_generation_ids=parent_generation_ids,
        )
        if start.id == "":
            start.id = f"gen_{secrets.token_hex(8)}"
        self._track_run_generation_id(run_key, start.id)

        recorder = (
            self._client.start_streaming_generation(start)
            if mode == GenerationMode.STREAM
            else self._client.start_generation(start)
        )
        self._runs[run_key] = _RunState(
            recorder=recorder,
            input_messages=input_messages,
            capture_outputs=self._capture_outputs,
        )

    def _on_chat_model_start(
        self,
        *,
        serialized: dict[str, Any] | None,
        messages: list[list[Any]],
        run_id: UUID,
        parent_run_id: UUID | None,
        invocation_params: dict[str, Any] | None,
        callback_kwargs: dict[str, Any] | None = None,
    ) -> None:
        run_key = str(run_id)
        if run_key in self._runs:
            return

        model_name = _resolve_model_name(serialized, invocation_params)
        provider_name = _resolve_provider(
            explicit_provider=self._provider,
            provider_resolver=self._provider_resolver,
            model_name=model_name,
            serialized=serialized,
            invocation_params=invocation_params,
        )

        mode = GenerationMode.STREAM if _is_streaming(invocation_params) else GenerationMode.SYNC
        input_messages = _map_chat_inputs(messages) if self._capture_inputs else []

        tags = dict(self._extra_tags)
        tags["sigil.framework.name"] = self._framework_name
        tags["sigil.framework.source"] = self._framework_source
        tags["sigil.framework.language"] = self._framework_language

        conversation_id, thread_id = _resolve_framework_conversation_id(
            framework_name=self._framework_name,
            run_key=run_key,
            serialized=serialized,
            invocation_params=invocation_params,
            callback_kwargs=callback_kwargs,
        )
        conversation_id = self._resolve_graph_conversation_id(conversation_id, parent_run_id)
        parent_run_key = _normalize_run_key(parent_run_id)
        component_name = _resolve_component_name(serialized, callback_kwargs)
        callback_tags = _normalize_framework_tags(_read(callback_kwargs, "tags"))
        retry_attempt = _resolve_framework_retry_attempt(callback_kwargs, invocation_params, serialized)
        langgraph_node = _resolve_langgraph_node(callback_kwargs, invocation_params, serialized)
        event_id = _resolve_framework_event_id(callback_kwargs, invocation_params, serialized)
        generation_id = _resolve_generation_id(callback_kwargs, invocation_params, serialized)
        parent_generation_ids = _resolve_parent_generation_ids(callback_kwargs, invocation_params, serialized)

        metadata: dict[str, Any] = dict(self._extra_metadata)
        metadata[_span_attr_framework_run_id] = run_key
        metadata[_span_attr_framework_run_type] = "chat"
        if thread_id != "":
            metadata[_span_attr_framework_thread_id] = thread_id
        if parent_run_key != "":
            metadata[_span_attr_framework_parent_run_id] = parent_run_key
        if component_name != "":
            metadata[_span_attr_framework_component_name] = component_name
        if callback_tags:
            metadata[_span_attr_framework_tags] = callback_tags
        if retry_attempt is not None:
            metadata[_span_attr_framework_retry_attempt] = retry_attempt
        if langgraph_node != "":
            metadata[_span_attr_framework_langgraph_node] = langgraph_node
        if event_id != "":
            metadata[_span_attr_framework_event_id] = event_id
        metadata = _normalize_framework_metadata(metadata)

        if parent_run_id is not None:
            self._run_to_graph_key[run_key] = str(parent_run_id)

        start = GenerationStart(
            id=generation_id,
            conversation_id=conversation_id,
            conversation_title=self._conversation_title,
            agent_name=self._agent_name,
            agent_version=self._agent_version,
            mode=mode,
            model=ModelRef(provider=provider_name, name=model_name),
            tags=tags,
            metadata=metadata,
            system_prompt=_as_str(_read(invocation_params, "system_prompt")),
            tools=_resolve_tool_definitions(invocation_params),
            temperature=_as_optional_float(_read(invocation_params, "temperature")),
            max_tokens=_as_optional_int(_read(invocation_params, "max_tokens")),
            top_p=_as_optional_float(_read(invocation_params, "top_p")),
            tool_choice=_as_str(_read(invocation_params, "tool_choice")) or None,
            parent_generation_ids=parent_generation_ids,
        )
        if start.id == "":
            start.id = f"gen_{secrets.token_hex(8)}"
        self._track_run_generation_id(run_key, start.id)

        recorder = (
            self._client.start_streaming_generation(start)
            if mode == GenerationMode.STREAM
            else self._client.start_generation(start)
        )
        self._runs[run_key] = _RunState(
            recorder=recorder,
            input_messages=input_messages,
            capture_outputs=self._capture_outputs,
        )

    def _on_llm_new_token(self, *, token: str, run_id: UUID) -> None:
        run_state = self._runs.get(str(run_id))
        if run_state is None:
            return

        if token.strip() == "":
            return

        if run_state.capture_outputs:
            run_state.output_chunks.append(token)

        if not run_state.first_token_recorded:
            run_state.first_token_recorded = True
            run_state.recorder.set_first_token_at(datetime.now(timezone.utc))

    def _on_llm_end(self, *, response: Any, run_id: UUID) -> None:
        run_key = str(run_id)
        run_state = self._runs.pop(run_key, None)
        self._run_to_graph_key.pop(run_key, None)
        if run_state is None:
            return

        try:
            llm_output = _read(response, "llm_output")
            response_message = _first_generation_message(response)
            response_metadata = _read(response_message, "response_metadata")
            raw_usage = (
                _read(llm_output, "token_usage")
                or _read(llm_output, "usage")
                or _read(response_message, "usage_metadata")
            )
            usage = _map_framework_usage(raw_usage)
            response_model = _as_str(_read(llm_output, "model_name")) or _as_str(_read(response_metadata, "model_name"))
            stop_reason = _as_str(
                _read(llm_output, "finish_reason")
                or _read(llm_output, "stop_reason")
                or _read(response_metadata, "finish_reason")
                or _read(response_metadata, "stop_reason")
            )

            output_messages: list[Message] = []
            if run_state.capture_outputs:
                output_messages = _map_output_messages(response)
                if not output_messages and run_state.output_chunks:
                    output_messages = [
                        Message(
                            role=MessageRole.ASSISTANT,
                            parts=[Part(kind=PartKind.TEXT, text="".join(run_state.output_chunks))],
                        )
                    ]

            run_state.recorder.set_result(
                Generation(
                    input=run_state.input_messages,
                    output=output_messages,
                    usage=usage,
                    response_model=response_model,
                    stop_reason=stop_reason,
                )
            )
        finally:
            run_state.recorder.end()

        recorder_error = run_state.recorder.err()
        if recorder_error is not None:
            raise recorder_error

    def _on_llm_error(self, *, error: BaseException, run_id: UUID) -> None:
        run_key = str(run_id)
        run_state = self._runs.pop(run_key, None)
        self._run_to_graph_key.pop(run_key, None)
        if run_state is None:
            return

        try:
            run_state.recorder.set_call_error(Exception(str(error)))
            if run_state.capture_outputs and run_state.output_chunks:
                run_state.recorder.set_result(
                    Generation(
                        input=run_state.input_messages,
                        output=[
                            Message(
                                role=MessageRole.ASSISTANT,
                                parts=[Part(kind=PartKind.TEXT, text="".join(run_state.output_chunks))],
                            )
                        ],
                    )
                )
        finally:
            run_state.recorder.end()

        recorder_error = run_state.recorder.err()
        if recorder_error is not None:
            raise recorder_error

    def _on_tool_start(
        self,
        *,
        serialized: dict[str, Any] | None,
        input_str: str,
        run_id: UUID,
        parent_run_id: UUID | None,
        callback_kwargs: dict[str, Any] | None = None,
    ) -> None:
        run_key = str(run_id)
        if run_key in self._tool_runs:
            return

        conversation_id, thread_id = _resolve_framework_conversation_id(
            framework_name=self._framework_name,
            run_key=run_key,
            serialized=serialized,
            invocation_params=None,
            callback_kwargs=callback_kwargs,
        )
        tool_name = _resolve_tool_name(serialized, callback_kwargs)
        include_content = self._capture_inputs or self._capture_outputs

        recorder = self._client.start_tool_execution(
            ToolExecutionStart(
                tool_name=tool_name,
                tool_description=_resolve_tool_description(serialized),
                conversation_id=conversation_id,
                agent_name=self._agent_name,
                agent_version=self._agent_version,
                include_content=include_content,
            )
        )
        arguments: Any = None
        if self._capture_inputs:
            arguments = _resolve_tool_arguments(input_str, callback_kwargs)
        self._tool_runs[run_key] = _ToolRunState(
            recorder=recorder,
            arguments=arguments,
            capture_outputs=self._capture_outputs,
        )

    def _on_tool_end(self, *, output: Any, run_id: UUID) -> None:
        run_state = self._tool_runs.pop(str(run_id), None)
        if run_state is None:
            return

        try:
            payload: dict[str, Any] = {}
            if run_state.arguments is not None:
                payload["arguments"] = run_state.arguments
            if run_state.capture_outputs:
                payload["result"] = _extract_tool_output(output)
            run_state.recorder.set_result(**payload)
        finally:
            run_state.recorder.end()

        recorder_error = run_state.recorder.err()
        if recorder_error is not None:
            raise recorder_error

    def _on_tool_error(self, *, error: BaseException, run_id: UUID) -> None:
        run_state = self._tool_runs.pop(str(run_id), None)
        if run_state is None:
            return

        try:
            run_state.recorder.set_exec_error(Exception(str(error)))
        finally:
            run_state.recorder.end()

        recorder_error = run_state.recorder.err()
        if recorder_error is not None:
            raise recorder_error

    def _on_chain_start(
        self,
        *,
        serialized: dict[str, Any] | None,
        run_id: UUID,
        parent_run_id: UUID | None,
        run_type: str = "chain",
        callback_kwargs: dict[str, Any] | None = None,
        inputs: dict[str, Any] | None = None,
    ) -> None:
        run_key = str(run_id)

        # Register parent mapping for transitive graph resolution.
        if parent_run_id is not None:
            self._run_to_graph_key[run_key] = str(parent_run_id)

        if self._capture_workflow_steps:
            # Top-level graph wrapper (parent=None) becomes a graph root.
            # Tracking a set instead of a single value keeps concurrent graph
            # invocations on the same handler isolated.
            if parent_run_id is None:
                # Nested graph invocation (e.g. sub-agent invoked inside a tool):
                # inherit the conversation_id from the already-active graph root
                # so all sub-graphs appear in the same Sigil conversation.
                if self._graph_root_run_keys and self._graph_run_conversation_id:
                    existing_root = next(
                        (r for r in self._graph_root_run_keys if r in self._graph_run_conversation_id),
                        None,
                    )
                    if existing_root is not None:
                        self._graph_run_conversation_id[run_key] = self._graph_run_conversation_id[existing_root]
                self._graph_root_run_keys.add(run_key)
                return
            # Only promote direct children of a graph root to workflow steps.
            # Sub-chains inside a node (e.g. conditional edge routers) are skipped.
            if not self._is_graph_node(parent_run_id):
                return
            if run_key in self._workflow_step_runs:
                return
            self._start_workflow_step(
                serialized=serialized,
                run_key=run_key,
                parent_run_id=parent_run_id,
                run_type=run_type,
                callback_kwargs=callback_kwargs,
                inputs=inputs,
            )
            return

        if run_key in self._chain_spans:
            return
        span = self._start_framework_span(
            run_key=run_key,
            parent_run_id=parent_run_id,
            serialized=serialized,
            invocation_params=None,
            callback_kwargs=callback_kwargs,
            run_type=run_type,
            operation_name="framework_chain",
            fallback_component="chain",
        )
        self._chain_spans[run_key] = span

    def _on_chain_end(self, *, run_id: UUID, outputs: dict[str, Any] | None = None) -> None:
        run_key = str(run_id)
        if run_key in self._graph_root_run_keys:
            self._cleanup_graph_state_for_root(run_key)
            return
        if run_key in self._workflow_step_runs:
            self._end_workflow_step(run_key=run_key, outputs=outputs, error=None)
            return
        self._run_to_graph_key.pop(run_key, None)
        self._end_framework_span(self._chain_spans, run_id=run_id, error=None)

    def _on_chain_error(self, *, error: BaseException, run_id: UUID) -> None:
        run_key = str(run_id)
        if run_key in self._graph_root_run_keys:
            self._cleanup_graph_state_for_root(run_key)
            return
        if run_key in self._workflow_step_runs:
            self._end_workflow_step(run_key=run_key, outputs=None, error=error)
            return
        self._run_to_graph_key.pop(run_key, None)
        self._end_framework_span(self._chain_spans, run_id=run_id, error=error)

    def _start_workflow_step(
        self,
        *,
        serialized: dict[str, Any] | None,
        run_key: str,
        parent_run_id: UUID | None,
        run_type: str,
        callback_kwargs: dict[str, Any] | None,
        inputs: dict[str, Any] | None,
    ) -> None:
        conversation_id, thread_id = _resolve_framework_conversation_id(
            framework_name=self._framework_name,
            run_key=run_key,
            serialized=serialized,
            invocation_params=None,
            callback_kwargs=callback_kwargs,
        )
        conversation_id = self._resolve_graph_conversation_id(conversation_id, parent_run_id)
        component_name = _resolve_component_name(serialized, callback_kwargs)
        langgraph_node = _resolve_langgraph_node(callback_kwargs, None, serialized)
        step_name = langgraph_node or component_name or run_type

        tags = dict(self._extra_tags)
        tags["sigil.framework.name"] = self._framework_name
        tags["sigil.framework.source"] = self._framework_source
        tags["sigil.framework.language"] = self._framework_language
        tags["sigil.workflow_step.type"] = run_type

        metadata: dict[str, Any] = dict(self._extra_metadata)
        metadata[_span_attr_framework_run_id] = run_key
        metadata[_span_attr_framework_run_type] = run_type
        if thread_id != "":
            metadata[_span_attr_framework_thread_id] = thread_id
        parent_run_key = _normalize_run_key(parent_run_id)
        if parent_run_key != "":
            metadata[_span_attr_framework_parent_run_id] = parent_run_key
        if component_name != "":
            metadata[_span_attr_framework_component_name] = component_name
        if langgraph_node != "":
            metadata[_span_attr_framework_langgraph_node] = langgraph_node
        metadata = _normalize_framework_metadata(metadata)

        step_id = f"wfs_{secrets.token_hex(8)}"
        graph_root_key = self._find_graph_root_key(run_key)

        parent_step_ids: list[str] = []
        if graph_root_key:
            last_step = self._graph_run_last_step_id.get(graph_root_key, "")
            if last_step:
                parent_step_ids = [last_step]

        input_state: dict[str, Any] = {}
        if self._capture_inputs and inputs is not None:
            input_state = _safe_serializable_dict(inputs)

        self._workflow_step_runs[run_key] = _WorkflowStepState(
            step_id=step_id,
            conversation_id=conversation_id,
            step_name=step_name,
            started_at=datetime.now(timezone.utc),
            tags=tags,
            metadata=metadata,
            parent_step_ids=parent_step_ids,
            graph_root_key=graph_root_key,
            capture_outputs=self._capture_outputs,
            input_state=input_state,
        )

    def _end_workflow_step(
        self,
        *,
        run_key: str,
        outputs: dict[str, Any] | None,
        error: BaseException | None,
    ) -> None:
        state = self._workflow_step_runs.pop(run_key, None)
        if state is None:
            return

        output_state: dict[str, Any] = {}
        if state.capture_outputs and outputs is not None:
            output_state = _safe_serializable_dict(outputs)

        step = WorkflowStep(
            id=state.step_id,
            conversation_id=state.conversation_id,
            step_name=state.step_name,
            framework=self._framework_name,
            agent_name=self._agent_name,
            agent_version=self._agent_version,
            started_at=state.started_at,
            completed_at=datetime.now(timezone.utc),
            input_state=state.input_state,
            output_state=output_state,
            error=str(error) if error is not None else "",
            tags=state.tags,
            metadata=state.metadata,
            linked_generation_ids=list(state.linked_generation_ids),
            parent_step_ids=state.parent_step_ids,
        )

        try:
            self._client.enqueue_workflow_step(step)
        except Exception as exc:  # noqa: BLE001
            log_warn = getattr(self._client, "_log_warn", None)
            if log_warn is not None:
                log_warn("sigil: failed to enqueue workflow step", exc)
            return

        if state.graph_root_key:
            self._graph_run_last_step_id[state.graph_root_key] = state.step_id

    def _on_retriever_start(
        self,
        *,
        serialized: dict[str, Any] | None,
        run_id: UUID,
        parent_run_id: UUID | None,
        callback_kwargs: dict[str, Any] | None = None,
    ) -> None:
        run_key = str(run_id)
        if run_key in self._retriever_spans:
            return

        span = self._start_framework_span(
            run_key=run_key,
            parent_run_id=parent_run_id,
            serialized=serialized,
            invocation_params=None,
            callback_kwargs=callback_kwargs,
            run_type="retriever",
            operation_name="framework_retriever",
            fallback_component="retriever",
        )
        self._retriever_spans[run_key] = span

    def _on_retriever_end(self, *, run_id: UUID) -> None:
        self._end_framework_span(self._retriever_spans, run_id=run_id, error=None)

    def _on_retriever_error(self, *, error: BaseException, run_id: UUID) -> None:
        self._end_framework_span(self._retriever_spans, run_id=run_id, error=error)

    def _start_framework_span(
        self,
        *,
        run_key: str,
        parent_run_id: UUID | None,
        serialized: dict[str, Any] | None,
        invocation_params: dict[str, Any] | None,
        callback_kwargs: dict[str, Any] | None,
        run_type: str,
        operation_name: str,
        fallback_component: str,
    ) -> Span:
        conversation_id, thread_id = _resolve_framework_conversation_id(
            framework_name=self._framework_name,
            run_key=run_key,
            serialized=serialized,
            invocation_params=invocation_params,
            callback_kwargs=callback_kwargs,
        )
        parent_run_key = _normalize_run_key(parent_run_id)
        component_name = _resolve_component_name(serialized, callback_kwargs)
        retry_attempt = _resolve_framework_retry_attempt(callback_kwargs, invocation_params, serialized)
        langgraph_node = _resolve_langgraph_node(callback_kwargs, invocation_params, serialized)
        event_id = _resolve_framework_event_id(callback_kwargs, invocation_params, serialized)

        span_name = f"{self._framework_name}.{fallback_component}"
        component = component_name if component_name != "" else fallback_component
        if component != "":
            span_name = f"{self._framework_name}.{fallback_component} {component}"

        span = self._framework_tracer().start_span(span_name, kind=SpanKind.INTERNAL)
        span.set_attribute(_span_attr_operation_name, operation_name)
        span.set_attribute(_span_attr_framework_name, self._framework_name)
        span.set_attribute(_span_attr_framework_source, self._framework_source)
        span.set_attribute(_span_attr_framework_language, self._framework_language)
        span.set_attribute(_span_attr_framework_run_id, run_key)
        span.set_attribute(_span_attr_framework_run_type, run_type)
        if thread_id != "":
            span.set_attribute(_span_attr_framework_thread_id, thread_id)
        if conversation_id != "":
            span.set_attribute(_span_attr_conversation_id, conversation_id)
        if parent_run_key != "":
            span.set_attribute(_span_attr_framework_parent_run_id, parent_run_key)
        if component_name != "":
            span.set_attribute(_span_attr_framework_component_name, component_name)
        if retry_attempt is not None:
            span.set_attribute(_span_attr_framework_retry_attempt, retry_attempt)
        if langgraph_node != "":
            span.set_attribute(_span_attr_framework_langgraph_node, langgraph_node)
        if event_id != "":
            span.set_attribute(_span_attr_framework_event_id, event_id)

        return span

    def _end_framework_span(
        self,
        span_map: dict[str, Span],
        *,
        run_id: UUID,
        error: BaseException | None,
    ) -> None:
        span = span_map.pop(str(run_id), None)
        if span is None:
            return

        if error is None:
            span.set_status(Status(StatusCode.OK))
            span.end()
            return

        span.set_attribute(_span_attr_error_type, "framework_error")
        span.set_attribute(_span_attr_error_category, "sdk_error")
        span.record_exception(error)
        span.set_status(Status(StatusCode.ERROR, str(error)))
        span.end()

    def _framework_tracer(self):
        tracer = getattr(self._client, "_tracer", None)
        if tracer is not None:
            return tracer
        return otel_trace.get_tracer(self._framework_instrumentation_name)


def _resolve_provider(
    *,
    explicit_provider: str,
    provider_resolver: ProviderResolver,
    model_name: str,
    serialized: dict[str, Any] | None,
    invocation_params: dict[str, Any] | None,
) -> str:
    explicit = _normalize_provider_name(explicit_provider)
    if explicit != "":
        return explicit

    if callable(provider_resolver):
        resolved = _normalize_provider_name(provider_resolver(model_name, serialized, invocation_params))
        return resolved if resolved != "" else "custom"

    for payload in (invocation_params, serialized):
        provider = _normalize_provider_name(_as_str(_read(payload, "provider")))
        if provider != "":
            return provider
        provider = _normalize_provider_name(_as_str(_read(payload, "ls_provider")))
        if provider != "":
            return provider

    return _infer_provider_from_model_name(model_name)


def _resolve_model_name(serialized: dict[str, Any] | None, invocation_params: dict[str, Any] | None) -> str:
    for payload in (invocation_params, serialized):
        for key in ("model", "model_name", "ls_model_name"):
            value = _as_str(_read(payload, key))
            if value != "":
                return value

        kwargs = _read(payload, "kwargs")
        for key in ("model", "model_name"):
            value = _as_str(_read(kwargs, key))
            if value != "":
                return value

    return "unknown"


def _infer_provider_from_model_name(model_name: str) -> str:
    normalized = model_name.strip().lower()
    if (
        normalized.startswith("gpt-")
        or normalized.startswith("o1")
        or normalized.startswith("o3")
        or normalized.startswith("o4")
    ):
        return "openai"
    if normalized.startswith("claude-"):
        return "anthropic"
    if normalized.startswith("gemini-"):
        return "gemini"
    return "custom"


def _normalize_provider_name(value: str) -> str:
    normalized = value.strip().lower()
    if normalized in {"openai", "anthropic", "gemini"}:
        return normalized
    return "" if normalized == "" else "custom"


def _is_streaming(invocation_params: dict[str, Any] | None) -> bool:
    if invocation_params is None:
        return False
    if _as_bool(_read(invocation_params, "stream")):
        return True
    return _as_bool(_read(invocation_params, "streaming"))


def _resolve_framework_thread_id(
    *,
    serialized: dict[str, Any] | None,
    invocation_params: dict[str, Any] | None,
    callback_kwargs: dict[str, Any] | None,
) -> str:
    for payload in (callback_kwargs, invocation_params, serialized):
        thread_id = _thread_id_from_payload(payload)
        if thread_id != "":
            return thread_id
    return ""


def _resolve_framework_conversation_id(
    *,
    framework_name: str,
    run_key: str,
    serialized: dict[str, Any] | None,
    invocation_params: dict[str, Any] | None,
    callback_kwargs: dict[str, Any] | None,
) -> tuple[str, str]:
    for payload in (callback_kwargs, invocation_params, serialized):
        conversation_id = _conversation_id_from_payload(payload)
        if conversation_id != "":
            thread_id = _thread_id_from_payload(payload)
            if thread_id != "":
                return conversation_id, thread_id
            return conversation_id, _resolve_framework_thread_id(
                serialized=serialized,
                invocation_params=invocation_params,
                callback_kwargs=callback_kwargs,
            )

    thread_id = _resolve_framework_thread_id(
        serialized=serialized,
        invocation_params=invocation_params,
        callback_kwargs=callback_kwargs,
    )
    if thread_id != "":
        return thread_id, thread_id

    # Deterministic fallback when framework context does not expose a stable conversation key.
    return f"sigil:framework:{framework_name}:{run_key}", ""


def _thread_id_from_payload(payload: Any) -> str:
    candidates = (
        _as_str(_read(payload, "thread_id")),
        _as_str(_read(payload, "threadId")),
        _as_str(_read(_read(payload, "metadata"), "thread_id")),
        _as_str(_read(_read(payload, "metadata"), "threadId")),
        _as_str(_read(_read(payload, "configurable"), "thread_id")),
        _as_str(_read(_read(payload, "configurable"), "threadId")),
        _as_str(_read(_read(payload, "config"), "thread_id")),
        _as_str(_read(_read(payload, "config"), "threadId")),
        _as_str(_read(_read(_read(payload, "config"), "metadata"), "thread_id")),
        _as_str(_read(_read(_read(payload, "config"), "metadata"), "threadId")),
        _as_str(_read(_read(_read(payload, "config"), "configurable"), "thread_id")),
        _as_str(_read(_read(_read(payload, "config"), "configurable"), "threadId")),
    )
    for candidate in candidates:
        if candidate != "":
            return candidate
    return ""


def _conversation_id_from_payload(payload: Any) -> str:
    candidates = (
        _as_str(_read(payload, "conversation_id")),
        _as_str(_read(payload, "conversationId")),
        _as_str(_read(payload, "session_id")),
        _as_str(_read(payload, "sessionId")),
        _as_str(_read(payload, "group_id")),
        _as_str(_read(payload, "groupId")),
        _as_str(_read(_read(payload, "metadata"), "conversation_id")),
        _as_str(_read(_read(payload, "metadata"), "conversationId")),
        _as_str(_read(_read(payload, "metadata"), "session_id")),
        _as_str(_read(_read(payload, "metadata"), "sessionId")),
        _as_str(_read(_read(payload, "metadata"), "group_id")),
        _as_str(_read(_read(payload, "metadata"), "groupId")),
        _as_str(_read(_read(payload, "configurable"), "conversation_id")),
        _as_str(_read(_read(payload, "configurable"), "conversationId")),
        _as_str(_read(_read(payload, "configurable"), "session_id")),
        _as_str(_read(_read(payload, "configurable"), "sessionId")),
        _as_str(_read(_read(payload, "configurable"), "group_id")),
        _as_str(_read(_read(payload, "configurable"), "groupId")),
        _as_str(_read(_read(payload, "config"), "conversation_id")),
        _as_str(_read(_read(payload, "config"), "conversationId")),
        _as_str(_read(_read(payload, "config"), "session_id")),
        _as_str(_read(_read(payload, "config"), "sessionId")),
        _as_str(_read(_read(payload, "config"), "group_id")),
        _as_str(_read(_read(payload, "config"), "groupId")),
    )
    for candidate in candidates:
        if candidate != "":
            return candidate
    return ""


def _resolve_component_name(serialized: dict[str, Any] | None, callback_kwargs: dict[str, Any] | None) -> str:
    candidates = (
        _as_str(_read(serialized, "name")),
        _id_path(_read(serialized, "id")),
        _id_path(_read(serialized, "lc_id")),
        _as_str(_read(_read(serialized, "kwargs"), "name")),
        _as_str(_read(callback_kwargs, "component_name")),
        _as_str(_read(callback_kwargs, "run_name")),
    )
    for candidate in candidates:
        if candidate != "":
            return candidate
    return ""


def _resolve_langgraph_node(
    callback_kwargs: dict[str, Any] | None,
    invocation_params: dict[str, Any] | None,
    serialized: dict[str, Any] | None,
) -> str:
    for payload in (callback_kwargs, invocation_params, serialized):
        candidate = _langgraph_node_from_payload(payload)
        if candidate != "":
            return candidate
    return ""


def _langgraph_node_from_payload(payload: Any) -> str:
    candidates = (
        _as_str(_read(payload, "langgraph_node")),
        _as_str(_read(payload, "langgraph_node_name")),
        _as_str(_read(payload, "node_name")),
        _as_str(_read(payload, "node")),
        _as_str(_read(_read(payload, "metadata"), "langgraph_node")),
        _as_str(_read(_read(payload, "metadata"), "langgraph_node_name")),
        _as_str(_read(_read(payload, "configurable"), "langgraph_node")),
        _as_str(_read(_read(payload, "configurable"), "langgraph_node_name")),
        _as_str(_read(_read(_read(payload, "config"), "configurable"), "__pregel_node")),
    )
    for candidate in candidates:
        if candidate != "":
            return candidate
    return ""


def _resolve_generation_id(*payloads: Any) -> str:
    for payload in payloads:
        candidate = _generation_id_from_payload(payload)
        if candidate != "":
            return candidate
    return ""


def _generation_id_from_payload(payload: Any) -> str:
    candidates = (
        _as_str(_read(payload, _span_attr_generation_id)),
        _as_str(_read(payload, "generation_id")),
        _as_str(_read(payload, "generationId")),
        _as_str(_read(_read(payload, "metadata"), _span_attr_generation_id)),
        _as_str(_read(_read(payload, "metadata"), "generation_id")),
        _as_str(_read(_read(payload, "metadata"), "generationId")),
        _as_str(_read(_read(payload, "configurable"), _span_attr_generation_id)),
        _as_str(_read(_read(payload, "configurable"), "generation_id")),
        _as_str(_read(_read(payload, "configurable"), "generationId")),
        _as_str(_read(_read(_read(payload, "config"), "metadata"), _span_attr_generation_id)),
        _as_str(_read(_read(_read(payload, "config"), "configurable"), _span_attr_generation_id)),
    )
    for candidate in candidates:
        if candidate != "":
            return candidate
    return ""


def _resolve_parent_generation_ids(*payloads: Any) -> list[str]:
    output: list[str] = []
    seen: set[str] = set()
    for payload in payloads:
        for raw_value in _parent_generation_id_values_from_payload(payload):
            for generation_id in _normalize_parent_generation_id_values(raw_value):
                if generation_id in seen:
                    continue
                seen.add(generation_id)
                output.append(generation_id)
    return output


def _parent_generation_id_values_from_payload(payload: Any) -> tuple[Any, ...]:
    return (
        _read(payload, _span_attr_parent_generation_ids),
        _read(payload, "parent_generation_ids"),
        _read(payload, "parentGenerationIds"),
        _read(_read(payload, "metadata"), _span_attr_parent_generation_ids),
        _read(_read(payload, "metadata"), "parent_generation_ids"),
        _read(_read(payload, "metadata"), "parentGenerationIds"),
        _read(_read(payload, "configurable"), _span_attr_parent_generation_ids),
        _read(_read(payload, "configurable"), "parent_generation_ids"),
        _read(_read(payload, "configurable"), "parentGenerationIds"),
        _read(_read(_read(payload, "config"), "metadata"), _span_attr_parent_generation_ids),
        _read(_read(_read(payload, "config"), "configurable"), _span_attr_parent_generation_ids),
    )


def _normalize_parent_generation_id_values(value: Any) -> list[str]:
    if value is None:
        return []
    if isinstance(value, str):
        trimmed = value.strip()
        return [trimmed] if trimmed != "" else []
    if isinstance(value, (list, tuple, set)):
        output: list[str] = []
        for item in value:
            trimmed = _as_str(item)
            if trimmed != "":
                output.append(trimmed)
        return output
    return []


def _normalize_framework_tags(value: Any) -> list[str]:
    raw_values = value if isinstance(value, list) else [value]
    output: list[str] = []
    seen: set[str] = set()
    for raw in raw_values:
        if not isinstance(raw, str):
            continue
        trimmed = raw.strip()
        if trimmed == "" or trimmed in seen:
            continue
        seen.add(trimmed)
        output.append(trimmed)
    return output


def _normalize_framework_metadata(value: dict[str, Any]) -> dict[str, Any]:
    output: dict[str, Any] = {}
    seen: set[int] = set()
    for raw_key, raw_item in value.items():
        if not isinstance(raw_key, str):
            continue
        key = raw_key.strip()
        if key == "":
            continue
        normalized_item = _normalize_framework_metadata_value(raw_item, depth=0, seen=seen)
        if normalized_item is _metadata_drop:
            continue
        output[key] = normalized_item
    return output


def _normalize_framework_metadata_value(value: Any, *, depth: int, seen: set[int]) -> Any:
    if depth > _max_framework_metadata_depth:
        return _metadata_drop

    if value is None:
        return None

    if isinstance(value, (str, bool, int)):
        return value

    if isinstance(value, float):
        return value if math.isfinite(value) else _metadata_drop

    if isinstance(value, datetime):
        return value.astimezone(timezone.utc).isoformat()

    if isinstance(value, (list, tuple)):
        output: list[Any] = []
        for item in value:
            normalized_item = _normalize_framework_metadata_value(item, depth=depth + 1, seen=seen)
            if normalized_item is _metadata_drop:
                continue
            output.append(normalized_item)
        return output

    if isinstance(value, dict):
        value_id = id(value)
        if value_id in seen:
            return "[circular]"
        seen.add(value_id)
        output: dict[str, Any] = {}
        for raw_key, raw_item in value.items():
            if not isinstance(raw_key, str):
                continue
            key = raw_key.strip()
            if key == "":
                continue
            normalized_item = _normalize_framework_metadata_value(raw_item, depth=depth + 1, seen=seen)
            if normalized_item is _metadata_drop:
                continue
            output[key] = normalized_item
        seen.remove(value_id)
        return output

    return _metadata_drop


def _resolve_framework_retry_attempt(*payloads: Any) -> int | None:
    for payload in payloads:
        value = _retry_attempt_from_payload(payload)
        if value is not None:
            return value
    return None


def _resolve_framework_event_id(*payloads: Any) -> str:
    for payload in payloads:
        if payload is None:
            continue
        candidates = (
            _as_str(_read(payload, "event_id")),
            _as_str(_read(payload, "eventId")),
            _as_str(_read(payload, "invocation_id")),
            _as_str(_read(payload, "invocationId")),
            _as_str(_read(_read(payload, "metadata"), "event_id")),
            _as_str(_read(_read(payload, "metadata"), "eventId")),
            _as_str(_read(_read(payload, "metadata"), "invocation_id")),
            _as_str(_read(_read(payload, "metadata"), "invocationId")),
        )
        for candidate in candidates:
            if candidate != "":
                return candidate
    return ""


def _retry_attempt_from_payload(payload: Any) -> int | None:
    candidates = (
        _as_optional_int(_read(payload, "retry_attempt")),
        _as_optional_int(_read(payload, "retryAttempt")),
        _as_optional_int(_read(payload, "attempt")),
        _as_optional_int(_read(payload, "retry")),
        _as_optional_int(_read(_read(payload, "metadata"), "retry_attempt")),
        _as_optional_int(_read(_read(payload, "metadata"), "retryAttempt")),
        _as_optional_int(_read(_read(payload, "configurable"), "retry_attempt")),
        _as_optional_int(_read(_read(payload, "configurable"), "retryAttempt")),
    )
    for candidate in candidates:
        if candidate is not None:
            return candidate
    return None


def _normalize_run_key(run_id: UUID | None) -> str:
    if run_id is None:
        return ""
    return str(run_id).strip()


def _resolve_tool_name(serialized: dict[str, Any] | None, callback_kwargs: dict[str, Any] | None) -> str:
    candidates = (
        _as_str(_read(serialized, "name")),
        _as_str(_read(serialized, "tool_name")),
        _as_str(_read(callback_kwargs, "name")),
        _as_str(_read(callback_kwargs, "run_name")),
        "framework_tool",
    )
    for candidate in candidates:
        if candidate != "":
            return candidate
    return "framework_tool"


def _resolve_tool_description(serialized: dict[str, Any] | None) -> str:
    return _as_str(_read(serialized, "description"))


def _resolve_tool_arguments(input_str: str, callback_kwargs: dict[str, Any] | None) -> Any:
    explicit_inputs = _read(callback_kwargs, "inputs")
    if explicit_inputs is not None:
        return explicit_inputs
    return input_str.strip()


def _map_chat_inputs(messages: list[list[Any]]) -> list[Message]:
    output: list[Message] = []
    for batch in messages:
        for message in batch:
            text = _extract_message_text(message)
            if text == "":
                continue
            output.append(
                Message(
                    role=_normalize_role(_extract_message_role(message)),
                    parts=[Part(kind=PartKind.TEXT, text=text)],
                )
            )
    return output


def _map_output_messages(response: Any) -> list[Message]:
    output: list[Message] = []
    text_chunks: list[str] = []
    for candidates in _as_list(_read(response, "generations")):
        for candidate in _as_list(candidates):
            mapped = _map_generation_candidate_message(candidate)
            text = _as_str(_read(candidate, "text"))
            if text != "":
                if mapped is None:
                    text_chunks.append(text)
                else:
                    output.append(mapped)
                continue

            if mapped is not None:
                output.append(mapped)

    if text_chunks:
        output.insert(
            0,
            Message(role=MessageRole.ASSISTANT, parts=[Part(kind=PartKind.TEXT, text="\n".join(text_chunks))]),
        )

    if not output:
        fallback_text = _as_str(_read(response, "text"))
        if fallback_text != "":
            output.append(Message(role=MessageRole.ASSISTANT, parts=[Part(kind=PartKind.TEXT, text=fallback_text)]))

    return output


def _map_generation_candidate_message(candidate: Any) -> Message | None:
    message = _read(candidate, "message")
    parts: list[Part] = []

    text = _extract_message_text(message)
    if text != "":
        parts.append(Part(kind=PartKind.TEXT, text=text))

    for tool_call in _as_list(_read(message, "tool_calls")):
        tool_call_part = _map_langchain_tool_call_part(tool_call)
        if tool_call_part is not None:
            parts.append(tool_call_part)

    if not parts:
        return None

    return Message(role=_normalize_role(_extract_message_role(message)), parts=parts)


def _map_langchain_tool_call_part(tool_call: Any) -> Part | None:
    name = _as_str(_read(tool_call, "name"))
    if name == "":
        return None

    args = _read(tool_call, "args")
    if args is None:
        args = _read(tool_call, "arguments")

    return Part(
        kind=PartKind.TOOL_CALL,
        tool_call=ToolCall(
            id=_as_str(_read(tool_call, "id")),
            name=name,
            input_json=_json_bytes(args),
        ),
    )


def _extract_message_text(message: Any) -> str:
    content = _read(message, "content")
    if isinstance(content, str):
        return content.strip()

    if isinstance(content, list):
        parts: list[str] = []
        for item in content:
            if isinstance(item, str):
                trimmed = item.strip()
                if trimmed != "":
                    parts.append(trimmed)
            else:
                text = _as_str(_read(item, "text"))
                if text != "":
                    parts.append(text)
        return " ".join(parts).strip()

    if isinstance(content, dict):
        text = _as_str(_read(content, "text"))
        if text != "":
            return text

    return ""


def _extract_message_role(message: Any) -> str:
    role = _as_str(_read(message, "role"))
    if role != "":
        return role

    role = _as_str(_read(message, "type"))
    if role != "":
        return role

    return "user"


def _first_generation_message(response: Any) -> Any:
    for candidates in _as_list(_read(response, "generations")):
        for candidate in _as_list(candidates):
            message = _read(candidate, "message")
            if message is not None:
                return message
    return None


def _map_framework_usage(raw_usage: Any):
    usage = map_usage(raw_usage)
    input_token_details = _read(raw_usage, "input_token_details")
    output_token_details = _read(raw_usage, "output_token_details")

    cache_read = _as_int(_read(input_token_details, "cache_read"))
    if cache_read > 0 and usage.cache_read_input_tokens == 0:
        usage.cache_read_input_tokens = cache_read

    reasoning = _as_int(_read(output_token_details, "reasoning"))
    if reasoning > 0 and usage.reasoning_tokens == 0:
        usage.reasoning_tokens = reasoning

    return usage


def _normalize_role(role: str) -> MessageRole:
    normalized = role.strip().lower()
    if normalized in {"assistant", "ai"}:
        return MessageRole.ASSISTANT
    if normalized == "tool":
        return MessageRole.TOOL
    return MessageRole.USER


def _read(value: Any, key: str) -> Any:
    if value is None:
        return None
    if isinstance(value, dict):
        return value.get(key)
    return getattr(value, key, None)


def _as_list(value: Any) -> list[Any]:
    if isinstance(value, list):
        return value
    return []


def _as_str(value: Any) -> str:
    if isinstance(value, str):
        return value.strip()
    return ""


def _safe_serializable_dict(value: Any) -> dict[str, Any]:
    """Convert a value to a JSON-safe dict for Struct proto fields.

    Does a JSON roundtrip so non-native types (e.g. LangChain BaseMessage)
    are coerced to strings by default=str and the returned dict contains
    only primitives that google.protobuf.Struct can handle.
    """
    if value is None:
        return {}
    if isinstance(value, dict):
        try:
            return json.loads(json.dumps(value, default=str))
        except Exception:  # noqa: BLE001
            return {"raw": str(value)}
    return {"raw": str(value)}


def _json_bytes(value: Any) -> bytes:
    if value is None:
        return b""
    try:
        return json.dumps(value, default=str, sort_keys=True).encode("utf-8")
    except Exception:
        return b""


def _id_path(value: Any) -> str:
    if not isinstance(value, list):
        return ""
    parts: list[str] = []
    for item in value:
        if not isinstance(item, str):
            continue
        trimmed = item.strip()
        if trimmed != "":
            parts.append(trimmed)
    return ".".join(parts)


def _as_int(value: Any) -> int:
    if isinstance(value, bool):
        return int(value)
    if isinstance(value, int):
        return value
    if isinstance(value, float):
        return int(value)
    if isinstance(value, str):
        stripped = value.strip()
        if stripped == "":
            return 0
        try:
            return int(stripped)
        except ValueError:
            return 0
    return 0


def _as_optional_int(value: Any) -> int | None:
    if value is None or isinstance(value, bool):
        return None
    if isinstance(value, int):
        return value
    if isinstance(value, float):
        integer = int(value)
        if float(integer) == value:
            return integer
        return None
    if isinstance(value, str):
        stripped = value.strip()
        if stripped == "":
            return None
        try:
            return int(stripped)
        except ValueError:
            return None
    return None


def _as_optional_float(value: Any) -> float | None:
    if value is None or isinstance(value, bool):
        return None
    if isinstance(value, (int, float)):
        return float(value)
    if isinstance(value, str):
        stripped = value.strip()
        if stripped == "":
            return None
        try:
            return float(stripped)
        except ValueError:
            return None
    return None


def _resolve_tool_definitions(invocation_params: dict[str, Any] | None) -> list[ToolDefinition]:
    raw = _read(invocation_params, "tools")
    if not isinstance(raw, (list, tuple)):
        return []
    result: list[ToolDefinition] = []
    for item in raw:
        if isinstance(item, ToolDefinition):
            result.append(item)
            continue

        tool_type = _as_str(_read(item, "type"))
        function = _read(item, "function") if tool_type == "function" else item
        name = _as_str(_read(function, "name"))
        if name == "":
            continue

        schema = _read(function, "parameters")
        if schema is None:
            schema = _read(function, "parameters_json_schema")

        result.append(
            ToolDefinition(
                name=name,
                description=_as_str(_read(function, "description")),
                type=tool_type or "function",
                input_schema_json=_json_bytes(schema),
            )
        )
    return result


def _as_bool(value: Any) -> bool:
    if isinstance(value, bool):
        return value
    if isinstance(value, str):
        normalized = value.strip().lower()
        return normalized in {"1", "true", "yes", "on"}
    if isinstance(value, int):
        return value != 0
    return False
