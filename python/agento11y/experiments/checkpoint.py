"""Run an actual agent against a published suite version and its pinned evaluators."""

from __future__ import annotations

import math
import time
from collections.abc import Callable, Mapping, Sequence
from typing import Any

from ..errors import EvaluationTimeoutError
from .client import Client
from .evals import (
    AgentOutput,
    _caller_metadata,
    _caller_tags,
    _experiment_id_or_env,
    _run_target,
    _validate_cases,
)
from .experiment import Experiment, experiment
from .suites import TestSuitesClient
from .types import Candidate, ExperimentStatus


def run_checkpoint(
    suite_id: str,
    version: str,
    target: Callable[[Any], AgentOutput],
    *,
    candidate: Candidate | dict[str, str] | None = None,
    name: str = "",
    experiment_id: str = "",
    expected_bindings: list[dict[str, Any]] | None = None,
    input_selector: str = "input.prompt",
    timeout: float = 3600.0,
    evaluation_timeout: float = 300.0,
    client: Client | None = None,
    test_suites_client: TestSuitesClient | None = None,
    use_experimental_otel: bool = True,
    metadata: Mapping[str, Any] | None = None,
    tags: Sequence[str] | None = None,
) -> Experiment:
    """Run ``target`` once per case of a published suite version and grade it in Grafana Cloud.

    Pulls the exact published cases and the version's evaluator bindings, and
    resolves every case's grading inputs before calling the target. Each case
    is then graded with every binding, using the evaluators, mappings and
    report roles reviewed in Grafana.

    ``target`` receives the selected case input and returns ``AgentOutput``
    with ``flush`` set to the flush of the client that instruments your agent,
    so the conversation is stored before it's graded.

    A case whose target, telemetry flush or grading fails is recorded as
    errored, and the run continues with the next case. If any case could not
    be fully graded, the experiment is finalized as failed, because Agent
    Observability won't complete one with a failed or unfinished evaluation.

    ``timeout`` bounds the whole run, including the grading-input checks and
    grading: when it runs out, no further case or grading starts and a
    ``TimeoutError`` is raised after the experiment is finalized as failed. It
    can't interrupt a target call that is already running, so give your agent
    its own timeout.

    Pass ``expected_bindings`` (for example, the bindings shown in Grafana when
    you copied the runner) to refuse a version whose bindings have changed.
    Their evaluator, version, input mappings and report role are compared.

    ``metadata`` and ``tags`` are added to the run, for example CI provenance
    such as a commit SHA or PR number. The SDK's own provenance keys
    (``execution_kind``, ``framework``, ``input_selector``, ``evaluation_plan``,
    ``suite_id``, ``suite_version``) are reserved, and passing one raises
    ``ValueError``. Without ``experiment_id``, the run uses
    ``AGENTO11Y_EXPERIMENT_ID`` when it is set, so a CI wrapper can allocate
    the run ID; with it set, run one helper per process.
    """

    if not math.isfinite(timeout) or timeout <= 0:
        raise ValueError("timeout must be greater than zero")
    if not math.isfinite(evaluation_timeout) or evaluation_timeout <= 0:
        raise ValueError("evaluation_timeout must be greater than zero")
    user_metadata = _caller_metadata(metadata)
    run_tags = _caller_tags(tags)
    deadline = time.monotonic() + timeout

    control = test_suites_client or TestSuitesClient()
    suite = control.pull_suite(suite_id, version, require_published=True)
    _validate_cases(suite, input_selector)
    bindings = control.get_bindings(suite.suite_id, suite.version)
    # The checkpoint gate compares exactly one primary verdict per case, so refuse anything else up front.
    if sum(binding.get("report_role") == "primary_verdict" for binding in bindings) != 1:
        raise ValueError("publish exactly one primary verdict binding before running this suite version")
    if expected_bindings is not None and _binding_identities(expected_bindings) != _binding_identities(bindings):
        raise ValueError("the suite version's bindings differ from expected_bindings; review the version again")
    for index, case in enumerate(suite.cases):
        if time.monotonic() >= deadline:
            raise TimeoutError(
                f"run timed out after {timeout:g}s while checking grading inputs "
                f"for {index} of {len(suite.cases)} cases"
            )
        for binding in bindings:
            control.preview_binding(suite.suite_id, suite.version, case.test_case_id, binding["binding_id"])

    with experiment(
        name or f"{suite.name or suite.suite_id} v{suite.version}",
        experiment_id=_experiment_id_or_env(experiment_id),
        suite=suite,
        client=client,
        candidate=candidate,
        planned_trial_count=len(suite.cases),
        use_experimental_otel=use_experimental_otel,
        tags=run_tags,
        metadata={
            **user_metadata,
            "framework": "grafana",
            "execution_kind": "agent_run",
            "input_selector": input_selector,
        },
    ) as run:
        ungraded = 0
        timed_out = False
        for index, case in enumerate(suite.cases):
            if timed_out or time.monotonic() >= deadline:
                raise TimeoutError(f"run timed out after {timeout:g}s with {index} of {len(suite.cases)} cases run")
            with run.trial(case) as trial:
                answer = _run_target(
                    run, trial, case, target, input_selector=input_selector, keep_going=True, require_flush=True
                )
                if answer is None:
                    continue
                errors = []
                for binding in bindings:
                    remaining = deadline - time.monotonic()
                    if remaining <= 0:
                        timed_out = True
                        errors.append(f"{binding['binding_id']}: run timed out before grading")
                        continue
                    try:
                        trial.evaluate(binding_id=binding["binding_id"], timeout=min(evaluation_timeout, remaining))
                    except EvaluationTimeoutError as error:
                        # Only the run deadline, not a slow judge within evaluation_timeout, ends the run.
                        timed_out = timed_out or remaining < evaluation_timeout
                        errors.append(f"{binding['binding_id']}: {type(error).__name__}: {error}")
                    except Exception as error:
                        errors.append(f"{binding['binding_id']}: {type(error).__name__}: {error}")
                if errors:
                    # Successful bindings keep their scores; the trial is not reported as fully graded.
                    ungraded += 1
                    trial.mark_errored("grading: " + "; ".join(errors))
        if timed_out:
            raise TimeoutError(
                f"run timed out after {timeout:g}s with {len(suite.cases)} of {len(suite.cases)} cases run"
            )
        if ungraded:
            # Completing the experiment would be rejected while a binding evaluation is failed or unfinished.
            run.finalize(
                ExperimentStatus.FAILED, error=f"{ungraded} of {len(suite.cases)} cases could not be fully graded"
            )
    return run


def _binding_identities(bindings: list[dict[str, Any]]) -> dict[str, tuple[Any, ...]]:
    # Compare only what decides grading, so new response fields or null vs {} mappings don't break pinned jobs.
    return {
        str(binding.get("binding_id")): (
            binding.get("evaluator_id") or "",
            binding.get("evaluator_version") or "",
            dict(binding.get("input_mappings") or {}),
            binding.get("report_role") or "",
        )
        for binding in bindings
    }
