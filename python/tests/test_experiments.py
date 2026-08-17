"""Tests for the experiment surface (agento11y.experiments).

Exercises the ergonomic Experiment/Trial API against a fake client that captures
exported scores, and asserts the bilingual OTel telemetry (trial span identity
attributes + gen_ai.evaluation.result events) via an in-memory span exporter.
"""

from __future__ import annotations

import pickle
import sys
from pathlib import Path
from types import ModuleType, SimpleNamespace

import pytest
from agento11y.errors import EvaluationExecutionError, EvaluationTimeoutError, ValidationError
from agento11y.experiments import (
    EvaluationResult,
    Evaluator,
    Experiment,
    GraderGeneration,
    LLMJudge,
    RegexJudge,
    TestCase,
    TestSuite,
    Trial,
    TrialEvaluation,
    TrialEvaluationStatus,
    TrialRef,
    otel,
    score,
)
from agento11y.models import CreateExperimentRequest, ScoreItem, TokenUsage
from opentelemetry import trace
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter


def _evaluation(
    status: TrialEvaluationStatus,
    *,
    evaluation_id: str = "eval-1",
    experiment_id: str = "run-1",
    trial_id: str = "trial-1",
    error: str = "",
) -> TrialEvaluation:
    """Builds a stored-evaluator status record for the fake client to return."""

    return TrialEvaluation(
        evaluation_id=evaluation_id,
        experiment_id=experiment_id,
        trial_id=trial_id,
        test_case_id="add",
        conversation_id="conv-cloud",
        evaluator_id="helpfulness",
        evaluator_version="v3",
        status=status,
        error=error,
    )


class FakeClient:
    """Captures upsert/score/finalize calls instead of hitting the network."""

    def __init__(self, *, use_experimental_otel: bool = False) -> None:
        self.use_experimental_otel = use_experimental_otel
        self.upserts: list[CreateExperimentRequest] = []
        self.scores: list[ScoreItem] = []
        self.finalized: list[tuple[str, str, int | None]] = []
        self.generations: list[str] = []
        self.generation_calls: list[tuple] = []
        self.trials: list[tuple] = []
        self.trial_updates: list[tuple] = []
        self.artifacts: list[tuple] = []
        self.calls: list[str] = []
        self.evaluation_order: list[str] = []
        self.triggered_evaluations: list[tuple[str, str, str, str]] = []
        self.evaluation_result = _evaluation(TrialEvaluationStatus.SUCCESS)
        self.evaluation_statuses: list[TrialEvaluation] = []

    def upsert_experiment(self, request: CreateExperimentRequest):
        self.upserts.append(request)
        return None

    def export_scores(self, scores, *, raise_on_reject: bool = True) -> int:
        self.calls.append("export_scores")
        self.scores.extend(scores)
        return len(scores)

    def record_generation(self, generation_id, **kwargs) -> str:
        self.generations.append(generation_id)
        self.generation_calls.append((generation_id, kwargs))
        self.evaluation_order.append("generation")
        return generation_id

    def flush_generations(self) -> None:
        self.calls.append("flush_generations")
        self.evaluation_order.append("flush")

    def upsert_trial(self, experiment_id, *, trial_id, **kwargs) -> dict:
        self.trials.append((experiment_id, trial_id, kwargs.get("status"), kwargs.get("test_case")))
        return {"trial_id": trial_id}

    def update_trial(self, experiment_id, trial_id, **kwargs) -> dict:
        self.trial_updates.append((experiment_id, trial_id, kwargs.get("status"), kwargs))
        self.evaluation_order.append("update")
        return {"trial_id": trial_id}

    def trigger_trial_evaluation(self, experiment_id, trial_id, evaluator_id, evaluator_version="") -> TrialEvaluation:
        self.evaluation_order.append("trigger")
        self.triggered_evaluations.append((experiment_id, trial_id, evaluator_id, evaluator_version))
        return self.evaluation_result

    def get_trial_evaluation(self, experiment_id, trial_id, evaluation_id) -> TrialEvaluation:
        self.evaluation_order.append("status")
        if self.evaluation_statuses:
            return self.evaluation_statuses.pop(0)
        return self.evaluation_result

    def upload_artifact(
        self,
        *,
        parent_id,
        name,
        kind,
        content,
        mime="",
        parent_kind="test_case_trial",
        experiment_id="",
    ) -> dict:
        self.artifacts.append((experiment_id, parent_id, name, kind, mime, content))
        return {"artifact_id": f"art_{name}", "name": name, "kind": kind}

    def finalize(self, experiment_id, status="succeeded", *, score_count=None, error=""):
        self.finalized.append((experiment_id, status, score_count))
        return None

    def experiment_url(self, experiment_id: str) -> str:
        return f"http://ui/{experiment_id}"


class FailingExportClient(FakeClient):
    def export_scores(self, scores, *, raise_on_reject: bool = True) -> int:
        self.calls.append("export_scores")
        raise RuntimeError("score export failed")


def _span_exporter() -> InMemorySpanExporter:
    exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(exporter))
    trace.set_tracer_provider(provider)
    return exporter


def _suite() -> TestSuite:
    return TestSuite(
        suite_id="smoke",
        name="Smoke Suite",
        version="1.2.0",
        test_cases=[TestCase(test_case_id="add", name="Addition", input="2+2", expected="4")],
    )


def test_experiment_lifecycle_and_score_wire_fields() -> None:
    client = FakeClient()
    suite = _suite()
    verifier = Evaluator(evaluator_id="exact", version="1", kind="deterministic")

    with Experiment(
        client,
        experiment_id="run-1",
        name="smoke run",
        suite=suite,
        planned_trial_count=3,
    ) as exp:
        with exp.trial(suite.test_cases[0]) as trial:
            trial.final_score(1.0, passed=True, explanation="matched", evaluator=verifier)
            trial.check_score("json_valid", passed=True)
            trial.rubric_score("helpfulness", 0.9, explanation="clear")

    # Upsert sent experiment_id (not run_id) and source=external.
    assert len(client.upserts) == 1
    assert client.upserts[0].run_id == "run-1"
    assert client.upserts[0].suite_id == "smoke"
    assert client.upserts[0].suite_version == "1.2.0"
    assert client.upserts[0].planned_trial_count == 3

    # Three scores, all attributed to the run + trial + test case.
    assert len(client.scores) == 3
    keys = {s.score_key for s in client.scores}
    assert keys == {"final", "json_valid", "helpfulness"}
    # One typed trial was created (so the report rolls up) and finalized.
    assert len(client.trials) == 1
    assert client.trials[0][0] == "run-1"
    assert client.trials[0][3] == {
        "test_case_id": "add",
        "suite_id": "smoke",
        "suite_version": "1.2.0",
        "name": "Addition",
        "description": "",
        "tags": [],
        "category": "",
        "input": {"value": "2+2"},
        "expected": {"value": "4"},
        "metadata": {},
        "artifact_refs": [],
    }
    assert client.trial_updates and client.trial_updates[0][2] == "completed"

    for s in client.scores:
        assert s.experiment_id == "run-1"
        assert s.test_case_id == "add"
        assert s.resolved_experiment_id == "run-1"
        # The typed trial_id attributes the score (the trial was created on enter).
        assert s.trial_id == client.scores[0].trial_id
        assert s.trial_id  # non-empty
        assert s.metadata["task_id"] == "add"
        # No record_io()/bind_generation(): scores rely on trial_id, no generation.
        assert s.generation_id == ""
    assert client.generations == []

    # Headline score carries the verdict; evaluator kinds mapped to OTel set.
    final = next(s for s in client.scores if s.score_key == "final")
    assert final.passed is True
    assert final.evaluator_kind == "deterministic"
    rubric = next(s for s in client.scores if s.score_key == "helpfulness")
    assert rubric.evaluator_kind == "llm_judge"

    # Normal finalization lets the backend count stored scores authoritatively.
    assert client.finalized == [("run-1", "completed", None)]


