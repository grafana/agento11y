"""Core conformance suite for the Sigil Python SDK."""

from __future__ import annotations

import concurrent.futures
import copy
import json
import socket
import threading
from collections.abc import Callable
from contextlib import nullcontext
from datetime import datetime, timedelta, timezone
from http.server import BaseHTTPRequestHandler, HTTPServer
from typing import Any

import grpc
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.metrics.export import InMemoryMetricReader
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
from sigil_sdk import (
    ApiConfig,
    Client,
    ClientConfig,
    ConversationRatingInput,
    ConversationRatingValue,
    EmbeddingResult,
    EmbeddingStart,
    Generation,
    GenerationExportConfig,
    GenerationStart,
    Message,
    MessageRole,
    ModelRef,
    Part,
    PartKind,
    TokenUsage,
    ToolCall,
    ToolDefinition,
    ToolExecutionEnd,
    ToolExecutionStart,
    ToolResult,
    WorkflowStep,
    with_agent_name,
    with_agent_version,
    with_conversation_title,
    with_user_id,
)
from sigil_sdk.internal.gen.sigil.v1 import generation_ingest_pb2 as sigil_pb2
from sigil_sdk.internal.gen.sigil.v1 import generation_ingest_pb2_grpc as sigil_pb2_grpc

_metadata_conversation_title = "sigil.conversation.title"
_metadata_user_id = "sigil.user.id"
_metadata_legacy_user_id = "user.id"
_span_attr_conversation_title = "sigil.conversation.title"
_span_attr_user_id = "user.id"


class _CapturingGenerationServicer(
    sigil_pb2_grpc.GenerationIngestServiceServicer,
    sigil_pb2_grpc.WorkflowStepIngestServiceServicer,
):
    def __init__(self) -> None:
        self.requests: list[sigil_pb2.ExportGenerationsRequest] = []
        self.workflow_step_requests: list[sigil_pb2.ExportWorkflowStepsRequest] = []
        self._lock = threading.Lock()

    def ExportGenerations(self, request, _context):  # noqa: N802
        with self._lock:
            self.requests.append(copy.deepcopy(request))
        return sigil_pb2.ExportGenerationsResponse(
            results=[
                sigil_pb2.ExportGenerationResult(generation_id=generation.id, accepted=True)
                for generation in request.generations
            ]
        )

    def ExportWorkflowSteps(self, request, _context):  # noqa: N802
        with self._lock:
            self.workflow_step_requests.append(copy.deepcopy(request))
        return sigil_pb2.ExportWorkflowStepsResponse(
            results=[
                sigil_pb2.ExportWorkflowStepResult(step_id=step.id, accepted=True) for step in request.workflow_steps
            ]
        )

    def single_generation(self) -> sigil_pb2.Generation:
        assert len(self.requests) == 1
        assert len(self.requests[0].generations) == 1
        return self.requests[0].generations[0]

    def single_workflow_step(self) -> sigil_pb2.WorkflowStep:
        assert len(self.workflow_step_requests) == 1
        assert len(self.workflow_step_requests[0].workflow_steps) == 1
        return self.workflow_step_requests[0].workflow_steps[0]


class _RatingCaptureServer:
    def __init__(self) -> None:
        self.requests: list[dict[str, Any]] = []

        class _Handler(BaseHTTPRequestHandler):
            def do_POST(handler):  # noqa: N802
                length = int(handler.headers.get("Content-Length", "0"))
                body = handler.rfile.read(length)
                self.requests.append(
                    {
                        "path": handler.path,
                        "headers": {k.lower(): v for k, v in handler.headers.items()},
                        "payload": json.loads(body.decode("utf-8")),
                    }
                )
                encoded = json.dumps(
                    {
                        "rating": {
                            "rating_id": "rat-1",
                            "conversation_id": "conv-rating",
                            "rating": "CONVERSATION_RATING_VALUE_BAD",
                            "created_at": "2026-03-12T09:00:00Z",
                        },
                        "summary": {
                            "total_count": 1,
                            "good_count": 0,
                            "bad_count": 1,
                            "latest_rating": "CONVERSATION_RATING_VALUE_BAD",
                            "latest_rated_at": "2026-03-12T09:00:00Z",
                            "has_bad_rating": True,
                        },
                    }
                ).encode("utf-8")
                handler.send_response(200)
                handler.send_header("Content-Type", "application/json")
                handler.send_header("Content-Length", str(len(encoded)))
                handler.end_headers()
                handler.wfile.write(encoded)

            def log_message(self, _format, *_args):  # noqa: A003
                return

        self.server = HTTPServer(("127.0.0.1", 0), _Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    @property
    def endpoint(self) -> str:
        return f"http://127.0.0.1:{self.server.server_address[1]}"

    def close(self) -> None:
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=2)


