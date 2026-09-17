"""Hermes plugin hook handlers.

All handlers fail open: ``_fail_open`` wraps every one of them, so an exception
is logged and the hermes loop continues. If the SDK client cannot be
constructed (missing creds, SDK error), every handler short-circuits via
``client is None``.
"""

from __future__ import annotations

import functools
import json
import logging
import os
import random
import secrets
from collections.abc import Callable
from datetime import UTC, datetime, timedelta
from typing import Any

from opentelemetry import context as otel_context
from opentelemetry import trace as otel_trace

from . import _client, _errors, _redact, _request, _state, _tags
from ._coerce import as_int, as_optional_int, coerce_text

logger = logging.getLogger(__name__)

# Cap on an error message before it reaches the span status and the payload.
# Independent of AGENTO11Y_HERMES_MAX_CHARS, which bounds tool I/O.
_ERROR_MAX_CHARS = 2000

# LEGACY: cleared only by the test reset. Delete with the rest of the
# pre-v2026.6.5 fallback.
_WARNED_LEGACY_HERMES = False
_WARNED_DEPRECATED_VERSION = False
_LOGGED_TRUNCATED_REQUEST = False
# Set on the first request that carries an api_request_id. Nothing reads the
# turn-scoped convo bookkeeping on that path, so its writers stop once we know
# which hermes we are on.
_SAW_REQUEST_ID = False

# The fields of a capture that belong to the model rather than to the agent.
_SAMPLING_FIELDS = ("max_tokens", "temperature", "top_p", "tool_choice")


def _fail_open(handler: Callable[..., None]) -> Callable[..., None]:
    """Catch unhandled exceptions from every handler registered by ``__init__``.

    Inner ``try`` blocks let handlers continue after a failure, for example to
    close a recorder after losing a tool result. This guard only keeps uncaught
    exceptions from reaching the hermes loop.
    """

    # Read through getattr because the annotation is Callable, which covers
    # callables that carry no __name__.
    name = getattr(handler, "__name__", "hook")

    @functools.wraps(handler)
    def wrapper(*args: Any, **kwargs: Any) -> None:
        try:
            handler(*args, **kwargs)
        except Exception as exc:
            logger.warning("grafana-agento11y-hermes: %s failed: %s", name, exc)

    return wrapper


def _warn_legacy_hermes_once() -> None:
    """Warn that hermes is too old to pair requests exactly.

    hermes v2026.6.5, which is ``hermes-agent`` 0.16.0 on PyPI, added
    ``api_request_id`` and the input/output messages to the API-request hooks.
    Older builds fall back to matching on ``api_call_count`` and recovering
    output from ``post_llm_call``, which cannot tell apart two requests running
    concurrently in one session.
    """
    global _WARNED_LEGACY_HERMES
    if _WARNED_LEGACY_HERMES:
        return
    _WARNED_LEGACY_HERMES = True
    logger.warning(
        "grafana-agento11y-hermes: this hermes does not send api_request_id. "
        "Using the legacy matching path, which mis-attributes concurrent "
        "requests in one session. Upgrade to hermes v2026.6.5 (PyPI 0.16.0) or newer."
    )


def _warn_deprecated_version_once() -> None:
    global _WARNED_DEPRECATED_VERSION
    if _WARNED_DEPRECATED_VERSION:
        return
    _WARNED_DEPRECATED_VERSION = True
    logger.warning(
        "grafana-agento11y-hermes: AGENTO11Y_HERMES_AGENT_VERSION is deprecated. "
        "Rename it to AGENTO11Y_AGENT_VERSION, which also sets the agent_version "
        "metric dimension."
    )


def _log_truncated_request_once() -> None:
    """Note that the request payload arrived clipped, once per process.

    Hermes sanitizes every hook payload against
    ``HERMES_PLUGIN_PAYLOAD_MAX_CHARS`` (50000 by default) and, past the cap,
    replaces the whole request envelope with a preview carrying no body. The
    plugin does not raise that variable itself: a telemetry plugin must not
    change what the host hands its other plugins and every tool subprocess.

    At INFO because hermes sets ``agent.log`` to INFO, so a DEBUG line reaches
    no log in a default install. One-shot ``-z`` disables logging outright
    right after plugin discovery, where no level helps.
    """
    global _LOGGED_TRUNCATED_REQUEST
    if _LOGGED_TRUNCATED_REQUEST:
        return
    _LOGGED_TRUNCATED_REQUEST = True
    logger.info(
        "grafana-agento11y-hermes: hermes truncated the request payload, so the system "
        "prompt and tool schemas come from an earlier request in this session, where "
        "there is one. Raising HERMES_PLUGIN_PAYLOAD_MAX_CHARS recovers the tool "
        "schemas; a system prompt over 8000 chars stays clipped at any value."
    )


def _reset_for_tests() -> None:
    global _WARNED_LEGACY_HERMES, _WARNED_DEPRECATED_VERSION, _SAW_REQUEST_ID
    global _LOGGED_TRUNCATED_REQUEST
    _WARNED_LEGACY_HERMES = False
    _WARNED_DEPRECATED_VERSION = False
    _LOGGED_TRUNCATED_REQUEST = False
    _SAW_REQUEST_ID = False


