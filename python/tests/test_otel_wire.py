from __future__ import annotations

import base64
import json
import os
from collections.abc import Iterator
from enum import Enum
from pathlib import Path
from typing import Any, cast

import pytest

pytest.importorskip("opentelemetry.util.genai")

from agento11y import Client, ClientShutdownError, EnqueueError, GenerationExportConfig, ValidationError
from agento11y.config import ClientConfig
from agento11y.models import (
    Artifact,
    ArtifactKind,
    ContentCaptureMode,
    Generation,
    GenerationMode,
    GenerationStart,
    Message,
    MessageRole,
    ModelRef,
    Part,
    PartKind,
    PartMetadata,
    TokenInputSemantics,
    TokenUsage,
    ToolCall,
    ToolDefinition,
    ToolResult,
)
from agento11y.otel_export import (
    INSTRUMENTATION_NAME,
    SCHEMA_URL,
    _encode_json_bytes,
    _encode_messages,
    _encode_part,
    _encode_tool_definitions,
    _encode_tool_response,
    build_otel_handler,
    generation_attributes,
    metric_attributes,
    operation_name,
    provider_name,
    start_invocation,
)
from agento11y.proto_mapping import _effective_version_digest, generation_to_proto
from opentelemetry.sdk.trace import ReadableSpan, TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
from opentelemetry.trace import SpanKind, get_current_span
from opentelemetry.util.genai.invocation import InferenceInvocation
from otel_receiver_probe import otlp_json, run_receiver, write_pair

_FIXTURE_DIR = Path(__file__).resolve().parents[2] / "go/agento11y/testdata/otlpwire"


class _FixtureMessageRole(str, Enum):
    SYSTEM = "system"
    USER = "user"
    ASSISTANT = "assistant"
    TOOL = "tool"


def _fixture_messages(items: list[dict[str, Any]]) -> list[Message]:
    messages: list[Message] = []
    for source in items:
        parts: list[Part] = []
        for source_part in source.get("parts", []):
            metadata = PartMetadata(**source_part.get("metadata", {}))
            if "tool_call" in source_part:
                call = dict(source_part["tool_call"])
                call["input_json"] = base64.b64decode(call.get("input_json", ""))
                part = Part(kind=PartKind.TOOL_CALL, tool_call=ToolCall(**call), metadata=metadata)
            elif "tool_result" in source_part:
                result = dict(source_part["tool_result"])
                result["content_json"] = base64.b64decode(result.get("content_json", ""))
                part = Part(kind=PartKind.TOOL_RESULT, tool_result=ToolResult(**result), metadata=metadata)
            elif "thinking" in source_part:
                part = Part(kind=PartKind.THINKING, thinking=source_part["thinking"], metadata=metadata)
            else:
                part = Part(kind=PartKind.TEXT, text=source_part.get("text", ""), metadata=metadata)
            parts.append(part)
        role = cast(MessageRole, _FixtureMessageRole(source["role"].removeprefix("MESSAGE_ROLE_").lower()))
        messages.append(Message(role=role, name=source.get("name", ""), parts=parts))
    return messages


def _fixture_generation(name: str) -> Generation:
    source = json.loads((_FIXTURE_DIR / f"{name}.generation.json").read_text())
    generation = Generation(
        id=source["id"],
        conversation_id=source.get("conversation_id", ""),
        operation_name=source.get("operation_name", ""),
        mode=GenerationMode.STREAM if source["mode"] == "GENERATION_MODE_STREAM" else GenerationMode.SYNC,
        model=ModelRef(**source["model"]),
        response_id=source.get("response_id", ""),
        response_model=source.get("response_model", ""),
        system_prompt=source.get("system_prompt", ""),
        input=_fixture_messages(source.get("input", [])),
        output=_fixture_messages(source.get("output", [])),
        usage=TokenUsage(**{key: int(value) for key, value in source.get("usage", {}).items()}),
        stop_reason=source.get("stop_reason", ""),
        agent_name=source.get("agent_name", ""),
        agent_version=source.get("agent_version", ""),
        parent_generation_ids=source.get("parent_generation_ids", []),
        effective_version=source.get("effective_version", ""),
        tags=source.get("tags", {}),
        metadata=source.get("metadata", {}),
    )
    for tool in source.get("tools", []):
        fields = dict(tool)
        fields["input_schema_json"] = base64.b64decode(fields.get("input_schema_json", ""))
        generation.tools.append(ToolDefinition(**fields))
    return generation


def _fixture_span_attributes(name: str) -> dict[str, Any]:
    source = json.loads((_FIXTURE_DIR / f"{name}.span.json").read_text())
    attributes: dict[str, Any] = {}
    for attribute in source["attributes"]:
        value = attribute["value"]
        if "string_value" in value:
            decoded = value["string_value"]
        elif "bool_value" in value:
            decoded = value["bool_value"]
        elif "int_value" in value:
            decoded = int(value["int_value"])
        elif "double_value" in value:
            decoded = value["double_value"]
        else:
            decoded = tuple(item["string_value"] for item in value["array_value"]["values"])
        attributes[attribute["key"]] = decoded
    return attributes