class _ConformanceEnv:
    def __init__(
        self,
        *,
        batch_size: int = 1,
        flush_interval: timedelta | None = None,
        now: Callable[[], datetime] | None = None,
        tags: dict[str, str] | None = None,
    ) -> None:
        self.servicer = _CapturingGenerationServicer()
        self.grpc_server = grpc.server(concurrent.futures.ThreadPoolExecutor(max_workers=2))
        sigil_pb2_grpc.add_GenerationIngestServiceServicer_to_server(self.servicer, self.grpc_server)
        sigil_pb2_grpc.add_WorkflowStepIngestServiceServicer_to_server(self.servicer, self.grpc_server)

        sock = socket.socket()
        sock.bind(("127.0.0.1", 0))
        port = sock.getsockname()[1]
        sock.close()

        self.grpc_server.add_insecure_port(f"127.0.0.1:{port}")
        self.grpc_server.start()

        self.rating_server = _RatingCaptureServer()
        self.span_exporter = InMemorySpanExporter()
        self.tracer_provider = TracerProvider()
        self.tracer_provider.add_span_processor(SimpleSpanProcessor(self.span_exporter))
        self.metric_reader = InMemoryMetricReader()
        self.meter_provider = MeterProvider(metric_readers=[self.metric_reader])

        export_config = GenerationExportConfig(
            protocol="grpc",
            endpoint=f"127.0.0.1:{port}",
            insecure=True,
            batch_size=batch_size,
            flush_interval=flush_interval or timedelta(hours=1),
            max_retries=1,
            initial_backoff=timedelta(milliseconds=1),
            max_backoff=timedelta(milliseconds=2),
        )
        self.client = Client(
            ClientConfig(
                tracer=self.tracer_provider.get_tracer("sigil-conformance-test"),
                meter=self.meter_provider.get_meter("sigil-conformance-test"),
                generation_export=export_config,
                api=ApiConfig(endpoint=self.rating_server.endpoint),
                now=now,
                tags=tags,
            )
        )
        self._closed = False

    def shutdown(self) -> None:
        if self._closed:
            return
        self._closed = True
        self.client.shutdown()
        self.tracer_provider.shutdown()
        self.meter_provider.shutdown()
        self.grpc_server.stop(grace=0)
        self.rating_server.close()

    def generation_span(self):
        spans = [
            span
            for span in self.span_exporter.get_finished_spans()
            if span.attributes.get("gen_ai.operation.name") in {"generateText", "streamText"}
        ]
        assert spans
        return spans[-1]

    def latest_span(self, operation_name: str):
        spans = [
            span
            for span in self.span_exporter.get_finished_spans()
            if span.attributes.get("gen_ai.operation.name") == operation_name
        ]
        assert spans
        return spans[-1]

    def metrics(self) -> dict[str, Any]:
        metrics = {}
        data = self.metric_reader.get_metrics_data()
        for resource_metric in data.resource_metrics:
            for scope_metric in resource_metric.scope_metrics:
                for metric in scope_metric.metrics:
                    metrics[metric.name] = metric.data
        return metrics


