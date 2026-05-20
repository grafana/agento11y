"""Sigil client runtime and recorder lifecycle implementation."""

from __future__ import annotations

import copy
import json
import logging
import re
import secrets
import threading
from collections.abc import Callable, Sequence
from dataclasses import dataclass, field, replace
from datetime import datetime, timezone
from typing import Any
from urllib import error as urllib_error
from urllib import parse as urllib_parse
from urllib import request as urllib_request

from opentelemetry import metrics, trace
from opentelemetry.metrics import Histogram
from opentelemetry.trace import Span, SpanKind, Status, StatusCode, use_span

from .config import ClientConfig, resolve_config
from .context import (
    _pop_capture_mode,
    _push_capture_mode,
    agent_name_from_context,
    agent_version_from_context,
    content_capture_mode_from_context,
    conversation_id_from_context,
    conversation_title_from_context,
    user_id_from_context,
)
from .errors import (
    ClientShutdownError,
    EnqueueError,
    QueueFullError,
    RatingConflictError,
    RatingTransportError,
    ValidationError,
)
from .exporters import GRPCGenerationExporter, HTTPGenerationExporter, NoopGenerationExporter
from .hooks import HookEvaluateRequest, HookEvaluateResponse
from .hooks import evaluate_hook as _evaluate_hook
from .models import (
    ContentCaptureMode,
    ConversationRating,
    ConversationRatingInput,
    ConversationRatingSummary,
    ConversationRatingValue,
    EmbeddingResult,
    EmbeddingStart,
    ExecuteToolCallsOptions,
    ExportGenerationsRequest,
    ExportWorkflowStepsRequest,
    Generation,
    GenerationMode,
    GenerationStart,
    Message,
    MessageRole,
    PartKind,
    SubmitConversationRatingResponse,
    ToolExecutionEnd,
    ToolExecutionStart,
    ToolResult,
    WorkflowStep,
    _metadata_key_content_capture_mode,
    tool_result_part,
)
from .proto_mapping import generation_to_proto
from .validation import validate_embedding_result, validate_embedding_start, validate_generation

_span_attr_generation_id = "sigil.generation.id"
_span_attr_sdk_name = "sigil.sdk.name"
_span_attr_framework_run_id = "sigil.framework.run_id"
_span_attr_framework_thread_id = "sigil.framework.thread_id"
_span_attr_framework_parent_run_id = "sigil.framework.parent_run_id"
_span_attr_framework_component_name = "sigil.framework.component_name"
_span_attr_framework_run_type = "sigil.framework.run_type"
_span_attr_framework_retry_attempt = "sigil.framework.retry_attempt"
_span_attr_framework_langgraph_node = "sigil.framework.langgraph.node"
_span_attr_framework_event_id = "sigil.framework.event_id"
_span_attr_conversation_id = "gen_ai.conversation.id"
_span_attr_conversation_title = "sigil.conversation.title"
_span_attr_user_id = "user.id"
_span_attr_agent_name = "gen_ai.agent.name"
_span_attr_agent_version = "gen_ai.agent.version"
_span_attr_error_type = "error.type"
_span_attr_error_category = "error.category"
_span_attr_operation_name = "gen_ai.operation.name"
_span_attr_provider_name = "gen_ai.provider.name"
_span_attr_request_model = "gen_ai.request.model"
_span_attr_request_max_tokens = "gen_ai.request.max_tokens"
_span_attr_request_temperature = "gen_ai.request.temperature"
_span_attr_request_top_p = "gen_ai.request.top_p"
_span_attr_request_tool_choice = "sigil.gen_ai.request.tool_choice"
_span_attr_request_thinking_enabled = "sigil.gen_ai.request.thinking.enabled"
_span_attr_request_thinking_budget = "sigil.gen_ai.request.thinking.budget_tokens"
_span_attr_response_id = "gen_ai.response.id"
_span_attr_response_model = "gen_ai.response.model"
_span_attr_finish_reasons = "gen_ai.response.finish_reasons"
_span_attr_input_tokens = "gen_ai.usage.input_tokens"
_span_attr_output_tokens = "gen_ai.usage.output_tokens"
_span_attr_embedding_input_count = "gen_ai.embeddings.input_count"
_span_attr_embedding_input_texts = "gen_ai.embeddings.input_texts"
_span_attr_embedding_dim_count = "gen_ai.embeddings.dimension.count"
_span_attr_request_encoding_formats = "gen_ai.request.encoding_formats"
_span_attr_cache_read_tokens = "gen_ai.usage.cache_read_input_tokens"
_span_attr_cache_write_tokens = "gen_ai.usage.cache_write_input_tokens"
_span_attr_reasoning_tokens = "gen_ai.usage.reasoning_tokens"
_span_attr_tool_name = "gen_ai.tool.name"
_span_attr_tool_call_id = "gen_ai.tool.call.id"
_span_attr_tool_type = "gen_ai.tool.type"
_span_attr_tool_description = "gen_ai.tool.description"
_span_attr_tool_call_arguments = "gen_ai.tool.call.arguments"
_span_attr_tool_call_result = "gen_ai.tool.call.result"
_span_attr_parent_generation_ids = "sigil.generation.parent_generation_ids"
_max_rating_conversation_id_len = 255
_max_rating_id_len = 128
_max_rating_generation_id_len = 255
_max_rating_actor_id_len = 255
_max_rating_source_len = 64
_max_rating_comment_bytes = 4096
_max_rating_metadata_bytes = 16 * 1024

_metric_operation_duration = "gen_ai.client.operation.duration"
_metric_token_usage = "gen_ai.client.token.usage"
_metric_ttft = "gen_ai.client.time_to_first_token"
_metric_tool_calls_per_operation = "gen_ai.client.tool_calls_per_operation"
_metric_attr_token_type = "gen_ai.token.type"
_metric_token_type_input = "input"
_metric_token_type_output = "output"
_metric_token_type_cache_read = "cache_read"
_metric_token_type_cache_write = "cache_write"
_metric_token_type_reasoning = "reasoning"

_DURATION_BUCKETS_SECONDS: tuple[float, ...] = (
    0.01,
    0.02,
    0.04,
    0.08,
    0.16,
    0.32,
    0.64,
    1.28,
    2.56,
    5.12,
    10.24,
    20.48,
    40.96,
    81.92,
)

_TOKEN_USAGE_BUCKETS: tuple[float, ...] = (
    1,
    4,
    16,
    64,
    256,
    1024,
    4096,
    16384,
    65536,
    262144,
    1048576,
    4194304,
    16777216,
    67108864,
)

_status_code_pattern = re.compile(r"\b([1-5][0-9][0-9])\b")
_instrumentation_name = "github.com/grafana/sigil/sdks/python"
_sdk_name = "sdk-python"

# Generation metadata keys for cache diagnostics (Sigil docs/guides/cache-diagnostics.md).
CACHE_DIAGNOSTICS_MISS_REASON_KEY = "sigil.cache_diagnostics.miss_reason"
CACHE_DIAGNOSTICS_MISSED_INPUT_TOKENS_KEY = "sigil.cache_diagnostics.missed_input_tokens"
CACHE_DIAGNOSTICS_PREVIOUS_MESSAGE_ID_KEY = "sigil.cache_diagnostics.previous_message_id"
_default_embedding_operation_name = "embeddings"
_metadata_user_id_key = "sigil.user.id"
_metadata_legacy_user_id_key = "user.id"


def _resolve_content_capture_mode(override: ContentCaptureMode, fallback: ContentCaptureMode) -> ContentCaptureMode:
    """Returns the effective mode from an override and a fallback. DEFAULT falls through."""
    if override != ContentCaptureMode.DEFAULT:
        return override
    return fallback


def _resolve_client_content_capture_mode(mode: ContentCaptureMode) -> ContentCaptureMode:
    """Resolves client-level mode. DEFAULT → NO_TOOL_CONTENT for backward compat."""
    if mode == ContentCaptureMode.DEFAULT:
        return ContentCaptureMode.NO_TOOL_CONTENT
    return mode


def _call_content_capture_resolver(
    resolver: Callable | None,
    metadata: dict[str, Any] | None,
    logger: logging.Logger | None = None,
) -> ContentCaptureMode:
    """Invokes resolver callback safely. Returns DEFAULT when None. Exceptions → METADATA_ONLY."""
    if resolver is None:
        return ContentCaptureMode.DEFAULT
    try:
        result = resolver(metadata)
        if not isinstance(result, ContentCaptureMode):
            if logger is not None:
                logger.warning(
                    "sigil: content capture resolver returned %s instead of ContentCaptureMode, "
                    "falling back to METADATA_ONLY",
                    type(result).__name__,
                )
            return ContentCaptureMode.METADATA_ONLY
        return result
    except Exception:  # noqa: BLE001
        if logger is not None:
            logger.warning("sigil: content capture resolver failed, falling back to METADATA_ONLY", exc_info=True)
        return ContentCaptureMode.METADATA_ONLY


def _resolve_tool_content_capture_mode(
    tool_mode: ContentCaptureMode,
    ctx_mode: ContentCaptureMode | None,
    client_default: ContentCaptureMode,
) -> ContentCaptureMode:
    """Resolves the effective content capture mode for a tool execution."""
    resolved = _resolve_client_content_capture_mode(client_default)
    if ctx_mode is not None and ctx_mode != ContentCaptureMode.DEFAULT:
        resolved = ctx_mode
    if tool_mode != ContentCaptureMode.DEFAULT:
        resolved = tool_mode
    return resolved


def _stamp_content_capture_metadata(generation: Generation, mode: ContentCaptureMode) -> None:
    """Sets the content capture mode marker on the generation."""
    generation.metadata[_metadata_key_content_capture_mode] = mode.value


def _strip_content(generation: Generation, error_category: str) -> None:
    """Strips sensitive content from a generation while preserving structure.

    Note: user-provided metadata and tags are NOT stripped. Callers are responsible
    for ensuring these dicts do not contain sensitive content when using MetadataOnly mode.
    """
    generation.system_prompt = ""
    generation.artifacts = []

    if generation.call_error != "":
        generation.call_error = error_category if error_category else "sdk_error"
    generation.metadata.pop("call_error", None)

    generation.conversation_title = ""
    generation.metadata.pop(_span_attr_conversation_title, None)

    for message in generation.input:
        _strip_message_content(message)
    for message in generation.output:
        _strip_message_content(message)
    for tool in generation.tools:
        tool.description = ""
        tool.input_schema_json = b""


