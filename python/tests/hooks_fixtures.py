"""Loaders for the cross-language hook wire fixtures in `conformance/hooks/`.

The preflight request is built with public SDK types so the conformance suite
compares what a caller can actually produce against the shared contract. See
`conformance/hooks/README.md` for the encoding rules.
"""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

from agento11y import (
    HookContext,
    HookEvaluateRequest,
    HookInput,
    HookModel,
    HookPhase,
    Message,
    MessageRole,
    Part,
    PartKind,
    ToolCall,
    ToolDefinition,
    ToolResult,
)

FIXTURE_DIR = Path(__file__).resolve().parents[2] / "conformance" / "hooks"

BASH_SCHEMA = (
    b'{"type":"object","properties":{"command":{"type":"string",'
    b'"description":"Shell command to run."}},"required":["command"]}'
)
READ_FILE_SCHEMA = b'{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}'


def load_preflight_request() -> dict[str, Any]:
    """Returns the parsed `request-preflight.json` fixture."""

    return json.loads((FIXTURE_DIR / "request-preflight.json").read_text(encoding="utf-8"))


def load_postflight_guard_request() -> dict[str, Any]:
    """Returns the parsed `request-postflight-guard.json` fixture."""

    return json.loads((FIXTURE_DIR / "request-postflight-guard.json").read_text(encoding="utf-8"))


def load_responses() -> dict[str, Any]:
    """Returns the parsed `responses.json` fixture keyed by scenario name."""

    return json.loads((FIXTURE_DIR / "responses.json").read_text(encoding="utf-8"))


def diff_json(got: Any, want: Any, path: str = "") -> list[str]:
    """Reports structural differences as dotted JSON paths plus both values.

    A failure has to name the offending field: the divergences this suite exists
    to catch are single renamed keys and single re-encoded payloads.
    """

    label = path or "<root>"
    if isinstance(want, dict):
        if not isinstance(got, dict):
            return [f"{label}: got {json.dumps(got)}, want an object"]
        diffs: list[str] = []
        for key in sorted(set(want) | set(got)):
            child = f"{path}.{key}" if path else key
            if key not in got:
                diffs.append(f"{child}: missing, want {json.dumps(want[key])}")
            elif key not in want:
                diffs.append(f"{child}: unexpected {json.dumps(got[key])}")
            else:
                diffs.extend(diff_json(got[key], want[key], child))
        return diffs
    if isinstance(want, list):
        if not isinstance(got, list):
            return [f"{label}: got {json.dumps(got)}, want an array"]
        if len(got) != len(want):
            return [f"{label}: got {len(got)} items, want {len(want)}"]
        diffs = []
        for index, (got_item, want_item) in enumerate(zip(got, want, strict=True)):
            diffs.extend(diff_json(got_item, want_item, f"{path}[{index}]"))
        return diffs
    if got != want or isinstance(got, bool) != isinstance(want, bool):
        return [f"{label}: got {json.dumps(got)}, want {json.dumps(want)}"]
    return []


def postflight_guard_request() -> HookEvaluateRequest:
    """Builds the postflight guard request from public Python SDK types.

    The shipped guards evaluate a tool call under `input.output`, and the server's
    tool filter scans that field before `input.messages`.
    """

    return HookEvaluateRequest(
        phase=HookPhase.POSTFLIGHT.value,
        context=HookContext(
            agent_name="conformance-guard",
            agent_version="1.2.3",
            conversation_id="conv-hooks-conformance",
            model=HookModel(provider="anthropic", name="claude-sonnet-4"),
        ),
        input=HookInput(
            output=[
                Message(
                    role=MessageRole.ASSISTANT,
                    parts=[
                        Part(
                            kind=PartKind.TOOL_CALL,
                            tool_call=ToolCall(
                                id="call-bash",
                                name="Bash",
                                input_json=b'{"command":"rm -rf /tmp/cache"}',
                            ),
                        )
                    ],
                )
            ]
        ),
    )


def preflight_request() -> HookEvaluateRequest:
    """Builds the preflight hook request from public Python SDK types."""

    return HookEvaluateRequest(
        phase=HookPhase.PREFLIGHT.value,
        context=HookContext(
            agent_name="conformance-agent",
            agent_version="1.2.3",
            model=HookModel(provider="anthropic", name="claude-sonnet-4"),
            tags={"env": "test", "team": "agent-observability"},
            conversation_id="conv-hooks-conformance",
            trace_id="0123456789abcdef0123456789abcdef",
            span_id="0123456789abcdef",
        ),
        input=HookInput(
            system_prompt="You are a careful assistant.",
            messages=[
                Message(
                    role=MessageRole.USER,
                    parts=[Part(kind=PartKind.TEXT, text="Delete the cache directory under /tmp.")],
                ),
                Message(
                    role=MessageRole.ASSISTANT,
                    parts=[
                        Part(
                            kind=PartKind.THINKING,
                            thinking="The request is destructive, so inspect the directory first.",
                        ),
                        Part(
                            kind=PartKind.TOOL_CALL,
                            tool_call=ToolCall(
                                id="call-read",
                                name="read_file",
                                input_json=b'{"path":"/tmp/cache/manifest.json"}',
                            ),
                        ),
                        Part(
                            kind=PartKind.TOOL_CALL,
                            tool_call=ToolCall(
                                id="call-bash",
                                name="Bash",
                                input_json=b'{"command":"rm -rf /tmp/cache"}',
                            ),
                        ),
                    ],
                ),
                Message(
                    role=MessageRole.TOOL,
                    name="read_file",
                    parts=[
                        Part(
                            kind=PartKind.TOOL_RESULT,
                            tool_result=ToolResult(
                                tool_call_id="call-read",
                                name="read_file",
                                content="3 entries",
                                content_json=b'{"entries":3}',
                            ),
                        )
                    ],
                ),
                Message(
                    role=MessageRole.TOOL,
                    name="Bash",
                    parts=[
                        Part(
                            kind=PartKind.TOOL_RESULT,
                            tool_result=ToolResult(
                                tool_call_id="call-bash",
                                name="Bash",
                                is_error=True,
                                content="rm: cannot remove '/tmp/cache': Permission denied",
                                content_json=b'{"exit_code":1}',
                            ),
                        )
                    ],
                ),
            ],
            tools=[
                ToolDefinition(
                    name="Bash",
                    description="Run a shell command.",
                    type="function",
                    input_schema_json=BASH_SCHEMA,
                ),
                ToolDefinition(
                    name="read_file",
                    description="Read a file from disk.",
                    type="function",
                    input_schema_json=READ_FILE_SCHEMA,
                ),
            ],
            conversation_preview="user: Delete the cache directory under /tmp.",
        ),
    )
