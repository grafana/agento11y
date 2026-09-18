from __future__ import annotations

from copy import deepcopy
from dataclasses import dataclass
from typing import Any

import pytest
from agento11y_deepeval import publish_deepeval_results, run_deepeval
from deepeval.evaluate.types import EvaluationResult as NativeEvaluationResult
from deepeval.evaluate.types import TestResult as NativeTestResult
from deepeval.test_case import ToolCall
from deepeval.test_run.api import MetricData, TurnApi


@dataclass
class Metric:
    name: str
    score: float | None
    success: bool | None
    reason: str = ""
    threshold: float | None = None
    evaluation_model: str | None = None
    error: str | None = None


@dataclass
class Result:
    name: str
    input: str
    actual_output: str
    expected_output: str
    metrics_data: list[Metric]
    success: bool = True
    metadata: dict[str, Any] | None = None
    context: list[str] | None = None
    retrieval_context: list[str] | None = None
    conversational: bool = False
    turns: list[Any] | None = None
    error: str | None = None


@dataclass
class Evaluation:
    test_results: list[Result]
    test_run_id: str = "deepeval-run-1"


class FakeClient:
    use_experimental_otel = False
    redact_secrets = False

    def __init__(self) -> None:
        self.experiments: list[Any] = []
        self.trials: list[dict[str, Any]] = []
        self.trial_updates: list[dict[str, Any]] = []
        self.scores: list[Any] = []
        self.generations: list[dict[str, Any]] = []
        self.finalized: list[tuple[str, str]] = []

    def upsert_experiment(self, request: Any) -> Any:
        self.experiments.append(request)
        return request

    def upsert_trial(self, experiment_id: str, **request: Any) -> dict[str, Any]:
        self.trials.append({"experiment_id": experiment_id, **request})
        return request

    def update_trial(self, experiment_id: str, trial_id: str, **request: Any) -> dict[str, Any]:
        self.trial_updates.append({"experiment_id": experiment_id, "trial_id": trial_id, **request})
        return request

    def export_scores(self, scores: list[Any]) -> int:
        self.scores.extend(scores)
        return len(scores)

    def export_generation(self, **request: Any) -> str:
        self.generations.append(request)
        return str(request["generation_id"])

    def record_generation(self, generation_id: str, **request: Any) -> str:
        self.generations.append({"generation_id": generation_id, **request})
        return generation_id

    def finalize(self, experiment_id: str, status: str, **_: Any) -> None:
        self.finalized.append((experiment_id, status))

    def experiment_url(self, experiment_id: str) -> str:
        return f"http://ui/{experiment_id}"


def test_live_runner_registers_before_target_and_retains_siblings():
    from deepeval.metrics import ExactMatchMetric
    from deepeval.test_case import LLMTestCase
    from opentelemetry import trace
    from opentelemetry.sdk.trace import TracerProvider

    client = FakeClient()
    provider = TracerProvider()
    # Use the existing global provider if another SDK test installed one.
    if isinstance(trace.get_tracer_provider(), trace.ProxyTracerProvider):
        trace.set_tracer_provider(provider)
    cases = [LLMTestCase(input=value, actual_output="", expected_output="ok") for value in ("error", "pass", "fail")]

    def target(case, trial):
        assert client.experiments[0].planned_trial_count == 3
        assert len(client.trials) == len(client.trial_updates) + 1
        assert trace.get_current_span().get_span_context().is_valid
        if case.input == "error":
            raise RuntimeError("provider unavailable")
        return "ok" if case.input == "pass" else "wrong"

    result = run_deepeval(cases, target, [ExactMatchMetric()], experiment_name="live", client=client)
    assert result.trial_count == 3
    assert [item.passed for item in client.scores] == [True, False]
    assert "provider unavailable" in client.trial_updates[0]["error"]
    assert all(case.actual_output == "" for case in cases)


