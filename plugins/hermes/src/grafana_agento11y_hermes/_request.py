"""Facts read out of the provider request that ``pre_api_request`` carries.

Hermes passes the call it is about to make as ``{"method": ..., "body":
<provider kwargs>}``. The body is the literal provider payload, unnormalized,
so where each field sits follows ``api_mode``:

- system prompt: ``body["system"]`` under ``anthropic_messages`` (a string or a
  content-block list) and ``bedrock_converse`` (blocks with no ``type`` key),
  ``body["instructions"]`` under ``codex_responses``, and nowhere on the body
  under ``chat_completions``, where the leading ``request_messages`` entry is
  the only copy
- output limit: ``max_tokens``, or ``max_completion_tokens`` on the OpenAI
  routes that reject that name, ``max_output_tokens`` under
  ``codex_responses``, ``inferenceConfig.maxTokens`` under ``bedrock_converse``
- tools: ``body["tools"]``, or ``body["toolConfig"]["tools"]`` wrapped in a
  ``toolSpec`` envelope under ``bedrock_converse``

The body is not raw. Hermes runs every hook payload through
``_sanitize_hook_payload`` against ``HERMES_PLUGIN_PAYLOAD_MAX_CHARS`` (50000
by default), in three passes:

1. unconditional: a string over 8000 chars gains a ``...[truncated N chars]``
   suffix, a list or dict over 200 entries gains a ``{"_truncated_items": N}``
   sentinel
2. still over the cap: the same, at 1000 chars and 50 entries
3. still over the cap: the whole envelope is replaced by ``{"_truncated":
   True, "original_type": ..., "preview": ...}``, which has no ``body`` key

Hermes's own system prompt and tool schemas cross that cap on ordinary
sessions, so a degraded payload is the normal case, not the exception.
``parse`` reports what it could not read, distinguishing a lost body
(``truncated``) from a field hermes shortened in place (``system_prompt_clipped``,
``tools_clipped``), and never raises.
"""

from __future__ import annotations

import logging
import re
from dataclasses import dataclass, field
from typing import Any

from ._coerce import as_optional_float, as_optional_int, coerce_text

logger = logging.getLogger(__name__)

# Roles that carry the system prompt when it travels as a message. GPT-5 and
# Codex models take it as ``developer`` rather than ``system``.
_SYSTEM_ROLES = frozenset({"system", "developer"})

# What hermes leaves behind when it shortens a value in place.
_CLIPPED_TEXT = re.compile(r"\.\.\.\[truncated \d+ chars\]")
_CLIP_SENTINEL = "_truncated_items"
# Hermes stops recursing at depth 8, so nothing it clipped sits below that.
_MAX_SCAN_DEPTH = 8


@dataclass(slots=True)
class RequestFacts:
    """What one ``pre_api_request`` payload yielded, with gaps left empty."""

    system_prompt: str = ""
    # list[agento11y.ToolDefinition]. Left untyped so this module needs no
    # import-time dependency on the SDK.
    tools: list = field(default_factory=list)
    max_tokens: int | None = None
    temperature: float | None = None
    top_p: float | None = None
    tool_choice: str | None = None
    # Hermes collapsed the envelope, so the provider body was unreadable.
    truncated: bool = False
    # Hermes shortened this field in place: the value is present but partial,
    # so a complete earlier capture of it is worth more.
    system_prompt_clipped: bool = False
    tools_clipped: bool = False


def parse(
    request: Any,
    *,
    system_prompt: Any = None,
    request_messages: Any = None,
) -> RequestFacts:
    """Read one ``pre_api_request`` payload into a ``RequestFacts``.

    ``system_prompt`` is the kwarg hermes added after 0.20.1 and no released
    version sends; it is preferred where it exists because it is the unclipped
    text. ``request_messages`` is the other unsanitized kwarg, and the only
    place a ``chat_completions`` system prompt appears.

    A body hermes reshaped in a way this does not expect is missing data, not
    an error, so nothing here propagates an exception to the hook.
    """
    facts = RequestFacts()

    body: Any = {}
    try:
        candidate = request.get("body") if isinstance(request, dict) else None
        if not isinstance(request, dict) or request.get("_truncated") or not isinstance(candidate, dict):
            # No readable body. The two unsanitized kwargs are still worth
            # reading, so carry on against an empty one rather than returning.
            facts.truncated = True
        else:
            body = candidate
    except Exception as exc:
        # The reads below are inside their own guards, but this one decides
        # what they read from, so it cannot be left to them.
        logger.debug("grafana-agento11y-hermes: could not read the request envelope: %s", exc)
        facts.truncated = True

    inference: Any = {}

    try:
        candidate = body.get("inferenceConfig")
        if isinstance(candidate, dict):
            inference = candidate
        facts.system_prompt, facts.system_prompt_clipped = _system_prompt(body, system_prompt, request_messages)
        facts.max_tokens = _output_cap(
            (body, "max_tokens"),
            (body, "max_completion_tokens"),
            (body, "max_output_tokens"),
            (inference, "maxTokens"),
        )
        facts.temperature = as_optional_float(_first_present((body, "temperature"), (inference, "temperature")))
        facts.top_p = as_optional_float(_first_present((body, "top_p"), (inference, "topP")))
        facts.tool_choice = _tool_choice(body.get("tool_choice"))
    except Exception as exc:
        logger.debug("grafana-agento11y-hermes: could not read the request body: %s", exc)

    # Tools last, in their own block. The mapping runs through a private SDK
    # path, so a break there must not also cost the sampling params above.
    try:
        raw_tools = body.get("tools")
        if raw_tools is None:
            config = body.get("toolConfig")
            raw_tools = config.get("tools") if isinstance(config, dict) else None
        facts.tools_clipped = _carries_clip_marker(raw_tools)
        facts.tools = _tool_definitions(_flatten_tool_specs(raw_tools))
    except Exception as exc:
        logger.debug("grafana-agento11y-hermes: could not read the request tools: %s", exc)

    return facts


