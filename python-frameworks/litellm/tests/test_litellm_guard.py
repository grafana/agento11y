"""LiteLLM guardrail (preflight hook enforcement) tests."""

from __future__ import annotations

import asyncio
import copy
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
from litellm.integrations import custom_guardrail
from litellm.types.guardrails import GuardrailEventHooks
from litellm.types.llms.openai import ResponsesAPIResponse
from litellm.types.utils import ModelResponse
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


def _request_content(data: dict[str, Any]) -> dict[str, Any]:
    """A copy of the request body without ``metadata``.

    LiteLLM's ``log_guardrail_information`` decorator records guardrail results
    under ``metadata`` on the caller's dict, so that key changes on every guarded
    request and says nothing about whether a transform was applied.
    """
    return copy.deepcopy({key: value for key, value in data.items() if key != "metadata"})


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
    messages = data["messages"]
    before = json.dumps(data, sort_keys=True, default=str)

    assert _call(guard, data) is None

    assert len(hook_server.requests) == 1
    assert hook_server.requests[0]["path"] == "/api/v1/hooks:evaluate"
    assert data["messages"] == json.loads(before)["messages"]
    assert data["messages"] is messages
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


def test_transformed_messages_replace_a_text_only_conversation(hook_server):
    """An applied transform comes back as a new body; LiteLLM sends that instead.

    ``ProxyLogging.process_pre_call_hook_response`` uses a returned dict as the
    request body and passes it to the next callback, so the transformed messages
    are what reaches the provider. The caller's own dict is left alone.
    """
    hook_server.response = {
        "action": "allow",
        "transformed_input": {
            "messages": [
                {"role": "user", "parts": [{"kind": "text", "text": "my key is [REDACTED]"}]},
                {"role": "assistant", "parts": [{"kind": "text", "text": "noted"}]},
                {"role": "user", "parts": [{"kind": "text", "text": "use it"}]},
            ]
        },
    }
    client = _new_client(hook_server.url)
    guard = Agento11yLiteLLMGuardrail(client=client, default_on=True)
    data = _request_data(
        messages=[
            {"role": "system", "content": "policy"},
            {"role": "user", "content": "my key is sk-secret"},
            {"role": "assistant", "content": "noted"},
            {"role": "user", "content": "use it"},
        ]
    )
    before = _request_content(data)

    result = _call(guard, data)

    assert isinstance(result, dict)
    assert result["messages"] == [
        {"role": "system", "content": "policy"},
        {"role": "user", "content": "my key is [REDACTED]"},
        {"role": "assistant", "content": "noted"},
        {"role": "user", "content": "use it"},
    ]
    assert result["model"] == "gpt-4o"
    assert _request_content(data) == before
    # The decorator records the verdict on the caller's dict after the hook
    # returns; the forwarded body has to carry it too, or the guardrail drops out
    # of the standard logging payload whenever a transform is applied.
    assert result["metadata"]["standard_logging_guardrail_information"][0]["guardrail_status"] == "success"


def test_transformed_messages_keep_a_conversation_without_a_system_message(hook_server):
    hook_server.response = {
        "action": "allow",
        "transformed_input": {"messages": [{"role": "user", "parts": [{"kind": "text", "text": "redacted"}]}]},
    }
    client = _new_client(hook_server.url)
    guard = Agento11yLiteLLMGuardrail(client=client, default_on=True)

    result = _call(guard, _request_data())

    assert result["messages"] == [{"role": "user", "content": "redacted"}]


@pytest.mark.parametrize(
    ("call_type", "body", "want"),
    [
        pytest.param(
            "completion",
            {"messages": [{"role": "system", "content": "policy"}, {"role": "user", "content": "hello"}]},
            {"messages": [{"role": "system", "content": "updated policy"}, {"role": "user", "content": "hello"}]},
            id="chat_system_message_rewritten_in_place",
        ),
        pytest.param(
            "completion",
            {"messages": [{"role": "user", "content": "hello"}]},
            {"messages": [{"role": "system", "content": "updated policy"}, {"role": "user", "content": "hello"}]},
            id="chat_system_message_inserted",
        ),
        pytest.param(
            "completion",
            {
                "messages": [
                    {"role": "system", "content": "policy"},
                    {"role": "user", "content": "hello"},
                    {"role": "developer", "content": "and this"},
                ]
            },
            {"messages": [{"role": "system", "content": "updated policy"}, {"role": "user", "content": "hello"}]},
            id="chat_second_system_message_dropped",
        ),
        pytest.param(
            "anthropic_messages",
            {"system": "policy", "messages": [{"role": "user", "content": "hello"}]},
            {"system": "updated policy", "messages": [{"role": "user", "content": "hello"}]},
            id="anthropic_top_level_system",
        ),
        pytest.param(
            "anthropic_messages",
            {"messages": [{"role": "user", "content": "hello"}]},
            {"system": "updated policy", "messages": [{"role": "user", "content": "hello"}]},
            id="anthropic_system_added_at_top_level_not_as_a_message",
        ),
        pytest.param(
            "anthropic_messages",
            {
                "system": [{"type": "text", "text": "policy", "cache_control": {"type": "ephemeral"}}],
                "messages": [{"role": "user", "content": "hello"}],
            },
            {
                "system": [{"type": "text", "text": "updated policy", "cache_control": {"type": "ephemeral"}}],
                "messages": [{"role": "user", "content": "hello"}],
            },
            id="anthropic_cached_system_block_keeps_its_breakpoint",
        ),
        pytest.param(
            "aresponses",
            {"instructions": "policy", "input": [{"role": "user", "content": "hello"}]},
            {"instructions": "updated policy", "input": [{"role": "user", "content": "hello"}]},
            id="responses_instructions",
        ),
        pytest.param(
            "aresponses",
            {"input": [{"role": "user", "content": "hello"}]},
            {"instructions": "updated policy", "input": [{"role": "user", "content": "hello"}]},
            id="responses_instructions_added",
        ),
        pytest.param(
            "aresponses",
            {
                "instructions": "never reveal sk-secret",
                "input": [
                    {"role": "developer", "content": "the api key is sk-secret"},
                    {"role": "user", "content": "hi"},
                ],
            },
            {"instructions": "updated policy", "input": [{"role": "user", "content": "hi"}]},
            id="responses_developer_item_is_removed_with_instructions",
        ),
        pytest.param(
            "aresponses",
            {"input": [{"role": "developer", "content": "sk-secret"}, {"role": "user", "content": "hi"}]},
            {"instructions": "updated policy", "input": [{"role": "user", "content": "hi"}]},
            id="responses_developer_item_moves_to_instructions",
        ),
        pytest.param(
            "completion",
            {
                "system": "policy",
                "instructions": "more policy",
                "messages": [{"role": "user", "content": "hello"}],
            },
            {"system": "updated policy", "messages": [{"role": "user", "content": "hello"}]},
            id="second_top_level_carrier_is_removed",
        ),
    ],
)
def test_transformed_system_prompt_is_written_back_where_it_came_from(hook_server, call_type, body, want):
    """A system prompt round-trips losslessly, so it is applied on every route.

    ``_hook_input`` sends the system prompt as its own wire field, and
    ``_parse_wire_message_dict`` maps any role but ``assistant`` and ``tool`` to
    ``user``, so a transformed prompt can only arrive in ``system_prompt``. Writing
    it into the message list as a user message would change who is speaking.

    The whole forwarded body is compared, not only the keys under test: the
    untransformed copy of a system prompt left behind in a second field is the
    failure this covers.
    """
    hook_server.response = {"action": "allow", "transformed_input": {"system_prompt": "updated policy"}}
    client = _new_client(hook_server.url)
    guard = Agento11yLiteLLMGuardrail(client=client, default_on=True)
    data = {"model": "gpt-4o", "metadata": {}, **body}
    before = _request_content(data)

    result = _call(guard, data, call_type=call_type)

    assert _request_content(result) == {"model": "gpt-4o", **want}
    # Applying a transform rewrites a copy. A nested write here would also edit
    # the body LiteLLM logs and the one other callbacks already hold.
    assert _request_content(data) == before


