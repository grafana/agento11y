"""
Secret redaction engine for agento11y content capture.

The patterns come from the shared table in ``redaction/patterns.json`` through
the generated ``_redaction_patterns`` module, so all SDKs and plugins redact the
same strings. Two tiers:
  - Tier 1: definite secret formats and optional email addresses
    used by both redact() and redact_lightweight()
  - Tier 2: heuristic key/value patterns
    used only by redact()

To add a pattern, edit ``redaction/patterns.json`` and run
``mise run generate:redaction``.
"""

from __future__ import annotations

import logging
import re
from dataclasses import dataclass
from typing import Any

from ._redaction_patterns import BASE_FLAGS, EMAIL_PATTERN, TIER1_PATTERNS, TIER2_PATTERNS
from .config import GenerationSanitizer, _env
from .models import Generation, Message, MessageRole, Part, PartKind

_logger = logging.getLogger("agento11y")

_TRUE_TOKENS = frozenset({"1", "true", "yes", "on"})
_FALSE_TOKENS = frozenset({"0", "false", "no", "off"})


@dataclass(frozen=True, slots=True)
class _SecretPattern:
    id: str
    regex: re.Pattern[str]


@dataclass(frozen=True, slots=True)
class SecretRedactionOptions:
    """
    Options for the built-in secret redaction sanitizer.

    `redact_input_messages` is None by default, which falls back to
    AGENTO11Y_REDACT_INPUT_MESSAGES and then to False (the current opencode
    plugin behavior). Set it explicitly to
    override the env var.

    `redact_email_addresses` defaults to True. Callers can opt out when email
    addresses should be preserved.
    """

    redact_input_messages: bool | None = None
    redact_email_addresses: bool = True


@dataclass(frozen=True, slots=True)
class _Tier2Pattern:
    id: str
    regex: re.Pattern[str]
    replacement: str


_TIER1_IDS: tuple[str, ...] = tuple(pattern_id for pattern_id, _ in TIER1_PATTERNS)

# Alternating every tier 1 pattern into one regex scans each input once instead
# of once per pattern. Each pattern is wrapped in a capturing group; the matched
# group index identifies which pattern fired. The generator rejects capturing
# groups inside a tier 1 pattern, which would shift that mapping. Scanning once
# is also what keeps this output identical to the other SDKs': with per-pattern
# passes an earlier pattern can rewrite text a later one would have matched.
_TIER1_COMBINED = re.compile("|".join(f"({source})" for _, source in TIER1_PATTERNS), BASE_FLAGS)

_EMAIL_PATTERN = _SecretPattern(EMAIL_PATTERN[0], re.compile(EMAIL_PATTERN[1], EMAIL_PATTERN[2]))

_TIER2_PATTERNS: tuple[_Tier2Pattern, ...] = tuple(
    _Tier2Pattern(pattern_id, re.compile(source, flags), replacement)
    for pattern_id, source, flags, replacement in TIER2_PATTERNS
)


class _SecretRedactor:
    """Regex-based redactor with full and lightweight modes."""

    def __init__(self, include_email_addresses: bool) -> None:
        self._include_email_addresses = include_email_addresses

    # Full redaction: tier 1 + tier 2. Use for tool call args and tool results.
    def redact(self, text: str) -> str:
        return _apply_tier2_patterns(self.redact_lightweight(text))

    # Lightweight redaction: tier 1 only. Use for assistant text and reasoning.
    def redact_lightweight(self, text: str) -> str:
        result = _apply_tier1(text)
        if self._include_email_addresses:
            result = _apply_pattern(result, _EMAIL_PATTERN)
        return result


def redact_secret_text(text: str, *, redact_email_addresses: bool = True) -> str:
    """Redacts known secret formats from arbitrary experiment text."""

    return _SecretRedactor(include_email_addresses=redact_email_addresses).redact(text)


def redact_secret_value(value: Any, *, redact_email_addresses: bool = True) -> Any:
    """Recursively redacts strings while preserving a JSON-like value's shape."""

    redactor = _SecretRedactor(include_email_addresses=redact_email_addresses)

    def visit(item: Any) -> Any:
        if isinstance(item, str):
            return redactor.redact(item)
        if isinstance(item, dict):
            return {key: visit(child) for key, child in item.items()}
        if isinstance(item, list):
            return [visit(child) for child in item]
        if isinstance(item, tuple):
            return tuple(visit(child) for child in item)
        return item

    return visit(value)


def _resolve_redact_input_messages(
    explicit: bool | None,
    env: dict[str, str] | None = None,
) -> bool:
    """Resolve input-message redaction: explicit > env > ``False``.

    ``AGENTO11Y_REDACT_INPUT_MESSAGES`` (or its legacy
    ``SIGIL_REDACT_INPUT_MESSAGES`` spelling) accepts ``1/0``, ``true/false``,
    ``yes/no``, ``on/off`` (case-insensitive)
    and is consulted only when ``explicit`` is ``None``. An unrecognised env
    value logs a warning naming the selected key and falls back to ``False``,
    so a typo cannot silently flip redaction.
    """

    if explicit is not None:
        return explicit
    raw, key = _env(env, "REDACT_INPUT_MESSAGES")
    if raw is None:
        return False
    parsed = _parse_strict_bool(raw)
    if parsed is None:
        _logger.warning("agento11y: ignoring invalid %s: %s", key, raw)
        return False
    return parsed


