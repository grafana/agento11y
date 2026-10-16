"""Compare two actual-agent runs of one published suite version for CI.

Both runs must have one completed attempt per case, graded by every binding the
version pins, with each case snapshot verified against the published version.
Only then are their primary-verdict pass rates compared. Exit codes: 0 passed,
1 regression, 2 the reports can't support the comparison.

Run it as ``agento11y-checkpoint-gate`` or ``python -m agento11y.experiments.gate``.
"""

from __future__ import annotations

import argparse
import json
import math
import sys
import urllib.error
from collections.abc import Sequence
from typing import Any

from ..errors import ValidationError
from ..models import ExperimentReport
from .client import Client
from .suites import TestSuitesClient
from .types import _env_value


class EvidenceError(ValueError):
    """A report cannot support this gate's comparison policy."""


class GateConfigError(Exception):
    """The gate's environment is incomplete."""


def _require(condition: bool, reason: str) -> None:
    if not condition:
        raise EvidenceError(reason)


def _evidence(
    report: ExperimentReport, suite_id: str, version: str, case_ids: set[str], bindings: list[dict[str, Any]]
) -> tuple[float, dict[str, list[tuple[str, ...]]]]:
    run, summary = report.run, report.summary
    _require(run.status == "completed" and not run.error and not run.result_error, "run is not successfully completed")
    _require(run.metadata.get("execution_kind") == "agent_run", "actual-agent execution declaration is missing")
    _require((run.suite_id, run.suite_version) == (suite_id, version), "checkpoint identity differs")
    _require(bool(case_ids), "checkpoint has no cases")
    _require(run.planned_trial_count == len(case_ids), "planned trial count differs from checkpoint")
    _require(
        summary.trial_count == summary.completed_count == summary.test_case_count == len(case_ids)
        and summary.failed_count == summary.canceled_count == 0,
        "trial coverage is incomplete",
    )
    expected = {binding["binding_id"]: binding for binding in bindings}
    _require(bool(expected) and len(expected) == len(bindings), "checkpoint bindings are missing or duplicated")
    _require(
        sum(binding.get("report_role") == "primary_verdict" for binding in bindings) == 1,
        "gate requires exactly one primary verdict binding",
    )
    identities: dict[str, list[tuple[str, ...]]] = {}
    passed = 0
    for row in report.rows:
        case_id = row.get("test_case_id")
        _require(
            case_id in case_ids and case_id not in identities, "case population is missing, duplicated or different"
        )
        trials = row.get("trials", [])
        _require(len(trials) == 1, "gate requires one attempt per case")
        result = trials[0]
        trial = result.get("trial", {})
        _require(
            trial.get("status") == "completed"
            and trial.get("attempt") == 1
            and trial.get("test_case_id") == case_id
            and trial.get("experiment_id") == run.experiment_id
            and not trial.get("error"),
            "trial is incomplete or has incompatible identity",
        )
        snapshot = trial.get("test_case") or {}
        _require(
            snapshot.get("test_case_id") == case_id
            and snapshot.get("suite_id") == suite_id
            and snapshot.get("suite_version") == version
            and snapshot.get("metadata", {}).get("sigil.checkpoint_match") == "matched"
            and trial.get("metadata", {}).get("sigil.checkpoint_match") == "matched",
            "case snapshot has not been verified against the checkpoint",
        )
        scores = result.get("scores", [])
        _require(bool(scores), "grading evidence is missing")
        seen: set[str] = set()
        primary: list[dict[str, Any]] = []
        identity = []
        for score in scores:
            metadata = score.get("metadata") or {}
            binding_id = metadata.get("binding_id")
            _require(binding_id in expected, "score is not backed by a published binding")
            binding = expected[binding_id]
            _require(
                score.get("evaluator_id") == binding.get("evaluator_id")
                and bool(score.get("evaluator_id"))
                and score.get("evaluator_version") == binding.get("evaluator_version")
                and bool(score.get("evaluator_version"))
                and score.get("report_role") == binding.get("report_role")
                and metadata.get("suite_id") == suite_id
                and metadata.get("suite_version") == version
                and score.get("trial_id") == trial.get("trial_id")
                and score.get("test_case_id") == case_id
                and score.get("experiment_id") == run.experiment_id,
                "score identity differs from pinned checkpoint grading",
            )
            _require(
                all(
                    isinstance(metadata.get(key), str) and metadata[key]
                    for key in ("binding_digest", "case_digest", "resolved_inputs_digest")
                )
                and bool(score.get("score_key"))
                and bool(score.get("score_type")),
                "pinned grading evidence is incomplete",
            )
            seen.add(binding_id)
            identity.append(
                tuple(
                    str(value)
                    for value in (
                        binding_id,
                        score["evaluator_id"],
                        score["evaluator_version"],
                        score["report_role"],
                        score["score_key"],
                        score["score_type"],
                        metadata["binding_digest"],
                        metadata["case_digest"],
                        metadata.get("judge_provider", ""),
                        metadata.get("judge_model", ""),
                    )
                )
            )
            if score.get("report_role") == "primary_verdict":
                primary.append(score)
        _require(seen == set(expected), "checkpoint grader coverage is incomplete")
        _require(len(identity) == len(set(identity)), "grading evidence is duplicated")
        _require(
            len(primary) == 1 and isinstance(primary[0].get("passed"), bool), "primary verdict is missing or ambiguous"
        )
        passed += int(primary[0]["passed"])
        identities[case_id] = sorted(identity)
    _require(set(identities) == case_ids, "checkpoint case coverage is incomplete")
    rate = passed / len(case_ids)
    _require(
        summary.pass_denominator == len(case_ids)
        and summary.pass_count == passed
        and summary.pass_rate is not None
        and math.isclose(summary.pass_rate, rate, abs_tol=1e-12),
        "report aggregate disagrees with complete primary verdicts",
    )
    return rate, identities


