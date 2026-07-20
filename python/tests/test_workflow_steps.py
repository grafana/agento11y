"""Workflow step tests: proto mapping, HTTP transport, handler wiring."""

from __future__ import annotations

import json
import threading
from datetime import datetime, timedelta, timezone
from http.server import BaseHTTPRequestHandler, HTTPServer
from typing import Any
from uuid import uuid4

from agento11y import (
    Client,
    ClientConfig,
    EnqueueError,
    GenerationExportConfig,
    ValidationError,
    WorkflowStep,
)
from agento11y.exporters.http import HTTPGenerationExporter, _normalize_endpoint
from agento11y.exporters.noop import NoopGenerationExporter
from agento11y.framework_handler import (
    Agento11yFrameworkHandlerBase,
    _safe_serializable_dict,
)
from agento11y.models import (
    ExportWorkflowStepResult,
    ExportWorkflowStepsRequest,
    ExportWorkflowStepsResponse,
)
from agento11y.proto_mapping import workflow_step_to_proto, workflow_step_to_proto_json

# ---------------------------------------------------------------------------
# proto mapping
# ---------------------------------------------------------------------------


def test_workflow_step_to_proto_round_trips_all_fields() -> None:
    started = datetime(2025, 1, 2, 3, 4, 5, tzinfo=timezone.utc)
    completed = datetime(2025, 1, 2, 3, 4, 6, tzinfo=timezone.utc)
    step = WorkflowStep(
        id="wfs_abc",
        conversation_id="conv-1",
        step_name="classify",
        framework="langgraph",
        agent_name="incident-pipeline",
        agent_version="v1",
        started_at=started,
        completed_at=completed,
        input_state={"text": "hello", "n": 1},
        output_state={"category": "incident"},
        error="boom",
        tags={"agento11y.framework.name": "langgraph"},
        metadata={"agento11y.framework.run_id": "run-1"},
        linked_generation_ids=["gen_1", "gen_2"],
        parent_step_ids=["wfs_parent"],
        trace_id="trace-1",
        span_id="span-1",
    )

    proto = workflow_step_to_proto(step)

    assert proto.id == "wfs_abc"
    assert proto.conversation_id == "conv-1"
    assert proto.step_name == "classify"
    assert proto.framework == "langgraph"
    assert proto.agent_name == "incident-pipeline"
    assert proto.agent_version == "v1"
    assert proto.error == "boom"
    assert dict(proto.tags) == {"agento11y.framework.name": "langgraph"}
    assert list(proto.linked_generation_ids) == ["gen_1", "gen_2"]
    assert list(proto.parent_step_ids) == ["wfs_parent"]
    assert proto.trace_id == "trace-1"
    assert proto.span_id == "span-1"
    assert proto.started_at.seconds == int(started.timestamp())
    assert proto.completed_at.seconds == int(completed.timestamp())
    assert proto.input_state["text"] == "hello"
    assert proto.input_state["n"] == 1
    assert proto.output_state["category"] == "incident"
    assert proto.metadata["agento11y.framework.run_id"] == "run-1"


def test_workflow_step_to_proto_json_uses_snake_case_keys() -> None:
    step = WorkflowStep(id="wfs_x", conversation_id="c", step_name="route")

    payload = workflow_step_to_proto_json(step)

    # Sigil HTTP parity uses snake_case proto field names.
    assert "stepName" not in payload
    assert payload.get("step_name") == "route"
    assert payload.get("conversation_id") == "c"


def test_safe_serializable_dict_handles_non_json_inputs() -> None:
    class _NotJsonable:
        def __init__(self, value: str) -> None:
            self.value = value

    raw: dict[str, Any] = {
        "ok": "value",
        "obj": _NotJsonable("inner"),
        "nested": {"deep": _NotJsonable("more")},
        "list": [1, _NotJsonable("item")],
    }

    result = _safe_serializable_dict(raw)

    json.dumps(result)  # must not raise


# ---------------------------------------------------------------------------
# HTTP transport
# ---------------------------------------------------------------------------


def test_normalize_endpoint_appends_workflow_steps_path() -> None:
    assert (
        _normalize_endpoint("http://host:8080", "/api/v1/workflow-steps:export")
        == "http://host:8080/api/v1/workflow-steps:export"
    )
    assert (
        _normalize_endpoint(
            "http://host:8080/api/v1/generations:export",
            "/api/v1/workflow-steps:export",
        )
        == "http://host:8080/api/v1/workflow-steps:export"
    )
    assert (
        _normalize_endpoint("http://host:8080/custom/ingest", "/api/v1/workflow-steps:export")
        == "http://host:8080/custom/ingest"
    )


