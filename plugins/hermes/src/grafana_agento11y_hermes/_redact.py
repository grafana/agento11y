"""Structural payload redaction.

Ported from the langfuse plugin's ``_safe_value`` family. Applies a depth limit
of 4, caps dict/list size at 50 entries, and truncates strings to a
caller-supplied ``max_chars``. Shared SDK secret patterns run before truncation. The aim
is to bound the size and shape of arbitrary tool I/O before it reaches the
generation exporter.

Callers thread their resolved plugin ``max_chars`` (from
``AGENTO11Y_HERMES_MAX_CHARS``) into every entry call. There is no env fallback
inside this module.
"""

from __future__ import annotations

import itertools
import json
from dataclasses import fields, is_dataclass, replace
from enum import Enum
from typing import Any

from agento11y import Generation
from agento11y.redaction import SecretRedactionOptions, create_secret_redaction_sanitizer, redact_secret_text

_MAX_DEPTH = 4
_MAX_ENTRIES = 50
_PROSE_SANITIZER = create_secret_redaction_sanitizer(SecretRedactionOptions(redact_input_messages=False))


def redact_prose(value: str) -> str:
    # Reuse the SDK's lightweight error policy without importing its private engine.
    return _PROSE_SANITIZER(Generation(call_error=value)).call_error


def truncate_prose(value: str, max_chars: int) -> str:
    return _truncate(redact_prose(value), max_chars)


def truncate_text(value: str, max_chars: int) -> str:
    return _truncate(redact_secret_text(value), max_chars)


def _truncate(value: str, max_chars: int) -> str:
    if len(value) <= max_chars:
        return value
    return value[:max_chars] + f"... [truncated {len(value) - max_chars} chars]"


def maybe_parse_json_string(value: str, max_chars: int) -> Any:
    """If ``value`` looks like JSON, return the parsed object; otherwise return as-is.

    Refuses to parse strings longer than ``max_chars`` — without this guard a
    multi-megabyte tool result would be fully decoded (allocating the entire
    parse tree) before any size cap kicked in.
    """
    if len(value) > max_chars:
        return value
    stripped = value.strip()
    if len(stripped) < 2 or stripped[0] not in "{[":
        return value
    try:
        parsed, idx = json.JSONDecoder().raw_decode(stripped)
    except Exception:
        return value
    # Unreachable while the prefix check above holds: a value starting with
    # "{" or "[" decodes to a dict or a list or not at all. Kept so relaxing
    # that check cannot silently start returning scalars from here.
    if not isinstance(parsed, (dict, list)):
        return value

    trailing = stripped[idx:].strip()
    if not trailing:
        return parsed

    hint_key = "_hint" if trailing.startswith("[Hint:") else "_trailing_text"
    if isinstance(parsed, dict):
        merged = dict(parsed)
        key = hint_key if hint_key not in merged else "_trailing_text"
        merged[key] = trailing
        return merged

    return {"data": parsed, hint_key: trailing}


def redact_record(value: Any) -> Any:
    """Copy SDK records, including fields outside the generation sanitizer's scope."""
    if isinstance(value, Enum):
        return value
    if isinstance(value, str):
        return redact_secret_text(value)
    if isinstance(value, bytes):
        return redact_secret_text(value.decode("utf-8", errors="replace")).encode("utf-8")
    if is_dataclass(value) and not isinstance(value, type):
        return replace(value, **{field.name: redact_record(getattr(value, field.name)) for field in fields(value)})
    if isinstance(value, dict):
        return {redact_record(key): redact_record(item) for key, item in value.items()}
    if isinstance(value, list):
        return [redact_record(item) for item in value]
    return value


def safe_value(
    value: Any,
    *,
    max_chars: int,
    depth: int = 0,
    parse_json_strings: bool = False,
) -> Any:
    """Return a structurally-bounded copy of ``value`` safe to record.

    ``max_chars`` is required from the caller — every recursive descent uses
    the same cap so a 50x50 nested dict only needs the value resolved once.
    """
    shaped = _safe_value(value, max_chars, depth, parse_json_strings)
    # Key/value patterns need the encoded object, not isolated string values.
    redacted = redact_secret_text(json.dumps(shaped))
    try:
        return json.loads(redacted)
    except json.JSONDecodeError:
        # Regex replacements can break JSON containing escaped quotes.
        return "[REDACTED:invalid-json]"


def _safe_value(value: Any, max_chars: int, depth: int, parse_json_strings: bool) -> Any:
    if depth > _MAX_DEPTH:
        return "<max-depth>"
    if value is None or isinstance(value, (int, float, bool)):
        return value
    if isinstance(value, bytes):
        return {"type": "bytes", "len": len(value)}
    if isinstance(value, str):
        if parse_json_strings:
            parsed = maybe_parse_json_string(value, max_chars)
            if parsed is not value:
                return _safe_value(parsed, max_chars, depth, True)
        return truncate_text(value, max_chars)
    if isinstance(value, dict):
        # Iterate via islice so we never materialize the full items() list —
        # a multi-million-entry dict would otherwise allocate before the cap.
        return {
            redact_secret_text(str(k)): _safe_value(v, max_chars, depth + 1, parse_json_strings)
            for k, v in itertools.islice(value.items(), _MAX_ENTRIES)
        }
    if isinstance(value, (list, tuple)):
        # Sequence slicing returns a view-sized copy — no full materialization.
        return [_safe_value(v, max_chars, depth + 1, parse_json_strings) for v in value[:_MAX_ENTRIES]]
    if isinstance(value, set):
        return [_safe_value(v, max_chars, depth + 1, parse_json_strings) for v in itertools.islice(value, _MAX_ENTRIES)]
    if hasattr(value, "__dict__"):
        return _safe_value(vars(value), max_chars, depth + 1, parse_json_strings)
    return truncate_text(repr(value), max_chars)
