"""Experiment lifecycle and score export transport tests."""

from __future__ import annotations

import base64
import json
import threading
from datetime import timezone
from http.server import BaseHTTPRequestHandler, HTTPServer

import pytest
from agento11y import _experiments_transport as transport
from agento11y.errors import ConflictError, ConflictKind, NotFoundError, ScoreExportError, ValidationError
from agento11y.experiments import Client as ExperimentClient
from agento11y.models import (
    CreateExperimentRequest,
    ExperimentEvaluator,
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
                suite_id="suite-1",
                suite_version="v2",
                candidate={"agent_name": "agent-a", "model_name": "model-a"},
                planned_trial_count=30,
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
            "suite_id": "suite-1",
            "suite_version": "v2",
            "candidate": {"agent_name": "agent-a", "model_name": "model-a"},
            "planned_trial_count": 30,
            "metadata": {"git_sha": "abc"},
        }
        assert run.run_id == "run_1"
        assert run.status == "running"
        assert run.created_at is not None and run.created_at.tzinfo == timezone.utc
    finally:
        server.shutdown()
        server.server_close()


def test_create_experiment_sends_known_empty_plan() -> None:
    recorder = _Recorder()
    recorder.push(200, {"run": _experiment_body(planned_trial_count=0), "created": True})
    server = _serve(recorder)
    try:
        run = transport.create_experiment(
            **_args(server),
            request=CreateExperimentRequest(name="empty", planned_trial_count=0),
        )

        assert recorder.requests[0]["payload"]["planned_trial_count"] == 0
        assert run.planned_trial_count == 0
    finally:
        server.shutdown()
        server.server_close()


def test_create_experiment_rejects_negative_planned_trial_count_before_request() -> None:
    recorder = _Recorder()
    server = _serve(recorder)
    try:
        with pytest.raises(ValidationError, match="planned_trial_count must be non-negative"):
            transport.create_experiment(
                **_args(server),
                request=CreateExperimentRequest(name="invalid", planned_trial_count=-1),
            )

        assert recorder.requests == []
    finally:
        server.shutdown()
        server.server_close()


@pytest.mark.parametrize(
    "create_request",
    [
        CreateExperimentRequest(name="invalid", collection_id="collection-1"),
        CreateExperimentRequest(
            name="invalid",
            evaluators=[ExperimentEvaluator(id="judge", selector="all")],
        ),
    ],
)
def test_create_experiment_rejects_unsupported_collection_fields(
    create_request: CreateExperimentRequest,
) -> None:
    recorder = _Recorder()
    server = _serve(recorder)
    try:
        with pytest.raises(ValidationError, match="not supported"):
            transport.create_experiment(**_args(server), request=create_request)
        assert recorder.requests == []
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
        assert not hasattr(run, "score_count")
    finally:
        server.shutdown()
        server.server_close()


@pytest.mark.parametrize(
    ("message", "kind", "recoverable"),
    [
        ("cannot complete experiment with 2 running trial(s)", ConflictKind.RUNNING_TRIALS, True),
        ("expected 12 scores, found 11", ConflictKind.SCORE_COUNT_MISMATCH, True),
        (
            "planned_trial_count conflicts with the existing experiment",
            ConflictKind.IMMUTABLE_FIELD,
            False,
        ),
        ('experiment "run_1" is already finalized as completed', ConflictKind.TERMINAL, False),
        ("suite version is not a draft", ConflictKind.IMMUTABLE_FIELD, False),
        ("suite draft is already published", ConflictKind.TERMINAL, False),
        ("suite has an open draft", ConflictKind.OPEN_DRAFT, True),
    ],
)
def test_conflict_has_stable_kind_for_real_server_messages(
    message: str,
    kind: ConflictKind,
    recoverable: bool,
) -> None:
    recorder = _Recorder()
    recorder.push(409, {"error": message})
    server = _serve(recorder)
    try:
        with pytest.raises(ConflictError) as caught:
            transport.finalize_experiment(
                **_args(server),
                run_id="run_1",
                status="completed",
            )
        assert caught.value.kind is kind
        assert caught.value.recoverable is recoverable
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


def test_experiment_client_generation_uses_lightweight_transport_and_redacts() -> None:
    recorder = _Recorder()
    recorder.push(200, {"results": [{"generation_id": "gen-1", "accepted": True}]})
    server = _serve(recorder)
    try:
        client = ExperimentClient(
            f"http://127.0.0.1:{server.server_address[1]}",
            ingest_token="token",
        )
        client._ensure_core = lambda: pytest.fail("record_generation must not build the core client")
        client.record_generation(
            "gen-1",
            conversation_id="conv-1",
            input_text="token glc_abcdefghijklmnopqrstuvwxyz",
            output_text="done",
            input_tokens=4,
            output_tokens=2,
        )

        generation = recorder.requests[0]["payload"]["generations"][0]
        assert generation["mode"] == "GENERATION_MODE_SYNC"
        assert generation["input"][0]["parts"][0]["text"] == "token [REDACTED:grafana-cloud-token]"
        assert generation["usage"]["total_tokens"] == 6
    finally:
        server.shutdown()
        server.server_close()


