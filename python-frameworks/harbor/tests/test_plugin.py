"""Live Harbor job plugin: error isolation, retries, concurrency and identity."""

from __future__ import annotations

import asyncio
from pathlib import Path

import pytest
from agento11y.experiments import Experiment
from agento11y_harbor import plugin as plugin_module
from agento11y_harbor import publish_harbor_job
from harbor_fakes import FakeClient, FakeJob, harbor_result, trial_event, write_job, write_trajectory


@pytest.fixture
def client(monkeypatch) -> FakeClient:
    fake = FakeClient()
    monkeypatch.setattr(plugin_module, "experiment", lambda name, **kwargs: Experiment(fake, name=name, **kwargs))
    return fake


def _plugin(**kwargs) -> plugin_module.Agento11yPlugin:
    return plugin_module.Agento11yPlugin(upload_artifacts=False, **kwargs)


async def _run_trial(job: FakeJob, event) -> None:
    await job.start(event)
    await job.agent_start(event)
    await job.end(event)


def _event(tmp_path: Path, name: str, result: dict) -> object:
    write_trajectory(tmp_path / name)
    return trial_event(tmp_path, name, result)


def test_failed_attempt_superseded_by_passing_retry_is_not_counted(tmp_path, client):
    job = FakeJob(max_retries=2)
    plugin = _plugin()
    result = harbor_result(error="sandbox crashed", reward=None)

    async def scenario():
        await plugin.on_job_start(job)
        # Harbor reruns the same trial name with a new trial UUID per retry.
        first = trial_event(tmp_path, "fix-bug__abc", result, trial_id="uuid-1")
        await _run_trial(job, first)
        retry_result = harbor_result(reward=1)
        write_trajectory(tmp_path / "fix-bug__abc")
        await _run_trial(job, trial_event(tmp_path, "fix-bug__abc", retry_result, trial_id="uuid-2"))
        await plugin.on_job_end(None)

    asyncio.run(scenario())

    assert len(client.trials) == 1
    assert [update["status"] for update in client.trial_updates] == ["completed"]
    assert client.scores[0].passed is True
    assert client.finalized[0][1] == "completed"


def test_failed_attempt_with_exhausted_retries_publishes_final_error(tmp_path, client):
    job = FakeJob(max_retries=1)
    plugin = _plugin()

    async def scenario():
        await plugin.on_job_start(job)
        for trial_id in ("uuid-1", "uuid-2"):
            event = trial_event(tmp_path, "fix-bug__abc", harbor_result(error="sandbox crashed"), trial_id=trial_id)
            await _run_trial(job, event)
        # The trial closes as soon as Harbor has no retries left, before the job ends.
        assert [update["status"] for update in client.trial_updates] == ["failed"]
        await plugin.on_job_end(None)

    asyncio.run(scenario())

    assert len(client.trials) == 1
    assert client.finalized[0][1] == "failed"


def test_non_retryable_failure_closes_immediately(tmp_path, client):
    job = FakeJob(max_retries=3)
    plugin = _plugin()

    async def scenario():
        await plugin.on_job_start(job)
        # No reward: Harbor's verifier didn't produce one after the timeout.
        result = harbor_result(error="too slow", error_type="AgentTimeoutError", reward=None)
        await _run_trial(job, trial_event(tmp_path, "fix-bug__abc", result))
        assert [update["status"] for update in client.trial_updates] == ["failed"]
        await plugin.on_job_end(None)

    asyncio.run(scenario())
    assert client.finalized[0][1] == "failed"


def test_pending_retry_is_published_when_the_job_ends(tmp_path, client):
    job = FakeJob(max_retries=2)
    plugin = _plugin()

    async def scenario():
        await plugin.on_job_start(job)
        await _run_trial(job, _event(tmp_path, "fix-bug__abc", harbor_result(error="sandbox crashed")))
        assert client.trial_updates == []
        await plugin.on_job_end(None)

    asyncio.run(scenario())
    assert [update["status"] for update in client.trial_updates] == ["failed"]
    assert "sandbox crashed" in client.trial_updates[0]["error"]


