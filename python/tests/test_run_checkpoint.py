from types import SimpleNamespace
from unittest.mock import MagicMock

import pytest
from agento11y.experiments import AgentOutput, TestCase, TestSuite, checkpoint, run_checkpoint
from test_experiments import FakeClient

BINDINGS = [
    {"binding_id": "quality", "evaluator_id": "judge", "evaluator_version": "v1", "report_role": "primary_verdict"},
    {"binding_id": "tone", "evaluator_id": "tone", "evaluator_version": "v2", "report_role": "diagnostic"},
]


class BindingClient(FakeClient):
    def __init__(self, failing: set[tuple[str, str]] = frozenset()) -> None:
        super().__init__()
        self.failing = failing
        self.trial_cases: dict[str, str] = {}

    def upsert_trial(self, experiment_id, *, trial_id, **kwargs):
        self.trial_cases[trial_id] = kwargs["test_case"]["test_case_id"]
        return super().upsert_trial(experiment_id, trial_id=trial_id, **kwargs)

    def trigger_trial_evaluation(self, experiment_id, trial_id, evaluator_id="", evaluator_version="", **kwargs):
        binding_id = kwargs["binding_id"]
        if (self.trial_cases[trial_id], binding_id) in self.failing:
            raise RuntimeError("judge unavailable")
        self.triggered_evaluations.append((experiment_id, trial_id, binding_id, evaluator_version))
        return self.evaluation_result


def control(*case_ids: str, bindings=BINDINGS) -> MagicMock:
    suites = MagicMock()
    suites.pull_suite.return_value = TestSuite(
        "suite",
        version="3",
        test_cases=[TestCase(case_id, input={"prompt": f"say {case_id}"}) for case_id in case_ids],
    )
    suites.get_bindings.return_value = bindings
    return suites


def final_statuses(client: FakeClient) -> dict[str, str]:
    statuses = {}
    for _, trial_id, status, _ in client.trial_updates:
        statuses[client.trial_cases[trial_id]] = status
    return statuses


def output(text: str) -> AgentOutput:
    return AgentOutput(text=text, flush=lambda: None)


def test_run_checkpoint_grades_every_case_and_keeps_going_past_case_errors():
    client = BindingClient(failing={("c", "tone")})
    suites = control("a", "b", "c")
    calls = []

    def target(prompt):
        calls.append(prompt)
        assert suites.preview_binding.call_count == 6
        if prompt == "say b":
            raise RuntimeError("provider overloaded")
        return output("ok")

    run = run_checkpoint("suite", "3", target, client=client, test_suites_client=suites)

    suites.pull_suite.assert_called_once_with("suite", "3", require_published=True)
    assert calls == ["say a", "say b", "say c"]
    assert final_statuses(client) == {"a": "completed", "b": "failed", "c": "failed"}
    assert [binding for _, _, binding, _ in client.triggered_evaluations] == ["quality", "tone", "quality"]
    assert client.upserts[0].metadata["execution_kind"] == "agent_run"
    assert client.upserts[0].planned_trial_count == 3
    assert client.finalized[0][1] == "completed"
    assert run.experiment_id == client.upserts[0].experiment_id


@pytest.mark.parametrize(
    ("bindings", "expected", "message"),
    [
        ([BINDINGS[1]], None, "primary verdict"),
        (BINDINGS, BINDINGS[:1], "differ from expected_bindings"),
    ],
)
def test_run_checkpoint_refuses_unusable_bindings_before_running(bindings, expected, message):
    client = BindingClient()
    target = MagicMock()
    with pytest.raises(ValueError, match=message):
        run_checkpoint(
            "suite",
            "3",
            target,
            expected_bindings=expected,
            client=client,
            test_suites_client=control("a", bindings=bindings),
        )
    target.assert_not_called()
    assert client.upserts == []


def test_run_checkpoint_stops_when_the_target_cannot_flush_its_telemetry():
    client = BindingClient()
    target = MagicMock(return_value="plain text")
    with pytest.raises(TypeError, match="AgentOutput with flush"):
        run_checkpoint("suite", "3", target, client=client, test_suites_client=control("a", "b"))
    assert target.call_count == 1
    assert client.finalized[0][1] == "failed"


def test_run_checkpoint_timeout_stops_starting_cases_and_fails_the_run(monkeypatch):
    clock = iter([0.0, 0.0, 5.0, 11.0])
    monkeypatch.setattr(checkpoint, "time", SimpleNamespace(monotonic=lambda: next(clock)))
    client = BindingClient()
    target = MagicMock(return_value=output("ok"))
    with pytest.raises(TimeoutError, match="1 of 2 cases"):
        run_checkpoint(
            "suite",
            "3",
            target,
            timeout=10,
            client=client,
            test_suites_client=control("a", "b", bindings=BINDINGS[:1]),
        )
    assert target.call_count == 1
    assert client.finalized[0][1] == "failed"
