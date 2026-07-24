"""Stored test-suite control-plane client tests."""

from __future__ import annotations

import json
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer
from typing import Any

import pytest
from agento11y.errors import ConflictError, NotFoundError
from agento11y.experiments import (
    Experiment,
    TestCase,
    TestSuite,
    TestSuitesClient,
    experiment_from_suite,
)
from agento11y.models import CreateExperimentRequest, ScoreItem


class _Recorder:
    def __init__(self) -> None:
        self.requests: list[dict[str, Any]] = []
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
                    "payload": json.loads(raw.decode("utf-8")) if raw else None,
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
        do_DELETE = _handle
        do_PATCH = _handle
        do_POST = _handle

        def log_message(self, _format, *_args):  # noqa: A003
            return

    return _Handler


def _serve(recorder: _Recorder) -> HTTPServer:
    server = HTTPServer(("127.0.0.1", 0), _make_handler(recorder))
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    return server


def _client(server: HTTPServer) -> TestSuitesClient:
    return TestSuitesClient(
        grafana_url=f"http://127.0.0.1:{server.server_address[1]}",
        service_account_token="control-token",
        timeout=2,
    )


def _suite_body(**overrides: Any) -> dict[str, Any]:
    body: dict[str, Any] = {
        "tenant_id": "tenant-a",
        "suite_id": "dashboard",
        "name": "Dashboard",
        "description": "dashboards",
        "tags": ["ui"],
        "versions": [
            {"suite_id": "dashboard", "version": "v1", "published": True, "test_case_count": 1},
            {"suite_id": "dashboard", "version": "v2", "published": True, "test_case_count": 2},
            {"suite_id": "dashboard", "version": "v3", "published": False, "test_case_count": 2},
        ],
    }
    body.update(overrides)
    return body


def test_endpoint_derivation_and_bearer_auth(monkeypatch: pytest.MonkeyPatch) -> None:
    recorder = _Recorder()
    recorder.push(200, _suite_body())
    server = _serve(recorder)
    monkeypatch.setenv("AGENTO11Y_GRAFANA_URL", f"http://127.0.0.1:{server.server_address[1]}")
    monkeypatch.setenv("AGENTO11Y_SERVICE_ACCOUNT_TOKEN", "env-token")
    try:
        client = TestSuitesClient(timeout=2)
        suite = client.get_suite("dashboard")
        request = recorder.requests[0]
        assert request["method"] == "GET"
        assert request["path"] == "/api/plugins/grafana-sigil-app/resources/eval/test-suites/dashboard"
        assert request["headers"]["authorization"] == "Bearer env-token"
        assert suite["suite_id"] == "dashboard"
        assert client.grafana_url == f"http://127.0.0.1:{server.server_address[1]}"
        assert client.control_endpoint.endswith("/api/plugins/grafana-sigil-app/resources/eval")
        assert client.service_account_token == "env-token"
    finally:
        server.shutdown()
        server.server_close()


def test_bearer_auth_normalizes_scheme_casing() -> None:
    recorder = _Recorder()
    recorder.push(200, _suite_body())
    server = _serve(recorder)
    try:
        client = TestSuitesClient(
            grafana_url=f"http://127.0.0.1:{server.server_address[1]}",
            service_account_token="bearer control-token",
            timeout=2,
        )

        client.get_suite("dashboard")

        assert recorder.requests[0]["headers"]["authorization"] == "Bearer control-token"
    finally:
        server.shutdown()
        server.server_close()


def test_control_endpoint_normalizes_grafana_app_url(monkeypatch: pytest.MonkeyPatch) -> None:
    recorder = _Recorder()
    recorder.push(200, _suite_body())
    server = _serve(recorder)
    monkeypatch.setenv(
        "AGENTO11Y_CONTROL_ENDPOINT",
        f"http://127.0.0.1:{server.server_address[1]}/a/grafana-sigil-app",
    )
    monkeypatch.setenv("AGENTO11Y_SERVICE_ACCOUNT_TOKEN", "env-token")
    try:
        client = TestSuitesClient(timeout=2)
        client.get_suite("dashboard")
        request = recorder.requests[0]
        assert request["path"] == "/api/plugins/grafana-sigil-app/resources/eval/test-suites/dashboard"
        assert request["headers"]["authorization"] == "Bearer env-token"
    finally:
        server.shutdown()
        server.server_close()


