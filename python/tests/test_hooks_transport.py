"""Tests for synchronous hook evaluation transport."""

from __future__ import annotations

import contextlib
import json
import threading
from collections.abc import Iterator
from dataclasses import replace
from datetime import timedelta
from http.server import BaseHTTPRequestHandler, HTTPServer
from typing import Any

import pytest
from agento11y import (
    ApiConfig,
    AuthConfig,
    Client,
    ClientConfig,
    GenerationExportConfig,
    HookContext,
    HookDeniedError,
    HookEvaluateRequest,
    HookInput,
    HookModel,
    HookPhase,
    HooksConfig,
    HookTransportError,
    Message,
    MessageRole,
    Part,
    PartKind,
    ToolCall,
    ToolDefinition,
    ToolResult,
    hook_denied_from_response,
    user_text_message,
    with_conversation_id,
)
from opentelemetry import context as otel_context
from opentelemetry import trace


def test_parse_hook_response_includes_transformed_input() -> None:
    from agento11y.hooks import _parse_response

    parsed = _parse_response(
        {
            "action": "allow",
            "transformed_input": {"conversation_preview": "[REDACTED]"},
            "evaluations": [],
        }
    )
    assert parsed.action == "allow"
    assert parsed.transformed_input is not None
    assert parsed.transformed_input.conversation_preview == "[REDACTED]"


def test_parse_hook_response_transformed_messages_numeric_proto_role() -> None:
    from agento11y.hooks import _parse_response

    parsed = _parse_response(
        {
            "action": "allow",
            "transformed_input": {
                "messages": [
                    {"role": 2, "parts": [{"text": "hello"}]},
                ],
            },
            "evaluations": [],
        }
    )
    assert parsed.transformed_input is not None
    assert len(parsed.transformed_input.messages) == 1
    assert parsed.transformed_input.messages[0].role == MessageRole.ASSISTANT


def test_parse_hook_response_keeps_a_tool_using_conversation() -> None:
    """A transform of an agent turn keeps its tool history, part for part.

    ``agento11y_litellm.guard`` matches transformed messages to the request by
    position, so every message has to come back and in order. The one thing the
    parser cannot recover is the role: ``system`` arrives as ``user``, because
    the wire enum has no value for it.
    """
    from agento11y.hooks import _parse_response

    parsed = _parse_response(
        {
            "action": "allow",
            "transformed_input": {
                "messages": [
                    {"role": "user", "parts": [{"kind": "text", "text": "list the temp dir"}]},
                    {
                        "role": "assistant",
                        "parts": [{"kind": "tool_call", "tool_call": {"id": "call_1", "name": "shell_exec"}}],
                    },
                    {
                        "role": "tool",
                        "parts": [{"kind": "tool_result", "tool_result": {"tool_call_id": "call_1", "content": "ok"}}],
                    },
                    {"role": "system", "parts": [{"kind": "text", "text": "policy"}]},
                ],
            },
        }
    )

    assert parsed.transformed_input is not None
    messages = parsed.transformed_input.messages
    assert [m.role for m in messages] == [
        MessageRole.USER,
        MessageRole.ASSISTANT,
        MessageRole.TOOL,
        MessageRole.USER,
    ]
    assert [[p.kind for p in m.parts] for m in messages] == [
        [PartKind.TEXT],
        [PartKind.TOOL_CALL],
        [PartKind.TOOL_RESULT],
        [PartKind.TEXT],
    ]
    assert messages[1].parts[0].tool_call is not None
    assert messages[1].parts[0].tool_call.name == "shell_exec"
    assert messages[2].parts[0].tool_result is not None
    assert messages[2].parts[0].tool_result.content == "ok"


