"""Type coercion for whatever hermes put on a hook payload.

Hermes mirrors provider shapes rather than normalizing them, so a field can
arrive as a string, a number, a typed content block or a list of them. These
helpers are generic: they hold no hook semantics, which is why both ``_hooks``
and ``_request`` can read them without one importing the other.
"""

from __future__ import annotations

import json
from typing import Any


def coerce_text(content: Any) -> str:
    """Best-effort conversion of a message ``content`` field to a string.

    Content can be a string or a list of typed blocks
    (``{"type": "text", "text": "..."}``). We collapse list blocks into
    newline-joined text.
    """
    if content is None:
        return ""
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        chunks: list[str] = []
        for block in content:
            if isinstance(block, str):
                chunks.append(block)
            elif isinstance(block, dict):
                if isinstance(block.get("text"), str):
                    chunks.append(block["text"])
                elif isinstance(block.get("content"), str):
                    chunks.append(block["content"])
                else:
                    chunks.append(json.dumps(block, default=str))
            else:
                chunks.append(repr(block))
        return "\n".join(c for c in chunks if c)
    return str(content)


def as_int(value: Any) -> int:
    """Integer value, or 0 when it cannot be converted.

    Token usage is built inside ``set_result``'s argument list, so a provider
    that reports a count as text would abort the close before it and export a
    generation with no input, output, usage or model at all.
    """
    try:
        return int(value or 0)
    except (TypeError, ValueError):
        return 0


def as_optional_int(value: Any) -> int | None:
    """Integer value, or ``None`` when absent or unconvertible."""
    try:
        return int(value)
    except (TypeError, ValueError):
        return None


def as_optional_float(value: Any) -> float | None:
    """Float value, or ``None`` when absent or unconvertible."""
    try:
        return float(value)
    except (TypeError, ValueError):
        return None
