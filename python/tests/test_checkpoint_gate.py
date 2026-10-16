"""Regression coverage for the checkpoint CI gate."""

import copy
import json
import urllib.error
from unittest.mock import MagicMock

import pytest
from agento11y.errors import ExperimentTransportError, ValidationError
from agento11y.experiments import gate
from agento11y.models import Experiment, ExperimentReport, ExperimentReportSummary

BINDINGS = [
    {"binding_id": "quality", "evaluator_id": "judge", "evaluator_version": "v1", "report_role": "primary_verdict"}
]


def report(passed=True):
    snapshot = {
        "test_case_id": "case",
        "suite_id": "suite",
        "suite_version": "v1",
        "metadata": {"sigil.checkpoint_match": "matched"},
    }
    score = {
        "trial_id": "trial",
        "test_case_id": "case",
        "experiment_id": "run",
        "evaluator_id": "judge",
        "evaluator_version": "v1",
        "report_role": "primary_verdict",
        "score_key": "quality",
        "score_type": "bool",
        "passed": passed,
        "metadata": {
            "binding_id": "quality",
            "binding_digest": "sha256:binding",
            "case_digest": "case",
            "resolved_inputs_digest": "sha256:input",
            "suite_id": "suite",
            "suite_version": "v1",
        },
    }
    return ExperimentReport(
        run=Experiment(
            experiment_id="run",
            name="test",
            status="completed",
            suite_id="suite",
            suite_version="v1",
            planned_trial_count=1,
            metadata={"execution_kind": "agent_run"},
        ),
        summary=ExperimentReportSummary(
            test_case_count=1,
            trial_count=1,
            completed_count=1,
            pass_count=int(passed),
            pass_denominator=1,
            pass_rate=float(passed),
        ),
        rows=[
            {
                "test_case_id": "case",
                "trials": [
                    {
                        "trial": {
                            "trial_id": "trial",
                            "experiment_id": "run",
                            "test_case_id": "case",
                            "attempt": 1,
                            "status": "completed",
                            "test_case": snapshot,
                            "metadata": {"sigil.checkpoint_match": "matched"},
                        },
                        "scores": [score],
                    }
                ],
            }
        ],
    )


def compare(candidate, **kwargs):
    return gate._compare(
        report(),
        candidate,
        suite_id="suite",
        version="v1",
        case_ids={"case"},
        bindings=BINDINGS,
        min_pass_rate=kwargs.get("min_pass_rate", 1),
        max_drop=kwargs.get("max_drop", 0),
    )


def test_baseline_regression_fixed_and_explicit_thresholds():
    assert compare(report())["status"] == "passed"
    assert compare(report(False))["status"] == "regression"
    assert compare(report(False), min_pass_rate=0, max_drop=1)["status"] == "passed"
    assert compare(report())["status"] == "passed"


@pytest.mark.parametrize(
    "change",
    [
        lambda r: setattr(r.run, "status", "failed"),
        lambda r: setattr(r.run, "error", "harness failure"),
        lambda r: setattr(r.run, "result_error", "projection failure"),
        lambda r: r.run.metadata.clear(),
        lambda r: r.run.metadata.update(execution_kind="simulation"),
        lambda r: setattr(r.run, "suite_version", "v2"),
        lambda r: setattr(r.run, "planned_trial_count", 2),
        lambda r: setattr(r.summary, "completed_count", 0),
        lambda r: setattr(r.summary, "pass_denominator", 0),
        lambda r: setattr(r.summary, "pass_rate", float("nan")),
        lambda r: r.rows.clear(),
        lambda r: r.rows.append(copy.deepcopy(r.rows[0])),
        lambda r: r.rows[0]["trials"].append(copy.deepcopy(r.rows[0]["trials"][0])),
        lambda r: r.rows[0]["trials"][0]["trial"].update(status="failed"),
        lambda r: r.rows[0]["trials"][0]["trial"]["metadata"].clear(),
        lambda r: r.rows[0]["trials"][0]["scores"].clear(),
        lambda r: r.rows[0]["trials"][0]["scores"][0].pop("passed"),
        lambda r: r.rows[0]["trials"][0]["scores"][0].update(passed=1),
        lambda r: r.rows[0]["trials"][0]["scores"][0].update(evaluator_version="v2"),
        lambda r: r.rows[0]["trials"][0]["scores"][0]["metadata"].pop("binding_digest"),
        lambda r: r.rows[0]["trials"][0]["scores"][0]["metadata"].update(binding_digest="sha256:changed"),
        lambda r: r.rows[0]["trials"][0]["scores"][0]["metadata"].update(case_digest="changed"),
        lambda r: r.rows[0]["trials"][0]["scores"].append(copy.deepcopy(r.rows[0]["trials"][0]["scores"][0])),
    ],
)
def test_incomplete_or_incompatible_evidence_never_passes(change):
    candidate = report()
    change(candidate)
    with pytest.raises(gate.EvidenceError):
        compare(candidate, min_pass_rate=0, max_drop=1)