@pytest.mark.parametrize("name", ["openai_sync", "anthropic_stream", "gemini_sync"])
def test_generation_attributes_match_go_fixtures(name: str) -> None:
    generation = _fixture_generation(name)
    expected = _fixture_span_attributes(name)
    expected["gen_ai.operation.name"] = "chat"
    expected["gen_ai.provider.name"] = provider_name(generation.model.provider)
    additions = {"agento11y.record": "true", "agento11y.sdk.name": "sdk-python"}
    attrs = generation_attributes(generation, ContentCaptureMode.FULL)
    assert set(attrs) == set(expected) | set(additions)
    for key, value in (expected | additions).items():
        if key == "agento11y.generation.metadata":
            assert json.loads(attrs[key]) == json.loads(value)
        else:
            assert type(attrs[key]) is type(value)
            assert attrs[key] == value


@pytest.fixture
def span_recorder(monkeypatch: pytest.MonkeyPatch) -> Iterator[tuple[TracerProvider, InMemorySpanExporter]]:
    monkeypatch.setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "true")
    exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(exporter))
    yield provider, exporter
    provider.shutdown()


@pytest.mark.parametrize("capture_env", ["NO_CONTENT", "SPAN_ONLY", "EVENT_ONLY", "SPAN_AND_EVENT"])
def test_public_invocation_suspends_and_keeps_content_collections_empty(
    capture_env: str,
    monkeypatch: pytest.MonkeyPatch,
    span_recorder: tuple[TracerProvider, InMemorySpanExporter],
) -> None:
    monkeypatch.setenv("OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT", capture_env)
    monkeypatch.setenv("OTEL_INSTRUMENTATION_GENAI_EMIT_EVENT", "true")
    provider, exporter = span_recorder
    handler = build_otel_handler(ClientConfig(tracer_provider=provider))
    generation = _fixture_generation("gemini_sync")
    with provider.get_tracer("parent").start_as_current_span("parent") as parent:
        invocation = start_invocation(handler, generation)
        assert isinstance(invocation, InferenceInvocation)
        assert get_current_span() is parent
        assert invocation.conversation_id == generation.conversation_id
        assert invocation.input_messages == []
        assert invocation.output_messages == []
        assert invocation.system_instruction == []
        assert not invocation.tool_definitions
        assert not invocation.finish_reasons
        assert not invocation.stop_sequences
        assert not invocation.prompt_variables
        invocation.attributes.update(generation_attributes(generation, ContentCaptureMode.FULL))
        assert invocation.input_messages == []
        assert invocation.output_messages == []
        assert invocation.system_instruction == []
        assert not invocation.tool_definitions
        invocation.stop()
        assert get_current_span() is parent
    span = exporter.get_finished_spans()[0]
    assert span.parent == parent.get_span_context()
    assert span.name == "chat gemini-2.5-pro"
    assert span.kind == SpanKind.CLIENT
    assert span.instrumentation_scope.name == INSTRUMENTATION_NAME
    assert span.instrumentation_scope.version == ""
    assert span.instrumentation_scope.schema_url == SCHEMA_URL
    staged = generation_attributes(generation, ContentCaptureMode.FULL)
    utility_additions: set[str] = set()
    assert set(span.attributes) == set(staged) | utility_additions
    assert dict(span.attributes) == staged
    assert not span.events


@pytest.mark.parametrize(
    "provider, expected",
    [
        ("gemini", "gcp.gemini"),
        ("mistral", "mistral_ai"),
        ("moonshotai", "moonshot_ai"),
        ("vertex", "gcp.vertex_ai"),
        ("bedrock", "aws.bedrock"),
        ("azure-openai", "azure.ai.openai"),
        ("azure-ai-inference", "azure.ai.inference"),
        ("watsonx", "ibm.watsonx.ai"),
        ("x-ai", "x_ai"),
        ("openai", "openai"),
        ("custom", "custom"),
        ("", ""),
    ],
)
def test_provider_name(provider: str, expected: str) -> None:
    assert provider_name(provider) == expected


@pytest.mark.parametrize(
    "operation, expected",
    [
        ("", "chat"),
        ("generateText", "chat"),
        ("streamText", "chat"),
        ("custom", "custom"),
    ],
)
def test_operation_name(operation: str, expected: str) -> None:
    assert operation_name(operation) == expected


