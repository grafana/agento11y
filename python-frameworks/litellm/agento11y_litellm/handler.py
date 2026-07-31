"""LiteLLM callback handler that exports generations to Agent Observability."""

from __future__ import annotations

import logging
from collections.abc import Sequence
from datetime import datetime, timezone
from typing import Any

from agento11y import Client
from agento11y.models import (
    EmbeddingResult,
    EmbeddingStart,
    Generation,
    GenerationMode,
    GenerationStart,
    Message,
    MessageRole,
    ModelRef,
    Part,
    PartKind,
    TokenUsage,
    ToolCall,
    ToolDefinition,
    ToolResult,
)
from agento11y.payload_mapping import content_parts, tool_definitions
from agento11y.usage import map_usage
from litellm.integrations.custom_logger import CustomLogger

logger = logging.getLogger(__name__)

_CHAT_CALL_TYPES = frozenset(
    {
        "completion",
        "acompletion",
        "text_completion",
        "atext_completion",
    }
)

_RESPONSES_CALL_TYPES = frozenset({"responses", "aresponses"})

_ANTHROPIC_MESSAGES_CALL_TYPES = frozenset({"anthropic_messages", "aanthropic_messages"})

_GENERATION_CALL_TYPES = _CHAT_CALL_TYPES | _RESPONSES_CALL_TYPES | _ANTHROPIC_MESSAGES_CALL_TYPES

_EMBEDDING_CALL_TYPES = frozenset({"embedding", "aembedding"})

# Provider recorded when LiteLLM never resolved one. Failures raised before a
# deployment is picked (budget, auth, no healthy deployment) carry an empty
# custom_llm_provider, and the SDK rejects a generation without a provider, so
# without a sentinel the failure is not recorded at all.
#
# The SDK's own marker for an unresolved provider is "custom"
# (``_is_unknown_provider`` in ``agento11y/framework_handler.py``). The LiteLLM
# name is used instead so the value points at what produced the event; the
# tradeoff is that these events do not read as "provider unknown" to SDK-side
# checks that only look for "" or "custom".
_UNKNOWN_PROVIDER_SENTINEL = "litellm"

_UNKNOWN_MODEL_SENTINEL = "unknown"

# Responses API content part for a refused turn. It keeps its text in
# ``refusal`` instead of ``text`` (LiteLLM's own ``ContentPartDonePartRefusal``).
_REFUSAL_CONTENT_PART_TYPE = "refusal"

# Content part types that carry plain text. ``text`` covers OpenAI chat and
# Anthropic content blocks; ``input_text``/``output_text`` cover the Responses
# API. A refusal is the only text a refused turn has, so it is read here too,
# the way the OpenAI provider wrappers surface it.
_TEXT_CONTENT_PART_TYPES = frozenset({"text", "input_text", "output_text", _REFUSAL_CONTENT_PART_TYPE})

# Content part types inside a Responses API ``reasoning`` output item. OpenAI
# sends ``summary_text``/``reasoning_text``; LiteLLM's chat-to-Responses bridge,
# which serves every provider without a native Responses config, sends
# ``output_text``.
_REASONING_TEXT_PART_TYPES = frozenset({"summary_text", "reasoning_text", "output_text", "text"})

# Responses API items that carry tool history. Chat hangs a call off an assistant
# message and sends the result as a ``tool`` role message; the Responses API
# sends both as top-level items with no role at all. Other tool item types
# (``computer_call``, ``mcp_call``, ``image_generation_call``) keep their payload
# in shapes of their own and are not mapped.
_RESPONSES_TOOL_CALL_ITEM_TYPE = "function_call"
_RESPONSES_TOOL_RESULT_ITEM_TYPE = "function_call_output"
_RESPONSES_TOOL_ITEM_TYPES = frozenset({_RESPONSES_TOOL_CALL_ITEM_TYPE, _RESPONSES_TOOL_RESULT_ITEM_TYPE})

# Metadata keys consulted, in order, to name the agent behind a request.
# ``agent_id`` is LiteLLM's own identity field, filled in by the proxy from the
# ``x-litellm-agent-id`` header or from an ``agent_id`` on the calling virtual
# key, so callers that know nothing about agento11y are still attributed to
# themselves rather than to the proxy. Override via ``agent_name_metadata_keys``
# to consult different keys, or pass ``("agent_name",)`` to opt out.
DEFAULT_AGENT_NAME_METADATA_KEYS = ("agent_name", "agent_id")

# Metadata keys consulted, in order, to group requests into one conversation,
# followed by LiteLLM's own session fields.
_CONVERSATION_ID_METADATA_KEYS = ("conversation_id", "session_id", "thread_id")
_LITELLM_SESSION_KEYS = ("litellm_session_id", "litellm_trace_id")