def test_live_and_imported_views_agree_after_retry(tmp_path, client):
    job = FakeJob(max_retries=1)
    plugin = _plugin()
    jobs_dir = tmp_path / "job-1"
    jobs_dir.mkdir()

    async def scenario():
        await plugin.on_job_start(job)
        await _run_trial(job, _event(jobs_dir, "fix-bug__abc", harbor_result(error="sandbox crashed")))
        await _run_trial(job, _event(jobs_dir, "fix-bug__abc", harbor_result(reward=1)))
        await plugin.on_job_end(None)

    asyncio.run(scenario())
    # Harbor deletes the superseded attempt's directory, so the import sees only the retry.
    write_job(jobs_dir, {"fix-bug__abc": harbor_result(reward=1)})
    imported = FakeClient()
    publish_harbor_job(jobs_dir, client=imported, upload_artifacts=False)

    assert [request.experiment_id for request in imported.experiments] == [
        request.experiment_id for request in client.experiments
    ]
    assert [(u["status"], u["error"]) for u in imported.trial_updates] == [
        (u["status"], u["error"]) for u in client.trial_updates
    ]
    assert imported.finalized == client.finalized == [(client.experiments[0].experiment_id, "completed")]


def test_concurrent_trials_publish_independently(tmp_path, client):
    job = FakeJob()
    plugin = _plugin()
    tasks = [f"task-{index}" for index in range(6)]

    async def scenario():
        await plugin.on_job_start(job)
        events = [
            _event(tmp_path, f"{task}__x", harbor_result(task, reward=index % 2)) for index, task in enumerate(tasks)
        ]
        await asyncio.gather(*(job.start(event) for event in events))
        await asyncio.gather(*(job.agent_start(event) for event in events))
        await asyncio.gather(*(job.end(event) for event in reversed(events)))
        await plugin.on_job_end(None)

    asyncio.run(scenario())

    assert len(client.experiments) == 1
    assert len({trial["trial_id"] for trial in client.trials}) == len(tasks)
    assert len(client.trial_updates) == len(tasks)
    assert sorted(score.passed for score in client.scores) == [False, False, False, True, True, True]
    assert client.finalized[0][1] == "completed"


def test_several_attempts_per_case_get_distinct_attempt_numbers(tmp_path, client):
    job = FakeJob()
    plugin = _plugin()

    async def scenario():
        await plugin.on_job_start(job)
        for suffix in ("a", "b", "c"):
            await _run_trial(job, _event(tmp_path, f"fix-bug__{suffix}", harbor_result(reward=1)))
        await plugin.on_job_end(None)

    asyncio.run(scenario())

    assert sorted(trial["attempt"] for trial in client.trials) == [1, 2, 3]
    assert len({trial["test_case_id"] for trial in client.trials}) == 1
    assert len(client.trial_updates) == 3


def test_one_bad_case_among_good_ones_does_not_stop_the_job(tmp_path, client):
    job = FakeJob()
    plugin = _plugin()
    client.fail["record_generation"] = lambda **request: (
        RuntimeError("ingest unavailable") if request["tags"]["test.case.id"].endswith(bad_case) else None
    )
    good = [_event(tmp_path, f"good-{index}__x", harbor_result(f"good-{index}")) for index in range(2)]
    bad = _event(tmp_path, "bad__x", harbor_result("bad"))
    bad_case = ""

    async def scenario():
        nonlocal bad_case
        await plugin.on_job_start(job)
        await job.start(bad)
        await job.agent_start(bad)
        bad_case = client.trials[-1]["test_case_id"]
        await asyncio.gather(*(job.start(event) for event in good))
        await asyncio.gather(*(job.agent_start(event) for event in good))
        await asyncio.gather(job.end(bad), *(job.end(event) for event in good))
        await plugin.on_job_end(None)

    asyncio.run(scenario())

    states = {update["status"] for update in client.trial_updates}
    assert len(client.trial_updates) == 3
    assert states == {"completed", "failed"}
    assert sum(score.passed is True for score in client.scores) == 2
    assert client.finalized[0][1] == "failed"
    assert "ingest unavailable" in client.finalize_errors[client.finalized[0][0]]