def test_start_invocation_maps_seed_and_immediately_suspends() -> None:
    calls: list[object] = []

    class Invocation:
        def suspend(self) -> None:
            calls.append("suspend")

    invocation = Invocation()

    class Handler:
        def inference(self, provider: str, **kwargs: object) -> Invocation:
            calls.append((provider, kwargs))
            return invocation

    seed = GenerationStart(model=ModelRef("bedrock", "claude"), operation_name="streamText", conversation_id="conv")
    assert start_invocation(Handler(), seed) is invocation
    assert calls == [
        ("aws.bedrock", {"request_model": "claude", "operation_name": "chat", "conversation_id": "conv"}),
        "suspend",
    ]


@pytest.mark.parametrize(
    "raw",
    [
        b' {"a":1}',
        b'{"a": 1}',
        b'{"a":1}\n',
        b'{"a":1,"a":2}',
        b'{"nested":{"a":1,"a":2}}',
        b'{"n":1e3}',
        b'{"n":1.00}',
        b'{"n":-0}',
        b'{"n":1E+10}',
        b'{"n":1e999}',
        b'{"path":"a\\/b"}',
        b'{"city":"\\u00fcrich"}',
        b'"\\ud800"',
        b"null",
        b" null ",
        b"NaN",
        b"Infinity",
        b"-Infinity",
        b'{"n":NaN}',
        b"not-json",
        b'"\xff"',
        b'\xef\xbb\xbf{"a":1}',
        b'{"a":1,}',
    ],
)
def test_json_bytes_preserve_noncanonical_and_invalid_payloads(raw: bytes) -> None:
    value, encoded = _encode_json_bytes(raw)
    assert value is None
    assert base64.b64decode(encoded) == raw


@pytest.mark.parametrize(
    "raw",
    [
        b'{"city":"Paris"}',
        b'{"n":1000.0}',
        b"0",
        b"false",
        b"[]",
        b'"text"',
        '{"city":"ürich"}'.encode(),
        b'{"text":"<>&"}',
        b'{"text":"a b\\nc"}',
    ],
)
def test_json_bytes_embed_only_actual_dump_fixed_points(raw: bytes) -> None:
    value, encoded = _encode_json_bytes(raw)
    assert encoded == ""
    assert json.dumps(value, ensure_ascii=False, separators=(",", ":"), allow_nan=False).encode() == raw


def test_json_bytes_empty_payload() -> None:
    assert _encode_json_bytes(b"") == (None, "")


def test_json_bytes_preserve_deeply_nested_documents() -> None:
    raw = b"[" * 2000 + b"0" + b"]" * 2000
    value, encoded = _encode_json_bytes(raw)
    if encoded:
        assert value is None
        assert base64.b64decode(encoded) == raw
    else:
        assert json.dumps(value, separators=(",", ":"), allow_nan=False).encode() == raw


@pytest.mark.parametrize(
    "raw",
    [
        b"",
        b'{"a":1}',
        b'{"a":1,"a":2}',
        b'{"a":1e3}',
        b"null",
        b"NaN",
        b'"\\xff"',
        b'"\\u00fc"',
        '"ü"'.encode(),
        b' {"a":1}',
    ],
)
def test_generation_wire_retains_argument_and_schema_bytes(raw: bytes) -> None:
    generation = Generation(
        input=[
            Message(
                role=MessageRole.ASSISTANT,
                parts=[
                    Part(kind=PartKind.TOOL_CALL, tool_call=ToolCall("tool", "call", raw)),
                ],
            )
        ],
        tools=[ToolDefinition(name="tool", input_schema_json=raw)],
    )
    attrs = generation_attributes(generation, ContentCaptureMode.FULL)
    call = json.loads(attrs["gen_ai.input.messages"])[0]["parts"][0]
    tool = json.loads(attrs["gen_ai.tool.definitions"])[0]
    for item, field, extension in (
        (call, "arguments", "agento11y.arguments_b64"),
        (tool, "parameters", "agento11y.input_schema_b64"),
    ):
        if extension in item:
            restored = base64.b64decode(item[extension])
            assert field not in item
        elif field in item:
            restored = json.dumps(item[field], ensure_ascii=False, separators=(",", ":"), allow_nan=False).encode()
        else:
            restored = b""
        assert restored == raw


@pytest.mark.parametrize(
    "content, raw",
    [
        ("plain text", b'{"a":1}'),
        ("", b'"structured string"'),
        ("", b"null"),
        ("", b'{"a":1,"a":2}'),
        ("text", b"\xff"),
        ("", b' {"a":1}'),
    ],
)
def test_tool_response_preserves_text_and_exact_json(content: str, raw: bytes) -> None:
    response, encoded = _encode_tool_response(content, raw)
    assert response == content
    assert base64.b64decode(encoded) == raw
    part = _encode_part(Part(kind=PartKind.TOOL_RESULT, tool_result=ToolResult(content=content, content_json=raw)))
    wire = json.loads(json.dumps(part))
    assert wire["response"] == content
    assert base64.b64decode(wire["agento11y.response_b64"]) == raw
    assert "content" not in wire


