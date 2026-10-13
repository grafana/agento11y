"""Centralised usage-mapping helpers for all known LLM response shapes."""

from __future__ import annotations

from collections.abc import Mapping
from typing import Any

from .models import TokenInputSemantics, TokenUsage


def from_anthropic(raw: Any) -> TokenUsage:
    """Extract usage from Anthropic's flat field layout.

    Anthropic reports ``input_tokens`` exclusive of both cache buckets. The
    OTel GenAI Anthropic rule requires summing them into the inclusive
    ``input_tokens`` this SDK emits, so this extractor normalizes up and
    marks the result. Only call it for payloads positively identified as
    Anthropic; guessed flat shapes go through ``map_usage``'s raw fallback.
    """
    if raw is None:
        return TokenUsage()
    flat = _flat_raw_usage(raw)
    input_tokens = flat.input_tokens + flat.cache_read_input_tokens + flat.cache_write_input_tokens
    return TokenUsage(
        input_tokens=input_tokens,
        output_tokens=flat.output_tokens,
        total_tokens=flat.total_tokens,
        cache_read_input_tokens=flat.cache_read_input_tokens,
        cache_write_input_tokens=flat.cache_write_input_tokens,
        input_semantics=TokenInputSemantics.INCLUSIVE,
    ).normalize()


def _flat_raw_usage(raw: Any) -> TokenUsage:
    """Read Anthropic-named flat keys verbatim: no normalization, no marker."""
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
    )


def from_openai_chat(raw: Any) -> TokenUsage:
    """Extract usage from OpenAI Chat Completions (prompt_tokens / completion_tokens + nested details)."""
    if raw is None:
        return TokenUsage()
    return TokenUsage(
        input_tokens=_as_int(_read(raw, "prompt_tokens")),
        output_tokens=_as_int(_read(raw, "completion_tokens")),
        total_tokens=_as_int(_read(raw, "total_tokens")),
        cache_read_input_tokens=_as_int(
            _read(_read(raw, "prompt_tokens_details"), "cached_tokens"),
        ),
        cache_write_input_tokens=_as_int(
            _read(_read(raw, "prompt_tokens_details"), "cache_creation_tokens"),
        ),
        reasoning_tokens=_as_int(
            _read(_read(raw, "completion_tokens_details"), "reasoning_tokens"),
        ),
        # prompt_tokens already includes cached tokens: inclusive as-is.
        input_semantics=TokenInputSemantics.INCLUSIVE,
    ).normalize()


def from_openai_responses(raw: Any) -> TokenUsage:
    """Extract usage from OpenAI Responses API (input_tokens / output_tokens + nested details)."""
    if raw is None:
        return TokenUsage()
    usage = TokenUsage(
        input_tokens=_as_int(_read(raw, "input_tokens")),
        output_tokens=_as_int(_read(raw, "output_tokens")),
        total_tokens=_as_int(_read(raw, "total_tokens")),
        cache_read_input_tokens=_as_int(
            _read(_read(raw, "input_tokens_details"), "cached_tokens"),
        ),
        reasoning_tokens=_as_int(
            _read(_read(raw, "output_tokens_details"), "reasoning_tokens"),
        ),
        # input_tokens already includes cached tokens: inclusive as-is.
        input_semantics=TokenInputSemantics.INCLUSIVE,
    ).normalize()

    usage.input_by_modality = _modality_partition(_read(raw, "input_tokens_details"), usage.input_tokens, openai=True)
    usage.output_by_modality = _modality_partition(
        _read(raw, "output_tokens_details"), usage.output_tokens, openai=True
    )
    usage.cache_read_by_modality = _modality_partition(
        _read(_read(raw, "input_tokens_details"), "cached_tokens_details"), usage.cache_read_input_tokens, openai=True
    )
    return usage


