"""Centralised usage-mapping helpers for all known LLM response shapes."""

from __future__ import annotations

from collections.abc import Mapping
from typing import Any

from .models import TokenUsage


def from_anthropic(raw: Any) -> TokenUsage:
    """Extract usage from Anthropic's flat field layout."""
    if raw is None:
        return TokenUsage()
    cache_write_raw = _read(raw, "cache_write_input_tokens")
    cache_write = (
        _as_int(cache_write_raw)
        if cache_write_raw is not None
        else _as_int(
            _read(raw, "cache_creation_input_tokens"),
        )
    )
    return TokenUsage(
        input_tokens=_as_int(_read(raw, "input_tokens")),
        output_tokens=_as_int(_read(raw, "output_tokens")),
        total_tokens=_as_int(_read(raw, "total_tokens")),
        cache_read_input_tokens=_as_int(_read(raw, "cache_read_input_tokens")),
        cache_write_input_tokens=cache_write,
        input_is_disjoint=True,
    ).normalize()


def from_openai_chat(raw: Any) -> TokenUsage:
    """Extract usage from OpenAI Chat Completions (prompt_tokens / completion_tokens + nested details)."""
    if raw is None:
        return TokenUsage()
    prompt_tokens = _as_int(_read(raw, "prompt_tokens"))
    cache_read = _as_int(
        _read(_read(raw, "prompt_tokens_details"), "cached_tokens"),
    )
    return TokenUsage(
        input_tokens=_fresh_input_tokens(prompt_tokens, cache_read),
        output_tokens=_as_int(_read(raw, "completion_tokens")),
        total_tokens=_as_int(_read(raw, "total_tokens")),
        cache_read_input_tokens=cache_read,
        cache_write_input_tokens=_as_int(
            _read(_read(raw, "prompt_tokens_details"), "cache_creation_tokens"),
        ),
        reasoning_tokens=_as_int(
            _read(_read(raw, "completion_tokens_details"), "reasoning_tokens"),
        ),
        input_is_disjoint=True,
    ).normalize()


def from_openai_responses(raw: Any) -> TokenUsage:
    """Extract usage from OpenAI Responses API (input_tokens / output_tokens + nested details)."""
    if raw is None:
        return TokenUsage()
    input_tokens = _as_int(_read(raw, "input_tokens"))
    cache_read = _as_int(
        _read(_read(raw, "input_tokens_details"), "cached_tokens"),
    )
    return TokenUsage(
        input_tokens=_fresh_input_tokens(input_tokens, cache_read),
        output_tokens=_as_int(_read(raw, "output_tokens")),
        total_tokens=_as_int(_read(raw, "total_tokens")),
        cache_read_input_tokens=cache_read,
        reasoning_tokens=_as_int(
            _read(_read(raw, "output_tokens_details"), "reasoning_tokens"),
        ),
        input_is_disjoint=True,
    ).normalize()


def from_gemini(raw: Any) -> TokenUsage:
    """Extract usage from Gemini's usage_metadata field names."""
    if raw is None:
        return TokenUsage()

    prompt_tokens = _as_int(_read(raw, "prompt_token_count"))
    candidate_tokens = _as_int(_read(raw, "candidates_token_count"))
    total_tokens = _as_int(_read(raw, "total_token_count"))
    tool_use_prompt_tokens = _as_int(_read(raw, "tool_use_prompt_token_count"))
    reasoning_tokens = _as_int(_read(raw, "thoughts_token_count"))
    cache_read = _as_int(_read(raw, "cached_content_token_count"))
    # Match Go/JS/.NET: fresh input excludes tool-use prompt tokens (metadata + total only).
    input_tokens = _fresh_input_tokens(prompt_tokens, cache_read)
    output_tokens = candidate_tokens + reasoning_tokens

    cache_write_raw = _read(raw, "cache_write_input_token_count")
    cache_write = (
        _as_int(cache_write_raw)
        if cache_write_raw is not None
        else _as_int(
            _read(raw, "cache_creation_input_token_count"),
        )
    )
    if total_tokens == 0:
        total_tokens = input_tokens + output_tokens + cache_read + cache_write + tool_use_prompt_tokens

    return TokenUsage(
        input_tokens=input_tokens,
        output_tokens=output_tokens,
        total_tokens=total_tokens,
        cache_read_input_tokens=cache_read,
        cache_write_input_tokens=cache_write,
        reasoning_tokens=reasoning_tokens,
        input_is_disjoint=True,
    )


def from_generic(raw: Any) -> TokenUsage:
    """Best-effort extraction for payloads that don't match a known provider shape.

    Tries both OpenAI-style (prompt_tokens) and Anthropic-style (input_tokens)
    key families, plus flat cache/reasoning fields. Used by framework adapters
    that may only expose a subset of counts.
    """
    if raw is None:
        return TokenUsage()

    prompt = _read(raw, "prompt_tokens")
    input_tokens = _as_int(prompt) if prompt is not None else _as_int(_read(raw, "input_tokens"))
    completion = _read(raw, "completion_tokens")
    output_tokens = _as_int(completion) if completion is not None else _as_int(_read(raw, "output_tokens"))
    total_tokens = _as_int(_read(raw, "total_tokens"))
    if total_tokens == 0:
        total_tokens = input_tokens + output_tokens

    cache_write_raw = _read(raw, "cache_write_input_tokens")
    cache_write = (
        _as_int(cache_write_raw)
        if cache_write_raw is not None
        else _as_int(
            _read(raw, "cache_creation_input_tokens"),
        )
    )
    return TokenUsage(
        input_tokens=input_tokens,
        output_tokens=output_tokens,
        total_tokens=total_tokens,
        cache_read_input_tokens=_as_int(_read(raw, "cache_read_input_tokens")),
        cache_write_input_tokens=cache_write,
        reasoning_tokens=_as_int(_read(raw, "reasoning_tokens")),
    )


def map_usage(raw: Any) -> TokenUsage:
    """Auto-detect the usage shape and dispatch to the appropriate extractor."""
    if raw is None:
        return TokenUsage()

    if _read(raw, "prompt_token_count") is not None or _read(raw, "candidates_token_count") is not None:
        return from_gemini(raw)

    if _read(raw, "prompt_tokens") is not None:
        return from_openai_chat(raw)

    if _read(raw, "input_tokens_details") is not None or _read(raw, "output_tokens_details") is not None:
        return from_openai_responses(raw)

    if _read(raw, "input_tokens") is not None:
        return from_anthropic(raw)

    return from_generic(raw)


def _read(value: Any, key: str, default: Any = None) -> Any:
    if value is None:
        return default

    if isinstance(value, Mapping):
        return value.get(key, default)

    if hasattr(value, key):
        return getattr(value, key)

    return default


def _fresh_input_tokens(raw_input_tokens: int, cache_read_input_tokens: int) -> int:
    """Convert cache-inclusive input totals into fresh-input tokens."""

    return max(raw_input_tokens - cache_read_input_tokens, 0)


def _as_int(value: Any) -> int:
    converted = _as_int_or_none(value)
    return converted if converted is not None else 0


def _as_int_or_none(value: Any) -> int | None:
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
        text = value.strip()
        if not text:
            return None
        try:
            return int(text)
        except ValueError:
            return None

    return None