def _agent_name() -> str:
    """Default agent name when the SDK can't resolve one from env/context.

    The SDK reads ``AGENTO11Y_AGENT_NAME`` itself; this fallback only kicks in
    when neither env nor a context override is set.
    """
    return os.environ.get("AGENTO11Y_AGENT_NAME", "").strip() or "hermes"


def _effective_version() -> str:
    """Version stamped on every generation as ``effective_version``.

    One variable, ``AGENTO11Y_AGENT_VERSION``: the SDK already reads it for
    ``agent_version``, which is the metric dimension, and the first-party
    plugins mirror it into ``effective_version`` too.
    ``AGENTO11Y_HERMES_AGENT_VERSION`` is the deprecated fallback, kept so
    existing installs keep working.
    """
    version = os.environ.get("AGENTO11Y_AGENT_VERSION", "").strip()
    if version:
        return version
    legacy = os.environ.get("AGENTO11Y_HERMES_AGENT_VERSION", "").strip()
    if legacy:
        _warn_deprecated_version_once()
    return legacy


def _error_fields(error: Any) -> tuple[str, str, int | None]:
    """Split whatever hermes put on the error hook into (type, message, status).

    The status code is only read here as a fallback for the hook's own
    ``status_code`` kwarg. It decides ``error.category``, so it is worth
    looking for in the payload rather than losing the whole classification.
    """
    if error is None:
        return "", "", None
    if isinstance(error, dict):
        return (
            str(error.get("type") or ""),
            str(error.get("message") or ""),
            as_optional_int(error.get("status_code") or error.get("status")),
        )
    if isinstance(error, BaseException):
        return type(error).__name__, str(error), as_optional_int(getattr(error, "status_code", None))
    if isinstance(error, str):
        return "", error, None
    return "", str(error), None


def _convo_key(task_id: str, session_id: str) -> tuple[str, str]:
    """Key for the running conversation history.

    Hermes does not pass ``task_id`` to ``pre_llm_call`` but does to
    ``pre_api_request`` — keying on session_id only is the only way both hooks
    address the same bucket. Wrapped in a tuple so the type matches what
    ``_state`` expects.
    """
    return ("", session_id or "")


def _should_sample() -> bool:
    """Return True if this trace should be recorded under AGENTO11Y_HERMES_SAMPLE_RATE.

    A pre-hook that returns False simply skips ``start_generation`` and
    never stores a recorder, so the matching post-hook becomes a natural
    no-op (``gen_pop`` returns None). Tool sampling is checked at
    ``post_tool_call`` time directly.
    """
    cfg = _client._get_plugin_config()
    if cfg is None or cfg.sample_rate >= 1.0:
        return True
    if cfg.sample_rate <= 0.0:
        return False
    return random.random() < cfg.sample_rate


def _split_system_prompt(messages: Any) -> tuple[str, list[dict]]:
    """Pull system messages out into a single prompt string, return remaining messages."""
    if not isinstance(messages, list):
        return "", []
    system_parts: list[str] = []
    rest: list[dict] = []
    for msg in messages:
        if not isinstance(msg, dict):
            continue
        if msg.get("role") == "system":
            text = coerce_text(msg.get("content"))
            if text:
                system_parts.append(text)
        else:
            rest.append(msg)
    return "\n\n".join(system_parts), rest


def _serialize_tool_calls(tool_calls: Any) -> list[dict[str, Any]]:
    if not tool_calls:
        return []
    out: list[dict[str, Any]] = []
    for tc in tool_calls:
        if isinstance(tc, dict):
            tc_id = tc.get("id", "")
            fn = tc.get("function") or {}
            name = fn.get("name") if isinstance(fn, dict) else None
            arguments = fn.get("arguments") if isinstance(fn, dict) else None
        else:
            tc_id = getattr(tc, "id", "")
            fn = getattr(tc, "function", None)
            name = getattr(fn, "name", None) if fn is not None else None
            arguments = getattr(fn, "arguments", None) if fn is not None else None
        if isinstance(arguments, str):
            try:
                arguments = json.loads(arguments)
            except Exception:
                pass
        out.append({"id": tc_id or "", "name": name or "", "arguments": arguments})
    return out


def _to_sdk_message(msg: dict[str, Any]):
    from agento11y import (
        Message,
        MessageRole,
        Part,
        ToolCall,
        ToolResult,
        text_part,
        tool_call_part,
        tool_result_part,
    )

    cfg = _client._get_plugin_config()
    max_chars = cfg.max_chars if cfg is not None else 12000
    role = msg.get("role")
    if role == "user":
        text = coerce_text(msg.get("content"))
        return Message(role=MessageRole.USER, parts=[text_part(text)] if text else [])
    if role == "tool":
        tool_call_id = msg.get("tool_call_id") or ""
        content = _redact.truncate_text(coerce_text(msg.get("content")), max_chars)
        return Message(
            role=MessageRole.TOOL,
            parts=[tool_result_part(ToolResult(tool_call_id=_redact.redact_record(tool_call_id), content=content))],
        )
    if role == "assistant":
        parts: list[Part] = []
        text = coerce_text(msg.get("content"))
        if text:
            parts.append(text_part(text))
        for tc in _serialize_tool_calls(msg.get("tool_calls")):
            input_json = b""
            if tc.get("arguments") is not None:
                try:
                    input_json = json.dumps(_redact.safe_value(tc["arguments"], max_chars=max_chars)).encode()
                except Exception:
                    input_json = b""
            parts.append(
                tool_call_part(
                    ToolCall(
                        name=_redact.redact_record(tc.get("name", "")),
                        id=_redact.redact_record(tc.get("id", "")),
                        input_json=input_json,
                    )
                )
            )
        return Message(role=MessageRole.ASSISTANT, parts=parts)
    # Unknown role (e.g. "system" should already be filtered out): drop.
    return None