FRAMEWORK_TAGS = {
    "agento11y.framework.name": "litellm",
    "agento11y.framework.source": "handler",
    "agento11y.framework.language": "python",
}


def _make_tool_call_part(*, call_id: str, name: str, arguments: str) -> Part:
    """Build an agento11y TOOL_CALL Part from normalized arguments."""
    return Part(
        kind=PartKind.TOOL_CALL,
        tool_call=ToolCall(
            id=call_id,
            name=name,
            input_json=arguments.encode("utf-8"),
        ),
    )


def _map_messages(messages: Any) -> tuple[list[Message], str]:
    """Map a logged LiteLLM request to agento11y Messages, extracting system prompt.

    Chat routes log OpenAI-format message dicts. Text completion routes log the
    raw ``prompt`` instead (``function_setup`` in LiteLLM's ``utils.py``), which
    is a string, a list of strings, or a list of token-id lists. Token ids carry
    no text, so they are skipped.

    ``/v1/messages`` is the exception: LiteLLM normalizes the *response* to chat
    shape but logs the request messages as they arrived, so content is a list of
    Anthropic blocks (``tool_use``, ``tool_result``, ``thinking``). The same
    ``anthropic_messages`` call type can also carry OpenAI-shaped messages once
    a caller replays LiteLLM's own normalized history, so both vocabularies are
    read per message instead of branching on the route.

    ``/v1/responses`` sends its ``input`` list here too, and tool history in that
    list is not role-shaped: the model's call is a top-level ``function_call``
    item and the result a ``function_call_output`` item, both mapped before the
    role dispatch.
    """
    if not messages:
        return [], ""

    if isinstance(messages, str):
        messages = [messages]

    if not isinstance(messages, list):
        return [], ""

    out: list[Message] = []
    system_chunks: list[str] = []

    for msg in messages:
        if isinstance(msg, str):
            if msg:
                out.append(Message(role=MessageRole.USER, parts=[Part(kind=PartKind.TEXT, text=msg)]))
            continue

        if not isinstance(msg, dict):
            continue

        item_type = str(msg.get("type") or "").lower()
        if item_type in _RESPONSES_TOOL_ITEM_TYPES:
            tool_message = _map_responses_tool_item(msg, item_type)
            if tool_message is not None:
                out.append(tool_message)
            continue

        role = (msg.get("role") or "").lower()
        raw_content = msg.get("content")

        if role in {"system", "developer"}:
            content = _extract_text_content(raw_content)
            if content:
                system_chunks.append(content)
            continue

        mapped_role = MessageRole.USER
        if role == "assistant":
            mapped_role = MessageRole.ASSISTANT
        elif role == "tool":
            mapped_role = MessageRole.TOOL

        if mapped_role == MessageRole.TOOL:
            # OpenAI-shaped tool message: the result is the whole content and
            # the ids live on the message. Anthropic puts results in
            # ``tool_result`` blocks of a user message, handled below.
            out.append(
                _tool_result_message(
                    content=_extract_text_content(raw_content),
                    tool_call_id=msg.get("tool_call_id", ""),
                    name=msg.get("name", ""),
                )
            )
            continue

        parts = content_parts(raw_content)

        if mapped_role == MessageRole.ASSISTANT and not any(part.kind == PartKind.THINKING for part in parts):
            # OpenAI-shaped reasoning lives beside the content, not in it. Skip
            # it when the content already carried thinking blocks, since
            # ``reasoning_content`` repeats them.
            parts = _map_thinking_parts(msg) + parts

        if not parts:
            # Content shapes ``content_parts`` has nothing to say about: a bare
            # string, or a dict that only stringifies.
            content = _extract_text_content(raw_content)
            if content:
                parts.append(Part(kind=PartKind.TEXT, text=content))

        if mapped_role == MessageRole.ASSISTANT:
            parts.extend(_map_tool_call_parts(msg.get("tool_calls")))

        if not parts:
            continue

        if any(part.kind == PartKind.TOOL_RESULT for part in parts):
            # An Anthropic ``tool_result`` block arrives on a user message.
            mapped_role = MessageRole.TOOL

        out.append(Message(role=mapped_role, parts=parts))

    return out, "\n\n".join(system_chunks)


