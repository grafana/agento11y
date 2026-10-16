"""Native Harbor job plugin for live Agent Observability experiments."""

from __future__ import annotations

import asyncio
import atexit
import logging
import math
import threading
from collections import defaultdict
from collections.abc import Callable, Mapping
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from agento11y.experiments import TestSuite, TrialStatus, experiment, stable_id

from .integration import (
    ARTIFACT_MAX_BYTES,
    _candidate,
    _close_failed_trial,
    _experiment_id,
    _experiment_name,
    _group_identity,
    _is_terminal_conflict,
    _record_guarded,
    _run_error,
    _test_case,
    _trial_error,
)

logger = logging.getLogger("agento11y_harbor")


@dataclass(slots=True)
class _LiveExperiment:
    context: Any
    experiment: Any
    suite: TestSuite
    errors: list[str] = field(default_factory=list)
    # Set when the experiment cannot accept trials; its trials are skipped.
    unavailable: str = ""
    # Trial names that started before the experiment could be opened and
    # haven't been published since.
    skipped: set[str] = field(default_factory=set)


@dataclass(slots=True)
class _LiveTrial:
    trial: Any
    live: _LiveExperiment
    retries: int = 0
    # The END event of a failed attempt that Harbor may still retry.
    pending: Any = None


class Agento11yPlugin:
    """Publish a normal concurrent Harbor job through its lifecycle hooks.

    Hooks never raise: an exception inside a Harbor hook cancels every sibling
    trial in the job. Publishing failures mark the affected trial errored, log
    a warning, and finalize the experiment as failed. If an experiment can't
    be opened, the next trial of that candidate tries again, and the trials
    left out are reported when the job ends. If the process exits
    before Harbor ends the job (for example, on Ctrl-C), open trials are marked
    errored and the experiments are finalized as failed.
    """

    def __init__(
        self,
        *,
        name: str = "",
        experiment_id: str = "",
        main_reward: str = "reward",
        pass_threshold: float = 1.0,
        upload_artifacts: bool = True,
    ) -> None:
        # Harbor JSON-decodes --plugin-kwarg values, so experiment_id=2024 arrives as an int.
        self.name = str(name).strip()
        self.experiment_id = str(experiment_id).strip()
        self.main_reward = str(main_reward).strip()
        self.pass_threshold = float(pass_threshold)
        self.upload_artifacts = bool(upload_artifacts)
        if not self.main_reward or not math.isfinite(self.pass_threshold):
            raise ValueError("main_reward must be nonempty and pass_threshold finite")
        self._job: Any = None
        self._lock = threading.Lock()
        self._experiments: dict[str, _LiveExperiment] = {}
        # Keyed by Harbor trial name, which every retry of one attempt reuses.
        self._trials: dict[str, _LiveTrial] = {}
        # Trials Harbor started whose agent hasn't started yet, by trial name.
        self._started: dict[str, Any] = {}
        # Trial names not published because their experiment could not be opened, by group identity.
        self._unpublished: dict[str, set[str]] = defaultdict(set)
        self._attempts: dict[tuple[str, str], int] = defaultdict(int)
        # One worker keeps publishing in hook order without occupying the default
        # executor, which Harbor uses for its own blocking work.
        self._executor = ThreadPoolExecutor(max_workers=1, thread_name_prefix="agento11y-harbor")

    async def on_job_start(self, job: Any) -> None:
        self._job = job
        job.on_trial_started(self._on_trial_started)
        job.on_agent_started(self._on_agent_started)
        job.on_trial_ended(self._on_trial_ended)
        atexit.register(self._finish_at_exit)

    async def on_job_end(self, job_result: Any) -> None:
        del job_result
        await self._run(self._finish)
        atexit.unregister(self._finish_at_exit)
        self._executor.shutdown(wait=False)

    def _finish_at_exit(self) -> None:
        """Harbor skips on_job_end when the job is interrupted or crashes."""

        if not self._lock.acquire(timeout=30):
            logger.warning("agento11y: Harbor experiments were left running; a publish was still in progress")
            return
        try:
            self._finish()
        except Exception:
            logger.warning("agento11y: could not finalize interrupted Harbor experiments", exc_info=True)
        finally:
            self._lock.release()

    async def _on_trial_started(self, event: Any) -> None:
        await self._run(self._start_trial, event)

    async def _on_agent_started(self, event: Any) -> None:
        await self._run(self._agent_started, event)

    async def _on_trial_ended(self, event: Any) -> None:
        await self._run(self._end_trial, event)

    async def _run(self, step: Callable[..., None], *args: Any) -> None:
        # Publishing makes blocking network calls, so it runs off the event loop.
        try:
            await asyncio.get_running_loop().run_in_executor(self._executor, self._locked, step, *args)
        except Exception:
            logger.warning("agento11y: Harbor hook failed; the job continues", exc_info=True)

    def _locked(self, step: Callable[..., None], *args: Any) -> None:
        with self._lock:
            step(*args)

    def _start_trial(self, event: Any) -> None:
        key = _trial_key(event)
        open_trial = self._trials.get(key)
        if open_trial is not None:
            # A Harbor retry: keep the one logical attempt open and drop the
            # superseded failure, so only the final outcome is published.
            open_trial.retries += 1
            open_trial.pending = None
            return
        # Harbor fills in the agent version only after agent setup, and an
        # experiment's candidate can't change once it exists. So the trial (and
        # its experiment) opens when the agent starts, or at END if setup failed.
        self._started[key] = event

    def _agent_started(self, event: Any) -> None:
        key = _trial_key(event)
        # Each step of a multi-step task starts the agent again.
        if key not in self._trials:
            self._open_trial(key, event)

    def _open_trial(self, key: str, event: Any) -> _LiveTrial | None:
        self._started.pop(key, None)
        result = event.result.model_dump(mode="json")
        live = self._live_experiment(result, key)
        if live.unavailable:
            logger.warning("agento11y: Harbor trial %s is not published: %s", key, live.unavailable)
            return None
        try:
            case = _test_case(result, live.suite.suite_id)
            attempt_key = (live.experiment.experiment_id, case.test_case_id)
            self._attempts[attempt_key] += 1
            trial = live.experiment.trial(case, attempt=self._attempts[attempt_key])
        except Exception as error:
            live.skipped.discard(key)
            live.errors.append(f"{key}: {type(error).__name__}: {error}")
            return None
        try:
            # START and END run in different worker threads, so the trial opens
            # without OTel context (see _live_experiment) and closes explicitly.
            trial.__enter__()
        except Exception as error:
            if _is_terminal_conflict(error):
                live.unavailable = (
                    f"experiment {live.experiment.experiment_id!r} is already finalized; "
                    "pass a new experiment_id plugin kwarg to publish this job again"
                )
                logger.warning("agento11y: %s", live.unavailable)
                _close_quietly(trial)
                return None
            live.skipped.discard(key)
            _close_failed_trial(trial, error)
            live.errors.append(_trial_error(trial))
            return None
        # A trial that couldn't open its experiment at agent start is retried at END,
        # so it only stops counting as skipped once it's published or recorded as errored.
        live.skipped.discard(key)
        open_trial = _LiveTrial(trial, live)
        self._trials[key] = open_trial
        return open_trial

    def _end_trial(self, event: Any) -> None:
        key = _trial_key(event)
        open_trial = self._trials.get(key) or self._open_trial(key, event)
        if open_trial is None:
            return
        if self._may_retry(event.result.model_dump(mode="json"), open_trial.retries):
            open_trial.pending = event
            return
        del self._trials[key]
        self._complete(open_trial, event)

    def _may_retry(self, result: Mapping[str, Any], retries: int) -> bool:
        """Mirrors Harbor's TrialQueue retry decision for a finished attempt."""

        info = result.get("exception_info")
        if not isinstance(info, Mapping):
            return False
        retry = getattr(getattr(self._job, "config", None), "retry", None)
        if retries >= int(getattr(retry, "max_retries", 0) or 0):
            return False
        kind = str(info.get("exception_type") or "")
        exclude = getattr(retry, "exclude_exceptions", None)
        include = getattr(retry, "include_exceptions", None)
        if exclude and kind in exclude:
            return False
        return not include or kind in include

    def _complete(self, open_trial: _LiveTrial, event: Any) -> None:
        trial, live = open_trial.trial, open_trial.live
        try:
            _record_guarded(
                trial,
                live.experiment.client,
                event.result.model_dump(mode="json"),
                Path(event.config.trials_dir) / event.config.trial_name,
                main_reward=self.main_reward,
                pass_threshold=self.pass_threshold,
                upload_artifacts=self.upload_artifacts,
                artifact_max_bytes=ARTIFACT_MAX_BYTES,
            )
        except Exception as error:
            # Only a finalized-experiment conflict reaches here.
            trial.mark_errored(f"Harbor result export failed: {error}")
        self._close_trial(trial, live)

    @staticmethod
    def _close_trial(trial: Any, live: _LiveExperiment) -> None:
        try:
            trial.close()
        except Exception as error:
            _close_failed_trial(trial, error)
        if trial.status == TrialStatus.ERRORED.value:
            live.errors.append(_trial_error(trial))

    def _live_experiment(self, result: dict[str, Any], trial_key: str) -> _LiveExperiment:
        identity = _group_identity(result)
        existing = self._experiments.get(identity)
        if existing is not None:
            return existing
        source = str(result.get("source") or "adhoc")
        suite = TestSuite(
            suite_id=stable_id("harbor-suite", source),
            version=stable_id("harbor-live", self._job.id, source),
            name=source,
        )
        run_id = _experiment_id(self.experiment_id or str(self._job.id), result)
        try:
            context = experiment(
                _experiment_name(self.name or f"Harbor {self._job.config.job_name}", result),
                experiment_id=run_id,
                suite=suite,
                candidate=_candidate(result),
                # Trial spans would attach an OTel context in one hook thread and
                # detach it in another. The agent runs in Harbor's sandbox, so
                # conversations link through IDs instead of trace context.
                use_experimental_otel=False,
                metadata={
                    "framework": "harbor",
                    "execution_mode": "live",
                    "execution_kind": "agent_run",
                    "harbor_job_id": str(self._job.id),
                },
            )
            opened = context.__enter__()
        except Exception as error:
            # Not cached: the next trial of this candidate tries to open it again.
            self._unpublished[identity].add(trial_key)
            return _LiveExperiment(None, None, suite, unavailable=f"cannot open experiment {run_id!r}: {error}")
        live = _LiveExperiment(context, opened, suite, skipped=self._unpublished.pop(identity, set()))
        print(f"Agent Observability experiment: {opened.url}", flush=True)
        self._experiments[identity] = live
        return live

    def _finish(self) -> None:
        # Trials whose agent never started are reported as errored below.
        for key, event in list(self._started.items()):
            try:
                self._open_trial(key, event)
            except Exception:
                logger.warning("agento11y: could not record a Harbor trial", exc_info=True)
        for open_trial in list(self._trials.values()):
            try:
                if open_trial.pending is not None:
                    self._complete(open_trial, open_trial.pending)
                else:
                    open_trial.trial.mark_errored("Harbor job ended before the trial result was reported")
                    self._close_trial(open_trial.trial, open_trial.live)
            except Exception:
                logger.warning("agento11y: could not close a Harbor trial", exc_info=True)
        self._trials.clear()
        for live in self._experiments.values():
            if live.context is None:
                continue
            if live.skipped:
                live.errors.append(
                    f"{len(live.skipped)} Harbor trial(s) were not published because the experiment could not be "
                    "opened; export the job directory with a new --experiment-id to publish every trial"
                )
            error = RuntimeError(_run_error(live.errors)) if live.errors else None
            try:
                live.context.__exit__(type(error) if error else None, error, None)
            except Exception:
                logger.warning(
                    "agento11y: could not finalize experiment %s",
                    live.experiment.experiment_id,
                    exc_info=not live.unavailable,
                )
        self._experiments.clear()
        for skipped in self._unpublished.values():
            logger.warning(
                "agento11y: %d Harbor trial(s) were not published because their experiment could not be opened; "
                "publish them with `agento11y harbor export %s`",
                len(skipped),
                getattr(self._job, "job_dir", "<job-dir>"),
            )
        self._unpublished.clear()


def _trial_key(event: Any) -> str:
    return str(getattr(event.config, "trial_name", "") or event.trial_id)


def _close_quietly(trial: Any) -> None:
    try:
        trial.close()
    except Exception:
        pass