def test_experiment_finalize_can_assert_score_count_explicitly() -> None:
    client = FakeClient()
    exp = Experiment(client, experiment_id="run-assert", auto_finalize=False)

    with exp:
        pass
    exp.finalize(score_count=4)

    assert client.finalized == [("run-assert", "completed", 4)]


def test_experiment_rejects_negative_planned_trial_count() -> None:
    with pytest.raises(ValueError, match="planned_trial_count must be non-negative"):
        Experiment(FakeClient(), planned_trial_count=-1)


def test_llm_judge_evaluates_publishes_transcript_and_links_score() -> None:
    client = FakeClient()
    suite = _suite()
    judge = LLMJudge(
        evaluator_id="judge.correctness",
        version="2026-07-21",
        invoke=lambda prompt: SimpleNamespace(
            content='{"score": 0.9, "passed": true, "explanation": "correct"}',
            usage_metadata={"input_tokens": 120, "output_tokens": 18, "total_tokens": 138},
        ),
        model_provider="anthropic",
        model_name="claude-test",
        prompt_template='Input: {input}\nExpected: {expected}\nOutput: {output}\nReturn {"score": 1}',
    )

    with Experiment(client, experiment_id="run-judge", suite=suite) as exp:
        with exp.trial(suite.test_cases[0], attempt=2) as trial:
            item = trial.evaluate_output(judge, input="2+2", expected="4", output="4")

    assert item.value.number == 0.9
    assert item.passed is True
    assert item.grader_conversation_id
    assert item.grader_generation_id
    assert item.metadata["judge_provider"] == "anthropic"
    assert item.metadata["judge_model"] == "claude-test"
    generation_id, generation = client.generation_calls[0]
    assert generation_id == item.grader_generation_id
    assert generation["conversation_id"] == item.grader_conversation_id
    assert generation["model_name"] == "claude-test"
    assert generation["usage"].input_tokens == 120
    assert generation["usage"].output_tokens == 18
    assert generation["input_text"].startswith("Input: 2+2")
    assert generation["input_text"].endswith('Return {"score": 1}')
    assert generation["output_text"].startswith('{"score"')


def test_regex_judge_scores_without_publishing_transcript() -> None:
    client = FakeClient()
    suite = _suite()
    judge = RegexJudge(evaluator_id="regex.answer", pattern=r"^4$", full_match=True)

    with Experiment(client, experiment_id="run-regex", suite=suite) as exp:
        with exp.trial(suite.test_cases[0]) as trial:
            item = trial.evaluate_output(judge, input="2+2", expected="4", output="4", score_key="final")

    assert item.passed is True
    assert item.value.boolean is True
    assert item.evaluator_kind == "deterministic"
    assert item.grader_conversation_id == ""
    assert client.generations == []


def test_record_evaluation_accepts_framework_produced_result() -> None:
    client = FakeClient()
    evaluator = Evaluator(evaluator_id="harbor.rubric", version="1", kind="llm_judge")
    result = EvaluationResult(
        evaluator=evaluator,
        value=0.8,
        passed=True,
        explanation="framework-owned trajectory passed",
        grader=GraderGeneration(
            input="harbor-rendered trajectory",
            output='{"score": 0.8}',
            model_provider="anthropic",
            model_name="claude-test",
            usage=TokenUsage(input_tokens=30, output_tokens=6, total_tokens=36),
        ),
    )

    with Experiment(client, experiment_id="run-framework", suite=_suite()) as exp:
        with exp.trial(exp.suite.test_cases[0]) as trial:
            item = trial.record_evaluation(result)

    assert item.evaluator_id == "harbor.rubric"
    assert item.grader_generation_id
    assert client.generation_calls[0][1]["input_text"] == "harbor-rendered trajectory"
    assert client.generation_calls[0][1]["usage"].total_tokens == 36


def test_llm_judge_invalid_response_is_explicit() -> None:
    judge = LLMJudge(
        evaluator_id="judge.invalid",
        invoke=lambda prompt: "not structured",
        model_name="test-model",
    )

    with pytest.raises(ValueError, match="did not contain a JSON object"):
        judge.evaluate_output(input="", output="answer")


def test_llm_judge_uses_valid_score_object_after_unrelated_json() -> None:
    judge = LLMJudge(
        evaluator_id="judge.preamble",
        invoke=lambda prompt: (
            'I first considered {"candidate": "incomplete"}.\n{"score": 0.8, "passed": true, "explanation": "grounded"}'
        ),
        model_name="test-model",
    )

    result = judge.evaluate_output(input="question", output="answer")

    assert result.value == 0.8
    assert result.passed is True
    assert result.explanation == "grounded"


def test_llm_judge_uses_top_level_score_instead_of_nested_rubric_score() -> None:
    judge = LLMJudge(
        evaluator_id="judge.nested-rubric",
        invoke=lambda prompt: (
            '{"score": 0.9, "passed": true, "explanation": "overall", '
            '"rubric": {"score": 0.2, "passed": false, "explanation": "nested"}}'
        ),
        model_name="test-model",
    )

    result = judge.evaluate_output(input="question", output="answer")

    assert result.value == 0.9
    assert result.passed is True
    assert result.explanation == "overall"


def test_llm_judge_renders_placeholders_in_one_pass() -> None:
    prompts: list[str] = []
    judge = LLMJudge(
        evaluator_id="judge.safe-template",
        invoke=lambda prompt: prompts.append(prompt) or '{"score": 1, "passed": true, "explanation": "ok"}',
        model_name="test-model",
        prompt_template="Input={input}; Output={output}; Expected={expected}",
    )

    judge.evaluate_output(input="{output}", output="{expected}", expected="secret-answer")

    assert prompts == ["Input={output}; Output={expected}; Expected=secret-answer"]


def test_trial_span_emits_otel_eval_telemetry() -> None:
    exporter = _span_exporter()
    client = FakeClient(use_experimental_otel=True)
    suite = _suite()
    verifier = Evaluator(evaluator_id="exact", version="2", kind="deterministic")
    secret = "glc_abcdefghijklmnopqrstuvwxyz"

    with Experiment(client, experiment_id="run-2", name="telemetry run", suite=suite) as exp:
        with exp.trial(suite.test_cases[0]) as trial:
            trial.final_score(
                0.5,
                passed=False,
                explanation=f"off by one; credential {secret}",
                evaluator=verifier,
            )

    spans = exporter.get_finished_spans()
    assert len(spans) == 1
    span = spans[0]
    assert span.name == "eval.trial add"

    attrs = dict(span.attributes)
    # test.* identity, emitted directly (no sigil.* mirror).
    assert attrs[otel.TEST_SUITE_RUN_ID] == "run-2"
    assert attrs[otel.TEST_CASE_ID] == "add"
    assert attrs[otel.TEST_SUITE_VERSION] == "1.2.0"
    assert attrs[otel.TEST_SUITE_RUN_STATUS] == "in_progress"
    # Proposed per-attempt trial identity.
    assert attrs[otel.TEST_CASE_RUN_ATTEMPT] == 1
    assert attrs[otel.TEST_CASE_RUN_ID]  # the trial id
    # Verdict maps onto the merged pass|fail enum (this trial failed).
    assert attrs[otel.TEST_CASE_RESULT_STATUS] == "fail"
    # The schema-version marker is the one agento11y.* attribute we keep.
    assert attrs[otel.ATTR_SCHEMA_VERSION] == otel.SCHEMA_VERSION
    # No mirrors are emitted, in neither the agento11y.* nor the legacy sigil.* namespace.
    assert not any(str(k).startswith("agento11y.eval.experiment") for k in attrs)
    assert not any(str(k).startswith("sigil.") for k in attrs)

    # The eval result event uses OTel names; the verdict is the score label.
    events = [e for e in span.events if e.name == otel.EVENT_EVAL_RESULT]
    assert len(events) == 1
    eattrs = dict(events[0].attributes)
    assert eattrs[otel.EVAL_NAME] == "final"
    assert eattrs[otel.EVAL_SCORE_VALUE] == 0.5
    assert eattrs[otel.EVAL_SCORE_LABEL] == "fail"
    assert eattrs[otel.EVAL_EVALUATOR_ID] == "exact"
    assert eattrs[otel.EVAL_EVALUATOR_TYPE] == "deterministic"
    assert "agento11y.eval.score.passed" not in eattrs  # verdict is the label, no mirror
    assert secret not in eattrs[otel.EVAL_EXPLANATION]
    assert eattrs[otel.EVAL_EXPLANATION] == "off by one; credential [REDACTED:grafana-cloud-token]"