@pytest.mark.parametrize("raw", [b'{"a":1}', b"false", b"0", b"[]"])
def test_tool_response_embeds_structured_json_when_text_is_empty(raw: bytes) -> None:
    assert _encode_tool_response("", raw) == (json.loads(raw), "")


def test_part_extensions_and_tool_schema_bytes() -> None:
    metadata = PartMetadata(provider_type="provider-part")
    assert _encode_part(Part(kind=PartKind.THINKING, thinking="think", metadata=metadata)) == {
        "type": "reasoning",
        "content": "think",
        "agento11y.provider_type": "provider-part",
    }
    result = _encode_part(
        Part(
            kind=PartKind.TOOL_RESULT,
            metadata=metadata,
            tool_result=ToolResult(
                tool_call_id="call",
                name="tool",
                content="text",
                is_error=True,
            ),
        )
    )
    assert result == {
        "type": "tool_call_response",
        "id": "call",
        "response": "text",
        "agento11y.tool_name": "tool",
        "agento11y.is_error": True,
        "agento11y.provider_type": "provider-part",
    }
    raw = b'{"n":1e3}'
    call = _encode_part(Part(kind=PartKind.TOOL_CALL, metadata=metadata, tool_call=ToolCall("tool", "call", raw)))
    assert call == {
        "type": "tool_call",
        "id": "call",
        "name": "tool",
        "agento11y.arguments_b64": base64.b64encode(raw).decode(),
        "agento11y.provider_type": "provider-part",
    }
    tool = _encode_tool_definitions(
        [ToolDefinition(name="tool", description="desc", input_schema_json=raw, deferred=True)]
    )[0]
    assert tool == {
        "type": "function",
        "name": "tool",
        "description": "desc",
        "agento11y.deferred": True,
        "agento11y.input_schema_b64": base64.b64encode(raw).decode(),
    }
    assert _encode_part(Part(kind=PartKind.TOOL_CALL)) is None
    assert _encode_part(Part(kind=PartKind.TOOL_RESULT)) is None


def test_output_message_has_required_empty_finish_reason() -> None:
    messages = [Message(role=MessageRole.ASSISTANT, name="assistant", parts=[])]
    assert _encode_messages(messages) == [{"role": "assistant", "name": "assistant", "parts": []}]
    assert _encode_messages(messages, "") == [
        {"role": "assistant", "name": "assistant", "parts": [], "finish_reason": ""}
    ]


@pytest.mark.parametrize("mode", list(ContentCaptureMode))
def test_content_capture_and_reserved_metadata_mirrors(mode: ContentCaptureMode) -> None:
    generation = _fixture_generation("openai_sync")
    generation.conversation_title = "sanitized title"
    generation.call_error = "unsafe direct error"
    generation.metadata = {
        "agento11y.conversation.title": "unsafe title",
        "sigil.conversation.title": "legacy unsafe title",
        "call_error": "unsafe error",
        "custom": "preserved",
    }
    generation.artifacts = [
        Artifact(
            kind=ArtifactKind.RESPONSE,
            name="response",
            content_type="application/json",
            payload=b"\xff",
            record_id="record",
            uri="uri",
        )
    ]
    generation.tools = [ToolDefinition(name="tool")]
    attrs = generation_attributes(generation, mode)
    assert json.loads(attrs["agento11y.generation.metadata"]) == {"custom": "preserved"}
    assert "unsafe" not in json.dumps(attrs)
    keys = {
        "agento11y.conversation.title",
        "agento11y.generation.raw_artifacts",
        "gen_ai.input.messages",
        "gen_ai.output.messages",
        "gen_ai.system_instructions",
        "gen_ai.tool.definitions",
    }
    if mode == ContentCaptureMode.METADATA_ONLY:
        assert not keys & attrs.keys()
    else:
        assert keys <= attrs.keys()
        assert attrs["agento11y.conversation.title"] == "sanitized title"
        assert json.loads(attrs["agento11y.generation.raw_artifacts"]) == [
            {
                "kind": "response",
                "name": "response",
                "content_type": "application/json",
                "payload_b64": "/w==",
                "record_id": "record",
                "uri": "uri",
            }
        ]
    generation.conversation_title = ""
    assert "agento11y.conversation.title" not in generation_attributes(generation, mode)


@pytest.mark.parametrize("value", [object(), float("nan"), float("inf"), float("-inf"), {"nested": float("nan")}])
@pytest.mark.parametrize("mode", [ContentCaptureMode.FULL, ContentCaptureMode.METADATA_ONLY])
def test_metadata_encoding_failure_rejects_staging(value: object, mode: ContentCaptureMode) -> None:
    generation = Generation(id="gen", metadata={"custom": value})
    with pytest.raises((TypeError, ValueError)):
        generation_attributes(generation, mode)
    assert generation.metadata["custom"] is value