def test_trial_start_network_error_marks_only_that_trial_errored(tmp_path, client):
    job = FakeJob()
    plugin = _plugin()
    client.fail["upsert_trial"] = lambda **request: (
        ConnectionError("connection reset") if request["test_case"]["name"] == "flaky" else None
    )

    async def scenario():
        await plugin.on_job_start(job)
        await _run_trial(job, _event(tmp_path, "flaky__x", harbor_result("flaky")))
        await _run_trial(job, _event(tmp_path, "steady__x", harbor_result("steady")))
        await plugin.on_job_end(None)

    asyncio.run(scenario())

    assert [trial["test_case"]["name"] for trial in client.trials] == ["steady"]
    assert client.scores[0].passed is True
    assert client.finalized[0][1] == "failed"
    assert "connection reset" in client.finalize_errors[client.finalized[0][0]]


def test_experiment_open_failure_skips_its_trials_without_raising(tmp_path, client, caplog):
    job = FakeJob()
    job.job_dir = tmp_path / "jobs" / "job-1"
    plugin = _plugin()
    client.fail["upsert_experiment"] = lambda **_: ConnectionError("network down")

    async def scenario():
        await plugin.on_job_start(job)
        await _run_trial(job, _event(tmp_path, "fix-bug__x", harbor_result()))
        await plugin.on_job_end(None)

    asyncio.run(scenario())
    assert client.trials == []
    assert client.finalized == []
    assert "1 Harbor trial(s) were not published" in caplog.text
    assert f"agento11y harbor export {job.job_dir}" in caplog.text


def test_experiment_open_failure_is_retried_by_the_next_trial(tmp_path, client):
    job = FakeJob()
    plugin = _plugin()
    # Down for trial a's open attempts at agent start and at its end.
    outage = [ConnectionError("network down")] * 2
    client.fail["upsert_experiment"] = lambda **_: outage.pop() if outage else None

    async def scenario():
        await plugin.on_job_start(job)
        await _run_trial(job, _event(tmp_path, "a__x", harbor_result("a")))
        await _run_trial(job, _event(tmp_path, "b__x", harbor_result("b")))
        await plugin.on_job_end(None)

    asyncio.run(scenario())
    assert [trial["test_case"]["name"] for trial in client.trials] == ["b"]
    assert [update["status"] for update in client.trial_updates] == ["completed"]
    # The experiment is missing a trial, so it can't be reported as complete.
    assert client.finalized[0][1] == "failed"
    assert "1 Harbor trial(s) were not published" in client.finalize_errors[client.finalized[0][0]]


def test_skipped_trial_published_at_its_end_does_not_fail_the_experiment(tmp_path, client):
    job = FakeJob()
    plugin = _plugin()
    outage = [ConnectionError("network down")]
    client.fail["upsert_experiment"] = lambda **_: outage.pop() if outage else None
    a = _event(tmp_path, "a__x", harbor_result("a"))
    b = _event(tmp_path, "b__x", harbor_result("b"))

    async def scenario():
        await plugin.on_job_start(job)
        await job.start(a)
        await job.agent_start(a)  # the experiment can't be opened yet
        await job.start(b)
        await job.agent_start(b)  # opens it
        await job.end(a)  # a is published after all
        await job.end(b)
        await plugin.on_job_end(None)

    asyncio.run(scenario())
    assert sorted(trial["test_case"]["name"] for trial in client.trials) == ["a", "b"]
    assert client.finalized[0][1] == "completed"
    assert client.finalize_errors[client.finalized[0][0]] == ""


def test_skipped_trial_that_then_fails_to_open_still_fails_the_experiment(tmp_path, client, monkeypatch):
    job = FakeJob()
    plugin = _plugin()
    outage = [ConnectionError("network down")]
    client.fail["upsert_experiment"] = lambda **_: outage.pop() if outage else None
    real_test_case = plugin_module._test_case

    def test_case(result, suite_id):
        if result["task_name"] == "a":
            raise ValueError("bad task id")
        return real_test_case(result, suite_id)

    monkeypatch.setattr(plugin_module, "_test_case", test_case)
    a = _event(tmp_path, "a__x", harbor_result("a"))
    b = _event(tmp_path, "b__x", harbor_result("b"))

    async def scenario():
        await plugin.on_job_start(job)
        await job.start(a)
        await job.agent_start(a)  # the experiment can't be opened yet
        await _run_trial(job, b)  # opens it
        await job.end(a)  # a's trial can't be created either
        await plugin.on_job_end(None)

    asyncio.run(scenario())
    assert [trial["test_case"]["name"] for trial in client.trials] == ["b"]
    assert client.finalized[0][1] == "failed"
    assert "bad task id" in client.finalize_errors[client.finalized[0][0]]


