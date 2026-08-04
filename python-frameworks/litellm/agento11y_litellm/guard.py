"""LiteLLM guardrail that enforces Agent Observability hook rules.

Call hooks are a proxy-only LiteLLM feature: ``ProxyLogging.pre_call_hook`` and
``ProxyLogging.post_call_success_hook`` invoke them, and nothing on the
``litellm.completion()`` SDK path does. A direct SDK call is therefore never
guarded by this class.
"""

from __future__ import annotations

import asyncio
import logging
from collections.abc import Sequence
from concurrent.futures import ThreadPoolExecutor
from contextvars import copy_context
from dataclasses import replace
from datetime import datetime
from functools import lru_cache, partial
from typing import TYPE_CHECKING, Any

import litellm
from agento11y import Client
from agento11y.errors import HookDeniedError, HookTransportError
from agento11y.hooks import (
    HookAction,
    HookContext,
    HookEvaluateRequest,
    HookEvaluateResponse,
    HookInput,
    HookModel,
    HookPhase,
    hook_denied_from_response,
)
from agento11y.models import Message, MessageRole, PartKind
from litellm.exceptions import GuardrailRaisedException
from litellm.integrations.custom_guardrail import CustomGuardrail, log_guardrail_information
from litellm.types.guardrails import GuardrailEventHooks

from .handler import (
    _ANTHROPIC_MESSAGES_CALL_TYPES,
    _RESPONSES_CALL_TYPES,
    DEFAULT_AGENT_NAME_METADATA_KEYS,
    FRAMEWORK_TAGS,
    _extract_text_content,
    _first_metadata_value,
    _map_messages,
    _map_tools_list,
    _metadata_sources_from,
    _request_tags_from,
    _resolve_conversation_id_from,
    _select_output_mappers,
)

if TYPE_CHECKING:
    from litellm.caching import DualCache
    from litellm.proxy._types import UserAPIKeyAuth
else:
    DualCache = Any
    UserAPIKeyAuth = Any

logger = logging.getLogger(__name__)

DEFAULT_GUARDRAIL_NAME = "agento11y"

# Sent when the model identity cannot be resolved from the request. The hooks
# API rejects an empty provider or name, and a rejected evaluation fails open,
# so a placeholder is the difference between rules running and rules never
# running. Matches the placeholder the agento11y CLI tool-call guard sends.
UNKNOWN_PROVIDER = "unknown"
UNKNOWN_MODEL = "unknown"

# ``during_call`` (``async_moderation_hook``) is not supported. It runs in
# parallel with the provider call, so it cannot save spend, and it is never
# handed the response, so all it can do is repeat the preflight check after the
# call has already started. A ``during_call`` deny does suppress the output:
# LiteLLM awaits the moderation gather before it reads the provider response.
SUPPORTED_EVENT_HOOKS = (GuardrailEventHooks.pre_call, GuardrailEventHooks.post_call)

# The hook phase each supported mode evaluates.
PHASE_FOR_EVENT_HOOK = {
    GuardrailEventHooks.pre_call.value: HookPhase.PREFLIGHT.value,
    GuardrailEventHooks.post_call.value: HookPhase.POSTFLIGHT.value,
}

# Call types this adapter can read request input from.
#
# ``ProxyLogging.pre_call_hook`` runs every registered CustomGuardrail on every
# route it covers, including embeddings, moderation, rerank, audio, realtime,
# MCP tool calls, and native pass-through. Those bodies carry no messages this
# adapter maps, and evaluating one would send empty input, get an allow back,
# and record a verdict that reads like a completed check. They are skipped
# instead, so a request that was never checked is not reported as allowed.
#
# Kept separate from the logger's ``_GENERATION_CALL_TYPES`` on purpose: what
# the logger records and what this guard can inspect are different questions,
# and a change to one should not silently move the other. Adding a route here
# means teaching ``_hook_input`` where that route keeps its input.
GUARDED_CALL_TYPES = frozenset(
    {
        "completion",
        "acompletion",
        "text_completion",
        "atext_completion",
        "responses",
        "aresponses",
        "anthropic_messages",
        "aanthropic_messages",
        "image_generation",
        "aimage_generation",
    }
)

# Body keys that carry conversation input, in precedence order. Chat and
# ``/v1/messages`` use ``messages``, ``/v1/responses`` uses ``input``, text
# completion and image generation use ``prompt``.
_INPUT_KEYS = ("messages", "input", "prompt")

# The subset of ``_INPUT_KEYS`` that carries a list of messages. ``prompt`` is
# left out: a text completion prompt is a string or a list of strings, and
# neither takes a message.
_MESSAGE_LIST_KEYS = ("messages", "input")

# Body keys that carry the system prompt outside the message list:
# ``/v1/messages`` sends ``system``, ``/v1/responses`` sends ``instructions``.
_SYSTEM_PROMPT_KEYS = ("system", "instructions")

# Where a route takes a system prompt the request body did not already carry.
# ``/v1/messages`` accepts only ``user`` and ``assistant`` in ``messages`` and
# rejects a ``system`` role outright; ``/v1/responses`` keeps its items in
# ``input`` and its prompt in ``instructions``. Chat routes are absent on
# purpose: a ``system`` message is their normal form.
#
# ``_apply_system_prompt`` reads this dict by call type, unlike everything else
# here, because the target cannot be inferred from a body that has no system
# prompt yet. LiteLLM's Anthropic adapter route reports call type
# ``text_completion``, so this dict has no entry for it, and an Anthropic body
# arriving that way gets a ``system`` message rather than a top-level ``system``
# field. That route translates the body to chat format before it reaches the
# provider, so the message survives.
_ROUTE_SYSTEM_PROMPT_KEY = {
    **{call_type: "system" for call_type in _ANTHROPIC_MESSAGES_CALL_TYPES},
    **{call_type: "instructions" for call_type in _RESPONSES_CALL_TYPES},
}

# Roles whose content ``_map_messages`` folds into the system prompt instead of
# the message list, so a transformed message list never contains them.
_SYSTEM_ROLES = frozenset({"system", "developer"})