def test_publishes_primary_and_diagnostic_metrics() -> None:
    client = FakeClient()
    evaluation = Evaluation(
        test_results=[
            Result(
                name="capital-fr",
                input="Capital of France?",
                actual_output="Paris",
                expected_output="Paris",
                metadata={"agento11y.test_case_id": "capital-fr"},
                metrics_data=[
                    Metric("Exact Match", 1.0, True, "matched", threshold=1.0),
                    Metric("Conciseness", 0.9, True, evaluation_model="judge-model"),
                ],
            )
        ]
    )

    published = publish_deepeval_results(
        evaluation,
        experiment_name="DeepEval smoke",
        primary_metric="Exact Match",
        client=client,  # type: ignore[arg-type]
    )

    assert published.experiment_id == "deepeval-run-1"
    assert published.trial_count == 1
    assert published.score_count == 2
    assert client.trials[0]["test_case_id"] == "capital-fr"
    assert len(client.generations) == 1
    assert [(score.score_key, score.report_role) for score in client.scores] == [
        ("exact_match", "primary_verdict"),
        ("conciseness", "diagnostic"),
    ]
    assert [score.evaluator_kind for score in client.scores] == ["custom", "llm_judge"]
    assert client.finalized == [("deepeval-run-1", "completed")]


def test_publishes_stored_suite_and_pins_returned_version() -> None:
    from types import SimpleNamespace

    client = FakeClient()
    pushed = []

    class Suites:
        def push_suite(self, suite, *, publish, prune):
            assert publish is True
            assert prune is False
            pushed.append(suite)
            suite.version = "v7"
            return SimpleNamespace(suite=suite)

    evaluation = Evaluation(
        test_results=[Result("case", "question", "wrong", "right", [Metric("Exact Match", 0, False)])]
    )
    publish_deepeval_results(
        evaluation,
        experiment_name="stored",
        client=client,
        test_suites_client=Suites(),
        suite_publication_policy="subset",
    )
    assert pushed[0].tags == ["framework:deepeval"]
    assert pushed[0].cases[0].expected == {"output": "right"}
    assert client.experiments[0].suite_version == "v7"
    assert client.scores[0].passed is False


def test_requires_primary_metric_for_multiple_metrics_before_writing() -> None:
    client = FakeClient()
    evaluation = Evaluation(
        test_results=[
            Result(
                name="case",
                input="input",
                actual_output="output",
                expected_output="expected",
                metrics_data=[Metric("Quality", 0.8, True), Metric("Safety", 1.0, True)],
            )
        ]
    )

    with pytest.raises(ValueError, match="primary_metric is required"):
        publish_deepeval_results(
            evaluation,
            experiment_name="invalid",
            client=client,  # type: ignore[arg-type]
        )

    assert client.experiments == []


def test_duplicate_case_ids_increment_attempts() -> None:
    client = FakeClient()
    result = Result(
        "case",
        "input",
        "output",
        "expected",
        [Metric("Exact Match", 1.0, True)],
        metadata={"agento11y.test_case_id": "duplicate"},
    )

    publish_deepeval_results(Evaluation([result, result]), experiment_name="retry", client=client)

    assert [trial["attempt"] for trial in client.trials] == [1, 2]


@pytest.mark.parametrize(
    ("result", "message"),
    [
        (
            Result(
                "case",
                "input",
                "output",
                "expected",
                [Metric("Exact Match", 1.0, True), Metric("Exact Match", 1.0, True)],
            ),
            "duplicate metric names",
        ),
    ],
)
def test_rejects_ambiguous_results_before_writing(result: Result, message: str) -> None:
    client = FakeClient()

    with pytest.raises(ValueError, match=message):
        publish_deepeval_results(Evaluation([result]), experiment_name="invalid", client=client)

    assert client.experiments == []


