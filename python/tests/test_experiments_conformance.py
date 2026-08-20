"""Experiment wire conformance for the Python SDK.

Checks the requests the SDK serializes and the responses it parses against the
shared fixtures in ``conformance/experiments/``, which are the only contract for
routes with no generated stubs. Go and JavaScript run the same fixtures; see
``conformance/experiments/README.md``.
"""

from __future__ import annotations

import json
import threading
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from typing import Any

import pytest
from agento11y import _experiments_transport as transport
from agento11y.errors import ExperimentTransportError
from agento11y.experiments import Client as ExperimentClient
from agento11y.experiments import Experiment, TestSuite, stable_id
from agento11y.models import CreateExperimentRequest, ScoreItem, ScoreSource, ScoreValue

# What this SDK substitutes for the ${SDK_ID} placeholder.
SDK_ID = "python"

FIXTURE_DIR = Path(__file__).resolve().parents[2] / "conformance" / "experiments"


def _load(name: str) -> Any:
    return json.loads((FIXTURE_DIR / name).read_text(encoding="utf-8"))


def _substitute_sdk_id(value: Any) -> Any:
    """Replaces the ``${SDK_ID}`` placeholder every ``source`` object carries."""

    if isinstance(value, str):
        return SDK_ID if value == "${SDK_ID}" else value
    if isinstance(value, list):
        return [_substitute_sdk_id(item) for item in value]
    if isinstance(value, dict):
        return {key: _substitute_sdk_id(item) for key, item in value.items()}
    return value


INPUTS = _load("inputs.json")
REQUESTS = _substitute_sdk_id(_load("requests.json"))
RESPONSES = _load("responses.json")
TRIAL_ID = stable_id("trial", INPUTS["experiment_id"], INPUTS["test_case_id"], INPUTS["attempt"])


class _Recorder:
    """Captures every request and answers with a path-aware canned body."""

    def __init__(self) -> None:
        self.requests: list[dict[str, Any]] = []
        self.lock = threading.Lock()

    def respond(self, path: str) -> Any:
        if path.endswith(":evaluate") or "/evaluations/" in path:
            return RESPONSES["evaluation_queued"]
        if path.endswith(":upsert"):
            return RESPONSES["run_upsert_response"]
        if path.endswith(":finalize"):
            return RESPONSES["run_finalize_response"]
        if path == "/api/v1/scores:export":
            return RESPONSES["scores_export_response"]
        return {"trial_id": TRIAL_ID}


def _make_handler(recorder: _Recorder, override: Any = None):
    class _Handler(BaseHTTPRequestHandler):
        def _handle(self) -> None:
            length = int(self.headers.get("Content-Length", "0"))
            raw = self.rfile.read(length) if length else b""
            path = self.path.split("?", 1)[0]
            with recorder.lock:
                recorder.requests.append(
                    {
                        "method": self.command,
                        "path": path,
                        "headers": {k.lower(): v for k, v in self.headers.items()},
                        "body": json.loads(raw.decode("utf-8")) if raw else None,
                        "raw": raw.decode("utf-8"),
                    }
                )
            body = override if override is not None else recorder.respond(path)
            encoded = json.dumps(body).encode("utf-8")
            self.send_response(200)
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


def _capture(call, override: Any = None) -> list[dict[str, Any]]:
    recorder = _Recorder()
    server = HTTPServer(("127.0.0.1", 0), _make_handler(recorder, override))
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        client = ExperimentClient(
            f"http://127.0.0.1:{server.server_address[1]}",
            ingest_token="token",
            redact_secrets=False,
            use_experimental_otel=False,
        )
        call(client)
    finally:
        server.shutdown()
        server.server_close()
    return recorder.requests


