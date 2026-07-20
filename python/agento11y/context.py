"""Context helpers for conversation and agent identity propagation."""

from __future__ import annotations

import contextvars
from collections.abc import Iterator
from contextlib import contextmanager
from typing import TYPE_CHECKING, NamedTuple

if TYPE_CHECKING:
    from .models import ContentCaptureMode

_conversation_id: contextvars.ContextVar[str | None] = contextvars.ContextVar("agento11y_conversation_id", default=None)
_conversation_title: contextvars.ContextVar[str | None] = contextvars.ContextVar(
    "agento11y_conversation_title", default=None
)
_user_id: contextvars.ContextVar[str | None] = contextvars.ContextVar("agento11y_user_id", default=None)
_agent_name: contextvars.ContextVar[str | None] = contextvars.ContextVar("agento11y_agent_name", default=None)
_agent_version: contextvars.ContextVar[str | None] = contextvars.ContextVar("agento11y_agent_version", default=None)
_content_capture_mode: contextvars.ContextVar[ContentCaptureMode | None] = contextvars.ContextVar(
    "agento11y_content_capture_mode", default=None
)


class _CaptureStackEntry(NamedTuple):
    recorder_id: int
    mode: ContentCaptureMode


# Stack of active GenerationRecorder capture modes.
# Used to correctly restore the ContextVar when overlapping generations end in
# non-LIFO order.
_capture_mode_stack: contextvars.ContextVar[tuple[_CaptureStackEntry, ...]] = contextvars.ContextVar(
    "_agento11y_capture_mode_stack", default=()
)
# Snapshot of _content_capture_mode before the first recorder pushed onto the
# stack.  Restored when the stack empties so that a surrounding
# with_content_capture_mode() block is not clobbered.
_capture_mode_stack_base: contextvars.ContextVar[ContentCaptureMode | None] = contextvars.ContextVar(
    "_agento11y_capture_mode_stack_base", default=None
)


def _push_capture_mode(recorder_id: int, mode: ContentCaptureMode) -> None:
    """Pushes a recorder's capture mode onto the stack and sets the ContextVar."""
    stack = _capture_mode_stack.get()
    if not stack:
        _capture_mode_stack_base.set(_content_capture_mode.get())
    _capture_mode_stack.set((*stack, _CaptureStackEntry(recorder_id, mode)))
    _content_capture_mode.set(mode)


def _pop_capture_mode(recorder_id: int) -> None:
    """Removes a recorder from the stack and restores the ContextVar."""
    stack = _capture_mode_stack.get()
    new_stack = tuple(e for e in stack if e.recorder_id != recorder_id)
    _capture_mode_stack.set(new_stack)
    _content_capture_mode.set(new_stack[-1].mode if new_stack else _capture_mode_stack_base.get())


@contextmanager
def with_conversation_id(conversation_id: str) -> Iterator[None]:
    """Sets conversation id within a context block."""

    token = _conversation_id.set(conversation_id)
    try:
        yield
    finally:
        _conversation_id.reset(token)


@contextmanager
def with_conversation_title(conversation_title: str) -> Iterator[None]:
    """Sets conversation title within a context block."""

    token = _conversation_title.set(conversation_title)
    try:
        yield
    finally:
        _conversation_title.reset(token)


@contextmanager
def with_user_id(user_id: str) -> Iterator[None]:
    """Sets user id within a context block."""

    token = _user_id.set(user_id)
    try:
        yield
    finally:
        _user_id.reset(token)


@contextmanager
def with_agent_name(agent_name: str) -> Iterator[None]:
    """Sets agent name within a context block."""

    token = _agent_name.set(agent_name)
    try:
        yield
    finally:
        _agent_name.reset(token)


@contextmanager
def with_agent_version(agent_version: str) -> Iterator[None]:
    """Sets agent version within a context block."""

    token = _agent_version.set(agent_version)
    try:
        yield
    finally:
        _agent_version.reset(token)


def conversation_id_from_context() -> str | None:
    """Returns the current conversation id from context variables."""

    return _conversation_id.get()


def agent_name_from_context() -> str | None:
    """Returns the current agent name from context variables."""

    return _agent_name.get()


def agent_version_from_context() -> str | None:
    """Returns the current agent version from context variables."""

    return _agent_version.get()


def conversation_title_from_context() -> str | None:
    """Returns the current conversation title from context variables."""

    return _conversation_title.get()


def user_id_from_context() -> str | None:
    """Returns the current user id from context variables."""

    return _user_id.get()


@contextmanager
def with_content_capture_mode(mode: ContentCaptureMode) -> Iterator[None]:
    """Sets the content capture mode within a context block.

    Overrides any client-level ``content_capture`` default for generations
    and tool executions inside the block, including ``FULL_WITH_METADATA_SPANS``.
    Embeddings are gated on client config + resolver only and do not honor this
    override, matching Go/Java/JS/.NET.
    """

    token = _content_capture_mode.set(mode)
    try:
        yield
    finally:
        _content_capture_mode.reset(token)


def content_capture_mode_from_context() -> ContentCaptureMode | None:
    """Returns the content capture mode from context, or None if not set."""

    return _content_capture_mode.get()
