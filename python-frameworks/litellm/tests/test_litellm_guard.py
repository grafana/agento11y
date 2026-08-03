"""LiteLLM guardrail (preflight hook enforcement) tests."""

from __future__ import annotations

import asyncio
import json
import logging
import sys
import threading
import time
import types
from datetime import timedelta
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any

import litellm
import pytest
from agento11y import ApiConfig, Client, ClientConfig, GenerationExportConfig, HooksConfig
from agento11y.context import with_conversation_id
from agento11y.errors import HookTransportError
from agento11y.models import ExportGenerationResult, ExportGenerationsResponse
from agento11y_litellm import (
    DEFAULT_GUARDRAIL_NAME,
    Agento11yLiteLLMGuardrail,
    Agento11yLiteLLMLogger,
    create_agento11y_litellm_guardrail,
)
from litellm.exceptions import GuardrailRaisedException
from litellm.types.guardrails import GuardrailEventHooks
from opentelemetry import trace
from opentelemetry.sdk.trace import TracerProvider


class _CapturingExporter:
    def __init__(self) -> None:
        self.requests: list[Any] = []

    def export_generations(self, request: Any) -> ExportGenerationsResponse:
        self.requests.append(request)
        return ExportGenerationsResponse(
            results=[ExportGenerationResult(generation_id=g.id, accepted=True) for g in request.generations]
        )

    def shutdown(self) -> None:
        pass


class _HookServer:
    """Local hook-evaluation server with programmable responses and delays."""

    def __init__(self) -> None:
        self.requests: list[dict[str, Any]] = []
        self.response: dict[str, Any] = {"action": "allow"}
        self.status = 200
        self.delay = 0.0
        self.in_flight = 0
        self.max_in_flight = 0
        self._lock = threading.Lock()

        server = self

        class _Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.0"

            def do_POST(self):  # noqa: N802
                length = int(self.headers.get("Content-Length", "0"))
                body = self.rfile.read(length)
                with server._lock:
                    server.requests.append(
                        {
                            "path": self.path,
                            "payload": json.loads(body.decode("utf-8")),
                        }
                    )
                    server.in_flight += 1
                    server.max_in_flight = max(server.max_in_flight, server.in_flight)
                try:
                    if server.delay:
                        time.sleep(server.delay)
                    encoded = json.dumps(server.response).encode("utf-8")
                    self.send_response(server.status)
                    self.send_header("Content-Type", "application/json")
                    self.send_header("Content-Length", str(len(encoded)))
                    self.end_headers()
                    self.wfile.write(encoded)
                finally:
                    with server._lock:
                        server.in_flight -= 1

            def log_message(self, _format, *_args):
                return

        self._server = ThreadingHTTPServer(("127.0.0.1", 0), _Handler)
        self._server.daemon_threads = True
        self._thread = threading.Thread(target=self._server.serve_forever, daemon=True)
        self._thread.start()

    @property
    def url(self) -> str:
        return f"http://127.0.0.1:{self._server.server_address[1]}"

    @property
    def payloads(self) -> list[dict[str, Any]]:
        return [entry["payload"] for entry in self.requests]

    def close(self) -> None:
        self._server.shutdown()
        self._server.server_close()


@pytest.fixture
def hook_server():
    server = _HookServer()
    try:
        yield server
    finally:
        server.close()


def _new_client(
    api_endpoint: str,
    *,
    hooks: HooksConfig | None = None,
    exporter: _CapturingExporter | None = None,
) -> Client:
    return Client(
        ClientConfig(
            generation_export=GenerationExportConfig(
                batch_size=10,
                flush_interval=timedelta(seconds=60),
            ),
            generation_exporter=exporter or _CapturingExporter(),
            api=ApiConfig(endpoint=api_endpoint),
            hooks=hooks if hooks is not None else HooksConfig(enabled=True, timeout_seconds=5.0),
        )
    )


class _UserAPIKey:
    """Stands in for ``litellm.proxy._types.UserAPIKeyAuth``.

    The guardrail reads exactly one attribute off it, and importing the real
    proxy type drags in optional proxy dependencies.
    """

    def __init__(self, parent_otel_span: Any = None) -> None:
        self.parent_otel_span = parent_otel_span


def _request_data(**overrides: Any) -> dict[str, Any]:
    data: dict[str, Any] = {
        "model": "gpt-4o",
        "messages": [{"role": "user", "content": "hello"}],
        "metadata": {},
    }
    data.update(overrides)
    return data