def test_trial_span_is_disabled_by_default() -> None:
    exporter = _span_exporter()
    client = FakeClient()
    suite = _suite()

    with Experiment(client, experiment_id="run-no-otel", name="no telemetry", suite=suite) as exp:
        with exp.trial(suite.test_cases[0]) as trial:
            trial.final_score(1.0, passed=True)

    assert not exporter.get_finished_spans()


def test_trial_from_ref_cross_process() -> None:
    client = FakeClient()
    ref = TrialRef(experiment_id="run-3", test_case_id="case-x", attempt=2, suite_id="s1")
    trial = Trial.from_ref(client, ref)
    with trial:
        trial.bind_conversation("conv-123")
        trial.final_score(True)
    assert len(client.scores) == 1
    s = client.scores[0]
    assert s.experiment_id == "run-3"
    assert s.test_case_id == "case-x"
    assert s.conversation_id == "conv-123"
    assert s.value.boolean is True
    assert s.passed is True  # boolean final score derives the verdict


def test_trial_from_ref_documented_close_creates_and_terminalizes_trial() -> None:
    client = FakeClient()
    trial = Trial.from_ref(client, TrialRef(experiment_id="run-worker", test_case_id="case-x"))

    trial.final_score(0.82, passed=True)
    trial.close()

    assert len(client.trials) == 1
    assert client.trials[0][1] == trial.trial_id
    assert client.scores[0].trial_id == trial.trial_id
    assert trial.accepted_scores == 1
    assert client.trial_updates[0][2] == "completed"


def test_experiment_finalize_closes_open_trial() -> None:
    client = FakeClient()
    with Experiment(client, experiment_id="run-open", name="open") as exp:
        trial = exp.trial("case-x")
        trial.final_score(1.0, passed=True)

    assert trial.accepted_scores == 1
    assert client.trial_updates[0][2] == "completed"
    assert client.finalized == [("run-open", "completed", None)]


def test_experiment_finalize_retries_trial_when_terminal_update_fails(monkeypatch) -> None:
    client = FakeClient()
    exp = Experiment(client, experiment_id="run-update-retry", auto_finalize=False)
    trial = exp.trial("case-x")
    trial.final_score(1.0, passed=True)
    update_calls = 0
    finalize_calls = 0
    original_update_trial = client.update_trial
    original_finalize = client.finalize

    def fail_first_update(*args, **kwargs):
        nonlocal update_calls
        update_calls += 1
        if update_calls == 1:
            raise RuntimeError("trial update failed")
        return original_update_trial(*args, **kwargs)

    def fail_first_finalize(*args, **kwargs):
        nonlocal finalize_calls
        finalize_calls += 1
        if finalize_calls == 1:
            raise RuntimeError("cannot finalize with running trials")
        return original_finalize(*args, **kwargs)

    monkeypatch.setattr(client, "update_trial", fail_first_update)
    monkeypatch.setattr(client, "finalize", fail_first_finalize)

    with pytest.raises(RuntimeError, match="trial update failed"):
        exp.finalize()

    assert not trial._closed
    assert trial.trial_id in exp._open_trials
    assert not exp._finalized

    exp.finalize()

    assert trial._closed
    assert trial.trial_id not in exp._open_trials
    assert exp._finalized
    assert update_calls == 2
    assert finalize_calls == 2
    assert len(client.scores) == 1
    assert client.finalized == [("run-update-retry", "completed", None)]


def test_experiment_finalize_closes_all_trials_and_marks_run_failed_on_close_error(
    monkeypatch,
) -> None:
    client = FakeClient()
    exp = Experiment(client, experiment_id="run-close-error", auto_finalize=False)
    first = exp.trial("case-a")
    second = exp.trial("case-b")
    close_calls: list[str] = []

    def fail_first() -> None:
        close_calls.append("first")
        raise RuntimeError("score export failed")

    def close_second() -> None:
        close_calls.append("second")

    monkeypatch.setattr(first, "close", fail_first)
    monkeypatch.setattr(second, "close", close_second)

    with pytest.raises(RuntimeError, match="score export failed"):
        exp.finalize()

    assert close_calls == ["first", "second"]
    assert client.finalized == [("run-close-error", "failed", None)]
    assert exp.status == "failed"


def test_experiment_exit_preserves_body_error_when_trial_close_also_fails(monkeypatch) -> None:
    client = FakeClient()

    def fail_close() -> None:
        raise RuntimeError("score export failed")

    with pytest.raises(ValueError, match="agent failed") as caught:
        with Experiment(client, experiment_id="run-body-error") as exp:
            trial = exp.trial("case-a")
            monkeypatch.setattr(trial, "close", fail_close)
            raise ValueError("agent failed")

    assert client.finalized == [("run-body-error", "failed", None)]
    if sys.version_info >= (3, 11):
        assert any("score export failed" in note for note in (caught.value.__notes__ or []))


def test_experiment_rejects_duplicate_case_attempt() -> None:
    client = FakeClient()
    with Experiment(client, experiment_id="run-duplicate", name="duplicate", auto_finalize=False) as exp:
        exp.trial("case-x", attempt=1)
        with pytest.raises(ValueError, match="increment attempt for a retry"):
            exp.trial("case-x", attempt=1)

        retry = exp.trial("case-x", attempt=2)

    assert retry.ref.attempt == 2


def test_record_io_without_scores_still_exports_generation() -> None:
    client = FakeClient()
    with Experiment(client, experiment_id="run-io", name="io") as exp:
        with exp.trial("case-x") as trial:
            trial.record_io(input="prompt", output="answer")

    assert client.generations == [trial.generation_id]
    assert client.trial_updates[0][3]["conversation_id"] == trial.conversation_id


def test_repeated_scores_and_grader_transcripts_have_distinct_ids() -> None:
    client = FakeClient()
    evaluator = Evaluator(evaluator_id="judge", version="1", kind="llm_judge")
    result = EvaluationResult(
        evaluator=evaluator,
        value=0.8,
        passed=True,
        grader=GraderGeneration(input="prompt", output="result", model_provider="test", model_name="judge"),
    )
    with Experiment(client, experiment_id="run-repeat", name="repeat") as exp:
        with exp.trial("case-x") as trial:
            first = trial.record_evaluation(result, score_key="quality")
            second = trial.record_evaluation(result, score_key="quality")
            trial.final_score(1.0, passed=True)

    assert first.score_id != second.score_id
    assert first.grader_generation_id != second.grader_generation_id
    assert first.grader_conversation_id != second.grader_conversation_id


def test_trial_ref_env_round_trip() -> None:
    ref = TrialRef(experiment_id="run-4", test_case_id="c1", attempt=3, suite_id="s", suite_version="2.0.0")
    env = ref.to_env()
    assert env["AGENTO11Y_EXPERIMENT_ID"] == "run-4"
    assert env["AGENTO11Y_ATTEMPT"] == "3"
    restored = TrialRef.from_env(env)
    assert restored is not None
    assert restored.experiment_id == "run-4"
    assert restored.test_case_id == "c1"
    assert restored.attempt == 3


