"""One bad Harbor trial must not abort the import or the sequential runner."""

from __future__ import annotations

import asyncio
import json
import sys
from types import SimpleNamespace

import pytest
from agento11y.experiments import TestCase, TestSuite
from agento11y_harbor import publish_harbor_job, run_harbor
from harbor_fakes import FakeClient, harbor_result, write_job, write_trajectory


def _job_with_bad_trial(tmp_path):
    job = tmp_path / "job"
    write_job(job, {f"task-{index}": harbor_result(f"task-{index}") for index in range(3)})
    (job / "task-1" / "agent" / "trajectory.worker.json").write_text("{not json")
    return job


def test_import_marks_corrupt_trajectory_trial_errored_and_continues(tmp_path):
    job = _job_with_bad_trial(tmp_path)
    client = FakeClient()

    published = publish_harbor_job(job, client=client, upload_artifacts=False)

    assert published.trial_count == 3
    states = [update["status"] for update in client.trial_updates]
    assert states.count("completed") == 2 and states.count("failed") == 1
    failed = next(update for update in client.trial_updates if update["status"] == "failed")
    assert "trajectory.worker.json" in failed["error"]
    # The errored trial keeps its reward as a diagnostic, never as the verdict.
    assert sum(score.report_role == "primary_verdict" for score in client.scores) == 2
    assert client.finalized[0][1] == "failed"
    assert "task-1 attempt 1" in client.finalize_errors[client.finalized[0][0]]


def test_import_skips_unreadable_trial_result(tmp_path, caplog):
    job = tmp_path / "job"
    write_job(job, {"task-0": harbor_result("task-0"), "task-1": harbor_result("task-1")})
    (job / "task-1" / "result.json").write_text("{truncated")
    client = FakeClient()

    published = publish_harbor_job(job, client=client, upload_artifacts=False)

    assert published.trial_count == 1
    assert "skipping unreadable Harbor trial result" in caplog.text


def test_import_continues_when_one_artifact_upload_fails(tmp_path):
    job = tmp_path / "job"
    write_job(job, {"task-0": harbor_result("task-0"), "task-1": harbor_result("task-1")})
    client = FakeClient()
    client.fail["upload_artifact"] = lambda **request: (
        ConnectionError("upload refused") if request["parent_id"] == client.trials[0]["trial_id"] else None
    )

    publish_harbor_job(job, client=client)

    assert sorted(update["status"] for update in client.trial_updates) == ["completed", "failed"]
    assert client.artifacts


def test_import_continues_when_one_trial_cannot_be_created(tmp_path):
    job = tmp_path / "job"
    write_job(job, {"task-0": harbor_result("task-0"), "task-1": harbor_result("task-1")})
    client = FakeClient()
    client.fail["upsert_trial"] = lambda **request: (
        ConnectionError("connection reset") if request["test_case"]["name"] == "task-0" else None
    )

    publish_harbor_job(job, client=client, upload_artifacts=False)

    assert [trial["test_case"]["name"] for trial in client.trials] == ["task-1"]
    assert client.finalized[0][1] == "failed"


def test_reimport_into_finalized_experiment_gives_clear_error(tmp_path):
    job = tmp_path / "job"
    write_job(job, {"task-0": harbor_result("task-0")})
    first = FakeClient()
    publish_harbor_job(job, client=first, upload_artifacts=False)
    again = FakeClient(terminal={first.experiments[0].experiment_id})

    with pytest.raises(ValueError, match="already finalized.*--experiment-id"):
        publish_harbor_job(job, client=again, upload_artifacts=False)

    fresh = FakeClient(terminal={first.experiments[0].experiment_id})
    rerun = publish_harbor_job(job, client=fresh, experiment_id="job-1-rerun", upload_artifacts=False)
    assert rerun.runs[0].experiment_id.startswith("job-1-rerun-")
    assert fresh.finalized[0][1] == "completed"


def test_import_always_appends_candidate_suffix(tmp_path):
    job = tmp_path / "job"
    write_job(job, {"task-0": harbor_result("task-0")})
    client = FakeClient()

    published = publish_harbor_job(job, client=client, experiment_id="nightly", upload_artifacts=False)

    assert published.runs[0].experiment_id.startswith("nightly-")
    assert published.runs[0].experiment_id != "nightly"


def test_import_and_runner_share_evaluator_versions(tmp_path, monkeypatch):
    job = tmp_path / "job"
    write_job(job, {"task-0": harbor_result("task-0")})
    imported = FakeClient()
    publish_harbor_job(job, client=imported, upload_artifacts=False)

    live = FakeClient()
    _run(tmp_path / "live", monkeypatch, live, {"case": harbor_result("task-0")})

    assert {score.evaluator_version for score in imported.scores} == {"sha-task-0"}
    assert {score.evaluator_version for score in live.scores} == {"sha-task-0"}


def _run(trial_root, monkeypatch, client, results, *, broken=()):
    class Native:
        def __init__(self, case_id):
            self.case_id = case_id
            self.paths = SimpleNamespace(trial_dir=trial_root / case_id)

        @classmethod
        async def create(cls, config):
            case_id = config.agent.env["AGENTO11Y_TEST_CASE_ID"]
            native = cls(case_id)
            write_trajectory(native.paths.trial_dir)
            if case_id in broken:
                (native.paths.trial_dir / "agent" / "trajectory.json").write_text("{broken")
            return native

        async def run(self):
            return SimpleNamespace(model_dump=lambda **_: results[self.case_id])

    monkeypatch.setitem(sys.modules, "harbor.trial.trial", SimpleNamespace(Trial=Native))
    configs = {
        case_id: SimpleNamespace(agent=SimpleNamespace(name="agent", model_name="model", env={})) for case_id in results
    }
    suite = TestSuite("live", test_cases=[TestCase(case_id) for case_id in results])
    return asyncio.run(run_harbor(suite, configs, client=client))