def _call(
    guard: Agento11yLiteLLMGuardrail,
    data: dict[str, Any],
    user_api_key: Any = None,
    call_type: str = "completion",
) -> Any:
    return asyncio.run(
        guard.async_pre_call_hook(
            user_api_key_dict=user_api_key or _UserAPIKey(),
            cache=None,
            data=data,
            call_type=call_type,
        )
    )


def test_preflight_allow_returns_none_and_leaves_data_untouched(hook_server):
    hook_server.response = {"action": "allow"}
    client = _new_client(hook_server.url)
    guard = Agento11yLiteLLMGuardrail(client=client, default_on=True)
    data = _request_data()
    before = json.dumps(data, sort_keys=True, default=str)

    assert _call(guard, data) is None

    assert len(hook_server.requests) == 1
    assert hook_server.requests[0]["path"] == "/api/v1/hooks:evaluate"
    assert data["messages"] == json.loads(before)["messages"]
    assert data["model"] == "gpt-4o"
    assert "tools" not in data


def test_preflight_deny_raises_guardrail_exception(hook_server):
    hook_server.response = {"action": "deny", "rule_id": "no-secrets", "reason": "contains a credential"}
    client = _new_client(hook_server.url)
    guard = Agento11yLiteLLMGuardrail(client=client, default_on=True, guardrail_name="agento11y-preflight")

    with pytest.raises(GuardrailRaisedException) as excinfo:
        _call(guard, _request_data())

    assert "contains a credential" in str(excinfo.value)
    assert "no-secrets" in str(excinfo.value)
    assert "agento11y-preflight" in str(excinfo.value)
    assert excinfo.value.guardrail_name == "agento11y-preflight"
    assert excinfo.value.status_code == 400


def test_preflight_request_shape(hook_server):
    client = _new_client(hook_server.url)
    guard = Agento11yLiteLLMGuardrail(
        client=client,
        default_on=True,
        agent_version="1.2.3",
        extra_tags={"team": "search"},
    )
    data = _request_data(
        messages=[
            {"role": "system", "content": "policy"},
            {"role": "user", "content": "hello"},
        ],
        tools=[
            {
                "type": "function",
                "function": {
                    "name": "lookup",
                    "description": "look things up",
                    "parameters": {"type": "object", "properties": {"q": {"type": "string"}}},
                },
            }
        ],
        metadata={"agent_id": "search-agent", "tags": ["prod"], "session_id": "conv-7"},
    )

    assert _call(guard, data) is None

    payload = hook_server.payloads[0]
    assert payload["phase"] == "preflight"
    assert payload["context"]["agent_name"] == "search-agent"
    assert payload["context"]["agent_version"] == "1.2.3"
    assert payload["context"]["conversation_id"] == "conv-7"
    assert payload["context"]["model"] == {"provider": "openai", "name": "gpt-4o"}
    assert payload["context"]["tags"] == {
        "agento11y.framework.name": "litellm",
        "agento11y.framework.source": "guardrail",
        "agento11y.framework.language": "python",
        "litellm.tag.prod": "prod",
        "team": "search",
    }
    assert payload["input"]["system_prompt"] == "policy"
    assert [m["parts"][0]["text"] for m in payload["input"]["messages"]] == ["hello"]
    assert [t["name"] for t in payload["input"]["tools"]] == ["lookup"]
    assert payload["input"]["conversation_preview"] == "hello"
    assert [part["kind"] for m in payload["input"]["messages"] for part in m["parts"]] == ["text"]