def _strip_message_content(message: Message) -> None:
    """Strips all content from message parts (text, thinking, tool call input, tool result)."""
    for part in message.parts:
        part.text = ""
        part.thinking = ""
        if part.tool_call is not None:
            part.tool_call.input_json = b""
        if part.tool_result is not None:
            part.tool_result.content = ""
            part.tool_result.content_json = b""


def _serialize_tool_result_payload(value: Any) -> tuple[str, bytes]:
    if value is None:
        return "", b""
    if isinstance(value, str):
        return value, b""
    if isinstance(value, (bytes, bytearray)):
        b = bytes(value)
        return "", b if b else b""
    try:
        return "", json.dumps(value, default=str).encode("utf-8")
    except (TypeError, ValueError):
        return str(value), b""


def _build_tool_result_message(
    tool_name: str,
    tool_call_id: str,
    result: Any,
    *,
    is_error: bool,
    error_message: str = "",
) -> Message:
    if is_error:
        tr = ToolResult(
            tool_call_id=tool_call_id,
            name=tool_name,
            content=error_message,
            is_error=True,
        )
    else:
        content, content_json = _serialize_tool_result_payload(result)
        tr = ToolResult(
            tool_call_id=tool_call_id,
            name=tool_name,
            content=content,
            content_json=content_json,
        )
    return Message(role=MessageRole.TOOL, name=tool_name, parts=[tool_result_part(tr)])