# Content block types that carry plain text, across the chat and Responses
# vocabularies.
#
# Deliberately narrower than what ``content_parts`` reads as text, which also
# covers ``refusal``: a refusal block is the provider's own wording, not
# something a request rewrites. See ``_is_text_block``.
_TEXT_BLOCK_TYPES = frozenset({"text", "input_text", "output_text"})

# Fields a text content block can carry and still survive a collapse into one
# string. The system prompt rewrite collapses more than one block that way.
# Anything else the block held (Anthropic ``cache_control``, ``citations``) would
# be dropped, so that rewrite is skipped instead. The message rewrite has no such
# limit: it writes text into the block the caller sent and leaves its other
# fields alone.
_TRANSFORMABLE_BLOCK_KEYS = frozenset({"type", "text"})

_GUARD_FRAMEWORK_TAGS = {**FRAMEWORK_TAGS, "agento11y.framework.source": "guardrail"}


class Agento11yLiteLLMGuardrail(CustomGuardrail):
    """Evaluates agento11y hook rules around a proxy request.

    Deliberately not a subclass of ``Agento11yLiteLLMLogger``. Both objects sit
    in ``litellm.callbacks`` at once, and a bare ``CustomGuardrail`` is harmless
    there only because it inherits ``CustomLogger``'s no-op logging methods. Give
    this class the logger's export behavior and every generation exports twice.

    ``ProxyLogging.pre_call_hook`` and ``ProxyLogging.post_call_success_hook``
    walk ``litellm.callbacks`` and run every ``CustomGuardrail`` they find, so a
    dotted path next to the logger is enough::

        litellm_settings:
          callbacks:
            - agento11y_callback.agento11y_handler
            - agento11y_callback.agento11y_guards

    ``event_hook`` selects the phases: ``pre_call`` (the default) evaluates
    preflight rules before the provider is called, ``post_call`` evaluates
    postflight rules against the response, and a list runs both.
    """

    def __init__(
        self,
        *,
        client: Client,
        agent_name: str = "",
        agent_name_metadata_keys: Sequence[str] = DEFAULT_AGENT_NAME_METADATA_KEYS,
        agent_version: str = "",
        max_concurrent_evaluations: int = 32,
        request_timeout_seconds: float = 2.0,
        apply_transforms: bool = True,
        extra_tags: dict[str, str] | None = None,
        **kwargs: Any,
    ) -> None:
        if max_concurrent_evaluations <= 0:
            raise ValueError(f"max_concurrent_evaluations must be greater than zero, got {max_concurrent_evaluations}")
        if request_timeout_seconds <= 0:
            raise ValueError(f"request_timeout_seconds must be greater than zero, got {request_timeout_seconds}")

        event_hook = _normalized_event_hook(kwargs.pop("event_hook", None))

        super().__init__(
            guardrail_name=kwargs.pop("guardrail_name", None) or DEFAULT_GUARDRAIL_NAME,
            supported_event_hooks=list(SUPPORTED_EVENT_HOOKS),
            # Kept as configured so LiteLLM's tag-conditional Mode form keeps
            # working. An unset mode means preflight only: LiteLLM reads None as
            # "every phase", which would start sending postflight evaluations
            # for every existing deployment on upgrade.
            event_hook=GuardrailEventHooks.pre_call if event_hook is None else event_hook,
            **kwargs,
        )
        self._client = client
        # Snapshotted once: ``Client.hooks_config`` deep-copies on every read,
        # and the client resolves its configuration at construction, so a
        # per-request read would only pay for the copy.
        hooks = client.hooks_config
        # ``resolve_config`` normalizes an empty phase list to ``["preflight"]``,
        # and the SDK defends against an empty one anyway; mirroring that keeps
        # an empty list from reading as "no phase configured".
        phases = hooks.phases or [HookPhase.PREFLIGHT.value]
        self._configured_phases = frozenset(phases) if hooks.enabled else frozenset()
        # The phases the configured modes evaluate. An unset mode is preflight,
        # the same default ``super().__init__`` is given above.
        self._guarded_phases = frozenset(
            PHASE_FOR_EVENT_HOOK[mode]
            for mode in _event_hook_values(event_hook) or [GuardrailEventHooks.pre_call.value]
        )
        self._fail_open = hooks.fail_open
        # Evaluations are submitted fail-closed so an SDK-side transport failure
        # reaches this adapter instead of resolving to a synthetic allow.
        self._fail_closed_hooks = replace(hooks, fail_open=False)
        self._warn_about_unconfigured_phases()
        self._agent_name = agent_name
        self._agent_name_metadata_keys = tuple(agent_name_metadata_keys)
        self._agent_version = agent_version
        self._max_concurrent_evaluations = max_concurrent_evaluations
        self._request_timeout_seconds = request_timeout_seconds
        self._apply_transforms = apply_transforms
        self._extra_tags = dict(extra_tags) if extra_tags else {}
        self._executor = ThreadPoolExecutor(max_workers=self._max_concurrent_evaluations)

    async def async_pre_call_hook(
        self,
        user_api_key_dict: UserAPIKeyAuth,
        cache: DualCache,
        data: dict,
        call_type: str,
    ) -> dict | None:
        """Blocks the request when an agento11y preflight rule denies it.

        Returns a new request body when an allow verdict carried a transform the
        adapter could apply. Returns ``None`` otherwise, and the proxy then
        forwards the caller's body untouched.

        Every gate that ends in "do not evaluate" sits outside the decorated
        body, so a request this guardrail did not check records no verdict at
        all: the SDK is not configured for the phase, the guardrail is not
        enabled for the request, the route carries input this adapter cannot
        map, or the mapped input is empty. The proxy applies the enablement gate
        before calling in, so that one only matters for direct invocation.

        A failed evaluation does record a verdict: the exception crosses
        ``_run_preflight``'s decorator, which files
        ``guardrail_failed_to_respond``, and this method then applies
        ``HooksConfig.fail_open``.
        """
        # Checked first: the result cannot change per request.
        if not self._phase_configured(HookPhase.PREFLIGHT):
            logger.debug("agento11y: skipping preflight evaluation, the SDK is not configured for the phase")
            return None

        if not self.should_run_guardrail(data, GuardrailEventHooks.pre_call):
            return None

        if call_type not in GUARDED_CALL_TYPES:
            logger.debug("agento11y: skipping preflight evaluation, call type %r carries no mappable input", call_type)
            return None

        hook_input = _hook_input(data)
        if not hook_input.messages and not hook_input.system_prompt:
            # A body whose input this adapter reads but cannot turn into text:
            # token-id prompts, or content that is entirely images or audio.
            logger.debug("agento11y: skipping preflight evaluation, request carries no evaluable text")
            return None

        try:
            return await self._run_preflight(
                user_api_key_dict=user_api_key_dict, data=data, hook_input=hook_input, call_type=call_type
            )
        except HookTransportError as exc:
            if not self._fail_open:
                raise
            logger.warning(
                "agento11y: guardrail %r allowing request (fail_open): %s",
                self.guardrail_name,
                exc,
            )
            return None

    async def async_post_call_success_hook(
        self,
        data: dict,
        user_api_key_dict: UserAPIKeyAuth,
        response: Any,
    ) -> None:
        """Evaluates agento11y postflight rules against the provider response.

        Returns ``None`` so LiteLLM keeps the response it already has; a deny is
        signalled by raising, not by returning a replacement.

        The provider has already been called and billed by the time this runs.
        Postflight decides whether the output reaches the caller, not whether the
        request costs money.

        What a deny can do depends on how the response is delivered:

        - Non-streaming: ``ProxyLogging.post_call_success_hook`` is awaited in
          ``base_process_llm_request`` before the route serializes anything, so
          raising returns HTTP 400 and the provider output never leaves the
          proxy.
        - Streaming: LiteLLM has already flushed every chunk and calls this hook
          on the assembled response from ``_run_deferred_stream_guardrails``,
          which catches whatever it raises. A deny is recorded and logged in
          ``_detect_postflight`` instead of raised, because raising buys nothing
          and costs a traceback in the proxy log. Only a ``CustomStreamWrapper``
          gets that deferred pass, so a streamed ``/v1/responses`` or
          ``/v1/messages`` response never reaches this hook at all and produces
          no verdict.

        This class deliberately implements neither
        ``async_post_call_streaming_iterator_hook`` nor ``apply_guardrail``.
        Defining either makes ``_run_deferred_stream_guardrails`` take a
        different branch and skip this hook on the streaming path, and
        ``apply_guardrail`` also reroutes the non-streaming path through
        ``UnifiedLLMGuardrails``, which bypasses this method as well.

        Every gate that ends in "do not evaluate" sits above the two evaluating
        methods, so a request this guardrail did not check records no verdict.

        A failed evaluation does record a verdict: it is filed as
        ``guardrail_failed_to_respond``, by the decorator on the enforcing path
        and by hand on the streamed one, and this method then applies
        ``HooksConfig.fail_open``.
        """
        # Checked first: the result cannot change per request.
        if not self._phase_configured(HookPhase.POSTFLIGHT):
            logger.debug("agento11y: skipping postflight evaluation, the SDK is not configured for the phase")
            return None

        if not self.should_run_guardrail(data, GuardrailEventHooks.post_call):
            return None

        hook_input = _hook_input(data)
        if not hook_input.messages and not hook_input.system_prompt:
            # Stands in for the call-type gate preflight applies: this hook is
            # not given a call type, and a body this adapter cannot read input
            # from is the same signal. It is also what a native pass-through
            # request looks like, whose provider-native body sits under
            # ``data["data"]``, so those routes stay unguarded on both phases.
            logger.debug("agento11y: skipping postflight evaluation, request carries no evaluable text")
            return None

        output = _map_output_messages(response)
        if not output:
            # An embedding, an image, or a turn whose content this adapter
            # cannot turn into text. Evaluating it would send empty output, get
            # an allow back, and record a verdict that reads like a check.
            logger.debug("agento11y: skipping postflight evaluation, response carries no evaluable text")
            return None

        hook_input.output = output

        # LiteLLM decides to stream on ``data["stream"] is True``, so a body
        # carrying ``1`` or ``"true"`` is served non-streamed and has to take the
        # enforcing branch. The value is whatever the client sent; nothing
        # coerces it before the hook runs.
        run = self._detect_postflight if data.get("stream") is True else self._enforce_postflight

        try:
            return await run(user_api_key_dict=user_api_key_dict, data=data, hook_input=hook_input)
        except HookTransportError as exc:
            if not self._fail_open:
                raise
            logger.warning(
                "agento11y: guardrail %r allowing the response (fail_open): %s",
                self.guardrail_name,
                exc,
            )
            return None

    @log_guardrail_information
    async def _run_preflight(
        self, *, user_api_key_dict: UserAPIKeyAuth, data: dict, hook_input: HookInput, call_type: str
    ) -> dict | None:
        """Evaluate preflight rules and translate a deny into a proxy 400.

        On allow, the verdict's transform is written back into the request. The
        deny check runs before this method looks at that transform, so a denied
        request never reaches the provider, rewritten or not.
        """
        request = HookEvaluateRequest(
            phase=HookPhase.PREFLIGHT.value,
            context=self._context(data, user_api_key_dict),
            input=hook_input,
        )

        response = await self._evaluate(request)
        denied = hook_denied_from_response(response)
        if denied is not None:
            raise self._denied_exception(denied)

        if not self._apply_transforms:
            return None
        try:
            return _apply_transform(data, response.transformed_input, call_type)
        except Exception as exc:  # noqa: BLE001
            # A body shape the transform code did not expect must not fail the
            # request. Raising here reaches the proxy as a 500. That is the
            # opposite of ``fail_open``, and of this module's rule that a request
            # goes out untouched rather than half-rewritten.
            logger.warning("agento11y: skipping transform, applying it failed: %s: %s", type(exc).__name__, exc)
            return None

    @log_guardrail_information
    async def _enforce_postflight(
        self,
        *,
        user_api_key_dict: UserAPIKeyAuth,
        data: dict,
        hook_input: HookInput,
    ) -> None:
        """Evaluate postflight rules and translate a deny into a proxy 400.

        Runs for a response the proxy has not sent yet. ``transformed_input``
        is ignored: the preflight rewrite applies to the request, and LiteLLM
        takes no replacement response from this hook, so a postflight transform
        has nowhere to go.

        The decorator infers the recorded guardrail mode from the wrapped
        function's name and falls back to ``self.event_hook`` for a name it does
        not know (``custom_guardrail.py``). A guardrail registered for both modes
        therefore records both modes on each entry; the phase is unambiguous in
        the hook request itself.
        """
        response = await self._evaluate(self._postflight_request(data, user_api_key_dict, hook_input))
        denied = hook_denied_from_response(response)
        if denied is None:
            return None

        raise self._denied_exception(denied)

    async def _detect_postflight(
        self,
        *,
        user_api_key_dict: UserAPIKeyAuth,
        data: dict,
        hook_input: HookInput,
    ) -> None:
        """Evaluate postflight rules for a response the caller already has.

        A deny cannot be enforced on a streamed response, so it is recorded as an
        intervention that says so and nothing is raised.

        Deliberately not wrapped in ``log_guardrail_information``: a deny has to
        be filed by hand, and the decorator files a normal return as ``success``
        on top of it. Its flag for a guardrail that records its own verdict
        (``_guardrail_self_recorded``) only exists from LiteLLM 1.95.0, and this
        package supports 1.82.3, so relying on it would double-file every
        streamed verdict on an older proxy. Every outcome is therefore recorded
        here, including the two the decorator would have handled.
        """
        request = self._postflight_request(data, user_api_key_dict, hook_input)

        started = datetime.now()
        try:
            response = await self._evaluate(request)
        except HookTransportError as exc:
            self._record_postflight_verdict(data, started, "guardrail_failed_to_respond", str(exc))
            raise

        denied = hook_denied_from_response(response)
        if denied is None:
            self._record_postflight_verdict(data, started, "success", {"action": HookAction.ALLOW.value})
            return None

        logger.warning(
            "agento11y: postflight rule denied a streamed response that has already been delivered "
            "to the caller, recording the verdict only: %s",
            _denial_detail(denied),
        )
        self._record_postflight_verdict(
            data,
            started,
            "guardrail_intervened",
            {
                "action": HookAction.DENY.value,
                "rule_id": denied.rule_id,
                "reason": denied.reason,
                # The only field that tells a recorded deny from an enforced one.
                "enforced": False,
            },
        )
        return None

    def _postflight_request(
        self,
        data: dict,
        user_api_key_dict: UserAPIKeyAuth,
        hook_input: HookInput,
    ) -> HookEvaluateRequest:
        return HookEvaluateRequest(
            phase=HookPhase.POSTFLIGHT.value,
            context=self._context(data, user_api_key_dict),
            input=hook_input,
        )

    def _record_postflight_verdict(self, data: dict, started: datetime, status: str, payload: Any) -> None:
        """File one postflight verdict the way the decorator would.

        The timings are passed for the reason the decorator passes them: without
        them the OTel exporter stamps the guardrail span at export time and
        reports a near-zero duration.
        """
        ended = datetime.now()
        self.add_standard_logging_guardrail_information_to_request_data(
            guardrail_json_response=payload,
            request_data=data,
            guardrail_status=status,
            start_time=started.timestamp(),
            end_time=ended.timestamp(),
            duration=(ended - started).total_seconds(),
            event_type=GuardrailEventHooks.post_call,
        )

    def _phase_configured(self, phase: HookPhase) -> bool:
        """Whether the SDK will evaluate this phase against the server.

        Mirrors the gate in ``agento11y.hooks.evaluate_hook``, which returns
        allow without contacting the server when hooks are disabled or the phase
        is not listed. Reading it here keeps that case out of the decorated
        body: the decorator would otherwise record a passed check, so an
        operator who set ``event_hook="post_call"`` but left ``HooksConfig``
        alone would see "N evaluated, N passed" for rules that never ran.
        """
        return phase.value in self._configured_phases

    def _warn_about_unconfigured_phases(self) -> None:
        """Warn at startup about a phase this guardrail runs but the SDK will not evaluate.

        A guardrail whose mode has no matching ``HooksConfig.phases`` entry is
        silent otherwise: it skips every request and records no verdict.
        """
        unconfigured = sorted(self._guarded_phases - self._configured_phases)
        if not unconfigured:
            return

        if self._guarded_phases & self._configured_phases:
            logger.warning(
                "agento11y: guardrail %r runs on %s but the client's hooks config does not enable it; "
                "that phase will not be evaluated",
                self.guardrail_name,
                ", ".join(unconfigured),
            )
            return

        logger.warning(
            "agento11y: guardrail %r is registered but the client's hooks config does not enable %s; "
            "no request will be evaluated",
            self.guardrail_name,
            ", ".join(unconfigured),
        )

    def _denied_exception(self, denied: HookDeniedError) -> GuardrailRaisedException:
        return GuardrailRaisedException(
            guardrail_name=self.guardrail_name,
            message=f"blocked by guardrail {self.guardrail_name}: {_denial_detail(denied)}",
            should_wrap_with_default_message=False,
        )

    def _context(self, data: dict, user_api_key_dict: Any) -> HookContext:
        """Build the hook context from the raw proxy request body.

        Agent identity follows the same metadata precedence as the logger, so a
        rule matching on ``agent_name`` selects the same traffic in both. Trace
        correlation uses the proxy request span. The ambient span during a
        pre-call hook is often an unrelated phase span.

        The model provider and name are resolved from the request rather than
        left empty: the hooks API requires both, and an evaluation it rejects
        fails open, so every rule would be skipped.
        """
        sources = _metadata_sources_from(data)
        tags: dict[str, str] = dict(_GUARD_FRAMEWORK_TAGS)
        for tag_value in _request_tags_from(sources):
            tags[f"litellm.tag.{tag_value}"] = tag_value
        tags.update(self._extra_tags)

        trace_id, span_id = _span_ids(getattr(user_api_key_dict, "parent_otel_span", None))

        model_name = str(data.get("model") or "")
        # The hooks API requires a name as well as a provider, so a route that
        # carries no model still has to send something.
        provider = _resolve_provider(data, model_name)
        model_name = model_name.strip() or UNKNOWN_MODEL

        return HookContext(
            model=HookModel(provider=provider, name=model_name),
            agent_name=_first_metadata_value(sources, self._agent_name_metadata_keys) or self._agent_name,
            agent_version=_first_metadata_value(sources, ("agent_version",)) or self._agent_version,
            tags=tags,
            conversation_id=_resolve_conversation_id_from(data, sources),
            trace_id=trace_id,
            span_id=span_id,
        )

    async def _evaluate(self, request: HookEvaluateRequest) -> HookEvaluateResponse:
        """Evaluate hook rules without blocking the proxy event loop.

        ``Client.evaluate_hook`` blocks on urllib, so a dedicated worker pool
        limits concurrent evaluations without consuming asyncio's default
        executor. Context variables are copied so SDK conversation and trace
        fallbacks remain available in the worker. The timeout covers time queued
        for a worker and time running the evaluation. A timed-out evaluation
        that has started keeps its pool slot until urllib finishes.

        The call goes out fail-closed regardless of the configured policy, so a
        transport failure raises here instead of resolving to an allow inside
        the SDK: an allow the SDK synthesized is indistinguishable from a server
        allow. Never return a synthetic allow from here, because LiteLLM files
        it as a completed check. Every failure crosses the recording decorator
        instead, and LiteLLM files it as ``guardrail_failed_to_respond`` with
        the real duration. The calling hook applies ``HooksConfig.fail_open``
        afterwards, so the allow-or-raise outcome still follows the configured
        policy.
        """
        try:
            loop = asyncio.get_running_loop()
            context = copy_context()
            evaluation = loop.run_in_executor(
                self._executor,
                context.run,
                # Bound with partial because run_in_executor takes no keyword
                # arguments.
                partial(self._client.evaluate_hook, hooks=self._fail_closed_hooks),
                request,
            )
            return await asyncio.wait_for(evaluation, self._request_timeout_seconds)
        except HookTransportError:
            raise
        except asyncio.TimeoutError as exc:
            raise HookTransportError(
                f"agento11y {request.phase} hook evaluation failed: timed out after {self._request_timeout_seconds}s"
            ) from exc
        except Exception as exc:  # noqa: BLE001
            raise HookTransportError(
                f"agento11y {request.phase} hook evaluation failed: {type(exc).__name__}: {exc}"
            ) from exc