def _map_responses_tool_item(item: dict[str, Any], item_type: str) -> Message | None:
    """Map a Responses API ``function_call``/``function_call_output`` item.

    A ``function_call`` becomes an assistant TOOL_CALL and a
    ``function_call_output`` a TOOL_RESULT, which is what the chat shape maps to,
    so a rule that matches tool history matches the same conversation on either
    route. Dropping them would leave a tool-using turn looking like it called
    nothing: preflight tool filters would see no history to deny on, and the
    recorded generation would lose its calls and results.

    ``call_id`` pairs a call with its result. ``id`` is the item's own id and is
    only a fallback, matching ``_map_responses_output``.
    """
    call_id = str(item.get("call_id") or item.get("id") or "")
    name = str(item.get("name") or "")

    if item_type == _RESPONSES_TOOL_CALL_ITEM_TYPE:
        if not name:
            return None
        return Message(
            role=MessageRole.ASSISTANT,
            parts=[_make_tool_call_part(call_id=call_id, name=name, arguments=str(item.get("arguments") or ""))],
        )

    if item_type != _RESPONSES_TOOL_RESULT_ITEM_TYPE:
        return None

    # ``output`` is a string or a list of content parts. A result whose output is
    # not text still records the pairing, so the call it answers does not read as
    # unanswered.
    return _tool_result_message(
        content=_extract_text_content(item.get("output")),
        tool_call_id=call_id,
        name=name,
    )


def _map_thinking_parts(message: dict[str, Any]) -> list[Part]:
    """Map reasoning/thinking from an OpenAI-format message to THINKING parts.

    Prefers structured ``thinking_blocks`` (Anthropic-style, may include
    redacted blocks) and falls back to the flat ``reasoning_content`` string.
    Reading both would double-emit the same text, since ``reasoning_content``
    is usually the concatenation of the blocks.
    """
    if not isinstance(message, dict):
        return []

    blocks = message.get("thinking_blocks")
    if isinstance(blocks, list) and blocks:
        out: list[Part] = []
        for block in blocks:
            if not isinstance(block, dict):
                continue
            if (block.get("type") or "").lower() == "redacted_thinking":
                text = block.get("data") or block.get("text") or ""
            else:
                text = block.get("thinking") or block.get("text") or ""
            if text:
                out.append(Part(kind=PartKind.THINKING, thinking=text))
        if out:
            return out

    reasoning = message.get("reasoning_content")
    if isinstance(reasoning, str) and reasoning:
        return [Part(kind=PartKind.THINKING, thinking=reasoning)]

    return []


def _map_tool_call_parts(tool_calls: list[dict[str, Any]] | None) -> list[Part]:
    """Map OpenAI-format tool_calls to agento11y ToolCall parts."""
    if not tool_calls:
        return []

    out: list[Part] = []
    for tc in tool_calls:
        function = tc.get("function") if isinstance(tc, dict) else getattr(tc, "function", None)
        if function is None:
            continue

        name = function.get("name", "") if isinstance(function, dict) else getattr(function, "name", "")
        if not name:
            continue

        arguments = function.get("arguments", "") if isinstance(function, dict) else getattr(function, "arguments", "")
        call_id = tc.get("id", "") if isinstance(tc, dict) else getattr(tc, "id", "")

        out.append(_make_tool_call_part(call_id=call_id or "", name=name, arguments=arguments or ""))
    return out


def _tool_result_message(*, content: str, tool_call_id: str, name: str) -> Message:
    """Create an agento11y tool result Message."""
    return Message(
        role=MessageRole.TOOL,
        parts=[
            Part(
                kind=PartKind.TOOL_RESULT,
                tool_result=ToolResult(
                    tool_call_id=tool_call_id,
                    name=name,
                    content=content,
                ),
            )
        ],
    )


def _map_response_output(response: Any) -> list[Message]:
    """Map SLO response to agento11y output Messages.

    Reads from the StandardLoggingPayload ``response`` field (dict or str)
    so that LiteLLM redaction settings are honoured.

    Chat shape (``choices[].message``) covers every route that reaches here,
    including ``/v1/messages``: LiteLLM converts the Anthropic response to a
    chat ``ModelResponse`` before it builds the payload
    (``Logging._handle_anthropic_messages_response_logging``). A provider-shaped
    payload still falls back to the Anthropic content blocks rather than
    recording an empty turn.
    """
    if response is None:
        return []

    if isinstance(response, str):
        if not response:
            return []
        return [Message(role=MessageRole.ASSISTANT, parts=[Part(kind=PartKind.TEXT, text=response)])]

    if not isinstance(response, dict):
        return []

    choices = response.get("choices")
    if not choices:
        parts = content_parts(response.get("content"), role_hint=MessageRole.ASSISTANT)
        return [Message(role=MessageRole.ASSISTANT, parts=parts)] if parts else []

    out: list[Message] = []
    for choice in choices:
        if not isinstance(choice, dict):
            continue

        response_message = choice.get("message")
        if not isinstance(response_message, dict):
            # Text completions put the completion in choices[].text.
            text = choice.get("text")
            if isinstance(text, str) and text:
                out.append(Message(role=MessageRole.ASSISTANT, parts=[Part(kind=PartKind.TEXT, text=text)]))
            continue

        content = response_message.get("content") or ""
        parts: list[Part] = []

        parts.extend(_map_thinking_parts(response_message))

        if content:
            parts.append(Part(kind=PartKind.TEXT, text=content))

        parts.extend(_map_tool_call_parts(response_message.get("tool_calls")))

        if not parts:
            continue

        out.append(Message(role=MessageRole.ASSISTANT, parts=parts))

    return out