def test_usage_optional_parameters_and_tags_have_explicit_allowlist() -> None:
    generation = Generation(
        id="gen",
        user_id="user",
        model=ModelRef("gemini", "model"),
        operation_name="custom",
        usage=TokenUsage(
            input_tokens=10,
            output_tokens=2,
            total_tokens=12,
            cache_read_input_tokens=3,
            cache_write_input_tokens=4,
            reasoning_tokens=1,
            input_semantics=TokenInputSemantics.INCLUSIVE,
        ),
        max_tokens=0,
        temperature=0.0,
        top_p=0.0,
        tool_choice=" auto ",
        thinking_enabled=False,
        metadata={"agento11y.gen_ai.request.thinking.budget_tokens": " 2048 "},
        tags={"export_only": "tag"},
    )
    attrs = generation_attributes(generation, ContentCaptureMode.FULL, {" client ": " value ", " ": "ignored"})
    expected = {
        "agento11y.sdk.name": "sdk-python",
        "agento11y.record": "true",
        "agento11y.generation.id": "gen",
        "gen_ai.operation.name": "custom",
        "gen_ai.provider.name": "gcp.gemini",
        "gen_ai.request.model": "model",
        "user.id": "user",
        "gen_ai.usage.input_tokens": 10,
        "gen_ai.usage.output_tokens": 2,
        "gen_ai.usage.cache_read.input_tokens": 3,
        "gen_ai.usage.cache_creation.input_tokens": 4,
        "gen_ai.usage.cache_write.input_tokens": 4,
        "gen_ai.usage.reasoning.output_tokens": 1,
        "agento11y.gen_ai.usage.total_tokens": 12,
        "gen_ai.token.semantics": "inclusive",
        "gen_ai.request.max_tokens": 0,
        "gen_ai.request.temperature": 0.0,
        "gen_ai.request.top_p": 0.0,
        "agento11y.gen_ai.request.tool_choice": "auto",
        "agento11y.gen_ai.request.thinking.enabled": False,
        "agento11y.gen_ai.request.thinking.budget_tokens": 2048,
        "agento11y.generation.metadata": '{"agento11y.gen_ai.request.thinking.budget_tokens":" 2048 "}',
        "agento11y.generation.tags": '{"export_only":"tag"}',
        "agento11y.tag.client": "value",
    }
    assert attrs == expected
    assert generation.tool_choice == " auto "
    assert generation.tags == {"export_only": "tag"}


def test_unreported_usage_is_absent_and_reported_usage_keeps_zero_bucket() -> None:
    generation = Generation()
    assert not any("usage" in key for key in generation_attributes(generation, ContentCaptureMode.FULL))
    generation.usage.output_tokens = 2
    attrs = generation_attributes(generation, ContentCaptureMode.FULL)
    assert attrs["gen_ai.usage.input_tokens"] == 0
    assert attrs["gen_ai.usage.output_tokens"] == 2


def test_metric_attributes_only_use_dimensional_tags_and_sdk_dimensions() -> None:
    generation = Generation(
        id="gen",
        conversation_id="conv",
        agent_name="agent",
        agent_version="version",
        tags={"export_only": "tag"},
        usage=TokenUsage(input_semantics=TokenInputSemantics.INCLUSIVE),
    )
    assert metric_attributes(generation, {"client": "value"}, error_category="rate_limit") == {
        "agento11y.tag.client": "value",
        "error.category": "rate_limit",
        "gen_ai.token.semantics": "inclusive",
        "gen_ai.agent.name": "agent",
        "gen_ai.agent.version": "version",
    }
    assert metric_attributes(Generation()) == {}


@pytest.mark.parametrize("version", ["", " \t\n", "agent-build-42", " version ", "sha256:already-hashed"])
def test_effective_version_digest_matches_native_mapping(version: str) -> None:
    digest = _effective_version_digest(version)
    assert generation_to_proto(Generation(effective_version=version)).effective_version == digest
    attrs = generation_attributes(Generation(effective_version=version), ContentCaptureMode.FULL)
    assert attrs.get("agento11y.agent.effective_version", "") == digest


_RECEIVER_BYTE_CASES = [
    b' {"a":1}',
    b'{"a": 1}',
    b'{"a":1}\n',
    b'{"a":1,"a":2}',
    b'{"nested":{"a":1,"a":2}}',
    b'{"n":1e3}',
    b'{"n":1.00}',
    b'{"n":-0}',
    b'{"n":1E+10}',
    b'{"n":1e999}',
    b"9007199254740993",
    b'{"path":"a\\/b"}',
    b'{"city":"\\u00fcrich"}',
    b'"\\ud800"',
    b"null",
    b" null ",
    b"NaN",
    b"Infinity",
    b"-Infinity",
    b'{"n":NaN}',
    b"not-json",
    b'"\xff"',
    b'\xef\xbb\xbf{"a":1}',
    b'{"a":1,}',
    b"\x00\xff\x80",
    b'{"text":"<>&"}',
    b'{"text":"\\u003c\\u003e\\u0026"}',
    b'{"n":1000.0}',
    b"0",
    b"false",
    b"[]",
    b'"text"',
    '{"city":"ürich"}'.encode(),
    '{"text":"\u2028\u2029"}'.encode(),
]