def test_http_exporter_round_trip_workflow_steps() -> None:
    captured: list[tuple[str, dict[str, Any]]] = []

    class _Handler(BaseHTTPRequestHandler):
        def do_POST(self) -> None:  # noqa: N802
            length = int(self.headers.get("Content-Length", "0"))
            body = self.rfile.read(length)
            captured.append((self.path, json.loads(body.decode("utf-8"))))
            payload = {
                "results": [
                    {"step_id": step["id"], "accepted": True, "error": ""}
                    for step in json.loads(body.decode("utf-8"))["workflow_steps"]
                ]
            }
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(json.dumps(payload).encode("utf-8"))

        def log_message(self, *args: Any, **kwargs: Any) -> None:
            return

    server = HTTPServer(("127.0.0.1", 0), _Handler)
    host, port = server.server_address
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        exporter = HTTPGenerationExporter(endpoint=f"http://{host}:{port}")
        response = exporter.export_workflow_steps(
            ExportWorkflowStepsRequest(
                workflow_steps=[
                    WorkflowStep(id="wfs_1", conversation_id="c1", step_name="route"),
                    WorkflowStep(id="wfs_2", conversation_id="c1", step_name="answer"),
                ]
            )
        )
    finally:
        server.shutdown()
        thread.join(timeout=2)

    assert len(response.results) == 2
    assert all(result.accepted for result in response.results)
    assert {result.step_id for result in response.results} == {"wfs_1", "wfs_2"}

    assert captured, "expected http export call"
    path, body = captured[0]
    assert path == "/api/v1/workflow-steps:export"
    assert len(body["workflow_steps"]) == 2


# ---------------------------------------------------------------------------
# Client batching + retry
# ---------------------------------------------------------------------------


class _RecordingExporter:
    def __init__(self) -> None:
        self.gen_batches: list[list[Any]] = []
        self.wf_batches: list[list[WorkflowStep]] = []
        self.fail_workflow_attempts = 0

    def export_generations(self, request):
        self.gen_batches.append(list(request.generations))
        from agento11y.models import ExportGenerationResult, ExportGenerationsResponse

        return ExportGenerationsResponse(
            results=[
                ExportGenerationResult(generation_id=generation.id, accepted=True) for generation in request.generations
            ]
        )

    def export_workflow_steps(self, request):
        if self.fail_workflow_attempts > 0:
            self.fail_workflow_attempts -= 1
            raise RuntimeError("transient")
        self.wf_batches.append(list(request.workflow_steps))
        return ExportWorkflowStepsResponse(
            results=[ExportWorkflowStepResult(step_id=step.id, accepted=True) for step in request.workflow_steps]
        )

    def shutdown(self) -> None:
        return


def _client_with_exporter(exporter) -> Client:
    # ``flush_interval=0`` disables the background timer so the test stays
    # deterministic — flushes happen only on explicit ``client.flush()``.
    return Client(
        ClientConfig(
            generation_export=GenerationExportConfig(
                batch_size=10,
                max_retries=3,
                flush_interval=timedelta(0),
                initial_backoff=timedelta(milliseconds=1),
                max_backoff=timedelta(milliseconds=10),
            ),
            generation_exporter=exporter,
        )
    )


def test_client_flushes_workflow_steps() -> None:
    exporter = _RecordingExporter()
    client = _client_with_exporter(exporter)
    try:
        client.enqueue_workflow_step(WorkflowStep(id="wfs_a", conversation_id="c1", step_name="route"))
        client.flush()
    finally:
        client.shutdown()

    assert len(exporter.wf_batches) == 1
    assert exporter.wf_batches[0][0].id == "wfs_a"


def test_client_retries_workflow_step_export() -> None:
    exporter = _RecordingExporter()
    exporter.fail_workflow_attempts = 2  # recover on third attempt
    client = _client_with_exporter(exporter)
    try:
        client.enqueue_workflow_step(WorkflowStep(id="wfs_b", conversation_id="c1", step_name="answer"))
        client.flush()
    finally:
        client.shutdown()

    assert len(exporter.wf_batches) == 1
    assert exporter.wf_batches[0][0].id == "wfs_b"


def test_enqueue_workflow_step_defaults_timestamps_and_merges_tags() -> None:
    now = datetime(2026, 3, 12, 12, 0, 0, tzinfo=timezone.utc)
    exporter = _RecordingExporter()
    client = Client(
        ClientConfig(
            now=lambda: now,
            tags={"client": "tag", "shared": "client"},
            generation_export=GenerationExportConfig(
                batch_size=10,
                flush_interval=timedelta(0),
            ),
            generation_exporter=exporter,
        )
    )
    try:
        # No timestamps supplied: both default to the client clock. The step's
        # own tag overrides the client-level tag on conflict.
        client.enqueue_workflow_step(
            WorkflowStep(id="wfs_a", conversation_id="c1", step_name="route", tags={"shared": "step"})
        )
        client.flush()
    finally:
        client.shutdown()

    step = exporter.wf_batches[0][0]
    assert step.started_at == now
    assert step.completed_at == now
    assert step.tags == {"client": "tag", "shared": "step"}