def test_changed_resolved_inputs_are_expected_between_candidates():
    candidate = report()
    candidate.rows[0]["trials"][0]["scores"][0]["metadata"]["resolved_inputs_digest"] = "sha256:new-response"
    assert compare(candidate)["status"] == "passed"


def test_missing_diagnostic_binding_is_incomplete():
    with pytest.raises(gate.EvidenceError, match="coverage"):
        gate._evidence(
            report(), "suite", "v1", {"case"}, BINDINGS + [{"binding_id": "safety", "report_role": "diagnostic"}]
        )


@pytest.mark.parametrize("value", [float("nan"), float("inf"), -0.1, 1.1])
def test_thresholds_must_be_finite_fractions(value):
    with pytest.raises(gate.EvidenceError):
        compare(report(), max_drop=value)


def gate_args(baseline="base /?", candidate="cand /?", version="v1"):
    return [
        "--suite-id",
        "suite",
        "--suite-version",
        version,
        "--baseline",
        baseline,
        "--candidate",
        candidate,
        "--min-pass-rate",
        "1",
        "--max-drop",
        "0",
    ]


gate_suite_client = gate.TestSuitesClient


@pytest.fixture
def gate_env(monkeypatch):
    monkeypatch.setenv("AGENTO11Y_ENDPOINT", "https://agento11y.example.grafana.net")
    monkeypatch.setenv("AGENTO11Y_AUTH_TOKEN", "test")
    control = MagicMock()
    control.pull_suite.return_value = MagicMock(suite_id="suite", version="v1", cases=[MagicMock(test_case_id="case")])
    control.get_bindings.return_value = BINDINGS
    monkeypatch.setattr(gate, "TestSuitesClient", lambda: control)
    return control


@pytest.mark.parametrize(("candidate", "exit_code"), [(True, 0), (False, 1), (None, 2), ("control_error", 2)])
@pytest.mark.parametrize("grafana_url", ["https://example.grafana.net/", ""])
def test_cli_machine_exit_status(monkeypatch, capsys, gate_env, candidate, exit_code, grafana_url):
    control = gate_env
    monkeypatch.setenv("AGENTO11Y_GRAFANA_URL", grafana_url)
    monkeypatch.delenv("SIGIL_GRAFANA_URL", raising=False)
    client = gate.Client("https://agento11y.example.grafana.net", ingest_token="test", grafana_url=grafana_url)
    baseline, result = report(), report(bool(candidate))
    for item, run_id in ((baseline, "base /?"), (result, "cand /?")):
        item.run.experiment_id = run_id
        item.rows[0]["trials"][0]["trial"]["experiment_id"] = run_id
        item.rows[0]["trials"][0]["scores"][0]["experiment_id"] = run_id
    if candidate is None:
        result.run.status = "failed"
    if candidate == "control_error":
        control.pull_suite.side_effect = RuntimeError("sensitive transport error")
    monkeypatch.setattr(client, "get_report", MagicMock(side_effect=[baseline, result]))
    monkeypatch.setattr(gate, "Client", lambda *args, **kwargs: client)
    assert gate.main(gate_args()) == exit_code
    output = json.loads(capsys.readouterr().out)
    assert output["status"] == (
        "error" if candidate == "control_error" else {0: "passed", 1: "regression", 2: "invalid"}[exit_code]
    )
    if grafana_url:
        base = "https://example.grafana.net/a/grafana-agento11y-app/experiments/runs/"
        assert output["baseline_url"] == base + "base%20%2F%3F"
        assert output["candidate_url"] == base + "cand%20%2F%3F"
    else:
        assert "baseline_url" not in output and "candidate_url" not in output
    assert "sensitive transport error" not in json.dumps(output)


