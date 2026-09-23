"""Hermes plugin that records every hook invocation to a JSONL file.

Ground truth for what the running hermes build passes each hook. The agento11y
plugin's assumptions are checked against this, not against the hook docs, which
lag the call sites. Values are shrunk so one file read stays cheap: strings clip
at 400 characters, containers at 20-40 entries, nesting at depth 3.

Set HOOKDUMP_FILE to move the output.
"""

from __future__ import annotations

import json
import os
import time
from typing import Any

HOOKS = (
    "pre_api_request",
    "post_api_request",
    "api_request_error",
    "pre_llm_call",
    "post_llm_call",
    "pre_tool_call",
    "post_tool_call",
    "on_session_start",
    "on_session_end",
    "on_session_finalize",
)

OUT = os.environ.get("HOOKDUMP_FILE") or os.path.join(
    os.environ.get("E2E_DIR", "/tmp/agento11y-hermes-e2e"), "hooks.jsonl"
)


def _shrink(value: Any, depth: int = 0) -> Any:
    if depth > 3:
        return "<depth>"
    if value is None or isinstance(value, (bool, int, float)):
        return value
    if isinstance(value, str):
        return value if len(value) <= 400 else value[:400] + f"...<{len(value)} chars>"
    if isinstance(value, dict):
        return {str(k): _shrink(v, depth + 1) for k, v in list(value.items())[:40]}
    if isinstance(value, (list, tuple)):
        return [_shrink(v, depth + 1) for v in list(value)[:20]]
    # Objects (hermes passes assistant messages as objects): keep the fields a
    # telemetry plugin would read.
    out: dict[str, Any] = {"__type__": type(value).__name__}
    for attr in ("role", "content", "tool_calls", "model", "id", "usage"):
        if hasattr(value, attr):
            out[attr] = _shrink(getattr(value, attr), depth + 1)
    return out


def _record(hook: str, kwargs: dict[str, Any]) -> None:
    row = {
        "t": time.time(),
        "hook": hook,
        "keys": sorted(kwargs),
        "payload": {key: _shrink(value) for key, value in kwargs.items()},
    }
    with open(OUT, "a") as handle:
        handle.write(json.dumps(row, default=str) + "\n")


def _make(hook: str):
    def handler(**kwargs: Any) -> None:
        try:
            _record(hook, kwargs)
        except Exception as exc:  # a probe must never break the agent loop
            with open(OUT, "a") as handle:
                handle.write(json.dumps({"hook": hook, "error": repr(exc)}) + "\n")

    return handler


def register(ctx) -> None:
    for hook in HOOKS:
        ctx.register_hook(hook, _make(hook))