def test_trial_ref_to_env_writes_both_spellings() -> None:
    """to_env writes every populated field under both names, matching Go's agento11y.TrialRef.ToEnv."""
    ref = TrialRef(
        experiment_id="run-5",
        test_case_id="c1",
        attempt=2,
        suite_id="s",
        suite_version="2.0.0",
        trajectory_id="traj-1",
    )
    env = ref.to_env()
    assert env == {
        "AGENTO11Y_EXPERIMENT_ID": "run-5",
        "SIGIL_EXPERIMENT_ID": "run-5",
        "AGENTO11Y_TEST_CASE_ID": "c1",
        "SIGIL_TEST_CASE_ID": "c1",
        "AGENTO11Y_ATTEMPT": "2",
        "SIGIL_ATTEMPT": "2",
        "AGENTO11Y_SUITE_ID": "s",
        "SIGIL_SUITE_ID": "s",
        "AGENTO11Y_SUITE_VERSION": "2.0.0",
        "SIGIL_SUITE_VERSION": "2.0.0",
        "AGENTO11Y_TRAJECTORY_ID": "traj-1",
        "SIGIL_TRAJECTORY_ID": "traj-1",
    }


def test_trial_ref_to_env_omits_empty_optional_fields() -> None:
    env = TrialRef(experiment_id="run-6", test_case_id="c1").to_env()
    assert set(env) == {
        "AGENTO11Y_EXPERIMENT_ID",
        "SIGIL_EXPERIMENT_ID",
        "AGENTO11Y_TEST_CASE_ID",
        "SIGIL_TEST_CASE_ID",
        "AGENTO11Y_ATTEMPT",
        "SIGIL_ATTEMPT",
    }


def test_trial_ref_from_env_reads_sigil_names_with_warning(caplog: pytest.LogCaptureFixture) -> None:
    env = {
        "SIGIL_EXPERIMENT_ID": "run-old",
        "SIGIL_TEST_CASE_ID": "c1",
        "SIGIL_ATTEMPT": "4",
        "SIGIL_SUITE_ID": "s",
        "SIGIL_SUITE_VERSION": "2.0.0",
        "SIGIL_TRAJECTORY_ID": "traj-1",
    }
    with caplog.at_level("WARNING", logger="agento11y"):
        ref = TrialRef.from_env(env)
    assert ref is not None
    assert ref.experiment_id == "run-old"
    assert ref.test_case_id == "c1"
    assert ref.attempt == 4
    assert ref.suite_id == "s"
    assert ref.suite_version == "2.0.0"
    assert ref.trajectory_id == "traj-1"
    messages = [record.getMessage() for record in caplog.records]
    assert any("SIGIL_EXPERIMENT_ID is deprecated; rename it to AGENTO11Y_EXPERIMENT_ID" in item for item in messages)
    assert any("SIGIL_TEST_CASE_ID is deprecated; rename it to AGENTO11Y_TEST_CASE_ID" in item for item in messages)


@pytest.mark.parametrize(
    "env,expected",
    [
        pytest.param(
            {"SIGIL_RUN_ID": "run-legacy", "SIGIL_TEST_CASE_ID": "c1"},
            ("run-legacy", "c1"),
            id="SIGIL_RUN_ID supplies the experiment id",
        ),
        pytest.param(
            {"SIGIL_EXPERIMENT_ID": "run-old", "SIGIL_RUN_ID": "run-older", "SIGIL_TEST_CASE_ID": "c1"},
            ("run-old", "c1"),
            id="SIGIL_EXPERIMENT_ID beats SIGIL_RUN_ID",
        ),
        pytest.param(
            {
                "AGENTO11Y_EXPERIMENT_ID": "run-new",
                "SIGIL_EXPERIMENT_ID": "run-old",
                "SIGIL_RUN_ID": "run-older",
                "AGENTO11Y_TEST_CASE_ID": "c-new",
                "SIGIL_TEST_CASE_ID": "c-old",
            },
            ("run-new", "c-new"),
            id="canonical wins over every legacy spelling",
        ),
        pytest.param(
            {"AGENTO11Y_RUN_ID": "run-x", "AGENTO11Y_TEST_CASE_ID": "c1"},
            None,
            id="AGENTO11Y_RUN_ID is not a name",
        ),
    ],
)
def test_trial_ref_from_env_alias_resolution(env: dict[str, str], expected: tuple[str, str] | None) -> None:
    ref = TrialRef.from_env(env)
    if expected is None:
        assert ref is None
        return
    assert ref is not None
    assert (ref.experiment_id, ref.test_case_id) == expected


def test_trial_from_ref_requires_ref() -> None:
    with pytest.raises(ValueError, match="trial ref is required"):
        Trial.from_ref(FakeClient(), None)


def test_no_synthetic_conversation_id_when_unbound() -> None:
    # Regression: a trial that never binds/records a real conversation must NOT
    # emit a conversation_id, or the experiments UI offers an "Open conversation"
    # link that 404s (no backing generation).
    client = FakeClient()
    suite = _suite()
    with Experiment(client, experiment_id="run-x", name="x", suite=suite) as exp:
        with exp.trial(suite.test_cases[0]) as trial:
            trial.final_score(1.0, passed=True)
        assert trial.conversation_id == ""
    assert all(s.conversation_id == "" for s in client.scores)
    assert client.generations == []  # nothing ingested, so nothing to link


def test_bind_trace_after_trial_creation_patches_span_id() -> None:
    client = FakeClient()
    with Experiment(client, experiment_id="run-trace", name="trace") as exp:
        with exp.trial("case-x") as trial:
            trial.bind_trace("a" * 32, "b" * 16)
            trial.final_score(True)

    assert any(
        update[3].get("trace_id") == "a" * 32 and update[3].get("span_id") == "b" * 16
        for update in client.trial_updates
    )


def test_record_io_mints_real_conversation() -> None:
    # When a real generation IS recorded, the trial gets a conversation id and the
    # scores carry it (the conversation is openable).
    client = FakeClient()
    suite = _suite()
    with Experiment(client, experiment_id="run-y", name="y", suite=suite) as exp:
        with exp.trial(suite.test_cases[0]) as trial:
            trial.record_io(input="2+2", output="4")
            trial.final_score(1.0, passed=True)
        conv = trial.conversation_id
    assert conv != ""
    assert client.generations == [trial.generation_id]
    assert all(s.conversation_id == conv for s in client.scores)


def test_recorded_generation_carries_candidate_agent_version() -> None:
    # Regression: the candidate's declared agent_version must reach the ingested
    # generation, or Agent Observability auto-derives a version from the prompt hash and version
    # comparison on the agent's Quality view is impossible.
    client = FakeClient()
    suite = _suite()
    candidate = {"agent_name": "adder", "agent_version": "v3"}
    with Experiment(client, experiment_id="run-v", name="v", suite=suite, candidate=candidate) as exp:
        with exp.trial(suite.test_cases[0]) as trial:
            trial.record_io(input="2+2", output="4")
            trial.final_score(1.0, passed=True)
    assert client.generation_calls, "expected a recorded generation"
    _, kwargs = client.generation_calls[0]
    assert kwargs.get("agent_version") == "v3"
    assert kwargs.get("agent_name") == "adder"


def test_record_io_agent_version_overrides_candidate() -> None:
    # A per-trial record_io(agent_version=...) takes precedence over the candidate's.
    client = FakeClient()
    suite = _suite()
    candidate = {"agent_name": "adder", "agent_version": "v3"}
    with Experiment(client, experiment_id="run-v2", name="v2", suite=suite, candidate=candidate) as exp:
        with exp.trial(suite.test_cases[0]) as trial:
            trial.record_io(input="2+2", output="4", agent_version="v4-local")
            trial.final_score(1.0, passed=True)
    _, kwargs = client.generation_calls[0]
    assert kwargs.get("agent_version") == "v4-local"


