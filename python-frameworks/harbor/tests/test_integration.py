from __future__ import annotations

import json
from copy import deepcopy
from datetime import datetime, timezone
from pathlib import Path
from types import SimpleNamespace

import pytest
from agento11y_harbor import publish_harbor_job
from harbor_fakes import FakeClient


class FakeSuitesClient:
    def __init__(self) -> None:
        self.calls = 0

    def push_suite(self, suite, *, publish, prune):
        self.calls += 1
        stored = deepcopy(suite)
        stored.version = "server-version"
        stored.test_cases[0].input = {"task_name": "[REDACTED]"}
        return SimpleNamespace(suite=stored, published=publish, pruned_case_ids=[] if not prune else ["old"])


def _write_job(root: Path) -> None:
    root.mkdir()
    (root / "result.json").write_text(
        json.dumps(
            {
                "id": "job-1",
                "started_at": "2026-09-17T00:00:00Z",
                "n_total_trials": 2,
                "stats": {},
                "trial_results": [],
            }
        )
    )
    for index, (agent, reward, error) in enumerate((("codex", 1, None), ("claude-code", 0, "sandbox failed"))):
        trial = root / f"task-{index}"
        (trial / "agent").mkdir(parents=True)
        (trial / "verifier").mkdir()
        result = {
            "id": f"trial-{index}",
            "task_name": "fix-bug",
            "trial_name": f"fix-bug-{index}",
            "trial_uri": trial.as_uri(),
            "task_id": {"name": "fix-bug"},
            "source": "terminal-bench@2.0",
            "task_checksum": "sha-task",
            "config": {},
            "agent_info": {
                "name": agent,
                "version": "1.2.3",
                "model_info": {"provider": "openai", "name": "gpt-5"},
            },
            "agent_result": {
                "model_usage": {
                    "gpt-5": {
                        "n_input_tokens": 100,
                        "n_cache_tokens": 40,
                        "n_output_tokens": 20,
                        "cost_usd": 0.25,
                    }
                }
            },
            "verifier_result": {"rewards": {"reward": reward, "tests": reward}},
            "agent_execution": {
                "started_at": "2026-09-17T00:00:00Z",
                "finished_at": "2026-09-17T00:00:12.345Z",
            },
        }
        if error:
            result["exception_info"] = {"exception_type": "RuntimeError", "exception_message": error}
        (trial / "result.json").write_text(json.dumps(result))
        (trial / "agent" / "trajectory.json").write_text(
            json.dumps(
                {
                    "schema_version": "ATIF-v1.8",
                    "session_id": f"session-{index}",
                    "agent": {"name": agent, "version": "1.2.3", "model_name": "openai/gpt-5"},
                    "steps": [
                        {"step_id": 1, "source": "user", "message": "Fix the bug"},
                        {
                            "step_id": 2,
                            "source": "agent",
                            "message": "Done",
                            "timestamp": "2026-09-17T00:00:12.345Z",
                            "tool_calls": [
                                {"tool_call_id": "call-1", "function_name": "shell", "arguments": {"cmd": "pytest"}}
                            ],
                            "observation": {"results": [{"source_call_id": "call-1", "content": "passed"}]},
                            "metrics": {
                                "prompt_tokens": 100,
                                "cached_tokens": 40,
                                "completion_tokens": 20,
                                "cost_usd": 0.25,
                            },
                        },
                    ],
                    "final_metrics": {
                        "total_prompt_tokens": 100,
                        "total_cached_tokens": 40,
                        "total_completion_tokens": 20,
                        "total_cost_usd": 0.25,
                    },
                    "subagent_trajectories": [
                        {
                            "schema_version": "ATIF-v1.8",
                            "trajectory_id": "worker-1",
                            "agent": {"name": "worker", "model_name": "openai/gpt-5"},
                            "steps": [
                                {"step_id": 1, "source": "user", "message": "Check tests"},
                                {"step_id": 2, "source": "agent", "message": "Checked"},
                                {"step_id": 4, "source": "agent", "message": "Dispatch", "llm_call_count": 0},
                                {
                                    "step_id": 3,
                                    "source": "agent",
                                    "message": "old duplicate",
                                    "is_copied_context": True,
                                },
                            ],
                        }
                    ],
                }
            )
        )
        (trial / "agent" / "trajectory.worker.json").write_text(
            json.dumps(
                {
                    "schema_version": "ATIF-v1.6",
                    "session_id": f"session-{index}",
                    "agent": {"name": "external-worker", "model_name": "openai/gpt-5"},
                    "steps": [{"step_id": 1, "source": "agent", "message": "External work"}],
                }
            )
        )
        (trial / "verifier" / "reward-details.json").write_text(json.dumps({"reason": "tests"}))


