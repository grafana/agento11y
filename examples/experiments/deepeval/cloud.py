"""DeepEval diagnostic + cloud ground-truth verdict on a published test suite.

Requires inherited SDK ingest/control credentials and the evaluator defined in
cloud-evaluator.json.
The deterministic test agent intentionally gets the second answer wrong.
"""

import json
from pathlib import Path
from uuid import uuid4

from agento11y.experiments import Evaluator, ReportRole, TestCase, TestSuite, TestSuitesClient, experiment
from deepeval.metrics import ExactMatchMetric
from deepeval.test_case import LLMTestCase


def answer(question: str) -> str:
    return {"What is the capital of France?": "Paris", "What is 2 + 2?": "5"}[question]


def main() -> None:
    cases = [TestCase(**case) for case in json.loads(Path(__file__).with_name("cases.json").read_text())]
    control = TestSuitesClient()
    pushed = control.push_suite(
        TestSuite(suite_id="deepeval-ground-truth", name="DeepEval ground truth", test_cases=cases),
        publish=True,
    )
    suite = control.pull_suite(pushed.suite_id, pushed.suite_version)
    assert len(suite.cases) == 2
    assert {case.test_case_id: case.expected for case in suite.cases} == {
        case.test_case_id: case.expected for case in cases
    }
    run_id = "deepeval-cloud-" + uuid4().hex[:12]
    print(f"Suite: {suite.suite_id}@{suite.version}; experiment: {run_id}", flush=True)
    with experiment(
        "DeepEval + cloud nested ground truth",
        experiment_id=run_id,
        suite=suite,
        planned_trial_count=len(suite.cases),
        candidate={"agent_name": "deepeval-example-agent", "agent_version": "1"},
        tags=["deepeval", "ground-truth"],
    ) as exp:
        for case in suite.cases:
            with exp.trial(case) as trial:
                question = case.input["question"]
                actual = answer(question)
                trial.record_io(input=question, output=actual, agent_name="deepeval-example-agent")
                metric = ExactMatchMetric()
                metric.measure(
                    LLMTestCase(input=question, actual_output=actual, expected_output=case.expected["answer"]["text"])
                )
                trial.score(
                    "exact_match",
                    metric.score,
                    evaluator=Evaluator(evaluator_id="deepeval.exact_match", version="1", kind="custom"),
                    passed=metric.is_successful(),
                    report_role=ReportRole.DIAGNOSTIC,
                )
                artifact = trial.artifact(
                    "test-result.json",
                    data={
                        "input": case.input,
                        "expected": case.expected,
                        "actual_output": actual,
                        "deepeval_score": metric.score,
                    },
                )
                assert artifact.get("artifact_id")
                evaluation = trial.evaluate(
                    "deepeval.ground_truth",
                    "1",
                    timeout=120,
                    report_role=ReportRole.PRIMARY_VERDICT,
                )
                assert evaluation.report_role == ReportRole.PRIMARY_VERDICT
                print(
                    f"{case.test_case_id}: cloud={evaluation.status.value}, DeepEval={metric.score}, artifact uploaded",
                    flush=True,
                )
    print(f"Completed: {run_id}", flush=True)


if __name__ == "__main__":
    main()