def test_conformance_sync_roundtrip_semantics() -> None:
    env = _ConformanceEnv()
    try:
        recorder = env.client.start_generation(
            GenerationStart(
                id="gen-roundtrip",
                conversation_id="conv-roundtrip",
                conversation_title="Roundtrip conversation",
                user_id="user-roundtrip",
                agent_name="agent-roundtrip",
                agent_version="v-roundtrip",
                model=ModelRef(provider="openai", name="gpt-5"),
                max_tokens=256,
                temperature=0.2,
                top_p=0.9,
                tool_choice="required",
                thinking_enabled=False,
                tools=[ToolDefinition(name="weather", description="Get weather", type="function")],
                tags={"tenant": "dev"},
                metadata={"trace": "roundtrip"},
            )
        )
        recorder.set_result(
            Generation(
                response_id="resp-roundtrip",
                response_model="gpt-5-2026",
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
                            Part(kind=PartKind.THINKING, thinking="reasoning"),
                            Part(
                                kind=PartKind.TOOL_CALL,
                                tool_call=ToolCall(
                                    id="call-1",
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
                                    tool_call_id="call-1",
                                    name="weather",
                                    content="sunny",
                                    content_json=b'{"temp_c":18}',
                                ),
                            )
                        ],
                    ),
                ],
                usage=TokenUsage(
                    input_tokens=12,
                    output_tokens=7,
                    total_tokens=19,
                    cache_read_input_tokens=2,
                    cache_write_input_tokens=1,
                    reasoning_tokens=4,
                ),
                stop_reason="stop",
                tags={"region": "eu"},
                metadata={"result": "ok"},
            )
        )
        recorder.end()
        env.shutdown()

        generation = env.servicer.single_generation()
        span = env.generation_span()
        metrics = env.metrics()

        assert generation.mode == sigil_pb2.GENERATION_MODE_SYNC
        assert generation.operation_name == "generateText"
        assert generation.conversation_id == "conv-roundtrip"
        assert generation.agent_name == "agent-roundtrip"
        assert generation.agent_version == "v-roundtrip"
        assert generation.trace_id == span.context.trace_id.to_bytes(16, "big").hex()
        assert generation.span_id == span.context.span_id.to_bytes(8, "big").hex()
        assert generation.metadata.fields[_metadata_conversation_title].string_value == "Roundtrip conversation"
        assert generation.metadata.fields[_metadata_user_id].string_value == "user-roundtrip"
        assert generation.input[0].parts[0].text == "hello"
        assert generation.output[0].parts[0].thinking == "reasoning"
        assert generation.output[0].parts[1].tool_call.name == "weather"
        assert generation.output[1].parts[0].tool_result.content == "sunny"
        assert generation.max_tokens == 256
        assert generation.temperature == 0.2
        assert generation.top_p == 0.9
        assert generation.tool_choice == "required"
        assert generation.thinking_enabled is False
        assert generation.usage.input_tokens == 12
        assert generation.usage.output_tokens == 7
        assert generation.usage.total_tokens == 19
        assert generation.usage.cache_read_input_tokens == 2
        assert generation.usage.cache_write_input_tokens == 1
        assert generation.usage.reasoning_tokens == 4
        assert generation.stop_reason == "stop"
        assert generation.tags["tenant"] == "dev"
        assert generation.tags["region"] == "eu"

        assert span.attributes["gen_ai.operation.name"] == "generateText"
        assert span.attributes[_span_attr_conversation_title] == "Roundtrip conversation"
        assert span.attributes[_span_attr_user_id] == "user-roundtrip"
        assert "gen_ai.client.operation.duration" in metrics
        assert "gen_ai.client.token.usage" in metrics
        assert "gen_ai.client.time_to_first_token" not in metrics
        assert any(
            point.attributes.get("gen_ai.agent.version") == "v-roundtrip"
            and point.attributes.get("gen_ai.token.type") == "input"
            for point in metrics["gen_ai.client.token.usage"].data_points
        )
    finally:
        env.shutdown()