def test_imports_candidates_trajectories_rewards_usage_and_errors(tmp_path: Path) -> None:
    job = tmp_path / "job"
    _write_job(job)
    client = FakeClient()

    published = publish_harbor_job(job, client=client)

    assert len(published.runs) == 2
    assert all("execution_kind" not in request.metadata for request in client.experiments)
    assert {status for _, status in client.finalized} == {"completed", "failed"}
    assert len({run.experiment_id for run in published.runs}) == 2
    assert published.trial_count == 2
    assert published.score_count == 4
    assert len({request.suite_version for request in client.experiments}) == 1
    assert len(client.generations) == 6
    assert all(generation["output_text"] not in {"old duplicate", "Dispatch"} for generation in client.generations)
    assert client.generations[0]["started_at"] == datetime(2026, 9, 17, 0, 0, 12, 345000, tzinfo=timezone.utc)
    assert client.generations[0]["completed_at"] == client.generations[0]["started_at"]
    assert client.generations[0]["metadata"]["harbor_timing_source"] == "atif_step_timestamp"
    assert all(generation["metadata"]["harbor_duration_known"] is False for generation in client.generations)
    assert client.generations[1]["started_at"] == client.generations[1]["completed_at"]
    assert client.generations[1]["metadata"]["harbor_timing_source"] == "ingestion_time"
    assert "Tool calls" in client.generations[0]["output_text"]
    assert {score.report_role for score in client.scores} == {"primary_verdict", "diagnostic"}
    assert sum(score.report_role == "primary_verdict" for score in client.scores) == 1
    assert {update["duration_ms"] for update in client.trial_updates} == {12345}
    assert {update["cost"] for update in client.trial_updates} == {0.25}
    assert {update["input_tokens"] for update in client.trial_updates} == {100}
    assert {update["output_tokens"] for update in client.trial_updates} == {20}
    assert client.generations[0]["metadata"]["harbor_prompt_tokens"] == 100
    assert client.generations[0]["metadata"]["harbor_completion_tokens"] == 20
    assert client.generations[0]["metadata"]["harbor_cached_tokens"] == 40
    assert client.generations[0]["usage"].input_tokens == 100
    assert client.generations[0]["usage"].output_tokens == 20
    assert client.generations[0]["usage"].cache_read_input_tokens == 40
    assert client.generations[0]["metadata"]["harbor_session_id"] == "session-0"
    assert client.generations[0]["conversation_id"] != "session-0"
    root_generations = {
        generation["conversation_id"]: generation["generation_id"]
        for generation in client.generations
        if generation["output_text"].startswith("Done")
    }
    assert all(score.generation_id == root_generations[score.conversation_id] for score in client.scores)
    replay = FakeClient()
    publish_harbor_job(job, client=replay, experiment_id="job-2", upload_artifacts=False)
    assert {item["conversation_id"] for item in client.generations}.isdisjoint(
        item["conversation_id"] for item in replay.generations
    )
    assert any(update["status"] == "failed" and "sandbox failed" in update["error"] for update in client.trial_updates)
    assert any(artifact["name"] == "agent__trajectory.json" for artifact in client.artifacts)
    assert any(artifact["name"] == "verifier__reward-details.json" for artifact in client.artifacts)


def test_missing_native_timing_stays_unknown(tmp_path: Path) -> None:
    job = tmp_path / "job"
    _write_job(job)
    path = job / "task-0" / "result.json"
    result = json.loads(path.read_text())
    result.pop("agent_execution")
    path.write_text(json.dumps(result))
    client = FakeClient()

    publish_harbor_job(job, client=client, upload_artifacts=False)

    assert any(update["duration_ms"] is None for update in client.trial_updates)


def test_empty_main_reward_is_rejected_before_remote_writes(tmp_path: Path) -> None:
    job = tmp_path / "job"
    _write_job(job)
    client = FakeClient()

    with pytest.raises(ValueError, match="main_reward must be nonempty"):
        publish_harbor_job(job, client=client, main_reward=" ")

    assert client.experiments == []


def test_publishes_and_uses_canonical_stored_cases(tmp_path: Path) -> None:
    job = tmp_path / "job"
    _write_job(job)
    client = FakeClient()
    suites = FakeSuitesClient()

    published = publish_harbor_job(
        job,
        client=client,
        test_suites_client=suites,
        suite_publication_policy="subset",
        upload_artifacts=False,
    )

    assert published.trial_count == 2
    assert suites.calls == 1
    assert {request.suite_version for request in client.experiments} == {"server-version"}
    snapshots = [request["test_case"] for request in client.trials]
    assert all(snapshot["input"] == {"task_name": "[REDACTED]"} for snapshot in snapshots)
    assert all(request["metadata"]["agento11y_case_transformed"] for request in client.trials)