class Client:
    """Sigil client that records generations, tool spans, and exports in background."""

    def __init__(self, config: ClientConfig | None = None) -> None:
        self._config = resolve_config(config)
        self._logger = self._config.logger
        self._now = self._config.now
        self._sleep = self._config.sleep

        self._pending_generations: list[Generation] = []
        self._pending_workflow_steps: list[WorkflowStep] = []
        self._pending_lock = threading.Lock()
        self._flush_lock = threading.Lock()
        self._flush_thread_lock = threading.Lock()
        self._flush_thread: threading.Thread | None = None

        self._shutdown_lock = threading.Lock()
        self._shutting_down = False
        self._closed = False

        self._generation_exporter = self._config.generation_exporter
        if self._generation_exporter is None:
            protocol = self._config.generation_export.protocol.strip().lower()
            if protocol == "http":
                self._generation_exporter = HTTPGenerationExporter(
                    endpoint=self._config.generation_export.endpoint,
                    headers=self._config.generation_export.headers,
                )
            elif protocol == "grpc":
                self._generation_exporter = GRPCGenerationExporter(
                    endpoint=self._config.generation_export.endpoint,
                    headers=self._config.generation_export.headers,
                    insecure=self._config.generation_export.insecure,
                )
            elif protocol == "none":
                self._generation_exporter = NoopGenerationExporter()
            else:
                raise ValueError(f"unsupported generation export protocol {self._config.generation_export.protocol!r}")

        self._tracer = (
            self._config.tracer if self._config.tracer is not None else trace.get_tracer(_instrumentation_name)
        )
        self._meter = self._config.meter if self._config.meter is not None else metrics.get_meter(_instrumentation_name)

        self._operation_duration_histogram: Histogram = self._meter.create_histogram(
            _metric_operation_duration,
            unit="s",
            explicit_bucket_boundaries_advisory=_DURATION_BUCKETS_SECONDS,
        )
        self._token_usage_histogram: Histogram = self._meter.create_histogram(
            _metric_token_usage,
            unit="token",
            explicit_bucket_boundaries_advisory=_TOKEN_USAGE_BUCKETS,
        )
        self._ttft_histogram: Histogram = self._meter.create_histogram(
            _metric_ttft,
            unit="s",
            explicit_bucket_boundaries_advisory=_DURATION_BUCKETS_SECONDS,
        )
        self._tool_calls_histogram: Histogram = self._meter.create_histogram(
            _metric_tool_calls_per_operation, unit="count"
        )

        self._timer_stop = threading.Event()
        self._timer_thread: threading.Thread | None = None
        flush_interval_s = self._config.generation_export.flush_interval.total_seconds()
        if flush_interval_s > 0:
            self._timer_thread = threading.Thread(target=self._run_flush_timer, daemon=True)
            self._timer_thread.start()

    def start_generation(self, start: GenerationStart) -> GenerationRecorder:
        """Starts a non-stream generation recorder."""

        return self._start_generation(start=start, default_mode=GenerationMode.SYNC)

    def start_streaming_generation(self, start: GenerationStart) -> GenerationRecorder:
        """Starts a stream generation recorder."""

        return self._start_generation(start=start, default_mode=GenerationMode.STREAM)

    def start_embedding(self, start: EmbeddingStart) -> EmbeddingRecorder:
        """Starts an embedding recorder."""

        self._assert_open()

        seed = copy.deepcopy(start)
        if seed.agent_name == "":
            agent_name = agent_name_from_context() or ""
            seed.agent_name = agent_name
        if seed.agent_name == "" and self._config.agent_name:
            seed.agent_name = self._config.agent_name
        if seed.agent_version == "":
            agent_version = agent_version_from_context() or ""
            seed.agent_version = agent_version
        if seed.agent_version == "" and self._config.agent_version:
            seed.agent_version = self._config.agent_version

        started_at = _to_utc(seed.started_at) if seed.started_at is not None else _to_utc(self._now())
        seed.started_at = started_at

        span = self._tracer.start_span(
            _embedding_span_name(seed.model.name),
            kind=SpanKind.CLIENT,
            start_time=_datetime_to_ns(started_at),
        )
        _set_embedding_start_span_attributes(span, seed)

        return EmbeddingRecorder(
            client=self,
            seed=seed,
            span=span,
            started_at=started_at,
        )

    def start_tool_execution(self, start: ToolExecutionStart) -> ToolExecutionRecorder:
        """Starts a tool execution recorder."""

        self._assert_open()

        seed = copy.deepcopy(start)
        seed.tool_name = seed.tool_name.strip()
        if seed.tool_name == "":
            return NoopToolExecutionRecorder()

        seed.conversation_title = seed.conversation_title.strip()
        if seed.conversation_id == "":
            conversation_id = conversation_id_from_context() or ""
            seed.conversation_id = conversation_id
        if seed.conversation_title == "":
            conversation_title = conversation_title_from_context() or ""
            seed.conversation_title = conversation_title.strip()
        if seed.agent_name == "":
            agent_name = agent_name_from_context() or ""
            seed.agent_name = agent_name
        if seed.agent_name == "" and self._config.agent_name:
            seed.agent_name = self._config.agent_name
        if seed.agent_version == "":
            agent_version = agent_version_from_context() or ""
            seed.agent_version = agent_version
        if seed.agent_version == "" and self._config.agent_version:
            seed.agent_version = self._config.agent_version

        started_at = _to_utc(seed.started_at) if seed.started_at is not None else _to_utc(self._now())
        seed.started_at = started_at

        # Resolve content capture before the span starts so MetadataOnly never
        # attaches sensitive content to live span attributes.
        resolver_mode = _call_content_capture_resolver(self._config.content_capture_resolver, {}, self._config.logger)
        effective_client_default = _resolve_content_capture_mode(resolver_mode, self._config.content_capture)
        ctx_mode = content_capture_mode_from_context()
        tool_mode = _resolve_tool_content_capture_mode(seed.content_capture, ctx_mode, effective_client_default)

        if tool_mode == ContentCaptureMode.METADATA_ONLY:
            seed.conversation_title = ""
            include_content = False
        elif tool_mode == ContentCaptureMode.FULL:
            include_content = True
        else:
            # NO_TOOL_CONTENT / DEFAULT: honor legacy include_content opt-in.
            include_content = seed.include_content

        span = self._tracer.start_span(
            _tool_span_name(seed.tool_name),
            kind=SpanKind.INTERNAL,
            start_time=_datetime_to_ns(started_at),
        )
        _set_tool_span_attributes(span, seed)

        return ToolExecutionRecorder(
            client=self,
            seed=seed,
            span=span,
            started_at=started_at,
            include_content=include_content,
        )

    def execute_tool_calls(
        self,
        messages: Sequence[Message],
        executor: Callable[[str, Any], Any],
        *,
        options: ExecuteToolCallsOptions | None = None,
    ) -> list[Message]:
        """Run each ``tool_call`` part under ``execute_tool`` spans and return tool messages.

        Walks *messages* (typically ``Generation.output``) and, for every
        :class:`~sigil_sdk.models.Part` with ``kind=TOOL_CALL``, calls *executor*
        as ``executor(tool_name, arguments)`` where *arguments* is JSON decoded
        from ``ToolCall.input_json`` (or ``{}`` when empty).

        Returns ``tool`` role :class:`~sigil_sdk.models.Message` values with
        ``tool_result`` parts suitable to append as the next model turn input.

        On executor failure, records :meth:`ToolExecutionRecorder.set_exec_error`
        and emits a ``tool_result`` with ``is_error=True``.

        ``ExecuteToolCallsOptions.tags`` is reserved for forward compatibility and
        is not applied to tool spans in this release.
        """

        self._assert_open()
        opts = options if options is not None else ExecuteToolCallsOptions()
        out: list[Message] = []

        for msg in messages or ():
            for part in getattr(msg, "parts", None) or ():
                if part.kind != PartKind.TOOL_CALL or part.tool_call is None:
                    continue
                tc = part.tool_call
                name = (tc.name or "").strip()
                if name == "":
                    continue
                call_id = (tc.id or "").strip()
                raw_args = tc.input_json or b""
                arguments: Any
                if not raw_args.strip():
                    arguments = {}
                else:
                    try:
                        arguments = json.loads(raw_args.decode("utf-8"))
                    except (json.JSONDecodeError, UnicodeDecodeError):
                        arguments = raw_args.decode("utf-8", errors="replace")

                rec = self.start_tool_execution(
                    ToolExecutionStart(
                        tool_name=name,
                        tool_call_id=call_id,
                        tool_type=opts.tool_type,
                        conversation_id=opts.conversation_id,
                        conversation_title=opts.conversation_title,
                        agent_name=opts.agent_name,
                        agent_version=opts.agent_version,
                        request_model=opts.request_model,
                        request_provider=opts.request_provider,
                        content_capture=opts.content_capture,
                    )
                )
                try:
                    result = executor(name, arguments)
                    rec.set_result(arguments=arguments, result=result)
                    out.append(_build_tool_result_message(name, call_id, result, is_error=False))
                except Exception as exc:  # noqa: BLE001
                    rec.set_exec_error(exc)
                    out.append(_build_tool_result_message(name, call_id, None, is_error=True, error_message=str(exc)))
                finally:
                    rec.end()

        return out

    def evaluate_hook(self, request: HookEvaluateRequest) -> HookEvaluateResponse:
        """Evaluates synchronous hook rules for the given request.

        Use this to enforce preflight (or postflight, when supported) guardrails
        on the LLM call's critical path. The server returns
        ``HookAction.DENY`` to block; callers typically translate that into a
        :class:`sigil_sdk.errors.HookDeniedError`.

        When ``hooks.enabled`` is ``False`` (the default) this short-circuits
        to ``allow``. When ``hooks.fail_open`` is ``True`` (the default),
        transport/decode failures also resolve to ``allow`` so the LLM call
        proceeds; set ``hooks.fail_open=False`` to surface
        :class:`sigil_sdk.errors.HookTransportError` instead.
        """

        self._assert_open()
        return _evaluate_hook(
            api_endpoint=self._config.api.endpoint,
            insecure=self._config.generation_export.insecure,
            extra_headers=self._config.generation_export.headers,
            hooks=self._config.hooks,
            request=request,
        )

    def submit_conversation_rating(
        self,
        conversation_id: str,
        rating: ConversationRatingInput,
    ) -> SubmitConversationRatingResponse:
        """Submits a user-facing conversation rating through Sigil HTTP API."""

        self._assert_open()

        normalized_conversation_id = conversation_id.strip()
        if normalized_conversation_id == "":
            raise ValidationError("sigil conversation rating validation failed: conversation_id is required")
        if len(normalized_conversation_id) > _max_rating_conversation_id_len:
            raise ValidationError("sigil conversation rating validation failed: conversation_id is too long")

        resolver_mode = _call_content_capture_resolver(
            self._config.content_capture_resolver, rating.metadata, self._config.logger
        )
        effective_mode = _resolve_content_capture_mode(
            resolver_mode, _resolve_client_content_capture_mode(self._config.content_capture)
        )
        if effective_mode == ContentCaptureMode.METADATA_ONLY:
            rating = replace(rating, comment="")

        normalized_rating = _normalize_conversation_rating_input(rating)
        endpoint = _conversation_rating_endpoint(
            self._config.api.endpoint,
            self._config.generation_export.insecure,
            normalized_conversation_id,
        )

        payload = {
            "rating_id": normalized_rating.rating_id,
            "rating": normalized_rating.rating.value,
        }
        if normalized_rating.comment != "":
            payload["comment"] = normalized_rating.comment
        if normalized_rating.metadata:
            payload["metadata"] = normalized_rating.metadata
        if normalized_rating.generation_id != "":
            payload["generation_id"] = normalized_rating.generation_id
        if normalized_rating.rater_id != "":
            payload["rater_id"] = normalized_rating.rater_id
        if normalized_rating.source != "":
            payload["source"] = normalized_rating.source

        body = json.dumps(payload).encode("utf-8")
        req = urllib_request.Request(
            endpoint,
            data=body,
            method="POST",
            headers={
                "Content-Type": "application/json",
                **self._config.generation_export.headers,
            },
        )

        raw: bytes
        status: int
        try:
            with urllib_request.urlopen(req, timeout=10) as response:
                status = response.getcode()
                raw = response.read()
        except urllib_error.HTTPError as exc:
            raw_error = exc.read().decode("utf-8", errors="replace").strip()
            if exc.code == 400:
                raise ValidationError(
                    f"sigil conversation rating validation failed: {_rating_error_text(raw_error, exc.code)}"
                ) from exc
            if exc.code == 409:
                raise RatingConflictError(
                    f"sigil conversation rating conflict: {_rating_error_text(raw_error, exc.code)}"
                ) from exc
            msg = _rating_error_text(raw_error, exc.code)
            raise RatingTransportError(f"sigil conversation rating transport failed: status {exc.code}: {msg}") from exc
        except Exception as exc:  # noqa: BLE001
            raise RatingTransportError(f"sigil conversation rating transport failed: {exc}") from exc

        if status < 200 or status >= 300:
            decoded = raw.decode("utf-8", errors="replace").strip()
            raise RatingTransportError(
                f"sigil conversation rating transport failed: status {status}: {_rating_error_text(decoded, status)}"
            )

        try:
            parsed = json.loads(raw.decode("utf-8"))
        except Exception as exc:  # noqa: BLE001
            raise RatingTransportError(
                f"sigil conversation rating transport failed: invalid JSON response: {exc}"
            ) from exc

        return _parse_submit_conversation_rating_response(parsed)

    def flush(self) -> None:
        """Flushes all queued generations immediately."""

        if self._shutting_down:
            raise ClientShutdownError("sigil: client is shutting down")
        self._flush_internal()

    def shutdown(self) -> None:
        """Flushes pending data and shuts down exporters."""

        with self._shutdown_lock:
            if self._closed:
                return
            self._shutting_down = True

            self._timer_stop.set()
            if self._timer_thread is not None:
                self._timer_thread.join(timeout=2)

            try:
                self._flush_internal()
            except Exception as exc:  # noqa: BLE001
                self._log_warn("sigil generation export flush on shutdown failed", exc)

            try:
                shutdown_fn = getattr(self._generation_exporter, "shutdown", None)
                if callable(shutdown_fn):
                    shutdown_fn()
            except Exception as exc:  # noqa: BLE001
                self._log_warn("sigil generation exporter shutdown failed", exc)

            self._closed = True

    def _start_generation(self, start: GenerationStart, default_mode: GenerationMode) -> GenerationRecorder:
        self._assert_open()

        seed = copy.deepcopy(start)
        if seed.mode is None:
            seed.mode = default_mode
        if seed.operation_name == "":
            seed.operation_name = _default_operation_name(seed.mode)

        seed.conversation_title = seed.conversation_title.strip()
        seed.user_id = seed.user_id.strip()
        if seed.conversation_id == "":
            seed.conversation_id = conversation_id_from_context() or ""
        if seed.conversation_title == "":
            seed.conversation_title = (conversation_title_from_context() or "").strip()
        if seed.user_id == "":
            seed.user_id = (user_id_from_context() or "").strip()
        if seed.user_id == "" and self._config.user_id:
            seed.user_id = self._config.user_id
        if seed.agent_name == "":
            seed.agent_name = agent_name_from_context() or ""
        if seed.agent_name == "" and self._config.agent_name:
            seed.agent_name = self._config.agent_name
        if seed.agent_version == "":
            seed.agent_version = agent_version_from_context() or ""
        if seed.agent_version == "" and self._config.agent_version:
            seed.agent_version = self._config.agent_version
        if self._config.tags:
            merged: dict[str, str] = dict(self._config.tags)
            merged.update(seed.tags)
            seed.tags = merged

        started_at = _to_utc(seed.started_at) if seed.started_at is not None else _to_utc(self._now())
        seed.started_at = started_at

        # Resolve content capture mode before the span starts so MetadataOnly
        # never attaches sensitive content to live span attributes.
        resolver_mode = _call_content_capture_resolver(
            self._config.content_capture_resolver, seed.metadata, self._config.logger
        )
        client_mode = _resolve_client_content_capture_mode(
            _resolve_content_capture_mode(resolver_mode, self._config.content_capture)
        )
        ctx_mode = content_capture_mode_from_context()
        if ctx_mode is not None:
            client_mode = _resolve_content_capture_mode(ctx_mode, client_mode)
        cc_mode = _resolve_content_capture_mode(seed.content_capture, client_mode)
        span_title = "" if cc_mode == ContentCaptureMode.METADATA_ONLY else seed.conversation_title

        span = self._tracer.start_span(
            _generation_span_name(seed.operation_name, seed.model.name),
            kind=SpanKind.CLIENT,
            start_time=_datetime_to_ns(started_at),
        )
        _set_generation_span_attributes(
            span,
            Generation(
                id=seed.id,
                conversation_id=seed.conversation_id,
                conversation_title=span_title,
                user_id=seed.user_id,
                agent_name=seed.agent_name,
                agent_version=seed.agent_version,
                mode=seed.mode,
                operation_name=seed.operation_name,
                model=copy.deepcopy(seed.model),
                max_tokens=seed.max_tokens,
                temperature=seed.temperature,
                top_p=seed.top_p,
                tool_choice=seed.tool_choice,
                thinking_enabled=seed.thinking_enabled,
                parent_generation_ids=list(seed.parent_generation_ids),
            ),
        )

        recorder = GenerationRecorder(
            client=self,
            seed=seed,
            span=span,
            started_at=started_at,
            _content_capture_mode=cc_mode,
        )
        _push_capture_mode(id(recorder), cc_mode)
        return recorder

    def _enqueue_generation(self, generation: Generation) -> None:
        if self._shutting_down or self._closed:
            raise ClientShutdownError("sigil: client is shutting down")

        max_payload_bytes = self._config.generation_export.payload_max_bytes
        if max_payload_bytes > 0:
            payload_size = generation_to_proto(generation).ByteSize()
            if payload_size > max_payload_bytes:
                raise EnqueueError(f"generation payload exceeds max bytes ({payload_size} > {max_payload_bytes})")

        should_trigger_flush = False
        with self._pending_lock:
            if len(self._pending_generations) >= self._config.generation_export.queue_size:
                raise QueueFullError("sigil: generation queue is full")
            self._pending_generations.append(copy.deepcopy(generation))
            if len(self._pending_generations) >= self._config.generation_export.batch_size:
                should_trigger_flush = True

        if should_trigger_flush:
            self._trigger_async_flush()

    def enqueue_workflow_step(self, step: WorkflowStep) -> None:
        """Enqueues a fully-built ``WorkflowStep`` for background export.

        This is a low-level primitive intended for advanced cases such as
        forwarding pre-built steps from another system. For general use, prefer
        a framework adapter (LangGraph, LangChain) or the upcoming
        ``Client.start_workflow_step`` recorder API, which automatically
        handles step IDs, timestamps, identity inheritance, and
        ``linked_generation_ids`` for nested generations.

        Design doc:
        ``sigil/docs/design-docs/2026-05-07-workflow-step-recorder-api.md``
        in the grafana/sigil repository.
        """
        if self._shutting_down or self._closed:
            raise ClientShutdownError("sigil: client is shutting down")
        should_trigger_flush = False
        with self._pending_lock:
            if len(self._pending_workflow_steps) >= self._config.generation_export.queue_size:
                raise QueueFullError("sigil: workflow step queue is full")
            self._pending_workflow_steps.append(copy.deepcopy(step))
            if len(self._pending_workflow_steps) >= self._config.generation_export.batch_size:
                should_trigger_flush = True
        if should_trigger_flush:
            self._trigger_async_flush()

    def _trigger_async_flush(self) -> None:
        with self._flush_thread_lock:
            if self._flush_thread is not None and self._flush_thread.is_alive():
                return
            self._flush_thread = threading.Thread(target=self._run_async_flush, daemon=True)
            self._flush_thread.start()

    def _run_async_flush(self) -> None:
        try:
            self._flush_internal()
        except Exception:  # noqa: BLE001
            pass

    def _has_generation_sanitizer(self) -> bool:
        return self._config.generation_sanitizer is not None

    def _sanitize_generation(self, generation: Generation) -> Generation:
        sanitizer = self._config.generation_sanitizer
        if sanitizer is None:
            return generation
        sanitized = sanitizer(copy.deepcopy(generation))
        if sanitized is None:
            raise ValueError("generation sanitizer must return a generation")
        return copy.deepcopy(sanitized)

    def _flush_internal(self) -> None:
        first_error: Exception | None = None
        with self._flush_lock:
            while True:
                with self._pending_lock:
                    has_gens = bool(self._pending_generations)
                    has_wfs = bool(self._pending_workflow_steps)
                    if not has_gens and not has_wfs:
                        break
                    batch_size = self._config.generation_export.batch_size

                    gen_batch: list[Generation] = []
                    if has_gens:
                        gen_batch = self._pending_generations[:batch_size]
                        del self._pending_generations[:batch_size]

                    wf_batch: list[WorkflowStep] = []
                    if has_wfs:
                        wf_batch = self._pending_workflow_steps[:batch_size]
                        del self._pending_workflow_steps[:batch_size]

                if gen_batch:
                    gen_request = ExportGenerationsRequest(generations=gen_batch)
                    try:
                        response = self._export_with_retry(
                            lambda req=gen_request: self._generation_exporter.export_generations(req)
                        )
                        for result in response.results:
                            if not result.accepted:
                                self._log_warn(
                                    f"sigil generation rejected id={result.generation_id} error={result.error}"
                                )
                    except Exception as exc:  # noqa: BLE001
                        self._log_warn("sigil generation export failed", exc)
                        first_error = first_error or exc

                if wf_batch:
                    wf_request = ExportWorkflowStepsRequest(workflow_steps=wf_batch)
                    try:
                        wf_response = self._export_with_retry(
                            lambda req=wf_request: self._generation_exporter.export_workflow_steps(req)
                        )
                        for result in wf_response.results:
                            if not result.accepted:
                                self._log_warn(f"sigil workflow step rejected id={result.step_id} error={result.error}")
                    except Exception as exc:  # noqa: BLE001
                        self._log_warn("sigil workflow step export failed", exc)
                        first_error = first_error or exc

                if first_error is not None:
                    break

        if first_error is not None:
            raise first_error

    def _export_with_retry(self, send: Callable[[], Any]):
        """Run an export call with the configured exponential backoff retry.

        ``send`` must be a zero-arg callable that performs the exporter call
        and returns the response. Re-raised on the final attempt."""
        attempts = self._config.generation_export.max_retries + 1
        backoff = self._config.generation_export.initial_backoff.total_seconds()
        max_backoff = self._config.generation_export.max_backoff.total_seconds()
        if backoff <= 0:
            backoff = 0.1

        last_error: Exception | None = None
        for attempt in range(attempts):
            try:
                return send()
            except Exception as exc:  # noqa: BLE001
                last_error = exc
                if attempt == attempts - 1:
                    break
                self._sleep(backoff)
                if backoff < max_backoff:
                    backoff *= 2
                    if backoff > max_backoff:
                        backoff = max_backoff

        assert last_error is not None
        raise last_error

    def _run_flush_timer(self) -> None:
        interval = self._config.generation_export.flush_interval.total_seconds()
        while not self._timer_stop.wait(interval):
            self._trigger_async_flush()

    def _assert_open(self) -> None:
        if self._closed:
            raise ClientShutdownError("sigil: client is shutting down")

    def _log_warn(self, message: str, error: Exception | None = None) -> None:
        if self._logger is None:
            return
        if error is None:
            self._logger.warning(message)
            return
        self._logger.warning("%s: %s", message, error)

    def _record_generation_metrics(
        self,
        generation: Generation,
        error_type: str,
        error_category: str,
        first_token_at: datetime | None,
    ) -> None:
        started_at = generation.started_at
        completed_at = generation.completed_at
        if started_at is None or completed_at is None:
            return

        duration_seconds = max((completed_at - started_at).total_seconds(), 0.0)
        identity_attributes = _metric_identity_attributes(
            generation.model.provider,
            generation.model.name,
            generation.agent_name,
            generation.agent_version,
        )
        self._operation_duration_histogram.record(
            duration_seconds,
            attributes={
                _span_attr_operation_name: generation.operation_name,
                **identity_attributes,
                _span_attr_error_type: error_type,
                _span_attr_error_category: error_category,
            },
        )

        usage = generation.usage
        self._record_token_usage(generation, _metric_token_type_input, usage.input_tokens)
        self._record_token_usage(generation, _metric_token_type_output, usage.output_tokens)
        self._record_token_usage(generation, _metric_token_type_cache_read, usage.cache_read_input_tokens)
        self._record_token_usage(generation, _metric_token_type_cache_write, usage.cache_write_input_tokens)
        self._record_token_usage(generation, _metric_token_type_reasoning, usage.reasoning_tokens)

        self._tool_calls_histogram.record(
            _count_tool_call_parts(generation.output),
            attributes={
                **identity_attributes,
            },
        )

        if generation.operation_name == _default_operation_name(GenerationMode.STREAM) and first_token_at is not None:
            ttft_seconds = (first_token_at - started_at).total_seconds()
            if ttft_seconds >= 0:
                self._ttft_histogram.record(
                    ttft_seconds,
                    attributes={
                        **identity_attributes,
                    },
                )

    def _record_embedding_metrics(
        self,
        seed: EmbeddingStart,
        result: EmbeddingResult,
        started_at: datetime,
        completed_at: datetime,
        error_type: str,
        error_category: str,
    ) -> None:
        duration_seconds = max((completed_at - started_at).total_seconds(), 0.0)
        identity_attributes = _metric_identity_attributes(
            seed.model.provider,
            seed.model.name,
            seed.agent_name,
            seed.agent_version,
        )
        self._operation_duration_histogram.record(
            duration_seconds,
            attributes={
                _span_attr_operation_name: _default_embedding_operation_name,
                **identity_attributes,
                _span_attr_error_type: error_type,
                _span_attr_error_category: error_category,
            },
        )

        if result.input_tokens != 0:
            self._token_usage_histogram.record(
                result.input_tokens,
                attributes={
                    _span_attr_operation_name: _default_embedding_operation_name,
                    **identity_attributes,
                    _metric_attr_token_type: _metric_token_type_input,
                },
            )

    def _record_token_usage(self, generation: Generation, token_type: str, value: int) -> None:
        if value == 0:
            return
        self._token_usage_histogram.record(
            value,
            attributes={
                _span_attr_operation_name: generation.operation_name,
                **_metric_identity_attributes(
                    generation.model.provider,
                    generation.model.name,
                    generation.agent_name,
                    generation.agent_version,
                ),
                _metric_attr_token_type: token_type,
            },
        )

    def _record_tool_execution_metrics(
        self,
        seed: ToolExecutionStart,
        started_at: datetime,
        completed_at: datetime,
        final_error: Exception | None,
    ) -> None:
        duration_seconds = max((completed_at - started_at).total_seconds(), 0.0)
        error_type = ""
        error_category = ""
        if final_error is not None:
            error_type = "tool_execution_error"
            error_category = _error_category_from_exception(final_error, fallback_sdk=True)

        self._operation_duration_histogram.record(
            duration_seconds,
            attributes={
                _span_attr_operation_name: "execute_tool",
                _span_attr_tool_name: seed.tool_name.strip(),
                **_metric_identity_attributes(
                    seed.request_provider,
                    seed.request_model,
                    seed.agent_name,
                    seed.agent_version,
                ),
                _span_attr_error_type: error_type,
                _span_attr_error_category: error_category,
            },
        )