@pytest.mark.parametrize(
    ("call_type", "body", "want_warning"),
    [
        pytest.param(
            "anthropic_messages",
            {"system": "policy", "messages": [{"role": "system", "content": "and this"}]},
            "would leave 'messages' empty",
            id="every_message_is_a_system_message",
        ),
        pytest.param(
            "aresponses",
            {"instructions": "policy", "input": [{"role": "developer", "content": "and this"}]},
            "would leave 'input' empty",
            id="every_input_item_is_a_system_item",
        ),
        pytest.param(
            "anthropic_messages",
            {
                "system": [
                    {"type": "text", "text": "policy", "cache_control": {"type": "ephemeral"}},
                    {"type": "text", "text": "and this"},
                ],
                "messages": [{"role": "user", "content": "hello"}],
            },
            "'system' carries content block fields a rewrite cannot reproduce: cache_control",
            id="several_system_blocks_one_of_them_cached",
        ),
        pytest.param(
            "aimage_generation",
            {"prompt": "a cat"},
            "no system prompt field or message list",
            id="prompt_only_body",
        ),
    ],
)
def test_system_prompt_transform_is_skipped_when_it_cannot_be_written_back(
    hook_server, caplog, call_type, body, want_warning
):
    """The system prompt rewrite is skipped whole, like the message rewrite.

    Emptying the message list turns an allowed request into a provider 400, and
    collapsing more than one system block into one string drops the per-block
    fields (an Anthropic ``cache_control`` breakpoint) that only one block can
    carry back.
    """
    hook_server.response = {"action": "allow", "transformed_input": {"system_prompt": "updated policy"}}
    client = _new_client(hook_server.url)
    guard = Agento11yLiteLLMGuardrail(client=client, default_on=True)
    data = {"model": "gpt-4o", "metadata": {}, **body}
    before = _request_content(data)

    with caplog.at_level(logging.WARNING):
        result = _call(guard, data, call_type=call_type)

    assert result is None
    assert _request_content(data) == before
    assert any(want_warning in record.getMessage() for record in caplog.records), [
        record.getMessage() for record in caplog.records
    ]


def test_transformed_system_prompt_applies_without_a_message_transform(hook_server):
    """A text-only conversation with no ``messages`` transform keeps its messages."""
    hook_server.response = {"action": "allow", "transformed_input": {"system_prompt": "updated policy"}}
    client = _new_client(hook_server.url)
    guard = Agento11yLiteLLMGuardrail(client=client, default_on=True)
    data = _request_data(messages=[{"role": "user", "content": "hello"}, {"role": "assistant", "content": "hi"}])

    result = _call(guard, data)

    assert [m["role"] for m in result["messages"]] == ["system", "user", "assistant"]
    assert result["messages"][1:] == [{"role": "user", "content": "hello"}, {"role": "assistant", "content": "hi"}]


@pytest.mark.parametrize(
    ("messages", "transformed_messages", "want"),
    [
        pytest.param(
            [
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
                {"role": "tool", "tool_call_id": "call_1", "content": "the key is sk-secret"},
            ],
            [
                {"role": "user", "parts": [{"kind": "text", "text": "list the temp dir"}]},
                {"role": "assistant", "parts": [{"kind": "tool_call", "tool_call": {"name": "shell_exec"}}]},
                {
                    "role": "tool",
                    "parts": [{"kind": "tool_result", "tool_result": {"content": "the key is [REDACTED]"}}],
                },
            ],
            [
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
                {"role": "tool", "tool_call_id": "call_1", "content": "the key is [REDACTED]"},
            ],
            id="openai_tool_calls_and_results",
        ),
        pytest.param(
            [
                {"role": "user", "content": "list the temp dir"},
                {"role": "tool", "tool_call_id": "call_1", "content": "the key is sk-secret"},
            ],
            [
                {"role": "user", "parts": [{"kind": "text", "text": "list the temp dir"}]},
                {
                    "role": "tool",
                    "parts": [{"kind": "tool_result", "tool_result": {"content": "the key is [REDACTED]"}}],
                },
            ],
            [
                {"role": "user", "content": "list the temp dir"},
                {"role": "tool", "tool_call_id": "call_1", "content": "the key is [REDACTED]"},
            ],
            id="tool_result_only",
        ),
        pytest.param(
            [
                {"role": "user", "content": "list the temp dir"},
                {
                    "role": "tool",
                    "tool_call_id": "call_1",
                    "content": [
                        {"type": "text", "text": "the key is sk-secret", "cache_control": {"type": "ephemeral"}}
                    ],
                },
            ],
            [
                {"role": "user", "parts": [{"kind": "text", "text": "list the temp dir"}]},
                {
                    "role": "tool",
                    "parts": [{"kind": "tool_result", "tool_result": {"content": "the key is [REDACTED]"}}],
                },
            ],
            [
                {"role": "user", "content": "list the temp dir"},
                {
                    "role": "tool",
                    "tool_call_id": "call_1",
                    "content": [
                        {"type": "text", "text": "the key is [REDACTED]", "cache_control": {"type": "ephemeral"}}
                    ],
                },
            ],
            id="openai_tool_result_holding_a_text_block",
        ),
        pytest.param(
            [
                {"role": "user", "content": [{"type": "text", "text": "list the temp dir"}]},
                {
                    "role": "assistant",
                    "content": [
                        {"type": "tool_use", "id": "call_1", "name": "shell_exec", "input": {"cmd": "ls /tmp"}}
                    ],
                },
                {
                    "role": "user",
                    "content": [{"type": "tool_result", "tool_use_id": "call_1", "content": "the key is sk-secret"}],
                },
            ],
            [
                {"role": "user", "parts": [{"kind": "text", "text": "list the temp dir"}]},
                {"role": "assistant", "parts": [{"kind": "tool_call", "tool_call": {"name": "shell_exec"}}]},
                {
                    "role": "tool",
                    "parts": [{"kind": "tool_result", "tool_result": {"content": "the key is [REDACTED]"}}],
                },
            ],
            [
                {"role": "user", "content": [{"type": "text", "text": "list the temp dir"}]},
                {
                    "role": "assistant",
                    "content": [
                        {"type": "tool_use", "id": "call_1", "name": "shell_exec", "input": {"cmd": "ls /tmp"}}
                    ],
                },
                {
                    "role": "user",
                    "content": [{"type": "tool_result", "tool_use_id": "call_1", "content": "the key is [REDACTED]"}],
                },
            ],
            id="anthropic_tool_blocks",
        ),
        pytest.param(
            [
                {"role": "user", "content": [{"type": "text", "text": "list the temp dir"}]},
                {
                    "role": "user",
                    "content": [
                        {
                            "type": "tool_result",
                            "tool_use_id": "call_1",
                            "content": [
                                {
                                    "type": "text",
                                    "text": "the key is sk-secret",
                                    "cache_control": {"type": "ephemeral"},
                                }
                            ],
                        }
                    ],
                },
            ],
            [
                {"role": "user", "parts": [{"kind": "text", "text": "list the temp dir"}]},
                {
                    "role": "tool",
                    "parts": [{"kind": "tool_result", "tool_result": {"content": "the key is [REDACTED]"}}],
                },
            ],
            [
                {"role": "user", "content": [{"type": "text", "text": "list the temp dir"}]},
                {
                    "role": "user",
                    "content": [
                        {
                            "type": "tool_result",
                            "tool_use_id": "call_1",
                            "content": [
                                {
                                    "type": "text",
                                    "text": "the key is [REDACTED]",
                                    "cache_control": {"type": "ephemeral"},
                                }
                            ],
                        }
                    ],
                },
            ],
            id="anthropic_tool_result_holding_a_text_block",
        ),
    ],
)
def test_tool_using_conversation_keeps_its_tool_history(hook_server, messages, transformed_messages, want):
    """A rewritten agent turn keeps the calls it made and the ids that pair them.

    Only text is written back, into the structure the caller sent, so a tool call
    survives whether it arrived as OpenAI ``tool_calls`` or an Anthropic
    ``tool_use`` block. The rewrite writes a tool result into the message or the
    block that carried it, and leaves its ``tool_call_id`` alone: the wire shape
    of a transformed tool call can omit the id, and a rewrite that dropped it
    would unpair the call from its result.
    """
    hook_server.response = {"action": "allow", "transformed_input": {"messages": transformed_messages}}
    client = _new_client(hook_server.url)
    guard = Agento11yLiteLLMGuardrail(client=client, default_on=True)
    data = _request_data(messages=messages)
    before = _request_content(data)

    result = _call(guard, data)

    assert result["messages"] == want
    assert _request_content(data) == before