@pytest.mark.parametrize("instrumented", [True, False])
def test_live_harbor_registers_before_sandbox_and_injects_context(tmp_path, monkeypatch, instrumented):
    import asyncio
    import sys

    from agento11y.experiments import TestCase, TestSuite
    from agento11y_harbor import run_harbor
    from opentelemetry import trace
    from opentelemetry.sdk.trace import TracerProvider

    if isinstance(trace.get_tracer_provider(), trace.ProxyTracerProvider):
        trace.set_tracer_provider(TracerProvider())
    client = FakeClient()
    seen = []

    class Native:
        paths = SimpleNamespace(trial_dir=tmp_path)

        @classmethod
        async def create(cls, config):
            assert client.experiments[0].planned_trial_count == 1
            assert len(client.trials) == 1
            seen.append(config.agent.env)
            return cls()

        async def run(self):
            return SimpleNamespace(
                model_dump=lambda **_: {
                    "verifier_result": {"rewards": {"reward": 1}},
                    "step_results": [
                        {"agent_result": {"n_input_tokens": 20, "n_output_tokens": 2, "cost_usd": 0.004}},
                        {
                            "agent_result": {
                                "n_input_tokens": 30,
                                "n_output_tokens": 3,
                                "cost_usd": 0.006,
                                "metadata": {"agento11y.conversation_id": "original-agent-conversation"}
                                if instrumented
                                else {},
                            }
                        },
                    ],
                }
            )

    monkeypatch.setitem(sys.modules, "harbor.trial.trial", SimpleNamespace(Trial=Native))
    _write_trajectory(tmp_path)
    config = SimpleNamespace(agent=SimpleNamespace(name="agent", model_name="model", env={}))
    suite = TestSuite("harbor-live", test_cases=[TestCase("case")])
    published = asyncio.run(run_harbor(suite, {"case": config}, client=client))
    assert seen[0]["TRACEPARENT"].startswith("00-")
    assert seen[0]["AGENTO11Y_TEST_CASE_ID"] == "case"
    assert config.agent.env == {}
    assert client.trial_updates[0]["input_tokens"] == 50
    assert client.experiments[0].metadata["execution_kind"] == "agent_run"
    if instrumented:
        assert client.trial_updates[0]["conversation_id"] == "original-agent-conversation"
        assert not client.generations
    else:
        assert client.scores[0].generation_id == client.generations[0]["generation_id"]
        assert client.trial_updates[0]["conversation_id"] == client.generations[0]["conversation_id"]
    assert client.scores[0].report_role == "primary_verdict"
    assert published.candidate == "agent / model"


def test_live_harbor_rejects_inconsistent_step_conversations():
    from agento11y_harbor.integration import _conversation_id

    result = {
        "step_results": [
            {"agent_result": {"metadata": {"agento11y.conversation_id": "first"}}},
            {"agent_result": {"metadata": {"agento11y.conversation_id": "second"}}},
        ]
    }
    with pytest.raises(ValueError, match="inconsistent"):
        _conversation_id(result)


def test_empty_native_logs_are_not_uploaded(tmp_path):
    from agento11y_harbor.integration import _upload_artifacts

    verifier = tmp_path / "verifier"
    verifier.mkdir()
    (verifier / "test-stdout.txt").write_text("")
    (verifier / "test-stderr.txt").write_text("")
    (verifier / "reward.txt").write_text("1")
    uploaded = []
    trial = SimpleNamespace(artifact=lambda name, **kwargs: uploaded.append(name))
    _upload_artifacts(trial, tmp_path, 100)
    assert uploaded == ["verifier__reward.txt"]