def test_native_conversation_preserves_turns_and_exports_chained_generations() -> None:
    turns = [
        TurnApi(role="user", content="Hello", order=0, userId="user-1", comments="opening"),
        TurnApi(role="assistant", content="Hi!", order=1),
        TurnApi(role="user", content="Can I return this?", order=2, retrievalContext=["30-day returns"]),
        TurnApi(
            role="assistant",
            content="Yes, within 30 days.",
            order=3,
            comments="resolved",
            toolsCalled=[ToolCall(name="lookup_policy", input_parameters={"item": "opened"})],
        ),
    ]
    evaluation = NativeEvaluationResult(
        test_results=[
            NativeTestResult(
                name="refund-chat",
                success=True,
                metrics_data=[
                    MetricData(name="Completeness", score=1.0, success=True),
                    MetricData(name="Role Adherence", score=0.9, success=True),
                ],
                conversational=True,
                turns=turns,
                metadata={"agento11y.test_case_id": "refund-chat"},
            )
        ],
        confident_link=None,
        test_run_id="conversation-run",
    )
    first = FakeClient()
    second = FakeClient()

    publish_deepeval_results(
        evaluation,
        experiment_name="conversation",
        primary_metric="Completeness",
        client=first,
    )
    publish_deepeval_results(
        evaluation,
        experiment_name="conversation",
        primary_metric="Completeness",
        client=second,
    )

    snapshot = first.trials[0]["test_case"]["input"]["turns"]
    assert [(turn["role"], turn["content"], turn["order"]) for turn in snapshot] == [
        ("user", "Hello", 0),
        ("assistant", "Hi!", 1),
        ("user", "Can I return this?", 2),
        ("assistant", "Yes, within 30 days.", 3),
    ]
    assert snapshot[0]["comments"] == "opening"
    assert snapshot[2]["retrieval_context"] == ["30-day returns"]
    assert snapshot[3]["tools_called"][0]["name"] == "lookup_policy"
    assert len(first.generations) == 2
    assert [item["generation_id"] for item in first.generations] == [
        item["generation_id"] for item in second.generations
    ]
    assert len({item["conversation_id"] for item in first.generations}) == 1
    assert first.generations[0]["parent_generation_ids"] == []
    assert first.generations[1]["parent_generation_ids"] == [first.generations[0]["generation_id"]]
    assert first.generations[1]["metadata"]["deepeval_turn"]["tools_called"][0]["name"] == "lookup_policy"
    assert first.generations[1]["metadata"]["deepeval_context_turns"][2]["retrieval_context"] == ["30-day returns"]
    assert [message.role.value for message in first.generations[1]["input_messages"]] == [
        "user",
        "assistant",
        "user",
    ]
    assert [score.report_role for score in first.scores] == ["primary_verdict", "diagnostic"]
    assert all(score.generation_id == "" for score in first.scores)
    assert all(score.conversation_id == first.generations[0]["conversation_id"] for score in first.scores)


def test_conversation_case_identity_excludes_candidate_responses() -> None:
    def result(answer: str, case_id: str | None = None) -> NativeTestResult:
        return NativeTestResult(
            name="same-chat",
            success=True,
            metrics_data=[MetricData(name="Completeness", score=1.0, success=True)],
            conversational=True,
            turns=[
                TurnApi(role="user", content="Question", order=0),
                TurnApi(role="assistant", content=answer, order=1),
            ],
            metadata={"agento11y.test_case_id": case_id} if case_id else None,
        )

    derived = FakeClient()
    publish_deepeval_results(
        NativeEvaluationResult(
            test_results=[result("First answer"), result("Second answer")],
            confident_link=None,
            test_run_id="derived-identity",
        ),
        experiment_name="derived identity",
        client=derived,
    )
    explicit = FakeClient()
    publish_deepeval_results(
        NativeEvaluationResult(
            test_results=[result("First answer", "fixed"), result("Second answer", "fixed")],
            confident_link=None,
            test_run_id="explicit-identity",
        ),
        experiment_name="explicit identity",
        client=explicit,
    )

    assert len({trial["test_case_id"] for trial in derived.trials}) == 1
    assert [trial["attempt"] for trial in derived.trials] == [1, 2]
    assert [trial["attempt"] for trial in explicit.trials] == [1, 2]


def test_conversation_ending_with_user_exports_pending_response_generation() -> None:
    client = FakeClient()
    evaluation = NativeEvaluationResult(
        test_results=[
            NativeTestResult(
                name="unfinished-chat",
                success=True,
                metrics_data=[MetricData(name="Completeness", score=1.0, success=True)],
                conversational=True,
                turns=[
                    TurnApi(role="user", content="Hello", order=0),
                    TurnApi(role="assistant", content="Hi", order=1),
                    TurnApi(role="user", content="One more question", order=2),
                ],
            )
        ],
        confident_link=None,
        test_run_id="unfinished-run",
    )

    publish_deepeval_results(evaluation, experiment_name="unfinished", client=client)

    pending = client.generations[1]
    assert [message.parts[0].text for message in pending["input_messages"]] == [
        "Hello",
        "Hi",
        "One more question",
    ]
    assert "output_messages" not in pending
    assert pending["metadata"]["deepeval_pending_response"] is True
    assert pending["parent_generation_ids"] == [client.generations[0]["generation_id"]]


def test_existing_conversation_binding_does_not_export_duplicate_generations() -> None:
    evaluation = NativeEvaluationResult(
        test_results=[
            NativeTestResult(
                name="instrumented-chat",
                success=True,
                metrics_data=[MetricData(name="Completeness", score=1.0, success=True)],
                conversational=True,
                turns=[
                    TurnApi(role="user", content="Hello", order=0),
                    TurnApi(role="assistant", content="Hi", order=1),
                ],
                metadata={
                    "agento11y.conversation_id": "existing-conversation",
                    "agento11y.generation_id": "existing-final-generation",
                },
            )
        ],
        confident_link=None,
        test_run_id="instrumented-run",
    )
    client = FakeClient()

    publish_deepeval_results(evaluation, experiment_name="instrumented", client=client)

    assert client.generations == []
    assert client.scores[0].conversation_id == "existing-conversation"
    assert client.scores[0].generation_id == ""