def _to_sdk_messages(messages: Any) -> list:
    if not isinstance(messages, list):
        return []
    out = []
    for msg in messages:
        if not isinstance(msg, dict):
            continue
        if msg.get("role") == "system":
            continue  # handled via system_prompt
        sdk_msg = _to_sdk_message(msg)
        if sdk_msg is not None:
            out.append(sdk_msg)
    return out


def _assistant_to_sdk_messages(assistant_message: Any) -> list:
    if assistant_message is None:
        return []

    if isinstance(assistant_message, dict):
        content = assistant_message.get("content")
        tool_calls = assistant_message.get("tool_calls")
    else:
        content = getattr(assistant_message, "content", None)
        tool_calls = getattr(assistant_message, "tool_calls", None)

    msg_dict = {"role": "assistant", "content": content, "tool_calls": tool_calls}
    sdk_msg = _to_sdk_message(msg_dict)
    return [sdk_msg] if sdk_msg is not None else []


def _span_context_of(recorder: Any) -> Any:
    """The recorder's span context, or ``None``. Never raises.

    Today's ``GenerationRecorder`` always carries a span, so this is a guard
    against a future recorder that does not, or a host tracer whose span
    answers ``get_span_context`` differently. Losing the link is acceptable;
    losing the generation is not.
    """
    try:
        span = getattr(recorder, "span", None)
        return None if span is None else span.get_span_context()
    except Exception:
        return None


def _build_token_usage(usage: Any):
    from agento11y import TokenUsage

    if not isinstance(usage, dict):
        return TokenUsage()

    input_tokens = as_int(usage.get("input_tokens") or usage.get("prompt_tokens"))
    output_tokens = as_int(usage.get("output_tokens") or usage.get("completion_tokens"))
    total_tokens = as_int(usage.get("total_tokens"))
    cache_read = as_int(usage.get("cache_read_tokens") or usage.get("cache_read_input_tokens"))
    cache_write = as_int(
        usage.get("cache_write_tokens")
        or usage.get("cache_creation_input_tokens")
        or usage.get("cache_write_input_tokens")
    )
    reasoning = as_int(usage.get("reasoning_tokens"))

    return TokenUsage(
        input_tokens=input_tokens,
        output_tokens=output_tokens,
        total_tokens=total_tokens,
        cache_read_input_tokens=cache_read,
        cache_write_input_tokens=cache_write,
        reasoning_tokens=reasoning,
    )


@_fail_open
def on_pre_llm_call(
    *,
    task_id: str = "",
    session_id: str = "",
    conversation_history: Any = None,
    user_message: Any = None,
    **_: Any,
) -> None:
    """Capture the start-of-turn conversation so request-scoped hooks have an input.

    Hermes does not pass ``messages`` to ``pre_api_request``. ``pre_llm_call``
    is the only hook that receives the actual conversation as
    ``conversation_history`` (the message list at the start of the turn).
    We snapshot it here and extend in-place as the tool-calling loop runs.

    LEGACY: current hermes passes the messages to ``pre_api_request`` itself,
    so once we have seen an ``api_request_id`` nothing reads this bucket.
    """
    if _SAW_REQUEST_ID:
        return
    if not isinstance(conversation_history, list):
        return
    convo = list(conversation_history)
    if (
        isinstance(user_message, str)
        and user_message
        and not any(isinstance(m, dict) and m.get("role") == "user" and m.get("content") == user_message for m in convo)
    ):
        convo.append({"role": "user", "content": user_message})
    key = _convo_key(task_id, session_id)
    _state.convo_set(key, convo)
    # Snapshot the assistant-message count BEFORE post_tool_call extends the
    # running convo with synthesized tool-call messages. ``_close_pending_for_session``
    # uses this to peel this turn's assistant outputs off the final history.
    start_asst_count = sum(1 for m in conversation_history if isinstance(m, dict) and m.get("role") == "assistant")
    _state.turn_start_asst_count_set(key, start_asst_count)


@_fail_open
def on_post_llm_call(
    *,
    task_id: str = "",
    session_id: str = "",
    turn_id: str = "",
    conversation_history: Any = None,
    assistant_response: Any = None,
    **_: Any,
) -> None:
    """Close all pending recorders for this turn with outputs from the final convo.

    ``conversation_history`` here is the FINAL state of the turn — includes
    every assistant message (with content/tool_calls) and every tool result.
    We pair the new assistant messages with our pending recorders in order.

    Also ends the turn's generation chain, so the next turn starts a new one.
    This hook does not fire for an interrupted turn, hence the session sweep in
    ``on_session_end`` and the cap on the map itself.
    """
    _close_pending_for_session(session_id or "", conversation_history)
    key = _convo_key(task_id, session_id)
    _state.convo_clear(key)
    _state.turn_start_asst_count_clear(key)
    _state.turn_last_gen_clear(turn_id)