def _map_responses_output(response: Any) -> list[Message]:
    """Map a Responses API SLO response to agento11y output Messages.

    Unlike ``/v1/messages``, LiteLLM does not normalize ``/v1/responses`` to a
    chat ``ModelResponse``, so the logged payload keeps the native
    ``ResponsesAPIResponse`` shape: ``output`` is a list of items typed
    ``message`` (``content[].output_text``), ``reasoning``, or
    ``function_call``.
    """
    if isinstance(response, str):
        return _map_response_output(response)

    if not isinstance(response, dict):
        return []

    output = response.get("output")
    if not isinstance(output, list):
        return []

    out: list[Message] = []
    for item in output:
        if not isinstance(item, dict):
            continue

        item_type = (item.get("type") or "").lower()

        if item_type == "reasoning":
            # ``summary`` and ``content`` hold different text (a visible summary
            # and the raw reasoning), so both are kept when present.
            parts = [
                Part(kind=PartKind.THINKING, thinking=text)
                for text in (
                    _join_text_parts(item.get("summary"), _REASONING_TEXT_PART_TYPES),
                    _join_text_parts(item.get("content"), _REASONING_TEXT_PART_TYPES),
                )
                if text
            ]
            if parts:
                out.append(Message(role=MessageRole.ASSISTANT, parts=parts))
            continue

        if item_type == _RESPONSES_TOOL_CALL_ITEM_TYPE:
            tool_message = _map_responses_tool_item(item, item_type)
            if tool_message is not None:
                out.append(tool_message)
            continue

        text = _extract_text_content(item.get("content"))
        if text:
            out.append(Message(role=MessageRole.ASSISTANT, parts=[Part(kind=PartKind.TEXT, text=text)]))

    return out


def _extract_text_content(content: Any) -> str:
    """Extract text from OpenAI message content (string or content parts array)."""
    return _join_text_parts(content, _TEXT_CONTENT_PART_TYPES)


def _join_text_parts(content: Any, part_types: frozenset[str]) -> str:
    """Join the text of every content part whose ``type`` is in ``part_types``."""
    if content is None:
        return ""
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        texts = []
        for item in content:
            if isinstance(item, dict):
                part_type = item.get("type")
                if part_type not in part_types:
                    continue
                # ``text`` can be present and null: LiteLLM's chat-to-Responses
                # bridge emits one output_text part per choice even when the
                # message content is None (a tool-call-only turn).
                text = item.get("refusal") if part_type == _REFUSAL_CONTENT_PART_TYPE else item.get("text")
                if isinstance(text, str) and text:
                    texts.append(text)
            elif isinstance(item, str):
                texts.append(item)
        return " ".join(texts)
    return str(content)


def _epoch_to_utc(epoch: float | None) -> datetime | None:
    """Convert epoch seconds to UTC datetime."""
    if epoch is None or epoch == 0:
        return None
    return datetime.fromtimestamp(epoch, tz=timezone.utc)


def _datetime_to_utc(dt: datetime | None) -> datetime | None:
    """Ensure a datetime is UTC.

    Naive datetimes are assumed to be local time (matching datetime.now()
    which LiteLLM uses to create start_time/end_time).
    """
    if dt is None:
        return None
    return dt.astimezone(timezone.utc)


def _extract_stop_reason(response: Any) -> str:
    """Extract finish_reason from the SLO response dict.

    Falls back to a top-level ``stop_reason`` for the same reason
    ``_map_response_output`` falls back to content blocks: the payload is
    normally chat-shaped, but a provider-shaped one should not lose the reason.
    """
    if not isinstance(response, dict):
        return ""
    choices = response.get("choices")
    if not choices:
        return str(response.get("stop_reason") or "")
    first_choice = choices[0]
    if not isinstance(first_choice, dict):
        return ""
    return first_choice.get("finish_reason") or ""


