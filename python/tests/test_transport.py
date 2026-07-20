"""Generation transport parity tests (HTTP and gRPC)."""

from __future__ import annotations

import base64
import json
import socket
import threading
from datetime import datetime, timedelta, timezone
from http.server import BaseHTTPRequestHandler, HTTPServer
from typing import Any

import grpc
from agento11y import (
    Artifact,
    ArtifactKind,
    AuthConfig,
    Client,
    ClientConfig,
    Generation,
    GenerationExportConfig,
    GenerationMode,
    GenerationStart,
    Message,
    MessageRole,
    ModelRef,
    Part,
    PartKind,
    TokenUsage,
    ToolCall,
    ToolDefinition,
    ToolResult,
)
from agento11y.internal.gen.sigil.v1 import generation_ingest_pb2 as sigil_pb2
from agento11y.internal.gen.sigil.v1 import generation_ingest_pb2_grpc as sigil_pb2_grpc
from opentelemetry.sdk.trace import TracerProvider


class _CapturingGenerationServicer(sigil_pb2_grpc.GenerationIngestServiceServicer):
    def __init__(self) -> None:
        self.requests: list[sigil_pb2.ExportGenerationsRequest] = []
        self.metadata: list[dict[str, str]] = []
        self._lock = threading.Lock()

    def ExportGenerations(self, request, context):  # noqa: N802
        with self._lock:
            self.requests.append(request)
            self.metadata.append({item.key: item.value for item in context.invocation_metadata()})
        return sigil_pb2.ExportGenerationsResponse(
            results=[
                sigil_pb2.ExportGenerationResult(generation_id=generation.id, accepted=True)
                for generation in request.generations
            ]
        )


import pytest
from agento11y.exporters.http import _normalize_endpoint
from agento11y.version import SDK_VERSION


@pytest.mark.parametrize(
    "raw, expected",
    [
        ("http://host:8080/api/v1/generations:export", "http://host:8080/api/v1/generations:export"),
        ("http://host:8080", "http://host:8080/api/v1/generations:export"),
        ("http://host:8080/", "http://host:8080/api/v1/generations:export"),
        ("https://host:443", "https://host:443/api/v1/generations:export"),
        ("HTTPS://host:443", "https://host:443/api/v1/generations:export"),
        ("host:8080", "http://host:8080/api/v1/generations:export"),
        ("host:8080/api/v1/generations:export", "http://host:8080/api/v1/generations:export"),
        ("http://host:8080/custom/ingest", "http://host:8080/custom/ingest"),
        ("http://host:8080?token=abc", "http://host:8080/api/v1/generations:export?token=abc"),
        ("http://host:8080/custom/ingest?token=abc", "http://host:8080/custom/ingest?token=abc"),
    ],
)
def test_normalize_endpoint(raw: str, expected: str) -> None:
    assert _normalize_endpoint(raw) == expected


def test_sdk_exports_generation_over_http_round_trip() -> None:
    captured = []

    class _Handler(BaseHTTPRequestHandler):
        def do_POST(self):  # noqa: N802
            length = int(self.headers.get("Content-Length", "0"))
            body = self.rfile.read(length)
            payload = json.loads(body.decode("utf-8"))
            captured.append(
                {
                    "path": self.path,
                    "content_type": self.headers.get("Content-Type"),
                    "payload": payload,
                }
            )

            response = {
                "results": [
                    {
                        "generation_id": generation["id"],
                        "accepted": True,
                    }
                    for generation in payload.get("generations", [])
                ]
            }
            encoded = json.dumps(response).encode("utf-8")
            self.send_response(202)
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
        GenerationExportConfig(
            protocol="http",
            endpoint=f"http://127.0.0.1:{server.server_address[1]}/api/v1/generations:export",
            batch_size=1,
            flush_interval=timedelta(seconds=1),
            max_retries=1,
            initial_backoff=timedelta(milliseconds=1),
            max_backoff=timedelta(milliseconds=10),
        )
    )

    try:
        start, result = _payload_fixture()
        rec = client.start_generation(start)
        rec.set_result(result)
        rec.end()
        assert rec.err() is None

        client.shutdown()

        assert len(captured) == 1
        request = captured[0]
        assert request["path"] == "/api/v1/generations:export"
        assert request["content_type"] == "application/json"

        payload = request["payload"]
        assert isinstance(payload, dict)
        generations = payload.get("generations")
        assert isinstance(generations, list)
        assert len(generations) == 1
        assert isinstance(generations[0], dict)

        _assert_generation_json_payload(generations[0])
    finally:
        server.shutdown()
        server.server_close()