def test_tool_using_conversation_still_gets_its_system_prompt_transformed(hook_server, caplog):
    """The adapter applies the system prompt rewrite and the message rewrite independently.

    A skipped message rewrite must not take the system prompt down with it, or a
    system-prompt rule would never take effect on the conversations where the
    message positions happen not to line up.
    """
    hook_server.response = {
        "action": "allow",
        "transformed_input": {
            "system_prompt": "updated policy",
            "messages": [{"role": "user", "parts": [{"kind": "text", "text": "redacted"}]}],
        },
    }
    client = _new_client(hook_server.url)
    guard = Agento11yLiteLLMGuardrail(client=client, default_on=True)
    data = _request_data(
        messages=[
            {"role": "system", "content": "policy"},
            {"role": "user", "content": "list the temp dir"},
            {"role": "tool", "tool_call_id": "call_1", "content": "ok"},
        ]
    )

    with caplog.at_level(logging.WARNING):
        result = _call(guard, data)

    assert result["messages"] == [
        {"role": "system", "content": "updated policy"},
        {"role": "user", "content": "list the temp dir"},
        {"role": "tool", "tool_call_id": "call_1", "content": "ok"},
    ]
    assert any("the positions no longer line up" in record.getMessage() for record in caplog.records)


@pytest.mark.parametrize(
    ("call_type", "body", "want_warning"),
    [
        pytest.param(
            "aresponses",
            {"input": [{"role": "user", "content": [{"type": "input_text", "text": "hello"}]}]},
            "chat message list",
            id="responses_input",
        ),
        pytest.param(
            "atext_completion",
            {"prompt": "hello"},
            "chat message list",
            id="text_completion_prompt",
        ),
    ],
)
def test_message_transform_is_skipped_when_the_body_cannot_take_it(hook_server, caplog, call_type, body, want_warning):
    """Only a chat body has messages to write into.

    ``/v1/responses`` keeps its input under ``input`` and text completion under
    ``prompt``. Writing chat messages into ``messages`` would leave the
    untransformed input in place and add a key the route ignores.
    """
    hook_server.response = {
        "action": "allow",
        "transformed_input": {
            "messages": [
                {"role": "user", "parts": [{"kind": "text", "text": "redacted"}]},
                {"role": "user", "parts": [{"kind": "text", "text": "redacted"}]},
            ]
        },
    }
    client = _new_client(hook_server.url)
    guard = Agento11yLiteLLMGuardrail(client=client, default_on=True)
    data = {"model": "gpt-4o", "metadata": {}, **body}
    before = _request_content(data)

    with caplog.at_level(logging.WARNING):
        result = _call(guard, data, call_type=call_type)

    assert result is None
    assert _request_content(data) == before
    assert any(want_warning in record.getMessage() for record in caplog.records), [
        record.getMessage() for record in caplog.records
    ]


