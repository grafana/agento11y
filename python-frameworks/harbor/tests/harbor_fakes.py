"""Shared fakes for the Harbor integration tests."""

from __future__ import annotations

import inspect
import json
from pathlib import Path
from types import SimpleNamespace
from typing import Any

from agento11y.errors import ConflictError, ConflictKind
from agento11y.experiments import Client

_RECORD_GENERATION = inspect.signature(Client.record_generation)


class FakeClient:
    """Records experiment writes. ``fail`` maps a method name to an error factory."""

    use_experimental_otel = False
    redact_secrets = False

    def __init__(self, *, terminal: set[str] | None = None) -> None:
        self.experiments: list[Any] = []
        self.trials: list[dict[str, Any]] = []
        self.trial_updates: list[dict[str, Any]] = []
        self.scores: list[Any] = []
        self.generations: list[dict[str, Any]] = []
        self.artifacts: list[dict[str, Any]] = []
        self.finalized: list[tuple[str, str]] = []
        self.finalize_errors: dict[str, str] = {}
        self.terminal = set(terminal or ())
        self.fail: dict[str, Any] = {}

    def _maybe_fail(self, method: str, **request: Any) -> None:
        factory = self.fail.get(method)
        error = factory(**request) if factory else None
        if error is not None:
            raise error

    def _check_terminal(self, experiment_id: str) -> None:
        if experiment_id in self.terminal:
            raise ConflictError(
                "agento11y trial create conflict: cannot mutate terminal experiment", kind=ConflictKind.TERMINAL
            )

    def upsert_experiment(self, request):
        self._maybe_fail("upsert_experiment", request=request)
        self.experiments.append(request)
        return request

    def upsert_trial(self, experiment_id, **request):
        self._check_terminal(experiment_id)
        self._maybe_fail("upsert_trial", experiment_id=experiment_id, **request)
        self.trials.append({"experiment_id": experiment_id, **request})
        return request

    def update_trial(self, experiment_id, trial_id, **request):
        self.trial_updates.append({"experiment_id": experiment_id, "trial_id": trial_id, **request})
        return request

    def export_scores(self, scores):
        self.scores.extend(scores)
        return len(scores)

    def record_generation(self, generation_id, **request):
        # Bind against the real client so a kwarg missing from core fails here.
        _RECORD_GENERATION.bind(self, generation_id, **request)
        self._maybe_fail("record_generation", generation_id=generation_id, **request)
        self.generations.append({"generation_id": generation_id, **request})
        return generation_id

    def flush_generations(self):
        return None

    def upload_artifact(self, **request):
        self._maybe_fail("upload_artifact", **request)
        self.artifacts.append(request)
        return {"artifact_id": f"artifact-{len(self.artifacts)}"}

    def finalize(self, experiment_id, status, *, error="", **_):
        self._maybe_fail("finalize", experiment_id=experiment_id, status=status)
        if experiment_id in self.terminal:
            raise ConflictError(f"experiment {experiment_id!r} is already finalized", kind=ConflictKind.TERMINAL)
        self.finalized.append((experiment_id, status))
        self.finalize_errors[experiment_id] = error

    def experiment_url(self, experiment_id):
        return f"https://stack.grafana.net/{experiment_id}"

    def status_of(self, experiment_id: str) -> str:
        return dict(self.finalized)[experiment_id]

    def final_trial_states(self) -> dict[str, dict[str, Any]]:
        return {update["trial_id"]: update for update in self.trial_updates}


def harbor_result(
    task: str = "fix-bug",
    *,
    agent: str = "codex",
    agent_version: str | None = "1.2.3",
    reward: float | None = 1,
    error: str | None = None,
    error_type: str = "RuntimeError",
    conversation_id: str = "",
) -> dict[str, Any]:
    """Returns a Harbor TrialResult JSON document."""

    result: dict[str, Any] = {
        "task_name": task,
        "task_id": {"name": task},
        "source": "terminal-bench@2.0",
        "task_checksum": f"sha-{task}",
        "config": {},
        "agent_info": {
            "name": agent,
            "version": agent_version,
            "model_info": {"provider": "openai", "name": "gpt-5"},
        },
        "agent_result": {
            "n_input_tokens": 20,
            "n_output_tokens": 5,
            "cost_usd": 0.01,
            "metadata": {"agento11y.conversation_id": conversation_id} if conversation_id else {},
        },
        "verifier_result": {"rewards": {"reward": reward}} if reward is not None else {},
        "agent_execution": {
            "started_at": "2026-09-17T00:00:00Z",
            "finished_at": "2026-09-17T00:00:12.345Z",
        },
    }
    if error:
        result["exception_info"] = {"exception_type": error_type, "exception_message": error}
    return result


def write_trajectory(root: Path, *, session: str = "session-live") -> None:
    (root / "agent").mkdir(parents=True, exist_ok=True)
    (root / "agent" / "trajectory.json").write_text(
        json.dumps(
            {
                "schema_version": "ATIF-v1.8",
                "session_id": session,
                "agent": {"name": "terminus-2", "model_name": "anthropic/claude-haiku-4-5-20251001"},
                "steps": [
                    {"step_id": 1, "source": "user", "message": "Compute the paid orders total"},
                    {
                        "step_id": 2,
                        "source": "agent",
                        "message": "Wrote the summary",
                        "timestamp": "2026-09-17T00:00:12.345Z",
                        "metrics": {"prompt_tokens": 20, "completion_tokens": 5},
                    },
                ],
            }
        )
    )


def write_job(root: Path, trials: dict[str, dict[str, Any]], *, job_id: str = "job-1") -> None:
    """Writes a Harbor job directory with one trial directory per entry."""

    root.mkdir(parents=True, exist_ok=True)
    (root / "result.json").write_text(json.dumps({"id": job_id, "stats": {}, "trial_results": []}))
    for name, result in trials.items():
        trial = root / name
        write_trajectory(trial, session=f"session-{name}")
        (trial / "result.json").write_text(json.dumps(result))


class FakeJob:
    """A Harbor Job double that records the plugin's trial hooks."""

    def __init__(self, *, job_id: str = "job-1", max_retries: int = 0) -> None:
        self.id = job_id
        self.config = SimpleNamespace(
            job_name="terminal-bench",
            retry=SimpleNamespace(
                max_retries=max_retries,
                include_exceptions=None,
                exclude_exceptions={"AgentTimeoutError"},
            ),
        )
        self.started: list[Any] = []
        self.agent_started: list[Any] = []
        self.ended: list[Any] = []

    def on_trial_started(self, callback):
        self.started.append(callback)
        return self

    def on_agent_started(self, callback):
        self.agent_started.append(callback)
        return self

    def on_trial_ended(self, callback):
        self.ended.append(callback)
        return self

    async def start(self, event) -> None:
        for callback in self.started:
            await callback(event)

    async def agent_start(self, event) -> None:
        for callback in self.agent_started:
            await callback(event)

    async def end(self, event) -> None:
        for callback in self.ended:
            await callback(event)


def trial_event(trials_dir: Path, trial_name: str, result: dict[str, Any], trial_id: str = "") -> Any:
    """Returns a TrialHookEvent double; ``result`` stays live so tests can mutate it."""

    return SimpleNamespace(
        result=SimpleNamespace(model_dump=lambda **_: result),
        config=SimpleNamespace(trials_dir=trials_dir, trial_name=trial_name),
        trial_id=trial_id or f"uuid-{trial_name}",
    )
