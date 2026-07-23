"""
Secret redaction engine for agento11y content capture.

~20 high-confidence patterns hand-curated from Gitleaks
(https://github.com/gitleaks/gitleaks). Two tiers:
  - Tier 1: definite secret formats and optional email addresses
    used by both redact() and redact_lightweight()
  - Tier 2: heuristic env patterns
    used only by redact()

Add more patterns when concrete unredacted secrets are observed.
"""

from __future__ import annotations

import logging
import re
from dataclasses import dataclass

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
    AGENTO11Y_REDACT_INPUT_MESSAGES (legacy SIGIL_REDACT_INPUT_MESSAGES) and
    then to False (the current opencode plugin behavior). Set it explicitly to
    override the env var.

    `redact_email_addresses` defaults to True. Callers can opt out when email
    addresses should be preserved.
    """

    redact_input_messages: bool | None = None
    redact_email_addresses: bool = True


# --- Tier 1: High-confidence patterns (definite secret formats) ---
_TIER1_PATTERNS: tuple[_SecretPattern, ...] = (
    _SecretPattern("grafana-cloud-token", re.compile(r"\bglc_[A-Za-z0-9_-]{20,}")),
    _SecretPattern("grafana-service-account-token", re.compile(r"\bglsa_[A-Za-z0-9_-]{20,}")),
    _SecretPattern("aws-access-token", re.compile(r"\b(?:A3T[A-Z0-9]|AKIA|ASIA|ABIA|ACCA)[A-Z2-7]{16}\b")),
    _SecretPattern("github-pat", re.compile(r"\bghp_[A-Za-z0-9_]{36,}")),
    _SecretPattern("github-oauth", re.compile(r"\bgho_[A-Za-z0-9_]{36,}")),
    _SecretPattern("github-app-token", re.compile(r"\bghs_[A-Za-z0-9_]{36,}")),
    _SecretPattern("github-fine-grained-pat", re.compile(r"\bgithub_pat_[A-Za-z0-9_]{82}")),
    _SecretPattern("anthropic-api-key", re.compile(r"\bsk-ant-api03-[a-zA-Z0-9_-]{93}AA")),
    _SecretPattern("anthropic-admin-key", re.compile(r"\bsk-ant-admin01-[a-zA-Z0-9_-]{93}AA")),
    _SecretPattern("openai-api-key", re.compile(r"\bsk-[a-zA-Z0-9]{20}T3BlbkFJ[a-zA-Z0-9]{20}")),
    _SecretPattern("openai-project-key", re.compile(r"\bsk-proj-[a-zA-Z0-9_-]{40,}")),
    _SecretPattern("openai-svcacct-key", re.compile(r"\bsk-svcacct-[a-zA-Z0-9_-]{40,}")),
    _SecretPattern("gcp-api-key", re.compile(r"\bAIza[A-Za-z0-9_-]{35}")),
    _SecretPattern(
        "private-key",
        re.compile(r"-----BEGIN[A-Z ]*PRIVATE KEY-----[\s\S]*?-----END[A-Z ]*PRIVATE KEY-----"),
    ),
    _SecretPattern("connection-string", re.compile(r"(?:postgres|mysql|mongodb|redis|amqp):\/\/[^\s'\"]+@[^\s'\"]+")),
    _SecretPattern("bearer-token", re.compile(r"[Bb]earer\s+[A-Za-z0-9_.\-~+/]{20,}={0,3}")),
    _SecretPattern("slack-token", re.compile(r"\bxox[bporas]-[A-Za-z0-9-]{10,}")),
    _SecretPattern("stripe-key", re.compile(r"\b[sr]k_(?:live|test)_[A-Za-z0-9]{20,}")),
    _SecretPattern("sendgrid-api-key", re.compile(r"\bSG\.[A-Za-z0-9_-]{22}\.[A-Za-z0-9_-]{43}")),
    _SecretPattern("twilio-api-key", re.compile(r"\bSK[a-f0-9]{32}")),
    _SecretPattern("npm-token", re.compile(r"\bnpm_[A-Za-z0-9]{36}")),
    _SecretPattern("pypi-token", re.compile(r"\bpypi-[A-Za-z0-9_-]{50,}")),
)

_EMAIL_PATTERN = _SecretPattern("email", re.compile(r"\b[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}\b", re.IGNORECASE))

# --- Tier 2: Heuristic patterns (env file values) ---
_TIER2_PATTERNS: tuple[_SecretPattern, ...] = (
    _SecretPattern(
        "env-secret-value",
        re.compile(
            r"((?:PASSWORD|SECRET|TOKEN|KEY|CREDENTIAL|API_KEY|PRIVATE_KEY|ACCESS_KEY)\s*[=:]\s*)([^\s\"{}\[\],]+)",
            re.IGNORECASE,
        ),
    ),
)


class _SecretRedactor:
    """Regex-based redactor with full and lightweight modes."""

    def __init__(self, include_email_addresses: bool) -> None:
        self._include_email_addresses = include_email_addresses

    # Full redaction: tier 1 + tier 2. Use for tool call args and tool results.
    def redact(self, text: str) -> str:
        result = _apply_patterns(text, _TIER1_PATTERNS)
        if self._include_email_addresses:
            result = _apply_pattern(result, _EMAIL_PATTERN)
        return _apply_tier2_patterns(result)

    # Lightweight redaction: tier 1 only. Use for assistant text and reasoning.
    def redact_lightweight(self, text: str) -> str:
        result = _apply_patterns(text, _TIER1_PATTERNS)
        if self._include_email_addresses:
            result = _apply_pattern(result, _EMAIL_PATTERN)
        return result


def _resolve_redact_input_messages(
    explicit: bool | None,
    env: dict[str, str] | None = None,
) -> bool:
    """Resolve input-message redaction: explicit > env > ``False``.

    ``AGENTO11Y_REDACT_INPUT_MESSAGES`` (legacy ``SIGIL_REDACT_INPUT_MESSAGES``)
    accepts ``1/0``, ``true/false``, ``yes/no``, ``on/off`` (case-insensitive)
    and is consulted only when ``explicit`` is ``None``. An unrecognised env
    value logs a warning naming the selected key and falls back to ``False``,
    never to the other spelling, so a typo cannot silently flip redaction.
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

        for message in generation.input:
            if redact_inputs:
                if message.role == MessageRole.USER:
                    mode = "full"
                elif message.role == MessageRole.ASSISTANT:
                    mode = "light"
                elif message.role == MessageRole.TOOL:
                    mode = "full"
                else:
                    mode = "none"
            else:
                mode = "none"
            _sanitize_message(message, redactor, mode)

        for message in generation.output:
            if message.role == MessageRole.ASSISTANT:
                mode = "light"
            elif message.role == MessageRole.TOOL:
                mode = "full"
            else:
                mode = "none"
            _sanitize_message(message, redactor, mode)

        return generation

    return _sanitize


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


def _apply_patterns(text: str, patterns: tuple[_SecretPattern, ...]) -> str:
    result = text
    for pattern in patterns:
        result = _apply_pattern(result, pattern)
    return result


def _apply_pattern(text: str, pattern: _SecretPattern) -> str:
    return pattern.regex.sub(f"[REDACTED:{pattern.id}]", text)


def _apply_tier2_patterns(text: str) -> str:
    result = text
    for pattern in _TIER2_PATTERNS:
        result = pattern.regex.sub(rf"\1[REDACTED:{pattern.id}]", result)
    return result