def _diff_json(got: Any, want: Any, path: str = "") -> list[str]:
    """Reports differences as dotted JSON paths, ignoring a fixture's ``comment``."""

    label = path or "<root>"
    if isinstance(want, dict):
        if not isinstance(got, dict):
            return [f"{label}: got {got!r}, want an object"]
        differences: list[str] = []
        want_keys = [key for key in want if key != "comment"]
        for key in got:
            if key not in want_keys:
                differences.append(f"{path}.{key}: unexpected field {got[key]!r}")
        for key in want_keys:
            if key not in got:
                differences.append(f"{path}.{key}: missing, want {want[key]!r}")
                continue
            differences.extend(_diff_json(got[key], want[key], f"{path}.{key}"))
        return differences
    if isinstance(want, list):
        if not isinstance(got, list):
            return [f"{label}: got {got!r}, want an array"]
        if len(got) != len(want):
            return [f"{label}: got {len(got)} items, want {len(want)}"]
        return [
            difference
            for index, item in enumerate(want)
            for difference in _diff_json(got[index], item, f"{path}[{index}]")
        ]
    if got != want:
        return [f"{label}: got {got!r}, want {want!r}"]
    return []


def _assert_matches(captured: dict[str, Any], fixture: dict[str, Any]) -> None:
    assert captured["method"] == fixture["method"]
    assert captured["path"] == fixture["path"]
    differences = _diff_json(captured["body"], fixture["body"])
    assert differences == [], "body differs from the fixture:\n" + "\n".join(differences)


def test_stable_ids_match_the_shared_vectors() -> None:
    vectors = _load("ids.json")["vectors"]
    assert vectors
    for vector in vectors:
        parts = [int(part) if isinstance(part, float) and part.is_integer() else part for part in vector["parts"]]
        assert stable_id(vector["prefix"], *parts) == vector["id"], vector


def test_run_upsert_matches_the_fixture() -> None:
    captured = _capture(
        lambda client: client.upsert_experiment(
            CreateExperimentRequest(
                experiment_id=INPUTS["experiment_id"],
                name=INPUTS["experiment_name"],
                source="external",
                tags=INPUTS["tags"],
                suite_id=INPUTS["suite_id"],
                suite_version=INPUTS["suite_version"],
                planned_trial_count=INPUTS["planned_trial_count"],
            )
        )
    )
    _assert_matches(captured[0], REQUESTS["run_upsert"])


def test_trial_create_matches_the_fixture() -> None:
    captured = _capture(
        lambda client: client.upsert_trial(
            INPUTS["experiment_id"],
            trial_id=TRIAL_ID,
            test_case_id=INPUTS["test_case_id"],
            attempt=INPUTS["attempt"],
        )
    )
    _assert_matches(captured[0], REQUESTS["trial_create"])


@pytest.mark.parametrize(
    ("fixture", "kwargs"),
    [
        ("trial_patch_conversation", {"conversation_id": INPUTS["conversation_id"]}),
        (
            "trial_patch_terminal",
            {"status": "completed", "conversation_id": INPUTS["conversation_id"]},
        ),
    ],
)
def test_trial_patches_match_the_fixtures(fixture: str, kwargs: dict[str, Any]) -> None:
    captured = _capture(lambda client: client.update_trial(INPUTS["experiment_id"], TRIAL_ID, **kwargs))
    _assert_matches(captured[0], REQUESTS[fixture])


@pytest.mark.parametrize(
    ("fixture", "trial_id", "version"),
    [
        ("trial_evaluate", TRIAL_ID, INPUTS["evaluator_version"]),
        ("trial_evaluate_latest_version", TRIAL_ID, ""),
        ("trial_evaluate_reserved_trial_id", "trial:one/blue", ""),
    ],
)
def test_evaluate_triggers_match_the_fixtures(fixture: str, trial_id: str, version: str) -> None:
    captured = _capture(
        lambda client: client.trigger_trial_evaluation(
            INPUTS["experiment_id"], trial_id, INPUTS["evaluator_id"], version
        )
    )
    _assert_matches(captured[0], REQUESTS[fixture])


