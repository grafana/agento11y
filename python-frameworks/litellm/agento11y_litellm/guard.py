"""LiteLLM guardrail that enforces Agent Observability preflight hook rules.

Pre-call hooks are a proxy-only LiteLLM feature: ``ProxyLogging.pre_call_hook``
invokes them, and nothing on the ``litellm.completion()`` SDK path does. A direct
SDK call is therefore never guarded by this class.
"""

from __future__ import annotations

import asyncio
import logging
from collections.abc import Sequence
from concurrent.futures import ThreadPoolExecutor
from contextvars import copy_context
from dataclasses import replace
from functools import lru_cache, partial
from typing import TYPE_CHECKING, Any

import litellm
from agento11y import Client
from agento11y.errors import HookTransportError
from agento11y.hooks import (
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
    DEFAULT_AGENT_NAME_METADATA_KEYS,
    FRAMEWORK_TAGS,
    _extract_text_content,
    _first_metadata_value,
    _map_messages,
    _map_tools_list,
    _metadata_sources_from,
    _request_tags_from,
    _resolve_conversation_id_from,
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

# Only preflight is wired up. post_call arrives with postflight support.
SUPPORTED_EVENT_HOOKS = (GuardrailEventHooks.pre_call,)

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

# Body keys that carry the system prompt outside the message list:
# ``/v1/messages`` sends ``system``, ``/v1/responses`` sends ``instructions``.
_SYSTEM_PROMPT_KEYS = ("system", "instructions")

_GUARD_FRAMEWORK_TAGS = {**FRAMEWORK_TAGS, "agento11y.framework.source": "guardrail"}


class Agento11yLiteLLMGuardrail(CustomGuardrail):
    """Evaluates agento11y hook rules before a proxy request reaches the provider.

    Deliberately not a subclass of ``Agento11yLiteLLMLogger``. Both objects sit
    in ``litellm.callbacks`` at once, and a bare ``CustomGuardrail`` is harmless
    there only because it inherits ``CustomLogger``'s no-op logging methods. Give
    this class the logger's export behavior and every generation exports twice.

    ``ProxyLogging.pre_call_hook`` walks ``litellm.callbacks`` and runs every
    ``CustomGuardrail`` it finds, so a dotted path next to the logger is enough::

        litellm_settings:
          callbacks:
            - agento11y_callback.agento11y_handler
            - agento11y_callback.agento11y_guards
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
        extra_tags: dict[str, str] | None = None,
        **kwargs: Any,
    ) -> None:
        if max_concurrent_evaluations <= 0:
            raise ValueError(f"max_concurrent_evaluations must be greater than zero, got {max_concurrent_evaluations}")
        if request_timeout_seconds <= 0:
            raise ValueError(f"request_timeout_seconds must be greater than zero, got {request_timeout_seconds}")

        event_hook = kwargs.pop("event_hook", None)
        _reject_unsupported_event_hooks(event_hook)

        super().__init__(
            guardrail_name=kwargs.pop("guardrail_name", None) or DEFAULT_GUARDRAIL_NAME,
            supported_event_hooks=list(SUPPORTED_EVENT_HOOKS),
            # Kept as configured so LiteLLM's tag-conditional Mode form keeps
            # working.
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
        # an empty list from reading as "preflight not configured".
        phases = hooks.phases or [HookPhase.PREFLIGHT.value]
        self._preflight_configured = hooks.enabled and HookPhase.PREFLIGHT.value in phases
        self._fail_open = hooks.fail_open
        # Evaluations are submitted fail-closed so an SDK-side transport failure
        # reaches this adapter instead of resolving to a synthetic allow.
        self._fail_closed_hooks = replace(hooks, fail_open=False)
        if not self._preflight_configured:
            logger.warning(
                "agento11y: guardrail %r is registered but the client's hooks config does not enable preflight; "
                "no request will be evaluated",
                self.guardrail_name,
            )
        self._agent_name = agent_name
        self._agent_name_metadata_keys = tuple(agent_name_metadata_keys)
        self._agent_version = agent_version
        self._max_concurrent_evaluations = max_concurrent_evaluations
        self._request_timeout_seconds = request_timeout_seconds
        self._extra_tags = dict(extra_tags) if extra_tags else {}
        self._executor = ThreadPoolExecutor(max_workers=self._max_concurrent_evaluations)

    async def async_pre_call_hook(
        self,
        user_api_key_dict: UserAPIKeyAuth,
        cache: DualCache,
        data: dict,
        call_type: str,
    ) -> None:
        """Blocks the request when an agento11y preflight rule denies it.

        Returns ``None`` on allow so the proxy forwards the request body
        untouched.

        Every gate that ends in "do not evaluate" sits outside the decorated
        body, so a request this guardrail did not check records no verdict at
        all: the client's hooks config does not enable preflight, the guardrail
        is not enabled for the request, the route carries input this adapter
        cannot map, or the mapped input is empty. The proxy applies the
        enablement gate before calling in, so that one only matters for direct
        invocation.

        A failed evaluation does record a verdict: the exception crosses
        ``_run_preflight``'s decorator, which files
        ``guardrail_failed_to_respond``, and this method then applies
        ``HooksConfig.fail_open``.
        """
        # Checked first: the result cannot change per request.
        if not self._preflight_configured:
            logger.debug("agento11y: skipping preflight evaluation, the client's hooks config disables preflight")
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
            return await self._run_preflight(user_api_key_dict=user_api_key_dict, data=data, hook_input=hook_input)
        except HookTransportError as exc:
            if not self._fail_open:
                raise
            logger.warning(
                "agento11y: guardrail %r allowing request (fail_open): %s",
                self.guardrail_name,
                exc,
            )
            return None

    @log_guardrail_information
    async def _run_preflight(self, *, user_api_key_dict: UserAPIKeyAuth, data: dict, hook_input: HookInput) -> None:
        """Evaluate preflight rules and translate a deny into a proxy 400.

        ``transformed_input`` from the evaluator is ignored: applying it would
        drop every tool call and tool result, because the SDK's wire parser
        keeps only text and thinking parts.
        """
        request = HookEvaluateRequest(
            phase=HookPhase.PREFLIGHT.value,
            context=self._context(data, user_api_key_dict),
            input=hook_input,
        )

        response = await self._evaluate(request)
        denied = hook_denied_from_response(response)
        if denied is None:
            return None

        detail = f"{denied.reason} (rule {denied.rule_id})" if denied.rule_id else denied.reason
        raise GuardrailRaisedException(
            guardrail_name=self.guardrail_name,
            message=f"blocked by guardrail {self.guardrail_name}: {detail}",
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
        the real duration. ``async_pre_call_hook`` applies
        ``HooksConfig.fail_open`` afterwards, so the allow-or-raise outcome
        still follows the configured policy.
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
                f"agento11y hook evaluation failed: timed out after {self._request_timeout_seconds}s"
            ) from exc
        except Exception as exc:  # noqa: BLE001
            raise HookTransportError(f"agento11y hook evaluation failed: {type(exc).__name__}: {exc}") from exc


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


def _reject_unsupported_event_hooks(event_hook: Any) -> None:
    """Raise on any configured mode other than ``pre_call``.

    A guardrail configured with an unsupported mode would silently never run;
    failing at construction surfaces the misconfiguration at proxy startup.
    """
    supported = {hook.value for hook in SUPPORTED_EVENT_HOOKS}
    for mode in _event_hook_values(event_hook):
        if mode not in supported:
            raise ValueError(
                f"Agento11yLiteLLMGuardrail does not support mode {mode!r}; "
                f"supported modes: {sorted(supported)}. Postflight evaluation is not implemented yet."
            )


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