def _prefer_cached(current: Any, current_clipped: bool, cached: Any, cached_clipped: bool) -> bool:
    """True when the session's copy of a field beats what this request carried.

    Empty loses to anything, and a value hermes shortened in place loses to a
    complete one. Between two shortened copies the longer one kept more of the
    same prompt or tool list, so length decides. Two complete reads leave the
    current one in place: it is the fresher inventory when ``tool_search``
    swapped the toolset.
    """
    if not cached:
        return False
    if not current:
        return True
    if not current_clipped:
        return False
    return not cached_clipped or len(cached) > len(current)


def _request_facts(
    session_id: str,
    model: str,
    request: Any,
    request_messages: Any,
    system_prompt: Any,
    tool_count: int,
) -> tuple[_request.RequestFacts, bool]:
    """Read this request, filling what it lost from the session's best capture.

    Merging field by field rather than wholesale keeps a partial read winning
    where it has data. What comes out is never worse than what was cached, so
    storing it back cannot degrade the capture the next request borrows from.

    The sampling params only come from a capture made on the same model. They
    are resolved per model profile, so a session that fell back to another
    provider would otherwise report the first model's cap and temperature on
    the second model's generations. Each model keeps its own cached params so
    a one-turn fallback cannot erase the primary model's capture.

    Returns the facts and whether any field came from the cache.
    """
    facts = _request.parse(request, system_prompt=system_prompt, request_messages=request_messages)
    if facts.truncated and request is not None:
        _log_truncated_request_once()
    if tool_count and len(facts.tools) < tool_count:
        # Third signal for a clipped tool list, independent of the two markers
        # hermes leaves in the payload: this count is raw and always accurate.
        facts.tools_clipped = True

    reused = False
    entry = _state.session_facts_get(session_id, model)
    if entry is not None:
        _, cached = entry
        if _prefer_cached(
            facts.system_prompt,
            facts.system_prompt_clipped,
            cached.system_prompt,
            cached.system_prompt_clipped,
        ):
            facts.system_prompt = cached.system_prompt
            facts.system_prompt_clipped = cached.system_prompt_clipped
            reused = True
        if _prefer_cached(facts.tools, facts.tools_clipped, cached.tools, cached.tools_clipped):
            # Copied, so one list is not shared by every generation of the
            # session and by the cache entry behind them.
            facts.tools = list(cached.tools)
            facts.tools_clipped = cached.tools_clipped
            reused = True
        for name in _SAMPLING_FIELDS:
            if getattr(facts, name) is None and getattr(cached, name) is not None:
                setattr(facts, name, getattr(cached, name))
                reused = True

    # Stored on every request. The merge above leaves the prompt and the toolset
    # no worse than the cache already held, and a request that resolved only
    # sampling params is exactly the one a later clipped request needs.
    _state.session_facts_put(session_id, model, facts)
    return facts, reused