def _compare(
    baseline: ExperimentReport,
    candidate: ExperimentReport,
    *,
    suite_id: str,
    version: str,
    case_ids: set[str],
    bindings: list[dict[str, Any]],
    min_pass_rate: float,
    max_drop: float,
) -> dict[str, Any]:
    _require(
        all(math.isfinite(value) and 0 <= value <= 1 for value in (min_pass_rate, max_drop)),
        "thresholds must be finite fractions in [0, 1]",
    )
    baseline_rate, baseline_identity = _evidence(baseline, suite_id, version, case_ids, bindings)
    candidate_rate, candidate_identity = _evidence(candidate, suite_id, version, case_ids, bindings)
    _require(baseline_identity == candidate_identity, "case or grading configuration differs between runs")
    drop = baseline_rate - candidate_rate
    reasons = []
    if candidate_rate < min_pass_rate:
        reasons.append("candidate pass rate is below the minimum")
    if drop > max_drop + 1e-12:
        reasons.append("pass rate drop exceeds the permitted maximum")
    return {
        "status": "regression" if reasons else "passed",
        "baseline_pass_rate": baseline_rate,
        "candidate_pass_rate": candidate_rate,
        "pass_rate_drop": drop,
        "rated_cases": len(case_ids),
        "min_pass_rate": min_pass_rate,
        "max_drop": max_drop,
        "reasons": reasons,
    }


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="agento11y-checkpoint-gate", description=__doc__.split("\n\n")[0])
    for flag in ("suite-id", "suite-version", "baseline", "candidate"):
        parser.add_argument(f"--{flag}", required=True)
    parser.add_argument("--min-pass-rate", type=float, required=True, help="minimum candidate pass rate, 0 to 1")
    parser.add_argument("--max-drop", type=float, required=True, help="largest allowed pass rate drop, 0 to 1")
    args = parser.parse_args(argv)
    result: dict[str, Any] = {
        "suite_id": args.suite_id,
        "suite_version": args.suite_version,
        "baseline": args.baseline,
        "candidate": args.candidate,
    }
    try:
        _require(
            args.suite_version not in ("latest", "latest_published", "draft"),
            "an exact published suite version is required",
        )
        _require(args.baseline != args.candidate, "baseline and candidate must be different runs")
        endpoint = _env_value("ENDPOINT").strip()
        if not endpoint:
            raise GateConfigError("set AGENTO11Y_ENDPOINT to your Grafana Cloud Agent Observability URL")
        if not _env_value("AUTH_TOKEN").strip():
            raise GateConfigError("set AGENTO11Y_AUTH_TOKEN to your Grafana Cloud ingestion API key")
        try:
            # Missing or invalid control-plane settings; the message names the variable to set.
            control = TestSuitesClient()
        except ValueError as error:
            raise GateConfigError(str(error)) from error
        with Client(endpoint, tenant_id=_env_value("AUTH_TENANT_ID")) as client:
            if client.grafana_url:
                result.update(
                    baseline_url=client.experiment_url(args.baseline),
                    candidate_url=client.experiment_url(args.candidate),
                )
            try:
                suite = control.pull_suite(args.suite_id, args.suite_version, require_published=True)
            except ValidationError as error:
                raise EvidenceError(str(error)) from error
            bindings = control.get_bindings(suite.suite_id, suite.version)
            baseline = client.get_report(args.baseline)
            candidate = client.get_report(args.candidate)
        _require(
            baseline.run.experiment_id == args.baseline and candidate.run.experiment_id == args.candidate,
            "report run identity differs",
        )
        result.update(
            _compare(
                baseline,
                candidate,
                suite_id=suite.suite_id,
                version=suite.version,
                case_ids={case.test_case_id for case in suite.cases},
                bindings=bindings,
                min_pass_rate=args.min_pass_rate,
                max_drop=args.max_drop,
            )
        )
    except EvidenceError as error:
        result.update(status="invalid", reasons=[str(error)])
    except GateConfigError as error:
        result.update(status="error", reasons=[str(error)])
    except Exception as error:
        # Transport/provider exception text can contain request content or credentials, so
        # report only the exception type and, for an HTTP failure, its status code.
        detail = type(error).__name__
        status_code = _http_status(error)
        if status_code is not None:
            detail += f", HTTP {status_code}"
        result.update(status="error", reasons=[f"unable to read or validate the suite version or reports ({detail})"])
    print(json.dumps(result, sort_keys=True))
    return {"passed": 0, "regression": 1}.get(result["status"], 2)


def _http_status(error: BaseException) -> int | None:
    seen: set[int] = set()
    current: BaseException | None = error
    while current is not None and id(current) not in seen:
        if isinstance(current, urllib.error.HTTPError):
            return current.code
        seen.add(id(current))
        current = current.__cause__ or current.__context__
    return None


if __name__ == "__main__":
    sys.exit(main())