def test_conformance_workflow_step_roundtrip_semantics() -> None:
    # Mirror the Go and JS conformance inputs exactly: no timestamps (to exercise
    # enqueue-time defaulting to the client clock) and client-level tags that the
    # step's own tags override on conflict.
    now = datetime(2026, 3, 12, 12, 0, 0, tzinfo=timezone.utc)
    env = _ConformanceEnv(
        now=lambda: now,
        tags={"client": "tag", "shared": "client"},
    )
    try:
        env.client.enqueue_workflow_step(
            WorkflowStep(
                id="wfs-roundtrip",
                conversation_id="conv-workflow",
                step_name="route",
                framework="custom",
                input_state={"prompt": "hello", "count": 1},
                output_state={"route": "answer"},
                tags={"shared": "step"},
                linked_generation_ids=["gen-roundtrip"],
                parent_step_ids=["wfs-root"],
                agent_name="agent-workflow",
                agent_version="v1",
                trace_id="0123456789abcdef0123456789abcdef",
                span_id="0123456789abcdef",
                metadata={"run_id": "run-1"},
            )
        )
        env.shutdown()

        step = env.servicer.single_workflow_step()
        assert step.id == "wfs-roundtrip"
        assert step.conversation_id == "conv-workflow"
        assert step.step_name == "route"
        assert step.framework == "custom"
        assert step.started_at.ToDatetime(tzinfo=timezone.utc) == now
        assert step.completed_at.ToDatetime(tzinfo=timezone.utc) == now
        assert step.tags["client"] == "tag"
        assert step.tags["shared"] == "step"
        assert step.input_state.fields["prompt"].string_value == "hello"
        assert step.output_state.fields["route"].string_value == "answer"
        assert list(step.linked_generation_ids) == ["gen-roundtrip"]
        assert list(step.parent_step_ids) == ["wfs-root"]
        assert step.agent_name == "agent-workflow"
        assert step.agent_version == "v1"
        assert step.metadata.fields["run_id"].string_value == "run-1"
    finally:
        env.shutdown()


def test_conformance_conversation_title_semantics() -> None:
    cases = [
        ("explicit wins", "Explicit", "Context", "Meta", "Explicit"),
        ("context fallback", "", "Context", "", "Context"),
        ("metadata fallback", "", "", "Meta", "Meta"),
        ("whitespace trimmed", "  Padded  ", "", "", "Padded"),
        ("whitespace omitted", "   ", "", "", ""),
    ]

    for _, start_title, context_title, metadata_title, want_title in cases:
        env = _ConformanceEnv()
        try:
            context = with_conversation_title(context_title) if context_title else nullcontext()
            with context:
                start = GenerationStart(
                    model=ModelRef(provider="openai", name="gpt-5"),
                    conversation_title=start_title,
                    metadata=({_metadata_conversation_title: metadata_title} if metadata_title else {}),
                )
                recorder = env.client.start_generation(start)
                recorder.set_result(Generation())
                recorder.end()
            env.shutdown()

            generation = env.servicer.single_generation()
            span = env.generation_span()
            field = generation.metadata.fields.get(_metadata_conversation_title)
            if want_title == "":
                assert field is None
                assert _span_attr_conversation_title not in span.attributes
            else:
                assert field is not None
                assert field.string_value == want_title
                assert span.attributes[_span_attr_conversation_title] == want_title
        finally:
            env.shutdown()


def test_conformance_user_id_semantics() -> None:
    cases = [
        ("explicit wins", "explicit", "ctx", "canonical", "legacy", "explicit"),
        ("context fallback", "", "ctx", "", "", "ctx"),
        ("canonical metadata", "", "", "canonical", "", "canonical"),
        ("legacy metadata", "", "", "", "legacy", "legacy"),
        ("canonical beats legacy", "", "", "canonical", "legacy", "canonical"),
        ("whitespace trimmed", "  padded  ", "", "", "", "padded"),
    ]

    for (
        _,
        start_user_id,
        context_user_id,
        canonical_user_id,
        legacy_user_id,
        want_user_id,
    ) in cases:
        env = _ConformanceEnv()
        try:
            metadata = {}
            if canonical_user_id:
                metadata[_metadata_user_id] = canonical_user_id
            if legacy_user_id:
                metadata[_metadata_legacy_user_id] = legacy_user_id

            context = with_user_id(context_user_id) if context_user_id else nullcontext()
            with context:
                recorder = env.client.start_generation(
                    GenerationStart(
                        model=ModelRef(provider="openai", name="gpt-5"),
                        user_id=start_user_id,
                        metadata=metadata,
                    )
                )
                recorder.set_result(Generation())
                recorder.end()
            env.shutdown()

            generation = env.servicer.single_generation()
            span = env.generation_span()
            assert generation.metadata.fields[_metadata_user_id].string_value == want_user_id
            assert span.attributes[_span_attr_user_id] == want_user_id
        finally:
            env.shutdown()