@_fail_open
def on_pre_api_request(
    *,
    task_id: str = "",
    session_id: str = "",
    model: str = "",
    provider: str = "",
    conversation_history: Any = None,
    api_request_id: str = "",
    turn_id: str = "",
    messages: Any = None,
    api_call_count: int = 0,
    max_tokens: Any = None,
    request: Any = None,
    request_messages: Any = None,
    system_prompt: Any = None,
    tool_count: int = 0,
    **_: Any,
) -> None:
    global _SAW_REQUEST_ID
    if api_request_id:
        _SAW_REQUEST_ID = True
    client = _client._get_client()
    if client is None:
        return

    # Coerced once, because the count is both compared against a length and
    # recorded. A provider that reports it as text would otherwise abort the
    # open and cost the whole generation, not just the count.
    tools_expected = as_int(tool_count)
    # Read the request before the sampling gate. The payloads that arrive
    # readable are the earliest of a session, so skipping those would leave
    # every sampled-in request with an empty capture behind it. Parsing costs
    # well under a millisecond and touches nothing but the cache.
    facts, facts_reused = _request_facts(session_id, model, request, request_messages, system_prompt, tools_expected)
    # post_tool_call carries neither model nor provider, so remember them
    # for the tool executions that follow this request.
    _state.session_model_put(session_id, model or "", provider or "")
    if not _should_sample():
        return

    from agento11y import GenerationStart, ModelRef

    # hermes v2026.6.5+ passes the input messages here as
    # ``conversation_history`` (agent/conversation_loop.py). ``messages`` is
    # accepted only because older builds and our own tests used that name.
    if not isinstance(conversation_history, list):
        conversation_history = messages
    if not isinstance(conversation_history, list):
        # LEGACY: no messages on the hook, so use the running history that
        # pre_llm_call captured and post_tool_call extends.
        _warn_legacy_hermes_once()
        conversation_history = _state.convo_get(_convo_key(task_id, session_id))
    # ``conversation_history`` carries no system message on any supported
    # hermes: it is the agent's running convo, and the system prompt is
    # prepended to the separate list that goes on the wire. The split is
    # kept for ``non_system``, which is the input, and as the last resort
    # for the prompt itself.
    history_system_prompt, non_system = _split_system_prompt(conversation_history)
    sdk_messages = _to_sdk_messages(non_system)

    resolved_system_prompt = facts.system_prompt or history_system_prompt
    # The body is what hermes actually put on the wire; the ``max_tokens``
    # kwarg beside it arrives as None on every supported release.
    resolved_max_tokens = facts.max_tokens
    if resolved_max_tokens is None:
        resolved_max_tokens = as_optional_int(max_tokens)

    # Stamp started_at on both seed and GenState. The seed timestamp is
    # what the SDK uses for the span's start_time; GenState carries it so
    # the close path can compute completed_at = started_at + api_duration.
    started_at = datetime.now(UTC)
    # Assign the id here rather than letting the SDK mint one inside end():
    # the tool executions this call asks for run while it is still unknown
    # otherwise. Same shape as the SDK's own framework handler.
    generation_id = f"gen_{secrets.token_hex(8)}"
    parent_generation_id = _state.turn_last_gen_get(turn_id)
    start = GenerationStart(
        id=generation_id,
        parent_generation_ids=[parent_generation_id] if parent_generation_id else [],
        model=ModelRef(provider=provider or "unknown", name=model or "unknown"),
        conversation_id=session_id or task_id or "",
        agent_name=_agent_name(),
        effective_version=_effective_version(),
        system_prompt=resolved_system_prompt,
        started_at=started_at,
        max_tokens=resolved_max_tokens,
        temperature=facts.temperature,
        top_p=facts.top_p,
        tool_choice=facts.tool_choice,
        tools=facts.tools,
        # The framework tags and the rest of the built-ins ride on the
        # ClientConfig instead, which is the only channel that also reaches
        # spans and metrics. The SDK merges them in under these.
        tags=_tags.seed_tags(),
        metadata={
            "hermes.api_call_count": as_int(api_call_count),
            "hermes.task_id": task_id,
            "hermes.session_id": session_id,
            "hermes.turn_id": turn_id,
            # Counted by hermes rather than read out of the payload, so it
            # stays accurate when the schemas do not: an empty ``tools`` next
            # to a non-zero count reads as a clipped payload rather than a
            # hermes with no tools.
            "hermes.tool_count": tools_expected,
            # True when hermes clipped this request's payload and at least
            # one field above came from an earlier request in the session.
            # A swapped toolset makes such a field stale, so the record
            # says which ones to trust.
            "hermes.request_facts_reused": facts_reused,
        },
    )
    recorder = client.start_generation(_redact.redact_record(start))
    recorder.__enter__()
    # Input is stashed on GenState and threaded into set_result at
    # close-time, so set_result is only called once.
    state = _state.GenState(
        recorder=recorder,
        input_messages=sdk_messages,
        system_prompt=resolved_system_prompt,
        session_id=session_id,
        generation_id=generation_id,
        turn_id=turn_id,
        started_at=started_at,
    )
    if api_request_id:
        # Keyed on the request id, which post_tool_call also carries. A
        # retry overwrites the link, so the tools of a request always point
        # at the attempt that was live when they ran.
        _state.gen_link_put(
            api_request_id,
            _state.GenLink(
                generation_id=generation_id,
                span_context=_span_context_of(recorder),
                session_id=session_id,
                turn_id=turn_id,
            ),
        )
        displaced = _state.req_put(api_request_id, state)
        if displaced is not None:
            # A retry reused the id, so the earlier attempt was abandoned
            # mid-flight. Close it as its own failed generation.
            _finish_generation(
                displaced,
                assistant_message=None,
                usage=None,
                finish_reason="",
                response_model="",
                api_duration=None,
                call_error=_errors.SupersededAttempt(),
            )
    else:
        # LEGACY: infer the pair from the call counter.
        _warn_legacy_hermes_once()
        _state.gen_put((task_id, session_id, as_int(api_call_count)), state)


@_fail_open
def on_api_request_error(
    *,
    api_request_id: str = "",
    error: Any = None,
    status_code: Any = None,
    **_: Any,
) -> None:
    """Close the generation for an API call that failed, carrying the error.

    Only some retry paths fire this hook. The rest re-enter
    ``pre_api_request`` with the same ``api_request_id``, where displacement
    closes the abandoned attempt instead. The two compose: whichever fires
    first pops the state, and the other finds nothing.

    Flushes before returning. On a one-shot run this hook is the last one that
    fires: hermes emits no session-end hook when the turn dies on a provider
    error, and exits through ``os._exit``, which skips the SDK's atexit flush.
    """
    if not api_request_id:
        return
    # Read the payload before popping. ``error`` is whatever hermes built,
    # so if reading it raises, the state is still in the map for
    # displacement or on_session_end to close rather than orphaned here.
    error_type, message, payload_status = _error_fields(error)
    state = _state.req_pop(api_request_id)
    if state is None:
        return
    _finish_generation(
        state,
        assistant_message=None,
        usage=None,
        finish_reason="",
        response_model="",
        api_duration=None,
        call_error=_errors.ProviderCallError(
            _redact.redact_prose(error_type), as_optional_int(status_code) or payload_status
        ),
        call_error_message=_redact.truncate_prose(message, _ERROR_MAX_CHARS),
    )
    cfg = _client._get_plugin_config()
    if cfg is not None:
        _client.flush_bounded(cfg.error_flush_timeout)