def from_gemini(raw: Any) -> TokenUsage:
    """Extract usage from Gemini's usage_metadata field names."""
    if raw is None:
        return TokenUsage()

    prompt_tokens = _as_int(_read(raw, "prompt_token_count"))
    candidate_tokens = _as_int(_read(raw, "candidates_token_count"))
    total_tokens = _as_int(_read(raw, "total_token_count"))
    tool_use_prompt_tokens = _as_int(_read(raw, "tool_use_prompt_token_count"))
    reasoning_tokens = _as_int(_read(raw, "thoughts_token_count"))
    input_tokens = prompt_tokens + tool_use_prompt_tokens
    output_tokens = candidate_tokens + reasoning_tokens

    if total_tokens == 0:
        total_tokens = input_tokens + output_tokens

    cache_write_raw = _read(raw, "cache_write_input_token_count")
    cache_write = (
        _as_int(cache_write_raw)
        if cache_write_raw is not None
        else _as_int(
            _read(raw, "cache_creation_input_token_count"),
        )
    )
    usage = TokenUsage(
        input_tokens=input_tokens,
        output_tokens=output_tokens,
        total_tokens=total_tokens,
        cache_read_input_tokens=_as_int(_read(raw, "cached_content_token_count")),
        cache_write_input_tokens=cache_write,
        reasoning_tokens=reasoning_tokens,
        input_semantics=TokenInputSemantics.INCLUSIVE,
    )

    usage.input_by_modality = _modality_partition(_read(raw, "prompt_tokens_details"), prompt_tokens)
    usage.output_by_modality = _modality_partition(
        _read(raw, "candidates_tokens_details"), output_tokens, thinking=reasoning_tokens
    )
    usage.cache_read_by_modality = _modality_partition(
        _read(raw, "cache_tokens_details"), usage.cache_read_input_tokens
    )
    if tool_use_prompt_tokens:
        from .models import ModalityTokenCounts

        usage.input_by_modality = usage.input_by_modality or ModalityTokenCounts()
        usage.input_by_modality.tokens["tool_use"] = tool_use_prompt_tokens
    return usage


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
        # A flat input_tokens key is a guess, not a verified Anthropic payload:
        # an unknown provider could already report inclusively, so neither the
        # Anthropic sum-up nor the marker may be applied here. Provider-raw,
        # UNSPECIFIED semantics; consumers fall back to provider heuristics.
        return _flat_raw_usage(raw).normalize()

    return from_generic(raw)


def _read(value: Any, key: str, default: Any = None) -> Any:
    if value is None:
        return default

    if isinstance(value, Mapping):
        return value.get(key, default)

    if hasattr(value, key):
        return getattr(value, key)

    return default


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


def _modality_partition(raw: Any, total: int, *, thinking: int = 0, openai: bool = False):
    from .models import ModalityTokenCounts

    if raw is None:
        return None
    tokens: dict[str, int] = {}
    if openai:
        for modality in ("text", "image", "audio", "video"):
            value = _read(raw, modality + "_tokens")
            if value is not None:
                tokens[modality] = _as_int(value)
    else:
        for item in raw:
            reported_modality = _read(item, "modality")
            modality = str(getattr(reported_modality, "value", reported_modality) or "unknown").lower()
            value = _read(item, "token_count")
            if value is None:
                value = _read(item, "tokens")
            tokens[modality] = tokens.get(modality, 0) + _as_int(value)
    if thinking:
        tokens["text"] = tokens.get("text", 0) + thinking
    return ModalityTokenCounts(tokens=tokens, complete=sum(tokens.values()) == total)


def from_openai_images(raw: Any) -> TokenUsage:
    """Map Images usage without estimating missing modality or cache counts."""
    return from_openai_responses(raw)


def from_gemini_interactions(raw: Any) -> TokenUsage:
    """Map final per-interaction usage; callers must not sum cumulative events."""
    if raw is None:
        return TokenUsage()
    from .models import ModalityTokenCounts

    thinking = _as_int(_read(raw, "total_thought_tokens"))
    usage = TokenUsage(
        input_tokens=_as_int(_read(raw, "total_input_tokens")),
        output_tokens=_as_int(_read(raw, "total_output_tokens")) + thinking,
        reasoning_tokens=thinking,
        cache_read_input_tokens=_as_int(_read(raw, "total_cached_tokens")),
        total_tokens=_as_int(_read(raw, "total_tokens")),
        input_semantics=TokenInputSemantics.INCLUSIVE,
    )
    usage.input_by_modality = _modality_partition(_read(raw, "input_tokens_by_modality"), usage.input_tokens)
    usage.output_by_modality = _modality_partition(
        _read(raw, "output_tokens_by_modality"), usage.output_tokens, thinking=thinking
    )
    usage.cache_read_by_modality = _modality_partition(
        _read(raw, "cached_tokens_by_modality"), usage.cache_read_input_tokens
    )
    tool_tokens = _as_int(_read(raw, "total_tool_use_tokens"))
    if tool_tokens:
        usage.input_tokens += tool_tokens
        usage.input_by_modality = usage.input_by_modality or ModalityTokenCounts()
        usage.input_by_modality.tokens["tool_use"] = tool_tokens
    return usage.normalize()