@dataclass(slots=True)
class GenerationRecorder:
    """Recorder for one generation lifecycle."""

    client: Client
    seed: GenerationStart
    span: Span
    started_at: datetime

    _content_capture_mode: ContentCaptureMode = ContentCaptureMode.NO_TOOL_CONTENT
    _lock: threading.Lock = field(default_factory=threading.Lock, init=False, repr=False)
    _ended: bool = False
    _call_error: Exception | None = None
    _mapping_error: Exception | None = None
    _result: Generation | None = None
    _last_generation: Generation | None = None
    _final_error: Exception | None = None
    _first_token_at: datetime | None = None
    _extra_metadata: dict[str, Any] | None = field(default=None, repr=False)

    def __enter__(self) -> GenerationRecorder:
        return self

    def __exit__(self, exc_type, exc, _tb) -> bool:
        if exc is not None and self._call_error is None:
            self.set_call_error(exc)
        self.end()
        return False

    def set_call_error(self, error: Exception) -> None:
        """Records provider/runtime call error on this generation."""

        if error is None:
            return
        with self._lock:
            self._call_error = error

    def set_result(
        self, generation: Generation | None = None, mapping_error: Exception | None = None, **kwargs: Any
    ) -> None:
        """Stores mapped generation result and optional mapping error."""

        if generation is None:
            generation = Generation(**kwargs)
        elif kwargs:
            raise ValueError("set_result accepts either a generation object or keyword fields")

        with self._lock:
            self._result = copy.deepcopy(generation)
            if mapping_error is not None:
                self._mapping_error = mapping_error

    def set_first_token_at(self, first_token_at: datetime) -> None:
        """Records when the first streaming token/chunk arrived."""

        if first_token_at is None:
            return
        with self._lock:
            self._first_token_at = _to_utc(first_token_at)

    def set_cache_diagnostics(
        self,
        miss_reason: str,
        *,
        missed_input_tokens: int | None = None,
        previous_message_id: str | None = None,
    ) -> None:
        """Attach Anthropic-style cache diagnostic metadata before ``end()``."""

        miss_reason = (miss_reason or "").strip()
        if not miss_reason:
            return
        with self._lock:
            if self._ended:
                return
            if self._extra_metadata is None:
                self._extra_metadata = {}
            self._extra_metadata.pop(CACHE_DIAGNOSTICS_MISSED_INPUT_TOKENS_KEY, None)
            self._extra_metadata.pop(CACHE_DIAGNOSTICS_PREVIOUS_MESSAGE_ID_KEY, None)
            self._extra_metadata[CACHE_DIAGNOSTICS_MISS_REASON_KEY] = miss_reason
            if missed_input_tokens is not None:
                self._extra_metadata[CACHE_DIAGNOSTICS_MISSED_INPUT_TOKENS_KEY] = str(int(missed_input_tokens))
            prev = (previous_message_id or "").strip()
            if prev:
                self._extra_metadata[CACHE_DIAGNOSTICS_PREVIOUS_MESSAGE_ID_KEY] = prev

    def end(self) -> None:
        """Finalizes span and queues generation export. Safe to call multiple times."""

        with self._lock:
            if self._ended:
                return
            self._ended = True
            call_error = self._call_error
            mapping_error = self._mapping_error
            result = copy.deepcopy(self._result) if self._result is not None else Generation()
            first_token_at = self._first_token_at
            extra_metadata = copy.deepcopy(self._extra_metadata) if self._extra_metadata else None

        try:
            completed_at = _to_utc(self.client._now())
            generation = self._normalize_generation(result, completed_at, call_error, extra_metadata)
            _apply_trace_context_from_span(self.span, generation)

            effective_content_capture_mode = self._content_capture_mode
            _stamp_content_capture_metadata(generation, self._content_capture_mode)
            validation_target = copy.deepcopy(generation)
            if self._content_capture_mode == ContentCaptureMode.METADATA_ONLY:
                error_cat = _error_category_from_exception(call_error, fallback_sdk=True) if call_error else ""
                _strip_content(generation, error_cat)
            elif self.client._has_generation_sanitizer():
                try:
                    generation = self.client._sanitize_generation(generation)
                    validation_target = copy.deepcopy(generation)
                except Exception:  # noqa: BLE001
                    effective_content_capture_mode = ContentCaptureMode.METADATA_ONLY
                    error_cat = _error_category_from_exception(call_error, fallback_sdk=True) if call_error else ""
                    _strip_content(generation, error_cat)
                    _stamp_content_capture_metadata(generation, effective_content_capture_mode)
                    if self.client._config.logger is not None:
                        self.client._config.logger.warning(
                            "sigil: generation sanitization failed, falling back to metadata_only",
                            exc_info=True,
                        )

            self.span.update_name(_generation_span_name(generation.operation_name, generation.model.name))
            _set_generation_span_attributes(self.span, generation)
            if (
                effective_content_capture_mode == ContentCaptureMode.METADATA_ONLY
                and self._content_capture_mode != ContentCaptureMode.METADATA_ONLY
            ):
                self.span.set_attribute(_span_attr_conversation_title, "")

            local_error: Exception | None = None
            try:
                validate_generation(validation_target)
            except Exception as exc:  # noqa: BLE001
                local_error = ValidationError(f"sigil: generation validation failed: {exc}")

            if local_error is None:
                try:
                    self.client._enqueue_generation(generation)
                except QueueFullError as exc:
                    local_error = exc
                except ClientShutdownError as exc:
                    local_error = exc
                except Exception as exc:  # noqa: BLE001
                    local_error = EnqueueError(f"sigil: generation enqueue failed: {exc}")

            is_metadata_only = effective_content_capture_mode == ContentCaptureMode.METADATA_ONLY

            if not is_metadata_only:
                if call_error is not None:
                    self.span.record_exception(call_error)
                if mapping_error is not None:
                    self.span.record_exception(mapping_error)
            # SDK-internal errors (validation/enqueue) contain no user content — always record.
            if local_error is not None:
                self.span.record_exception(local_error)

            error_type = ""
            error_category = ""
            if call_error is not None:
                error_type = "provider_call_error"
                error_category = _error_category_from_exception(call_error, fallback_sdk=True)
                self.span.set_attribute(_span_attr_error_type, error_type)
                self.span.set_attribute(_span_attr_error_category, error_category)
                self.span.set_status(Status(StatusCode.ERROR, error_category if is_metadata_only else str(call_error)))
            elif mapping_error is not None:
                error_type = "mapping_error"
                error_category = "sdk_error"
                self.span.set_attribute(_span_attr_error_type, error_type)
                self.span.set_attribute(_span_attr_error_category, error_category)
                self.span.set_status(
                    Status(StatusCode.ERROR, error_category if is_metadata_only else str(mapping_error))
                )
            elif local_error is not None:
                error_type = "validation_error" if isinstance(local_error, ValidationError) else "enqueue_error"
                error_category = "sdk_error"
                self.span.set_attribute(_span_attr_error_type, error_type)
                self.span.set_attribute(_span_attr_error_category, error_category)
                self.span.set_status(Status(StatusCode.ERROR, error_category if is_metadata_only else str(local_error)))
            else:
                self.span.set_status(Status(StatusCode.OK))

            with use_span(self.span, end_on_exit=False):
                self.client._record_generation_metrics(generation, error_type, error_category, first_token_at)

            self.span.end(end_time=_datetime_to_ns(generation.completed_at or completed_at))

            with self._lock:
                self._last_generation = copy.deepcopy(generation)
                self._final_error = local_error
        finally:
            _pop_capture_mode(id(self))

    def err(self) -> Exception | None:
        """Returns local validation/enqueue error after `end()`."""

        with self._lock:
            return self._final_error

    @property
    def last_generation(self) -> Generation | None:
        """Returns the normalized generation payload after end for tests/debug."""

        with self._lock:
            return copy.deepcopy(self._last_generation)

    def _normalize_generation(
        self,
        raw: Generation,
        completed_at: datetime,
        call_error: Exception | None,
        extra_metadata: dict[str, Any] | None = None,
    ) -> Generation:
        generation = copy.deepcopy(raw)

        if generation.id == "":
            generation.id = self.seed.id
        if generation.id == "":
            generation.id = _new_random_id("gen")

        if generation.conversation_id == "":
            generation.conversation_id = self.seed.conversation_id
        if generation.conversation_title == "":
            generation.conversation_title = self.seed.conversation_title
        if generation.user_id == "":
            generation.user_id = self.seed.user_id
        if generation.agent_name == "":
            generation.agent_name = self.seed.agent_name
        if generation.agent_version == "":
            generation.agent_version = self.seed.agent_version

        if generation.mode is None:
            generation.mode = self.seed.mode
        if generation.mode is None:
            generation.mode = GenerationMode.SYNC

        if generation.operation_name == "":
            generation.operation_name = self.seed.operation_name
        if generation.operation_name == "":
            generation.operation_name = _default_operation_name(generation.mode)

        if generation.model.provider == "":
            generation.model.provider = self.seed.model.provider
        if generation.model.name == "":
            generation.model.name = self.seed.model.name

        if generation.system_prompt == "":
            generation.system_prompt = self.seed.system_prompt

        if generation.max_tokens is None:
            generation.max_tokens = self.seed.max_tokens
        if generation.temperature is None:
            generation.temperature = self.seed.temperature
        if generation.top_p is None:
            generation.top_p = self.seed.top_p
        if generation.tool_choice is None:
            generation.tool_choice = self.seed.tool_choice
        if generation.thinking_enabled is None:
            generation.thinking_enabled = self.seed.thinking_enabled

        if len(generation.tools) == 0:
            generation.tools = copy.deepcopy(self.seed.tools)

        if len(generation.parent_generation_ids) == 0:
            generation.parent_generation_ids = list(self.seed.parent_generation_ids)

        if not generation.effective_version.strip():
            generation.effective_version = self.seed.effective_version

        merged_tags = dict(self.seed.tags)
        merged_tags.update(generation.tags)
        generation.tags = merged_tags

        merged_metadata: dict[str, Any] = dict(self.seed.metadata)
        merged_metadata.update(generation.metadata)
        if extra_metadata:
            merged_metadata.update(extra_metadata)
        generation.metadata = merged_metadata

        conversation_title = generation.conversation_title.strip()
        if conversation_title == "":
            conversation_title = _metadata_string_value(generation.metadata, _span_attr_conversation_title) or ""
        generation.conversation_title = conversation_title
        if conversation_title != "":
            generation.metadata[_span_attr_conversation_title] = conversation_title

        user_id = generation.user_id.strip()
        if user_id == "":
            user_id = _metadata_string_value(generation.metadata, _metadata_user_id_key) or ""
        if user_id == "":
            user_id = _metadata_string_value(generation.metadata, _metadata_legacy_user_id_key) or ""
        generation.user_id = user_id
        if user_id != "":
            generation.metadata[_metadata_user_id_key] = user_id

        generation.started_at = _to_utc(generation.started_at) if generation.started_at is not None else self.started_at
        generation.completed_at = (
            _to_utc(generation.completed_at) if generation.completed_at is not None else completed_at
        )

        if call_error is not None:
            if generation.call_error == "":
                generation.call_error = str(call_error)
            generation.metadata["call_error"] = str(call_error)

        generation.metadata[_span_attr_sdk_name] = _sdk_name
        generation.usage = generation.usage.normalize()
        return generation