def test_evaluate_hook_disabled_short_circuits() -> None:
    class _Handler(BaseHTTPRequestHandler):
        def do_POST(self):  # noqa: N802
            self.send_error(500)

        def log_message(self, _format, *_args):  # noqa: A003
            return

    server = HTTPServer(("127.0.0.1", 0), _Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()

    client = _new_client(
        f"http://127.0.0.1:{server.server_address[1]}",
        hooks=HooksConfig(enabled=False),
    )
    try:
        response = client.evaluate_hook(
            HookEvaluateRequest(
                phase=HookPhase.PREFLIGHT.value,
                context=HookContext(model=HookModel(provider="openai", name="gpt-4o")),
                input=HookInput(),
            )
        )
        assert response.action == "allow"
    finally:
        client.shutdown()
        server.shutdown()
        server.server_close()


def test_evaluate_hook_posts_to_hooks_evaluate() -> None:
    captured: dict[str, object] = {}

    class _Handler(BaseHTTPRequestHandler):
        def do_POST(self):  # noqa: N802
            length = int(self.headers.get("Content-Length", "0"))
            body = self.rfile.read(length)
            captured["path"] = self.path
            captured["headers"] = {k.lower(): v for k, v in self.headers.items()}
            captured["payload"] = json.loads(body.decode("utf-8"))
            out = {
                "action": "allow",
                "evaluations": [
                    {
                        "rule_id": "pii",
                        "evaluator_id": "ev-pii",
                        "evaluator_kind": "regex",
                        "passed": True,
                        "latency_ms": 12,
                    }
                ],
            }
            encoded = json.dumps(out).encode("utf-8")
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(encoded)))
            self.end_headers()
            self.wfile.write(encoded)

        def log_message(self, _format, *_args):  # noqa: A003
            return

    server = HTTPServer(("127.0.0.1", 0), _Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()

    client = _new_client(
        f"http://127.0.0.1:{server.server_address[1]}",
        hooks=HooksConfig(
            enabled=True,
            phases=["preflight"],
            timeout_seconds=15.0,
        ),
        auth=AuthConfig(mode="tenant", tenant_id="tenant-a"),
    )
    try:
        response = client.evaluate_hook(
            HookEvaluateRequest(
                phase=HookPhase.PREFLIGHT.value,
                context=HookContext(
                    agent_name="agent-a",
                    agent_version="1.0.0",
                    model=HookModel(provider="openai", name="gpt-4o"),
                    tags={"env": "test"},
                ),
                input=HookInput(
                    system_prompt="be helpful",
                    messages=[user_text_message("hello world")],
                ),
            )
        )
        assert captured["path"] == "/api/v1/hooks:evaluate"
        headers = captured["headers"]
        assert isinstance(headers, dict)
        assert headers.get("x-agento11y-hook-timeout-ms") == "15000"
        assert "x-sigil-hook-timeout-ms" not in headers
        assert headers.get("x-scope-orgid") == "tenant-a"
        assert headers.get("content-type") == "application/json"
        payload = captured["payload"]
        assert isinstance(payload, dict)
        assert payload.get("phase") == "preflight"
        assert payload.get("context", {}).get("agent_name") == "agent-a"
        assert payload.get("context", {}).get("model", {}).get("name") == "gpt-4o"
        messages = payload.get("input", {}).get("messages", [])
        assert len(messages) == 1
        assert messages[0].get("role") == "user", "message role should be a string, not an integer"
        assert response.action == "allow"
        assert len(response.evaluations) == 1
        assert response.evaluations[0].rule_id == "pii"
    finally:
        client.shutdown()
        server.shutdown()
        server.server_close()


def test_evaluate_hook_adds_correlation_from_context() -> None:
    with _capturing_hook_server({"action": "allow", "evaluations": []}) as (captured, endpoint):
        client = _new_client(endpoint, hooks=HooksConfig(enabled=True))
        span_context = trace.SpanContext(
            trace_id=int("0123456789abcdef0123456789abcdef", 16),
            span_id=int("0123456789abcdef", 16),
            is_remote=False,
            trace_flags=trace.TraceFlags(1),
        )
        token = otel_context.attach(trace.set_span_in_context(trace.NonRecordingSpan(span_context)))
        try:
            with with_conversation_id("conv-guarded"):
                client.evaluate_hook(
                    HookEvaluateRequest(
                        phase=HookPhase.PREFLIGHT.value,
                        context=HookContext(model=HookModel(provider="openai", name="gpt-4o")),
                        input=HookInput(),
                    )
                )
        finally:
            otel_context.detach(token)
            client.shutdown()

    payload = captured["payload"]
    assert isinstance(payload, dict)
    context_payload = payload["context"]
    assert isinstance(context_payload, dict)
    assert context_payload["conversation_id"] == "conv-guarded"
    assert context_payload["trace_id"] == "0123456789abcdef0123456789abcdef"
    assert context_payload["span_id"] == "0123456789abcdef"


def test_evaluate_hook_deny() -> None:
    class _Handler(BaseHTTPRequestHandler):
        def do_POST(self):  # noqa: N802
            out = {
                "action": "deny",
                "rule_id": "rule-block",
                "reason": "nope",
                "evaluations": [
                    {
                        "rule_id": "rule-block",
                        "evaluator_id": "ev-1",
                        "evaluator_kind": "static",
                        "passed": False,
                        "latency_ms": 1,
                    }
                ],
            }
            encoded = json.dumps(out).encode("utf-8")
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(encoded)))
            self.end_headers()
            self.wfile.write(encoded)

        def log_message(self, _format, *_args):  # noqa: A003
            return

    server = HTTPServer(("127.0.0.1", 0), _Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()

    client = _new_client(
        f"http://127.0.0.1:{server.server_address[1]}",
        hooks=HooksConfig(enabled=True),
    )
    try:
        response = client.evaluate_hook(
            HookEvaluateRequest(
                phase=HookPhase.PREFLIGHT.value,
                context=HookContext(model=HookModel(provider="openai", name="gpt-4o")),
                input=HookInput(),
            )
        )
        assert response.is_deny
        err = hook_denied_from_response(response)
        assert isinstance(err, HookDeniedError)
        assert err.rule_id == "rule-block"
        assert "nope" in err.reason or "nope" in str(err)
    finally:
        client.shutdown()
        server.shutdown()
        server.server_close()


def test_evaluate_hook_fails_open_on_error() -> None:
    class _Handler(BaseHTTPRequestHandler):
        def do_POST(self):  # noqa: N802
            self.send_error(500)

        def log_message(self, _format, *_args):  # noqa: A003
            return

    server = HTTPServer(("127.0.0.1", 0), _Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()

    client = _new_client(
        f"http://127.0.0.1:{server.server_address[1]}",
        hooks=HooksConfig(enabled=True, fail_open=True),
    )
    try:
        response = client.evaluate_hook(
            HookEvaluateRequest(
                phase=HookPhase.PREFLIGHT.value,
                context=HookContext(model=HookModel(provider="openai", name="gpt-4o")),
                input=HookInput(),
            )
        )
        assert response.action == "allow"
    finally:
        client.shutdown()
        server.shutdown()
        server.server_close()


def test_evaluate_hook_fails_closed() -> None:
    class _Handler(BaseHTTPRequestHandler):
        def do_POST(self):  # noqa: N802
            self.send_error(500)

        def log_message(self, _format, *_args):  # noqa: A003
            return

    server = HTTPServer(("127.0.0.1", 0), _Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()

    client = _new_client(
        f"http://127.0.0.1:{server.server_address[1]}",
        hooks=HooksConfig(enabled=True, fail_open=False),
    )
    try:
        with pytest.raises(HookTransportError):
            client.evaluate_hook(
                HookEvaluateRequest(
                    phase=HookPhase.PREFLIGHT.value,
                    context=HookContext(model=HookModel(provider="openai", name="gpt-4o")),
                    input=HookInput(),
                )
            )
    finally:
        client.shutdown()
        server.shutdown()
        server.server_close()


def test_evaluate_hook_skips_mismatched_phase() -> None:
    class _Handler(BaseHTTPRequestHandler):
        def do_POST(self):  # noqa: N802
            self.send_error(500)

        def log_message(self, _format, *_args):  # noqa: A003
            return

    server = HTTPServer(("127.0.0.1", 0), _Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()

    client = _new_client(
        f"http://127.0.0.1:{server.server_address[1]}",
        hooks=HooksConfig(enabled=True, phases=["postflight"]),
    )
    try:
        response = client.evaluate_hook(
            HookEvaluateRequest(
                phase=HookPhase.PREFLIGHT.value,
                context=HookContext(model=HookModel(provider="openai", name="gpt-4o")),
                input=HookInput(),
            )
        )
        assert response.action == "allow"
    finally:
        client.shutdown()
        server.shutdown()
        server.server_close()


def test_evaluate_hook_override_forces_fail_closed() -> None:
    """A per-call override lets a caller see the transport failure itself.

    An adapter that reports a verdict to an external system has to distinguish
    a server allow from an SDK fail-open allow, and the response carries no
    marker separating the two.
    """
    client = _new_client("http://127.0.0.1:1", hooks=HooksConfig(enabled=True, fail_open=True))
    try:
        with pytest.raises(HookTransportError):
            client.evaluate_hook(
                _preflight_request(),
                hooks=replace(client.hooks_config, fail_open=False),
            )

        assert client.hooks_config.fail_open is True
    finally:
        client.shutdown()


def test_evaluate_hook_override_enables_a_disabled_client_for_one_call() -> None:
    with _capturing_hook_server({"action": "allow"}) as (captured, base_url):
        client = _new_client(base_url, hooks=HooksConfig(enabled=False))
        try:
            response = client.evaluate_hook(
                _preflight_request(),
                hooks=HooksConfig(enabled=True, phases=["preflight"], fail_open=False),
            )
            assert response.action == "allow"
            assert captured["path"] == "/api/v1/hooks:evaluate"

            assert client.hooks_config.enabled is False
            # The client's own configuration still short-circuits.
            captured.clear()
            assert client.evaluate_hook(_preflight_request()).action == "allow"
            assert captured == {}
        finally:
            client.shutdown()


def test_evaluate_hook_without_override_keeps_client_configuration() -> None:
    client = _new_client("http://127.0.0.1:1", hooks=HooksConfig(enabled=True, fail_open=True))
    try:
        assert client.evaluate_hook(_preflight_request()).action == "allow"
    finally:
        client.shutdown()


def _preflight_request() -> HookEvaluateRequest:
    return HookEvaluateRequest(
        phase=HookPhase.PREFLIGHT.value,
        context=HookContext(model=HookModel(provider="openai", name="gpt-4o")),
        input=HookInput(),
    )


def test_evaluate_hook_serializes_tool_definitions_including_deferred() -> None:
    with _capturing_hook_server({"action": "allow", "evaluations": []}) as (captured, base_url):
        client = _new_client(base_url, hooks=HooksConfig(enabled=True, phases=["preflight"]))
        try:
            response = client.evaluate_hook(
                HookEvaluateRequest(
                    phase=HookPhase.PREFLIGHT.value,
                    context=HookContext(model=HookModel(provider="openai", name="gpt-4o")),
                    input=HookInput(
                        tools=[
                            ToolDefinition(name="search", description="search the web", type="function"),
                            ToolDefinition(name="approve_refund", type="function", deferred=True),
                        ],
                    ),
                )
            )
        finally:
            client.shutdown()

    assert response.action == "allow"
    tools = captured["payload"]["input"]["tools"]
    assert [t["name"] for t in tools] == ["search", "approve_refund"]
    assert "deferred" not in tools[0]
    assert tools[1]["deferred"] is True


def test_evaluate_hook_tags_every_part_with_its_kind() -> None:
    """Guards dispatch on ``kind``, and only a text part survives without one.

    A tool call sent without ``kind`` arrives at rule evaluation as an empty
    part, so a tool-filter guard finds no tool calls and allows the request.
    Tool arguments have to be embedded JSON for the same reason: the server
    reads them as raw JSON, so a base64 blob is what an argument-level rule
    like ``shell_exec(*cmd*)`` would match against.
    """

    messages = [
        Message(
            role=MessageRole.ASSISTANT,
            parts=[
                Part(kind=PartKind.THINKING, thinking="weighing options"),
                Part(kind=PartKind.TEXT, text="running it"),
                Part(
                    kind=PartKind.TOOL_CALL,
                    tool_call=ToolCall(id="call_1", name="shell_exec", input_json=b'{"cmd":"ls"}'),
                ),
            ],
        ),
        Message(
            role=MessageRole.TOOL,
            parts=[
                Part(
                    kind=PartKind.TOOL_RESULT,
                    tool_result=ToolResult(tool_call_id="call_1", content="ok", content_json=b'{"status":"ok"}'),
                )
            ],
        ),
    ]

    with _capturing_hook_server({"action": "allow", "evaluations": []}) as (captured, base_url):
        client = _new_client(base_url, hooks=HooksConfig(enabled=True, phases=["preflight"]))
        try:
            client.evaluate_hook(
                HookEvaluateRequest(
                    phase=HookPhase.PREFLIGHT.value,
                    context=HookContext(model=HookModel(provider="openai", name="gpt-4o")),
                    input=HookInput(messages=messages),
                )
            )
        finally:
            client.shutdown()

    wire_messages = captured["payload"]["input"]["messages"]
    assert [[part["kind"] for part in message["parts"]] for message in wire_messages] == [
        ["thinking", "text", "tool_call"],
        ["tool_result"],
    ]
    tool_call = wire_messages[0]["parts"][2]["tool_call"]
    assert tool_call["name"] == "shell_exec"
    assert tool_call["input_json"] == {"cmd": "ls"}
    tool_result = wire_messages[1]["parts"][0]["tool_result"]
    assert tool_result["tool_call_id"] == "call_1"
    assert tool_result["content_json"] == {"status": "ok"}


def test_evaluate_hook_keeps_unparseable_tool_args_as_text() -> None:
    """Bytes that are not JSON still have to leave the SDK as valid JSON."""

    messages = [
        Message(
            role=MessageRole.ASSISTANT,
            parts=[Part(kind=PartKind.TOOL_CALL, tool_call=ToolCall(name="shell_exec", input_json=b"not json"))],
        )
    ]

    with _capturing_hook_server({"action": "allow", "evaluations": []}) as (captured, base_url):
        client = _new_client(base_url, hooks=HooksConfig(enabled=True, phases=["preflight"]))
        try:
            client.evaluate_hook(
                HookEvaluateRequest(
                    phase=HookPhase.PREFLIGHT.value,
                    context=HookContext(model=HookModel(provider="openai", name="gpt-4o")),
                    input=HookInput(messages=messages),
                )
            )
        finally:
            client.shutdown()

    tool_call = captured["payload"]["input"]["messages"][0]["parts"][0]["tool_call"]
    assert tool_call["input_json"] == "not json"


@contextlib.contextmanager
def _capturing_hook_server(response: dict[str, Any]) -> Iterator[tuple[dict[str, Any], str]]:
    """Run a stub hooks:evaluate server that captures one request and returns `response`."""
    captured: dict[str, Any] = {}
    body = json.dumps(response).encode("utf-8")

    class _Handler(BaseHTTPRequestHandler):
        def do_POST(self):  # noqa: N802
            length = int(self.headers.get("Content-Length", "0"))
            captured["path"] = self.path
            captured["headers"] = {k.lower(): v for k, v in self.headers.items()}
            captured["payload"] = json.loads(self.rfile.read(length).decode("utf-8"))
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, _format, *_args):  # noqa: A003
            return

    server = HTTPServer(("127.0.0.1", 0), _Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    try:
        yield captured, f"http://127.0.0.1:{server.server_address[1]}"
    finally:
        server.shutdown()
        server.server_close()


def _new_client(
    api_endpoint: str,
    hooks: HooksConfig | None = None,
    auth: AuthConfig | None = None,
) -> Client:
    if hooks is None:
        hooks = HooksConfig()
    if auth is None:
        auth = AuthConfig()
    return Client(
        ClientConfig(
            generation_export=GenerationExportConfig(
                protocol="http",
                endpoint=f"{api_endpoint}/api/v1/generations:export",
                auth=auth,
                insecure=True,
                batch_size=1,
                flush_interval=timedelta(seconds=1),
                max_retries=1,
                initial_backoff=timedelta(milliseconds=1),
                max_backoff=timedelta(milliseconds=10),
            ),
            api=ApiConfig(endpoint=api_endpoint),
            hooks=hooks,
            tracer=trace.get_tracer("agento11y-sdk-python-hooks-test"),
        )
    )