def test_trial_without_final_score_fails() -> None:
    client = FakeClient()
    suite = _suite()
    with Experiment(client, experiment_id="run-no-final", name="x", suite=suite) as exp:
        with exp.trial(suite.test_cases[0]) as trial:
            trial.check_score("json_valid", passed=True)
    assert trial.status == "failed"
    assert trial.error == "trial closed without a final score"
    assert client.trial_updates and client.trial_updates[0][2] == "completed"


def test_trial_cloud_evaluation_persists_binding_and_allows_exit_without_local_score() -> None:
    client = FakeClient()
    suite = _suite()

    with Experiment(client, experiment_id="run-cloud", name="cloud", suite=suite) as exp:
        with exp.trial(suite.test_cases[0]) as trial:
            trial.bind_conversation("conv-cloud")
            client.evaluation_result = _evaluation(
                TrialEvaluationStatus.QUEUED,
                evaluation_id="eval-cloud",
                experiment_id=exp.experiment_id,
                trial_id=trial.trial_id,
            )
            client.evaluation_statuses = [
                _evaluation(
                    TrialEvaluationStatus.SUCCESS,
                    evaluation_id="eval-cloud",
                    experiment_id=exp.experiment_id,
                    trial_id=trial.trial_id,
                )
            ]
            evaluation = trial.evaluate("helpfulness", "v3", timeout=1, poll_interval=0.001)

    assert evaluation.status == TrialEvaluationStatus.SUCCESS
    assert trial.status == "completed"
    assert trial.accepted_scores == 0
    assert client.trial_updates[0][3]["conversation_id"] == "conv-cloud"
    # Nothing to export for a caller-bound conversation, so the wait starts right
    # after the binding is persisted.
    assert client.evaluation_order[:4] == ["update", "flush", "trigger", "status"]
    assert client.triggered_evaluations == [("run-cloud", trial.trial_id, "helpfulness", "v3")]
    # The stored trial closes as completed with no error, and finalization omits a
    # local score count so the backend counts the evaluator's scores.
    assert client.trial_updates[-1][2] == "completed"
    assert client.trial_updates[-1][3]["error"] == ""
    assert client.finalized == [("run-cloud", "completed", None)]


def test_trial_cloud_evaluation_exports_recorded_io_before_queueing() -> None:
    client = FakeClient()
    suite = _suite()

    with Experiment(client, experiment_id="run-cloud-io", name="cloud", suite=suite) as exp:
        with exp.trial(suite.test_cases[0]) as trial:
            trial.record_io(input="2+2", output="4")
            client.evaluation_result = _evaluation(
                TrialEvaluationStatus.SUCCESS,
                evaluation_id="eval-io",
                experiment_id=exp.experiment_id,
                trial_id=trial.trial_id,
            )
            trial.evaluate("helpfulness")

    # record_io mints the conversation and buffers the anchor generation. The
    # worker reads that conversation, so the export has to precede the trigger
    # rather than wait for trial close.
    assert client.evaluation_order[:4] == ["update", "generation", "flush", "trigger"]
    assert client.generation_calls[0][1]["conversation_id"] == trial.conversation_id
    assert client.trial_updates[0][3]["conversation_id"] == trial.conversation_id
    assert client.generations == [trial.generation_id]


@pytest.mark.parametrize(
    "mark_terminal,expected_status,expected_error",
    [
        pytest.param(True, "passed", "", id="runner marks the trial"),
        pytest.param(False, "failed", "trial closed without a final score", id="runner leaves it running"),
    ],
)
def test_client_level_evaluation_queues_now_and_polls_later(
    mark_terminal: bool,
    expected_status: str,
    expected_error: str,
) -> None:
    """Queue the evaluation through the client, then poll after the trial closes.

    Trial.evaluate() blocks; a runner that does not want to wait per trial can
    call the same two client methods itself. That path skips the bookkeeping
    evaluate() does, so the conversation binding is patched by hand and the
    trial has no cloud-evaluated flag to close on.
    """

    client = FakeClient()
    suite = _suite()
    pending: list[tuple[str, str]] = []

    with Experiment(client, experiment_id="run-cloud-async", name="cloud", suite=suite) as exp:
        with exp.trial(suite.test_cases[0]) as trial:
            trial.bind_conversation("conv-async")
            exp.client.update_trial(exp.experiment_id, trial.trial_id, conversation_id="conv-async")
            client.evaluation_result = _evaluation(
                TrialEvaluationStatus.QUEUED,
                evaluation_id="eval-async",
                experiment_id=exp.experiment_id,
                trial_id=trial.trial_id,
            )
            queued = exp.client.trigger_trial_evaluation(exp.experiment_id, trial.trial_id, "helpfulness", "v3")
            pending.append((trial.trial_id, queued.evaluation_id))
            if mark_terminal:
                trial.succeed()

        client.evaluation_statuses = [
            _evaluation(TrialEvaluationStatus.QUEUED, evaluation_id="eval-async", trial_id=trial.trial_id),
            _evaluation(TrialEvaluationStatus.SUCCESS, evaluation_id="eval-async", trial_id=trial.trial_id),
        ]
        for trial_id, evaluation_id in pending:
            evaluation = exp.client.get_trial_evaluation(exp.experiment_id, trial_id, evaluation_id)
            while not evaluation.status.terminal:
                evaluation = exp.client.get_trial_evaluation(exp.experiment_id, trial_id, evaluation_id)

    assert queued.status == TrialEvaluationStatus.QUEUED
    assert evaluation.status == TrialEvaluationStatus.SUCCESS
    assert client.triggered_evaluations == [("run-cloud-async", trial.trial_id, "helpfulness", "v3")]
    assert client.evaluation_order.count("status") == 2
    assert trial.status == expected_status
    assert trial.error == expected_error
    # No local score either way, and the run finalizes without a score count so
    # the backend counts what the stored evaluator wrote.
    assert trial.accepted_scores == 0
    assert client.finalized == [("run-cloud-async", "completed", None)]


@pytest.mark.parametrize("error_cls", [EvaluationExecutionError, EvaluationTimeoutError])
def test_cloud_evaluation_errors_survive_pickle(error_cls: type[Exception]) -> None:
    original = error_cls("eval-42", "judge crashed")

    restored = pickle.loads(pickle.dumps(original))

    assert type(restored) is error_cls
    assert restored.evaluation_id == "eval-42"
    assert restored.detail == "judge crashed"
    assert str(restored) == str(original)


def test_trial_cloud_evaluation_surfaces_worker_failure() -> None:
    client = FakeClient()
    suite = _suite()

    with pytest.raises(EvaluationExecutionError, match="budget exhausted") as exc_info:
        with Experiment(client, experiment_id="run-cloud-fail", name="cloud", suite=suite) as exp:
            with exp.trial(suite.test_cases[0]) as trial:
                trial.bind_conversation("conv-cloud")
                client.evaluation_result = _evaluation(
                    TrialEvaluationStatus.FAILED,
                    evaluation_id="eval-failed",
                    experiment_id=exp.experiment_id,
                    trial_id=trial.trial_id,
                    error="budget exhausted",
                )
                trial.evaluate("helpfulness")

    assert exc_info.value.evaluation_id == "eval-failed"
    assert exc_info.value.detail == "budget exhausted"


def test_trial_exception_after_cloud_evaluation_still_errors() -> None:
    client = FakeClient()
    suite = _suite()

    with pytest.raises(RuntimeError, match="verifier crashed"):
        with Experiment(client, experiment_id="run-cloud-raise", name="cloud", suite=suite) as exp:
            with exp.trial(suite.test_cases[0]) as trial:
                trial.bind_conversation("conv-cloud")
                client.evaluation_result = _evaluation(
                    TrialEvaluationStatus.SUCCESS,
                    evaluation_id="eval-cloud",
                    experiment_id=exp.experiment_id,
                    trial_id=trial.trial_id,
                )
                trial.evaluate("helpfulness")
                raise RuntimeError("verifier crashed")

    # A cloud-graded trial must not swallow a later failure in the trial block.
    assert trial.status == "errored"
    assert trial.error == "verifier crashed"
    assert client.trial_updates[-1][2] == "failed"
    assert client.trial_updates[-1][3]["error"] == "verifier crashed"