def test_evaluation_status_read_matches_the_fixture() -> None:
    captured = _capture(
        lambda client: client.get_trial_evaluation(INPUTS["experiment_id"], TRIAL_ID, INPUTS["evaluation_id"])
    )
    fixture = REQUESTS["trial_evaluation_status"]
    assert captured[0]["method"] == fixture["method"]
    assert captured[0]["path"] == fixture["path"]
    assert captured[0]["raw"] == "", "a status read sends no body"


def test_score_export_matches_the_fixture() -> None:
    score = ScoreItem(
        score_id=stable_id(
            "score",
            INPUTS["experiment_id"],
            TRIAL_ID,
            INPUTS["score_key"],
            INPUTS["score_evaluator_id"],
        ),
        evaluator_id=INPUTS["score_evaluator_id"],
        evaluator_version=INPUTS["score_evaluator_version"],
        # Local-only on purpose: no SDK puts the evaluator kind on the wire.
        evaluator_kind="deterministic",
        score_key=INPUTS["score_key"],
        value=ScoreValue(boolean=True),
        conversation_id=INPUTS["conversation_id"],
        experiment_id=INPUTS["experiment_id"],
        trial_id=TRIAL_ID,
        test_case_id=INPUTS["test_case_id"],
        passed=True,
        explanation="matched the expected answer",
        created_at=datetime(2026, 1, 1, tzinfo=timezone.utc),
        source=ScoreSource(kind="experiment", id=INPUTS["experiment_id"]),
    )
    captured = _capture(lambda client: client.export_scores([score]))
    _assert_matches(captured[0], REQUESTS["scores_export"])


@pytest.mark.parametrize(
    ("fixture", "score_count"),
    [("run_finalize", None), ("run_finalize_with_score_count", 1)],
)
def test_finalize_matches_the_fixture(fixture: str, score_count: int | None) -> None:
    captured = _capture(lambda client: client.finalize(INPUTS["experiment_id"], "completed", score_count=score_count))
    _assert_matches(captured[0], REQUESTS[fixture])


@pytest.mark.parametrize(
    ("name", "status"),
    [
        ("evaluation_queued", "queued"),
        ("evaluation_claimed", "claimed"),
        ("evaluation_success", "success"),
        ("evaluation_failed", "failed"),
    ],
)
def test_canned_evaluations_parse_into_the_same_result(name: str, status: str) -> None:
    # Every suite checks this field list on these fixtures, so a misparse in one SDK
    # cannot pass while the others check a different subset.
    parsed = transport._parse_trial_evaluation(RESPONSES[name])  # noqa: SLF001
    assert parsed.status.value == status
    assert parsed.evaluation_id == INPUTS["evaluation_id"]
    assert parsed.experiment_id == INPUTS["experiment_id"]
    assert parsed.trial_id == TRIAL_ID
    assert parsed.conversation_id == INPUTS["conversation_id"]
    assert parsed.evaluator_id == INPUTS["evaluator_id"]
    assert parsed.evaluator_version == INPUTS["evaluator_version"]


def test_failed_evaluation_carries_the_backend_detail() -> None:
    parsed = transport._parse_trial_evaluation(RESPONSES["evaluation_failed"])  # noqa: SLF001
    assert parsed.error == "grader crashed"
    assert parsed.attempts == 3


def test_evaluation_timestamps_and_case_id_parse() -> None:
    queued = transport._parse_trial_evaluation(RESPONSES["evaluation_queued"])  # noqa: SLF001
    assert queued.attempts == 0
    assert queued.test_case_id == INPUTS["test_case_id"]
    success = transport._parse_trial_evaluation(RESPONSES["evaluation_success"])  # noqa: SLF001
    assert success.attempts == 1
    assert success.test_case_id == INPUTS["test_case_id"]
    assert success.created_at == datetime(2026, 1, 1, tzinfo=timezone.utc)
    assert success.updated_at == datetime(2026, 1, 1, 0, 0, 5, tzinfo=timezone.utc)