def _extract_responses_stop_reason(response: Any) -> str:
    """Derive a stop reason from the Responses API terminal status.

    ``ResponsesAPIResponse`` has no ``finish_reason``, so ``status`` stands in
    for it. Normalized the same way the OpenAI provider wrappers in Python, Go,
    and JS do it: ``completed`` becomes ``stop``, an ``incomplete`` status
    reports ``incomplete_details.reason``, anything else is the status itself.
    """
    if not isinstance(response, dict):
        return ""

    status = str(response.get("status") or "").lower()
    details = response.get("incomplete_details")
    reason = str((details.get("reason") if isinstance(details, dict) else "") or "").lower()

    if status == "incomplete" and reason:
        return reason
    if status == "completed":
        return "stop"
    return status


def _map_tool_definitions(kwargs: dict[str, Any]) -> list[ToolDefinition]:
    """Extract tool schemas from optional_params.

    The shape follows the route, and one call type can produce more than one:
    ``/v1/messages`` against Anthropic logs Anthropic tools (flat, with
    ``input_schema``), the same call type bridged to a chat provider logs the
    translated OpenAI tools (nested under ``function``), and ``/v1/responses``
    logs flat tools with ``parameters``. ``tool_definitions`` reads all three.
    """
    optional_params = kwargs.get("optional_params") or {}
    return _map_tools_list(optional_params.get("tools"))


def _map_tools_list(tools: Any) -> list[ToolDefinition]:
    """Map an OpenAI-format ``tools`` list to agento11y ToolDefinitions.

    Takes the list itself so it serves both the logging path
    (``kwargs["optional_params"]["tools"]``) and the proxy pre-call path
    (``data["tools"]``).
    """
    if not tools or not isinstance(tools, list):
        return []

    return tool_definitions(tools)


def _safe_cast(params: dict[str, Any], key: str, cast: type) -> Any:
    """Safely cast a model parameter, returning None on missing or invalid values."""
    if key not in params:
        return None
    try:
        return cast(params[key])
    except (ValueError, TypeError):
        return None


def _normalize_embedding_inputs(inputs: Any) -> list[str]:
    """Extract embedding input text, dropping token-id inputs that aren't text.

    LiteLLM clears the input to an empty string when message logging is off, so
    an empty string is dropped to stay consistent with ``_embedding_input_count``.
    """
    if isinstance(inputs, str):
        return [inputs] if inputs else []
    if isinstance(inputs, list):
        return [item for item in inputs if isinstance(item, str)]
    return []


def _embedding_input_count(inputs: Any) -> int:
    """Count distinct embedding inputs.

    A single pre-tokenized input (``list[int]``) counts as one; a batch of
    token-id lists (``list[list[int]]``) counts each entry. LiteLLM clears the
    input to an empty string when message logging is off, so an empty string is
    treated as no input rather than a single one.
    """
    if inputs is None:
        return 0
    if isinstance(inputs, str):
        return 1 if inputs else 0
    if isinstance(inputs, list):
        if inputs and all(isinstance(item, int) for item in inputs):
            return 1
        return len(inputs)
    return 0


def _embedding_dimensions_from_response(response_obj: Any) -> int | None:
    """Read the embedding vector length from the first response item."""
    data = getattr(response_obj, "data", None)
    if not data:
        return None
    first = data[0]
    embedding = first.get("embedding") if isinstance(first, dict) else getattr(first, "embedding", None)
    if not isinstance(embedding, (list, tuple)):
        return None
    return len(embedding)


def _response_model(response_obj: Any) -> str:
    """Extract the response model name, tolerating a missing attribute."""
    return getattr(response_obj, "model", "") or ""


def _resolve_model_ref(slo: dict[str, Any]) -> ModelRef:
    """Resolve the provider and a bare model name from a LiteLLM SLO.

    ``slo["model"]`` is ``reconstruct_model_name`` output, which returns the
    router deployment string, so a proxied call reports ``openai/gpt-4o-mini``
    rather than ``gpt-4o-mini``. Cost and catalog lookups downstream match on
    the bare name, so prefer ``hidden_params["litellm_model_name"]`` (the name
    LiteLLM sent to the provider) over ``slo["model"]``, and strip one leading
    ``<custom_llm_provider>/`` segment from whichever name is used. Only that one
    prefix is stripped: an Azure deployment alias (``azure/my-deployment``) names
    a deployment rather than a catalog model, and there is no catalog name to map
    it to, so the alias is kept.

    A failure raised before LiteLLM picked a deployment (budget, auth, no
    healthy deployment) carries no provider and sometimes no model. Those get
    sentinels so SDK validation keeps the event instead of rejecting it.
    """
    provider = str(slo.get("custom_llm_provider") or "").lower()
    hidden_params = slo.get("hidden_params") or {}
    name = str(hidden_params.get("litellm_model_name") or slo.get("model") or "")

    if provider:
        name = name.removeprefix(f"{provider}/")
    else:
        provider = _UNKNOWN_PROVIDER_SENTINEL
        name = name or str(slo.get("model_group") or "")

    return ModelRef(provider=provider, name=name or _UNKNOWN_MODEL_SENTINEL)