def test_experiment_records_the_agent_version_harbor_reports_after_setup(tmp_path, client):
    job = FakeJob()
    plugin = _plugin()
    result = harbor_result(agent_version=None)
    event = _event(tmp_path, "fix-bug__x", result)

    async def scenario():
        await plugin.on_job_start(job)
        await job.start(event)
        assert client.experiments == []
        # Harbor sets agent_info after agent setup, before the agent starts.
        result["agent_info"]["version"] = "2.0.1"
        await job.agent_start(event)
        await job.end(event)
        await plugin.on_job_end(None)

    asyncio.run(scenario())
    assert client.experiments[0].candidate["agent_version"] == "2.0.1"
    assert client.finalized[0][1] == "completed"


def test_trial_whose_setup_failed_is_published_at_its_end(tmp_path, client):
    job = FakeJob()
    plugin = _plugin()
    event = _event(tmp_path, "fix-bug__x", harbor_result(error="docker build failed", reward=None))

    async def scenario():
        await plugin.on_job_start(job)
        # Setup failed, so Harbor never started the agent.
        await job.start(event)
        await job.end(event)
        await plugin.on_job_end(None)

    asyncio.run(scenario())
    assert [update["status"] for update in client.trial_updates] == ["failed"]
    assert "docker build failed" in client.trial_updates[0]["error"]
    assert client.finalized[0][1] == "failed"


def test_each_step_of_a_multi_step_task_reuses_one_trial(tmp_path, client):
    job = FakeJob()
    plugin = _plugin()
    event = _event(tmp_path, "fix-bug__x", harbor_result())

    async def scenario():
        await plugin.on_job_start(job)
        await job.start(event)
        for _ in range(3):
            await job.agent_start(event)
        await job.end(event)
        await plugin.on_job_end(None)

    asyncio.run(scenario())
    assert [trial["attempt"] for trial in client.trials] == [1]
    assert [update["status"] for update in client.trial_updates] == ["completed"]


def test_finish_closes_every_experiment_when_one_finalize_fails(tmp_path, client):
    job = FakeJob()
    plugin = _plugin()
    failing: set[str] = set()
    client.fail["finalize"] = lambda experiment_id, **_: (
        ConnectionError("finalize failed") if experiment_id in failing else None
    )

    async def scenario():
        await plugin.on_job_start(job)
        await _run_trial(job, _event(tmp_path, "a__x", harbor_result(agent="codex")))
        failing.add(client.experiments[0].experiment_id)
        await _run_trial(job, _event(tmp_path, "b__x", harbor_result(agent="claude-code")))
        await plugin.on_job_end(None)

    asyncio.run(scenario())

    assert len(client.experiments) == 2
    assert [status for _, status in client.finalized] == ["completed"]
    assert client.finalized[0][0] == client.experiments[1].experiment_id


def test_finalized_experiment_is_skipped_with_a_clear_warning(tmp_path, caplog, monkeypatch):
    job = FakeJob()
    plugin = _plugin()
    client = FakeClient()
    monkeypatch.setattr(plugin_module, "experiment", lambda name, **kwargs: Experiment(client, name=name, **kwargs))
    client.terminal.add(f"job-1-{_candidate_suffix()}")

    async def scenario():
        await plugin.on_job_start(job)
        await _run_trial(job, _event(tmp_path, "a__x", harbor_result("a")))
        await _run_trial(job, _event(tmp_path, "b__x", harbor_result("b")))
        await plugin.on_job_end(None)

    asyncio.run(scenario())
    assert client.trials == []
    assert "already finalized" in caplog.text


def test_trial_hooks_never_attach_otel_context(tmp_path, monkeypatch):
    import importlib

    experiment_module = importlib.import_module("agento11y.experiments.experiment")
    attached = []
    monkeypatch.setattr(experiment_module.otel_context, "attach", lambda ctx: attached.append(ctx) or object())
    client = FakeClient()
    client.use_experimental_otel = True
    # Use the real experiment() factory path so the plugin's OTel choice is honored.
    monkeypatch.setattr(
        plugin_module,
        "experiment",
        lambda name, **kwargs: experiment_module.experiment(name, client=client, **kwargs),
    )
    job = FakeJob()
    plugin = _plugin()

    async def scenario():
        await plugin.on_job_start(job)
        await _run_trial(job, _event(tmp_path, "fix-bug__x", harbor_result()))
        await plugin.on_job_end(None)

    asyncio.run(scenario())
    assert attached == []
    assert client.finalized[0][1] == "completed"