@_fail_open
def on_post_api_request(
    *,
    task_id: str = "",
    session_id: str = "",
    api_call_count: int = 0,
    api_request_id: str = "",
    model: str = "",
    usage: Any = None,
    finish_reason: str = "",
    response_model: str = "",
    assistant_message: Any = None,
    api_duration: float | None = None,
    **_: Any,
) -> None:
    """Close the generation for this API call.

    hermes v2026.6.5+ passes ``api_request_id`` and ``assistant_message`` here
    (agent/conversation_loop.py), so the pair is exact and the output is in
    hand: set the result and close immediately. This hook fires at most once
    per id and always after the retry loop, so the state it pops belongs to the
    attempt that was kept; the discarded ones close earlier, in
    ``on_pre_api_request`` or ``on_api_request_error``.

    LEGACY: without ``api_request_id`` the output is not available yet, so only
    the partial fields are stashed and the close is deferred to post_llm_call,
    which is the first hook carrying the assistant content.
    """
    if api_request_id:
        state = _state.req_pop(api_request_id)
        if state is None:
            return
        _finish_generation(
            state,
            assistant_message=assistant_message,
            usage=usage,
            finish_reason=finish_reason,
            response_model=response_model or model or "",
            api_duration=api_duration,
        )
        # Chain the turn's next call onto this one. Recorded here rather than at
        # open time because this hook only sees the attempt hermes kept, so a
        # superseded retry never becomes a parent.
        _state.turn_last_gen_put(state.turn_id, state.generation_id, state.session_id)
        return

    # as_int, not int(): the two hooks have to agree on the key even when a
    # provider reports the counter as text, and this one runs outside any
    # narrower guard.
    state = _state.gen_get((task_id, session_id, as_int(api_call_count)))
    if state is None:
        return
    state.usage = usage
    state.finish_reason = finish_reason or ""
    state.response_model = response_model or model or ""
    if isinstance(api_duration, (int, float)) and api_duration >= 0:
        state.api_duration = float(api_duration)


def _finish_generation(
    state: Any,
    *,
    assistant_message: Any,
    usage: Any,
    finish_reason: str,
    response_model: str,
    api_duration: float | None,
    call_error: Exception | None = None,
    call_error_message: str = "",
) -> None:
    """Set the result on one recorder and close it.

    Pins ``completed_at`` to ``started_at + api_duration`` when hermes gave us a
    duration, so the span and the ``gen_ai.client.operation.duration`` metric
    cover the LLM call rather than the recorder's lifetime. That matters on the
    legacy path, where the close can happen long after the call.

    The two error arguments feed different sinks. ``call_error_message`` is the
    readable text in the exported payload; ``call_error`` stamps the span's
    ``error.type`` / ``error.category`` and the failure metric. Setting the
    message through ``set_result`` first keeps it, because the SDK only derives
    ``call_error`` from the exception when the field is still empty.
    """
    recorder = state.recorder
    try:
        sdk_output = _assistant_to_sdk_messages(assistant_message) if assistant_message is not None else []
        duration = api_duration if api_duration is not None else state.api_duration
        completed_at: datetime | None = None
        if state.started_at is not None and duration is not None:
            completed_at = state.started_at + timedelta(seconds=duration)
        recorder.set_result(
            input=state.input_messages,
            output=sdk_output,
            usage=_build_token_usage(usage),
            stop_reason=_redact.redact_record(finish_reason or ""),
            response_model=_redact.redact_record(response_model or ""),
            started_at=state.started_at,
            completed_at=completed_at,
            call_error=call_error_message,
        )
    except Exception as exc:
        logger.warning("grafana-agento11y-hermes: set_result failed: %s", exc)
    if call_error is not None:
        try:
            recorder.set_call_error(call_error)
        except Exception as exc:
            logger.warning("grafana-agento11y-hermes: set_call_error failed: %s", exc)
    try:
        recorder.__exit__(None, None, None)
    except Exception as exc:
        logger.warning("grafana-agento11y-hermes: recorder __exit__ failed: %s", exc)


