"""Grafana Agent O11y Evals: the default text-reference workflow.

Cases use ``input.prompt`` and a reviewed ``expected.assistant_response``.
Exact matching is deliberately strict; use native framework metrics or stored
judges when correctness cannot be expressed as an exact reference answer.
"""

from __future__ import annotations

import time
from collections.abc import Callable
from copy import deepcopy
from dataclasses import dataclass
from typing import Any

from ..context import with_conversation_id
from .cases import case_value
from .client import Client
from .control import EvaluatorsClient, StoredEvaluator
from .experiment import Experiment, Trial, experiment, stable_id
from .plan import EvaluationPlan
from .types import Candidate, Evaluator, TestCase, TestSuite


@dataclass(frozen=True, slots=True)
class AgentOutput:
    """Text plus explicit correlation/flush for an independently instrumented agent."""

    text: str
    conversation_id: str = ""
    flush: Callable[[], Any] | None = None


def run_evals(
    suite: TestSuite,
    target: Callable[[Any], str | AgentOutput],
    *,
    name: str = "Grafana Agent O11y Evals",
    experiment_id: str = "",
    client: Client | None = None,
    candidate: Candidate | dict[str, str] | None = None,
    record_io: bool = False,
    evaluate: Callable[[Trial, TestCase, str], None] | None = None,
    use_experimental_otel: bool = True,
    plan: EvaluationPlan | None = None,
    evaluators_client: EvaluatorsClient | None = None,
    input_selector: str = "input.prompt",
) -> Experiment:
    """Run a published suite against a text target and publish exact-match verdicts.

    Validate every case before creating a run. The target normally owns its
    instrumentation. Opt into ``record_io`` for an uninstrumented target to
    create input/output anchor generations (which captures their content).
    The existing experiment lifecycle records execution failures and finalizes;
    a failed verdict is a completed trial, not a runner failure.

    ``evaluate(trial, case, answer)`` replaces exact match with caller-owned
    local scoring or a stored evaluator invocation. OTel is enabled by default;
    configure a TracerProvider/exporter in the host application. Duration covers
    target execution only, excluding evaluator work.

    ``plan`` combines local and stored judges, validates selectors up front,
    provisions exact remote definitions and emits one all-required verdict.
    It is mutually exclusive with ``evaluate``. With a plan, target/judge
    failures remain inspectable and successful sibling cases are retained.
    ``input_selector='input'`` passes structured context rather than just the
    prompt. AgentOutput supports explicit correlation and an export flush.
    """
    if plan is not None and evaluate is not None:
        raise ValueError("choose plan or evaluate, not both")
    _validate_cases(suite, input_selector)
    if evaluate is None and plan is None:
        for case in suite.cases:
            if not isinstance(case.expected, dict) or not isinstance(case.expected.get("assistant_response"), str):
                raise ValueError(f"{case.test_case_id}: expected.assistant_response must be a reviewed string")
    if plan is not None:
        plan.validate(suite.cases)
        if any(isinstance(check.judge, StoredEvaluator) for check in plan.checks):
            plan.provision(evaluators_client or EvaluatorsClient())
    with experiment(
        name,
        experiment_id=experiment_id,
        suite=suite,
        client=client,
        candidate=candidate,
        planned_trial_count=len(suite.cases),
        use_experimental_otel=use_experimental_otel,
        metadata={
            "framework": "grafana",
            "execution_kind": "agent_run",
            "input_selector": input_selector,
            **({"evaluation_plan": plan.provenance()} if plan is not None else {}),
        },
    ) as run:
        for case in suite.cases:
            with run.trial(case) as trial:
                answer = _run_target(
                    run, trial, case, target, input_selector=input_selector, keep_going=plan is not None
                )
                if answer is None:
                    continue
                if record_io:
                    trial.record_io(input=case_value(case, input_selector), output=answer)
                if plan is not None:
                    plan.evaluate(trial, case, answer, run.client)
                    continue
                if evaluate is not None:
                    evaluate(trial, case, answer)
                    continue
                passed = answer == case.expected["assistant_response"]
                trial.score(
                    "exact_match",
                    passed,
                    passed=passed,
                    report_role="primary_verdict",
                    evaluator=Evaluator("grafana.exact_match", version="1", kind="custom"),
                    explanation="Answer matches the reviewed reference"
                    if passed
                    else "Answer differs from the reviewed reference",
                )
    return run


def _validate_cases(suite: TestSuite, input_selector: str) -> None:
    """Reject a suite the text-target runners cannot execute, before any run exists."""

    if not suite.cases:
        raise ValueError("the suite must contain at least one test case")
    if not (input_selector == "input" or input_selector.startswith("input.")):
        raise ValueError("input_selector must select input, never expected values or metadata")
    if len({case.test_case_id for case in suite.cases}) != len(suite.cases):
        raise ValueError("suite contains duplicate case IDs")
    for case in suite.cases:
        selected_input = case_value(case, input_selector)
        if input_selector == "input.prompt" and (not isinstance(selected_input, str) or not selected_input.strip()):
            raise ValueError(f"{case.test_case_id}: input.prompt must be a nonempty string")


def _run_target(
    run: Experiment,
    trial: Trial,
    case: TestCase,
    target: Callable[[Any], str | AgentOutput],
    *,
    input_selector: str,
    keep_going: bool,
    require_flush: bool = False,
) -> str | None:
    """Run the target for one case and flush its telemetry.

    Returns the answer, or ``None`` after marking the trial errored when
    ``keep_going`` is set. Without ``keep_going`` the failure propagates.
    ``require_flush`` raises for a target that can't flush its own client,
    since every later case would fail the same way.
    """

    trial.bind_conversation(stable_id("conv", run.experiment_id, case.test_case_id, trial.ref.attempt))
    started = time.perf_counter()
    try:
        with with_conversation_id(trial.conversation_id):
            result = target(deepcopy(case_value(case, input_selector)))
    except Exception as error:
        if not keep_going:
            raise
        trial.mark_errored(f"target: {type(error).__name__}: {error}")
        return None
    finally:
        trial.set_duration(int((time.perf_counter() - started) * 1000))
    if require_flush and (not isinstance(result, AgentOutput) or result.flush is None):
        raise TypeError("target must return AgentOutput with flush set to your instrumentation client's flush")
    answer = result.text if isinstance(result, AgentOutput) else result
    if not isinstance(answer, str):
        if not keep_going:
            raise TypeError("target must return a string")
        trial.mark_errored("target must return a string or AgentOutput with string text")
        return None
    try:
        if isinstance(result, AgentOutput):
            if result.conversation_id:
                trial.bind_conversation(result.conversation_id)
            if result.flush is not None:
                result.flush()
        run.client.flush_generations()
    except Exception as error:
        if not keep_going:
            raise
        trial.mark_errored(f"telemetry flush: {error}")
        return None
    return answer
