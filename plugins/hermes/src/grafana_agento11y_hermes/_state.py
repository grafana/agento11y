"""In-flight recorder state.

Two maps, one per pairing strategy.

``_REQ_STATE`` is the current path. Hermes v2026.6.5 and later, which is
``hermes-agent`` 0.16.0 and later on PyPI, pass ``api_request_id`` to both
``pre_api_request`` and ``post_api_request``. It is one id per API call, so the
pre/post pair needs no inference. The id is not unique per hook invocation:
every retry re-fires ``pre_api_request`` with the same one.

``_GEN_STATE`` and the convo maps below serve the legacy path, for hermes
builds older than v2026.6.5 that send no ``api_request_id``. There the pair is
inferred from ``(task_id, session_id, api_call_count)``, and output content has
to be recovered later from ``post_llm_call``. Delete everything marked LEGACY
when support for pre-v2026.6.5 hermes is dropped.

Generation state carries the parsed input messages alongside the recorder,
because ``set_result(input=[], output=...)`` would clear the input we seeded.

Two more maps outlive the recorders. ``_GEN_LINKS`` keeps each request's
generation id and span context so a tool execution can be recorded inside that
generation's trace, which happens after the generation itself has closed.
``_TURN_LAST_GEN`` keeps the last generation of each turn, which is the parent
of the turn's next one.

Tool executions don't need cross-hook state. We only register
``post_tool_call``, do all the work there, and close immediately.
"""

from __future__ import annotations

import threading
from collections import OrderedDict
from dataclasses import dataclass, field, replace
from datetime import datetime
from typing import Any

from ._request import RequestFacts


@dataclass(slots=True)
class GenState:
    recorder: Any
    input_messages: list = field(default_factory=list)
    system_prompt: str = ""
    # Set on the current path so a session drain can find this request's state.
    session_id: str = ""
    # Pre-assigned at pre_api_request, so tools running after the generation
    # closes can still name it, and so the close can chain the next call of the
    # turn onto it.
    generation_id: str = ""
    turn_id: str = ""
    # LEGACY: partial fields filled in by post_api_request when ``set_result``
    # is deferred to post_llm_call to recover the assistant message. The
    # current path closes in post_api_request and never reads these.
    usage: Any = None
    finish_reason: str = ""
    response_model: str = ""
    # Captured at pre_api_request and post_api_request so the generation span
    # and gen_ai.client.operation.duration metric reflect the LLM call alone,
    # not the close-deferred recorder lifetime that runs through tool execution
    # and any subsequent calls in the same turn.
    started_at: datetime | None = None
    api_duration: float | None = None


@dataclass(slots=True)
class GenLink:
    """What a tool execution needs to point back at the call that requested it.

    Outlives the generation: the recorder closes in ``post_api_request`` and the
    tools it asked for run after that.
    """

    generation_id: str
    span_context: Any = None
    session_id: str = ""
    turn_id: str = ""


# Current path: api_request_id -> state.
_REQ_STATE: dict[str, GenState] = {}
# api_request_id -> GenLink, read by post_tool_call. Entries are dropped per
# session at session end, but a session that never ends (interrupt, one-shot
# exit) would leak, so both this and _TURN_LAST_GEN are capped and drop
# oldest-first. Insertion order is the eviction order, which dict guarantees.
_GEN_LINKS: dict[str, GenLink] = {}
# turn_id -> (generation_id, session_id) of the last generation kept in that
# turn, which the next call of the turn names as its parent.
_TURN_LAST_GEN: dict[str, tuple[str, str]] = {}
_MAX_ENTRIES = 512
# LEGACY: inferred key for hermes builds without api_request_id.
_GEN_STATE: dict[tuple[str, str, int], GenState] = {}
# Per-(task_id, session_id) running hermes-shaped message list. Populated by
# ``pre_llm_call`` from ``conversation_history`` and extended in-place as
# ``post_tool_call`` fires — so each ``pre_api_request`` snapshot reflects
# the messages going into THIS request, not the start-of-turn snapshot.
_CONVO_STATE: dict[tuple[str, str], list[dict]] = {}
# Count of ``role="assistant"`` messages present in ``conversation_history``
# at ``pre_llm_call`` time. Used by ``_close_pending_for_session`` to slice
# off prior turns' assistants and pair only this turn's outputs to recorders.
# A live count from ``_CONVO_STATE`` is wrong — ``post_tool_call`` extends
# the running convo with synthesized assistant tool-call messages, which we
# don't want included.
_TURN_START_ASST_COUNT: dict[tuple[str, str], int] = {}
# Last (model, provider) seen on a ``pre_api_request``, per session.
# ``post_tool_call`` carries neither, so without this the tool duration metric
# reports an empty ``gen_ai.request.model``.
_SESSION_MODEL: dict[str, tuple[str, str]] = {}