def test_conformance_agent_identity_semantics() -> None:
    cases = [
        (
            "explicit fields",
            "agent-explicit",
            "v1.2.3",
            "",
            "",
            "",
            "",
            "agent-explicit",
            "v1.2.3",
        ),
        (
            "context fallback",
            "",
            "",
            "agent-context",
            "v-context",
            "",
            "",
            "agent-context",
            "v-context",
        ),
        (
            "result-time override",
            "agent-seed",
            "v-seed",
            "",
            "",
            "agent-result",
            "v-result",
            "agent-result",
            "v-result",
        ),
        ("empty omission", "", "", "", "", "", "", "", ""),
    ]

    for (
        _,
        start_name,
        start_version,
        context_name,
        context_version,
        result_name,
        result_version,
        want_name,
        want_version,
    ) in cases:
        env = _ConformanceEnv()
        try:
            with with_agent_name(context_name) if context_name else nullcontext():
                with with_agent_version(context_version) if context_version else nullcontext():
                    recorder = env.client.start_generation(
                        GenerationStart(
                            model=ModelRef(provider="openai", name="gpt-5"),
                            agent_name=start_name,
                            agent_version=start_version,
                        )
                    )
                    recorder.set_result(
                        Generation(
                            agent_name=result_name,
                            agent_version=result_version,
                        )
                    )
                    recorder.end()
            env.shutdown()

            generation = env.servicer.single_generation()
            span = env.generation_span()
            assert generation.agent_name == want_name
            assert generation.agent_version == want_version
            if want_name:
                assert span.attributes["gen_ai.agent.name"] == want_name
            else:
                assert "gen_ai.agent.name" not in span.attributes
            if want_version:
                assert span.attributes["gen_ai.agent.version"] == want_version
            else:
                assert "gen_ai.agent.version" not in span.attributes
        finally:
            env.shutdown()


# echo -n "1.2.3" | shasum -a 256
EFFECTIVE_VERSION_DIGEST_1_2_3 = "sha256:c47f5b18b8a430e698b9fe15e51f6119984e78334bcf3f45e210d30c37ef2f9e"


def test_conformance_effective_version_semantics() -> None:
    cases = [
        ("unset leaves proto field absent", "", ""),
        ("whitespace-only leaves proto field absent", "   ", ""),
        ("raw 1.2.3 hashes to pinned digest", "1.2.3", EFFECTIVE_VERSION_DIGEST_1_2_3),
        (
            "surrounding whitespace is trimmed before hashing",
            "  1.2.3\t\n",
            EFFECTIVE_VERSION_DIGEST_1_2_3,
        ),
    ]

    for _, effective_version, want in cases:
        env = _ConformanceEnv()
        try:
            recorder = env.client.start_generation(
                GenerationStart(
                    model=ModelRef(provider="openai", name="gpt-5"),
                    effective_version=effective_version,
                )
            )
            recorder.set_result(Generation())
            recorder.end()
            assert recorder.err() is None
            env.shutdown()

            generation = env.servicer.single_generation()
            if want == "":
                assert not generation.HasField("effective_version")
            else:
                assert generation.effective_version == want
        finally:
            env.shutdown()


def test_conformance_effective_version_result_overrides_start() -> None:
    # echo -n "result-only" | shasum -a 256
    result_only_digest = "sha256:f61f2b041f07a7e4a58a926df31279f4c11ebd1f716147d8ee8cbfad6a69f30e"

    cases = [
        (
            "start falls through when result is empty",
            "1.2.3",
            "",
            EFFECTIVE_VERSION_DIGEST_1_2_3,
        ),
        (
            "start falls through when result is whitespace-only",
            "1.2.3",
            "   ",
            EFFECTIVE_VERSION_DIGEST_1_2_3,
        ),
        (
            "result wins over start",
            "ignored",
            "result-only",
            result_only_digest,
        ),
    ]

    for _, start_value, result_value, want in cases:
        env = _ConformanceEnv()
        try:
            recorder = env.client.start_generation(
                GenerationStart(
                    model=ModelRef(provider="openai", name="gpt-5"),
                    effective_version=start_value,
                )
            )
            recorder.set_result(Generation(effective_version=result_value))
            recorder.end()
            assert recorder.err() is None
            env.shutdown()

            generation = env.servicer.single_generation()
            assert generation.effective_version == want
        finally:
            env.shutdown()