@dataclass(slots=True)
class EmbeddingRecorder:
    """Recorder for one embedding lifecycle."""

    client: Client
    seed: EmbeddingStart
    span: Span
    started_at: datetime

    _lock: threading.Lock = field(default_factory=threading.Lock, init=False, repr=False)
    _ended: bool = False
    _call_error: Exception | None = None
    _result: EmbeddingResult = field(default_factory=EmbeddingResult)
    _has_result: bool = False
    _final_error: Exception | None = None

    def __enter__(self) -> EmbeddingRecorder:
        return self

    def __exit__(self, exc_type, exc, _tb) -> bool:
        if exc is not None and self._call_error is None:
            self.set_call_error(exc)
        self.end()
        return False

    def set_call_error(self, error: Exception) -> None:
        """Records provider/runtime call error on this embedding lifecycle."""

        if error is None:
            return
        with self._lock:
            self._call_error = error

    def set_result(self, result: EmbeddingResult | None = None, **kwargs: Any) -> None:
        """Stores mapped embedding result payload."""

        payload = EmbeddingResult(**kwargs) if result is None else result
        with self._lock:
            self._result = copy.deepcopy(payload)
            self._has_result = True

    def end(self) -> None:
        """Finalizes embedding span lifecycle. Safe to call multiple times."""

        with self._lock:
            if self._ended:
                return
            self._ended = True
            call_error = self._call_error
            result = copy.deepcopy(self._result)
            has_result = self._has_result

        completed_at = _to_utc(self.client._now())
        self.span.update_name(_embedding_span_name(self.seed.model.name))
        _set_embedding_end_span_attributes(self.span, result, has_result, self.client._config.embedding_capture)

        local_error: Exception | None = None
        try:
            validate_embedding_start(self.seed)
        except Exception as exc:  # noqa: BLE001
            local_error = ValidationError(f"sigil: embedding validation failed: {exc}")

        if local_error is None:
            try:
                validate_embedding_result(result)
            except Exception as exc:  # noqa: BLE001
                local_error = ValidationError(f"sigil: embedding validation failed: {exc}")

        if call_error is not None:
            self.span.record_exception(call_error)
        if local_error is not None:
            self.span.record_exception(local_error)

        error_type = ""
        error_category = ""
        if call_error is not None:
            error_type = "provider_call_error"
            error_category = _error_category_from_exception(call_error, fallback_sdk=True)
            self.span.set_status(Status(StatusCode.ERROR, str(call_error)))
        elif local_error is not None:
            error_type = "validation_error"
            error_category = "sdk_error"
            self.span.set_status(Status(StatusCode.ERROR, str(local_error)))
        else:
            self.span.set_status(Status(StatusCode.OK))

        if error_type != "":
            self.span.set_attribute(_span_attr_error_type, error_type)
            self.span.set_attribute(_span_attr_error_category, error_category)

        with use_span(self.span, end_on_exit=False):
            self.client._record_embedding_metrics(
                self.seed,
                result,
                self.started_at,
                completed_at,
                error_type,
                error_category,
            )
        self.span.end(end_time=_datetime_to_ns(completed_at))

        with self._lock:
            self._final_error = local_error

    def err(self) -> Exception | None:
        """Returns local validation error after `end()`."""

        with self._lock:
            return self._final_error


