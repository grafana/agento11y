"""Experiment lifecycle and score export transport tests."""

from __future__ import annotations

import base64
import json
import threading
from datetime import timezone
from http.server import BaseHTTPRequestHandler, HTTPServer

import pytest
from sigil_sdk import _experiments_transport as transport
from sigil_sdk.errors import NotFoundError, ScoreExportError, ValidationError
from sigil_sdk.experiments import Client as ExperimentClient
from sigil_sdk.models import (
    CreateExperimentRequest,
    ExperimentStatus,
    ScoreItem,
    ScoreSource,
    ScoreValue,
)


class _Recorder:
    """Captures the last request and serves a scripted response sequence."""

    def __init__(self) -> None:
        self.requests: list[dict[str, object]] = []
        self.responses: list[tuple[int, object]] = []
        self.lock = threading.Lock()

    def push(self, status: int, body: object) -> None:
        self.responses.append((status, body))

    def take(self) -> tuple[int, object]:
        with self.lock:
            if len(self.responses) == 1:
                return self.responses[0]
            return self.responses.pop(0)


def _make_handler(recorder: _Recorder):
    class _Handler(BaseHTTPRequestHandler):
        def _handle(self) -> None:  # noqa: N802
            length = int(self.headers.get("Content-Length", "0"))
            raw = self.rfile.read(length) if length else b""
            recorder.requests.append(
                {
                    "method": self.command,
                    "path": self.path,
                    "headers": {k.lower(): v for k, v in self.headers.items()},
                    "raw_payload": raw,
                    "payload": json.loads(raw.decode("utf-8"))
                    if raw and self.headers.get("Content-Type") == "application/json"
                    else raw,
                }
            )
            status, body = recorder.take()
            encoded = json.dumps(body).encode("utf-8")
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(encoded)))
            self.end_headers()
            self.wfile.write(encoded)

        do_GET = _handle
        do_POST = _handle
        do_PATCH = _handle

        def log_message(self, _format, *_args):  # noqa: A003
            return

    return _Handler


def _serve(recorder: _Recorder) -> HTTPServer:
    server = HTTPServer(("127.0.0.1", 0), _make_handler(recorder))
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    return server


def _args(server: HTTPServer) -> dict:
    return {
        "api_endpoint": f"http://127.0.0.1:{server.server_address[1]}",
        "insecure": True,
        "headers": {
            "X-Scope-OrgID": "tenant-a",
            "Authorization": _basic("tenant-a", "ingest-token-a"),
        },
        "retry": transport.RetryPolicy(max_retries=2, initial_backoff=0.001, max_backoff=0.005),
    }


def _experiment_body(**overrides: object) -> dict[str, object]:
    body = {
        "tenant_id": "tenant-a",
        "experiment_id": "run_1",
        "name": "PR 123",
        "source": "external",
        "status": "running",
        "score_count": 0,
        "created_at": "2026-05-28T12:00:00Z",
        "updated_at": "2026-05-28T12:00:00Z",
    }
    body.update(overrides)
    return body


def test_create_experiment_upserts_external_run() -> None:
    recorder = _Recorder()
    recorder.push(200, {"run": _experiment_body(tags=["smoke"], metadata={"git_sha": "abc"}), "created": True})
    server = _serve(recorder)
    try:
        run = transport.create_experiment(
            **_args(server),
            request=CreateExperimentRequest(
                run_id="run_1",
                name="PR 123",
                source="external",
                tags=["smoke"],
                metadata={"git_sha": "abc"},
            ),
        )
        request = recorder.requests[0]
        assert request["method"] == "POST"
        assert request["path"] == "/api/v1/experiment-runs:upsert"
        assert request["headers"]["x-scope-orgid"] == "tenant-a"
        assert request["headers"]["authorization"] == _basic("tenant-a", "ingest-token-a")
        assert request["payload"] == {
            "name": "PR 123",
            "source": {"kind": "sdk", "id": "python"},
            "experiment_id": "run_1",
            "tags": ["smoke"],
            "metadata": {"git_sha": "abc"},
        }
        assert run.run_id == "run_1"
        assert run.status == "running"
        assert run.created_at is not None and run.created_at.tzinfo == timezone.utc
    finally:
        server.shutdown()
        server.server_close()