def _close_pending_for_session(session_id: str, conversation_history: Any) -> None:
    """LEGACY: drain deferred recorders, assigning outputs from the final convo.

    Only reachable on hermes older than v2026.6.5, which sends no
    ``api_request_id``. Called from ``post_llm_call`` (normal path) and
    ``on_session_end`` (interrupt safety). Walks the new portion of
    ``conversation_history`` to find assistant messages in order and pairs them
    with stored GenStates by api_call_count.
    """
    pending = _state.gen_pop_session(session_id or "")
    if not pending:
        return

    asst_messages: list[dict] = []
    if isinstance(conversation_history, list):
        for msg in conversation_history:
            if isinstance(msg, dict) and msg.get("role") == "assistant":
                asst_messages.append(msg)

    # Slice off prior turns' assistants using the count snapshotted at
    # pre_llm_call time. Falling back to 0 keeps tests that skip pre_llm_call
    # working — they pass single-turn histories where everything is "new".
    start_count = _state.turn_start_asst_count_get(_convo_key("", session_id or "")) or 0
    new_asst = asst_messages[start_count:]

    # End-anchor: pair the LAST n_new pending recorders with the n_new new
    # assistant messages. Hermes increments api_call_count on every iteration,
    # including discarded retries (incomplete <REASONING_SCRATCHPAD>, invalid-
    # response retries), so pending can have more entries than there are kept
    # assistants. Anchoring from the end is correct because post_llm_call only
    # fires when ``final_response`` is set, so the LAST iteration was kept; leading
    # discards leave their recorder with no output rather than stealing a
    # message from a successful call or a prior turn.
    n_new = len(new_asst)
    pair_offset = max(0, len(pending) - n_new)

    for idx, ((_, _, _api_call_count), gen_state) in enumerate(pending):
        new_idx = idx - pair_offset
        asst = new_asst[new_idx] if 0 <= new_idx < n_new else None
        _finish_generation(
            gen_state,
            assistant_message=asst,
            usage=gen_state.usage,
            finish_reason=gen_state.finish_reason,
            response_model=gen_state.response_model,
            api_duration=None,
        )


def _start_tool_execution_under(client: Any, start: Any, link: Any) -> Any:
    """Start the tool execution inside the requesting generation's span context.

    The SDK's ``start_tool_execution`` starts its span from the ambient OTel
    context, so attaching the generation's context around the call is what makes
    the tool span a child of it and puts both in one trace. The Go plugins get
    this for free, because their ``StartGeneration`` returns a context; the
    Python recorder never activates its span, so we do it by hand.

    The generation span has already ended by then, since we close it in
    ``post_api_request`` and the tools it asked for run after. OTel allows a
    child of an ended span and the ids are right, but the child outlives the
    parent's end, which a Go-produced trace does not do.

    Without a usable parent this is exactly ``client.start_tool_execution``, so
    the tool is still recorded as its own root span.
    """
    span_context = getattr(link, "span_context", None)
    if span_context is None or not getattr(span_context, "is_valid", False):
        return client.start_tool_execution(start)

    token = otel_context.attach(otel_trace.set_span_in_context(otel_trace.NonRecordingSpan(span_context)))
    try:
        return client.start_tool_execution(start)
    finally:
        otel_context.detach(token)


def _stamp_parent_generation(recorder: Any, link: Any) -> None:
    """Name the requesting generation on the tool span. Never raises.

    ``NoopToolExecutionRecorder``, returned for an empty tool name, has no span.

    Speculative: ``llms.txt`` lists
    ``agento11y.generation.parent_generation_ids`` under the attributes carried
    by generation *and* tool spans, but no SDK writes it on an ``execute_tool``
    span and no first-party plugin sets it there. It is one attribute, and the
    trace parenting above stands on its own if the UI ignores it.
    """
    generation_id = getattr(link, "generation_id", "")
    if not generation_id:
        return
    try:
        span = getattr(recorder, "span", None)
        if span is None:
            return
        span.set_attribute("agento11y.generation.parent_generation_ids", [generation_id])
    except Exception as exc:
        logger.debug("grafana-agento11y-hermes: parent generation attribute failed: %s", exc)