def test_live_experiment_id_matches_import_when_agent_version_arrives_late(tmp_path, client):
    job = FakeJob()
    plugin = _plugin()
    jobs_dir = tmp_path / "job-1"
    final = harbor_result(agent_version="2.0.1")
    start = harbor_result(agent_version=None)
    event = _event(jobs_dir, "fix-bug__x", start)

    async def scenario():
        await plugin.on_job_start(job)
        await job.start(event)
        start.update(final)
        await job.agent_start(event)
        await job.end(event)
        await plugin.on_job_end(None)

    asyncio.run(scenario())
    write_job(jobs_dir, {"fix-bug__x": final})
    imported = FakeClient()
    publish_harbor_job(jobs_dir, client=imported, upload_artifacts=False)

    assert imported.experiments[0].experiment_id == client.experiments[0].experiment_id
    assert imported.experiments[0].candidate == client.experiments[0].candidate
    assert imported.experiments[0].name == client.experiments[0].name.replace("terminal-bench", "job-1")
    assert imported.scores[0].evaluator_version == client.scores[0].evaluator_version == "sha-fix-bug"


def _candidate_suffix() -> str:
    from agento11y.experiments import stable_id
    from agento11y_harbor.integration import _group_identity

    return stable_id("candidate", _group_identity(harbor_result())).removeprefix("candidate-")


def test_live_plugin_grades_a_timed_out_agent_that_harbor_still_verified(tmp_path, client):
    job = FakeJob()
    plugin = _plugin()

    async def scenario():
        await plugin.on_job_start(job)
        result = harbor_result(reward=1, error="too slow", error_type="AgentTimeoutError")
        await _run_trial(job, trial_event(tmp_path, "fix-bug__abc", result))
        await plugin.on_job_end(None)

    asyncio.run(scenario())
    assert [update["status"] for update in client.trial_updates] == ["completed"]
    assert client.scores[0].passed is True
    assert client.finalized[0][1] == "completed"


def test_json_decoded_plugin_kwargs_are_accepted(tmp_path, client):
    # Harbor JSON-decodes --plugin-kwarg values: experiment_id=2024 arrives as an int.
    job = FakeJob()
    plugin = _plugin(experiment_id=2024, name=7, main_reward="reward")

    async def scenario():
        await plugin.on_job_start(job)
        await _run_trial(job, trial_event(tmp_path, "fix-bug__abc", harbor_result()))
        await plugin.on_job_end(None)

    asyncio.run(scenario())
    assert client.finalized[0][0].startswith("2024-")


def test_interrupted_job_finalizes_its_experiments_at_exit(tmp_path, client, monkeypatch):
    registered = []
    monkeypatch.setattr(plugin_module.atexit, "register", registered.append)
    monkeypatch.setattr(plugin_module.atexit, "unregister", registered.remove)
    job = FakeJob()
    plugin = _plugin()

    async def scenario():
        await plugin.on_job_start(job)
        # Harbor starts the trial, then the process is interrupted before it ends.
        await job.start(trial_event(tmp_path, "fix-bug__abc", harbor_result()))

    asyncio.run(scenario())
    assert registered == [plugin._finish_at_exit]
    registered[0]()

    assert [update["status"] for update in client.trial_updates] == ["failed"]
    assert "ended before the trial result" in client.trial_updates[0]["error"]
    assert client.finalized[0][1] == "failed"


def test_finished_job_does_not_finalize_again_at_exit(tmp_path, client, monkeypatch):
    registered = []
    monkeypatch.setattr(plugin_module.atexit, "register", registered.append)
    monkeypatch.setattr(plugin_module.atexit, "unregister", registered.remove)
    job = FakeJob()
    plugin = _plugin()

    async def scenario():
        await plugin.on_job_start(job)
        await _run_trial(job, trial_event(tmp_path, "fix-bug__abc", harbor_result()))
        await plugin.on_job_end(None)

    asyncio.run(scenario())
    assert registered == []
    assert client.finalized == [(client.finalized[0][0], "completed")]