def test_complete_experiment_finalizes_run() -> None:
    recorder = _Recorder()
    recorder.push(200, {"run": _experiment_body(status="completed", score_count=3)})
    server = _serve(recorder)
    try:
        # SUCCEEDED is a friendly alias; the backend's terminal status is `completed`.
        run = transport.finalize_experiment(
            **_args(server),
            run_id="run_1",
            status=ExperimentStatus.SUCCEEDED,
            score_count=3,
        )
        request = recorder.requests[0]
        assert request["method"] == "POST"
        assert request["path"] == "/api/v1/experiment-runs/run_1:finalize"
        assert request["payload"] == {
            "status": "completed",
            "score_count": 3,
            "source": {"kind": "sdk", "id": "python"},
        }
        assert run.status == "completed"
        assert run.score_count == 3
    finally:
        server.shutdown()
        server.server_close()


def test_export_scores_round_trip_and_accepted_count() -> None:
    recorder = _Recorder()
    recorder.push(
        202,
        {"results": [{"score_id": "sc1", "accepted": True}, {"score_id": "sc2", "accepted": False, "error": "bad"}]},
    )
    server = _serve(recorder)
    try:
        response = transport.export_scores(
            **_args(server),
            scores=[
                ScoreItem(
                    score_id="sc1",
                    generation_id="gen1",
                    conversation_id="conv1",
                    run_id="run_1",
                    evaluator_id="smoke.reward",
                    evaluator_version="2026-05-28",
                    score_key="reward",
                    value=ScoreValue(number=0.82),
                    passed=True,
                    metadata={"task_id": "t1"},
                    source=ScoreSource(kind="experiment", id="run_1"),
                ),
                ScoreItem(
                    score_id="sc2",
                    generation_id="gen2",
                    run_id="run_1",
                    evaluator_id="smoke.reward",
                    evaluator_version="2026-05-28",
                    score_key="pass",
                    value=ScoreValue(boolean=True),
                ),
            ],
        )
        request = recorder.requests[0]
        assert request["path"] == "/api/v1/scores:export"
        scores = request["payload"]["scores"]
        assert scores[0]["value"] == {"number": 0.82}
        assert scores[0]["experiment_id"] == "run_1"
        assert "run_id" not in scores[0]  # backend has no run_id field
        assert scores[0]["source"] == {"kind": "experiment", "id": "run_1"}
        assert scores[1]["value"] == {"bool": True}
        assert response.accepted_count == 1
        assert [r.score_id for r in response.rejected] == ["sc2"]
    finally:
        server.shutdown()
        server.server_close()


def test_experiment_client_uploads_trial_artifact_to_ingest_route() -> None:
    recorder = _Recorder()
    recorder.push(200, {"artifact_id": "art-1", "name": "details", "kind": "json"})
    server = _serve(recorder)
    endpoint = f"http://127.0.0.1:{server.server_address[1]}"
    try:
        client = ExperimentClient(endpoint, tenant_id="tenant-a", ingest_token="ingest-token-a")
        record = client.upload_artifact(
            experiment_id="run_1",
            parent_id="trial_1",
            name="details",
            kind="json",
            mime="application/json",
            content=b'{"ok":true}',
        )
        request = recorder.requests[0]
        assert request["method"] == "POST"
        expected_path = (
            "/api/v1/experiment-runs/run_1/trials/trial_1/artifacts:upload"
            "?name=details&kind=json&mime=application%2Fjson"
        )
        assert request["path"] == expected_path
        assert request["headers"]["authorization"] == _basic("tenant-a", "ingest-token-a")
        assert request["raw_payload"] == b'{"ok":true}'
        assert record["artifact_id"] == "art-1"
    finally:
        server.shutdown()
        server.server_close()


def test_export_scores_validates_missing_value() -> None:
    server = _serve(_Recorder())
    try:
        with pytest.raises(ValidationError):
            transport.export_scores(
                **_args(server),
                scores=[
                    ScoreItem(
                        score_id="sc1",
                        generation_id="gen1",
                        evaluator_id="ev",
                        evaluator_version="v1",
                        score_key="reward",
                        value=ScoreValue(),
                    )
                ],
            )
    finally:
        server.shutdown()
        server.server_close()


def test_get_experiment_maps_not_found() -> None:
    recorder = _Recorder()
    recorder.push(404, {"error": "missing"})
    server = _serve(recorder)
    try:
        with pytest.raises(NotFoundError):
            transport.get_experiment(**_args(server), run_id="run_missing")
    finally:
        server.shutdown()
        server.server_close()