@pytest.mark.parametrize(
    ("call_type", "data", "want_texts", "want_system_prompt", "want_preview"),
    [
        pytest.param(
            "atext_completion",
            {"model": "gpt-3.5-turbo-instruct", "prompt": "my password is hunter2", "metadata": {}},
            ["my password is hunter2"],
            "",
            "my password is hunter2",
            id="text_completion_prompt",
        ),
        pytest.param(
            "atext_completion",
            {"model": "gpt-3.5-turbo-instruct", "prompt": ["first", "second"], "metadata": {}},
            ["first", "second"],
            "",
            "second",
            id="text_completion_prompt_list",
        ),
        pytest.param(
            "anthropic_messages",
            {
                "model": "claude-sonnet-4-20250514",
                "system": [{"type": "text", "text": "you are a pirate"}],
                "messages": [{"role": "user", "content": [{"type": "text", "text": "hello"}]}],
                "metadata": {},
            },
            ["hello"],
            "you are a pirate",
            "hello",
            id="anthropic_top_level_system",
        ),
        pytest.param(
            "aresponses",
            {
                "model": "gpt-4o",
                "instructions": "you are a pirate",
                "input": [{"role": "user", "content": [{"type": "input_text", "text": "hello"}]}],
                "metadata": {},
            },
            ["hello"],
            "you are a pirate",
            "hello",
            id="responses_input_and_instructions",
        ),
        pytest.param(
            "aresponses",
            {"model": "gpt-4o", "input": "hello", "metadata": {}},
            ["hello"],
            "",
            "hello",
            id="responses_string_input",
        ),
        pytest.param(
            "aimage_generation",
            {"model": "dall-e-3", "prompt": "a cat wearing a hat", "metadata": {}},
            ["a cat wearing a hat"],
            "",
            "a cat wearing a hat",
            id="image_generation_prompt",
        ),
    ],
)
def test_non_chat_routes_send_their_input(hook_server, call_type, data, want_texts, want_system_prompt, want_preview):
    """Every guarded route reaches the evaluator with its text.

    The guard sees the request body before LiteLLM translates it, so the input
    is under ``prompt`` on text completion and image generation, under ``input``
    on ``/v1/responses``, and the system prompt is top-level on ``/v1/messages``
    (``system``) and ``/v1/responses`` (``instructions``). Reading ``messages``
    alone leaves content and system-prompt rules matching nothing on those
    routes, which allows traffic a deny rule was written to block.
    """
    client = _new_client(hook_server.url)
    guard = Agento11yLiteLLMGuardrail(client=client, default_on=True)

    assert _call(guard, data, call_type=call_type) is None

    payload = hook_server.payloads[0]
    assert [part["text"] for m in payload["input"]["messages"] for part in m["parts"]] == want_texts
    assert payload["input"].get("system_prompt", "") == want_system_prompt
    assert payload["input"].get("conversation_preview", "") == want_preview


def test_deny_blocks_a_route_whose_input_is_not_in_messages(hook_server):
    hook_server.response = {"action": "deny", "rule_id": "no-secrets", "reason": "contains a credential"}
    client = _new_client(hook_server.url)
    guard = Agento11yLiteLLMGuardrail(client=client, default_on=True)
    data = {"model": "gpt-3.5-turbo-instruct", "prompt": "my password is hunter2", "metadata": {}}

    with pytest.raises(GuardrailRaisedException):
        _call(guard, data, call_type="atext_completion")


@pytest.mark.parametrize(
    ("call_type", "data"),
    [
        pytest.param("aembedding", {"model": "text-embedding-3-small", "input": "hello"}, id="embedding"),
        pytest.param("amoderation", {"model": "omni-moderation-latest", "input": "hello"}, id="moderation"),
        pytest.param("arerank", {"model": "rerank-v3", "query": "hello", "documents": ["a"]}, id="rerank"),
        pytest.param("aspeech", {"model": "tts-1", "input": "hello"}, id="speech"),
        pytest.param("call_mcp_tool", {"name": "lookup", "arguments": {}}, id="mcp_tool_call"),
        pytest.param("pass_through_endpoint", {"contents": [{"parts": [{"text": "hello"}]}]}, id="pass_through"),
    ],
)
def test_routes_without_mappable_input_record_no_verdict(hook_server, call_type, data):
    """An unmapped route is skipped rather than evaluated against empty input.

    ``ProxyLogging.pre_call_hook`` runs this guardrail on every route it covers.
    Evaluating one whose body this adapter cannot read would return allow and
    record a verdict that reads like a completed check.
    """
    client = _new_client(hook_server.url)
    guard = Agento11yLiteLLMGuardrail(client=client, default_on=True)
    data = {**data, "metadata": {}}

    assert _call(guard, data, call_type=call_type) is None
    assert hook_server.requests == []
    assert "standard_logging_guardrail_information" not in data["metadata"]


@pytest.mark.parametrize(
    ("call_type", "body"),
    [
        pytest.param("acompletion", {"messages": []}, id="no_messages"),
        pytest.param("atext_completion", {"prompt": [[1233, 8765]]}, id="token_id_prompt"),
        pytest.param(
            "acompletion",
            {"messages": [{"role": "user", "content": [{"type": "image_url", "image_url": {"url": "data:..."}}]}]},
            id="image_only_content",
        ),
    ],
)
def test_request_without_evaluable_text_records_no_verdict(hook_server, call_type, body):
    client = _new_client(hook_server.url)
    guard = Agento11yLiteLLMGuardrail(client=client, default_on=True)
    data = {"model": "gpt-4o", "metadata": {}, **body}

    assert _call(guard, data, call_type=call_type) is None
    assert hook_server.requests == []
    assert "standard_logging_guardrail_information" not in data["metadata"]