@pytest.mark.parametrize(
    ("name", "message"),
    [
        ("evaluation_unsupported_status", "unsupported evaluation status"),
        ("evaluation_missing_id", "carries no evaluation_id"),
    ],
)
def test_unusable_evaluation_responses_fail_the_call(name: str, message: str) -> None:
    with pytest.raises(ExperimentTransportError, match=message):
        transport._parse_trial_evaluation(RESPONSES[name])  # noqa: SLF001


@pytest.mark.parametrize("name", ["evaluation_unsupported_status", "evaluation_missing_id"])
def test_unusable_evaluation_responses_fail_a_status_read(name: str) -> None:
    with pytest.raises(ExperimentTransportError):
        _capture(
            lambda client: client.get_trial_evaluation(INPUTS["experiment_id"], TRIAL_ID, INPUTS["evaluation_id"]),
            override=RESPONSES[name],
        )


def test_both_report_envelopes_parse_alike() -> None:
    from_experiment = transport._parse_report(RESPONSES["report_experiment_envelope"])  # noqa: SLF001
    run_envelope = {key: value for key, value in RESPONSES["report_run_envelope"].items() if key != "comment"}
    from_run = transport._parse_report(run_envelope)  # noqa: SLF001
    assert from_experiment == from_run
    assert from_experiment.run.experiment_id == INPUTS["experiment_id"]
    assert from_experiment.summary.trial_count == 1
    # Only a score under the "final" key feeds the pass rate, and a stored
    # evaluator writes under its own key.
    assert from_experiment.summary.pass_rate is None


def test_cloud_evaluated_trial_produces_the_pinned_call_order(monkeypatch) -> None:
    monkeypatch.setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "true")
    recorder = _Recorder()

    def respond(path: str) -> Any:
        if path.endswith(":evaluate") or "/evaluations/" in path:
            return RESPONSES["evaluation_success"]
        return _Recorder.respond(recorder, path)

    recorder.respond = respond  # type: ignore[method-assign]
    server = HTTPServer(("127.0.0.1", 0), _make_handler(recorder))
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        client = ExperimentClient(
            f"http://127.0.0.1:{server.server_address[1]}",
            ingest_token="token",
            redact_secrets=False,
            use_experimental_otel=False,
        )
        suite = TestSuite(suite_id=INPUTS["suite_id"], version=INPUTS["suite_version"])
        with Experiment(
            client,
            experiment_id=INPUTS["experiment_id"],
            name=INPUTS["experiment_name"],
            suite=suite,
            tags=list(INPUTS["tags"]),
            planned_trial_count=INPUTS["planned_trial_count"],
            auto_finalize=False,
        ) as experiment:
            with experiment.trial(INPUTS["test_case_id"]) as trial:
                trial.bind_conversation(INPUTS["conversation_id"])
                trial.evaluate(INPUTS["evaluator_id"], evaluator_version=INPUTS["evaluator_version"])
            experiment.finalize("completed")
    finally:
        server.shutdown()
        server.server_close()

    calls = [f"{request['method']} {request['path']}" for request in recorder.requests]
    assert calls == [
        "POST /api/v1/experiment-runs:upsert",
        f"POST /api/v1/experiment-runs/{INPUTS['experiment_id']}/trials",
        f"PATCH /api/v1/experiment-runs/{INPUTS['experiment_id']}/trials/{TRIAL_ID}",
        f"POST /api/v1/experiment-runs/{INPUTS['experiment_id']}/trials/{TRIAL_ID}:evaluate",
        f"PATCH /api/v1/experiment-runs/{INPUTS['experiment_id']}/trials/{TRIAL_ID}",
        f"POST /api/v1/experiment-runs/{INPUTS['experiment_id']}:finalize",
    ]
    assert "/api/v1/scores:export" not in [request["path"] for request in recorder.requests]
    _assert_matches(recorder.requests[2], REQUESTS["trial_patch_conversation"])
    _assert_matches(recorder.requests[3], REQUESTS["trial_evaluate"])
    _assert_matches(recorder.requests[5], REQUESTS["run_finalize"])