def test_conformance_streaming_telemetry_semantics() -> None:
    env = _ConformanceEnv()
    try:
        start = GenerationStart(model=ModelRef(provider="openai", name="gpt-5"))
        recorder = env.client.start_streaming_generation(start)
        recorder.set_first_token_at(datetime(2026, 3, 12, 9, 0, 0, 250000, tzinfo=timezone.utc))
        recorder.set_result(
            Generation(
                output=[
                    Message(
                        role=MessageRole.ASSISTANT,
                        parts=[Part(kind=PartKind.TEXT, text="Hello world")],
                    )
                ],
                usage=TokenUsage(input_tokens=4, output_tokens=3, total_tokens=7),
                started_at=datetime(2026, 3, 12, 9, 0, 0, tzinfo=timezone.utc),
                completed_at=datetime(2026, 3, 12, 9, 0, 1, tzinfo=timezone.utc),
            )
        )
        recorder.end()
        env.shutdown()

        generation = env.servicer.single_generation()
        span = env.generation_span()
        metrics = env.metrics()

        assert generation.mode == sigil_pb2.GENERATION_MODE_STREAM
        assert generation.operation_name == "streamText"
        assert generation.output[0].parts[0].text == "Hello world"
        assert span.name == "streamText gpt-5"
        assert "gen_ai.client.operation.duration" in metrics
        assert "gen_ai.client.time_to_first_token" in metrics

        expected_buckets = (
            0.01,
            0.02,
            0.04,
            0.08,
            0.16,
            0.32,
            0.64,
            1.28,
            2.56,
            5.12,
            10.24,
            20.48,
            40.96,
            81.92,
        )
        for metric_name in (
            "gen_ai.client.operation.duration",
            "gen_ai.client.time_to_first_token",
        ):
            data_points = list(metrics[metric_name].data_points)
            assert data_points, f"expected {metric_name} data points"
            assert tuple(data_points[0].explicit_bounds) == expected_buckets, (
                f"{metric_name} bucket boundaries mismatch: {tuple(data_points[0].explicit_bounds)}"
            )

        expected_token_usage_buckets = (
            1,
            4,
            16,
            64,
            256,
            1024,
            4096,
            16384,
            65536,
            262144,
            1048576,
            4194304,
            16777216,
            67108864,
        )
        token_usage_points = list(metrics["gen_ai.client.token.usage"].data_points)
        assert token_usage_points, "expected gen_ai.client.token.usage data points"
        assert tuple(token_usage_points[0].explicit_bounds) == expected_token_usage_buckets, (
            f"gen_ai.client.token.usage bucket boundaries mismatch: {tuple(token_usage_points[0].explicit_bounds)}"
        )
    finally:
        env.shutdown()


def test_conformance_tool_execution_semantics() -> None:
    env = _ConformanceEnv()
    try:
        with with_conversation_title("Context title"):
            with with_agent_name("agent-context"):
                with with_agent_version("v-context"):
                    recorder = env.client.start_tool_execution(
                        ToolExecutionStart(
                            tool_name="weather",
                            tool_call_id="call-weather-1",
                            tool_type="function",
                            include_content=True,
                        )
                    )
                    recorder.set_result(
                        ToolExecutionEnd(
                            arguments={"city": "Paris"},
                            result={"forecast": "sunny"},
                        )
                    )
                    recorder.end()
        env.shutdown()

        span = env.latest_span("execute_tool")
        metrics = env.metrics()

        assert env.servicer.requests == []
        assert span.name == "execute_tool weather"
        assert span.attributes["gen_ai.operation.name"] == "execute_tool"
        assert span.attributes["gen_ai.tool.name"] == "weather"
        assert span.attributes["gen_ai.tool.call.id"] == "call-weather-1"
        assert span.attributes["gen_ai.tool.type"] == "function"
        assert "Paris" in str(span.attributes["gen_ai.tool.call.arguments"])
        assert "sunny" in str(span.attributes["gen_ai.tool.call.result"])
        assert span.attributes[_span_attr_conversation_title] == "Context title"
        assert span.attributes["gen_ai.agent.name"] == "agent-context"
        assert span.attributes["gen_ai.agent.version"] == "v-context"
        assert "gen_ai.client.operation.duration" in metrics
        assert "gen_ai.client.time_to_first_token" not in metrics
    finally:
        env.shutdown()