@pytest.mark.parametrize(
    ("messages", "transformed_messages", "want"),
    [
        pytest.param(
            [
                {"role": "system", "content": "policy"},
                {"role": "developer", "content": "and this"},
                {"role": "user", "content": "my key is sk-secret"},
                {
                    "role": "user",
                    "content": [
                        {"type": "text", "text": "and this one is sk-other"},
                        {"type": "image_url", "image_url": {"url": "data:image/png;base64,AAA"}},
                    ],
                },
            ],
            [
                {"role": "user", "parts": [{"kind": "text", "text": "my key is [REDACTED]"}]},
                {"role": "user", "parts": [{"kind": "text", "text": "and this one is [REDACTED]"}]},
            ],
            [
                {"role": "system", "content": "policy"},
                {"role": "developer", "content": "and this"},
                {"role": "user", "content": "my key is [REDACTED]"},
                {
                    "role": "user",
                    "content": [
                        {"type": "text", "text": "and this one is [REDACTED]"},
                        {"type": "image_url", "image_url": {"url": "data:image/png;base64,AAA"}},
                    ],
                },
            ],
            id="image_block_and_system_messages_stay_in_place",
        ),
        pytest.param(
            [
                {"role": "user", "content": "my key is sk-secret"},
                {"role": "assistant", "content": "noted sk-secret", "reasoning_content": "the key is sk-secret"},
            ],
            [
                {"role": "user", "parts": [{"kind": "text", "text": "my key is [REDACTED]"}]},
                {
                    "role": "assistant",
                    "parts": [
                        {"kind": "thinking", "thinking": "the key is [REDACTED]"},
                        {"kind": "text", "text": "noted [REDACTED]"},
                    ],
                },
            ],
            [
                {"role": "user", "content": "my key is [REDACTED]"},
                {"role": "assistant", "content": "noted [REDACTED]", "reasoning_content": "the key is sk-secret"},
            ],
            id="reasoning_is_left_as_sent",
        ),
        pytest.param(
            [
                {
                    "role": "user",
                    "content": [{"type": "text", "text": "a long document", "cache_control": {"type": "ephemeral"}}],
                }
            ],
            [{"role": "user", "parts": [{"kind": "text", "text": "a redacted document"}]}],
            [
                {
                    "role": "user",
                    "content": [
                        {"type": "text", "text": "a redacted document", "cache_control": {"type": "ephemeral"}}
                    ],
                }
            ],
            id="cache_control_breakpoint_survives",
        ),
        pytest.param(
            [{"role": "user", "content": "my key is sk-secret", "name": "alice"}],
            [{"role": "user", "parts": [{"kind": "text", "text": "my key is [REDACTED]"}]}],
            [{"role": "user", "content": "my key is [REDACTED]", "name": "alice"}],
            id="message_name_survives",
        ),
    ],
)
def test_message_rewrite_touches_only_the_text(hook_server, messages, transformed_messages, want):
    """Everything the transform did not carry is left as the caller sent it.

    The rewrite writes text into the structure that held it, so an image block, a
    message ``name``, and an Anthropic ``cache_control`` breakpoint come through
    untouched. ``reasoning_content`` is left as sent even when the transform
    redacted it: a provider validates a reasoning payload against its own
    signature and rejects a rewritten one.

    ``system`` and ``developer`` messages keep their positions. They are not part
    of the transformed list, which is why the two lists are matched after they are
    filtered out.
    """
    hook_server.response = {"action": "allow", "transformed_input": {"messages": transformed_messages}}
    client = _new_client(hook_server.url)
    guard = Agento11yLiteLLMGuardrail(client=client, default_on=True)
    data = _request_data(messages=messages)
    before = _request_content(data)

    result = _call(guard, data)

    assert result["messages"] == want
    assert _request_content(data) == before


@pytest.mark.parametrize(
    ("messages", "transformed_messages", "want_warning"),
    [
        pytest.param(
            [
                {"role": "user", "content": "list the temp dir"},
                {
                    "role": "assistant",
                    "tool_calls": [
                        {"id": "call_1", "type": "function", "function": {"name": "shell_exec", "arguments": "{}"}}
                    ],
                },
            ],
            [
                {"role": "user", "parts": [{"kind": "text", "text": "list the temp dir"}]},
                {"role": "assistant", "parts": [{"kind": "text", "text": "invented"}]},
            ],
            "message 1 carries no content the transform can be written into",
            id="text_for_a_turn_that_only_called_a_tool",
        ),
        pytest.param(
            [{"role": "user", "content": "my key is sk-secret"}],
            [
                {
                    "role": "user",
                    "parts": [{"kind": "text", "text": "my key is"}, {"kind": "text", "text": "[REDACTED]"}],
                }
            ],
            "message 0 holds one string and the transform carries 2 values for it",
            id="two_values_for_one_string",
        ),
        pytest.param(
            [
                {
                    "role": "user",
                    "content": [
                        {"type": "text", "text": "my key is sk-secret"},
                        {"type": "image_url", "image_url": {"url": "data:image/png;base64,AAA"}},
                    ],
                }
            ],
            [
                {
                    "role": "user",
                    "parts": [{"kind": "text", "text": "my key is"}, {"kind": "text", "text": "[REDACTED]"}],
                }
            ],
            "message 0 holds 1 text block and the transform carries 2 texts",
            id="more_texts_than_text_blocks",
        ),
        pytest.param(
            [
                {
                    "role": "assistant",
                    "content": [
                        {"type": "text", "text": "here it is"},
                        {"type": "refusal", "refusal": "I cannot help with that"},
                    ],
                }
            ],
            [
                {
                    "role": "assistant",
                    "parts": [
                        {"kind": "text", "text": "here it is"},
                        {"kind": "text", "text": "I cannot help with that"},
                    ],
                }
            ],
            "message 0 holds 1 text block and the transform carries 2 texts",
            id="refusal_block_is_not_a_text_slot",
        ),
        pytest.param(
            [
                {
                    "role": "user",
                    "content": [
                        {
                            "type": "tool_result",
                            "tool_use_id": "call_1",
                            "content": [
                                {"type": "text", "text": "the key is sk-secret"},
                                {"type": "text", "text": "and so is this"},
                            ],
                        }
                    ],
                }
            ],
            [
                {
                    "role": "user",
                    "parts": [{"kind": "tool_result", "tool_result": {"content": "the key is [REDACTED]"}}],
                }
            ],
            "message 0 holds a tool result block at position 0 a rewrite cannot reproduce",
            id="tool_result_with_several_nested_blocks",
        ),
        pytest.param(
            [
                {"role": "user", "content": "list the temp dir"},
                {
                    "role": "tool",
                    "tool_call_id": "call_1",
                    "content": [
                        {"type": "text", "text": "the key is sk-secret"},
                        {"type": "text", "text": "and so is this"},
                    ],
                },
            ],
            [
                {"role": "user", "parts": [{"kind": "text", "text": "list the temp dir"}]},
                {
                    "role": "tool",
                    "parts": [{"kind": "tool_result", "tool_result": {"content": "the key is [REDACTED]"}}],
                },
            ],
            "message 1 holds tool result content a rewrite cannot reproduce",
            id="openai_tool_message_with_several_text_blocks",
        ),
    ],
)
def test_transform_the_messages_cannot_hold_is_skipped(
    hook_server, caplog, messages, transformed_messages, want_warning
):
    """A transformed value with no place to go stops the whole rewrite.

    A turn that only called a tool has no ``content`` to write into, and a string
    or a block list holds a fixed number of text slots. Writing what fits and
    dropping the rest would forward a half-redacted conversation.
    """
    hook_server.response = {"action": "allow", "transformed_input": {"messages": transformed_messages}}
    client = _new_client(hook_server.url)
    guard = Agento11yLiteLLMGuardrail(client=client, default_on=True)
    data = _request_data(messages=messages)
    before = _request_content(data)

    with caplog.at_level(logging.WARNING):
        result = _call(guard, data)

    assert result is None
    assert _request_content(data) == before
    warnings = [record.getMessage() for record in caplog.records]
    assert any(want_warning in message for message in warnings), warnings