def test_runner_continues_past_one_bad_case(tmp_path, monkeypatch):
    client = FakeClient()
    results = {case: harbor_result(case) for case in ("good-1", "bad", "good-2")}

    published = _run(tmp_path, monkeypatch, client, results, broken={"bad"})

    assert published.trial_count == 3
    by_case = {trial["trial_id"]: trial["test_case_id"] for trial in client.trials}
    states = {by_case[update["trial_id"]]: update for update in client.trial_updates}
    assert states["good-1"]["status"] == states["good-2"]["status"] == "completed"
    assert states["bad"]["status"] == "failed"
    assert "trajectory import" in states["bad"]["error"]
    assert sum(score.passed is True for score in client.scores) == 2
    assert client.finalized[0][1] == "failed"


def test_runner_continues_when_artifact_upload_fails(tmp_path, monkeypatch):
    client = FakeClient()
    client.fail["upload_artifact"] = lambda **request: (
        ConnectionError("upload refused") if request["name"].endswith("trajectory.json") else None
    )
    results = {case: harbor_result(case) for case in ("one", "two")}
    for case in results:
        (tmp_path / case).mkdir()
        (tmp_path / case / "result.json").write_text(json.dumps(results[case]))

    published = _run(tmp_path, monkeypatch, client, results)

    assert published.trial_count == 2
    assert [update["status"] for update in client.trial_updates] == ["failed", "failed"]
    assert all("upload refused" in update["error"] for update in client.trial_updates)


@pytest.mark.parametrize("error_type", ["AgentTimeoutError", "NonZeroAgentExitCodeError"])
def test_import_grades_an_agent_failure_that_harbor_still_verified(tmp_path, error_type):
    job = tmp_path / "job"
    write_job(job, {"task-0": harbor_result("task-0", reward=1, error="agent stopped", error_type=error_type)})
    client = FakeClient()

    publish_harbor_job(job, client=client, upload_artifacts=False)

    # Harbor runs the verifier after these errors and counts the reward, so the verdict stands.
    assert [update["status"] for update in client.trial_updates] == ["completed"]
    assert client.scores[0].report_role == "primary_verdict" and client.scores[0].passed is True
    assert client.scores[0].metadata["harbor_agent_error"] == f"{error_type}: agent stopped"
    assert client.finalized[0][1] == "completed"


@pytest.mark.parametrize(
    ("reward", "error_type"),
    [(1, "RuntimeError"), (None, "AgentTimeoutError")],
)
def test_import_still_errors_other_failures_or_a_missing_reward(tmp_path, reward, error_type):
    job = tmp_path / "job"
    write_job(job, {"task-0": harbor_result("task-0", reward=reward, error="broke", error_type=error_type)})
    client = FakeClient()

    publish_harbor_job(job, client=client, upload_artifacts=False)

    assert [update["status"] for update in client.trial_updates] == ["failed"]
    assert all(score.report_role == "diagnostic" for score in client.scores)
    assert client.finalized[0][1] == "failed"


def test_one_failing_candidate_does_not_stop_the_others_and_a_rerun_fills_the_gap(tmp_path):
    job = tmp_path / "job"
    write_job(
        job,
        {
            "task-0": harbor_result("task-0", agent="codex"),
            "task-1": harbor_result("task-1", agent="claude-code"),
        },
    )
    first = FakeClient()
    calls = []
    first.fail["upsert_experiment"] = lambda request: (
        calls.append(1) or (ConnectionError("refused") if len(calls) == 1 else None)
    )

    partial = publish_harbor_job(job, client=first, upload_artifacts=False)

    assert len(partial.runs) == 1 and len(partial.failed) == 1
    assert "ConnectionError: refused" in partial.failed[0]
    done = partial.runs[0].experiment_id

    rerun_client = FakeClient(terminal={done})
    rerun = publish_harbor_job(job, client=rerun_client, upload_artifacts=False)

    assert len(rerun.runs) == 1 and rerun.runs[0].experiment_id != done
    assert len(rerun.skipped) == 1 and done in rerun.skipped[0]
    assert rerun.failed == []


def test_oversized_tool_payloads_are_bounded_so_the_trajectory_still_imports(tmp_path):
    job = tmp_path / "job"
    write_job(job, {"fix-bug__x": harbor_result()})
    (job / "fix-bug__x" / "agent" / "trajectory.json").write_text(
        json.dumps(
            {
                "schema_version": "ATIF-v1.8",
                "session_id": "session-big",
                "agent": {"name": "terminus-2", "model_name": "anthropic/claude-haiku-4-5-20251001"},
                "steps": [
                    {"step_id": 1, "source": "user", "message": "Read the log"},
                    {
                        "step_id": 2,
                        "source": "agent",
                        "message": "Reading it",
                        "tool_calls": [{"function_name": "cat", "arguments": {"path": "é" * 10_000}}],
                        # One file read larger than the SDK's 4 MiB generation limit.
                        "observation": {"results": [{"content": "x" * (3 << 20)}]},
                    },
                ],
            }
        )
    )
    client = FakeClient()

    publish_harbor_job(job, client=client, upload_artifacts=False)

    output = client.generations[0]["output_text"]
    assert len(output.encode()) <= (1 << 20) + len(" [truncated]")
    assert output.endswith(" [truncated]")
    tool_calls = output.split("Tool calls:\n", 1)[1].split("\n\nObservation:", 1)[0]
    assert len(tool_calls.encode()) <= 4096 + len(" [truncated]")
    assert [update["status"] for update in client.trial_updates] == ["completed"]
    assert client.finalized[0][1] == "completed"