def _denial_detail(denied: HookDeniedError) -> str:
    """Render a deny verdict as one line of reason plus rule id."""
    return f"{denied.reason} (rule {denied.rule_id})" if denied.rule_id else denied.reason


def _map_output_messages(response: Any) -> list[Message]:
    """Map a provider response into agento11y output messages.

    Each route hands the post-call hook a different object: a ``ModelResponse``
    for chat completions and for an assembled stream, a ``ResponsesAPIResponse``
    for ``/v1/responses``, and a plain Anthropic-shaped dict for ``/v1/messages``
    (LiteLLM normalizes that one to chat shape for logging, but not here).

    The shape is read off the payload rather than off a call type, because this
    hook is not given one. The logger shares the rule through
    ``_select_output_mappers``, so a guard verdict and the exported generation
    read one response the same way.
    """
    payload = _response_payload(response)
    if payload is None:
        return []
    map_output, _ = _select_output_mappers("", payload)
    return map_output(payload)


def _response_payload(response: Any) -> Any:
    """Reduce a LiteLLM response object to the dict or string the mappers read."""
    if response is None or isinstance(response, (str, dict)):
        return response
    dump = getattr(response, "model_dump", None)
    if dump is None:
        return None
    try:
        return dump()
    except Exception:  # noqa: BLE001
        logger.debug("agento11y: could not read %s as a response payload", type(response).__name__)
        return None