def test_control_endpoint_preserves_grafana_subpath() -> None:
    client = TestSuitesClient(
        control_endpoint="https://example.test/grafana/a/grafana-sigil-app/experiments/test-suites",
        service_account_token="token",
    )

    assert client.control_endpoint == ("https://example.test/grafana/api/plugins/grafana-sigil-app/resources/eval")


def test_direct_control_endpoint_keeps_separate_grafana_url(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("AGENTO11Y_CONTROL_ENDPOINT", "http://localhost:8080/api/v1/eval")
    monkeypatch.setenv("AGENTO11Y_GRAFANA_URL", "http://localhost:3000")
    monkeypatch.setenv("AGENTO11Y_SERVICE_ACCOUNT_TOKEN", "env-token")

    client = TestSuitesClient(timeout=2)

    assert client.control_endpoint == "http://localhost:8080/api/v1/eval"
    assert client.grafana_url == "http://localhost:3000"


def test_legacy_control_environment_is_ignored(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("AGENTO11Y_CONTROL_ENDPOINT", raising=False)
    monkeypatch.delenv("AGENTO11Y_GRAFANA_URL", raising=False)
    monkeypatch.delenv("AGENTO11Y_SERVICE_ACCOUNT_TOKEN", raising=False)
    monkeypatch.setenv("SIGIL_CONTROL_ENDPOINT", "https://legacy.example")
    monkeypatch.setenv("SIGIL_SERVICE_ACCOUNT_TOKEN", "legacy-token")

    with pytest.raises(ValueError, match="control_endpoint is required"):
        TestSuitesClient()


def test_version_resolution_aliases() -> None:
    client = TestSuitesClient(
        control_endpoint="http://example.test/api/plugins/grafana-sigil-app/resources/eval",
        service_account_token="token",
    )
    suite = _suite_body()
    assert client.resolve_version(suite, "latest_published") == "v2"
    assert client.resolve_version(suite, "latest") == "v3"
    assert client.resolve_version(suite, "draft") == "v3"
    assert client.resolve_version(suite, "v1") == "v1"
    with pytest.raises(NotFoundError):
        client.resolve_version(suite, "v9")


def test_list_suites_paginates() -> None:
    recorder = _Recorder()
    recorder.push(200, {"items": [{"suite_id": "one"}], "next_cursor": "next"})
    recorder.push(200, {"items": [{"suite_id": "two"}], "next_cursor": ""})
    server = _serve(recorder)
    try:
        suites = _client(server).list_suites(limit=1)
        assert [suite["suite_id"] for suite in suites] == ["one", "two"]
        assert recorder.requests[0]["path"].endswith("/test-suites?limit=1")
        assert recorder.requests[1]["path"].endswith("/test-suites?limit=1&cursor=next")
    finally:
        server.shutdown()
        server.server_close()


def test_experiment_from_suite_requires_control_token() -> None:
    with pytest.raises(ValueError, match="service_account_token is required"):
        experiment_from_suite(
            "suite",
            control_endpoint="https://example.test/a/grafana-sigil-app",
            endpoint="https://ingest.example",
            ingest_token="token",
        )


def test_yaml_shape_validation_is_explicit() -> None:
    with pytest.raises(ValueError, match="suite tags must be a list of strings"):
        TestSuite.from_dict({"suite_id": "bad", "tags": "smoke"})
    with pytest.raises(ValueError, match="suite cases must be a list"):
        TestSuite.from_dict({"suite_id": "bad", "cases": {"id": "case"}})
    with pytest.raises(ValueError, match="tags must be a list of strings"):
        TestSuite.from_dict({"suite_id": "bad", "cases": [{"id": "case", "tags": "smoke"}]})
    with pytest.raises(ValueError, match="weight must be numeric"):
        TestSuite.from_dict({"suite_id": "bad", "cases": [{"id": "case", "weight": "heavy"}]})


def test_pull_suite_paginates_and_round_trips_portable_shape() -> None:
    recorder = _Recorder()
    recorder.push(200, _suite_body())
    recorder.push(
        200,
        {
            "items": [
                {
                    "test_case_id": "case-object",
                    "name": "Object case",
                    "input": {"prompt": "hello"},
                    "expected": {"answer": "hi"},
                    "metadata": {
                        "origin": "remote",
                        "agento11y.sdk.portability": {"version": 1, "weight": 2.5},
                    },
                    "artifact_refs": [{"artifact_id": "artifact-1", "name": "rubric"}],
                }
            ],
            "next_cursor": "2",
        },
    )
    recorder.push(
        200,
        {
            "items": [
                {
                    "test_case_id": "case-scalar",
                    "input": {"value": "2+2"},
                    "expected": {"value": "4"},
                    "metadata": {
                        "agento11y.sdk.portability": {
                            "version": 1,
                            "wrapped_fields": ["input", "expected"],
                        }
                    },
                }
            ],
            "next_cursor": "",
        },
    )
    server = _serve(recorder)
    try:
        suite = _client(server).pull_suite("dashboard", version="latest_published")
        assert suite.suite_id == "dashboard"
        assert suite.version == "v2"
        assert [case.test_case_id for case in suite.cases] == ["case-object", "case-scalar"]
        assert suite.case("case-object").input == {"prompt": "hello"}
        assert suite.case("case-object").weight == 2.5
        assert suite.case("case-object").metadata == {"origin": "remote"}
        assert suite.case("case-object").artifact_refs == [{"artifact_id": "artifact-1", "name": "rubric"}]
        assert suite.case("case-scalar").input == "2+2"
        assert suite.case("case-scalar").expected == "4"
        assert suite.case("case-scalar").metadata == {}
        assert recorder.requests[1]["path"].endswith("/versions/v2/test-cases?limit=200")
        assert recorder.requests[2]["path"].endswith("/versions/v2/test-cases?limit=200&cursor=2")
    finally:
        server.shutdown()
        server.server_close()


def test_push_then_pull_preserves_portable_case_shape() -> None:
    recorder = _Recorder()
    draft = {
        "suite_id": "portable",
        "name": "Portable",
        "versions": [{"version": "v2", "published": False, "changelog": "round trip"}],
    }
    published = {
        "suite_id": "portable",
        "name": "Portable",
        "versions": [{"version": "v2", "published": True, "changelog": "round trip"}],
    }
    remote_case = {
        "test_case_id": "scalar",
        "name": "Scalar",
        "tags": ["smoke"],
        "input": {"value": "question"},
        "expected": {"value": "answer"},
        "metadata": {
            "owner": "sdk",
            "agento11y.sdk.portability": {
                "version": 1,
                "weight": 2.0,
                "wrapped_fields": ["input", "expected"],
            },
        },
    }
    recorder.push(200, draft)
    recorder.push(200, draft)
    recorder.push(200, remote_case)
    recorder.push(200, {"version": "v2", "published": True})
    recorder.push(200, published)
    recorder.push(200, {"items": [remote_case], "next_cursor": ""})
    server = _serve(recorder)
    try:
        source = TestSuite(
            suite_id="portable",
            name="Portable",
            changelog="round trip",
            test_cases=[
                TestCase(
                    test_case_id="scalar",
                    name="Scalar",
                    tags=["smoke"],
                    input="question",
                    expected="answer",
                    weight=2.0,
                    metadata={"owner": "sdk"},
                )
            ],
        )
        client = _client(server)
        client.push_suite(source, publish=True)
        pulled = client.pull_suite("portable")

        assert pulled.cases[0].to_dict() == source.cases[0].to_dict()
        assert pulled.name == source.name
        assert pulled.changelog == source.changelog
    finally:
        server.shutdown()
        server.server_close()


def test_push_suite_creates_missing_suite_reuses_open_draft_upserts_and_publishes() -> None:
    recorder = _Recorder()
    recorder.push(404, {"error": "missing"})
    recorder.push(200, {"suite_id": "local", "name": "Local"})
    recorder.push(
        200,
        {
            "suite_id": "local",
            "name": "Local",
            "versions": [{"suite_id": "local", "version": "v2", "published": False, "test_case_count": 0}],
        },
    )
    recorder.push(200, {"test_case_id": "scalar", "input": {"value": "question"}})
    recorder.push(200, {"suite_id": "local", "version": "v2", "published": True, "test_case_count": 1})
    server = _serve(recorder)
    try:
        suite = TestSuite(
            suite_id="local",
            name="Local",
            test_cases=[TestCase(test_case_id="scalar", input="question", expected="answer")],
        )
        pushed = _client(server).push_suite(suite, publish=True)
        assert pushed.suite_id == "local"
        assert pushed.suite_version == "v2"
        assert pushed.published is True
        assert pushed.pruned_case_ids == []
        assert [r["path"] for r in recorder.requests] == [
            "/api/plugins/grafana-sigil-app/resources/eval/test-suites/local",
            "/api/plugins/grafana-sigil-app/resources/eval/test-suites",
            "/api/plugins/grafana-sigil-app/resources/eval/test-suites/local",
            "/api/plugins/grafana-sigil-app/resources/eval/test-suites/local/versions/v2/test-cases",
            "/api/plugins/grafana-sigil-app/resources/eval/test-suites/local/versions/v2:publish",
        ]
        upsert = recorder.requests[3]["payload"]
        assert upsert["input"] == {"value": "question"}
        assert upsert["expected"] == {"value": "answer"}
        assert upsert["metadata"]["agento11y.sdk.portability"] == {
            "version": 1,
            "wrapped_fields": ["input", "expected"],
        }
    finally:
        server.shutdown()
        server.server_close()


def test_push_suite_can_prune_remote_only_cases() -> None:
    recorder = _Recorder()
    draft = {
        "suite_id": "local",
        "name": "Local",
        "versions": [{"suite_id": "local", "version": "v2", "published": False, "test_case_count": 2}],
    }
    recorder.push(200, draft)
    recorder.push(200, draft)
    recorder.push(200, {"test_case_id": "keep", "input": {"prompt": "hi"}})
    recorder.push(
        200,
        {
            "items": [
                {"test_case_id": "keep", "input": {"prompt": "hi"}},
                {"test_case_id": "remove", "input": {"prompt": "old"}},
            ],
            "next_cursor": "",
        },
    )
    recorder.push(204, {})
    server = _serve(recorder)
    try:
        suite = TestSuite(
            suite_id="local",
            name="Local",
            test_cases=[TestCase(test_case_id="keep", input={"prompt": "hi"}, weight=2.0)],
        )
        pushed = _client(server).push_suite(suite, prune=True)
        assert pushed.pruned_case_ids == ["remove"]
        assert recorder.requests[2]["payload"]["metadata"]["agento11y.sdk.portability"] == {
            "version": 1,
            "weight": 2.0,
        }
        assert recorder.requests[4]["method"] == "DELETE"
        assert recorder.requests[4]["path"].endswith("/test-cases/remove")
    finally:
        server.shutdown()
        server.server_close()


def test_push_suite_recovers_from_create_draft_conflict_by_reusing_draft() -> None:
    recorder = _Recorder()
    existing = {
        "suite_id": "local",
        "name": "Local",
        "versions": [{"suite_id": "local", "version": "v1", "published": True, "test_case_count": 1}],
    }
    recorder.push(200, existing)
    recorder.push(200, existing)
    recorder.push(409, {"error": "open draft"})
    recorder.push(
        200,
        {
            "suite_id": "local",
            "name": "Local",
            "versions": [
                {"suite_id": "local", "version": "v1", "published": True, "test_case_count": 1},
                {"suite_id": "local", "version": "v2", "published": False, "test_case_count": 1},
            ],
        },
    )
    recorder.push(200, {"test_case_id": "object", "input": {"prompt": "hi"}})
    server = _serve(recorder)
    try:
        suite = TestSuite(
            suite_id="local",
            name="",
            test_cases=[TestCase(test_case_id="object", input={"prompt": "hi"})],
        )
        pushed = _client(server).push_suite(suite)
        assert pushed.suite_version == "v2"
        assert recorder.requests[2]["method"] == "POST"
        assert recorder.requests[2]["path"].endswith("/test-suites/local/versions")
        assert recorder.requests[4]["payload"] == {"test_case_id": "object", "input": {"prompt": "hi"}}
    finally:
        server.shutdown()
        server.server_close()


def test_push_suite_rechecks_options_after_create_draft_race() -> None:
    recorder = _Recorder()
    published = {
        "suite_id": "local",
        "name": "Local",
        "versions": [{"version": "v1", "published": True}],
    }
    recorder.push(200, published)
    recorder.push(200, published)
    recorder.push(409, {"error": "open draft"})
    recorder.push(
        200,
        {
            **published,
            "versions": [
                {"version": "v1", "published": True},
                {"version": "v2", "published": False, "changelog": "other publisher"},
            ],
        },
    )
    server = _serve(recorder)
    try:
        suite = TestSuite(suite_id="local", test_cases=[TestCase(test_case_id="case", input="x")])
        with pytest.raises(ConflictError, match="different changelog"):
            _client(server).push_suite(suite, changelog="this publisher")
        assert len(recorder.requests) == 4
    finally:
        server.shutdown()
        server.server_close()


def test_existing_draft_rejects_unapplied_creation_options() -> None:
    client = TestSuitesClient(
        control_endpoint="http://example.test/api/v1/eval",
        service_account_token="token",
    )
    suite = {
        "versions": [
            {"version": "v2", "published": False, "changelog": "existing"},
        ]
    }

    with pytest.raises(ConflictError, match="different changelog"):
        client._ensure_draft_version("suite", suite, changelog="new", empty_draft=False)
    with pytest.raises(ConflictError, match="empty_draft"):
        client._ensure_draft_version("suite", suite, changelog="existing", empty_draft=True)


class _FakeExperimentClient:
    def __init__(self) -> None:
        self.use_experimental_otel = False
        self.upserts: list[CreateExperimentRequest] = []
        self.scores: list[ScoreItem] = []
        self.trials: list[tuple[str, str, dict[str, Any]]] = []
        self.trial_updates: list[tuple[str, str, dict[str, Any]]] = []
        self.finalized: list[tuple[str, str, int]] = []

    def upsert_experiment(self, request: CreateExperimentRequest):
        self.upserts.append(request)

    def upsert_trial(self, experiment_id, *, trial_id, **kwargs):
        self.trials.append((experiment_id, trial_id, kwargs))
        return {"trial_id": trial_id}

    def update_trial(self, experiment_id, trial_id, **kwargs):
        self.trial_updates.append((experiment_id, trial_id, kwargs))
        return {"trial_id": trial_id}

    def export_scores(self, scores, *, raise_on_reject: bool = True) -> int:
        self.scores.extend(scores)
        return len(scores)

    def flush_generations(self) -> None:
        return None

    def finalize(self, experiment_id, status="completed", *, score_count=None, error=""):
        self.finalized.append((experiment_id, status, score_count or 0))

    def experiment_url(self, experiment_id: str) -> str:
        return f"http://ui/{experiment_id}"


def test_experiment_with_pulled_suite_preserves_suite_and_case_provenance() -> None:
    suite = TestSuite(
        suite_id="stored-suite",
        name="Stored",
        version="v2",
        test_cases=[TestCase(test_case_id="case-1", input="2+2", expected="4")],
    )
    client = _FakeExperimentClient()

    with Experiment(client, experiment_id="run-stored", name="stored run", suite=suite) as exp:
        for case in suite.cases:
            with exp.trial(case) as trial:
                trial.score("final", case.expected == "4", passed=True, metadata={"expected": case.expected})

    assert client.upserts[0].metadata["suite_id"] == "stored-suite"
    assert client.upserts[0].metadata["suite_version"] == "v2"
    assert client.upserts[0].suite_id == "stored-suite"
    assert client.upserts[0].suite_version == "v2"
    assert client.trials[0][2]["test_case_id"] == "case-1"
    score = client.scores[0]
    assert score.experiment_id == "run-stored"
    assert score.test_case_id == "case-1"
    assert score.metadata["task_id"] == "case-1"
    assert score.metadata["expected"] == "4"


def test_experiment_from_suite_pulls_then_builds_linked_experiment() -> None:
    suite = TestSuite(
        suite_id="stored-suite",
        name="Stored",
        version="v4",
        test_cases=[TestCase(test_case_id="case-1", input="2+2", expected="4")],
    )

    class _FakeSuitesClient:
        def pull_suite(self, suite_id: str, version: str = "latest_published") -> TestSuite:
            assert suite_id == "stored-suite"
            assert version == "v4"
            return suite

    client = _FakeExperimentClient()
    exp = experiment_from_suite(
        "stored-suite",
        version="v4",
        client=client,
        test_suites_client=_FakeSuitesClient(),
        experiment_id="run-stored",
        planned_trial_count=3,
    )
    assert exp.suite is suite
    assert exp.name == "Stored experiment"
    assert exp.planned_trial_count == 3
