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


def _reference_suite() -> TestSuite:
    return TestSuite(
        "smoke",
        version="2",
        test_cases=[TestCase("case", input={"prompt": "2+2"}, expected={"assistant_response": "4"})],
    )


def test_run_evals_adds_caller_metadata_and_tags_without_overriding_provenance():
    client = FakeClient()
    ci = {"ci.provider": "github_actions", "git.commit_sha": "abc123"}

    run_evals(_reference_suite(), lambda prompt: "4", client=client, metadata=ci, tags=["ci"])

    request = client.upserts[0]
    assert request.metadata == {
        **ci,
        "framework": "grafana",
        "execution_kind": "agent_run",
        "input_selector": "input.prompt",
        "suite_id": "smoke",
        "suite_version": "2",
    }
    assert request.tags == ["ci"]
    assert ci == {"ci.provider": "github_actions", "git.commit_sha": "abc123"}


def test_run_evals_keeps_tags_passed_as_an_iterator():
    client = FakeClient()

    run_evals(_reference_suite(), lambda prompt: "4", client=client, tags=iter(["ci", "pr"]))

    assert client.upserts[0].tags == ["ci", "pr"]


def test_run_evals_refuses_tags_that_are_not_strings():
    client = FakeClient()
    with pytest.raises(TypeError, match="tags must be a sequence of strings"):
        run_evals(_reference_suite(), lambda prompt: "4", client=client, tags=iter(["ci", 1]))
    assert client.upserts == []


@pytest.mark.parametrize(
    "key", ["execution_kind", "framework", "input_selector", "evaluation_plan", "suite_id", "suite_version"]
)
def test_run_evals_refuses_metadata_that_overrides_sdk_provenance(key):
    client = FakeClient()
    with pytest.raises(ValueError, match=f"reserved for the SDK's provenance: {key}"):
        run_evals(_reference_suite(), lambda prompt: "4", client=client, metadata={key: "x"})
    assert client.upserts == []


def test_run_evals_refuses_non_mapping_metadata():
    with pytest.raises(TypeError, match="metadata must be a mapping"):
        run_evals(_reference_suite(), lambda prompt: "4", client=FakeClient(), metadata=[("git.sha", "abc")])


@pytest.mark.parametrize(
    ("env", "explicit", "expected"),
    [
        pytest.param({"AGENTO11Y_EXPERIMENT_ID": "ci-run-1"}, "", "ci-run-1", id="env allocates the run ID"),
        pytest.param({"SIGIL_EXPERIMENT_ID": "ci-run-old"}, "", "ci-run-old", id="legacy alias"),
        pytest.param({"AGENTO11Y_EXPERIMENT_ID": "ci-run-1"}, "explicit-run", "explicit-run", id="explicit beats env"),
    ],
)
def test_run_evals_defaults_the_experiment_id_from_the_environment(monkeypatch, env, explicit, expected):
    for key, value in env.items():
        monkeypatch.setenv(key, value)
    client = FakeClient()

    run = run_evals(_reference_suite(), lambda prompt: "4", client=client, experiment_id=explicit)

    assert client.upserts[0].experiment_id == expected
    assert run.experiment_id == expected


def test_run_evals_warns_once_when_the_legacy_experiment_id_supplies_the_run(monkeypatch, caplog):
    monkeypatch.setenv("SIGIL_EXPERIMENT_ID", "ci-run-old")
    with caplog.at_level("WARNING", logger="agento11y"):
        run_evals(_reference_suite(), lambda prompt: "4", client=FakeClient())
    assert any(
        "SIGIL_EXPERIMENT_ID is deprecated; rename it to AGENTO11Y_EXPERIMENT_ID" in record.getMessage()
        for record in caplog.records
    )