def test_conformance_embedding_semantics() -> None:
    env = _ConformanceEnv()
    try:
        with with_agent_name("agent-context"):
            with with_agent_version("v-context"):
                recorder = env.client.start_embedding(
                    EmbeddingStart(
                        model=ModelRef(provider="openai", name="text-embedding-3-small"),
                        dimensions=512,
                    )
                )
                recorder.set_result(
                    EmbeddingResult(
                        input_count=2,
                        input_tokens=8,
                        input_texts=["hello", "world"],
                        response_model="text-embedding-3-small",
                        dimensions=512,
                    )
                )
                recorder.end()
        env.shutdown()

        span = env.latest_span("embeddings")
        metrics = env.metrics()

        assert env.servicer.requests == []
        assert span.name == "embeddings text-embedding-3-small"
        assert span.attributes["gen_ai.operation.name"] == "embeddings"
        assert span.attributes["gen_ai.agent.name"] == "agent-context"
        assert span.attributes["gen_ai.agent.version"] == "v-context"
        assert span.attributes["gen_ai.embeddings.input_count"] == 2
        assert span.attributes["gen_ai.embeddings.dimension.count"] == 512
        assert span.attributes["gen_ai.response.model"] == "text-embedding-3-small"
        assert "gen_ai.client.operation.duration" in metrics
        assert "gen_ai.client.token.usage" in metrics
        assert "gen_ai.client.time_to_first_token" not in metrics
        assert "gen_ai.client.tool_calls_per_operation" not in metrics
    finally:
        env.shutdown()


def test_conformance_validation_and_error_semantics() -> None:
    env = _ConformanceEnv()
    try:
        invalid = env.client.start_generation(
            GenerationStart(model=ModelRef(provider="anthropic", name="claude-sonnet-4-5"))
        )
        invalid.set_result(
            Generation(
                input=[
                    Message(
                        role=MessageRole.USER,
                        parts=[
                            Part(
                                kind=PartKind.TOOL_CALL,
                                tool_call=ToolCall(name="weather"),
                            )
                        ],
                    )
                ]
            )
        )
        invalid.end()

        assert invalid.err() is not None
        assert env.servicer.requests == []
        assert env.generation_span().attributes["error.type"] == "validation_error"

        call_error = env.client.start_generation(GenerationStart(model=ModelRef(provider="openai", name="gpt-5")))
        call_error.set_call_error(RuntimeError("provider unavailable"))
        call_error.set_result(Generation())
        call_error.end()
        env.shutdown()

        generation = env.servicer.single_generation()
        spans = env.span_exporter.get_finished_spans()
        assert call_error.err() is None
        assert generation.call_error == "provider unavailable"
        assert generation.metadata.fields["call_error"].string_value == "provider unavailable"
        assert spans[-1].attributes["error.type"] == "provider_call_error"
    finally:
        env.shutdown()


def test_conformance_rating_submission_semantics() -> None:
    env = _ConformanceEnv()
    try:
        response = env.client.submit_conversation_rating(
            "conv-rating",
            ConversationRatingInput(
                rating_id="rat-1",
                rating=ConversationRatingValue.BAD,
                comment="wrong answer",
                metadata={"channel": "assistant"},
            ),
        )
        env.shutdown()

        assert len(env.rating_server.requests) == 1
        request = env.rating_server.requests[0]
        assert request["path"] == "/api/v1/conversations/conv-rating/ratings"
        assert request["payload"] == {
            "rating_id": "rat-1",
            "rating": "CONVERSATION_RATING_VALUE_BAD",
            "comment": "wrong answer",
            "metadata": {"channel": "assistant"},
        }
        assert response.rating.conversation_id == "conv-rating"
        assert response.summary.bad_count == 1
    finally:
        env.shutdown()


def test_conformance_shutdown_flush_semantics() -> None:
    env = _ConformanceEnv(batch_size=10)
    try:
        recorder = env.client.start_generation(
            GenerationStart(
                conversation_id="conv-shutdown",
                agent_name="agent-shutdown",
                agent_version="v-shutdown",
                model=ModelRef(provider="openai", name="gpt-5"),
            )
        )
        recorder.set_result(Generation())
        recorder.end()

        assert env.servicer.requests == []
        env.shutdown()

        generation = env.servicer.single_generation()
        assert generation.conversation_id == "conv-shutdown"
        assert generation.agent_name == "agent-shutdown"
        assert generation.agent_version == "v-shutdown"
    finally:
        env.shutdown()