def _probe_root(tmp_path: Path) -> Path:
    return Path(os.environ.get("PYTHON_OTLP_PROBE_DIR", str(tmp_path)))


def _wire_client(provider: TracerProvider, **export_options: Any) -> Client:
    return Client(
        ClientConfig(
            generation_export=GenerationExportConfig(protocol="otel", **export_options),
            tracer_provider=provider,
            content_capture=ContentCaptureMode.FULL,
            tags={"team": "receiver-proof"},
        )
    )


def _record_client_generation(
    client: Client,
    exporter: InMemorySpanExporter,
    generation: Generation,
) -> tuple[ReadableSpan, Generation]:
    before = len(exporter.get_finished_spans())
    parent_context = get_current_span().get_span_context()
    recorder = client.start_generation(GenerationStart(model=generation.model, id=generation.id))
    recorder.set_result(generation)
    recorder.end()
    recorder.end()
    assert recorder.err() is None
    assert len(exporter.get_finished_spans()) == before + 1
    span = exporter.get_finished_spans()[-1]
    assert span.parent == (parent_context if parent_context.is_valid else None)
    assert get_current_span().get_span_context() == parent_context
    normalized = recorder.last_generation
    assert normalized is not None
    assert dict(span.attributes) == generation_attributes(normalized, ContentCaptureMode.FULL, client._config.tags)
    assert span.kind == SpanKind.CLIENT
    assert span.instrumentation_scope.name == INSTRUMENTATION_NAME
    assert span.instrumentation_scope.version == ""
    assert span.instrumentation_scope.schema_url == SCHEMA_URL
    assert not span.events
    assert span.dropped_attributes == 0
    assert client._pending_generations == []
    assert client._generation_exporter is None
    return span, normalized


def _rich_generation() -> Generation:
    return Generation(
        id="rich-core",
        conversation_id="rich-conversation",
        conversation_title="title <>&",
        user_id="user-42",
        model=ModelRef("bedrock", "claude-test"),
        mode=GenerationMode.STREAM,
        operation_name="streamText",
        agent_name="agent",
        agent_version="v42",
        response_id="response-id",
        response_model="response-model",
        system_prompt="system <>&\nsecond line",
        stop_reason="tool_calls",
        max_tokens=0,
        temperature=0.0,
        top_p=0.0,
        tool_choice="auto",
        thinking_enabled=False,
        usage=TokenUsage(
            input_tokens=100,
            output_tokens=20,
            total_tokens=120,
            cache_read_input_tokens=30,
            cache_write_input_tokens=10,
            reasoning_tokens=5,
            input_semantics=TokenInputSemantics.INCLUSIVE,
        ),
        parent_generation_ids=["parent-1", "parent-2"],
        effective_version=" agent-build-42 ",
        tags={"request_id": "per-call", "html": "<>&"},
        metadata={
            "nested": {"number": 1.25, "null": None, "list": [True, "<>&"]},
            "agento11y.gen_ai.request.thinking.budget_tokens": 2048,
        },
        input=[
            Message(MessageRole.USER, [Part(PartKind.TEXT, text="question <>&")], name="user"),
            Message(
                MessageRole.ASSISTANT,
                [
                    Part(
                        PartKind.TOOL_CALL,
                        tool_call=ToolCall("tool", "call", b'{"n":1e3}'),
                        metadata=PartMetadata("provider-call"),
                    )
                ],
            ),
            Message(
                MessageRole.TOOL,
                [
                    Part(
                        PartKind.TOOL_RESULT,
                        tool_result=ToolResult("call", "tool", "text <>&", b"null", True),
                        metadata=PartMetadata("provider-result"),
                    )
                ],
            ),
        ],
        output=[
            Message(
                MessageRole.ASSISTANT,
                [
                    Part(PartKind.THINKING, thinking="reasoning <>&", metadata=PartMetadata("provider-thinking")),
                    Part(PartKind.TEXT, text="answer <>&", metadata=PartMetadata("provider-text")),
                ],
                name="assistant",
            )
        ],
        tools=[ToolDefinition("tool", "description <>&", "function", b'{"a":1,"a":2}', True)],
    )


