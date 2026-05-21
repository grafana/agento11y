"""Experiment and score transport tests."""

from __future__ import annotations

import json
import threading
from datetime import timedelta
from http.server import BaseHTTPRequestHandler, HTTPServer

import pytest
from opentelemetry import trace
from sigil_sdk import (
    ApiConfig,
    AuthConfig,
    CandidateRef,
    Client,
    ClientConfig,
    CreateExperimentRequest,
    DatasetItem,
    DatasetRef,
    DatasetTargetResult,
    EvalConflictError,
    ExperimentSpec,
    GenerationExportConfig,
    ScoreItem,
    ScoreOutput,
    ScoreSource,
    ScoreValue,
    UpdateExperimentRequest,
)


def test_experiment_and_score_http_round_trip() -> None:
    captured: list[dict[str, object]] = []

    class _Handler(BaseHTTPRequestHandler):
        def do_POST(self):  # noqa: N802
            payload = _read_json(self)
            captured.append({"method": "POST", "path": self.path, "payload": payload, "headers": _headers(self)})
            if self.path == "/api/v1/eval/experiments":
                _write_json(
                    self,
                    {
                        "tenant_id": "tenant-a",
                        "run_id": payload["run_id"],
                        "name": payload["name"],
                        "source": "external",
                        "status": "running",
                        "score_count": 0,
                        "created_at": "2026-05-20T12:00:00Z",
                        "updated_at": "2026-05-20T12:00:00Z",
                    },
                )
                return
            if self.path == "/api/v1/scores:export":
                _write_json(
                    self,
                    {"results": [{"score_id": payload["scores"][0]["score_id"], "accepted": True}]},
                    status=202,
                )
                return
            self.send_error(404)

        def do_PATCH(self):  # noqa: N802
            payload = _read_json(self)
            captured.append({"method": "PATCH", "path": self.path, "payload": payload, "headers": _headers(self)})
            _write_json(
                self,
                {
                    "tenant_id": "tenant-a",
                    "run_id": "run-sdk",
                    "name": "SDK run",
                    "source": "external",
                    "status": payload["status"],
                    "score_count": payload["score_count"],
                    "created_at": "2026-05-20T12:00:00Z",
                    "updated_at": "2026-05-20T12:01:00Z",
                    "completed_at": "2026-05-20T12:01:00Z",
                },
            )

        def do_GET(self):  # noqa: N802
            captured.append({"method": "GET", "path": self.path, "headers": _headers(self)})
            _write_json(
                self,
                {
                    "run": {
                        "tenant_id": "tenant-a",
                        "run_id": "run-sdk",
                        "name": "SDK run",
                        "source": "external",
                        "status": "succeeded",
                        "score_count": 1,
                        "created_at": "2026-05-20T12:00:00Z",
                        "updated_at": "2026-05-20T12:01:00Z",
                    },
                    "summary": {"n_scores": 1, "mean_score": 0.9},
                    "breakdowns": {},
                    "points": [],
                },
            )

        def log_message(self, _format, *_args):  # noqa: A003
            return

    server = _server(_Handler)
    client = _new_client(server)

    try:
        created = client.create_experiment(
            CreateExperimentRequest(
                run_id="run-sdk",
                name="SDK run",
                tags=["o11y-bench"],
                metadata={"dataset_id": "tiny"},
            )
        )
        response = client.export_scores(
            [
                ScoreItem(
                    score_id="sc-1",
                    generation_id="gen-1",
                    conversation_id="conv-1",
                    evaluator_id="bench.correctness",
                    evaluator_version="v1",
                    score_key="correctness",
                    value=ScoreValue(number=0.9),
                    run_id="run-sdk",
                    passed=True,
                    metadata={"task_id": "task-1"},
                    source=ScoreSource(kind="experiment", id="run-sdk"),
                )
            ]
        )
        completed = client.update_experiment("run-sdk", UpdateExperimentRequest(status="succeeded", score_count=1))
        report = client.get_experiment_report("run-sdk")

        assert created.run_id == "run-sdk"
        assert response.accepted_count == 1
        assert completed.status == "succeeded"
        assert completed.score_count == 1
        assert report.summary["n_scores"] == 1
        assert captured[0]["path"] == "/api/v1/eval/experiments"
        assert captured[0]["headers"]["x-scope-orgid"] == "tenant-a"
        assert captured[1]["path"] == "/api/v1/scores:export"
        assert captured[1]["payload"]["scores"][0]["source"] == {"kind": "experiment", "id": "run-sdk"}
        assert captured[2]["payload"]["score_count"] == 1
    finally:
        client.shutdown()
        server.shutdown()
        server.server_close()