@dataclass(slots=True)
class ToolExecutionRecorder:
    """Recorder for one tool execution lifecycle."""

    client: Client
    seed: ToolExecutionStart
    span: Span
    started_at: datetime
    include_content: bool

    _lock: threading.Lock = field(default_factory=threading.Lock, init=False, repr=False)
    _ended: bool = False
    _exec_error: Exception | None = None
    _result: ToolExecutionEnd = field(default_factory=ToolExecutionEnd)
    _has_result: bool = False
    _final_error: Exception | None = None

    def __enter__(self) -> ToolExecutionRecorder:
        return self

    def __exit__(self, exc_type, exc, _tb) -> bool:
        if exc is not None and self._exec_error is None:
            self.set_exec_error(exc)
        self.end()
        return False

    def set_exec_error(self, error: Exception) -> None:
        """Records a tool execution error."""

        if error is None:
            return
        with self._lock:
            self._exec_error = error

    def set_result(self, end: ToolExecutionEnd | None = None, **kwargs: Any) -> None:
        """Stores tool arguments/result payload."""

        payload = ToolExecutionEnd(**kwargs) if end is None else end
        with self._lock:
            self._result = copy.deepcopy(payload)
            self._has_result = True

    def end(self) -> None:
        """Finalizes the execute_tool span. Safe to call multiple times."""

        with self._lock:
            if self._ended:
                return
            self._ended = True
            exec_error = self._exec_error
            result = copy.deepcopy(self._result)

        completed_at = _to_utc(result.completed_at) if result.completed_at is not None else _to_utc(self.client._now())

        self.span.update_name(_tool_span_name(self.seed.tool_name))
        _set_tool_span_attributes(self.span, self.seed)

        content_error: Exception | None = None
        if self.include_content:
            try:
                arguments = _serialize_tool_content(result.arguments)
                if arguments != "":
                    self.span.set_attribute(_span_attr_tool_call_arguments, arguments)
            except Exception as exc:  # noqa: BLE001
                content_error = RuntimeError(f"serialize tool arguments: {exc}")

            try:
                tool_result = _serialize_tool_content(result.result)
                if tool_result != "":
                    self.span.set_attribute(_span_attr_tool_call_result, tool_result)
            except Exception as exc:  # noqa: BLE001
                if content_error is None:
                    content_error = RuntimeError(f"serialize tool result: {exc}")

        final_error: Exception | None = None
        if exec_error is not None and content_error is not None:
            final_error = RuntimeError(f"{exec_error}; {content_error}")
        elif exec_error is not None:
            final_error = exec_error
        elif content_error is not None:
            final_error = content_error

        if final_error is not None:
            self.span.record_exception(final_error)
            self.span.set_attribute(_span_attr_error_type, "tool_execution_error")
            self.span.set_attribute(
                _span_attr_error_category, _error_category_from_exception(final_error, fallback_sdk=True)
            )
            self.span.set_status(Status(StatusCode.ERROR, str(final_error)))
        else:
            self.span.set_status(Status(StatusCode.OK))

        with use_span(self.span, end_on_exit=False):
            self.client._record_tool_execution_metrics(self.seed, self.started_at, completed_at, final_error)

        self.span.end(end_time=_datetime_to_ns(completed_at))

        with self._lock:
            self._final_error = final_error

    def err(self) -> Exception | None:
        """Returns execute_tool finalization error after `end()`."""

        with self._lock:
            return self._final_error


class NoopToolExecutionRecorder:
    """No-op tool recorder returned for empty tool names."""

    def __enter__(self) -> NoopToolExecutionRecorder:
        return self

    def __exit__(self, exc_type, exc, tb) -> bool:  # noqa: ARG002
        return False

    def set_exec_error(self, error: Exception) -> None:  # noqa: ARG002
        return

    def set_result(self, end: ToolExecutionEnd | None = None, **kwargs: Any) -> None:  # noqa: ARG002
        return

    def end(self) -> None:
        return

    def err(self) -> Exception | None:
        return None


def _apply_trace_context_from_span(span: Span, generation: Generation) -> None:
    context = span.get_span_context()
    if context is None:
        return

    if context.trace_id:
        generation.trace_id = f"{context.trace_id:032x}"
    if context.span_id:
        generation.span_id = f"{context.span_id:016x}"


def _generation_span_name(operation_name: str, model_name: str) -> str:
    operation = operation_name.strip()
    if operation == "":
        operation = _default_operation_name(GenerationMode.SYNC)

    model = model_name.strip()
    if model == "":
        return operation
    return f"{operation} {model}"


def _embedding_span_name(model_name: str) -> str:
    model = model_name.strip()
    if model == "":
        return _default_embedding_operation_name
    return f"{_default_embedding_operation_name} {model}"


def _tool_span_name(tool_name: str) -> str:
    name = tool_name.strip() or "unknown"
    return f"execute_tool {name}"