@pytest.mark.parametrize(
    ("call_type", "body"),
    [
        pytest.param(
            "acompletion",
            {
                "messages": [
                    {"role": "user", "content": "list the temp dir"},
                    {
                        "role": "assistant",
                        "tool_calls": [
                            {
                                "id": "call_1",
                                "type": "function",
                                "function": {"name": "shell_exec", "arguments": '{"cmd":"ls /tmp"}'},
                            }
                        ],
                    },
                    {"role": "tool", "tool_call_id": "call_1", "content": "ok"},
                ]
            },
            id="chat_tool_calls",
        ),
        pytest.param(
            "aresponses",
            {
                "input": [
                    {"role": "user", "content": [{"type": "input_text", "text": "list the temp dir"}]},
                    {
                        "type": "function_call",
                        "id": "fc_1",
                        "call_id": "call_1",
                        "name": "shell_exec",
                        "arguments": '{"cmd":"ls /tmp"}',
                    },
                    {"type": "function_call_output", "call_id": "call_1", "output": "ok"},
                ]
            },
            id="responses_function_call_items",
        ),
    ],
)
def test_tool_calls_in_history_keep_their_kind_on_the_wire(hook_server, call_type, body):
    """A tool-filter guard only sees tool calls that arrive as ``tool_call`` parts.

    The server dispatches on ``kind`` and recovers a missing one for text only,
    so a mapped tool call without it reaches rule evaluation as an empty part
    and every blocked_names pattern silently matches nothing.

    ``/v1/responses`` keeps the same history in top-level ``function_call`` and
    ``function_call_output`` items instead of role messages, so a mapper that
    reads only chat shapes sends the turn with no tool history at all and rules
    written against it match nothing.
    """
    client = _new_client(hook_server.url)
    guard = Agento11yLiteLLMGuardrail(client=client, default_on=True)
    data = {"model": "gpt-4o", "metadata": {}, **body}

    assert _call(guard, data, call_type=call_type) is None

    messages = hook_server.payloads[0]["input"]["messages"]
    assert [[part["kind"] for part in m["parts"]] for m in messages] == [["text"], ["tool_call"], ["tool_result"]]
    assert [m["role"] for m in messages] == ["user", "assistant", "tool"]
    tool_call = messages[1]["parts"][0]["tool_call"]
    assert tool_call["name"] == "shell_exec"
    assert tool_call["id"] == "call_1"
    # Argument-level patterns such as shell_exec(*ls*) match against this, so it
    # has to be the arguments themselves and not an encoded blob.
    assert tool_call["input_json"] == {"cmd": "ls /tmp"}
    assert messages[2]["parts"][0]["tool_result"]["tool_call_id"] == "call_1"
    assert messages[2]["parts"][0]["tool_result"]["content"] == "ok"


@pytest.mark.parametrize(
    ("data_overrides", "want_provider", "want_name"),
    [
        pytest.param({"model": "gpt-4o"}, "openai", "gpt-4o", id="bare_name_in_model_map"),
        pytest.param({"model": "openai/gpt-4o-mini"}, "openai", "openai/gpt-4o-mini", id="prefixed_model"),
        pytest.param({"model": "claude-sonnet-4-5"}, "anthropic", "claude-sonnet-4-5", id="other_provider"),
        pytest.param({"model": "prod-fast-alias"}, "unknown", "prod-fast-alias", id="deployment_alias"),
        pytest.param({"model": ""}, "unknown", "unknown", id="missing_model"),
        pytest.param(
            {"model": "prod-fast-alias", "custom_llm_provider": "Bedrock"},
            "bedrock",
            "prod-fast-alias",
            id="explicit_provider_wins",
        ),
    ],
)
def test_model_identity_is_never_empty(hook_server, data_overrides, want_provider, want_name):
    """The hooks API rejects an empty provider or name, and a rejected evaluation fails open.

    Sending either one empty means every rule is skipped and nothing is ever
    denied, so the placeholder matters as much as a correct resolution.
    """
    client = _new_client(hook_server.url)
    guard = Agento11yLiteLLMGuardrail(client=client, default_on=True)

    assert _call(guard, _request_data(**data_overrides)) is None

    assert hook_server.payloads[0]["context"]["model"] == {"provider": want_provider, "name": want_name}


def test_evaluation_preserves_context_variables(hook_server):
    client = _new_client(hook_server.url)
    guard = Agento11yLiteLLMGuardrail(client=client, default_on=True)

    with with_conversation_id("context-conversation"):
        assert _call(guard, _request_data()) is None

    assert hook_server.payloads[0]["context"]["conversation_id"] == "context-conversation"