def test_experiment_runner_exports_and_finalizes() -> None:
    captured: list[dict[str, object]] = []

    class _Handler(BaseHTTPRequestHandler):
        def do_POST(self):  # noqa: N802
            payload = _read_json(self)
            captured.append({"method": "POST", "path": self.path, "payload": payload})
            if self.path == "/api/v1/eval/experiments":
                _write_json(self, _experiment_payload(payload["run_id"], payload["name"], "running", 0))
                return
            if self.path == "/api/v1/scores:export":
                _write_json(
                    self,
                    {
                        "results": [
                            {"score_id": score["score_id"], "accepted": True} for score in payload.get("scores", [])
                        ]
                    },
                    status=202,
                )
                return
            self.send_error(404)

        def do_PATCH(self):  # noqa: N802
            payload = _read_json(self)
            captured.append({"method": "PATCH", "path": self.path, "payload": payload})
            _write_json(self, _experiment_payload("run-runner", "Runner", payload["status"], payload["score_count"]))

        def do_GET(self):  # noqa: N802
            _write_json(
                self,
                {
                    "run": _experiment_payload("run-runner", "Runner", "succeeded", 1),
                    "summary": {"n_scores": 1, "mean_score": 1},
                    "breakdowns": {},
                    "points": [{"score_id": "sc-1"}],
                },
            )

        def log_message(self, _format, *_args):  # noqa: A003
            return

    server = _server(_Handler)
    client = _new_client(server)

    def target(item: DatasetItem[str, str]) -> DatasetTargetResult[str]:
        return DatasetTargetResult(output="ok", generation_ids=["gen-1"], conversation_id="conv-1")

    def scorer(item: DatasetItem[str, str], result: DatasetTargetResult[str]) -> list[ScoreOutput]:
        return [
            ScoreOutput(
                evaluator_id="bench.correctness",
                evaluator_version="v1",
                score_key="correctness",
                value=ScoreValue(boolean=True),
                passed=True,
                metadata={"task_id": item.id},
            )
        ]

    try:
        runner = client.create_experiment_runner(
            ExperimentSpec(
                run_id="run-runner",
                name="Runner",
                tags=["o11y-bench"],
                dataset=DatasetRef(id="tiny", version="v1"),
                candidate=CandidateRef(agent_name="agent", agent_version="v1"),
            ),
            score_batch_size=1,
        )
        result = runner.run([DatasetItem(id="task-1", input="prompt", expected="ok")], target, [scorer])

        assert result.exported_scores == 1
        score_payload = captured[1]["payload"]["scores"][0]
        assert score_payload["metadata"]["dataset_id"] == "tiny"
        assert score_payload["metadata"]["item_id"] == "task-1"
        assert score_payload["metadata"]["candidate"]["agent_name"] == "agent"
        assert score_payload["value"] == {"bool": True}
        assert captured[2]["payload"] == {"status": "succeeded", "score_count": 1}
    finally:
        client.shutdown()
        server.shutdown()
        server.server_close()


def test_experiment_runner_surfaces_original_error_when_cleanup_also_fails(caplog) -> None:
    from sigil_sdk.experiments import Experiment, ExperimentRunner

    class _StubClient:
        def __init__(self) -> None:
            self.update_calls = 0

        def create_experiment(self, _request):
            return Experiment(run_id="run-x", name="Runner", source="external", status="running")

        def flush(self) -> None:
            return None

        def update_experiment(self, _run_id, _request):
            self.update_calls += 1
            raise RuntimeError("cleanup PATCH exploded")

    spec = ExperimentSpec(
        run_id="run-x",
        name="Runner",
        dataset=DatasetRef(id="tiny"),
    )
    stub = _StubClient()
    runner = ExperimentRunner(stub, spec)

    def target(_item: DatasetItem[str, str]) -> DatasetTargetResult[str]:
        raise ValueError("target blew up")

    def scorer(_item, _result):
        return []

    with caplog.at_level("WARNING", logger="sigil_sdk.experiments"):
        with pytest.raises(ValueError, match="target blew up"):
            runner.run([DatasetItem(id="task-1", input="x")], target, [scorer])

    assert stub.update_calls == 1
    assert any("cleanup PATCH exploded" in record.getMessage() for record in caplog.records)


def test_create_experiment_maps_conflict() -> None:
    class _Handler(BaseHTTPRequestHandler):
        def do_POST(self):  # noqa: N802
            encoded = b"experiment already exists"
            self.send_response(409)
            self.send_header("Content-Type", "text/plain")
            self.send_header("Content-Length", str(len(encoded)))
            self.end_headers()
            self.wfile.write(encoded)

        def log_message(self, _format, *_args):  # noqa: A003
            return

    server = _server(_Handler)
    client = _new_client(server)

    try:
        with pytest.raises(EvalConflictError):
            client.create_experiment(CreateExperimentRequest(run_id="run-1", name="Run"))
    finally:
        client.shutdown()
        server.shutdown()
        server.server_close()


def _new_client(server: HTTPServer) -> Client:
    return Client(
        ClientConfig(
            generation_export=GenerationExportConfig(
                protocol="none",
                auth=AuthConfig(mode="tenant", tenant_id="tenant-a"),
                batch_size=1,
                flush_interval=timedelta(seconds=1),
                max_retries=1,
                initial_backoff=timedelta(milliseconds=1),
                max_backoff=timedelta(milliseconds=10),
            ),
            api=ApiConfig(endpoint=f"http://127.0.0.1:{server.server_address[1]}"),
            tracer=trace.get_tracer("sigil-sdk-python-experiment-test"),
        )
    )


def _server(handler: type[BaseHTTPRequestHandler]) -> HTTPServer:
    server = HTTPServer(("127.0.0.1", 0), handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    return server


def _read_json(handler: BaseHTTPRequestHandler) -> dict[str, object]:
    length = int(handler.headers.get("Content-Length", "0"))
    if length == 0:
        return {}
    return json.loads(handler.rfile.read(length).decode("utf-8"))


def _write_json(handler: BaseHTTPRequestHandler, payload: dict[str, object], status: int = 200) -> None:
    encoded = json.dumps(payload).encode("utf-8")
    handler.send_response(status)
    handler.send_header("Content-Type", "application/json")
    handler.send_header("Content-Length", str(len(encoded)))
    handler.end_headers()
    handler.wfile.write(encoded)


def _headers(handler: BaseHTTPRequestHandler) -> dict[str, str]:
    return {key.lower(): value for key, value in handler.headers.items()}


def _experiment_payload(run_id: str, name: str, status: str, score_count: int) -> dict[str, object]:
    return {
        "tenant_id": "tenant-a",
        "run_id": run_id,
        "name": name,
        "source": "external",
        "status": status,
        "score_count": score_count,
        "created_at": "2026-05-20T12:00:00Z",
        "updated_at": "2026-05-20T12:01:00Z",
    }