def _apply_transform(data: dict, transformed: HookInput | None, call_type: str) -> dict | None:
    """Write an allow verdict's ``transformed_input`` back into the request body.

    Returns a new body for the proxy to forward, or ``None`` when nothing was
    applied. ``ProxyLogging.process_pre_call_hook_response`` uses a returned dict
    as the request body and hands it to the remaining callbacks, so this is what
    reaches the provider; ``None`` forwards the caller's body untouched.

    The caller's dict is never mutated. A guardrail that edited it in place would
    also edit the body LiteLLM logs and the one other callbacks already hold.

    The system prompt and the message list are applied independently. A system
    prompt is one string with one place to go; a message list is matched to the
    request by position and can fail that match. Tying them together would mean a
    system-prompt rule never takes effect on a conversation whose messages did
    not line up.
    """
    if transformed is None or (not transformed.messages and not transformed.system_prompt):
        return None

    out = dict(data)
    # Messages first, so a skip warning indexes the list the caller sent. The
    # system prompt rewrite can drop a system message, which shifts every index
    # after it.
    changed = _apply_messages(out, transformed.messages)
    changed = _apply_system_prompt(out, transformed.system_prompt, call_type) or changed
    return out if changed else None


def _apply_system_prompt(data: dict, system_prompt: str, call_type: str) -> bool:
    """Write a transformed system prompt back into the field that carried it.

    ``_hook_input`` sends the system prompt as its own wire field, and
    ``_parse_wire_message_dict`` maps any role but ``assistant`` and ``tool`` to
    ``user``, so a transformed prompt can only arrive in ``system_prompt``.

    The value replaces the whole system prompt, because that is what was
    evaluated: ``_hook_input`` joins the top-level field and every system or
    developer message in the input list into one string. The body can hold system
    content in more than one carrier at once, meaning the top-level field and any
    ``system`` or ``developer`` message. The rewrite writes the transformed value
    into one carrier and removes the rest, so a body that kept its prompt in two
    places is not left half-rewritten.

    The rewrite is skipped, with a warning, when:

    - The body has nowhere to put a prompt.
    - Removing the other carriers would leave the input list empty.
    - The target holds content blocks a rewrite would strip fields from.

    An empty transformed value means no change. ``_parse_hook_input_wire`` drops
    an empty ``system_prompt``, so "clear the system prompt" cannot be expressed
    on the wire and is indistinguishable from "no transform".
    """
    if not system_prompt:
        return False

    input_key = next((key for key in _MESSAGE_LIST_KEYS if isinstance(data.get(key), list) and data[key]), "")
    items = data[input_key] if input_key else []
    system_indices = _system_message_indices(items)
    top_level_carriers = [key for key in _SYSTEM_PROMPT_KEYS if data.get(key)]
    top_level_key = top_level_carriers[0] if top_level_carriers else _ROUTE_SYSTEM_PROMPT_KEY.get(call_type, "")

    if not top_level_key and not input_key:
        # Text completion and image generation have nowhere to put a system
        # prompt: the body is a bare ``prompt``.
        logger.warning(
            "agento11y: skipping system prompt transform, request body has no system prompt field or message list"
        )
        return False

    if top_level_key:
        if system_indices and len(system_indices) == len(items):
            logger.warning(
                "agento11y: skipping system prompt transform, dropping the system messages would leave %r empty",
                input_key,
            )
            return False
        value, reason = _rewritten_system_value(data.get(top_level_key), system_prompt)
        if reason:
            logger.warning(
                "agento11y: skipping system prompt transform, %r carries %s",
                top_level_key,
                reason,
            )
            return False
        data[top_level_key] = value
        for key in top_level_carriers[1:]:
            data.pop(key, None)
        if system_indices:
            dropped = set(system_indices)
            data[input_key] = [item for index, item in enumerate(items) if index not in dropped]
        return True

    if system_indices:
        first, rest = system_indices[0], set(system_indices[1:])
        content, reason = _rewritten_system_value(items[first].get("content"), system_prompt)
        if reason:
            logger.warning(
                "agento11y: skipping system prompt transform, message %d carries %s",
                first,
                reason,
            )
            return False
        data[input_key] = [
            {**item, "content": content} if index == first else item
            for index, item in enumerate(items)
            if index not in rest
        ]
        return True

    data[input_key] = [{"role": "system", "content": system_prompt}, *items]
    return True