def test_trace_ids_come_from_parent_otel_span_not_ambient(hook_server):
    client = _new_client(hook_server.url)
    guard = Agento11yLiteLLMGuardrail(client=client, default_on=True)
    tracer = TracerProvider().get_tracer("agento11y-litellm-guard-test")
    request_span = tracer.start_span("proxy-request")
    request_ctx = request_span.get_span_context()

    with tracer.start_as_current_span("auth-phase") as ambient:
        ambient_ctx = ambient.get_span_context()
        assert trace.get_current_span().get_span_context().span_id == ambient_ctx.span_id
        _call(guard, _request_data(), _UserAPIKey(parent_otel_span=request_span))

    context = hook_server.payloads[0]["context"]
    assert context["trace_id"] == format(request_ctx.trace_id, "032x")
    assert context["span_id"] == format(request_ctx.span_id, "016x")
    assert context["span_id"] != format(ambient_ctx.span_id, "016x")


def test_transformed_input_is_ignored(hook_server):
    hook_server.response = {
        "action": "allow",
        "transformed_input": {
            "system_prompt": "rewritten policy",
            "messages": [{"role": "user", "parts": [{"text": "redacted"}]}],
        },
    }
    client = _new_client(hook_server.url)
    guard = Agento11yLiteLLMGuardrail(client=client, default_on=True)
    data = _request_data()

    assert _call(guard, data) is None
    assert data["messages"] == [{"role": "user", "content": "hello"}]


@pytest.mark.parametrize("mode", ["post_call", "during_call", "logging_only"])
def test_unsupported_modes_rejected_at_construction(mode):
    client = _new_client("http://127.0.0.1:1")
    with pytest.raises(ValueError, match=mode):
        Agento11yLiteLLMGuardrail(client=client, event_hook=mode)


@pytest.mark.parametrize(
    "event_hook",
    ["pre_call", GuardrailEventHooks.pre_call, ["pre_call"]],
    ids=["string", "enum", "list"],
)
def test_pre_call_mode_is_accepted_in_every_shape(hook_server, event_hook):
    client = _new_client(hook_server.url)
    guard = Agento11yLiteLLMGuardrail(client=client, event_hook=event_hook, default_on=True)

    assert guard.event_hook == event_hook
    assert _call(guard, _request_data()) is None
    assert len(hook_server.requests) == 1


def test_unsupported_mode_in_list_rejected_at_construction():
    client = _new_client("http://127.0.0.1:1")
    with pytest.raises(ValueError, match="post_call"):
        Agento11yLiteLLMGuardrail(client=client, event_hook=["pre_call", "post_call"])


@pytest.mark.parametrize(
    ("kwargs", "match"),
    [
        ({"max_concurrent_evaluations": 0}, "max_concurrent_evaluations"),
        ({"request_timeout_seconds": 0}, "request_timeout_seconds"),
        ({"request_timeout_seconds": -1.0}, "request_timeout_seconds"),
    ],
)
def test_invalid_limits_rejected_at_construction(kwargs, match):
    client = _new_client("http://127.0.0.1:1")
    with pytest.raises(ValueError, match=match):
        Agento11yLiteLLMGuardrail(client=client, **kwargs)


@pytest.mark.parametrize(
    "hooks",
    [
        HooksConfig(enabled=False),
        HooksConfig(enabled=True, phases=["postflight"]),
    ],
    ids=["hooks-disabled", "preflight-phase-not-configured"],
)
def test_client_without_preflight_records_no_verdict(hook_server, hooks):
    """A client that does not enable preflight makes the guardrail a no-op.

    Reaching the SDK would return a synthetic allow, which LiteLLM records as a
    completed check: a proxy whose hooks are switched off would report a green
    guard verdict on every request.
    """
    client = _new_client(hook_server.url, hooks=hooks)
    guard = Agento11yLiteLLMGuardrail(client=client, default_on=True)
    data = _request_data()

    assert _call(guard, data) is None
    assert hook_server.requests == []
    assert "standard_logging_guardrail_information" not in data["metadata"]


@pytest.mark.parametrize(
    "hooks",
    [
        HooksConfig(enabled=False),
        HooksConfig(enabled=True, phases=["postflight"]),
    ],
    ids=["hooks-disabled", "preflight-phase-not-configured"],
)
def test_client_without_preflight_warns_at_construction(hook_server, caplog, hooks):
    client = _new_client(hook_server.url, hooks=hooks)

    with caplog.at_level(logging.WARNING):
        Agento11yLiteLLMGuardrail(client=client, default_on=True, guardrail_name="agento11y-preflight")

    warnings = [record for record in caplog.records if record.levelno == logging.WARNING]
    assert len(warnings) == 1
    assert "agento11y-preflight" in warnings[0].getMessage()
    assert "no request will be evaluated" in warnings[0].getMessage()