def _parse_strict_bool(raw: str) -> bool | None:
    token = raw.strip().lower()
    if token in _TRUE_TOKENS:
        return True
    if token in _FALSE_TOKENS:
        return False
    return None


def create_secret_redaction_sanitizer(
    options: SecretRedactionOptions | None = None,
) -> GenerationSanitizer:
    """Returns a reusable generation sanitizer that redacts known secret formats."""

    resolved = options or SecretRedactionOptions()
    redactor = _SecretRedactor(include_email_addresses=resolved.redact_email_addresses)
    redact_inputs = _resolve_redact_input_messages(resolved.redact_input_messages)

    def _sanitize(generation: Generation) -> Generation:
        if generation.system_prompt:
            generation.system_prompt = redactor.redact(generation.system_prompt)

        # conversation_title and call_error are short natural-language strings;
        # lightweight redaction (tier 1 + email) avoids mangling them with the
        # tier 2 heuristics.
        if generation.conversation_title:
            generation.conversation_title = redactor.redact_lightweight(generation.conversation_title)
        if generation.call_error:
            generation.call_error = redactor.redact_lightweight(generation.call_error)

        for message in generation.input:
            _sanitize_message(message, redactor, _input_text_mode(message.role, redact_inputs))

        for message in generation.output:
            _sanitize_message(message, redactor, _output_text_mode(message.role))

        return generation

    return _sanitize


def _input_text_mode(role: MessageRole, redact_user_input: bool) -> str:
    """Picks the redaction mode for an input message.

    Historic assistant turns and tool results in input replay the same secret
    surface as the output, so they are redacted whatever the caller chose. Only
    user text waits for an explicit opt-in.
    """

    if role == MessageRole.USER:
        return "full" if redact_user_input else "none"
    if role == MessageRole.TOOL:
        return "full"
    if role == MessageRole.ASSISTANT:
        return "light"
    return "none"


def _output_text_mode(role: MessageRole) -> str:
    if role == MessageRole.ASSISTANT:
        return "light"
    if role == MessageRole.TOOL:
        return "full"
    return "none"


def _sanitize_message(message: Message, redactor: _SecretRedactor, default_text_mode: str) -> None:
    for part in message.parts:
        _sanitize_part(part, redactor, default_text_mode)


def _sanitize_part(part: Part, redactor: _SecretRedactor, default_text_mode: str) -> None:
    if default_text_mode == "none":
        return
    if part.kind == PartKind.TEXT:
        part.text = _redact_string(part.text, redactor, default_text_mode)
        return
    if part.kind == PartKind.THINKING:
        part.thinking = redactor.redact_lightweight(part.thinking)
        return
    if part.kind == PartKind.TOOL_CALL and part.tool_call is not None:
        if len(part.tool_call.input_json) > 0:
            part.tool_call.input_json = redactor.redact(part.tool_call.input_json.decode("utf-8")).encode("utf-8")
        return
    if part.kind == PartKind.MEDIA:
        # A media URL is generation content, but this sanitizer only redacts text
        # and JSON payloads. metadata_only capture is the only mode that clears a
        # media URL, and it runs instead of the sanitizer, so under every other
        # mode the URL is exported as the caller set it.
        return
    if part.kind == PartKind.TOOL_RESULT and part.tool_result is not None:
        part.tool_result.content = redactor.redact(part.tool_result.content)
        if len(part.tool_result.content_json) > 0:
            part.tool_result.content_json = redactor.redact(part.tool_result.content_json.decode("utf-8")).encode(
                "utf-8"
            )


def _redact_string(value: str, redactor: _SecretRedactor, mode: str) -> str:
    if mode == "full":
        return redactor.redact(value)
    if mode == "light":
        return redactor.redact_lightweight(value)
    return value


def _apply_tier1(text: str) -> str:
    def label(match: re.Match[str]) -> str:
        # Exactly one branch of the alternation participates in a match, so
        # lastindex is the index of the pattern that fired.
        return f"[REDACTED:{_TIER1_IDS[(match.lastindex or 1) - 1]}]"

    return _TIER1_COMBINED.sub(label, text)


def _apply_pattern(text: str, pattern: _SecretPattern) -> str:
    return pattern.regex.sub(f"[REDACTED:{pattern.id}]", text)


def _apply_tier2_patterns(text: str) -> str:
    result = text
    for pattern in _TIER2_PATTERNS:
        result = pattern.regex.sub(pattern.replacement, result)
    return result