@pytest.mark.parametrize(
    ("messages", "transformed_messages"),
    [
        pytest.param(
            [
                {"role": "user", "content": "my key is sk-secret"},
                {"role": "assistant", "content": ""},
                {"role": "user", "content": "use it"},
            ],
            [
                {"role": "user", "parts": [{"kind": "text", "text": "my key is [REDACTED]"}]},
                {"role": "user", "parts": [{"kind": "text", "text": "use it"}]},
            ],
            id="forward_mapping_dropped_an_empty_message",
        ),
        pytest.param(
            [
                {"role": "user", "content": "my key is sk-secret"},
                {"role": "user", "content": "use it"},
            ],
            [{"role": "user", "parts": [{"kind": "text", "text": "my key is [REDACTED]"}]}],
            id="rule_returned_a_shorter_list",
        ),
        pytest.param(
            [
                {"role": "user", "content": "list the temp dir"},
                {"role": "assistant", "function_call": {"name": "shell_exec", "arguments": "{}"}},
                {"role": "function", "name": "shell_exec", "content": "ok"},
            ],
            [
                {"role": "user", "parts": [{"kind": "text", "text": "list the [REDACTED] dir"}]},
                {"role": "user", "parts": [{"kind": "text", "text": "[REDACTED]"}]},
            ],
            id="legacy_function_call",
        ),
    ],
)
def test_transform_of_a_different_length_is_skipped(hook_server, caplog, messages, transformed_messages):
    """Matching by position needs the two lists to be the same length.

    ``_map_messages`` drops a message it cannot turn into text, so the transform
    comes back one message short and every message after the gap would take the
    wrong turn's text. A rule is free to return a shorter list for its own
    reasons, with the same result. A legacy ``function_call`` turn reaches this
    too: it is not mapped forward, so it is never sent and never comes back.
    """
    hook_server.response = {"action": "allow", "transformed_input": {"messages": transformed_messages}}
    client = _new_client(hook_server.url)
    guard = Agento11yLiteLLMGuardrail(client=client, default_on=True)
    data = _request_data(messages=messages)
    before = _request_content(data)

    with caplog.at_level(logging.WARNING):
        result = _call(guard, data)

    assert result is None
    assert _request_content(data) == before
    warnings = [record.getMessage() for record in caplog.records]
    assert any("the positions no longer line up" in message for message in warnings), warnings


def test_anthropic_messages_body_gets_its_message_rewrite(hook_server):
    """A plain-text ``/v1/messages`` body is rewritten like a chat body.

    ``{role, content}`` is valid on both routes. Anthropic's own rules about
    alternation and a leading user turn are the server's to enforce; the adapter
    does not reorder anything.
    """
    hook_server.response = {
        "action": "allow",
        "transformed_input": {
            "messages": [{"role": "user", "parts": [{"kind": "text", "text": "my key is [REDACTED]"}]}]
        },
    }
    client = _new_client(hook_server.url)
    guard = Agento11yLiteLLMGuardrail(client=client, default_on=True)
    data = {
        "model": "claude-3-5-sonnet-20240620",
        "metadata": {},
        "system": "policy",
        "messages": [{"role": "user", "content": "my key is sk-secret"}],
    }

    result = _call(guard, data, call_type="anthropic_messages")

    assert _request_content(result) == {
        "model": "claude-3-5-sonnet-20240620",
        "system": "policy",
        "messages": [{"role": "user", "content": "my key is [REDACTED]"}],
    }


def test_apply_transforms_false_leaves_the_request_unchanged(hook_server):
    hook_server.response = {
        "action": "allow",
        "transformed_input": {
            "system_prompt": "updated policy",
            "messages": [{"role": "user", "parts": [{"kind": "text", "text": "redacted"}]}],
        },
    }
    client = _new_client(hook_server.url)
    guard = Agento11yLiteLLMGuardrail(client=client, default_on=True, apply_transforms=False)
    data = _request_data()
    before = _request_content(data)

    assert _call(guard, data) is None

    assert len(hook_server.requests) == 1
    assert _request_content(data) == before


@pytest.mark.parametrize("apply_transforms", [True, False])
def test_deny_wins_over_a_transform(hook_server, apply_transforms):
    """A denied request is blocked, not rewritten and forwarded.

    Enforcement runs before the transform flag is read, so the flag cannot change
    the outcome.
    """
    hook_server.response = {
        "action": "deny",
        "rule_id": "no-secrets",
        "reason": "contains a credential",
        "transformed_input": {"messages": [{"role": "user", "parts": [{"kind": "text", "text": "redacted"}]}]},
    }
    client = _new_client(hook_server.url)
    guard = Agento11yLiteLLMGuardrail(client=client, default_on=True, apply_transforms=apply_transforms)
    data = _request_data()

    with pytest.raises(GuardrailRaisedException):
        _call(guard, data)

    assert data["messages"] == [{"role": "user", "content": "hello"}]


@pytest.mark.parametrize("mode", ["during_call", "logging_only"])
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
    with pytest.raises(ValueError, match="during_call"):
        Agento11yLiteLLMGuardrail(client=client, event_hook=["pre_call", "during_call"])


@pytest.mark.parametrize("event_hook", [[], (), set()], ids=["list", "tuple", "set"])
def test_empty_event_hook_rejected_at_construction(event_hook):
    """An empty sequence matches no mode, so the guardrail would never run."""
    client = _new_client("http://127.0.0.1:1")
    with pytest.raises(ValueError, match="at least one mode"):
        Agento11yLiteLLMGuardrail(client=client, event_hook=event_hook)


@pytest.mark.parametrize(
    ("event_hook", "want"),
    [
        pytest.param(("pre_call", "post_call"), ["pre_call", "post_call"], id="tuple"),
        pytest.param({"pre_call"}, ["pre_call"], id="set"),
        pytest.param((GuardrailEventHooks.pre_call,), ["pre_call"], id="tuple-of-enums"),
    ],
)
def test_a_sequence_of_modes_is_normalized_to_a_list(hook_server, event_hook, want):
    """LiteLLM matches a list and compares anything else to the mode string.

    A tuple or a set never equals ``"pre_call"``, so an unnormalized one builds
    a guardrail that runs on no phase and says nothing about it.
    """
    client = _new_client(hook_server.url)
    guard = Agento11yLiteLLMGuardrail(client=client, event_hook=event_hook, default_on=True)

    assert guard.event_hook == want
    assert guard.should_run_guardrail(_request_data(), GuardrailEventHooks.pre_call) is True
    assert _call(guard, _request_data()) is None
    assert len(hook_server.requests) == 1


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
    ("kwargs", "hooks", "want"),
    [
        pytest.param(
            {},
            HooksConfig(enabled=False),
            ("preflight", "no request will be evaluated"),
            id="hooks-disabled",
        ),
        pytest.param(
            {},
            HooksConfig(enabled=True, phases=["postflight"]),
            ("preflight", "no request will be evaluated"),
            id="preflight-phase-not-configured",
        ),
        pytest.param(
            {"event_hook": "post_call"},
            HooksConfig(enabled=True, phases=["preflight"]),
            ("postflight", "no request will be evaluated"),
            id="postflight-phase-not-configured",
        ),
        pytest.param(
            {"event_hook": ["pre_call", "post_call"]},
            HooksConfig(enabled=True, phases=["preflight"]),
            ("postflight", "that phase will not be evaluated"),
            id="one-of-two-phases-configured",
        ),
    ],
)
def test_guardrail_warns_at_construction_about_an_unconfigured_phase(hook_server, caplog, kwargs, hooks, want):
    """A mode with no matching ``HooksConfig.phases`` entry is silent otherwise."""
    client = _new_client(hook_server.url, hooks=hooks)

    with caplog.at_level(logging.WARNING):
        Agento11yLiteLLMGuardrail(client=client, default_on=True, guardrail_name="agento11y-guard", **kwargs)

    warnings = [record for record in caplog.records if record.levelno == logging.WARNING]
    assert len(warnings) == 1
    assert "agento11y-guard" in warnings[0].getMessage()
    for fragment in want:
        assert fragment in warnings[0].getMessage()