def test_guardrail_not_enabled_for_request_is_skipped(hook_server):
    client = _new_client(hook_server.url)
    guard = Agento11yLiteLLMGuardrail(client=client, default_on=False)
    data = _request_data()

    assert guard.should_run_guardrail(data, GuardrailEventHooks.pre_call) is False
    assert _call(guard, data) is None
    assert hook_server.requests == []
    assert "standard_logging_guardrail_information" not in data["metadata"]


def test_constructor_defaults_are_enough_for_a_plain_callbacks_entry(hook_server):
    """Registration is a ``litellm.callbacks`` entry, with no ``guardrails:`` block.

    ``ProxyLogging.pre_call_hook`` walks ``litellm.callbacks`` and runs every
    ``CustomGuardrail`` whose ``should_run_guardrail`` returns True, so the mode
    and the per-request gate both have to come from the constructor.
    """
    client = _new_client(hook_server.url)
    guard = Agento11yLiteLLMGuardrail(client=client, default_on=True)

    assert guard.event_hook == GuardrailEventHooks.pre_call
    assert guard.should_run_guardrail(_request_data(), GuardrailEventHooks.pre_call) is True


def test_guardrail_requested_per_request_runs(hook_server):
    client = _new_client(hook_server.url)
    guard = Agento11yLiteLLMGuardrail(client=client, default_on=False)
    data = _request_data(metadata={"guardrails": [DEFAULT_GUARDRAIL_NAME]})

    assert _call(guard, data) is None
    assert len(hook_server.requests) == 1


def test_transport_failure_fail_open_allows_and_warns(hook_server, caplog):
    hook_server.close()  # nothing is listening on that port any more
    client = _new_client(hook_server.url, hooks=HooksConfig(enabled=True, timeout_seconds=1.0, fail_open=True))
    guard = Agento11yLiteLLMGuardrail(client=client, default_on=True, request_timeout_seconds=5.0)
    data = _request_data()

    with caplog.at_level(logging.WARNING):
        assert _call(guard, data) is None

    warnings = [record for record in caplog.records if "allowing request (fail_open)" in record.getMessage()]
    assert len(warnings) == 1
    entry = _only_guardrail_entry(data)
    assert entry["guardrail_status"] == "guardrail_failed_to_respond"
    assert entry["duration"] > 0


def test_transport_failure_fail_closed_raises(hook_server):
    hook_server.close()
    client = _new_client(hook_server.url, hooks=HooksConfig(enabled=True, timeout_seconds=1.0, fail_open=False))
    guard = Agento11yLiteLLMGuardrail(client=client, default_on=True, request_timeout_seconds=5.0)
    data = _request_data()

    with pytest.raises(HookTransportError):
        _call(guard, data)

    entry = _only_guardrail_entry(data)
    assert entry["guardrail_status"] == "guardrail_failed_to_respond"
    assert entry["duration"] > 0


def test_unexpected_evaluation_error_is_recorded_as_failed_to_respond(hook_server, monkeypatch):
    """An adapter bug must not be filed as a completed guard check.

    Fail-open still allows the request, but the recorded verdict has to say the
    evaluation never produced one.
    """
    client = _new_client(hook_server.url, hooks=HooksConfig(enabled=True, timeout_seconds=1.0, fail_open=True))
    guard = Agento11yLiteLLMGuardrail(client=client, default_on=True, request_timeout_seconds=5.0)

    def _boom(*_args, **_kwargs):
        raise RuntimeError("boom")

    monkeypatch.setattr(client, "evaluate_hook", _boom)
    data = _request_data()

    assert _call(guard, data) is None

    entry = _only_guardrail_entry(data)
    assert entry["guardrail_status"] == "guardrail_failed_to_respond"
    assert "RuntimeError" in str(entry["guardrail_response"])
    assert "boom" in str(entry["guardrail_response"])


