"""Shared mapping of provider payload fragments to agento11y models.

Used by the first-party provider wrappers and framework handlers, not by
application code. Two shapes show up in more than one package:

- Content blocks. Anthropic ``/v1/messages`` messages carry typed blocks
  (``text``, ``thinking``, ``tool_use``, ``tool_result``); OpenAI chat and the
  Responses API carry ``text``/``input_text``/``output_text``/``refusal`` parts.
  ``content_parts`` reads both vocabularies, so a caller that can receive either
  shape (the LiteLLM handler receives both, sometimes for the same call type)
  needs one pass instead of a shape guess.
- Tool schemas. The same tool list is nested under ``function`` on OpenAI chat,
  flat with ``parameters`` on the Responses API, and flat with ``input_schema``
  on Anthropic. ``tool_definitions`` accepts all three.

Values may be plain dicts or provider SDK objects (Pydantic models,
dataclasses), so fields are read through ``_read`` rather than by subscript.
"""

from __future__ import annotations

import json
from collections.abc import Iterable, Mapping
from dataclasses import asdict, is_dataclass
from typing import Any

from .models import MessageRole, Part, PartKind, ToolCall, ToolDefinition, ToolResult

# Block types whose text lives in ``text``. ``text`` is Anthropic and OpenAI
# chat; ``input_text``/``output_text`` are the Responses API.
_TEXT_BLOCK_TYPES = frozenset({"text", "input_text", "output_text"})

# A refused turn keeps its text in ``refusal``, and that is the only text it
# has, so it is read as message text.
_REFUSAL_BLOCK_TYPE = "refusal"

_TOOL_CALL_BLOCK_TYPES = frozenset({"tool_use", "server_tool_use", "mcp_tool_use"})


def content_parts(content: Any, *, role_hint: MessageRole = MessageRole.USER) -> list[Part]:
    """Map provider content blocks to Parts, preserving block order.

    ``role_hint`` is the role of the message the blocks came from. On a
    tool-role message every block is treated as a tool result, which is how
    frameworks that keep results in an untyped block report them.

    Unknown block types (images, documents, citations) are skipped: they carry
    no text we record today.
    """
    blocks = _as_list(content)
    if not blocks and isinstance(content, str):
        text = _raw_text(content)
        return [Part(kind=PartKind.TEXT, text=text)] if text else []

    parts: list[Part] = []
    for block in blocks:
        if isinstance(block, str):
            text = _raw_text(block)
            if text:
                parts.append(Part(kind=PartKind.TEXT, text=text))
            continue

        block_type = _as_str(_read(block, "type")).lower()

        if block_type in _TEXT_BLOCK_TYPES:
            # ``text`` can be present and null: LiteLLM's chat-to-Responses
            # bridge emits one output_text part per choice even when the
            # message content is None (a tool-call-only turn).
            text = _raw_text(_read(block, "text"))
            if text:
                parts.append(Part(kind=PartKind.TEXT, text=text))
            continue

        if block_type == _REFUSAL_BLOCK_TYPE:
            text = _raw_text(_read(block, "refusal")) or _raw_text(_read(block, "text"))
            if text:
                parts.append(Part(kind=PartKind.TEXT, text=text))
            continue

        if block_type == "thinking":
            thinking = _raw_text(_read(block, "thinking")) or _raw_text(_read(block, "text"))
            if thinking:
                parts.append(Part(kind=PartKind.THINKING, thinking=thinking))
            continue

        if block_type == "redacted_thinking":
            thinking = _raw_text(_read(block, "data")) or _raw_text(_read(block, "text"))
            if thinking:
                parts.append(Part(kind=PartKind.THINKING, thinking=thinking))
            continue

        if block_type in _TOOL_CALL_BLOCK_TYPES:
            name = _as_str(_read(block, "name"))
            parts.append(
                Part(
                    kind=PartKind.TOOL_CALL,
                    tool_call=ToolCall(
                        id=_as_str(_read(block, "id")),
                        name=name,
                        input_json=_json_bytes(_read(block, "input")),
                    ),
                )
            )
            continue

        if block_type == "tool_result" or role_hint == MessageRole.TOOL:
            result_content = _read(block, "content")
            parts.append(
                Part(
                    kind=PartKind.TOOL_RESULT,
                    tool_result=ToolResult(
                        tool_call_id=_as_str(_read(block, "tool_use_id")) or _as_str(_read(block, "tool_call_id")),
                        name=_as_str(_read(block, "name")),
                        content=content_text(result_content),
                        content_json=_json_bytes(result_content),
                        is_error=_as_bool(_read(block, "is_error")),
                    ),
                )
            )
            continue

    return parts