def test_a_guardrail_whose_every_phase_is_configured_warns_about_nothing(hook_server, caplog):
    client = _new_client(hook_server.url, hooks=HooksConfig(enabled=True, phases=["preflight", "postflight"]))

    with caplog.at_level(logging.WARNING):
        Agento11yLiteLLMGuardrail(client=client, default_on=True, event_hook=["pre_call", "post_call"])

    assert [record for record in caplog.records if record.levelno == logging.WARNING] == []


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


def _chat_response(content: str = "here is the secret") -> ModelResponse:
    return ModelResponse(
        id="chatcmpl-1",
        model="gpt-4o",
        choices=[{"index": 0, "finish_reason": "stop", "message": {"role": "assistant", "content": content}}],
    )


def _responses_api_response(content: str = "here is the secret") -> ResponsesAPIResponse:
    return ResponsesAPIResponse(
        id="resp-1",
        created_at=1,
        model="gpt-4o",
        object="response",
        output=[
            {
                "type": "message",
                "id": "msg-1",
                "status": "completed",
                "role": "assistant",
                "content": [{"type": "output_text", "text": content, "annotations": []}],
            }
        ],
        parallel_tool_calls=False,
        tool_choice="auto",
        tools=[],
        status="completed",
    )


_NO_RESPONSE = object()


def _post_call(
    guard: Agento11yLiteLLMGuardrail,
    data: dict[str, Any],
    response: Any = _NO_RESPONSE,
) -> Any:
    """Run the post-call hook, defaulting to a plain chat response.

    ``None`` is itself a response under test, so the default is a sentinel.
    """
    return asyncio.run(
        guard.async_post_call_success_hook(
            data=data,
            user_api_key_dict=_UserAPIKey(),
            response=_chat_response() if response is _NO_RESPONSE else response,
        )
    )


def _postflight_guard(client: Client, **kwargs: Any) -> Agento11yLiteLLMGuardrail:
    return Agento11yLiteLLMGuardrail(client=client, event_hook="post_call", default_on=True, **kwargs)


def test_postflight_allow_returns_none_and_leaves_the_response_untouched(hook_server):
    hook_server.response = {"action": "allow"}
    client = _new_client(hook_server.url, hooks=HooksConfig(enabled=True, phases=["preflight", "postflight"]))
    guard = _postflight_guard(client)
    response = _chat_response()
    before = response.model_dump()

    assert _post_call(guard, _request_data(), response) is None

    assert len(hook_server.requests) == 1
    assert response.model_dump() == before


def test_postflight_ignores_a_transform(hook_server):
    hook_server.response = {
        "action": "allow",
        "transformed_input": {"messages": [{"role": "user", "parts": [{"kind": "text", "text": "redacted"}]}]},
    }
    client = _new_client(hook_server.url, hooks=HooksConfig(enabled=True, phases=["postflight"]))
    guard = _postflight_guard(client)
    data = _request_data()
    response = _chat_response()
    before = response.model_dump()

    assert _post_call(guard, data, response) is None

    assert response.model_dump() == before
    assert data["messages"] == _request_data()["messages"]


def test_postflight_request_carries_the_phase_the_request_and_the_output(hook_server):
    client = _new_client(hook_server.url, hooks=HooksConfig(enabled=True, phases=["postflight"]))
    guard = _postflight_guard(client, agent_version="1.2.3", extra_tags={"team": "search"})
    data = _request_data(
        messages=[
            {"role": "system", "content": "policy"},
            {"role": "user", "content": "hello"},
        ],
        metadata={"agent_id": "search-agent", "session_id": "conv-7"},
    )

    assert _post_call(guard, data) is None

    payload = hook_server.payloads[0]
    assert payload["phase"] == "postflight"
    assert payload["context"]["agent_name"] == "search-agent"
    assert payload["context"]["conversation_id"] == "conv-7"
    assert payload["context"]["model"] == {"provider": "openai", "name": "gpt-4o"}
    assert payload["context"]["tags"]["team"] == "search"
    assert payload["input"]["system_prompt"] == "policy"
    assert [m["parts"][0]["text"] for m in payload["input"]["messages"]] == ["hello"]
    assert [m["role"] for m in payload["input"]["output"]] == ["assistant"]
    assert [part["text"] for m in payload["input"]["output"] for part in m["parts"]] == ["here is the secret"]


@pytest.mark.parametrize(
    ("response", "want_texts"),
    [
        pytest.param(_chat_response("chat text"), ["chat text"], id="chat_model_response"),
        pytest.param(_responses_api_response("responses text"), ["responses text"], id="responses_api_response"),
        pytest.param(
            {
                "id": "msg-1",
                "type": "message",
                "role": "assistant",
                "content": [{"type": "text", "text": "anthropic text"}],
                "stop_reason": "end_turn",
            },
            ["anthropic text"],
            id="anthropic_messages_dict",
        ),
    ],
)
def test_postflight_maps_the_output_of_every_response_shape(hook_server, response, want_texts):
    """Each route hands the hook a different object.

    ``/v1/chat/completions`` and streamed responses arrive as a ``ModelResponse``,
    ``/v1/responses`` as a ``ResponsesAPIResponse``, and ``/v1/messages`` as a
    plain Anthropic-shaped dict. A mapper that reads only ``choices`` sends an
    empty output for two of the three, and a rule written against the response
    matches nothing.
    """
    client = _new_client(hook_server.url, hooks=HooksConfig(enabled=True, phases=["postflight"]))
    guard = _postflight_guard(client)

    assert _post_call(guard, _request_data(), response) is None

    output = hook_server.payloads[0]["input"]["output"]
    assert [part["text"] for m in output for part in m["parts"]] == want_texts


def test_postflight_tool_calls_in_the_output_keep_their_kind(hook_server):
    """A tool-filter guard blocks a proposed call only if it arrives as a tool_call part."""
    client = _new_client(hook_server.url, hooks=HooksConfig(enabled=True, phases=["postflight"]))
    guard = _postflight_guard(client)
    response = ModelResponse(
        id="chatcmpl-1",
        model="gpt-4o",
        choices=[
            {
                "index": 0,
                "finish_reason": "tool_calls",
                "message": {
                    "role": "assistant",
                    "content": None,
                    "tool_calls": [
                        {
                            "id": "call_1",
                            "type": "function",
                            "function": {"name": "shell_exec", "arguments": '{"cmd":"rm -rf /"}'},
                        }
                    ],
                },
            }
        ],
    )

    assert _post_call(guard, _request_data(), response) is None

    output = hook_server.payloads[0]["input"]["output"]
    assert [[part["kind"] for part in m["parts"]] for m in output] == [["tool_call"]]
    assert output[0]["parts"][0]["tool_call"]["name"] == "shell_exec"
    assert output[0]["parts"][0]["tool_call"]["input_json"] == {"cmd": "rm -rf /"}