def _rewritten_system_value(value: Any, system_prompt: str) -> tuple[Any | None, str]:
    """Return ``value`` carrying ``system_prompt``, or why it cannot.

    A string field takes the text directly. A lone text block keeps its own
    fields and takes the new text, so an Anthropic ``cache_control`` breakpoint
    survives a redaction. More than one block collapses into one string. That is
    safe only when no block carries fields of its own: the evaluated prompt was
    their joined text, and there is no way back to per-block values.
    """
    if not isinstance(value, list):
        return system_prompt, ""
    if len(value) == 1 and isinstance(value[0], dict) and str(value[0].get("type") or "").lower() in _TEXT_BLOCK_TYPES:
        return [{**value[0], "text": system_prompt}], ""
    reason = _block_collapse_reason(value)
    return (None, reason) if reason else (system_prompt, "")


def _apply_messages(data: dict, messages: Sequence[Message]) -> bool:
    """Write transformed text back into the messages the caller sent.

    Each transformed message is matched to the request message at the same
    position, and only the text is replaced: a string ``content`` takes the new
    text, a text content block takes it in place, and a tool result takes the
    rewritten result. Everything else on the message is left as it arrived, so
    tool calls, reasoning, images, and an Anthropic ``cache_control`` breakpoint
    survive a redaction instead of blocking it. ``plugins/pi/src/mappers.ts``
    writes a transform back the same way.

    ``system`` and ``developer`` messages take no part in the matching and stay
    where they are: ``_hook_input`` sends the system prompt as its own field, so
    the transformed list never contains them.

    This function writes nothing unless it can write every transformed message,
    and it logs a warning saying why: a half-rewritten conversation is worse than
    an untouched one. Cases that skip:

    - The body keeps its input somewhere other than ``messages``.
      ``/v1/responses`` uses ``input`` and text completion uses ``prompt``, and
      neither key takes chat messages.
    - The transformed list is a different length than the messages it matches, so
      the positions no longer line up.
    - A transformed message carries a different number of values than the request
      message has places to put them, so the positions no longer line up inside
      that one message either.
    """
    if not messages:
        return False

    original = data.get("messages")
    if not isinstance(original, list) or not original:
        logger.warning("agento11y: skipping message transform, request body has no chat message list to rewrite")
        return False

    system_indices = set(_system_message_indices(original))
    numbered = [(index, message) for index, message in enumerate(original) if index not in system_indices]

    if len(messages) != len(numbered):
        # The forward mapping drops a message it cannot turn into text, and a rule
        # is free to return a shorter list, so matching by position here would
        # write a transform onto the wrong turn.
        logger.warning(
            "agento11y: skipping message transform, the transform carries %d messages and the request has %d, "
            "so the positions no longer line up",
            len(messages),
            len(numbered),
        )
        return False

    rewritten: dict[int, Any] = {}
    for (index, message), transformed in zip(numbered, messages, strict=True):
        updated, reason = _rewrite_message(message, transformed)
        if reason:
            logger.warning("agento11y: skipping message transform, message %d %s", index, reason)
            return False
        if updated is not None:
            rewritten[index] = updated

    if not rewritten:
        return False

    data["messages"] = [rewritten.get(index, message) for index, message in enumerate(original)]
    return True


