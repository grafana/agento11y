"""grafana-agento11y-hermes: Grafana Agent Observability plugin for Hermes Agent.

Records every LLM API call (`pre_api_request`/`post_api_request`) as a
generation and every tool invocation (`post_tool_call`) as a tool execution.
On `on_session_end`, flushes the SDK's HTTP exporter and any OTel providers the
plugin installed.

Configuration is the canonical ``AGENTO11Y_*`` schema for the generations
channel and the standard OpenTelemetry ``OTEL_*`` schema for the OTel channel.
See README:
  - Generations:  ``AGENTO11Y_ENDPOINT`` / ``AGENTO11Y_PROTOCOL`` /
                  ``AGENTO11Y_AUTH_*``
  - OTel:         ``OTEL_EXPORTER_OTLP_ENDPOINT`` /
                  ``OTEL_EXPORTER_OTLP_HEADERS`` / ``OTEL_SERVICE_NAME``
  - Plugin-only:  ``AGENTO11Y_HERMES_SAMPLE_RATE`` /
                  ``AGENTO11Y_HERMES_MAX_CHARS`` / ``AGENTO11Y_HERMES_OTEL_AUTO``

The plugin fails open: missing credentials, SDK errors, exporter failures, and
network errors all become silent no-ops after at most one warning log. The
hermes agent loop is never blocked or interrupted by telemetry issues.
"""

from __future__ import annotations

from ._compat import apply_legacy_env
from ._hooks import (
    on_api_request_error,
    on_post_api_request,
    on_post_llm_call,
    on_post_tool_call,
    on_pre_api_request,
    on_pre_llm_call,
    on_session_end,
    on_session_finalize,
)


def register(ctx) -> None:
    # Runs before anything reads config, so the SDK sees only AGENTO11Y_* names.
    apply_legacy_env()

    # LEGACY: pre_llm_call / post_llm_call are turn-scoped and serve only the
    # fallback for hermes older than v2026.6.5 (PyPI 0.16.0), which sends no
    # api_request_id. On current hermes the API-request hooks carry both the
    # input messages and the assistant message, so a generation opens and
    # closes within them.
    #
    # We deliberately do not register pre_tool_call: post_tool_call is the only
    # hook of the pair carrying the result, the status and duration_ms, so a
    # recorder opened in pre would sit open across the call for nothing.
    #
    # api_request_error closes the generation for a call that failed. It covers
    # the retryable provider failures, but not every retry path: some re-enter
    # pre_api_request with the same api_request_id and fire no hook at all,
    # which is why on_pre_api_request also closes whatever a repeated id
    # displaces.
    ctx.register_hook("pre_llm_call", on_pre_llm_call)
    ctx.register_hook("post_llm_call", on_post_llm_call)
    ctx.register_hook("pre_api_request", on_pre_api_request)
    ctx.register_hook("post_api_request", on_post_api_request)
    ctx.register_hook("api_request_error", on_api_request_error)
    #
    # on_session_end fires per completed turn, so a turn that dies on a
    # provider error never reaches it. on_session_finalize fires once at CLI
    # exit either way, and is the only session hook the interactive failure
    # path gets.
    ctx.register_hook("post_tool_call", on_post_tool_call)
    ctx.register_hook("on_session_end", on_session_end)
    ctx.register_hook("on_session_finalize", on_session_finalize)


__all__ = [
    "register",
    "on_pre_llm_call",
    "on_post_llm_call",
    "on_pre_api_request",
    "on_post_api_request",
    "on_api_request_error",
    "on_post_tool_call",
    "on_session_end",
    "on_session_finalize",
]
