from types import SimpleNamespace
from unittest.mock import MagicMock

import pytest
from agento11y.errors import EvaluationTimeoutError
from agento11y.experiments import AgentOutput, TestCase, TestSuite, checkpoint, run_checkpoint
from test_experiments import FakeClient

BINDINGS = [
    {"binding_id": "quality", "evaluator_id": "judge", "evaluator_version": "v1", "report_role": "primary_verdict"},
    {"binding_id": "tone", "evaluator_id": "tone", "evaluator_version": "v2", "report_role": "diagnostic"},
]


class BindingClient(FakeClient):
    def __init__(self, failing: set[tuple[str, str]] = frozenset(), slow: set[tuple[str, str]] = frozenset()) -> None:
        super().__init__()
        self.failing = failing
        self.slow = slow
        self.trial_cases: dict[str, str] = {}
        self.finalize_errors: list[str] = []

    def finalize(self, experiment_id, status="succeeded", *, score_count=None, error=""):
        self.finalize_errors.append(error)
        return super().finalize(experiment_id, status, score_count=score_count, error=error)

    def upsert_trial(self, experiment_id, *, trial_id, **kwargs):
        self.trial_cases[trial_id] = kwargs["test_case"]["test_case_id"]
        return super().upsert_trial(experiment_id, trial_id=trial_id, **kwargs)

    def trigger_trial_evaluation(self, experiment_id, trial_id, evaluator_id="", evaluator_version="", **kwargs):
        binding_id = kwargs["binding_id"]
        if (self.trial_cases[trial_id], binding_id) in self.failing:
            raise RuntimeError("judge unavailable")
        if (self.trial_cases[trial_id], binding_id) in self.slow:
            raise EvaluationTimeoutError("eval-slow", "still running")
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


def fake_clock(monkeypatch, *readings: float) -> None:
    """Return each reading in turn, then keep returning the last one."""

    remaining = list(readings)

    def monotonic() -> float:
        return remaining.pop(0) if len(remaining) > 1 else remaining[0]

    monkeypatch.setattr(checkpoint, "time", SimpleNamespace(monotonic=monotonic))


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
    # Agent Observability won't complete a run with a failed binding evaluation, so it's finalized as failed.
    assert client.finalized[0][1] == "failed"
    assert client.finalize_errors == ["1 of 3 cases could not be fully graded"]
    assert run.experiment_id == client.upserts[0].experiment_id


def test_run_checkpoint_completes_when_only_the_agent_fails():
    client = BindingClient()

    def target(prompt):
        if prompt == "say b":
            raise RuntimeError("provider overloaded")
        return output("ok")

    run_checkpoint("suite", "3", target, client=client, test_suites_client=control("a", "b"))
    assert final_statuses(client) == {"a": "completed", "b": "failed"}
    assert client.finalized[0][1] == "completed"


def test_run_checkpoint_slow_judge_within_its_budget_does_not_end_the_run():
    client = BindingClient(slow={("a", "quality")})
    target = MagicMock(return_value=output("ok"))
    run_checkpoint("suite", "3", target, client=client, test_suites_client=control("a", "b", bindings=BINDINGS[:1]))
    assert target.call_count == 2
    assert final_statuses(client) == {"a": "failed", "b": "completed"}
    assert client.finalized[0][1] == "failed"


def test_run_checkpoint_timeout_while_grading_the_last_case_fails_the_run(monkeypatch):
    # start, the input check, the case start, then grading after the deadline
    fake_clock(monkeypatch, 0.0, 0.0, 0.0, 11.0)
    client = BindingClient()
    target = MagicMock(return_value=output("ok"))
    with pytest.raises(TimeoutError, match="1 of 1 cases"):
        run_checkpoint(
            "suite", "3", target, timeout=10, client=client, test_suites_client=control("a", bindings=BINDINGS[:1])
        )
    assert target.call_count == 1
    assert final_statuses(client) == {"a": "failed"}
    assert client.finalized[0][1] == "failed"


@pytest.mark.parametrize(
    ("bindings", "expected", "message"),
    [
        ([BINDINGS[1]], None, "exactly one primary verdict"),
        ([BINDINGS[0], {**BINDINGS[1], "report_role": "primary_verdict"}], None, "exactly one primary verdict"),
        (BINDINGS, BINDINGS[:1], "differ from expected_bindings"),
        (BINDINGS, [BINDINGS[0], {**BINDINGS[1], "evaluator_version": "v3"}], "differ from expected_bindings"),
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


def test_run_checkpoint_ignores_binding_fields_that_do_not_affect_grading():
    client = BindingClient()
    served = [{**binding, "input_mappings": None, "created_at": "2026-10-01T00:00:00Z"} for binding in BINDINGS]
    expected = [{**binding, "input_mappings": {}} for binding in BINDINGS]
    run_checkpoint(
        "suite",
        "3",
        MagicMock(return_value=output("ok")),
        expected_bindings=expected,
        client=client,
        test_suites_client=control("a", bindings=served),
    )
    assert client.finalized[0][1] == "completed"


def test_run_checkpoint_timeout_during_input_checks_never_calls_the_target(monkeypatch):
    # start, first case's check, second case's check (past the deadline)
    fake_clock(monkeypatch, 0.0, 0.0, 11.0)
    client = BindingClient()
    target = MagicMock(return_value=output("ok"))
    with pytest.raises(TimeoutError, match="checking grading inputs for 1 of 2 cases"):
        run_checkpoint(
            "suite", "3", target, timeout=10, client=client, test_suites_client=control("a", "b", bindings=BINDINGS[:1])
        )
    target.assert_not_called()
    assert client.upserts == []


def test_run_checkpoint_timeout_stops_starting_cases_and_fails_the_run(monkeypatch):
    # start, two input checks, first case start, its grading, second case start (past the deadline)
    fake_clock(monkeypatch, 0.0, 0.0, 0.0, 0.0, 5.0, 11.0)
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