def test_experiment_client_treats_duplicate_generation_as_success() -> None:
    recorder = _Recorder()
    recorder.push(
        200,
        {
            "results": [
                {
                    "generation_id": "gen-1",
                    "accepted": False,
                    "error": "generation already exists",
                }
            ]
        },
    )
    server = _serve(recorder)
    try:
        client = ExperimentClient(
            f"http://127.0.0.1:{server.server_address[1]}",
            ingest_token="token",
        )

        generation_id = client.record_generation("gen-1", output_text="done")

        assert generation_id == "gen-1"
        assert len(recorder.requests) == 1
    finally:
        server.shutdown()
        server.server_close()


def test_experiment_client_can_explicitly_disable_redaction() -> None:
    recorder = _Recorder()
    recorder.push(200, {"results": [{"generation_id": "gen-1", "accepted": True}]})
    server = _serve(recorder)
    try:
        client = ExperimentClient(
            f"http://127.0.0.1:{server.server_address[1]}",
            ingest_token="token",
            redact_secrets=False,
        )
        client.record_generation(
            "gen-1",
            conversation_id="conv-1",
            input_text="glc_abcdefghijklmnopqrstuvwxyz",
        )
        generation = recorder.requests[0]["payload"]["generations"][0]
        assert generation["input"][0]["parts"][0]["text"] == "glc_abcdefghijklmnopqrstuvwxyz"
    finally:
        server.shutdown()
        server.server_close()


def test_experiment_client_redacts_scores_and_text_artifacts_without_mutating_input() -> None:
    recorder = _Recorder()
    recorder.push(200, {"results": [{"score_id": "score-1", "accepted": True}]})
    recorder.push(200, {"artifact_id": "artifact-1", "name": "log", "kind": "text"})
    server = _serve(recorder)
    secret = "glc_abcdefghijklmnopqrstuvwxyz"
    try:
        client = ExperimentClient(
            f"http://127.0.0.1:{server.server_address[1]}",
            ingest_token="token",
        )
        score = ScoreItem(
            score_id="score-1",
            trial_id="trial-1",
            evaluator_id="judge",
            evaluator_version="1",
            score_key="final",
            value=ScoreValue(string=secret),
            explanation=f"found {secret}",
            metadata={"raw": secret},
        )
        client.export_scores([score])
        client.upload_artifact(
            experiment_id="run-1",
            parent_id="trial-1",
            name="log",
            kind="text",
            mime="text/plain",
            content=f"artifact {secret}".encode(),
        )

        exported_score = recorder.requests[0]["payload"]["scores"][0]
        assert secret not in json.dumps(exported_score)
        assert secret not in recorder.requests[1]["raw_payload"].decode()
        assert score.value.string == secret
        assert score.explanation == f"found {secret}"
    finally:
        server.shutdown()
        server.server_close()


def test_experiment_client_redacts_non_utf8_text_artifact_without_changing_encoding() -> None:
    recorder = _Recorder()
    recorder.push(200, {"artifact_id": "artifact-1", "name": "log", "kind": "text"})
    server = _serve(recorder)
    secret = b"glc_abcdefghijklmnopqrstuvwxyz"
    try:
        client = ExperimentClient(
            f"http://127.0.0.1:{server.server_address[1]}",
            ingest_token="token",
        )
        client.upload_artifact(
            experiment_id="run-1",
            parent_id="trial-1",
            name="log",
            kind="text",
            mime="text/plain; charset=iso-8859-1",
            content=b"artifact " + secret + b" caf\xe9",
        )

        uploaded = recorder.requests[0]["raw_payload"]
        assert secret not in uploaded
        assert uploaded.endswith(b" caf\xe9")
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
        assert request["headers"]["x-sigil-ingest-actor"] == "ingest:sdk/python"
        assert request["raw_payload"] == b'{"ok":true}'
        assert record["artifact_id"] == "art-1"
    finally:
        server.shutdown()
        server.server_close()


def test_experiment_client_uses_one_default_actor_for_all_lifecycle_requests() -> None:
    client = ExperimentClient("http://example.test", ingest_token="token")

    assert client.actor == "ingest:sdk/python"
    assert client._headers()["X-Sigil-Ingest-Actor"] == "ingest:sdk/python"  # noqa: SLF001


def test_experiment_client_preserves_explicit_actor() -> None:
    client = ExperimentClient("http://example.test", ingest_token="token", actor=" runner/harbor ")

    assert client.actor == "runner/harbor"


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