def _routing_metadata(slo: dict[str, Any]) -> dict[str, str]:
    """Keep the router deployment identity that ``_resolve_model_ref`` strips."""
    hidden_params = slo.get("hidden_params") or {}
    routing = {
        "agento11y.framework.litellm.model": slo.get("model") or "",
        "agento11y.framework.litellm.model_group": slo.get("model_group") or "",
        "agento11y.framework.litellm.model_id": slo.get("model_id") or hidden_params.get("model_id") or "",
    }
    return {key: str(value) for key, value in routing.items() if value}


def _metadata_sources(kwargs: dict[str, Any]) -> list[dict[str, Any]]:
    """Collect the metadata dicts LiteLLM may attach to a logged request."""
    return _metadata_sources_from(kwargs.get("litellm_params") or {})


def _metadata_sources_from(container: dict[str, Any]) -> list[dict[str, Any]]:
    """Collect the metadata dicts LiteLLM may attach under ``container``.

    Chat completions carry client metadata in ``metadata``; assistant and thread
    routes use ``litellm_metadata``; metadata passed through the Router lands one
    level deeper, under ``metadata.metadata``.

    The container is ``kwargs["litellm_params"]`` on the logging path and the raw
    request body on the proxy pre-call path, where the same keys sit at the top
    level.
    """
    if not isinstance(container, dict):
        return []
    sources: list[dict[str, Any]] = []
    for key in ("metadata", "litellm_metadata"):
        candidate = container.get(key)
        if not isinstance(candidate, dict):
            continue
        sources.append(candidate)
        nested = candidate.get("metadata")
        if isinstance(nested, dict):
            sources.append(nested)
    return sources


def _resolve_conversation_id_from(container: dict[str, Any], sources: list[dict[str, Any]]) -> str:
    """Resolve a conversation id from metadata, then LiteLLM's session fields.

    Checks metadata keys first (conversation_id, session_id, thread_id), then
    LiteLLM's built-in session tracking fields (litellm_session_id,
    litellm_trace_id) in both metadata and ``container``. Returns "" when none
    is present, so callers can apply their own fallback.
    """
    value = _first_metadata_value(sources, _CONVERSATION_ID_METADATA_KEYS)
    if value:
        return value

    for key in _LITELLM_SESSION_KEYS:
        value = _first_metadata_value(sources, (key,)) or container.get(key)
        if value:
            return str(value)
    return ""


def _request_tags_from(sources: list[dict[str, Any]]) -> list[str]:
    """Collect LiteLLM request tags (``metadata.tags``) from metadata sources."""
    for source in sources:
        tags = source.get("tags")
        if isinstance(tags, list):
            return [str(tag) for tag in tags if tag]
    return []


def _first_metadata_value(sources: list[dict[str, Any]], keys: tuple[str, ...]) -> str:
    """Return the first non-empty value for ``keys``, in key priority order.

    Keys are the outer loop so that a lower-priority key in the first source
    never beats a higher-priority key in a later one.
    """
    for key in keys:
        for source in sources:
            value = source.get(key)
            if value:
                return str(value)
    return ""


def _extract_detailed_usage(response_obj: Any, slo: dict[str, Any]) -> TokenUsage:
    """Build TokenUsage with detailed breakdowns from response_obj, basic counts from SLO.

    The breakdown field names differ per route: chat responses nest them under
    ``prompt_tokens_details``/``completion_tokens_details``, the Responses API
    under ``input_tokens_details``/``output_tokens_details``. ``map_usage``
    picks the mapping from the shape it gets.
    """
    usage = TokenUsage(
        input_tokens=slo.get("prompt_tokens") or 0,
        output_tokens=slo.get("completion_tokens") or 0,
        total_tokens=slo.get("total_tokens") or 0,
    )

    if response_obj is None:
        return usage

    resp_usage = getattr(response_obj, "usage", None)
    if resp_usage is None:
        return usage

    detail = map_usage(resp_usage)
    usage.cache_read_input_tokens = detail.cache_read_input_tokens
    usage.cache_write_input_tokens = detail.cache_write_input_tokens
    usage.reasoning_tokens = detail.reasoning_tokens
    return usage