@_fail_open
def on_post_tool_call(
    *,
    tool_name: str = "",
    args: Any = None,
    result: Any = None,
    task_id: str = "",
    session_id: str = "",
    tool_call_id: str = "",
    api_request_id: str = "",
    duration_ms: int | None = None,
    status: str = "",
    error_type: str = "",
    error_message: str = "",
    **_: Any,
) -> None:
    """Record the tool execution and extend the running convo for the next call.

    All work is done here, not split with pre_tool_call. post_tool_call
    (``_emit_post_tool_call_hook``, ``model_tools.py:974`` in hermes 0.19.0) is
    the only hook of the pair carrying the result, the status and
    ``duration_ms``, so a recorder opened in pre would sit open across the tool
    call for nothing. Opening and closing in post also leaves no key to
    mismatch between the two hooks, which would leak a recorder.

    Order: append the synthesized assistant tool-call message, then the tool
    result, so the next ``pre_api_request``'s input chain reads
    ``user → assistant(tool_calls) → tool``. Then start, set_result, and close
    the tool execution recorder, using ``duration_ms`` from hermes to backdate
    the span's started_at so its duration reflects the tool's wallclock time.

    ``api_request_id`` names the LLM call that asked for this tool, which is
    what puts the tool span in that generation's trace. See
    ``_start_tool_execution_under``.

    ``status`` ``blocked`` and ``cancelled`` stay unrecorded, matching the
    first-party plugins, which only map ``error``.
    """
    convo_key = _convo_key(task_id, session_id)
    # LEGACY: the running convo only feeds pre-v2026.6.5 hermes.
    if tool_call_id and not _SAW_REQUEST_ID:
        try:
            args_str = json.dumps(args) if args is not None else "{}"
        except Exception:
            args_str = "{}"
        _state.convo_append(
            convo_key,
            {
                "role": "assistant",
                "content": None,
                "tool_calls": [
                    {
                        "id": tool_call_id,
                        "type": "function",
                        "function": {"name": tool_name or "", "arguments": args_str},
                    }
                ],
            },
        )
        try:
            content = result if isinstance(result, str) else json.dumps(result, default=str)
        except Exception:
            content = repr(result)
        _state.convo_append(
            convo_key,
            {"role": "tool", "tool_call_id": tool_call_id, "content": content},
        )

    client = _client._get_client()
    if client is None:
        return
    if not _should_sample():
        return

    if str(status).lower() in {"blocked", "cancelled"}:
        return

    from agento11y import ToolExecutionStart

    completed_at = datetime.now(UTC)
    if isinstance(duration_ms, (int, float)) and duration_ms >= 0:
        started_at = completed_at - timedelta(milliseconds=float(duration_ms))
    else:
        started_at = completed_at

    # Leave include_content at its default. The SDK resolves tool-content
    # capture from the mode: forced on under full, forced off under
    # metadata_only / full_with_metadata_spans, and the seed is honored
    # under no_tool_content. Pinning it True kept args/results in the span
    # even when the user set no_tool_content.
    request_model, request_provider = _state.session_model_get(session_id)
    start = ToolExecutionStart(
        tool_name=tool_name or "",
        tool_call_id=tool_call_id or "",
        tool_type="function",
        conversation_id=session_id or task_id or "",
        agent_name=_agent_name(),
        request_model=request_model,
        request_provider=request_provider,
        started_at=started_at,
    )
    link = _state.gen_link_get(api_request_id)
    recorder = _start_tool_execution_under(client, _redact.redact_record(start), link)
    recorder.__enter__()
    cfg = _client._get_plugin_config()
    # cfg is non-None here: _get_client() above only returns a client after
    # _CONFIG was populated by `_client._get_client()`.
    max_chars = cfg.max_chars if cfg is not None else 12000
    try:
        _stamp_parent_generation(recorder, link)
        # Before set_result, matching the first-party plugins. The recorder
        # has no call-error channel, so set_exec_error is the only way a
        # failed tool reaches the span and the failure metric.
        if str(status).lower() == "error":
            recorder.set_exec_error(
                Exception(
                    _redact.truncate_prose(
                        str(error_message or error_type or "tool returned error"),
                        _ERROR_MAX_CHARS,
                    )
                )
            )
        try:
            recorder.set_result(
                arguments=_redact.safe_value(args, max_chars=max_chars, parse_json_strings=True),
                result=_redact.safe_value(result, max_chars=max_chars, parse_json_strings=True),
                completed_at=completed_at,
            )
        except Exception as exc:
            logger.warning("grafana-agento11y-hermes: tool set_result failed: %s", exc)
    finally:
        try:
            recorder.__exit__(None, None, None)
        except Exception as exc:
            logger.warning("grafana-agento11y-hermes: tool recorder __exit__ failed: %s", exc)


@_fail_open
def on_session_end(*, session_id: str = "", **_: Any) -> None:
    # Interrupt safety. A request whose post_api_request never fired (the user
    # interrupted, or the call errored into api_request_error) would leak its
    # recorder. Close it with the input and timing we already have; there is no
    # output to recover.
    if session_id:
        for state in _state.req_pop_session(session_id):
            _finish_generation(
                state,
                assistant_message=None,
                usage=None,
                finish_reason="",
                response_model="",
                api_duration=None,
            )
        # Drop the model, which every pre_api_request rewrites anyway.
        #
        # The request capture is deliberately left alone. This hook fires at
        # the end of every ``run_conversation``, which is once per user message
        # (agent/turn_finalizer.py in hermes 0.19.0), not once per session.
        # Clearing here would empty the cache exactly when turn 2 needs it: its
        # first request already carries the grown history, so its payload is
        # clipped and there would be nothing left to fall back to. The entry is
        # keyed by session and bounded by an LRU in ``_state`` instead.
        _state.session_model_clear(session_id)

    # No tool of this session can fire from here on, so the links it would have
    # read go too. post_llm_call clears the turn chain of a completed turn; this
    # covers the turns that were interrupted.
    if session_id:
        _state.gen_link_pop_session(session_id)
        _state.turn_last_gen_clear_session(session_id)

    # LEGACY: same safety for the deferred path, where post_llm_call only fires
    # on a successful turn (agent/turn_finalizer.py:481 in hermes 0.19.0,
    # ``if final_response and not interrupted``).
    if session_id:
        _close_pending_for_session(session_id, None)

    # flush() leaves the singleton client open so subsequent hermes sessions
    # in the same process keep working. shutdown() would set _closed=True and
    # every future start_generation/start_tool_execution call would raise.
    # Unbounded on purpose: this hook fires while hermes still owns the loop,
    # not on the way out, so the SDK's own timeouts are the right bound.
    # _flush_channels also drains the OTel pipeline, which Client.flush() does
    # not touch.
    _client._flush_channels()


@_fail_open
def on_session_finalize(*, session_id: str = "", **_: Any) -> None:
    """CLI exit. Same work as ``on_session_end``, which does not always fire.

    Interactive hermes fires ``on_session_end`` per completed turn and
    ``on_session_finalize`` once at exit. A turn that died on a provider error
    reaches exit having fired only the latter, so registering both is what
    makes the interactive failure path flush through a hook rather than through
    the SDK's atexit handler. Both firing on a normal exit is harmless: the
    second flush drains an empty queue.
    """
    on_session_end(session_id=session_id)
