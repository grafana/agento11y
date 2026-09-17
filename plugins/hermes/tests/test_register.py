"""Verify register(ctx) binds exactly the expected hooks."""

from __future__ import annotations

import grafana_agento11y_hermes

EXPECTED_HOOKS = {
    "pre_llm_call",
    "post_llm_call",
    "pre_api_request",
    "post_api_request",
    "api_request_error",
    "post_tool_call",
    "on_session_end",
    "on_session_finalize",
}


def test_register_binds_exactly_expected_hooks(ctx) -> None:
    grafana_agento11y_hermes.register(ctx)
    assert set(ctx.hooks.keys()) == EXPECTED_HOOKS


def test_register_handlers_are_callable(ctx) -> None:
    grafana_agento11y_hermes.register(ctx)
    for name, handler in ctx.hooks.items():
        assert callable(handler), f"handler for {name} is not callable"


def test_register_does_not_bind_session_start(ctx) -> None:
    grafana_agento11y_hermes.register(ctx)
    assert "on_session_start" not in ctx.hooks