def test_malformed_conversation_is_rejected_before_writing() -> None:
    client = FakeClient()
    evaluation = NativeEvaluationResult(
        test_results=[
            NativeTestResult(
                name="invalid-chat",
                success=False,
                metrics_data=[MetricData(name="Completeness", score=0.0, success=False)],
                conversational=True,
                turns=[TurnApi(role="system", content="unsupported", order=0)],
            )
        ],
        confident_link=None,
        test_run_id="invalid-run",
    )

    with pytest.raises(ValueError, match="unsupported role"):
        publish_deepeval_results(evaluation, experiment_name="invalid", client=client)

    assert client.experiments == []


def test_blank_conversation_content_is_rejected_before_writing() -> None:
    client = FakeClient()
    evaluation = NativeEvaluationResult(
        test_results=[
            NativeTestResult(
                name="blank-chat",
                success=False,
                metrics_data=[MetricData(name="Completeness", score=0.0, success=False)],
                conversational=True,
                turns=[TurnApi(role="user", content=" ", order=0)],
            )
        ],
        confident_link=None,
        test_run_id="blank-run",
    )

    with pytest.raises(ValueError, match="non-blank string"):
        publish_deepeval_results(evaluation, experiment_name="blank", client=client)

    assert client.experiments == []


def test_errored_user_only_conversation_does_not_block_sibling() -> None:
    client = FakeClient()
    evaluation = NativeEvaluationResult(
        test_results=[
            NativeTestResult(
                name="errored-chat",
                success=False,
                metrics_data=[MetricData(name="Completeness", error="judge failed")],
                conversational=True,
                turns=[TurnApi(role="user", content="Hello", order=0)],
            ),
            NativeTestResult(
                name="successful-chat",
                success=True,
                metrics_data=[MetricData(name="Completeness", score=1.0, success=True)],
                conversational=True,
                turns=[
                    TurnApi(role="user", content="Hello", order=0),
                    TurnApi(role="assistant", content="Hi", order=1),
                ],
            ),
        ],
        confident_link=None,
        test_run_id="partial-conversation-run",
    )

    published = publish_deepeval_results(evaluation, experiment_name="partial conversations", client=client)

    assert published.trial_count == 2
    assert published.score_count == 1
    assert len(client.trials) == 2
    assert len(client.generations) == 2
    assert client.trial_updates[0]["status"] == "failed"
    assert client.trial_updates[0]["error"] == "Completeness: judge failed"
    assert client.scores[0].passed is True


def test_live_conversation_uses_completed_turns_without_mutating_caller(monkeypatch) -> None:
    from deepeval.test_case import ConversationalTestCase, Turn

    class MetricTemplate:
        __name__ = "Completeness"
        score = 0.0
        success = False
        reason = ""
        error = None
        evaluation_model = None

        def measure(self, case) -> None:
            self.score = 1.0
            self.success = len(case.turns) == 4

        def is_successful(self) -> bool:
            return self.success

    monkeypatch.setattr("deepeval.metrics.utils.copy_metrics", lambda metrics: deepcopy(metrics))
    original = ConversationalTestCase(turns=[Turn(role="user", content="Hello")], name="live-chat")
    client = FakeClient()

    def target(case, trial):
        return [
            *case.turns,
            Turn(role="assistant", content="Hi"),
            Turn(role="user", content="Need help"),
            Turn(role="assistant", content="Of course"),
        ]

    published = run_deepeval(
        [original],
        target,
        [MetricTemplate()],
        experiment_name="live conversation",
        client=client,
        record_io=True,
    )

    assert published.score_count == 1
    assert [(turn.role, turn.content) for turn in original.turns] == [("user", "Hello")]
    assert len(client.generations) == 2
    assert client.generations[1]["parent_generation_ids"] == [client.generations[0]["generation_id"]]