def test_enqueue_workflow_step_rejects_invalid_step() -> None:
    exporter = _RecordingExporter()
    client = _client_with_exporter(exporter)
    try:
        # Missing step_name fails validation before it is queued.
        try:
            client.enqueue_workflow_step(WorkflowStep(id="wfs_a", conversation_id="c1"))
            raise AssertionError("expected ValidationError")
        except ValidationError:
            pass
        client.flush()
    finally:
        client.shutdown()

    assert exporter.wf_batches == []


def test_enqueue_workflow_step_validates_raw_input_before_defaulting() -> None:
    # A step with only completed_at (started_at unset) is valid: the
    # completed < started rule only applies when the caller supplied both.
    # Validation runs on the raw input, so defaulting started_at to now() must
    # not turn this into a spurious rejection. Matches Go and JS.
    now = datetime(2026, 3, 12, 12, 0, 0, tzinfo=timezone.utc)
    completed = datetime(2020, 1, 1, 0, 0, 0, tzinfo=timezone.utc)
    exporter = _RecordingExporter()
    client = Client(
        ClientConfig(
            now=lambda: now,
            generation_export=GenerationExportConfig(batch_size=10, flush_interval=timedelta(0)),
            generation_exporter=exporter,
        )
    )
    try:
        client.enqueue_workflow_step(
            WorkflowStep(id="wfs_a", conversation_id="c1", step_name="route", completed_at=completed)
        )
        client.flush()
    finally:
        client.shutdown()

    step = exporter.wf_batches[0][0]
    assert step.started_at == now  # defaulted
    assert step.completed_at == completed  # caller-supplied, preserved


def test_enqueue_workflow_step_rejects_oversized_payload() -> None:
    exporter = _RecordingExporter()
    client = Client(
        ClientConfig(
            generation_export=GenerationExportConfig(
                batch_size=10,
                flush_interval=timedelta(0),
                payload_max_bytes=32,
            ),
            generation_exporter=exporter,
        )
    )
    try:
        try:
            client.enqueue_workflow_step(
                WorkflowStep(
                    id="wfs_a",
                    conversation_id="c1",
                    step_name="route",
                    input_state={"blob": "x" * 1000},
                )
            )
            raise AssertionError("expected EnqueueError")
        except EnqueueError:
            pass
        client.flush()
    finally:
        client.shutdown()

    assert exporter.wf_batches == []


def test_noop_exporter_accepts_workflow_steps() -> None:
    exporter = NoopGenerationExporter()
    response = exporter.export_workflow_steps(
        ExportWorkflowStepsRequest(workflow_steps=[WorkflowStep(id="wfs_a", conversation_id="c1", step_name="x")])
    )
    assert len(response.results) == 1
    assert response.results[0].accepted is True
    assert response.results[0].step_id == "wfs_a"


# ---------------------------------------------------------------------------
# Handler: workflow step capture, walk-chain linked_generation_ids,
# multi-root concurrency, cleanup
# ---------------------------------------------------------------------------


class _CollectingClient:
    """Minimal stand-in for ``Client`` that records workflow step enqueues
    and pretends to issue generation recorders."""

    def __init__(self) -> None:
        self.workflow_steps: list[WorkflowStep] = []
        self._counter = 0

    def enqueue_workflow_step(self, step: WorkflowStep) -> None:
        self.workflow_steps.append(step)

    def start_generation(self, start: Any) -> Any:
        return _DummyRecorder()

    def start_streaming_generation(self, start: Any) -> Any:
        return _DummyRecorder()

    def start_tool_execution(self, start: Any) -> Any:  # pragma: no cover - unused here
        return _DummyRecorder()


class _DummyRecorder:
    def set_first_token_at(self, *args: Any, **kwargs: Any) -> None: ...
    def set_result(self, *args: Any, **kwargs: Any) -> None: ...
    def set_call_error(self, *args: Any, **kwargs: Any) -> None: ...
    def end(self) -> None: ...
    def err(self) -> None:  # noqa: D401
        return None


def _make_handler(*, capture_workflow_steps: bool = True) -> tuple[Agento11yFrameworkHandlerBase, _CollectingClient]:
    client = _CollectingClient()
    handler = Agento11yFrameworkHandlerBase(
        client=client,  # type: ignore[arg-type]
        framework_name="langgraph",
        agent_name="my-pipeline",
        capture_workflow_steps=capture_workflow_steps,
    )
    return handler, client