def _rewrite_message(original: Any, transformed: Message) -> tuple[Any | None, str]:
    """Return the request message carrying ``transformed``'s text, or why it cannot.

    A ``tool`` message is matched as a whole rather than block by block, because
    that is how ``_map_messages`` read it: one tool result holding all of its
    content.

    Returns ``(None, "")`` when the transform has nothing to write for this
    message. That is the normal case for an assistant turn that only called a
    tool. It also covers a message a rule rewrote to nothing: the wire shape
    drops an empty text part, so "redact this message away" cannot be expressed
    and reads as "no change".

    The message and any block list it holds are copied rather than edited, so the
    body the caller still holds, and the one LiteLLM logs, keep the original text.
    """
    if not isinstance(original, dict):
        return None, "has no object form"

    texts = [part.text for part in transformed.parts if part.kind == PartKind.TEXT and part.text]
    results = [
        part.tool_result.content
        for part in transformed.parts
        if part.kind == PartKind.TOOL_RESULT and part.tool_result is not None
    ]
    if not texts and not results:
        return None, ""

    content = original.get("content")

    if isinstance(content, (str, list)) and str(original.get("role") or "").lower() == "tool":
        return _rewritten_tool_message(original, texts + results)

    if isinstance(content, str):
        # A string is one part on the way out, so it takes one value back.
        values = texts + results
        if len(values) != 1:
            return None, f"holds one string and the transform carries {len(values)} values for it"
        return ({**original, "content": values[0]}, "") if values[0] != content else (None, "")

    if isinstance(content, list):
        blocks, reason = _rewrite_blocks(content, texts, results)
        if blocks is None:
            return None, reason
        return ({**original, "content": blocks}, "") if blocks != content else (None, "")

    # A tool-call-only assistant turn has no content at all, and the transform
    # carries text for it only if a rule invented some.
    return None, "carries no content the transform can be written into"