class Agento11yLiteLLMLogger(CustomLogger):
    """LiteLLM callback logger that exports generations to Agent Observability.

    Uses the agento11y SDK recorder pattern directly. The SDK handles
    batching and export internally, so this extends CustomLogger
    (not CustomBatchLogger) to avoid double-batching.
    """

    def __init__(
        self,
        *,
        client: Client,
        capture_inputs: bool = True,
        capture_outputs: bool = True,
        agent_name: str = "",
        agent_name_metadata_keys: Sequence[str] = DEFAULT_AGENT_NAME_METADATA_KEYS,
        agent_version: str = "",
        conversation_id: str = "",
        extra_tags: dict[str, str] | None = None,
        extra_metadata: dict[str, Any] | None = None,
        **kwargs: Any,
    ) -> None:
        super().__init__(**kwargs)
        self._client = client
        self._capture_inputs = capture_inputs
        self._capture_outputs = capture_outputs
        self._agent_name = agent_name
        self._agent_name_metadata_keys = tuple(agent_name_metadata_keys)
        self._agent_version = agent_version
        self._conversation_id = conversation_id
        self._extra_tags = dict(extra_tags) if extra_tags else {}
        self._extra_metadata = dict(extra_metadata) if extra_metadata else {}

    def log_success_event(self, kwargs: dict, response_obj: Any, start_time: datetime, end_time: datetime) -> None:
        self._log_event(kwargs, response_obj, start_time, end_time, is_failure=False)

    def log_failure_event(self, kwargs: dict, response_obj: Any, start_time: datetime, end_time: datetime) -> None:
        self._log_event(kwargs, response_obj, start_time, end_time, is_failure=True)

    async def async_log_success_event(
        self, kwargs: dict, response_obj: Any, start_time: datetime, end_time: datetime
    ) -> None:
        self._log_event(kwargs, response_obj, start_time, end_time, is_failure=False)

    async def async_log_failure_event(
        self, kwargs: dict, response_obj: Any, start_time: datetime, end_time: datetime
    ) -> None:
        self._log_event(kwargs, response_obj, start_time, end_time, is_failure=True)

    def _log_event(
        self,
        kwargs: dict,
        response_obj: Any,
        start_time: datetime,
        end_time: datetime,
        *,
        is_failure: bool,
    ) -> None:
        slo = kwargs.get("standard_logging_object")
        if slo is None:
            return

        call_type = slo.get("call_type") or ""
        try:
            if call_type in _EMBEDDING_CALL_TYPES:
                self._record_embedding(kwargs, response_obj, slo, start_time, end_time, is_failure=is_failure)
            else:
                self._record_generation(kwargs, response_obj, slo, start_time, end_time, is_failure=is_failure)
        except Exception:
            logger.exception("agento11y: failed to record LiteLLM event")

    def _resolve_agent_name(self, kwargs: dict[str, Any]) -> str:
        """Resolve agent_name from per-request metadata, falling back to static."""
        keys = self._agent_name_metadata_keys
        return _first_metadata_value(_metadata_sources(kwargs), keys) or self._agent_name

    def _resolve_agent_version(self, kwargs: dict[str, Any]) -> str:
        """Resolve agent_version from per-request metadata, falling back to static."""
        return _first_metadata_value(_metadata_sources(kwargs), ("agent_version",)) or self._agent_version

    def _resolve_conversation_id(self, kwargs: dict[str, Any]) -> str:
        """Resolve conversation_id from per-request metadata, falling back to static.

        Checks metadata keys first (conversation_id, session_id, thread_id),
        then LiteLLM's built-in session tracking fields (litellm_session_id,
        litellm_trace_id) in both metadata and litellm_params.
        """
        litellm_params = kwargs.get("litellm_params") or {}
        resolved = _resolve_conversation_id_from(litellm_params, _metadata_sources_from(litellm_params))
        return resolved or self._conversation_id

    def _record_generation(
        self,
        kwargs: dict[str, Any],
        response_obj: Any,
        slo: dict[str, Any],
        start_time: datetime,
        end_time: datetime,
        *,
        is_failure: bool,
    ) -> None:
        call_type = slo.get("call_type") or ""
        if call_type and call_type not in _GENERATION_CALL_TYPES:
            return

        is_stream = bool(slo.get("stream"))

        tags: dict[str, str] = dict(FRAMEWORK_TAGS)
        request_tags = slo.get("request_tags") or []
        for tag_value in request_tags:
            tag_str = str(tag_value)
            tags[f"litellm.tag.{tag_str}"] = tag_str
        # extra_tags take precedence
        tags.update(self._extra_tags)

        metadata: dict[str, Any] = {**_routing_metadata(slo), **self._extra_metadata}

        model_params = slo.get("model_parameters") or {}
        temperature = _safe_cast(model_params, "temperature", float)
        max_tokens = _safe_cast(model_params, "max_tokens", int)
        top_p = _safe_cast(model_params, "top_p", float)

        system_prompt = ""
        input_messages: list[Message] = []
        if self._capture_inputs:
            input_messages, system_prompt = _map_messages(slo.get("messages"))

        gen_id = slo.get("id") or ""
        user_id = slo.get("end_user") or ""
        conversation_id = self._resolve_conversation_id(kwargs)
        started_at = _datetime_to_utc(start_time)
        tools = _map_tool_definitions(kwargs)

        seed = GenerationStart(
            id=gen_id,
            model=_resolve_model_ref(slo),
            mode=GenerationMode.STREAM if is_stream else GenerationMode.SYNC,
            system_prompt=system_prompt,
            temperature=temperature,
            max_tokens=max_tokens,
            top_p=top_p,
            user_id=user_id,
            agent_name=self._resolve_agent_name(kwargs),
            agent_version=self._resolve_agent_version(kwargs),
            conversation_id=conversation_id,
            tags=tags,
            metadata=metadata,
            started_at=started_at,
            tools=tools,
        )

        if is_stream:
            recorder = self._client.start_streaming_generation(seed)
        else:
            recorder = self._client.start_generation(seed)

        try:
            if is_stream:
                completion_start = slo.get("completionStartTime")
                if completion_start:
                    first_token_at = _epoch_to_utc(float(completion_start))
                    if first_token_at is not None:
                        recorder.set_first_token_at(first_token_at)

            if is_failure:
                error_str = slo.get("error_str") or ""
                if error_str:
                    recorder.set_call_error(RuntimeError(error_str))

            slo_response = slo.get("response")

            if call_type in _RESPONSES_CALL_TYPES:
                map_output, extract_stop_reason = _map_responses_output, _extract_responses_stop_reason
            else:
                map_output, extract_stop_reason = _map_response_output, _extract_stop_reason

            output_messages = map_output(slo_response) if self._capture_outputs else []
            usage = _extract_detailed_usage(response_obj, slo)
            stop_reason = extract_stop_reason(slo_response)

            recorder.set_result(
                generation=Generation(
                    input=input_messages,
                    output=output_messages,
                    usage=usage,
                    stop_reason=stop_reason,
                    completed_at=_datetime_to_utc(end_time),
                ),
            )
        finally:
            recorder.end()
            err = recorder.err()
            if err is not None:
                logger.error("agento11y: generation dropped: %s", err)

    def _record_embedding(
        self,
        kwargs: dict[str, Any],
        response_obj: Any,
        slo: dict[str, Any],
        start_time: datetime,
        end_time: datetime,
        *,
        is_failure: bool,
    ) -> None:
        model_ref = _resolve_model_ref(slo)
        optional_params = kwargs.get("optional_params") or {}
        dimensions = _safe_cast(optional_params, "dimensions", int)
        encoding_format = optional_params.get("encoding_format") or ""

        tags: dict[str, str] = dict(FRAMEWORK_TAGS)
        for tag_value in slo.get("request_tags") or []:
            tag_str = str(tag_value)
            tags[f"litellm.tag.{tag_str}"] = tag_str
        tags.update(self._extra_tags)

        recorder = self._client.start_embedding(
            EmbeddingStart(
                model=model_ref,
                agent_name=self._resolve_agent_name(kwargs),
                agent_version=self._resolve_agent_version(kwargs),
                dimensions=dimensions,
                encoding_format=encoding_format,
                tags=tags,
                metadata=dict(self._extra_metadata),
                started_at=_datetime_to_utc(start_time),
            )
        )

        try:
            if is_failure:
                error_str = slo.get("error_str") or ""
                if error_str:
                    recorder.set_call_error(RuntimeError(error_str))

            # Embedding input lives in kwargs["input"], not the SLO. LiteLLM clears
            # it (sets it to "") before invoking callbacks when message logging is
            # turned off, so reading it here honours LiteLLM redaction settings.
            inputs = kwargs.get("input")
            input_texts = _normalize_embedding_inputs(inputs) if self._capture_inputs else []

            recorder.set_result(
                EmbeddingResult(
                    input_count=_embedding_input_count(inputs),
                    input_tokens=slo.get("prompt_tokens") or 0,
                    input_texts=input_texts,
                    response_model=_response_model(response_obj) or model_ref.name,
                    dimensions=dimensions or _embedding_dimensions_from_response(response_obj),
                )
            )
        finally:
            recorder.end()
            err = recorder.err()
            if err is not None:
                # Unlike a generation, the embedding span is still emitted; it
                # carries the validation error instead of the recorded values.
                logger.error("agento11y: embedding validation failed: %s", err)