def content_text(value: Any) -> str:
    """Extract the text of a content value: a string, a block, or a list of either."""
    if value is None:
        return ""

    if isinstance(value, str):
        return value.strip()

    if isinstance(value, bytes):
        return value.decode("utf-8", errors="replace").strip()

    if isinstance(value, Mapping):
        text = _as_str(value.get("text"))
        if text:
            return text
        content = value.get("content")
        if content is not None:
            return content_text(content)
        return ""

    if isinstance(value, (list, tuple)):
        chunks: list[str] = []
        for item in value:
            chunk = content_text(item)
            if chunk:
                chunks.append(chunk)
        return "\n".join(chunks)

    model_dump = getattr(value, "model_dump", None)
    if callable(model_dump):
        try:
            return content_text(model_dump(mode="json"))
        except TypeError:
            return content_text(model_dump())

    return _as_str(value)


def tool_definitions(value: Any) -> list[ToolDefinition]:
    """Map a request tool list to ToolDefinitions, accepting all three shapes.

    - OpenAI chat: ``{"type": "function", "function": {"name", "description", "parameters"}}``
    - Responses API: ``{"type": "function", "name", "description", "parameters"}``
    - Anthropic: ``{"name", "description", "input_schema"}``

    Provider built-in tools (``web_search``, ``bash``, ...) have a type and a
    name but no schema, and are recorded with the type the provider used. A
    tool without a name is skipped: there is nothing to attribute a call to.
    """
    out: list[ToolDefinition] = []
    for raw_tool in _as_list(value):
        tool_type = _as_str(_read(raw_tool, "type")) or "function"

        function = _read(raw_tool, "function")
        payload = raw_tool if function is None else function

        name = _as_str(_read(payload, "name"))
        if not name:
            continue

        schema = _read(payload, "input_schema")
        if schema is None:
            schema = _read(payload, "parameters")

        out.append(
            ToolDefinition(
                name=name,
                description=_as_str(_read(payload, "description")),
                type=tool_type,
                # Absent schema stays empty rather than the JSON literal
                # ``null``, matching the Go and JS providers.
                input_schema_json=b"" if schema is None else _json_bytes(schema),
            )
        )

    return out


def _raw_text(value: Any) -> str:
    """Return a text field unchanged, or "" when it is missing or blank.

    Whitespace decides whether a block carries text, but the text itself is
    recorded as sent: leading and trailing whitespace is meaningful to the model
    that produced or consumed it. Matches the Go and JS providers.
    """
    if isinstance(value, str):
        return value if value.strip() else ""
    return _as_str(value)


def _read(value: Any, key: str, default: Any = None) -> Any:
    if value is None:
        return default

    if isinstance(value, Mapping):
        return value.get(key, default)

    if hasattr(value, key):
        return getattr(value, key)

    getter = getattr(value, "get", None)
    if callable(getter):
        try:
            return getter(key, default)
        except Exception:  # noqa: BLE001
            return default

    return default


def _as_list(value: Any) -> list[Any]:
    if value is None:
        return []
    if isinstance(value, list):
        return value
    if isinstance(value, tuple):
        return list(value)
    if isinstance(value, Iterable) and not isinstance(value, (str, bytes, Mapping)):
        return list(value)
    return []


def _as_str(value: Any) -> str:
    if value is None:
        return ""
    if isinstance(value, str):
        return value.strip()
    return str(value).strip()


def _as_bool(value: Any) -> bool:
    if isinstance(value, bool):
        return value
    if isinstance(value, str):
        lowered = value.strip().lower()
        if lowered in {"true", "1", "yes", "on"}:
            return True
        if lowered in {"false", "0", "no", "off"}:
            return False
    return False


def _json_bytes(value: Any) -> bytes:
    return json.dumps(_to_plain(value), separators=(",", ":"), sort_keys=True, default=str).encode("utf-8")


def _to_plain(value: Any) -> Any:
    if value is None:
        return None

    if isinstance(value, (str, int, float, bool)):
        return value

    if isinstance(value, bytes):
        return value.decode("utf-8", errors="replace")

    if isinstance(value, Mapping):
        return {str(key): _to_plain(inner) for key, inner in value.items()}

    if isinstance(value, (list, tuple)):
        return [_to_plain(inner) for inner in value]

    if is_dataclass(value) and not isinstance(value, type):
        return {key: _to_plain(inner) for key, inner in asdict(value).items()}

    model_dump = getattr(value, "model_dump", None)
    if callable(model_dump):
        try:
            dumped = model_dump(mode="json")
        except TypeError:
            dumped = model_dump()
        return _to_plain(dumped)

    to_dict = getattr(value, "to_dict", None)
    if callable(to_dict):
        return _to_plain(to_dict())

    dict_method = getattr(value, "dict", None)
    if callable(dict_method):
        return _to_plain(dict_method())

    if hasattr(value, "__dict__"):
        return _to_plain(vars(value))

    return str(value)
