import pytest
from agento11y.experiments import TestCase, TestSuite, run_evals
from test_experiments import FakeClient


def test_default_evals_validate_then_publish_primary_verdicts():
    client = FakeClient()
    suite = TestSuite(
        "smoke",
        test_cases=[
            TestCase("pass", input={"prompt": "2+2"}, expected={"assistant_response": "4"}),
            TestCase("fail", input={"prompt": "3+3"}, expected={"assistant_response": "6"}),
        ],
    )
    run_evals(suite, lambda prompt: "4", client=client, record_io=True, experiment_id="explicit-run")
    assert client.upserts[0].experiment_id == "explicit-run"
    assert [score.passed for score in client.scores] == [True, False]
    assert all(score.report_role == "primary_verdict" for score in client.scores)
    assert len(client.generations) == 2
    assert client.finalized[0][1] == "completed"

    suite.cases[1].expected = {}
    invalid_client = FakeClient()
    with pytest.raises(ValueError, match="expected.assistant_response"):
        run_evals(suite, lambda prompt: "4", client=invalid_client)
    assert invalid_client.upserts == []


def test_default_evals_target_failure_finalizes_failed_run():
    client = FakeClient()
    suite = TestSuite(
        "smoke", test_cases=[TestCase("case", input={"prompt": "hi"}, expected={"assistant_response": "hello"})]
    )
    with pytest.raises(TypeError, match="target must return a string"):
        run_evals(suite, lambda prompt: None, client=client)
    assert client.finalized[0][1] == "failed"


def test_custom_evaluator_runs_after_target_in_active_trial_without_reference():
    from opentelemetry import trace

    client = FakeClient()
    suite = TestSuite("custom", test_cases=[TestCase("case", input={"prompt": "hello"})])
    order = []

    def target(prompt):
        assert client.upserts[0].planned_trial_count == 1
        order.append(("target", trace.get_current_span()))
        return "answer"

    def evaluate(trial, case, answer):
        order.append(("evaluate", trace.get_current_span()))
        assert case.test_case_id == "case" and answer == "answer"
        trial.score("custom", True, passed=True, report_role="primary_verdict")

    run_evals(suite, target, client=client, evaluate=evaluate)
    assert [name for name, _ in order] == ["target", "evaluate"]
    assert order[0][1] is order[1][1]
    assert client.scores[0].score_key == "custom"
