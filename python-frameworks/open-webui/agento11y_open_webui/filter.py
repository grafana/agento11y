from __future__ import annotations

import asyncio
import atexit
import logging
import threading
from dataclasses import dataclass
from datetime import datetime, timezone
from typing import Any
from uuid import uuid4

from agento11y import (
    Client,
    ClientConfig,
    ContentCaptureMode,
    Generation,
    GenerationStart,
    ModelRef,
    SecretRedactionOptions,
    TokenUsage,
    assistant_text_message,
    create_secret_redaction_sanitizer,
    user_text_message,
)

_STATE_KEY = "agento11y.open_webui"
_MODEL = ModelRef(provider="custom", name="open-webui-response-summary")
_TEXT_BLOCK_TYPES = frozenset({"text", "input_text", "output_text"})
_logger = logging.getLogger(__name__)


@dataclass(frozen=True, slots=True)
class _Snapshot:
    generation_id: str
    conversation_id: str
    user_id: str
    selected_message_id: str
    selected_model_id: str
    input_text: str
    output_text: str
    usage: TokenUsage
    usage_status: str
    streaming: bool
    started_at: datetime
    completed_at: datetime


class Filter:
    """Export completed Open WebUI responses without changing chat content."""

    def __init__(self, client: Client | None = None) -> None:
        self._client = client
        self._client_lock = threading.Lock()
        self._content_capture_mode = self._configured_content_capture(client)
        self._agent_name = self._configured_agent_name(client)
        self._sanitizer = create_secret_redaction_sanitizer(SecretRedactionOptions(redact_input_messages=True))

    async def inlet(self, body: dict[str, Any], __metadata__: dict[str, Any] | None = None) -> dict[str, Any]:
        if isinstance(__metadata__, dict):
            __metadata__[_STATE_KEY] = {
                "generation_id": str(uuid4()),
                "started_at": datetime.now(timezone.utc).isoformat(),
                "streaming": bool(body.get("stream", False)),
                "consumed": False,
            }
        return body

    async def outlet(
        self,
        body: dict[str, Any],
        __metadata__: dict[str, Any] | None = None,
        __user__: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        state = __metadata__.get(_STATE_KEY) if isinstance(__metadata__, dict) else None
        if not isinstance(state, dict) or state.get("consumed") is not False:
            return body

        state["consumed"] = True

        try:
            await asyncio.to_thread(
                self._export_snapshot,
                body,
                state,
                __metadata__ if isinstance(__metadata__, dict) else {},
                __user__ if isinstance(__user__, dict) else {},
            )
        except Exception:  # noqa: BLE001
            self._log_failure("export_execution")

        return body

    def _export_snapshot(
        self,
        body: dict[str, Any],
        state: dict[str, Any],
        metadata: dict[str, Any],
        user: dict[str, Any],
    ) -> None:
        try:
            client = self._get_client()
        except Exception:  # noqa: BLE001
            self._log_failure("client_initialization")
            return

        capture_mode = self._content_capture_mode or ContentCaptureMode.METADATA_ONLY
        try:
            snapshot = _build_snapshot(
                body,
                state,
                metadata,
                user,
                capture_content=capture_mode is not ContentCaptureMode.METADATA_ONLY,
            )
        except Exception:  # noqa: BLE001
            self._log_failure("snapshot_mapping")
            return

        if snapshot is None:
            return

        start = GenerationStart(
            id=snapshot.generation_id,
            conversation_id=snapshot.conversation_id,
            user_id=snapshot.user_id,
            agent_name=self._agent_name,
            model=_MODEL,
            content_capture=capture_mode,
            metadata={
                "capture_scope": "response_snapshot",
                "selected_message_id": snapshot.selected_message_id,
                "selected_model_id": snapshot.selected_model_id,
                "usage_status": snapshot.usage_status,
            },
            started_at=snapshot.started_at,
        )
        generation = Generation(
            input=[user_text_message(snapshot.input_text)] if snapshot.input_text.strip() else [],
            output=[assistant_text_message(snapshot.output_text)] if snapshot.output_text.strip() else [],
            usage=snapshot.usage,
            completed_at=snapshot.completed_at,
        )

        if capture_mode is not ContentCaptureMode.METADATA_ONLY:
            try:
                generation = self._sanitizer(generation)
            except Exception:  # noqa: BLE001
                start.content_capture = ContentCaptureMode.METADATA_ONLY
                generation.input = []
                generation.output = []
                self._log_failure("content_sanitization")

        try:
            recorder = (
                client.start_streaming_generation(start) if snapshot.streaming else client.start_generation(start)
            )
        except Exception:  # noqa: BLE001
            self._log_failure("recorder_initialization")
            return

        try:
            recorder.set_result(generation)
            recorder.end()
        except Exception:  # noqa: BLE001
            self._log_failure("recorder_finalization")
            return

        try:
            queue_error = recorder.err()
        except Exception:  # noqa: BLE001
            self._log_failure("snapshot_queueing")
            return

        if queue_error is not None:
            self._log_failure("snapshot_queueing")

    def _get_client(self) -> Client:
        if self._client is not None:
            return self._client

        with self._client_lock:
            if self._client is not None:
                return self._client

            config = ClientConfig.from_env()
            if config.content_capture is ContentCaptureMode.DEFAULT:
                config.content_capture = ContentCaptureMode.METADATA_ONLY
            if not config.agent_name:
                config.agent_name = "open-webui"
            config.generation_sanitizer = self._sanitizer
            client = Client(config)
            self._client = client
            self._content_capture_mode = config.content_capture
            self._agent_name = config.agent_name or "open-webui"
            atexit.register(client.shutdown)
            return client

    @staticmethod
    def _configured_content_capture(client: Client | None) -> ContentCaptureMode | None:
        if client is None:
            return None
        config = getattr(client, "_config", None)
        mode = getattr(config, "content_capture", None)
        if not isinstance(mode, ContentCaptureMode) or mode is ContentCaptureMode.DEFAULT:
            return ContentCaptureMode.METADATA_ONLY
        return mode

    @staticmethod
    def _configured_agent_name(client: Client | None) -> str:
        config = getattr(client, "_config", None) if client is not None else None
        return getattr(config, "agent_name", "") or "open-webui"

    @staticmethod
    def _log_failure(category: str) -> None:
        _logger.warning("agento11y Open WebUI export failed: %s", category)


def _build_snapshot(
    body: dict[str, Any],
    state: dict[str, Any],
    metadata: dict[str, Any],
    user: dict[str, Any],
    *,
    capture_content: bool,
) -> _Snapshot | None:
    selected_message_id = _nonempty_string(body.get("id"))
    messages = body.get("messages")
    if not selected_message_id or not isinstance(messages, list):
        return None

    selected_index = next(
        (
            index
            for index, message in enumerate(messages)
            if isinstance(message, dict)
            and message.get("role") == "assistant"
            and message.get("id") == selected_message_id
        ),
        None,
    )
    if selected_index is None:
        return None

    assistant = messages[selected_index]
    user_message = next(
        (
            message
            for message in reversed(messages[:selected_index])
            if isinstance(message, dict) and message.get("role") == "user"
        ),
        None,
    )

    continuation = bool(_nonempty_string(metadata.get("assistant_message_id")))
    usage, usage_status = _map_usage(assistant.get("usage"), continuation=continuation)
    generation_id = _required_string(state.get("generation_id"))
    conversation_id = _nonempty_string(body.get("chat_id")) or generation_id

    return _Snapshot(
        generation_id=generation_id,
        conversation_id=conversation_id,
        user_id=_nonempty_string(user.get("id")),
        selected_message_id=selected_message_id,
        selected_model_id=_nonempty_string(body.get("model")),
        input_text=_extract_text(user_message.get("content")) if capture_content and user_message else "",
        output_text=_extract_text(assistant.get("content")) if capture_content else "",
        usage=usage,
        usage_status=usage_status,
        streaming=bool(state.get("streaming", False)),
        started_at=_parse_timestamp(state.get("started_at")),
        completed_at=datetime.now(timezone.utc),
    )


def _extract_text(content: Any) -> str:
    if isinstance(content, str):
        return content
    if not isinstance(content, list):
        return ""

    return "".join(
        block["text"]
        for block in content
        if isinstance(block, dict) and block.get("type") in _TEXT_BLOCK_TYPES and isinstance(block.get("text"), str)
    )


def _map_usage(raw: Any, *, continuation: bool) -> tuple[TokenUsage, str]:
    if continuation:
        return TokenUsage(), "continuation_omitted"
    if not isinstance(raw, dict):
        return TokenUsage(), "unavailable"

    input_tokens = _token_count(raw.get("input_tokens"))
    output_tokens = _token_count(raw.get("output_tokens"))
    if input_tokens is None or output_tokens is None:
        return TokenUsage(), "unavailable"

    if "total_tokens" in raw:
        total_tokens = _token_count(raw.get("total_tokens"))
        if total_tokens is None:
            return TokenUsage(), "unavailable"
    else:
        total_tokens = input_tokens + output_tokens

    return (
        TokenUsage(
            input_tokens=input_tokens,
            output_tokens=output_tokens,
            total_tokens=total_tokens,
        ),
        "reported",
    )


def _token_count(value: Any) -> int | None:
    if isinstance(value, bool) or not isinstance(value, int) or value < 0:
        return None
    return value


def _required_string(value: Any) -> str:
    result = _nonempty_string(value)
    if not result:
        raise ValueError("required Filter state is missing")
    return result


def _nonempty_string(value: Any) -> str:
    return value.strip() if isinstance(value, str) else ""


def _parse_timestamp(value: Any) -> datetime:
    timestamp = datetime.fromisoformat(_required_string(value))
    if timestamp.tzinfo is None:
        raise ValueError("Filter timestamp has no timezone")
    return timestamp.astimezone(timezone.utc)