def _set_generation_span_attributes(span: Span, generation: Generation) -> None:
    span.set_attribute(_span_attr_operation_name, generation.operation_name or _default_operation_name(generation.mode))
    span.set_attribute(_span_attr_sdk_name, _sdk_name)

    if generation.id:
        span.set_attribute(_span_attr_generation_id, generation.id)
    if generation.conversation_id:
        span.set_attribute(_span_attr_conversation_id, generation.conversation_id)
    if generation.conversation_title:
        span.set_attribute(_span_attr_conversation_title, generation.conversation_title)
    if generation.user_id:
        span.set_attribute(_span_attr_user_id, generation.user_id)
    if generation.agent_name:
        span.set_attribute(_span_attr_agent_name, generation.agent_name)
    if generation.agent_version:
        span.set_attribute(_span_attr_agent_version, generation.agent_version)
    if generation.model.provider:
        span.set_attribute(_span_attr_provider_name, generation.model.provider)
    if generation.model.name:
        span.set_attribute(_span_attr_request_model, generation.model.name)
    if generation.max_tokens is not None:
        span.set_attribute(_span_attr_request_max_tokens, generation.max_tokens)
    if generation.temperature is not None:
        span.set_attribute(_span_attr_request_temperature, generation.temperature)
    if generation.top_p is not None:
        span.set_attribute(_span_attr_request_top_p, generation.top_p)
    if generation.tool_choice is not None:
        tool_choice = generation.tool_choice.strip()
        if tool_choice != "":
            span.set_attribute(_span_attr_request_tool_choice, tool_choice)
    if generation.thinking_enabled is not None:
        span.set_attribute(_span_attr_request_thinking_enabled, generation.thinking_enabled)
    thinking_budget = _thinking_budget_from_metadata(generation.metadata)
    if thinking_budget is not None:
        span.set_attribute(_span_attr_request_thinking_budget, thinking_budget)
    framework_run_id = _metadata_string_value(generation.metadata, _span_attr_framework_run_id)
    if framework_run_id is not None:
        span.set_attribute(_span_attr_framework_run_id, framework_run_id)
    framework_thread_id = _metadata_string_value(generation.metadata, _span_attr_framework_thread_id)
    if framework_thread_id is not None:
        span.set_attribute(_span_attr_framework_thread_id, framework_thread_id)
    framework_parent_run_id = _metadata_string_value(generation.metadata, _span_attr_framework_parent_run_id)
    if framework_parent_run_id is not None:
        span.set_attribute(_span_attr_framework_parent_run_id, framework_parent_run_id)
    framework_component_name = _metadata_string_value(generation.metadata, _span_attr_framework_component_name)
    if framework_component_name is not None:
        span.set_attribute(_span_attr_framework_component_name, framework_component_name)
    framework_run_type = _metadata_string_value(generation.metadata, _span_attr_framework_run_type)
    if framework_run_type is not None:
        span.set_attribute(_span_attr_framework_run_type, framework_run_type)
    framework_retry_attempt = _metadata_int_value(generation.metadata, _span_attr_framework_retry_attempt)
    if framework_retry_attempt is not None:
        span.set_attribute(_span_attr_framework_retry_attempt, framework_retry_attempt)
    framework_langgraph_node = _metadata_string_value(generation.metadata, _span_attr_framework_langgraph_node)
    if framework_langgraph_node is not None:
        span.set_attribute(_span_attr_framework_langgraph_node, framework_langgraph_node)
    framework_event_id = _metadata_string_value(generation.metadata, _span_attr_framework_event_id)
    if framework_event_id is not None:
        span.set_attribute(_span_attr_framework_event_id, framework_event_id)
    if generation.parent_generation_ids:
        span.set_attribute(_span_attr_parent_generation_ids, list(generation.parent_generation_ids))
    if generation.response_id:
        span.set_attribute(_span_attr_response_id, generation.response_id)
    if generation.response_model:
        span.set_attribute(_span_attr_response_model, generation.response_model)
    if generation.stop_reason:
        span.set_attribute(_span_attr_finish_reasons, [generation.stop_reason])

    usage = generation.usage
    if usage.input_tokens:
        span.set_attribute(_span_attr_input_tokens, usage.input_tokens)
    if usage.output_tokens:
        span.set_attribute(_span_attr_output_tokens, usage.output_tokens)
    if usage.cache_read_input_tokens:
        span.set_attribute(_span_attr_cache_read_tokens, usage.cache_read_input_tokens)
    if usage.cache_write_input_tokens:
        span.set_attribute(_span_attr_cache_write_tokens, usage.cache_write_input_tokens)
    if usage.reasoning_tokens:
        span.set_attribute(_span_attr_reasoning_tokens, usage.reasoning_tokens)


def _set_embedding_start_span_attributes(span: Span, start: EmbeddingStart) -> None:
    span.set_attribute(_span_attr_operation_name, _default_embedding_operation_name)
    span.set_attribute(_span_attr_sdk_name, _sdk_name)

    if start.model.provider:
        span.set_attribute(_span_attr_provider_name, start.model.provider)
    if start.model.name:
        span.set_attribute(_span_attr_request_model, start.model.name)
    if start.agent_name:
        span.set_attribute(_span_attr_agent_name, start.agent_name)
    if start.agent_version:
        span.set_attribute(_span_attr_agent_version, start.agent_version)
    if start.dimensions is not None:
        span.set_attribute(_span_attr_embedding_dim_count, start.dimensions)
    if start.encoding_format.strip() != "":
        span.set_attribute(_span_attr_request_encoding_formats, [start.encoding_format.strip()])


def _set_embedding_end_span_attributes(
    span: Span,
    result: EmbeddingResult,
    has_result: bool,
    capture_config,
) -> None:
    if has_result:
        span.set_attribute(_span_attr_embedding_input_count, result.input_count)
    if result.input_tokens != 0:
        span.set_attribute(_span_attr_input_tokens, result.input_tokens)
    if result.response_model:
        span.set_attribute(_span_attr_response_model, result.response_model)
    if result.dimensions is not None:
        span.set_attribute(_span_attr_embedding_dim_count, result.dimensions)
    if capture_config.capture_input and result.input_texts:
        texts = _capture_embedding_input_texts(
            result.input_texts,
            capture_config.max_input_items,
            capture_config.max_text_length,
        )
        if texts:
            span.set_attribute(_span_attr_embedding_input_texts, texts)


def _set_tool_span_attributes(span: Span, start: ToolExecutionStart) -> None:
    span.set_attribute(_span_attr_operation_name, "execute_tool")
    span.set_attribute(_span_attr_tool_name, start.tool_name)
    span.set_attribute(_span_attr_sdk_name, _sdk_name)

    if start.tool_call_id:
        span.set_attribute(_span_attr_tool_call_id, start.tool_call_id)
    if start.tool_type:
        span.set_attribute(_span_attr_tool_type, start.tool_type)
    if start.tool_description:
        span.set_attribute(_span_attr_tool_description, start.tool_description)
    if start.conversation_id:
        span.set_attribute(_span_attr_conversation_id, start.conversation_id)
    if start.conversation_title:
        span.set_attribute(_span_attr_conversation_title, start.conversation_title)
    if start.agent_name:
        span.set_attribute(_span_attr_agent_name, start.agent_name)
    if start.agent_version:
        span.set_attribute(_span_attr_agent_version, start.agent_version)
    if start.request_provider:
        span.set_attribute(_span_attr_provider_name, start.request_provider)
    if start.request_model:
        span.set_attribute(_span_attr_request_model, start.request_model)


def _thinking_budget_from_metadata(metadata: dict[str, Any]) -> int | None:
    if not metadata:
        return None

    raw = metadata.get(_span_attr_request_thinking_budget)
    if raw is None or isinstance(raw, bool):
        return None

    if isinstance(raw, int):
        return raw

    if isinstance(raw, float):
        integer = int(raw)
        if float(integer) == raw:
            return integer
        return None

    if isinstance(raw, str):
        text = raw.strip()
        if text == "":
            return None
        try:
            return int(text)
        except ValueError:
            return None

    return None


def _metadata_string_value(metadata: dict[str, Any], key: str) -> str | None:
    if not metadata:
        return None

    raw = metadata.get(key)
    if not isinstance(raw, str):
        return None

    text = raw.strip()
    return text if text != "" else None


def _metadata_int_value(metadata: dict[str, Any], key: str) -> int | None:
    if not metadata:
        return None

    raw = metadata.get(key)
    if raw is None or isinstance(raw, bool):
        return None

    if isinstance(raw, int):
        return raw

    if isinstance(raw, float):
        integer = int(raw)
        if float(integer) == raw:
            return integer
        return None

    if isinstance(raw, str):
        text = raw.strip()
        if text == "":
            return None
        try:
            return int(text)
        except ValueError:
            return None

    return None


def _default_operation_name(mode: GenerationMode | None) -> str:
    if mode == GenerationMode.STREAM:
        return "streamText"
    return "generateText"


def _metric_identity_attributes(
    provider: str | None,
    model: str | None,
    agent_name: str | None,
    agent_version: str | None,
) -> dict[str, str]:
    attrs = {
        _span_attr_provider_name: provider.strip() if provider else "",
        _span_attr_request_model: model.strip() if model else "",
        _span_attr_agent_name: agent_name.strip() if agent_name else "",
    }
    if agent_version and agent_version.strip():
        attrs[_span_attr_agent_version] = agent_version.strip()
    return attrs


def _new_random_id(prefix: str) -> str:
    return f"{prefix}_{secrets.token_hex(8)}"


def _to_utc(value: datetime) -> datetime:
    if value.tzinfo is None:
        return value.replace(tzinfo=timezone.utc)
    return value.astimezone(timezone.utc)


def _datetime_to_ns(value: datetime) -> int:
    return int(_to_utc(value).timestamp() * 1_000_000_000)


def _serialize_tool_content(value: Any) -> str:
    if value is None:
        return ""
    if isinstance(value, str):
        trimmed = value.strip()
        if trimmed == "":
            return ""
        try:
            json.loads(trimmed)
            return trimmed
        except Exception:  # noqa: BLE001
            return json.dumps(trimmed)
    if isinstance(value, bytes):
        trimmed = value.decode("utf-8", errors="replace").strip()
        if trimmed == "":
            return ""
        try:
            json.loads(trimmed)
            return trimmed
        except Exception:  # noqa: BLE001
            return json.dumps(trimmed)
    return json.dumps(value)


def _capture_embedding_input_texts(input_texts: list[str], max_input_items: int, max_text_length: int) -> list[str]:
    if not input_texts:
        return []

    item_limit = max_input_items if max_input_items > 0 else 20
    text_limit = max_text_length if max_text_length > 0 else 1024

    out: list[str] = []
    for raw_text in input_texts[:item_limit]:
        text = raw_text if isinstance(raw_text, str) else str(raw_text)
        out.append(_truncate_embedding_text(text, text_limit))
    return out


def _truncate_embedding_text(text: str, max_text_length: int) -> str:
    if len(text) <= max_text_length:
        return text
    if max_text_length <= 3:
        return text[:max_text_length]
    return text[: max_text_length - 3] + "..."


def _count_tool_call_parts(messages: list[Message]) -> int:
    total = 0
    for message in messages:
        for part in message.parts:
            if part.kind == PartKind.TOOL_CALL:
                total += 1
    return total


def _error_category_from_exception(error: Exception | str | None, fallback_sdk: bool) -> str:
    if error is None:
        return "sdk_error" if fallback_sdk else ""
    if isinstance(error, str):
        return _classify_error_category(_extract_status_code_from_message(error), error, fallback_sdk)

    status_code = _extract_status_code_from_exception(error)
    message = str(error)
    if isinstance(error, TimeoutError):
        return "timeout"
    return _classify_error_category(status_code, message, fallback_sdk)