# Best read of a ``pre_api_request`` body so far, per session. Hermes clips the
# payload once its system prompt and tool schemas cross
# ``HERMES_PLUGIN_PAYLOAD_MAX_CHARS``, which happens on an ordinary session, so
# only the earliest requests of a session tend to arrive readable and the rest
# are filled in from here.
#
# ``tools/tool_search.py`` can swap the toolset mid-session, so a reused entry
# can name tools the current request did not send. A stale inventory is still
# worth more than an empty one, and an entry never leaves its own session.
#
# Sampling params are cached per model,
# because they are the one part of a capture that another model does not share:
# a mid-session fallback resolves its own cap and temperature from its own
# profile. The system prompt and the toolset belong to the agent rather than to
# the model, so those carry across such a switch.
#
# Nothing clears an entry: ``on_session_end`` fires per turn, not per session,
# so clearing there would empty the cache exactly when the second turn needs
# it. The bound below is what keeps a long-lived process from growing, and
# hermes issues a new session id on ``/reset``, so a stale entry is never read.
@dataclass(slots=True)
class SessionRequestFacts:
    shared: RequestFacts
    models: OrderedDict[str, RequestFacts] = field(default_factory=OrderedDict)


_SESSION_REQUEST_FACTS: OrderedDict[str, SessionRequestFacts] = OrderedDict()
# Sessions kept, least-recently-used evicted first. An entry holds a full
# system prompt plus every tool schema, so this is the largest map here.
_SESSION_FACTS_MAX = 32
_SESSION_FACTS_MODELS_MAX = 8
_LOCK = threading.Lock()


def req_put(request_id: str, state: GenState) -> GenState | None:
    """Store state for a request, returning any state it displaced.

    Hermes assigns ``api_request_id`` above its retry loop, so a second
    ``pre_api_request`` for the same id means the first attempt was abandoned.
    The caller closes what it gets back, otherwise that recorder leaks.
    """
    with _LOCK:
        previous = _REQ_STATE.get(request_id)
        _REQ_STATE[request_id] = state
        return previous


def req_pop(request_id: str) -> GenState | None:
    with _LOCK:
        return _REQ_STATE.pop(request_id, None)


def req_pop_session(session_id: str) -> list[GenState]:
    """Pop every request-keyed state for a session, for interrupt cleanup."""
    with _LOCK:
        matching = [(k, v) for k, v in _REQ_STATE.items() if v.session_id == session_id]
        for k, _ in matching:
            del _REQ_STATE[k]
    return [v for _, v in matching]


def _evict_oldest(mapping: dict) -> None:
    """Drop from the front until the map is back under the cap. Caller holds the lock."""
    while len(mapping) > _MAX_ENTRIES:
        del mapping[next(iter(mapping))]


def gen_link_put(request_id: str, link: GenLink) -> None:
    if not request_id:
        return
    with _LOCK:
        _GEN_LINKS[request_id] = link
        _evict_oldest(_GEN_LINKS)


def gen_link_get(request_id: str) -> GenLink | None:
    if not request_id:
        return None
    with _LOCK:
        return _GEN_LINKS.get(request_id)


def gen_link_pop_session(session_id: str) -> None:
    """Drop every link for a session, once its tools can no longer fire."""
    with _LOCK:
        for key in [k for k, v in _GEN_LINKS.items() if v.session_id == session_id]:
            del _GEN_LINKS[key]


def turn_last_gen_put(turn_id: str, generation_id: str, session_id: str) -> None:
    if not (turn_id and generation_id):
        return
    with _LOCK:
        _TURN_LAST_GEN[turn_id] = (generation_id, session_id)
        _evict_oldest(_TURN_LAST_GEN)


def turn_last_gen_get(turn_id: str) -> str:
    if not turn_id:
        return ""
    with _LOCK:
        entry = _TURN_LAST_GEN.get(turn_id)
    return entry[0] if entry else ""


def turn_last_gen_clear(turn_id: str) -> None:
    with _LOCK:
        _TURN_LAST_GEN.pop(turn_id, None)


def turn_last_gen_clear_session(session_id: str) -> None:
    with _LOCK:
        for key in [k for k, v in _TURN_LAST_GEN.items() if v[1] == session_id]:
            del _TURN_LAST_GEN[key]