def test_export_scores_retries_then_succeeds_on_5xx() -> None:
    recorder = _Recorder()
    recorder.push(503, {"error": "unavailable"})
    recorder.push(202, {"results": [{"score_id": "sc1", "accepted": True}]})
    server = _serve(recorder)
    try:
        response = transport.export_scores(
            **_args(server),
            scores=[
                ScoreItem(
                    score_id="sc1",
                    generation_id="gen1",
                    evaluator_id="ev",
                    evaluator_version="v1",
                    score_key="reward",
                    value=ScoreValue(number=1.0),
                )
            ],
        )
        assert response.accepted_count == 1
        assert len(recorder.requests) == 2  # one retry after the 503
    finally:
        server.shutdown()
        server.server_close()


def test_export_scores_exhausts_retries_and_raises() -> None:
    recorder = _Recorder()
    recorder.push(500, {"error": "boom"})  # single scripted response reused for all attempts
    server = _serve(recorder)
    try:
        with pytest.raises(ScoreExportError):
            transport.export_scores(
                **_args(server),
                scores=[
                    ScoreItem(
                        score_id="sc1",
                        generation_id="gen1",
                        evaluator_id="ev",
                        evaluator_version="v1",
                        score_key="reward",
                        value=ScoreValue(number=1.0),
                    )
                ],
            )
        # initial attempt + max_retries (2) = 3 requests
        assert len(recorder.requests) == 3
    finally:
        server.shutdown()
        server.server_close()


def test_experiment_lifecycle_and_scores_share_configured_auth() -> None:
    recorder = _Recorder()
    recorder.push(200, {"run": _experiment_body()})  # create_experiment
    recorder.push(202, {"results": [{"score_id": "sc1", "accepted": True}]})  # export_scores
    server = _serve(recorder)
    try:
        transport.create_experiment(
            **_args(server),
            request=CreateExperimentRequest(run_id="run_1", name="shared auth", source="external"),
        )
        create_req = recorder.requests[0]
        assert create_req["path"] == "/api/v1/experiment-runs:upsert"
        assert create_req["headers"].get("x-scope-orgid") == "tenant-a"
        assert create_req["headers"].get("authorization") == _basic("tenant-a", "ingest-token-a")

        transport.export_scores(
            **_args(server),
            scores=[
                ScoreItem(
                    score_id="sc1",
                    generation_id="gen1",
                    evaluator_id="ev",
                    evaluator_version="v1",
                    score_key="reward",
                    value=ScoreValue(number=1.0),
                    run_id="run_1",
                )
            ],
        )
        score_req = recorder.requests[1]
        assert score_req["path"] == "/api/v1/scores:export"
        assert score_req["headers"].get("x-scope-orgid") == "tenant-a"
        assert score_req["headers"].get("authorization") == _basic("tenant-a", "ingest-token-a")
    finally:
        server.shutdown()
        server.server_close()


def _basic(user: str, password: str) -> str:
    return "Basic " + base64.b64encode(f"{user}:{password}".encode()).decode()


def test_get_experiment_report_parses_summary() -> None:
    recorder = _Recorder()
    recorder.push(
        200,
        {
            # Backend keys the run under `experiment`, rows under `rows`, cost as `total_cost`.
            "experiment": _experiment_body(status="completed"),
            "summary": {
                "test_case_count": 2,
                "trial_count": 3,
                "completed_count": 3,
                "pass_rate": 0.66,
                "pass_at_k": {"1": 0.66},
                "pass_power_k": {"1": 0.66},
                "final_score_avg": 0.8,
                "total_cost": 0.5,
                "total_tokens": 1200,
            },
            "rows": [{"test_case_id": "t1", "trials": []}],
        },
    )
    server = _serve(recorder)
    try:
        report = transport.get_experiment_report(**_args(server), run_id="run_1")
        assert recorder.requests[0]["path"] == "/api/v1/eval/experiments/run_1/report"
        assert report.run.status == "completed"
        assert report.summary.test_case_count == 2
        assert report.summary.pass_rate == pytest.approx(0.66)
        assert report.summary.final_score_avg == pytest.approx(0.8)
        assert report.summary.total_cost == pytest.approx(0.5)
        assert report.summary.total_tokens == 1200
        assert report.summary.pass_at_k == {"1": 0.66}
        assert report.rows[0]["test_case_id"] == "t1"
    finally:
        server.shutdown()
        server.server_close()