def _rewrite_blocks(
    content: Sequence[Any], texts: Sequence[str], results: Sequence[str]
) -> tuple[list[Any] | None, str]:
    """Write transformed values into a content block list, or explain why not.

    Blocks are matched to values in the order ``content_parts`` read them: the
    nth text block takes the nth transformed text, and the nth ``tool_result``
    block the nth transformed result. A block that carried no text produced no
    part and takes no value, which is what keeps the two orders aligned. Every
    other block (image, ``tool_use``, ``thinking``) is copied through.

    The counts have to match exactly, in both directions. One value too few means
    a block would keep text a rule redacted, and one too many means a value has
    nowhere to go; either way this is not the request the guard evaluated.
    """
    text_slots = [index for index, block in enumerate(content) if _is_text_block(block)]
    if len(text_slots) != len(texts):
        return None, (
            f"holds {_counted(len(text_slots), 'text block', 'text blocks')} "
            f"and the transform carries {_counted(len(texts), 'text', 'texts')}"
        )

    result_slots = [index for index, block in enumerate(content) if _is_tool_result_block(block)]
    if len(result_slots) != len(results):
        return None, (
            f"holds {_counted(len(result_slots), 'tool result block', 'tool result blocks')} "
            f"and the transform carries {_counted(len(results), 'tool result', 'tool results')}"
        )

    rewritten: dict[int, Any] = {}
    for slot, text in zip(text_slots, texts, strict=True):
        rewritten[slot] = {**content[slot], "text": text}
    for slot, result in zip(result_slots, results, strict=True):
        block = _rewritten_tool_result(content[slot], result)
        if block is None:
            return None, f"holds a tool result block at position {slot} a rewrite cannot reproduce"
        rewritten[slot] = block

    return [rewritten.get(index, block) for index, block in enumerate(content)], ""


def _is_text_block(block: Any) -> bool:
    """Report whether ``content_parts`` read this block as one text part.

    A block whose text is blank produced no part, so it takes no value back. A
    ``refusal`` block is left out even though it does produce a text part: it is
    the provider's own wording, not something a request rewrites, so a
    conversation replaying one comes out one value short and skips the rewrite.
    """
    if not isinstance(block, dict):
        return False
    if str(block.get("type") or "").lower() not in _TEXT_BLOCK_TYPES:
        return False
    return bool(str(block.get("text") or "").strip())


def _is_tool_result_block(block: Any) -> bool:
    """Report whether ``content_parts`` read this block as one tool result part.

    ``tool_result`` is the one spelling it reads. A provider-specific variant such
    as ``web_search_tool_result`` is not mapped forward, so nothing comes back for
    it and it takes no value.
    """
    return isinstance(block, dict) and str(block.get("type") or "").lower() == "tool_result"


def _counted(count: int, singular: str, plural: str) -> str:
    return f"{count} {singular if count == 1 else plural}"


def _rewritten_tool_message(original: dict[str, Any], values: Sequence[str]) -> tuple[Any | None, str]:
    """Return an OpenAI ``tool`` message carrying the rewritten result, or why it cannot.

    ``_map_messages`` reads the whole content of a tool message as one tool
    result, so the transform carries one value for it however many blocks that
    content held. Counting text blocks against the transform here, the way a user
    or assistant message is counted, would find no text to match a tool result
    against and skip the rewrite of the whole conversation with it.
    """
    if len(values) != 1:
        return None, (
            f"holds one tool result and the transform carries {_counted(len(values), 'value', 'values')} for it"
        )
    updated = _rewritten_tool_result(original, values[0])
    if updated is None:
        return None, "holds tool result content a rewrite cannot reproduce"
    return (updated, "") if updated != original else (None, "")


def _rewritten_tool_result(block: dict[str, Any], result: str) -> dict[str, Any] | None:
    """Write a rewritten tool result into the ``content`` that carried it.

    Takes an Anthropic ``tool_result`` block or an OpenAI ``tool`` message; both
    hold the result in ``content``. ``content_parts`` flattens that content to
    text, so a string takes the rewritten value directly and a lone text block
    takes it while keeping its other fields. Returns ``None`` for content holding
    more than one nested block, or one nested block that is not text: their joined
    text is what was evaluated, and there is no way back to per-block values.
    """
    nested = block.get("content")
    if isinstance(nested, list):
        if len(nested) != 1 or not _is_text_block(nested[0]):
            return None
        return {**block, "content": [{**nested[0], "text": result}]}
    return {**block, "content": result}


def _system_message_indices(messages: Any) -> list[int]:
    """Positions of the system and developer messages in a chat message list."""
    if not isinstance(messages, list):
        return []
    return [
        index
        for index, message in enumerate(messages)
        if isinstance(message, dict) and str(message.get("role") or "").lower() in _SYSTEM_ROLES
    ]


def _block_collapse_reason(content: Sequence[Any]) -> str:
    """Say what a collapse of these blocks into one string would lose.

    Returns an empty string when the collapse loses nothing.
    """
    for block in content:
        if not isinstance(block, dict) or str(block.get("type") or "").lower() not in _TEXT_BLOCK_TYPES:
            return "non-text content"
        extra = sorted(set(block) - _TRANSFORMABLE_BLOCK_KEYS)
        if extra:
            return f"content block fields a rewrite cannot reproduce: {', '.join(extra)}"
    return ""