def test_accepts_current_deepeval_public_result_types() -> None:
    client = FakeClient()
    evaluation = NativeEvaluationResult(
        test_results=[
            NativeTestResult(
                name="native-case",
                success=True,
                metrics_data=[MetricData(name="Exact Match", score=1.0, success=True, reason="matched")],
                conversational=False,
                input="2 + 2?",
                actual_output="4",
                expected_output="4",
                metadata={"agento11y.test_case_id": "native-case"},
            )
        ],
        confident_link=None,
        test_run_id="native-run",
    )

    published = publish_deepeval_results(
        evaluation,
        experiment_name="native",
        client=client,  # type: ignore[arg-type]
        record_io=False,
    )

    assert published.experiment_id == "native-run"
    assert client.scores[0].report_role == "primary_verdict"


def test_metric_errors_are_unrated_and_siblings_are_retained() -> None:
    client = FakeClient()
    evaluation = Evaluation(
        test_results=[
            Result("errored", "q1", "", "a1", [Metric("Exact Match", None, None, error="judge failed")]),
            Result(
                "failed",
                "q2",
                "wrong",
                "a2",
                [Metric("Exact Match", 0, False)],
                metadata={"agento11y.duration_ms": 12345},
            ),
        ]
    )

    publish_deepeval_results(evaluation, experiment_name="errors", client=client)

    assert len(client.scores) == 1
    assert client.scores[0].passed is False
    assert client.trial_updates[0]["status"] == "failed"
    assert client.trial_updates[0]["error"] == "Exact Match: judge failed"
    assert client.trial_updates[0]["duration_ms"] is None
    assert client.trial_updates[1]["duration_ms"] == 12345


def test_result_error_without_metrics_does_not_block_successful_sibling() -> None:
    client = FakeClient()
    evaluation = Evaluation(
        test_results=[
            Result("errored", "q1", "", "a1", [], error="case setup failed"),
            Result("passed", "q2", "a2", "a2", [Metric("Exact Match", 1, True)]),
        ]
    )

    published = publish_deepeval_results(evaluation, experiment_name="partial", client=client)

    assert published.trial_count == 2
    assert published.score_count == 1
    assert len(client.trials) == 2
    assert client.trial_updates[0]["status"] == "failed"
    assert client.trial_updates[0]["error"] == "case setup failed"
    assert client.scores[0].passed is True


def test_additional_metadata_fallback_preserves_identity_and_binding() -> None:
    from types import SimpleNamespace

    client = FakeClient()
    result = SimpleNamespace(
        name="legacy-native-case",
        input="question",
        actual_output="answer",
        expected_output="answer",
        metrics_data=[Metric("Exact Match", 1, True)],
        success=True,
        additional_metadata={
            "agento11y.test_case_id": "metadata-case",
            "agento11y.conversation_id": "metadata-conversation",
            "agento11y.generation_id": "metadata-generation",
        },
    )

    publish_deepeval_results(Evaluation([result]), experiment_name="metadata", client=client)

    assert client.trials[0]["test_case_id"] == "metadata-case"
    assert client.scores[0].conversation_id == "metadata-conversation"
    assert client.scores[0].generation_id == "metadata-generation"


def test_server_returned_case_is_the_private_canonical_snapshot() -> None:
    from types import SimpleNamespace

    client = FakeClient()

    class Suites:
        def push_suite(self, suite, *, publish, prune):
            assert publish is True and prune is False
            suite.version = "v8"
            suite.cases[0].input = {"input": "[REDACTED]"}
            return SimpleNamespace(suite=suite)

    evaluation = Evaluation(
        test_results=[Result("case", "person@example.com", "ok", "ok", [Metric("Exact Match", 1, True)])]
    )
    publish_deepeval_results(
        evaluation,
        experiment_name="canonical",
        client=client,
        test_suites_client=Suites(),
        suite_publication_policy="subset",
    )

    snapshot = client.trials[0]["test_case"]
    assert snapshot["input"] == {"input": "[REDACTED]"}
    assert client.trials[0]["metadata"]["agento11y_case_transformed"] is True
    assert "person@example.com" not in str(snapshot)


def test_reordering_results_preserves_derived_case_ids() -> None:
    first = Result("one", "q1", "a1", "a1", [Metric("Exact Match", 1, True)])
    second = Result("two", "q2", "a2", "a2", [Metric("Exact Match", 1, True)])

    left = FakeClient()
    right = FakeClient()
    publish_deepeval_results(Evaluation([first, second]), experiment_name="left", client=left)
    publish_deepeval_results(Evaluation([second, first]), experiment_name="right", client=right)

    assert {trial["test_case_id"] for trial in left.trials} == {trial["test_case_id"] for trial in right.trials}
