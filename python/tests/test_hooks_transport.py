"""Tests for synchronous hook evaluation transport."""

from __future__ import annotations

import contextlib
import json
import threading
from collections.abc import Iterator
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
    MessageRole,
    ToolDefinition,
    hook_denied_from_response,
    user_text_message,
)
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
        assert headers.get("x-sigil-hook-timeout-ms") == "15000"
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
            tracer=trace.get_tracer("sigil-sdk-python-hooks-test"),
        )
    )