def test_actual_python_spans_and_optional_local_receiver(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    span_recorder: tuple[TracerProvider, InMemorySpanExporter],
) -> None:
    root = _probe_root(tmp_path)
    directory = root / "compatible"
    directory.mkdir(parents=True, exist_ok=True)
    provider, exporter = span_recorder
    handler = build_otel_handler(ClientConfig(tracer_provider=provider))
    accepted = rejected = 0
    # Go's SYSTEM fixture role is absent from Python's public roles,
    # so this adapter proof skips Client validation.
    # Separate Client cases reject SYSTEM and accept valid-core fixtures.
    with provider.get_tracer("proof-parent").start_as_current_span("parent") as parent:
        for name in ("openai_sync", "anthropic_stream", "gemini_sync"):
            generation = _fixture_generation(name)
            invocation = start_invocation(handler, generation)
            attrs = generation_attributes(generation, ContentCaptureMode.FULL)
            invocation.attributes.update(attrs)
            invocation.stop()
            span = exporter.get_finished_spans()[-1]
            assert dict(span.attributes) == attrs
            assert span.parent == parent.get_span_context()
            assert get_current_span() is parent
            expected = json.loads((_FIXTURE_DIR / f"{name}.generation.json").read_text())
            if generation.effective_version:
                expected["effective_version"] = _effective_version_digest(generation.effective_version)
            write_pair(directory, f"adapter_{name}", span, expected)
            accepted += 1

        client = _wire_client(provider)
        try:
            for name in ("openai_sync", "anthropic_stream", "gemini_sync"):
                generation = _fixture_generation(name)
                if name == "openai_sync":
                    generation.input = [message for message in generation.input if message.role.value != "system"]
                span, normalized = _record_client_generation(client, exporter, generation)
                assert span.parent == parent.get_span_context()
                write_pair(directory, f"client_{name}_valid_core", span, normalized)
                accepted += 1
            span, normalized = _record_client_generation(client, exporter, _rich_generation())
            assert span.attributes["gen_ai.usage.cache_write.input_tokens"] == 10
            assert span.attributes["gen_ai.usage.cache_creation.input_tokens"] == 10
            assert span.attributes["gen_ai.token.semantics"] == "inclusive"
            assert span.attributes["user.id"] == "user-42"
            assert span.attributes["agento11y.agent.effective_version"] == _effective_version_digest("agent-build-42")
            write_pair(directory, "client_rich_core", span, normalized)
            accepted += 1
            for index, raw in enumerate(_RECEIVER_BYTE_CASES):
                for content in ("", "text <>&"):
                    generation = Generation(
                        id=f"bytes-{index}-{'both' if content else 'json'}",
                        model=ModelRef("openai", "test"),
                        input=[
                            Message(
                                MessageRole.ASSISTANT,
                                [Part(PartKind.TOOL_CALL, tool_call=ToolCall("tool", "call", raw))],
                            ),
                            Message(
                                MessageRole.TOOL,
                                [Part(PartKind.TOOL_RESULT, tool_result=ToolResult("call", "tool", content, raw))],
                            ),
                        ],
                        tools=[ToolDefinition("tool", type="function", input_schema_json=raw)],
                    )
                    span, normalized = _record_client_generation(client, exporter, generation)
                    write_pair(directory, generation.id, span, normalized)
                    accepted += 1
        finally:
            client.shutdown()

        for reason in ("normalization", "validation", "encoding", "mapping", "admission", "shutdown", "system"):
            client = _wire_client(provider, **({"payload_max_bytes": 1} if reason == "admission" else {}))
            before = len(exporter.get_finished_spans())
            recorder = client.start_generation(
                GenerationStart(
                    id=f"reject-{reason}",
                    model=ModelRef("openai", "test"),
                    conversation_id="rejected-conversation",
                    conversation_title="secret-title",
                )
            )
            generation = Generation(output=[Message(MessageRole.ASSISTANT, [Part(PartKind.TEXT, text="secret")])])
            if reason == "validation":
                generation.output[0].parts[0].text = ""
            elif reason == "encoding":
                generation.metadata["invalid"] = float("nan")
            elif reason == "system":
                generation = _fixture_generation("openai_sync")
            with monkeypatch.context() as patch:
                if reason == "normalization":

                    def fail_normalization(*args: Any, **kwargs: Any) -> Generation:
                        raise ValueError("secret normalization error")

                    patch.setattr(type(recorder), "_normalize_generation", fail_normalization)
                recorder.set_result(
                    generation, mapping_error=ValueError("secret mapping error") if reason == "mapping" else None
                )
                if reason == "shutdown":
                    client.shutdown()
                recorder.end()
                recorder.end()
            expected_error = (
                ClientShutdownError
                if reason == "shutdown"
                else EnqueueError
                if reason == "admission"
                else ValidationError
            )
            assert isinstance(recorder.err(), expected_error)
            assert len(exporter.get_finished_spans()) == before + 1
            if reason == "system":
                assert "role must be one of user|assistant|tool" in str(recorder.err().__cause__)
            span = exporter.get_finished_spans()[-1]
            attrs = dict(span.attributes)
            assert attrs["agento11y.record"] == "false"
            assert attrs["gen_ai.operation.name"] == "agento11y.invalid_generation"
            forbidden = (
                "agento11y.generation.",
                "sigil.generation.",
                "gen_ai.input.",
                "gen_ai.output.",
                "gen_ai.system_instructions",
                "gen_ai.tool.definitions",
                "agento11y.conversation.title",
            )
            assert not any(key.startswith(forbidden) for key in attrs)
            assert "secret" not in span.to_json()
            assert span.parent == parent.get_span_context()
            (directory / f"{reason}.rejected.json").write_text(json.dumps(otlp_json(span)))
            rejected += 1
            assert get_current_span() is parent
            client.shutdown()
    assert len(list(directory.glob("*.span.json"))) == accepted, "use a clean probe directory"
    assert len(list(directory.glob("*.rejected.json"))) == rejected
    # Manifest is evidence, not an allowlist that relaxes protobuf comparisons.
    (root / "manifest.json").write_text(
        json.dumps(
            {
                "accepted_pairs": accepted,
                "diagnostic_spans": rejected,
                "artifact_pair_directory": "rich-artifacts (separate exact-protobuf test)",
                "byte_cases": len(_RECEIVER_BYTE_CASES),
                "byte_variants": ["json-only", "both-text-json"],
                "openai_system": "public adapter direct; normal Client rejects; valid core removes SYSTEM input only",
                "differences": [
                    "chat operation",
                    "provider wire spelling",
                    "sdk-python/record attributes",
                    "live utility timestamps and span context",
                    "Client merges client tags; adds sdk.name/content_capture_mode and title/user.id metadata mirrors",
                    "Client fills absent total_tokens with input+output",
                    "zero usage absent",
                    "effective-version digest",
                    "cache_write alias alongside receiver cache_creation spelling",
                ],
            },
            indent=2,
        )
    )
    result = run_receiver(directory, rejected=True)
    if result is not None:
        assert "TestPythonLocalReceiverActualRejected" in result.stdout, "overlay must inspect actual rejected spans"
        assert result.returncode == 0, result.stdout