def test_workflow_step_uses_handler_agent_name_not_step_name() -> None:
    handler, client = _make_handler()
    root = uuid4()
    step_run = uuid4()

    handler._on_chain_start(serialized=None, run_id=root, parent_run_id=None)
    handler._on_chain_start(serialized={"name": "router"}, run_id=step_run, parent_run_id=root, run_type="chain")
    handler._on_chain_end(run_id=step_run, outputs={"foo": "bar"})
    handler._on_chain_end(run_id=root, outputs={})

    assert len(client.workflow_steps) == 1
    step = client.workflow_steps[0]
    assert step.agent_name == "my-pipeline"
    assert step.framework == "langgraph"
    assert step.step_name  # step_name is computed from serialized/component
    assert step.step_name != step.agent_name


def test_linked_generation_ids_walks_chain_for_nested_llms() -> None:
    handler, client = _make_handler()
    root = uuid4()
    step_run = uuid4()
    nested_chain = uuid4()
    llm_run = uuid4()

    handler._on_chain_start(serialized=None, run_id=root, parent_run_id=None)
    handler._on_chain_start(serialized={"name": "agent"}, run_id=step_run, parent_run_id=root, run_type="chain")
    # A nested sub-chain inside the workflow step (e.g. LCEL composition).
    handler._on_chain_start(
        serialized={"name": "router"}, run_id=nested_chain, parent_run_id=step_run, run_type="chain"
    )
    handler._on_chat_model_start(
        serialized={"name": "ChatOpenAI", "kwargs": {"model": "gpt-4o"}},
        messages=[],
        run_id=llm_run,
        parent_run_id=nested_chain,
        invocation_params=None,
    )
    handler._on_llm_end(response={"generations": [[]]}, run_id=llm_run)
    handler._on_chain_end(run_id=nested_chain, outputs={})
    handler._on_chain_end(run_id=step_run, outputs={})
    handler._on_chain_end(run_id=root, outputs={})

    assert len(client.workflow_steps) == 1
    step = client.workflow_steps[0]
    assert len(step.linked_generation_ids) == 1, "LLM nested in sub-chain should be linked to workflow step"


def test_concurrent_graph_roots_are_isolated() -> None:
    handler, client = _make_handler()
    root_a = uuid4()
    root_b = uuid4()
    step_a = uuid4()
    step_b = uuid4()

    handler._on_chain_start(serialized=None, run_id=root_a, parent_run_id=None)
    handler._on_chain_start(serialized=None, run_id=root_b, parent_run_id=None)

    handler._on_chain_start(serialized={"name": "node_a"}, run_id=step_a, parent_run_id=root_a, run_type="chain")
    handler._on_chain_start(serialized={"name": "node_b"}, run_id=step_b, parent_run_id=root_b, run_type="chain")

    handler._on_chain_end(run_id=step_a, outputs={"r": "a"})
    handler._on_chain_end(run_id=step_b, outputs={"r": "b"})
    handler._on_chain_end(run_id=root_a, outputs={})
    handler._on_chain_end(run_id=root_b, outputs={})

    # Both should produce independent workflow steps with no parent links
    # (since they are the first node in their respective graphs).
    assert len(client.workflow_steps) == 2
    for step in client.workflow_steps:
        assert step.parent_step_ids == []


def test_graph_state_is_cleaned_up_after_root_ends() -> None:
    handler, _client = _make_handler()
    root = uuid4()
    step_run = uuid4()

    handler._on_chain_start(serialized=None, run_id=root, parent_run_id=None)
    handler._on_chain_start(serialized={"name": "router"}, run_id=step_run, parent_run_id=root, run_type="chain")
    handler._on_chain_end(run_id=step_run, outputs={})
    handler._on_chain_end(run_id=root, outputs={})

    assert handler._graph_root_run_keys == set()
    assert handler._graph_run_conversation_id == {}
    assert handler._graph_run_last_step_id == {}
    assert handler._workflow_step_runs == {}
    # The chain-end cleanup pops _run_to_graph_key entries belonging to the
    # finished root, so the map is now empty.
    assert handler._run_to_graph_key == {}


def test_sequential_workflow_steps_get_parent_step_ids() -> None:
    handler, client = _make_handler()
    root = uuid4()
    first = uuid4()
    second = uuid4()

    handler._on_chain_start(serialized=None, run_id=root, parent_run_id=None)
    handler._on_chain_start(serialized={"name": "first"}, run_id=first, parent_run_id=root, run_type="chain")
    handler._on_chain_end(run_id=first, outputs={})
    handler._on_chain_start(serialized={"name": "second"}, run_id=second, parent_run_id=root, run_type="chain")
    handler._on_chain_end(run_id=second, outputs={})
    handler._on_chain_end(run_id=root, outputs={})

    assert len(client.workflow_steps) == 2
    first_step, second_step = client.workflow_steps
    assert first_step.parent_step_ids == []
    assert second_step.parent_step_ids == [first_step.id]