def test_sdk_exports_generation_over_http_base_url_only() -> None:
    """Endpoint with no path auto-appends /api/v1/generations:export."""
    captured = []

    class _Handler(BaseHTTPRequestHandler):
        def do_POST(self):  # noqa: N802
            length = int(self.headers.get("Content-Length", "0"))
            body = self.rfile.read(length)
            payload = json.loads(body.decode("utf-8"))
            captured.append({"path": self.path})

            response = {
                "results": [{"generation_id": g["id"], "accepted": True} for g in payload.get("generations", [])]
            }
            encoded = json.dumps(response).encode("utf-8")
            self.send_response(202)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(encoded)))
            self.end_headers()
            self.wfile.write(encoded)

        def log_message(self, _format, *_args):  # noqa: A003
            return

    server = HTTPServer(("127.0.0.1", 0), _Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()

    # Only pass base URL — no path
    client = _new_client(
        GenerationExportConfig(
            protocol="http",
            endpoint=f"http://127.0.0.1:{server.server_address[1]}",
            batch_size=1,
            flush_interval=timedelta(seconds=1),
            max_retries=1,
            initial_backoff=timedelta(milliseconds=1),
            max_backoff=timedelta(milliseconds=10),
        )
    )

    try:
        start, result = _payload_fixture()
        rec = client.start_generation(start)
        rec.set_result(result)
        rec.end()
        assert rec.err() is None
        client.shutdown()

        assert len(captured) == 1
        assert captured[0]["path"] == "/api/v1/generations:export"
    finally:
        server.shutdown()
        server.server_close()


def test_sdk_exports_generation_over_grpc_round_trip() -> None:
    servicer = _CapturingGenerationServicer()
    grpc_server = grpc.server(thread_pool=__import__("concurrent.futures").futures.ThreadPoolExecutor(max_workers=2))
    sigil_pb2_grpc.add_GenerationIngestServiceServicer_to_server(servicer, grpc_server)

    sock = socket.socket()
    sock.bind(("127.0.0.1", 0))
    port = sock.getsockname()[1]
    sock.close()

    grpc_server.add_insecure_port(f"127.0.0.1:{port}")
    grpc_server.start()

    client = _new_client(
        GenerationExportConfig(
            protocol="grpc",
            endpoint=f"127.0.0.1:{port}",
            insecure=True,
            batch_size=1,
            flush_interval=timedelta(seconds=1),
            max_retries=1,
            initial_backoff=timedelta(milliseconds=1),
            max_backoff=timedelta(milliseconds=10),
        )
    )

    try:
        start, result = _payload_fixture()
        rec = client.start_generation(start)
        rec.set_result(result)
        rec.end()
        assert rec.err() is None

        client.shutdown()

        assert len(servicer.requests) == 1
        request = servicer.requests[0]
        assert len(request.generations) == 1
        _assert_generation_proto_payload(request.generations[0])
    finally:
        grpc_server.stop(grace=0)


def test_sdk_generation_auth_tenant_over_http() -> None:
    captured_headers: list[dict[str, str]] = []

    class _Handler(BaseHTTPRequestHandler):
        def do_POST(self):  # noqa: N802
            captured_headers.append({k.lower(): v for k, v in self.headers.items()})
            self.send_response(202)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(b'{"results":[{"generation_id":"gen-fixture-1","accepted":true}]}')

        def log_message(self, _format, *_args):  # noqa: A003
            return

    server = HTTPServer(("127.0.0.1", 0), _Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()

    client = _new_client(
        GenerationExportConfig(
            protocol="http",
            endpoint=f"http://127.0.0.1:{server.server_address[1]}/api/v1/generations:export",
            auth=AuthConfig(mode="tenant", tenant_id="tenant-a"),
            batch_size=1,
            flush_interval=timedelta(seconds=1),
            max_retries=1,
            initial_backoff=timedelta(milliseconds=1),
            max_backoff=timedelta(milliseconds=10),
        )
    )

    try:
        start, result = _payload_fixture()
        rec = client.start_generation(start)
        rec.set_result(result)
        rec.end()
        assert rec.err() is None
        client.shutdown()

        assert len(captured_headers) >= 1
        assert all(headers.get("x-scope-orgid") == "tenant-a" for headers in captured_headers)
    finally:
        server.shutdown()
        server.server_close()


_DEFAULT_USER_AGENT = f"sigil-sdk-python/{SDK_VERSION}"
_OVERRIDE_USER_AGENT = f"sigil-plugin-langgraph/1.2.3 {_DEFAULT_USER_AGENT}"

# A non-blank caller User-Agent wins; a blank or whitespace-only one must not
# blank out the default. None exercises the no-header path. HTTP and gRPC must
# resolve identically.
_USER_AGENT_CASES = [
    pytest.param(None, _DEFAULT_USER_AGENT, id="no-header"),
    pytest.param({"User-Agent": _OVERRIDE_USER_AGENT}, _OVERRIDE_USER_AGENT, id="override"),
    pytest.param({"User-Agent": ""}, _DEFAULT_USER_AGENT, id="empty"),
    pytest.param({"User-Agent": "   "}, _DEFAULT_USER_AGENT, id="whitespace"),
]


@pytest.mark.parametrize(("headers", "expected_user_agent"), _USER_AGENT_CASES)
def test_sdk_generation_user_agent_over_http(headers, expected_user_agent) -> None:
    captured_headers: list[dict[str, str]] = []

    class _Handler(BaseHTTPRequestHandler):
        def do_POST(self):  # noqa: N802
            captured_headers.append({k.lower(): v for k, v in self.headers.items()})
            self.send_response(202)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(b'{"results":[{"generation_id":"gen-fixture-1","accepted":true}]}')

        def log_message(self, _format, *_args):  # noqa: A003
            return

    server = HTTPServer(("127.0.0.1", 0), _Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()

    client = _new_client(
        GenerationExportConfig(
            protocol="http",
            endpoint=f"http://127.0.0.1:{server.server_address[1]}/api/v1/generations:export",
            headers=headers,
            batch_size=1,
            flush_interval=timedelta(seconds=1),
            max_retries=1,
            initial_backoff=timedelta(milliseconds=1),
            max_backoff=timedelta(milliseconds=10),
        )
    )

    try:
        start, result = _payload_fixture()
        rec = client.start_generation(start)
        rec.set_result(result)
        rec.end()
        assert rec.err() is None
        client.shutdown()

        assert len(captured_headers) >= 1
        assert captured_headers[0].get("user-agent") == expected_user_agent
    finally:
        server.shutdown()
        server.server_close()


@pytest.mark.parametrize(("headers", "expected_user_agent"), _USER_AGENT_CASES)
def test_sdk_generation_user_agent_over_grpc(headers, expected_user_agent) -> None:
    servicer = _CapturingGenerationServicer()
    grpc_server = grpc.server(thread_pool=__import__("concurrent.futures").futures.ThreadPoolExecutor(max_workers=2))
    sigil_pb2_grpc.add_GenerationIngestServiceServicer_to_server(servicer, grpc_server)

    sock = socket.socket()
    sock.bind(("127.0.0.1", 0))
    port = sock.getsockname()[1]
    sock.close()

    grpc_server.add_insecure_port(f"127.0.0.1:{port}")
    grpc_server.start()

    client = _new_client(
        GenerationExportConfig(
            protocol="grpc",
            endpoint=f"127.0.0.1:{port}",
            insecure=True,
            headers=headers,
            batch_size=1,
            flush_interval=timedelta(seconds=1),
            max_retries=1,
            initial_backoff=timedelta(milliseconds=1),
            max_backoff=timedelta(milliseconds=10),
        )
    )

    try:
        start, result = _payload_fixture()
        rec = client.start_generation(start)
        rec.set_result(result)
        rec.end()
        assert rec.err() is None
        client.shutdown()

        assert len(servicer.metadata) == 1
        # grpc appends its own token after ours, so compare the first token.
        user_agent = servicer.metadata[0].get("user-agent", "")
        assert user_agent.split(" ", 1)[0] == expected_user_agent.split(" ", 1)[0]
    finally:
        grpc_server.stop(grace=0)


def test_sdk_generation_auth_bearer_over_grpc_with_header_override() -> None:
    servicer = _CapturingGenerationServicer()
    grpc_server = grpc.server(thread_pool=__import__("concurrent.futures").futures.ThreadPoolExecutor(max_workers=2))
    sigil_pb2_grpc.add_GenerationIngestServiceServicer_to_server(servicer, grpc_server)

    sock = socket.socket()
    sock.bind(("127.0.0.1", 0))
    port = sock.getsockname()[1]
    sock.close()

    grpc_server.add_insecure_port(f"127.0.0.1:{port}")
    grpc_server.start()

    client = _new_client(
        GenerationExportConfig(
            protocol="grpc",
            endpoint=f"127.0.0.1:{port}",
            insecure=True,
            headers={"authorization": "Bearer override-token"},
            auth=AuthConfig(mode="bearer", bearer_token="should-not-win"),
            batch_size=1,
            flush_interval=timedelta(seconds=1),
            max_retries=1,
            initial_backoff=timedelta(milliseconds=1),
            max_backoff=timedelta(milliseconds=10),
        )
    )

    try:
        start, result = _payload_fixture()
        rec = client.start_generation(start)
        rec.set_result(result)
        rec.end()
        assert rec.err() is None
        client.shutdown()

        assert len(servicer.requests) == 1
        assert len(servicer.metadata) == 1
        assert servicer.metadata[0].get("authorization") == "Bearer override-token"
    finally:
        grpc_server.stop(grace=0)


def test_grpc_metadata_keys_are_lowercased() -> None:
    """Mixed-case header keys must be lowercased for gRPC metadata (grpcio rejects uppercase)."""
    servicer = _CapturingGenerationServicer()
    grpc_server = grpc.server(thread_pool=__import__("concurrent.futures").futures.ThreadPoolExecutor(max_workers=2))
    sigil_pb2_grpc.add_GenerationIngestServiceServicer_to_server(servicer, grpc_server)

    sock = socket.socket()
    sock.bind(("127.0.0.1", 0))
    port = sock.getsockname()[1]
    sock.close()

    grpc_server.add_insecure_port(f"127.0.0.1:{port}")
    grpc_server.start()

    client = _new_client(
        GenerationExportConfig(
            protocol="grpc",
            endpoint=f"127.0.0.1:{port}",
            insecure=True,
            auth=AuthConfig(mode="tenant", tenant_id="12345"),
            batch_size=1,
            flush_interval=timedelta(seconds=1),
            max_retries=1,
            initial_backoff=timedelta(milliseconds=1),
            max_backoff=timedelta(milliseconds=10),
        )
    )

    try:
        start, result = _payload_fixture()
        rec = client.start_generation(start)
        rec.set_result(result)
        rec.end()
        assert rec.err() is None
        client.shutdown()

        assert len(servicer.metadata) == 1
        meta = servicer.metadata[0]
        assert meta.get("x-scope-orgid") == "12345"
        assert not any(k != k.lower() for k in meta)
    finally:
        grpc_server.stop(grace=0)


def _assert_generation_json_payload(generation: dict[str, Any]) -> None:
    assert generation["id"] == "gen-fixture-1"
    assert generation["conversation_id"] == "conv-fixture-1"
    assert generation["operation_name"] == "streamText"
    assert generation["mode"] == "GENERATION_MODE_STREAM"

    trace_id = generation["trace_id"]
    span_id = generation["span_id"]
    assert isinstance(trace_id, str) and len(trace_id) == 32
    assert isinstance(span_id, str) and len(span_id) == 16

    model = generation["model"]
    assert isinstance(model, dict)
    assert model["provider"] == "anthropic"
    assert model["name"] == "claude-sonnet-4-5"

    assert generation["response_id"] == "resp-fixture"
    assert generation["response_model"] == "claude-sonnet-4-5-20260201"
    assert generation["system_prompt"] == "be concise"
    assert _proto_json_int(generation["max_tokens"]) == 256
    assert generation["temperature"] == 0.25
    assert generation["top_p"] == 0.95
    assert generation["tool_choice"] == "required"
    assert generation["thinking_enabled"] is False

    input_messages = generation["input"]
    assert isinstance(input_messages, list) and len(input_messages) == 1
    user_message = input_messages[0]
    assert user_message["role"] == "MESSAGE_ROLE_USER"
    assert user_message["parts"][0]["text"] == "hello"

    output_messages = generation["output"]
    assert isinstance(output_messages, list) and len(output_messages) == 2

    assistant_message = output_messages[0]
    assert assistant_message["role"] == "MESSAGE_ROLE_ASSISTANT"
    assert assistant_message["parts"][0]["thinking"] == "think"
    tool_call = assistant_message["parts"][1]["tool_call"]
    assert tool_call["id"] == "tool-call-1"
    assert tool_call["name"] == "weather"
    assert _decode_proto_json_bytes(tool_call["input_json"]) == b'{"city":"Paris"}'

    tool_message = output_messages[1]
    assert tool_message["role"] == "MESSAGE_ROLE_TOOL"
    tool_result = tool_message["parts"][0]["tool_result"]
    assert tool_result["tool_call_id"] == "tool-call-1"
    assert tool_result["name"] == "weather"
    assert tool_result["content"] == "18C"
    assert _decode_proto_json_bytes(tool_result["content_json"]) == b'{"temp_c":18}'

    tools = generation["tools"]
    assert isinstance(tools, list) and len(tools) == 1
    assert tools[0]["name"] == "weather"
    assert tools[0]["description"] == "Get weather"
    assert tools[0]["type"] == "function"
    assert _decode_proto_json_bytes(tools[0]["input_schema_json"]) == b'{"type":"object"}'
    assert tools[0]["deferred"] is True

    usage = generation["usage"]
    assert _proto_json_int(usage["input_tokens"]) == 120
    assert _proto_json_int(usage["output_tokens"]) == 42
    assert _proto_json_int(usage["total_tokens"]) == 162
    assert _proto_json_int(usage["cache_read_input_tokens"]) == 8
    assert _proto_json_int(usage["cache_write_input_tokens"]) == 4
    assert _proto_json_int(usage["reasoning_tokens"]) == 5
    assert _proto_json_int(usage.get("cache_creation_input_tokens", 0)) == 0

    assert generation["stop_reason"] == "end_turn"
    assert generation["started_at"] == "2026-02-11T12:00:00Z"
    assert generation["completed_at"] == "2026-02-11T12:00:01Z"
    assert generation["tags"] == {"env": "test", "suite": "transport"}

    metadata = generation["metadata"]
    assert metadata["seed"] in (1, 1.0)
    assert metadata["nested"] == {"ok": True}
    assert metadata["sigil.sdk.name"] == "sdk-python"

    artifacts = generation["raw_artifacts"]
    assert isinstance(artifacts, list) and len(artifacts) == 2

    request_artifact = artifacts[0]
    assert request_artifact["kind"] == "ARTIFACT_KIND_REQUEST"
    assert request_artifact["name"] == "request"
    assert request_artifact["content_type"] == "application/json"
    assert _decode_proto_json_bytes(request_artifact["payload"]) == b'{"request":true}'
    assert request_artifact["record_id"] == "rec-1"
    assert request_artifact["uri"] == "sigil://artifact/1"

    provider_event_artifact = artifacts[1]
    assert provider_event_artifact["kind"] == "ARTIFACT_KIND_PROVIDER_EVENT"
    assert provider_event_artifact["name"] == "event"
    assert provider_event_artifact["content_type"] == "application/json"
    assert _decode_proto_json_bytes(provider_event_artifact["payload"]) == b'{"event":true}'

    assert generation["agent_name"] == "agent-fixture"
    assert generation["agent_version"] == "v1.2.3"


def _assert_generation_proto_payload(generation: sigil_pb2.Generation) -> None:
    assert generation.id == "gen-fixture-1"
    assert generation.conversation_id == "conv-fixture-1"
    assert generation.operation_name == "streamText"
    assert generation.mode == sigil_pb2.GENERATION_MODE_STREAM

    assert len(generation.trace_id) == 32
    assert len(generation.span_id) == 16

    assert generation.model.provider == "anthropic"
    assert generation.model.name == "claude-sonnet-4-5"

    assert generation.response_id == "resp-fixture"
    assert generation.response_model == "claude-sonnet-4-5-20260201"
    assert generation.system_prompt == "be concise"
    assert generation.max_tokens == 256
    assert generation.temperature == 0.25
    assert generation.top_p == 0.95
    assert generation.tool_choice == "required"
    assert generation.thinking_enabled is False

    assert len(generation.input) == 1
    assert generation.input[0].role == sigil_pb2.MESSAGE_ROLE_USER
    assert len(generation.input[0].parts) == 1
    assert generation.input[0].parts[0].text == "hello"

    assert len(generation.output) == 2

    assert generation.output[0].role == sigil_pb2.MESSAGE_ROLE_ASSISTANT
    assert len(generation.output[0].parts) == 2
    assert generation.output[0].parts[0].thinking == "think"
    assert generation.output[0].parts[1].tool_call.id == "tool-call-1"
    assert generation.output[0].parts[1].tool_call.name == "weather"
    assert generation.output[0].parts[1].tool_call.input_json == b'{"city":"Paris"}'

    assert generation.output[1].role == sigil_pb2.MESSAGE_ROLE_TOOL
    assert len(generation.output[1].parts) == 1
    assert generation.output[1].parts[0].tool_result.tool_call_id == "tool-call-1"
    assert generation.output[1].parts[0].tool_result.name == "weather"
    assert generation.output[1].parts[0].tool_result.content == "18C"
    assert generation.output[1].parts[0].tool_result.content_json == b'{"temp_c":18}'

    assert len(generation.tools) == 1
    assert generation.tools[0].name == "weather"
    assert generation.tools[0].description == "Get weather"
    assert generation.tools[0].type == "function"
    assert generation.tools[0].input_schema_json == b'{"type":"object"}'
    assert generation.tools[0].deferred is True

    assert generation.usage.input_tokens == 120
    assert generation.usage.output_tokens == 42
    assert generation.usage.total_tokens == 162
    assert generation.usage.cache_read_input_tokens == 8
    assert generation.usage.cache_write_input_tokens == 4
    assert generation.usage.reasoning_tokens == 5

    assert generation.stop_reason == "end_turn"
    assert generation.started_at.ToDatetime(tzinfo=timezone.utc) == datetime(2026, 2, 11, 12, 0, tzinfo=timezone.utc)
    assert generation.completed_at.ToDatetime(tzinfo=timezone.utc) == datetime(
        2026, 2, 11, 12, 0, 1, tzinfo=timezone.utc
    )
    assert dict(generation.tags) == {"env": "test", "suite": "transport"}

    assert generation.metadata["seed"] in (1, 1.0)
    assert generation.metadata["nested"] == {"ok": True}
    assert generation.metadata["sigil.sdk.name"] == "sdk-python"

    assert len(generation.raw_artifacts) == 2

    request_artifact = generation.raw_artifacts[0]
    assert request_artifact.kind == sigil_pb2.ARTIFACT_KIND_REQUEST
    assert request_artifact.name == "request"
    assert request_artifact.content_type == "application/json"
    assert request_artifact.payload == b'{"request":true}'
    assert request_artifact.record_id == "rec-1"
    assert request_artifact.uri == "sigil://artifact/1"

    provider_event_artifact = generation.raw_artifacts[1]
    assert provider_event_artifact.kind == sigil_pb2.ARTIFACT_KIND_PROVIDER_EVENT
    assert provider_event_artifact.name == "event"
    assert provider_event_artifact.content_type == "application/json"
    assert provider_event_artifact.payload == b'{"event":true}'

    assert generation.agent_name == "agent-fixture"
    assert generation.agent_version == "v1.2.3"


def _decode_proto_json_bytes(value: Any) -> bytes:
    assert isinstance(value, str)
    return base64.b64decode(value)


def _proto_json_int(value: Any) -> int:
    if isinstance(value, int):
        return value
    if isinstance(value, str):
        return int(value)
    raise AssertionError(f"expected int-compatible value, got {type(value)!r}")


def _new_client(generation_export: GenerationExportConfig) -> Client:
    trace_provider = TracerProvider()
    return Client(
        ClientConfig(
            generation_export=generation_export,
            tracer=trace_provider.get_tracer("sigil-sdk-python-transport-test"),
        )
    )


def _payload_fixture() -> tuple[GenerationStart, Generation]:
    started_at = datetime(2026, 2, 11, 12, 0, tzinfo=timezone.utc)
    completed_at = started_at + timedelta(seconds=1)

    start = GenerationStart(
        id="gen-fixture-1",
        conversation_id="conv-fixture-1",
        agent_name="agent-fixture",
        agent_version="v1.2.3",
        mode=GenerationMode.STREAM,
        operation_name="streamText",
        model=ModelRef(provider="anthropic", name="claude-sonnet-4-5"),
        system_prompt="be concise",
        max_tokens=512,
        temperature=0.7,
        top_p=0.9,
        tool_choice="auto",
        thinking_enabled=True,
        tools=[
            ToolDefinition(
                name="weather",
                description="Get weather",
                type="function",
                input_schema_json=b'{"type":"object"}',
                deferred=True,
            )
        ],
        tags={"env": "test"},
        metadata={"seed": 1},
        started_at=started_at,
    )

    result = Generation(
        id=start.id,
        conversation_id=start.conversation_id,
        agent_name=start.agent_name,
        agent_version=start.agent_version,
        mode=start.mode,
        operation_name=start.operation_name,
        model=start.model,
        response_id="resp-fixture",
        response_model="claude-sonnet-4-5-20260201",
        system_prompt=start.system_prompt,
        max_tokens=256,
        temperature=0.25,
        top_p=0.95,
        tool_choice="required",
        thinking_enabled=False,
        input=[
            Message(
                role=MessageRole.USER,
                parts=[Part(kind=PartKind.TEXT, text="hello")],
            )
        ],
        output=[
            Message(
                role=MessageRole.ASSISTANT,
                parts=[
                    Part(kind=PartKind.THINKING, thinking="think"),
                    Part(
                        kind=PartKind.TOOL_CALL,
                        tool_call=ToolCall(
                            id="tool-call-1",
                            name="weather",
                            input_json=b'{"city":"Paris"}',
                        ),
                    ),
                ],
            ),
            Message(
                role=MessageRole.TOOL,
                parts=[
                    Part(
                        kind=PartKind.TOOL_RESULT,
                        tool_result=ToolResult(
                            tool_call_id="tool-call-1",
                            name="weather",
                            content="18C",
                            content_json=b'{"temp_c":18}',
                        ),
                    )
                ],
            ),
        ],
        tools=start.tools,
        usage=TokenUsage(
            input_tokens=120,
            output_tokens=42,
            total_tokens=162,
            cache_read_input_tokens=8,
            cache_write_input_tokens=4,
            reasoning_tokens=5,
        ),
        stop_reason="end_turn",
        started_at=started_at,
        completed_at=completed_at,
        tags={"env": "test", "suite": "transport"},
        metadata={"seed": 1, "nested": {"ok": True}},
        artifacts=[
            Artifact(
                kind=ArtifactKind.REQUEST,
                name="request",
                content_type="application/json",
                payload=b'{"request":true}',
                record_id="rec-1",
                uri="sigil://artifact/1",
            ),
            Artifact(
                kind=ArtifactKind.PROVIDER_EVENT,
                name="event",
                content_type="application/json",
                payload=b'{"event":true}',
            ),
        ],
    )
    return start, result