def test_actual_client_call_errors_survive_receiver_decode(
    tmp_path: Path,
    span_recorder: tuple[TracerProvider, InMemorySpanExporter],
) -> None:
    provider, exporter = span_recorder
    client = _wire_client(provider)
    directory = _probe_root(tmp_path) / "call-errors"
    try:
        for source in ("generation", "recorder"):
            recorder = client.start_generation(GenerationStart(model=ModelRef("openai", "test")))
            if source == "generation":
                recorder.set_result(call_error="safe provider failure")
            else:
                recorder.set_call_error(ValueError("safe provider failure"))
            recorder.end()
            recorder.end()
            assert recorder.err() is None
            span = exporter.get_finished_spans()[-1]
            normalized = recorder.last_generation
            assert normalized.call_error == "safe provider failure"
            expected_attributes = generation_attributes(normalized, ContentCaptureMode.FULL, client._config.tags)
            expected_attributes.update({"error.type": "provider_call_error", "error.category": "sdk_error"})
            assert dict(span.attributes) == expected_attributes
            assert span.status.description == normalized.call_error
            write_pair(directory, source, span, normalized)
        result = run_receiver(directory)
        if result is not None:
            assert result.returncode == 0, result.stdout
    finally:
        client.shutdown()


def test_actual_client_artifacts_use_receiver_payload_b64(
    tmp_path: Path,
    span_recorder: tuple[TracerProvider, InMemorySpanExporter],
) -> None:
    provider, exporter = span_recorder
    client = _wire_client(provider)
    generation = _rich_generation()
    generation.id = "rich-artifacts"
    generation.artifacts = [
        Artifact(
            kind=kind,
            name=f"artifact-{kind.value}",
            content_type="application/octet-stream",
            payload=b"\x00\xff<>&\x80",
            record_id=f"record-{kind.value}",
            uri=f"uri:{kind.value}",
        )
        for kind in ArtifactKind
    ]
    try:
        span, normalized = _record_client_generation(client, exporter, generation)
        directory = _probe_root(tmp_path) / "rich-artifacts"
        write_pair(directory, "client_rich_artifacts", span, normalized)
        result = run_receiver(directory)
        if result is not None:
            # Exact full protobuf equality, including every artifact byte.
            assert result.returncode == 0, result.stdout
        artifacts = json.loads(span.attributes["agento11y.generation.raw_artifacts"])
        for artifact in artifacts:
            assert base64.b64decode(artifact["payload_b64"]) == b"\x00\xff<>&\x80"
            assert "payload" not in artifact
    finally:
        client.shutdown()