def gen_put(key: tuple[str, str, int], state: GenState) -> None:
    with _LOCK:
        _GEN_STATE[key] = state


def gen_get(key: tuple[str, str, int]) -> GenState | None:
    with _LOCK:
        return _GEN_STATE.get(key)


def gen_pop(key: tuple[str, str, int]) -> GenState | None:
    with _LOCK:
        return _GEN_STATE.pop(key, None)


def gen_pop_session(session_id: str) -> list[tuple[tuple[str, str, int], GenState]]:
    """Pop and return all GenStates for a session, sorted by api_call_count."""
    with _LOCK:
        matching = [(k, v) for k, v in _GEN_STATE.items() if k[1] == session_id]
        for k, _ in matching:
            del _GEN_STATE[k]
    matching.sort(key=lambda kv: kv[0][2])
    return matching


def convo_set(key: tuple[str, str], messages: list[dict]) -> None:
    with _LOCK:
        _CONVO_STATE[key] = list(messages)


def convo_get(key: tuple[str, str]) -> list[dict]:
    with _LOCK:
        return list(_CONVO_STATE.get(key) or [])


def convo_append(key: tuple[str, str], message: dict) -> None:
    with _LOCK:
        if key in _CONVO_STATE:
            _CONVO_STATE[key].append(message)


def convo_clear(key: tuple[str, str]) -> None:
    with _LOCK:
        _CONVO_STATE.pop(key, None)


def turn_start_asst_count_set(key: tuple[str, str], count: int) -> None:
    with _LOCK:
        _TURN_START_ASST_COUNT[key] = count


def turn_start_asst_count_get(key: tuple[str, str]) -> int | None:
    with _LOCK:
        return _TURN_START_ASST_COUNT.get(key)


def turn_start_asst_count_clear(key: tuple[str, str]) -> None:
    with _LOCK:
        _TURN_START_ASST_COUNT.pop(key, None)


def session_model_put(session_id: str, model: str, provider: str) -> None:
    if not session_id:
        return
    with _LOCK:
        _SESSION_MODEL[session_id] = (model, provider)


def session_model_get(session_id: str) -> tuple[str, str]:
    with _LOCK:
        return _SESSION_MODEL.get(session_id or "", ("", ""))


def session_model_clear(session_id: str) -> None:
    with _LOCK:
        _SESSION_MODEL.pop(session_id, None)


def session_facts_put(session_id: str, model: str, facts: RequestFacts) -> None:
    """Keep this capture for the session, under the model its params belong to."""
    if not session_id:
        return
    with _LOCK:
        entry = _SESSION_REQUEST_FACTS.get(session_id)
        if entry is None:
            entry = SessionRequestFacts(shared=facts)
            _SESSION_REQUEST_FACTS[session_id] = entry
        entry.shared = facts
        entry.models[model] = replace(facts, system_prompt="", tools=[])
        entry.models.move_to_end(model)
        while len(entry.models) > _SESSION_FACTS_MODELS_MAX:
            entry.models.popitem(last=False)
        _SESSION_REQUEST_FACTS.move_to_end(session_id)
        while len(_SESSION_REQUEST_FACTS) > _SESSION_FACTS_MAX:
            _SESSION_REQUEST_FACTS.popitem(last=False)


def session_facts_get(session_id: str, model: str | None = None) -> tuple[str, RequestFacts] | None:
    """Combine shared prompt/tools with one model's params; default to the latest model."""
    with _LOCK:
        entry = _SESSION_REQUEST_FACTS.get(session_id or "")
        if entry is None:
            return None
        _SESSION_REQUEST_FACTS.move_to_end(session_id)
        if model is None:
            model = next(reversed(entry.models))
        cached = entry.models.get(model)
        if cached is not None:
            entry.models.move_to_end(model)
        return model, replace(
            cached if cached is not None else RequestFacts(),
            system_prompt=entry.shared.system_prompt,
            system_prompt_clipped=entry.shared.system_prompt_clipped,
            tools=list(entry.shared.tools),
            tools_clipped=entry.shared.tools_clipped,
        )


def reset_for_tests() -> None:
    with _LOCK:
        _REQ_STATE.clear()
        _GEN_LINKS.clear()
        _TURN_LAST_GEN.clear()
        _GEN_STATE.clear()
        _CONVO_STATE.clear()
        _TURN_START_ASST_COUNT.clear()
        _SESSION_MODEL.clear()
        _SESSION_REQUEST_FACTS.clear()