def test_slow_server_honors_request_timeout_and_fails_open(hook_server, caplog):
    hook_server.delay = 1.0
    client = _new_client(hook_server.url, hooks=HooksConfig(enabled=True, timeout_seconds=5.0, fail_open=True))
    guard = Agento11yLiteLLMGuardrail(client=client, default_on=True, request_timeout_seconds=0.1)

    data = _request_data()

    async def _run() -> tuple[Any, float]:
        started = time.monotonic()
        result = await _evaluate(guard, data)
        return result, time.monotonic() - started

    with caplog.at_level(logging.WARNING):
        # Timed inside the loop: asyncio.run() joins the default executor on the
        # way out, so it would also wait for the abandoned worker thread.
        result, elapsed = asyncio.run(_run())

    assert result is None
    assert elapsed < 0.9
    warnings = [record for record in caplog.records if "allowing request (fail_open)" in record.getMessage()]
    assert len(warnings) == 1
    assert "timed out after 0.1s" in warnings[0].getMessage()
    entry = _only_guardrail_entry(data)
    assert entry["guardrail_status"] == "guardrail_failed_to_respond"
    assert entry["duration"] >= 0.1


def test_adapter_timeout_respects_fail_closed(hook_server):
    hook_server.delay = 1.0
    client = _new_client(hook_server.url, hooks=HooksConfig(enabled=True, timeout_seconds=5.0, fail_open=False))
    guard = Agento11yLiteLLMGuardrail(client=client, default_on=True, request_timeout_seconds=0.1)
    data = _request_data()

    with pytest.raises(HookTransportError, match="timed out"):
        _call(guard, data)

    entry = _only_guardrail_entry(data)
    assert entry["guardrail_status"] == "guardrail_failed_to_respond"
    assert entry["duration"] >= 0.1


def test_event_loop_stays_responsive_during_evaluation(hook_server):
    hook_server.delay = 0.5
    client = _new_client(hook_server.url)
    guard = Agento11yLiteLLMGuardrail(client=client, default_on=True, request_timeout_seconds=5.0)
    order: list[str] = []

    async def _heartbeat() -> None:
        for _ in range(5):
            await asyncio.sleep(0.01)
        order.append("heartbeat")

    async def _run() -> None:
        beat = asyncio.create_task(_heartbeat())
        await guard.async_pre_call_hook(
            user_api_key_dict=_UserAPIKey(),
            cache=None,
            data=_request_data(),
            call_type="completion",
        )
        order.append("hook")
        await beat

    asyncio.run(_run())

    assert order == ["heartbeat", "hook"]
    assert len(hook_server.requests) == 1


def test_concurrency_ceiling_is_respected(hook_server):
    hook_server.delay = 0.2
    client = _new_client(hook_server.url)
    guard = Agento11yLiteLLMGuardrail(
        client=client,
        default_on=True,
        max_concurrent_evaluations=2,
        request_timeout_seconds=10.0,
    )

    async def _run() -> None:
        await asyncio.gather(*(_evaluate(guard, _request_data()) for _ in range(3)))

    asyncio.run(_run())

    assert len(hook_server.requests) == 3
    assert hook_server.max_in_flight <= 2


def test_timed_out_waits_do_not_release_the_slot_early(hook_server):
    """A worker still blocked in urllib keeps holding its concurrency slot.

    Releasing on coroutine timeout instead would let a third evaluation start
    while both workers are still in flight, putting three threads on the wire
    with ``max_concurrent_evaluations=2``.
    """
    hook_server.delay = 0.6
    client = _new_client(hook_server.url, hooks=HooksConfig(enabled=True, timeout_seconds=5.0, fail_open=True))
    guard = Agento11yLiteLLMGuardrail(
        client=client,
        default_on=True,
        max_concurrent_evaluations=2,
        request_timeout_seconds=0.25,
    )

    async def _late() -> None:
        await asyncio.sleep(0.1)
        await _evaluate(guard, _request_data())

    async def _run() -> None:
        await asyncio.gather(
            _evaluate(guard, _request_data()),
            _evaluate(guard, _request_data()),
            _late(),
        )
        # Let the detached workers finish before the server is torn down.
        await asyncio.sleep(0.6)

    asyncio.run(_run())

    assert len(hook_server.requests) == 2
    assert hook_server.max_in_flight == 2


def test_guardrail_information_is_recorded_in_request_metadata(hook_server):
    client = _new_client(hook_server.url)
    guard = Agento11yLiteLLMGuardrail(client=client, default_on=True)
    data = _request_data()

    _call(guard, data)

    entries = data["metadata"]["standard_logging_guardrail_information"]
    assert len(entries) == 1
    assert entries[0]["guardrail_name"] == DEFAULT_GUARDRAIL_NAME
    assert entries[0]["guardrail_status"] == "success"