@pytest.mark.parametrize(
    ("args", "reason"),
    [
        (gate_args(candidate="base /?"), "different runs"),
        (gate_args(version="latest_published"), "exact published suite version"),
    ],
)
def test_cli_rejects_unusable_arguments_before_reading_reports(capsys, gate_env, args, reason):
    assert gate.main(args) == 2
    output = json.loads(capsys.readouterr().out)
    assert output["status"] == "invalid" and reason in output["reasons"][0]
    gate_env.pull_suite.assert_not_called()


def test_cli_reports_an_unpublished_version_as_invalid(capsys, gate_env):
    gate_env.pull_suite.side_effect = ValidationError("a published checkpoint is required")
    assert gate.main(gate_args()) == 2
    output = json.loads(capsys.readouterr().out)
    assert output == {**output, "status": "invalid", "reasons": ["a published checkpoint is required"]}


@pytest.mark.parametrize("missing", ["AGENTO11Y_ENDPOINT", "AGENTO11Y_AUTH_TOKEN"])
def test_cli_names_missing_credentials(monkeypatch, capsys, gate_env, missing):
    monkeypatch.delenv(missing)
    monkeypatch.delenv(missing.replace("AGENTO11Y_", "SIGIL_"), raising=False)
    assert gate.main(gate_args()) == 2
    output = json.loads(capsys.readouterr().out)
    assert output["status"] == "error" and missing in output["reasons"][0]


@pytest.mark.parametrize("missing", ["AGENTO11Y_CONTROL_ENDPOINT", "AGENTO11Y_SERVICE_ACCOUNT_TOKEN"])
def test_cli_names_missing_suite_credentials(monkeypatch, capsys, gate_env, missing):
    monkeypatch.setattr(gate, "TestSuitesClient", gate_suite_client)
    for name in ("GRAFANA_URL", "CONTROL_ENDPOINT", "SERVICE_ACCOUNT_TOKEN"):
        monkeypatch.delenv(f"SIGIL_{name}", raising=False)
    monkeypatch.delenv("AGENTO11Y_GRAFANA_URL", raising=False)
    monkeypatch.setenv("AGENTO11Y_CONTROL_ENDPOINT", "https://example.grafana.net/api/plugins/x/resources")
    monkeypatch.setenv("AGENTO11Y_SERVICE_ACCOUNT_TOKEN", "glsa_test")
    monkeypatch.delenv(missing)
    assert gate.main(gate_args()) == 2
    output = json.loads(capsys.readouterr().out)
    assert output["status"] == "error" and missing in output["reasons"][0]


def test_cli_reports_http_status_without_the_response_body(capsys, gate_env):
    error = ExperimentTransportError("agento11y test suite transport failed: status 503: secret body")
    error.__cause__ = urllib.error.HTTPError("https://example", 503, "unavailable", {}, None)
    gate_env.pull_suite.side_effect = error
    assert gate.main(gate_args()) == 2
    output = json.loads(capsys.readouterr().out)
    assert output["status"] == "error"
    assert output["reasons"] == [
        "unable to read or validate the suite version or reports (ExperimentTransportError, HTTP 503)"
    ]


def test_diagnostic_failure_does_not_override_primary_verdict():
    baseline, candidate = report(), report()
    binding = {"binding_id": "safety", "evaluator_id": "safety", "evaluator_version": "v1", "report_role": "diagnostic"}
    for item in (baseline, candidate):
        score = copy.deepcopy(item.rows[0]["trials"][0]["scores"][0])
        score.update(evaluator_id="safety", report_role="diagnostic", passed=False)
        score["metadata"].update(binding_id="safety", binding_digest="sha256:safety")
        item.rows[0]["trials"][0]["scores"].append(score)
    assert (
        gate._compare(
            baseline,
            candidate,
            suite_id="suite",
            version="v1",
            case_ids={"case"},
            bindings=BINDINGS + [binding],
            min_pass_rate=1,
            max_drop=0,
        )["status"]
        == "passed"
    )


def test_second_primary_output_is_ambiguous_even_with_distinct_key():
    candidate = report()
    score = copy.deepcopy(candidate.rows[0]["trials"][0]["scores"][0])
    score["score_key"] = "other_quality"
    candidate.rows[0]["trials"][0]["scores"].append(score)
    with pytest.raises(gate.EvidenceError, match="ambiguous"):
        compare(candidate)


def test_missing_resolved_input_receipt_is_rejected():
    candidate = report()
    del candidate.rows[0]["trials"][0]["scores"][0]["metadata"]["resolved_inputs_digest"]
    with pytest.raises(gate.EvidenceError, match="incomplete"):
        compare(candidate)