def test_postflight_deny_blocks_a_non_streaming_response(hook_server):
    """A non-streaming deny reaches the caller as a 400 and the output is suppressed.

    Verified against a live proxy: ``ProxyLogging.post_call_success_hook`` is
    awaited before the route serializes the response, so the exception replaces
    the provider output instead of arriving after it.
    """
    hook_server.response = {"action": "deny", "rule_id": "no-secrets", "reason": "sensitive output"}
    client = _new_client(hook_server.url, hooks=HooksConfig(enabled=True, phases=["postflight"]))
    guard = _postflight_guard(client, guardrail_name="agento11y-postflight")
    data = _request_data()

    with pytest.raises(GuardrailRaisedException) as excinfo:
        _post_call(guard, data)

    assert "sensitive output" in str(excinfo.value)
    assert "no-secrets" in str(excinfo.value)
    assert excinfo.value.guardrail_name == "agento11y-postflight"
    assert excinfo.value.status_code == 400
    entries = data["metadata"]["standard_logging_guardrail_information"]
    assert [entry["guardrail_status"] for entry in entries] == ["guardrail_intervened"]


def test_postflight_deny_on_a_streamed_response_records_without_blocking(hook_server, caplog):
    """A streamed response is already delivered, so a deny can only be recorded.

    LiteLLM runs this hook on the assembled response after the last chunk has
    been flushed (``_run_deferred_stream_guardrails``) and swallows whatever it
    raises, so raising would produce a proxy traceback and nothing else.
    """
    hook_server.response = {"action": "deny", "rule_id": "no-secrets", "reason": "sensitive output"}
    client = _new_client(hook_server.url, hooks=HooksConfig(enabled=True, phases=["postflight"]))
    guard = _postflight_guard(client)
    data = _request_data(stream=True)
    response = _chat_response()

    with caplog.at_level(logging.WARNING):
        assert _post_call(guard, data, response) is None

    assert response.choices[0].message.content == "here is the secret"
    assert any("already been delivered" in record.getMessage() for record in caplog.records)
    entries = data["metadata"]["standard_logging_guardrail_information"]
    assert [entry["guardrail_status"] for entry in entries] == ["guardrail_intervened"]
    assert entries[0]["guardrail_response"]["reason"] == "sensitive output"
    # The only field that tells a recorded deny apart from an enforced one.
    assert entries[0]["guardrail_response"]["enforced"] is False


def test_streamed_deny_record_carries_its_own_timings(hook_server):
    """The record is written by hand, so it has to carry what the decorator would.

    Without timings the OTel exporter stamps the guardrail span at export time
    and reports a near-zero duration.
    """
    hook_server.response = {"action": "deny", "rule_id": "no-secrets", "reason": "sensitive output"}
    client = _new_client(hook_server.url, hooks=HooksConfig(enabled=True, phases=["postflight"]))
    guard = _postflight_guard(client)
    data = _request_data(stream=True)

    assert _post_call(guard, data) is None

    entry = data["metadata"]["standard_logging_guardrail_information"][0]
    assert entry["start_time"] is not None
    assert entry["end_time"] >= entry["start_time"]
    assert entry["duration"] >= 0


class _NeverSelfRecorded:
    """Stands in for the flag LiteLLM added in 1.95.0, on a version without it."""

    def set(self, value: bool) -> None:
        return None

    def get(self) -> bool:
        return False

    def reset(self, token: Any) -> None:
        return None


@pytest.mark.parametrize(
    ("hook_response", "hook_status", "want_status"),
    [
        pytest.param({"action": "allow"}, 200, "success", id="allow"),
        pytest.param(
            {"action": "deny", "rule_id": "no-secrets", "reason": "sensitive output"},
            200,
            "guardrail_intervened",
            id="deny",
        ),
        pytest.param({}, 500, "guardrail_failed_to_respond", id="evaluation-failed"),
    ],
)
def test_a_streamed_verdict_is_recorded_once_without_litellm_s_self_record_flag(
    hook_server,
    monkeypatch,
    hook_response,
    hook_status,
    want_status,
):
    """LiteLLM before 1.95.0 has no flag for a guardrail that files its own verdict.

    On those versions ``log_guardrail_information`` appends a passing entry after
    a normal return whatever the wrapped function recorded, so a hand-filed deny
    reads as denied and passed at once. This package supports 1.82.3, so the
    streamed path files every verdict itself and stays undecorated.
    """
    monkeypatch.setattr(custom_guardrail, "_guardrail_self_recorded", _NeverSelfRecorded(), raising=False)
    hook_server.response = hook_response
    hook_server.status = hook_status
    client = _new_client(
        hook_server.url,
        hooks=HooksConfig(enabled=True, phases=["postflight"], timeout_seconds=1.0, fail_open=True),
    )
    guard = _postflight_guard(client)
    data = _request_data(stream=True)

    assert _post_call(guard, data) is None

    assert _only_guardrail_entry(data)["guardrail_status"] == want_status


@pytest.mark.parametrize("stream", [1, "true", "True"], ids=["int", "lowercase-string", "string"])
def test_postflight_deny_blocks_a_request_whose_stream_flag_is_truthy_but_not_true(hook_server, stream):
    """LiteLLM streams on ``data["stream"] is True`` and nothing coerces the value.

    A client that sends ``"stream": 1`` is served non-streamed, so a deny has to
    raise. Reading the flag as truthy would take the record-only branch and hand
    the denied content to the caller with HTTP 200.
    """
    hook_server.response = {"action": "deny", "rule_id": "no-secrets", "reason": "sensitive output"}
    client = _new_client(hook_server.url, hooks=HooksConfig(enabled=True, phases=["postflight"]))
    guard = _postflight_guard(client)

    with pytest.raises(GuardrailRaisedException):
        _post_call(guard, _request_data(stream=stream))


def test_preflight_deny_blocks_a_streaming_request(hook_server):
    """Only postflight downgrades to recording on a stream.

    Preflight runs before routing, so a streamed request is rejected before the
    first chunk and the caller still gets a 400.
    """
    hook_server.response = {"action": "deny", "rule_id": "no-secrets", "reason": "blocked"}
    client = _new_client(hook_server.url)
    guard = Agento11yLiteLLMGuardrail(client=client, default_on=True)

    with pytest.raises(GuardrailRaisedException):
        _call(guard, _request_data(stream=True))