def _classify_error_category(status_code: int | None, message: str, fallback_sdk: bool) -> str:
    message_lower = message.lower()
    if "timeout" in message_lower or "deadline exceeded" in message_lower:
        return "timeout"
    if status_code == 429:
        return "rate_limit"
    if status_code in (401, 403):
        return "auth_error"
    if status_code == 408:
        return "timeout"
    if status_code is not None and 500 <= status_code <= 599:
        return "server_error"
    if status_code is not None and 400 <= status_code <= 499:
        return "client_error"
    if fallback_sdk:
        return "sdk_error"
    return ""


def _extract_status_code_from_exception(error: Exception) -> int | None:
    for attr in ("status", "status_code", "Status", "StatusCode"):
        parsed = _as_status_code(getattr(error, attr, None))
        if parsed is not None:
            return parsed

    response = getattr(error, "response", None)
    if response is not None:
        for attr in ("status", "status_code", "Status", "StatusCode"):
            parsed = _as_status_code(getattr(response, attr, None))
            if parsed is not None:
                return parsed

    inner_error = getattr(error, "error", None)
    if inner_error is not None:
        for field in ("status", "status_code", "Status", "StatusCode"):
            parsed = _as_status_code(getattr(inner_error, field, None))
            if parsed is not None:
                return parsed

    return _extract_status_code_from_message(str(error))


def _extract_status_code_from_message(message: str) -> int | None:
    matches = _status_code_pattern.findall(message)
    for match in matches:
        parsed = _as_status_code(match)
        if parsed is not None:
            return parsed
    return None


def _as_status_code(value: Any) -> int | None:
    if isinstance(value, bool):
        return None
    if isinstance(value, int):
        parsed = value
    elif isinstance(value, str):
        stripped = value.strip()
        if stripped == "":
            return None
        try:
            parsed = int(stripped)
        except ValueError:
            return None
    else:
        return None
    if 100 <= parsed <= 599:
        return parsed
    return None


def _normalize_conversation_rating_input(input_value: ConversationRatingInput) -> ConversationRatingInput:
    rating_id = input_value.rating_id.strip()
    if rating_id == "":
        raise ValidationError("sigil conversation rating validation failed: rating_id is required")
    if len(rating_id) > _max_rating_id_len:
        raise ValidationError("sigil conversation rating validation failed: rating_id is too long")

    rating_value = (
        input_value.rating.value if isinstance(input_value.rating, ConversationRatingValue) else str(input_value.rating)
    ).strip()
    if rating_value not in {
        ConversationRatingValue.GOOD.value,
        ConversationRatingValue.BAD.value,
    }:
        raise ValidationError(
            "sigil conversation rating validation failed:"
            " rating must be CONVERSATION_RATING_VALUE_GOOD or CONVERSATION_RATING_VALUE_BAD"
        )

    comment = input_value.comment.strip()
    if len(comment.encode("utf-8")) > _max_rating_comment_bytes:
        raise ValidationError("sigil conversation rating validation failed: comment is too long")

    generation_id = input_value.generation_id.strip()
    if len(generation_id) > _max_rating_generation_id_len:
        raise ValidationError("sigil conversation rating validation failed: generation_id is too long")

    rater_id = input_value.rater_id.strip()
    if len(rater_id) > _max_rating_actor_id_len:
        raise ValidationError("sigil conversation rating validation failed: rater_id is too long")

    source = input_value.source.strip()
    if len(source) > _max_rating_source_len:
        raise ValidationError("sigil conversation rating validation failed: source is too long")

    metadata = dict(input_value.metadata or {})
    if metadata:
        encoded = json.dumps(metadata)
        if len(encoded.encode("utf-8")) > _max_rating_metadata_bytes:
            raise ValidationError("sigil conversation rating validation failed: metadata is too large")

    return ConversationRatingInput(
        rating_id=rating_id,
        rating=ConversationRatingValue(rating_value),
        comment=comment,
        metadata=metadata,
        generation_id=generation_id,
        rater_id=rater_id,
        source=source,
    )


def _conversation_rating_endpoint(endpoint: str, insecure: bool, conversation_id: str) -> str:
    base_url = _base_url_from_api_endpoint(endpoint, insecure)
    return f"{base_url}/api/v1/conversations/{urllib_parse.quote(conversation_id, safe='')}/ratings"


def _base_url_from_api_endpoint(endpoint: str, insecure: bool) -> str:
    trimmed = endpoint.strip()
    if trimmed == "":
        raise RatingTransportError("sigil conversation rating transport failed: api endpoint is required")

    if trimmed.startswith("http://") or trimmed.startswith("https://"):
        parsed = urllib_parse.urlparse(trimmed)
        if parsed.scheme == "" or parsed.netloc == "":
            raise RatingTransportError("sigil conversation rating transport failed: api endpoint host is required")
        return f"{parsed.scheme}://{parsed.netloc}"

    without_scheme = trimmed[7:] if trimmed.startswith("grpc://") else trimmed
    host = without_scheme.split("/", 1)[0].strip()
    if host == "":
        raise RatingTransportError("sigil conversation rating transport failed: api endpoint host is required")
    scheme = "http" if insecure else "https"
    return f"{scheme}://{host}"


def _parse_submit_conversation_rating_response(payload: Any) -> SubmitConversationRatingResponse:
    if not isinstance(payload, dict):
        raise RatingTransportError("sigil conversation rating transport failed: invalid response payload")

    rating_payload = payload.get("rating")
    summary_payload = payload.get("summary")
    if not isinstance(rating_payload, dict) or not isinstance(summary_payload, dict):
        raise RatingTransportError("sigil conversation rating transport failed: invalid response payload")

    return SubmitConversationRatingResponse(
        rating=_parse_conversation_rating(rating_payload),
        summary=_parse_conversation_rating_summary(summary_payload),
    )


def _parse_conversation_rating(payload: dict[str, Any]) -> ConversationRating:
    rating_id = _require_string(payload, "rating_id")
    conversation_id = _require_string(payload, "conversation_id")
    try:
        rating = ConversationRatingValue(_require_string(payload, "rating"))
    except ValueError as exc:
        raise RatingTransportError("sigil conversation rating transport failed: invalid rating payload") from exc
    created_at = _parse_utc_timestamp(_require_string(payload, "created_at"))

    metadata = payload.get("metadata", {})
    if metadata is None:
        metadata = {}
    if not isinstance(metadata, dict):
        raise RatingTransportError("sigil conversation rating transport failed: invalid rating payload")

    generation_id = payload.get("generation_id", "")
    rater_id = payload.get("rater_id", "")
    source = payload.get("source", "")
    comment = payload.get("comment", "")
    if (
        not isinstance(generation_id, str)
        or not isinstance(rater_id, str)
        or not isinstance(source, str)
        or not isinstance(comment, str)
    ):
        raise RatingTransportError("sigil conversation rating transport failed: invalid rating payload")

    return ConversationRating(
        rating_id=rating_id,
        conversation_id=conversation_id,
        rating=rating,
        created_at=created_at,
        comment=comment,
        metadata=metadata,
        generation_id=generation_id,
        rater_id=rater_id,
        source=source,
    )


def _parse_conversation_rating_summary(payload: dict[str, Any]) -> ConversationRatingSummary:
    total_count = _require_int(payload, "total_count")
    good_count = _require_int(payload, "good_count")
    bad_count = _require_int(payload, "bad_count")
    latest_rated_at = _parse_utc_timestamp(_require_string(payload, "latest_rated_at"))
    has_bad_rating = _require_bool(payload, "has_bad_rating")

    latest_rating_raw = payload.get("latest_rating")
    latest_rating: ConversationRatingValue | None = None
    if latest_rating_raw is not None:
        if not isinstance(latest_rating_raw, str) or latest_rating_raw.strip() == "":
            raise RatingTransportError("sigil conversation rating transport failed: invalid rating summary payload")
        try:
            latest_rating = ConversationRatingValue(latest_rating_raw)
        except ValueError as exc:
            raise RatingTransportError(
                "sigil conversation rating transport failed: invalid rating summary payload"
            ) from exc

    latest_bad_at_raw = payload.get("latest_bad_at")
    latest_bad_at: datetime | None = None
    if latest_bad_at_raw is not None:
        if not isinstance(latest_bad_at_raw, str) or latest_bad_at_raw.strip() == "":
            raise RatingTransportError("sigil conversation rating transport failed: invalid rating summary payload")
        latest_bad_at = _parse_utc_timestamp(latest_bad_at_raw)

    return ConversationRatingSummary(
        total_count=total_count,
        good_count=good_count,
        bad_count=bad_count,
        latest_rated_at=latest_rated_at,
        has_bad_rating=has_bad_rating,
        latest_rating=latest_rating,
        latest_bad_at=latest_bad_at,
    )


def _parse_utc_timestamp(value: str) -> datetime:
    normalized = value.strip()
    if normalized.endswith("Z"):
        normalized = normalized[:-1] + "+00:00"
    try:
        return _to_utc(datetime.fromisoformat(normalized))
    except ValueError as exc:
        raise RatingTransportError(
            "sigil conversation rating transport failed: invalid timestamp in response payload"
        ) from exc


def _require_string(payload: dict[str, Any], key: str) -> str:
    value = payload.get(key)
    if not isinstance(value, str) or value.strip() == "":
        raise RatingTransportError("sigil conversation rating transport failed: invalid response payload")
    return value


def _require_int(payload: dict[str, Any], key: str) -> int:
    value = payload.get(key)
    if not isinstance(value, int):
        raise RatingTransportError("sigil conversation rating transport failed: invalid response payload")
    return value


def _require_bool(payload: dict[str, Any], key: str) -> bool:
    value = payload.get(key)
    if not isinstance(value, bool):
        raise RatingTransportError("sigil conversation rating transport failed: invalid response payload")
    return value


def _rating_error_text(body: str, status: int) -> str:
    if body != "":
        return body
    return f"HTTP {status}"