def _system_prompt(body: dict, system_prompt: Any, request_messages: Any) -> tuple[str, bool]:
    """Resolve the system prompt, and say whether hermes clipped what we took.

    The candidates are in preference order, except that a complete one beats a
    clipped one ahead of it: hermes sanitizes the body but passes
    ``system_prompt`` and ``request_messages`` as it built them.
    """
    candidates = (
        system_prompt,
        body.get("system"),
        body.get("instructions"),
        _leading_system_message(request_messages),
    )
    clipped_text = ""
    for raw in candidates:
        text = coerce_text(raw)
        if not text:
            continue
        if not _carries_clip_marker(raw):
            return text, False
        if not clipped_text:
            clipped_text = text
    return clipped_text, bool(clipped_text)


def _leading_system_message(messages: Any) -> Any:
    """Content of a leading system message, which ``chat_completions`` needs.

    That mode puts no ``system`` key on the body at all, so the first message
    is the only copy hermes sends.
    """
    if not isinstance(messages, list) or not messages:
        return None
    first = messages[0]
    if not isinstance(first, dict) or first.get("role") not in _SYSTEM_ROLES:
        return None
    return first.get("content")


def _first_present(*sources: tuple[dict, str]) -> Any:
    """First ``(mapping, key)`` pair that holds a value other than ``None``.

    A present ``0`` or ``0.0`` is a real setting, so the search is on ``None``
    rather than on falsiness.
    """
    for mapping, key in sources:
        value = mapping.get(key)
        if value is not None:
            return value
    return None


def _output_cap(*sources: tuple[dict, str]) -> int | None:
    """First ``(mapping, key)`` pair holding a usable output limit.

    Walking on past a value that is unreadable or at or below zero, rather than
    stopping at the first key present, is what hermes's own reader
    ``_requested_output_cap_from_api_kwargs`` (``run_agent.py``) does. A zero
    cap is not a setting the way a zero temperature is: it would cap the
    response at nothing, and hermes never puts it on the wire. That reader
    tries the same three names in the opposite order, which decides nothing
    here, because each transport writes exactly one cap key.
    """
    for mapping, key in sources:
        cap = as_optional_int(mapping.get(key))
        if cap is not None and cap > 0:
            return cap
    return None


def _flatten_tool_specs(tools: Any) -> Any:
    """Unwrap the ``bedrock_converse`` ``toolSpec`` envelope, pass the rest through.

    Converse nests each tool as ``{"toolSpec": {"name", "description",
    "inputSchema": {"json": ...}}}``, which is the Anthropic flat shape with
    two extra wrappers. Every other mode already sends a shape the SDK mapper
    reads, and the clipped-list sentinel has no ``toolSpec``, so both fall
    through untouched.
    """
    if not isinstance(tools, list):
        return tools
    out = []
    for entry in tools:
        spec = entry.get("toolSpec") if isinstance(entry, dict) else None
        if not isinstance(spec, dict):
            out.append(entry)
            continue
        schema = spec.get("inputSchema")
        out.append(
            {
                "name": spec.get("name"),
                "description": spec.get("description"),
                "input_schema": schema.get("json") if isinstance(schema, dict) else schema,
            }
        )
    return out


def _tool_definitions(tools: Any) -> list:
    """Map the request's tool list through the SDK's own request mapper.

    ``payload_mapping`` reads the OpenAI nested, Responses flat and Anthropic
    flat shapes, and skips an entry with no name, which is what drops the
    ``{"_truncated_items": N}`` sentinel hermes appends to a clipped list. It
    is not a public export; the ``agento11y>=0.17,<0.18`` pin in
    ``pyproject.toml`` is what keeps that coupling under review.
    """
    from agento11y.payload_mapping import tool_definitions

    return tool_definitions(tools)


def _carries_clip_marker(value: Any, depth: int = 0) -> bool:
    """True when hermes shortened this value or anything under it.

    Both markers survive into the body: the ``...[truncated N chars]`` suffix
    on a clipped string and the ``{"_truncated_items": N}`` sentinel on a
    clipped list or dict. The sentinel has to be read here, before the SDK
    mapper drops it for having no name.
    """
    if depth > _MAX_SCAN_DEPTH:
        return False
    if isinstance(value, str):
        return bool(_CLIPPED_TEXT.search(value))
    if isinstance(value, dict):
        if _CLIP_SENTINEL in value:
            return True
        return any(_carries_clip_marker(item, depth + 1) for item in value.values())
    if isinstance(value, list):
        return any(_carries_clip_marker(item, depth + 1) for item in value)
    return False


def _tool_choice(value: Any) -> str | None:
    """Collapse a tool-choice object to the string ``GenerationStart`` takes.

    Anthropic and the Responses API send ``{"type": "auto"}`` or
    ``{"type": "tool", "name": ...}``; chat completions sends the bare string
    or ``{"type": "function", "function": {"name": ...}}``. A forced tool keeps
    its name, as ``type:name``, because which tool was forced is the whole
    content of that choice.
    """
    if isinstance(value, str):
        return value.strip() or None
    if not isinstance(value, dict):
        return None
    choice = value.get("type")
    if not isinstance(choice, str) or not choice:
        return None
    name = value.get("name")
    if not isinstance(name, str):
        function = value.get("function")
        name = function.get("name") if isinstance(function, dict) else None
    return f"{choice}:{name}" if isinstance(name, str) and name else choice
