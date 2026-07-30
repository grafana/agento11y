"""Cloud experiments: run benchmarks/evals against Agent Observability with a single ingest token.

This is the high-level surface for evaluation harnesses. It writes over the v1
one-token ingest path (run upsert, generation export, score export, finalize).
Experimental OpenTelemetry GenAI eval telemetry is available only when
``use_experimental_otel=True`` or ``AGENTO11Y_USE_EXPERIMENTAL_OTEL=true``.

Quick start (``endpoint`` is your Grafana Cloud Agent Observability URL, ``tenant_id`` your
stack id, and ``ingest_token`` your Cloud ingestion API key)::

    import os
    from agento11y.experiments import Client, Experiment, TestSuite, TestCase, Evaluator

    client = Client(
        os.environ["AGENTO11Y_ENDPOINT"],
        tenant_id=os.environ.get("AGENTO11Y_AUTH_TENANT_ID", ""),
        ingest_token=os.environ["AGENTO11Y_AUTH_TOKEN"],
    )
    suite = TestSuite(suite_id="smoke", name="Smoke", test_cases=[
        TestCase(test_case_id="add", input="2+2", expected="4"),
    ])
    verifier = Evaluator(evaluator_id="exact", version="1", kind="deterministic")

    with Experiment(client, experiment_id="run-1", name="smoke run", suite=suite) as exp:
        for case in suite.test_cases:
            with exp.trial(case) as trial:
                answer = run_agent(case.input)
                trial.final_score(answer == case.expected, evaluator=verifier)

Cross-process (e.g. a verifier container) opens a trial from a serialized ref::

    from agento11y.experiments import Trial, TrialRef
    ref = TrialRef.from_env()
    if ref is None:
        raise RuntimeError("missing Agent Observability trial environment")
    trial = Trial.from_ref(client, ref)
    trial.final_score(0.82, passed=True); trial.close()
"""

from __future__ import annotations

from . import otel, score
from .client import Client
from .evaluators import (
    DEFAULT_LLM_JUDGE_PROMPT,
    EvaluationResult,
    GraderGeneration,
    LLMJudge,
    OutputEvaluator,
    RegexJudge,
)
from .experiment import Experiment, Trial, experiment, experiment_from_suite, stable_id
from .suites import PushedSuite, TestSuitesClient
from .types import (
    Candidate,
    Evaluator,
    EvaluatorKind,
    ExperimentStatus,
    TestCase,
    TestSuite,
    TrialRef,
    TrialStatus,
    normalize_evaluator_kind,
)

__all__ = [
    "Client",
    "TestSuitesClient",
    "PushedSuite",
    "Experiment",
    "Trial",
    "TrialRef",
    "TestSuite",
    "TestCase",
    "Candidate",
    "Evaluator",
    "EvaluatorKind",
    "DEFAULT_LLM_JUDGE_PROMPT",
    "EvaluationResult",
    "GraderGeneration",
    "OutputEvaluator",
    "LLMJudge",
    "RegexJudge",
    "ExperimentStatus",
    "TrialStatus",
    "normalize_evaluator_kind",
    "experiment",
    "experiment_from_suite",
    "stable_id",
    "score",
    "otel",
]