def test_trial_cloud_evaluation_defaults() -> None:
    import inspect

    signature = inspect.signature(Trial.evaluate)
    assert signature.parameters["evaluator_version"].default == ""
    assert signature.parameters["timeout"].default == 300.0
    assert signature.parameters["poll_interval"].default == 0.5


@pytest.mark.parametrize(
    "conversation_id,evaluator_id,timeout,poll_interval,error",
    [
        pytest.param("", "helpfulness", 300.0, 0.5, "bind a conversation first", id="missing conversation"),
        pytest.param("conv-1", "", 300.0, 0.5, "evaluator_id is required", id="missing evaluator"),
        pytest.param("conv-1", "helpfulness", 0.0, 0.5, "timeout must be greater", id="zero timeout"),
        pytest.param("conv-1", "helpfulness", float("inf"), 0.5, "timeout must be greater", id="infinite timeout"),
        pytest.param("conv-1", "helpfulness", 300.0, 0.0, "poll_interval must be greater", id="zero poll"),
    ],
)
def test_trial_cloud_evaluation_validates_wait_options(
    conversation_id: str,
    evaluator_id: str,
    timeout: float,
    poll_interval: float,
    error: str,
) -> None:
    client = FakeClient()
    trial = Trial.from_ref(client, TrialRef(experiment_id="run-validation", test_case_id="case-validation"))
    trial.bind_conversation(conversation_id)

    with pytest.raises(ValidationError, match=error):
        trial.evaluate(evaluator_id, timeout=timeout, poll_interval=poll_interval)

    assert client.trials == []
    assert client.triggered_evaluations == []


def test_trial_cloud_evaluation_uses_monotonic_timeout(monkeypatch: pytest.MonkeyPatch) -> None:
    import importlib

    experiment_module = importlib.import_module("agento11y.experiments.experiment")
    client = FakeClient()
    trial = Trial.from_ref(client, TrialRef(experiment_id="run-timeout", test_case_id="case-timeout"))
    trial.bind_conversation("conv-timeout")
    client.evaluation_result = _evaluation(
        TrialEvaluationStatus.QUEUED,
        evaluation_id="eval-timeout",
        experiment_id="run-timeout",
        trial_id=trial.trial_id,
    )
    monotonic_values = iter((10.0, 311.0))
    monkeypatch.setattr(experiment_module.time, "monotonic", lambda: next(monotonic_values))

    with pytest.raises(EvaluationTimeoutError, match="timed out after 300 seconds") as exc_info:
        trial.evaluate("helpfulness")

    assert exc_info.value.evaluation_id == "eval-timeout"
    assert client.evaluation_order.count("status") == 0


def test_trial_cloud_evaluation_backs_off_between_polls(monkeypatch: pytest.MonkeyPatch) -> None:
    import importlib

    experiment_module = importlib.import_module("agento11y.experiments.experiment")
    clock = {"now": 0.0}
    sleeps: list[float] = []

    def fake_sleep(seconds: float) -> None:
        sleeps.append(seconds)
        clock["now"] += seconds

    monkeypatch.setattr(experiment_module.time, "monotonic", lambda: clock["now"])
    monkeypatch.setattr(experiment_module.time, "sleep", fake_sleep)

    client = FakeClient()
    trial = Trial.from_ref(client, TrialRef(experiment_id="run-backoff", test_case_id="case-backoff"))
    trial.bind_conversation("conv-backoff")
    client.evaluation_result = _evaluation(TrialEvaluationStatus.QUEUED, evaluation_id="eval-backoff")

    with pytest.raises(EvaluationTimeoutError):
        trial.evaluate("helpfulness")

    polls = client.evaluation_order.count("status")
    assert sleeps[:5] == [0.5, 1.0, 2.0, 4.0, 5.0]
    assert max(sleeps) == 5.0
    # The last sleep is trimmed to the remaining time instead of overshooting the deadline.
    assert sleeps[-1] < 5.0
    assert sum(sleeps) == pytest.approx(300.0)
    # A fixed 0.5s interval would read status ~600 times over the same wait.
    assert polls == len(sleeps)
    assert polls < 100


def test_cloud_evaluation_surface_is_public() -> None:
    import agento11y
    from agento11y import experiments as experiments_module

    for name in (
        "TrialEvaluation",
        "TrialEvaluationStatus",
        "EvaluationExecutionError",
        "EvaluationTimeoutError",
        "ExperimentalFeatureDisabledError",
    ):
        assert name in experiments_module.__all__
        assert hasattr(experiments_module, name)
        assert name in agento11y.__all__
        assert hasattr(agento11y, name)
    # Pre-existing experiments exports stay available.
    for name in ("Experiment", "Trial", "TrialRef", "TestSuite", "TestCase", "Evaluator"):
        assert name in experiments_module.__all__