def test_postflight_allow_records_a_success_verdict(hook_server):
    client = _new_client(hook_server.url, hooks=HooksConfig(enabled=True, phases=["postflight"]))
    guard = _postflight_guard(client)
    data = _request_data()

    _post_call(guard, data)

    entries = data["metadata"]["standard_logging_guardrail_information"]
    assert [entry["guardrail_status"] for entry in entries] == ["success"]
    assert entries[0]["guardrail_name"] == DEFAULT_GUARDRAIL_NAME


@pytest.mark.parametrize(
    "hooks",
    [
        HooksConfig(enabled=False),
        HooksConfig(enabled=True),
        HooksConfig(enabled=True, phases=["preflight"]),
    ],
    ids=["hooks-disabled", "default-phases", "postflight-phase-not-configured"],
)
def test_postflight_does_not_run_when_the_sdk_phase_is_not_configured(hook_server, hooks):
    """``mode: post_call`` is only one of the two gates; ``HooksConfig.phases`` is the other.

    The gate has to sit outside the decorated body. Left to
    ``Client.evaluate_hook``, it returns allow without contacting the server and
    the decorator records a passed check, so a deployment that enabled
    ``post_call`` and left ``HooksConfig`` alone reports every response as
    evaluated and passed.
    """
    client = _new_client(hook_server.url, hooks=hooks)
    guard = _postflight_guard(client)
    response = _chat_response()
    data = _request_data()

    assert _post_call(guard, data, response) is None
    assert hook_server.requests == []
    assert "standard_logging_guardrail_information" not in data["metadata"]


def test_postflight_does_not_run_on_a_body_it_cannot_read_input_from(hook_server):
    """Stands in for the call-type gate preflight applies.

    The post-call hook is not given a call type, and a native pass-through body
    sits under ``data["data"]``, so the request input is the only signal that
    this is not a route the adapter guards.
    """
    client = _new_client(hook_server.url, hooks=HooksConfig(enabled=True, phases=["postflight"]))
    guard = _postflight_guard(client)
    data = {
        "model": "claude-3-5-sonnet",
        "data": {"messages": [{"role": "user", "content": "hello"}]},
        "metadata": {},
    }

    assert _post_call(guard, data) is None
    assert hook_server.requests == []
    assert "standard_logging_guardrail_information" not in data["metadata"]


def test_postflight_transport_failure_fail_closed_raises(hook_server):
    """Fail-closed turns an already-billed provider success into a proxy 500."""
    hook_server.status = 500
    client = _new_client(
        hook_server.url,
        hooks=HooksConfig(enabled=True, phases=["postflight"], timeout_seconds=1.0, fail_open=False),
    )
    guard = _postflight_guard(client)
    data = _request_data()

    with pytest.raises(HookTransportError):
        _post_call(guard, data)

    assert _only_guardrail_entry(data)["guardrail_status"] == "guardrail_failed_to_respond"


def test_postflight_does_not_run_when_the_guardrail_is_not_enabled_for_the_request(hook_server):
    client = _new_client(hook_server.url, hooks=HooksConfig(enabled=True, phases=["postflight"]))
    guard = Agento11yLiteLLMGuardrail(client=client, event_hook="post_call", default_on=False)
    data = _request_data()

    assert _post_call(guard, data) is None
    assert hook_server.requests == []
    assert "standard_logging_guardrail_information" not in data["metadata"]


def test_postflight_does_not_run_on_a_preflight_only_guardrail(hook_server):
    client = _new_client(hook_server.url, hooks=HooksConfig(enabled=True, phases=["preflight", "postflight"]))
    guard = Agento11yLiteLLMGuardrail(client=client, default_on=True)

    assert _post_call(guard, _request_data()) is None
    assert hook_server.requests == []


@pytest.mark.parametrize(
    "response",
    [
        pytest.param(litellm.ImageResponse(created=1, data=[{"url": "https://example.test/cat.png"}]), id="image"),
        pytest.param(_chat_response(""), id="empty_content"),
        pytest.param(None, id="no_response"),
    ],
)
def test_postflight_records_no_verdict_when_the_response_carries_no_text(hook_server, response):
    """A response this adapter cannot read is skipped rather than sent as empty output.

    An evaluation with no output returns allow and records a verdict that reads
    like a completed check.
    """
    client = _new_client(hook_server.url, hooks=HooksConfig(enabled=True, phases=["postflight"]))
    guard = _postflight_guard(client)
    data = _request_data()

    assert _post_call(guard, data, response) is None
    assert hook_server.requests == []
    assert "standard_logging_guardrail_information" not in data["metadata"]


def test_both_phases_evaluate_exactly_once_each_for_one_request(hook_server):
    client = _new_client(hook_server.url, hooks=HooksConfig(enabled=True, phases=["preflight", "postflight"]))
    guard = Agento11yLiteLLMGuardrail(client=client, event_hook=["pre_call", "post_call"], default_on=True)
    data = _request_data()

    assert _call(guard, data) is None
    assert _post_call(guard, data) is None

    assert [payload["phase"] for payload in hook_server.payloads] == ["preflight", "postflight"]
    entries = data["metadata"]["standard_logging_guardrail_information"]
    assert len(entries) == 2


@pytest.mark.parametrize(
    "event_hook",
    ["post_call", GuardrailEventHooks.post_call, ["post_call"]],
    ids=["string", "enum", "list"],
)
def test_post_call_mode_is_accepted_in_every_shape(hook_server, event_hook):
    client = _new_client(hook_server.url, hooks=HooksConfig(enabled=True, phases=["postflight"]))
    guard = Agento11yLiteLLMGuardrail(client=client, event_hook=event_hook, default_on=True)

    assert guard.event_hook == event_hook
    assert _post_call(guard, _request_data()) is None
    assert len(hook_server.requests) == 1


def test_postflight_slow_server_honors_request_timeout_and_names_the_phase(hook_server, caplog):
    hook_server.delay = 1.0
    client = _new_client(
        hook_server.url,
        hooks=HooksConfig(enabled=True, phases=["postflight"], timeout_seconds=5.0, fail_open=True),
    )
    guard = _postflight_guard(client, request_timeout_seconds=0.1)

    data = _request_data()

    with caplog.at_level(logging.WARNING):
        assert _post_call(guard, data) is None

    assert any(
        "postflight hook evaluation failed" in record.getMessage() and "timed out after 0.1s" in record.getMessage()
        for record in caplog.records
    )
    entry = _only_guardrail_entry(data)
    assert entry["guardrail_status"] == "guardrail_failed_to_respond"
    assert entry["duration"] >= 0.1


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


def test_public_factory_accepts_postflight_mode():
    client = _new_client("http://127.0.0.1:1")
    guard = create_agento11y_litellm_guardrail(client=client, event_hook=["pre_call", "post_call"])

    assert guard.event_hook == ["pre_call", "post_call"]


def test_public_factory_passes_apply_transforms_through(hook_server):
    hook_server.response = {
        "action": "allow",
        "transformed_input": {"messages": [{"role": "user", "parts": [{"kind": "text", "text": "redacted"}]}]},
    }
    client = _new_client(hook_server.url)
    guard = create_agento11y_litellm_guardrail(client=client, default_on=True, apply_transforms=False)

    assert _call(guard, _request_data()) is None


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