def _resolve_provider(data: dict, model_name: str) -> str:
    """Resolve the provider for the planned call, falling back to a placeholder.

    A request may carry ``custom_llm_provider`` outright. Otherwise the model
    string is mapped through LiteLLM, which covers both a prefixed model
    (``openai/gpt-4o-mini``) and a bare name LiteLLM knows (``gpt-4o``).

    Proxy deployment aliases resolve to ``UNKNOWN_PROVIDER``: pre-call runs
    before the router picks a deployment, and a model group can span providers,
    so the real provider is not knowable here. Rules that match on
    ``model.provider`` are unreliable through this adapter for that reason;
    match on ``model.name``, ``agent_name``, or tags instead.
    """
    explicit = data.get("custom_llm_provider")
    if isinstance(explicit, str) and explicit.strip():
        return explicit.strip().lower()
    return _provider_for_model(model_name)


@lru_cache(maxsize=512)
def _provider_for_model(model_name: str) -> str:
    """Map a model string to a provider, cached because this is a hot path.

    ``get_llm_provider`` walks LiteLLM's model map and raises on anything it
    cannot place. Caching keeps that off every request, and keeps LiteLLM's
    provider-list banner to once per unresolved model per process.
    """
    if not model_name.strip():
        return UNKNOWN_PROVIDER
    try:
        _, provider, _, _ = litellm.get_llm_provider(model=model_name)
    except Exception:  # noqa: BLE001
        return UNKNOWN_PROVIDER
    provider = (provider or "").strip().lower()
    return provider or UNKNOWN_PROVIDER


def _normalized_event_hook(event_hook: Any) -> Any:
    """Validate the configured mode and coerce it to a shape LiteLLM matches.

    A guardrail that runs on no phase runs silently, so both ways of building
    one fail at construction instead:

    - A mode this adapter does not implement. ``during_call`` and
      ``logging_only`` are valid LiteLLM modes, so nothing downstream objects.
    - A sequence with no modes in it, which matches nothing whatever its type.

    A tuple or a set is rewritten as a list. ``_event_hook_is_event_type``
    special-cases a list and a ``Mode`` and compares everything else to the mode
    string, an equality no tuple or set can satisfy.
    """
    supported = {hook.value for hook in SUPPORTED_EVENT_HOOKS}
    modes = _event_hook_values(event_hook)
    for mode in modes:
        if mode not in supported:
            raise ValueError(
                f"Agento11yLiteLLMGuardrail does not support mode {mode!r}; supported modes: {sorted(supported)}."
            )
    if isinstance(event_hook, (list, tuple, set)):
        if not modes:
            raise ValueError(
                f"Agento11yLiteLLMGuardrail needs at least one mode, got {event_hook!r}; "
                f"supported modes: {sorted(supported)}."
            )
        return modes
    return event_hook


def _event_hook_values(event_hook: Any) -> list[str]:
    """Flatten LiteLLM's several ``event_hook`` shapes into mode strings."""
    if event_hook is None:
        return []
    if isinstance(event_hook, GuardrailEventHooks):
        return [event_hook.value]
    if isinstance(event_hook, str):
        return [event_hook]
    if isinstance(event_hook, (list, tuple, set)):
        out: list[str] = []
        for item in event_hook:
            out.extend(_event_hook_values(item))
        return out
    # litellm.types.guardrails.Mode: tag-conditional modes plus a default.
    tags = getattr(event_hook, "tags", None)
    default = getattr(event_hook, "default", None)
    if tags is None and default is None:
        return [str(event_hook)]
    out = []
    if isinstance(tags, dict):
        for value in tags.values():
            out.extend(_event_hook_values(value))
    out.extend(_event_hook_values(default))
    return out


def _span_ids(span: Any) -> tuple[str, str]:
    """Read hex trace/span ids off an OTel span, tolerating anything else."""
    if span is None:
        return "", ""
    try:
        span_context = span.get_span_context()
        if not span_context.is_valid:
            return "", ""
        return format(span_context.trace_id, "032x"), format(span_context.span_id, "016x")
    except Exception:  # noqa: BLE001
        return "", ""


def _hook_input(data: dict) -> HookInput:
    """Build hook input from the raw proxy request body.

    The guard runs before LiteLLM translates the body, so the shape is whatever
    the route accepts: chat and ``/v1/messages`` carry ``messages``,
    ``/v1/responses`` carries ``input``, text completion and image generation
    carry ``prompt``. The system prompt is part of the message list only on chat
    routes; ``/v1/messages`` sends it as top-level ``system`` and
    ``/v1/responses`` as ``instructions``.

    Keys are read by shape rather than by call type, because one call type
    carries more than one shape: LiteLLM's Anthropic adapter route runs an
    Anthropic body, ``messages`` plus top-level ``system``, through
    ``pre_call_hook`` as call type ``text_completion``.
    """
    messages, message_system_prompt = _map_messages(_first_present(data, _INPUT_KEYS))
    system_chunks = [_top_level_system_prompt(data), message_system_prompt]
    return HookInput(
        messages=messages,
        tools=_map_tools_list(data.get("tools")),
        system_prompt="\n\n".join(chunk for chunk in system_chunks if chunk),
        conversation_preview=_last_user_text(messages),
    )


def _first_present(data: dict, keys: Sequence[str]) -> Any:
    """Return the first non-empty value among ``keys``."""
    for key in keys:
        value = data.get(key)
        if value:
            return value
    return None


def _top_level_system_prompt(data: dict) -> str:
    """Join the system prompt fields that sit outside the message list.

    Anthropic ``system`` is a string or a list of text blocks; Responses API
    ``instructions`` is a string. A body carries at most one of them, but both
    are read so neither route depends on the call type being recognized.
    """
    chunks = [_extract_text_content(data.get(key)) for key in _SYSTEM_PROMPT_KEYS]
    return "\n\n".join(chunk for chunk in chunks if chunk)


def _last_user_text(messages: Sequence[Message]) -> str:
    """Return the text of the last user message, for rule preview matching.

    Reads mapped messages rather than the request body, so a text-completion
    ``prompt`` and a ``/v1/responses`` ``input`` item are covered too: both
    arrive here as user messages.
    """
    for message in reversed(messages):
        if message.role != MessageRole.USER:
            continue
        text = " ".join(part.text for part in message.parts if part.kind == PartKind.TEXT and part.text)
        if text:
            return text
    return ""