def test_cloud_evaluation_is_blocked_without_the_experimental_gate(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """The default a caller sees: no opt-in set, so nothing is sent."""

    from agento11y.errors import ExperimentalFeatureDisabledError
    from agento11y.experimental import ENV_ENABLE_EXPERIMENTAL_FEATURES

    client = FakeClient()
    suite = _suite()

    with Experiment(client, experiment_id="run-gated", name="gated", suite=suite) as exp:
        with exp.trial(suite.test_cases[0]) as trial:
            trial.bind_conversation("conv-gated")
            # Cleared rather than set to a falsy value so this exercises the
            # default, not a specific opt-out spelling.
            monkeypatch.delenv(ENV_ENABLE_EXPERIMENTAL_FEATURES, raising=False)
            with pytest.raises(ExperimentalFeatureDisabledError) as blocked:
                trial.evaluate("helpfulness")
            trial.final_score(1.0, passed=True)

    message = str(blocked.value)
    assert "cloud trial evaluation" in message
    assert ENV_ENABLE_EXPERIMENTAL_FEATURES in message
    # evaluate() persists the binding and flushes the anchor generation before it
    # triggers anything. A blocked call must reach neither step. The trial's own
    # close still writes an update and a flush, so only the evaluation steps are
    # asserted here.
    assert client.triggered_evaluations == []
    assert "trigger" not in client.evaluation_order
    assert "status" not in client.evaluation_order


def test_experimental_gate_reads_truthy_values(monkeypatch: pytest.MonkeyPatch) -> None:
    from agento11y.experimental import (
        ENV_ENABLE_EXPERIMENTAL_FEATURES,
        experimental_features_enabled,
    )

    for raw, expected in (
        ("1", True),
        ("true", True),
        ("TRUE", True),
        (" true ", True),
        ("yes", True),
        ("on", True),
        ("", False),
        ("0", False),
        ("false", False),
        ("no", False),
        ("off", False),
        ("maybe", False),
    ):
        monkeypatch.setenv(ENV_ENABLE_EXPERIMENTAL_FEATURES, raw)
        assert experimental_features_enabled() is expected, raw

    monkeypatch.delenv(ENV_ENABLE_EXPERIMENTAL_FEATURES, raising=False)
    assert experimental_features_enabled() is False

    # The name postdates the rename, so no release ever read SIGIL_ for it.
    monkeypatch.setenv("SIGIL_ENABLE_EXPERIMENTAL_FEATURES", "true")
    assert experimental_features_enabled() is False


def test_experimental_feature_disabled_error_survives_pickle() -> None:
    from agento11y.errors import ExperimentalFeatureDisabledError

    original = ExperimentalFeatureDisabledError("cloud trial evaluation", "AGENTO11Y_X")
    restored = pickle.loads(pickle.dumps(original))
    assert restored.feature == original.feature
    assert restored.env_var == original.env_var
    assert str(restored) == str(original)


def test_trial_cleanup_runs_when_flush_fails() -> None:
    client = FailingExportClient()
    suite = _suite()
    with pytest.raises(RuntimeError, match="score export failed"):
        with Experiment(client, experiment_id="run-flush-fail", name="x", suite=suite) as exp:
            with exp.trial(suite.test_cases[0]) as trial:
                trial.final_score(1.0, passed=True)
    assert client.trial_updates and client.trial_updates[0][2] == "completed"
    assert len(trial._buffer) == 1
    assert client.calls[:2] == ["flush_generations", "export_scores"]


def test_trial_flush_flushes_generation_client_before_scores() -> None:
    client = FakeClient()
    suite = _suite()
    with Experiment(client, experiment_id="run-flush-order", name="x", suite=suite) as exp:
        with exp.trial(suite.test_cases[0]) as trial:
            trial.bind_generation("gen-existing", conversation_id="conv-existing")
            trial.final_score(1.0, passed=True)
    assert client.calls[:2] == ["flush_generations", "export_scores"]


def test_score_value_helpers() -> None:
    assert score.number(0.5).number == 0.5
    assert score.boolean(True).boolean is True
    assert score.string("x").string == "x"


def test_suite_from_dict_and_cases_alias() -> None:
    suite = TestSuite.from_dict(
        {
            "id": "regression",
            "name": "Dashboard regression",
            "version": "2.0.0",
            "cases": [
                {"id": "panels", "input": "add panels", "expected": "ok", "tags": ["dash"]},
                {"test_case_id": "annot", "name": "Annotation"},
            ],
        }
    )
    assert suite.suite_id == "regression"
    assert suite.version == "2.0.0"
    assert [c.test_case_id for c in suite.cases] == ["panels", "annot"]
    assert suite.cases is suite.test_cases
    assert suite.case("panels").tags == ["dash"]


def test_suite_from_yaml(tmp_path) -> None:
    path = tmp_path / "suite.yaml"
    path.write_text(
        "suite_id: y\nname: From YAML\ncases:\n  - id: a\n    input: hi\n  - id: b\n",
        encoding="utf-8",
    )
    suite = TestSuite.from_yaml(str(path))
    assert suite.suite_id == "y"
    assert [c.test_case_id for c in suite.cases] == ["a", "b"]


def test_suite_yaml_round_trip(tmp_path) -> None:
    suite = TestSuite(
        suite_id="round-trip",
        name="Round Trip",
        version="v2",
        description="portable dataset",
        tags=["smoke"],
        changelog="publish v2",
        test_cases=[
            TestCase(
                test_case_id="case-1",
                name="Case 1",
                input={"prompt": "hello"},
                expected={"answer": "hi"},
                metadata={"source": "unit"},
            )
        ],
    )
    path = tmp_path / "round-trip.yaml"
    text = suite.to_yaml(str(path))
    restored = TestSuite.from_yaml(str(path))
    assert "suite_id: round-trip" in text
    assert restored.to_dict() == suite.to_dict()


def test_candidate_dict_coercion_into_experiment() -> None:
    client = FakeClient()
    suite = _suite()
    with Experiment(
        client,
        experiment_id="run-cand",
        name="cand",
        suite=suite,
        candidate={
            "agent_name": "agent-a",
            "agent_version": "1.2.3",
            "git_sha": "abc123",
            "model_name": "gpt-5",
            "unknown": "ignored",
        },
    ) as exp:
        with exp.trial(suite.test_cases[0]) as trial:
            trial.score("final", value=0.8, passed=True, explanation="good")
    md = client.upserts[0].metadata
    assert client.upserts[0].candidate == {
        "agent_name": "agent-a",
        "agent_version": "1.2.3",
        "model_name": "gpt-5",
        "git_sha": "abc123",
    }
    assert "unknown" not in client.upserts[0].candidate
    assert md["git_sha"] == "abc123"
    assert md["model_name"] == "gpt-5"
    final = next(s for s in client.scores if s.score_key == "final")
    assert final.passed is True
    assert final.value.number == 0.8


def test_trial_artifact_upload() -> None:
    client = FakeClient()
    suite = _suite()
    with Experiment(client, experiment_id="run-art", name="art", suite=suite) as exp:
        with exp.trial(suite.test_cases[0]) as trial:
            trial.final_score(1.0, passed=True)
            rec_json = trial.artifact("grading-details", data={"passed": True}, kind="json")
            trial.artifact("notes", text="looks good")
    assert rec_json["artifact_id"] == "art_grading-details"
    names = {a[2] for a in client.artifacts}
    assert names == {"grading-details", "notes"}
    json_upload = next(a for a in client.artifacts if a[2] == "grading-details")
    assert json_upload[0] == "run-art"
    assert json_upload[3] == "json" and json_upload[4] == "application/json"
    assert trial.artifacts[0]["artifact_id"] == "art_grading-details"


def test_experiment_factory_uses_supplied_client() -> None:
    client = FakeClient()
    suite = _suite()
    from agento11y.experiments import experiment as experiment_factory

    with experiment_factory(
        "factory run",
        suite=suite,
        client=client,
        experiment_id="run-f",
        planned_trial_count=1,
    ) as exp:
        assert exp._owns_client is False
        with exp.trial(suite.test_cases[0]) as trial:
            trial.score("final", value=True)
    assert client.upserts[0].planned_trial_count == 1
    assert client.finalized and client.finalized[0][0] == "run-f"


@pytest.mark.parametrize(
    "env",
    [
        pytest.param(
            {
                "AGENTO11Y_ENDPOINT": "https://agento11y",
                "AGENTO11Y_AUTH_TENANT_ID": "1",
                "AGENTO11Y_AUTH_TOKEN": "token",
                "AGENTO11Y_INGEST_ACTOR": "ingest:test",
            },
            id="canonical",
        ),
        pytest.param(
            {
                "SIGIL_ENDPOINT": "https://agento11y",
                "SIGIL_AUTH_TENANT_ID": "1",
                "SIGIL_AUTH_TOKEN": "token",
                "SIGIL_INGEST_ACTOR": "ingest:test",
            },
            id="legacy",
        ),
        pytest.param(
            {
                "SIGIL_API_ENDPOINT": "https://agento11y",
                "SIGIL_TENANT_ID": "1",
                "SIGIL_AUTH_TOKEN": "token",
                "SIGIL_INGEST_ACTOR": "ingest:test",
            },
            id="legacy secondary spellings",
        ),
        pytest.param(
            {
                "AGENTO11Y_ENDPOINT": "https://agento11y",
                "SIGIL_ENDPOINT": "https://legacy",
                "AGENTO11Y_AUTH_TENANT_ID": "1",
                "SIGIL_AUTH_TENANT_ID": "2",
                "AGENTO11Y_AUTH_TOKEN": "token",
                "SIGIL_AUTH_TOKEN": "legacy-token",
                "AGENTO11Y_INGEST_ACTOR": "ingest:test",
                "SIGIL_INGEST_ACTOR": "ingest:legacy",
            },
            id="canonical wins over legacy",
        ),
    ],
)
def test_experiment_factory_uses_connection_env(env: dict[str, str], monkeypatch: pytest.MonkeyPatch) -> None:
    from agento11y.experiments import experiment as experiment_factory

    for key, value in env.items():
        monkeypatch.setenv(key, value)
    client = experiment_factory("conn").client
    assert (client.endpoint, client.tenant_id, client.ingest_token, client.actor) == (
        "https://agento11y",
        "1",
        "token",
        "ingest:test",
    )


def test_experiment_factory_ignores_agento11y_api_endpoint(monkeypatch: pytest.MonkeyPatch) -> None:
    from agento11y.experiments import experiment as experiment_factory

    monkeypatch.setenv("AGENTO11Y_API_ENDPOINT", "https://nope")
    monkeypatch.setenv("AGENTO11Y_AUTH_TOKEN", "tok")
    with pytest.raises(ValueError, match="endpoint is required"):
        experiment_factory("conn")


@pytest.mark.parametrize(
    "env,expected_otel",
    [
        pytest.param(
            {
                "AGENTO11Y_AUTH_TOKEN": "token",
                "AGENTO11Y_GRAFANA_URL": "https://g.example/",
                "AGENTO11Y_USE_EXPERIMENTAL_OTEL": "false",
            },
            False,
            id="canonical",
        ),
        pytest.param(
            {
                "SIGIL_AUTH_TOKEN": "token",
                "SIGIL_GRAFANA_URL": "https://g.example/",
                "SIGIL_USE_EXPERIMENTAL_OTEL": "true",
            },
            True,
            id="legacy",
        ),
        pytest.param(
            {
                "AGENTO11Y_AUTH_TOKEN": "token",
                "SIGIL_AUTH_TOKEN": "legacy-token",
                "AGENTO11Y_GRAFANA_URL": "https://g.example/",
                "SIGIL_GRAFANA_URL": "https://legacy.example",
                "AGENTO11Y_USE_EXPERIMENTAL_OTEL": "false",
                "SIGIL_USE_EXPERIMENTAL_OTEL": "true",
            },
            False,
            id="canonical wins over legacy",
        ),
    ],
)
def test_experiments_client_uses_env(env: dict[str, str], expected_otel: bool, monkeypatch: pytest.MonkeyPatch) -> None:
    from agento11y.experiments.client import Client as IngestClient

    for key, value in env.items():
        monkeypatch.setenv(key, value)
    client = IngestClient("https://sigil.example")
    assert client.ingest_token == "token"
    assert client.grafana_url == "https://g.example"
    assert client.use_experimental_otel is expected_otel


def test_final_score_primitive_derives_boolean_verdict() -> None:
    client = FakeClient()
    suite = _suite()

    with Experiment(client, experiment_id="run-primitive-final", name="x", suite=suite) as exp:
        with exp.trial(suite.test_cases[0]) as trial:
            final = trial.score("final", value=True)

    assert final.passed is True
    assert trial.status == "passed"
    assert client.scores[0].passed is True


def test_artifact_content_kind_inference() -> None:
    from agento11y.experiments.experiment import _artifact_content, _kind_from_mime

    assert _kind_from_mime("image/png") == "image"
    assert _kind_from_mime("application/pdf") == "pdf"
    assert _kind_from_mime("text/markdown") == "markdown"
    assert _kind_from_mime("application/octet-stream") == "binary"
    content, kind, mime = _artifact_content("", {"a": 1}, "", "", "")
    assert kind == "json" and mime == "application/json" and b'"a"' in content


def test_trial_result_status_only_pass_fail() -> None:
    # test.case.result.status is the merged pass|fail enum; non-verdict states omit it.
    assert otel.trial_status_telemetry("passed") == "pass"
    assert otel.trial_status_telemetry("failed") == "fail"
    for state in ("running", "completed", "errored", "skipped", ""):
        assert otel.trial_status_telemetry(state) == ""


def test_numeric_final_without_verdict_is_neutral() -> None:
    client = FakeClient()
    with Experiment(client, experiment_id="run-neutral", name="neutral") as exp:
        with exp.trial("case-x") as trial:
            item = trial.final_score(0.7)

    assert item.passed is None
    assert trial.status == "completed"
    assert otel.trial_status_telemetry(trial.status) == ""


def test_non_verdict_states_omit_result_status() -> None:
    # completed/errored/skipped/running carry no pass|fail verdict, so the attribute
    # is omitted. A cloud-evaluated trial closes as `completed` with no local verdict.
    for state in ("completed", "errored", "skipped", "running"):
        attrs = otel.trial_identity_attributes(
            experiment_id="run-e", test_case_id="c1", trial_id="t1", trial_status=state
        )
        assert otel.TEST_CASE_RESULT_STATUS not in attrs
    # a passed trial does carry it.
    passed = otel.trial_identity_attributes(experiment_id="r", test_case_id="c", trial_status="passed")
    assert passed[otel.TEST_CASE_RESULT_STATUS] == "pass"


def test_parse_report_matches_backend_shape() -> None:
    # Regression: the report keys the run under `experiment` and cost under
    # `total_cost` (older drafts used `run` / `total_cost_usd`).
    from agento11y._experiments_transport import _parse_report

    payload = {
        "experiment": {
            "experiment_id": "run-9",
            "name": "nightly",
            "status": "completed",
            "suite_id": "suite-9",
            "suite_version": "v4",
            "candidate": {"agent_name": "agent-a"},
            "planned_trial_count": 6,
            "result_status": "ready",
            "result": {
                "trial_count": 6,
                "pass_count": 3,
                "pass_denominator": 6,
                "final_score_sum": 4.26,
                "final_score_count": 6,
                "token_coverage": "complete",
                "cost_coverage": "partial",
            },
        },
        "summary": {
            "test_case_count": 2,
            "trial_count": 6,
            "completed_count": 6,
            "pass_rate": 0.5,
            "pass_at_k": {"1": 0.5, "3": 0.83},
            "pass_power_k": {"1": 0.5},
            "final_score_avg": 0.71,
            "total_cost": 0.0123,
            "total_tokens": 4096,
            "pass_count": 3,
            "pass_denominator": 6,
            "final_score_sum": 4.26,
            "final_score_count": 6,
            "token_coverage": "complete",
            "cost_coverage": "partial",
        },
        "rows": [{"test_case_id": "add", "trials": []}],
    }
    report = _parse_report(payload)
    assert report.run.run_id == "run-9"  # not blank (parsed from the `experiment` key)
    assert report.run.name == "nightly"
    assert report.run.suite_id == "suite-9"
    assert report.run.suite_version == "v4"
    assert report.run.candidate == {"agent_name": "agent-a"}
    assert report.run.planned_trial_count == 6
    assert report.run.result_status == "ready"
    assert report.run.result is not None
    assert report.run.result.pass_denominator == 6
    assert report.run.result.final_score_count == 6
    assert report.summary.total_cost == 0.0123  # not dropped
    assert report.summary.total_tokens == 4096
    assert report.summary.pass_count == 3
    assert report.summary.pass_denominator == 6
    assert report.summary.final_score_sum == 4.26
    assert report.summary.final_score_count == 6
    assert report.summary.token_coverage == "complete"
    assert report.summary.cost_coverage == "partial"
    assert report.summary.pass_at_k == {"1": 0.5, "3": 0.83}
    assert report.summary.pass_power_k == {"1": 0.5}
    assert report.summary.final_score_avg == 0.71
    assert report.rows and report.rows[0]["test_case_id"] == "add"


def test_report_preserves_missing_nullable_aggregates() -> None:
    from agento11y._experiments_transport import _parse_report

    report = _parse_report(
        {
            "experiment": {"experiment_id": "run-empty", "name": "empty", "status": "running"},
            "summary": {"trial_count": 0},
        }
    )

    assert report.summary.pass_rate is None
    assert report.summary.final_score_avg is None
    assert report.summary.total_cost is None
    assert report.summary.total_tokens is None


def test_example_json_fallbacks_handle_malformed_slices(monkeypatch: pytest.MonkeyPatch) -> None:
    example_root = Path(__file__).resolve().parents[2] / "examples" / "experiments" / "python"
    fake_anthropic = ModuleType("anthropic")
    fake_anthropic.Anthropic = object
    monkeypatch.setitem(sys.modules, "anthropic", fake_anthropic)
    sys.path.insert(0, str(example_root))
    try:
        from app.agent import _parse_grade
        from app.dashboard_agent import _parse_json
    finally:
        sys.path.pop(0)

    assert _parse_grade("prefix {not json} suffix") == {
        "score": 0.0,
        "passed": False,
        "explanation": "graded by LLM judge",
    }
    assert _parse_json("prefix {not json} suffix") == {}