@pytest.mark.parametrize(
    "mode", ["instrumented", "native", "missing_reward", "runtime_error", "export_error", "interrupted"]
)
def test_job_plugin_publishes_live_trial(tmp_path, monkeypatch, mode):
    import asyncio

    from agento11y.experiments import Experiment
    from agento11y_harbor import plugin as plugin_module

    client = FakeClient()
    monkeypatch.setattr(
        plugin_module,
        "experiment",
        lambda name, **kwargs: Experiment(client, name=name, **kwargs),
    )
    callbacks = {}
    job = SimpleNamespace(
        id="job-1",
        config=SimpleNamespace(job_name="terminal-bench"),
        on_trial_started=lambda callback: callbacks.setdefault("start", callback),
        on_agent_started=lambda callback: callbacks.setdefault("agent_start", callback),
        on_trial_ended=lambda callback: callbacks.setdefault("end", callback),
    )
    result = {
        "task_name": "fix-bug",
        "task_id": {"name": "fix-bug"},
        "task_checksum": "sha-task",
        "source": "terminal-bench@2.0",
        "agent_info": {
            "name": "codex",
            "version": "1",
            "model_info": {"provider": "openai", "name": "gpt-5"},
        },
        "agent_result": {
            "n_input_tokens": 20,
            "n_output_tokens": 5,
            "cost_usd": 0.01,
            "metadata": {"agento11y.conversation_id": "conversation-1"},
        },
        "verifier_result": {"rewards": {"reward": 1}},
    }
    _write_trajectory(tmp_path / "trial-1")
    if mode != "instrumented":
        result["agent_result"]["metadata"] = {}
    if mode == "missing_reward":
        result["verifier_result"] = {}
    if mode == "runtime_error":
        result["exception_info"] = {"exception_type": "TimeoutError", "exception_message": "agent deadline exceeded"}
    if mode == "export_error":

        def fail_export(*args):
            raise RuntimeError("trajectory export failed")

        from agento11y_harbor import integration

        monkeypatch.setattr(integration, "_publish_trajectory", fail_export)
    event = SimpleNamespace(
        event=SimpleNamespace(value="start"),
        result=SimpleNamespace(model_dump=lambda **_: result),
        config=SimpleNamespace(trials_dir=tmp_path, trial_name="trial-1"),
        trial_id="native-trial-1",
    )
    plugin = plugin_module.Agento11yPlugin(upload_artifacts=False)

    asyncio.run(plugin.on_job_start(job))
    asyncio.run(callbacks["start"](event))
    asyncio.run(callbacks["agent_start"](event))
    event.event.value = "end"
    if mode != "interrupted":
        # A failed export must not raise out of the hook: that would cancel sibling trials.
        asyncio.run(callbacks["end"](event))
    asyncio.run(plugin.on_job_end(SimpleNamespace()))

    assert len(client.experiments) == 1
    assert client.trials[0]["status"] == "running"
    assert client.experiments[0].metadata["execution_kind"] == "agent_run"
    if mode in {"instrumented", "native"}:
        assert client.trial_updates[0]["cost"] == 0.01
        assert client.scores[0].report_role == "primary_verdict"
        assert client.finalized[0][1] == "completed"
        if mode == "instrumented":
            assert client.trial_updates[0]["conversation_id"] == "conversation-1"
            assert not client.generations
        else:
            assert client.scores[0].generation_id == client.generations[0]["generation_id"]
            assert client.trial_updates[0]["conversation_id"] == client.generations[0]["conversation_id"]
    else:
        assert client.finalized[0][1] == "failed"
        assert client.trial_updates[0]["status"] == "failed"
        assert client.trial_updates[0]["error"] not in {"None", ""}
        assert all(score.report_role == "diagnostic" for score in client.scores)
    if mode == "export_error":
        assert "trajectory export failed" in client.trial_updates[0]["error"]
        assert "trajectory export failed" in client.finalize_errors[client.finalized[0][0]]


def _write_trajectory(root):
    (root / "agent").mkdir(parents=True, exist_ok=True)
    (root / "agent" / "trajectory.json").write_text(
        json.dumps(
            {
                "schema_version": "ATIF-v1.8",
                "session_id": "session-live",
                "agent": {"name": "terminus-2", "model_name": "anthropic/claude-haiku-4-5-20251001"},
                "steps": [
                    {"step_id": 1, "source": "user", "message": "Compute the paid orders total"},
                    {
                        "step_id": 2,
                        "source": "agent",
                        "message": "Wrote the summary",
                        "metrics": {"prompt_tokens": 20, "completion_tokens": 5},
                    },
                ],
            }
        )
    )


@pytest.mark.parametrize("failure", ["native_exception", "result_exception", "missing_reward"])
def test_live_harbor_finalizes_operational_failures_without_false_verdict(tmp_path, monkeypatch, failure):
    import asyncio
    import sys

    from agento11y.experiments import TestCase, TestSuite
    from agento11y_harbor import run_harbor

    class Native:
        paths = SimpleNamespace(trial_dir=tmp_path)

        @classmethod
        async def create(cls, config):
            if failure == "native_exception":
                raise RuntimeError("sandbox unavailable")
            return cls()

        async def run(self):
            result = {}
            if failure == "result_exception":
                result = {
                    "exception_info": {
                        "exception_type": "TimeoutError",
                        "exception_message": "agent deadline exceeded",
                    },
                    "verifier_result": {"rewards": {"reward": 1}},
                }
            return SimpleNamespace(model_dump=lambda **_: result)

    monkeypatch.setitem(sys.modules, "harbor.trial.trial", SimpleNamespace(Trial=Native))
    config = SimpleNamespace(agent=SimpleNamespace(name="agent", model_name="model", env={}))
    client = FakeClient()
    asyncio.run(run_harbor(TestSuite("live", test_cases=[TestCase("case")]), {"case": config}, client=client))
    assert client.finalized[0][1] == "failed"
    assert client.trial_updates[0]["status"] == "failed"
    assert client.trial_updates[0]["error"] not in {"None", ""}
    assert all(score.report_role == "diagnostic" for score in client.scores)