def test_guardrail_deny_is_recorded_in_request_metadata(hook_server):
    hook_server.response = {"action": "deny", "rule_id": "no-secrets", "reason": "contains a credential"}
    client = _new_client(hook_server.url)
    guard = Agento11yLiteLLMGuardrail(client=client, default_on=True)
    data = _request_data()

    with pytest.raises(GuardrailRaisedException):
        _call(guard, data)

    entries = data["metadata"]["standard_logging_guardrail_information"]
    assert len(entries) == 1
    assert entries[0]["guardrail_status"] == "guardrail_intervened"


def test_guardrail_span_is_emitted_for_the_request(hook_server, monkeypatch):
    """LiteLLM's decorator hands each execution to its OTel guardrail emitter.

    The span itself is built by LiteLLM and parented to the proxy request span
    (``resolve_request_span_context``), which only exists inside a running
    proxy; what this asserts is that our decorated hook reaches that emitter
    with an entry naming this guardrail.
    """
    emitted: list[Any] = []
    stub = types.ModuleType("litellm.integrations.otel.logger")
    stub.emit_guardrail_span = emitted.append
    # Stubbed as a module because importing the real one pulls in optional
    # proxy dependencies that the SDK test environment does not have.
    monkeypatch.setitem(sys.modules, "litellm.integrations.otel.logger", stub)
    client = _new_client(hook_server.url)
    guard = Agento11yLiteLLMGuardrail(client=client, default_on=True)

    _call(guard, _request_data())

    assert [entry["guardrail_name"] for entry in emitted] == [DEFAULT_GUARDRAIL_NAME]
    assert emitted[0]["guardrail_mode"] == GuardrailEventHooks.pre_call


def test_guardrail_and_logger_register_without_double_export(hook_server):
    """Both objects sit in ``litellm.callbacks``; only the logger may export."""
    exporter = _CapturingExporter()
    client = _new_client(hook_server.url, exporter=exporter)
    logger_callback = Agento11yLiteLLMLogger(client=client)
    guard = Agento11yLiteLLMGuardrail(client=client, default_on=True)

    original = list(litellm.callbacks)
    try:
        litellm.logging_callback_manager.add_litellm_callback(logger_callback)
        litellm.logging_callback_manager.add_litellm_callback(guard)
        registered = [cb for cb in litellm.callbacks if cb in (logger_callback, guard)]
        assert registered == [logger_callback, guard]

        kwargs, response_obj, start, end = _success_event()

        async def _run() -> None:
            for callback in registered:
                await callback.async_log_success_event(kwargs, response_obj, start, end)

        asyncio.run(_run())
    finally:
        litellm.callbacks = original

    client.shutdown()
    generations = [g for request in exporter.requests for g in request.generations]
    assert len(generations) == 1


def test_public_factory_builds_a_configured_guardrail():
    client = _new_client("http://127.0.0.1:1")
    guard = create_agento11y_litellm_guardrail(
        client=client,
        agent_name="static-agent",
        guardrail_name="agento11y-preflight",
        default_on=True,
        max_concurrent_evaluations=4,
        request_timeout_seconds=0.5,
    )

    assert isinstance(guard, Agento11yLiteLLMGuardrail)
    assert guard.guardrail_name == "agento11y-preflight"
    assert guard.default_on is True
    assert guard.event_hook is GuardrailEventHooks.pre_call


def _only_guardrail_entry(data: dict[str, Any]) -> dict[str, Any]:
    """Return the single guardrail entry LiteLLM recorded for the request."""
    entries = data["metadata"]["standard_logging_guardrail_information"]
    assert len(entries) == 1
    return entries[0]


async def _evaluate(guard: Agento11yLiteLLMGuardrail, data: dict[str, Any]) -> Any:
    return await guard.async_pre_call_hook(
        user_api_key_dict=_UserAPIKey(),
        cache=None,
        data=data,
        call_type="completion",
    )


def _success_event() -> tuple[dict[str, Any], Any, Any, Any]:
    from datetime import datetime, timezone

    start = datetime(2026, 1, 1, tzinfo=timezone.utc)
    end = datetime(2026, 1, 1, 0, 0, 1, tzinfo=timezone.utc)
    slo = {
        "call_type": "acompletion",
        "id": "gen-1",
        "model": "gpt-4o",
        "custom_llm_provider": "openai",
        "messages": [{"role": "user", "content": "hello"}],
        "response": {"choices": [{"message": {"role": "assistant", "content": "hi"}, "finish_reason": "stop"}]},
        "prompt_tokens": 1,
        "completion_tokens": 1,
        "total_tokens": 2,
    }
    kwargs = {"standard_logging_object": slo, "litellm_params": {"metadata": {}}}
    return kwargs, None, start, end
