"""Pull a stored suite, run it, and publish experiment scores."""

from __future__ import annotations

import os
from typing import Any

from agento11y import experiments
from dotenv import load_dotenv


def run_agent(case_input: Any) -> str:
    """Tiny deterministic placeholder; replace with your real agent."""

    if isinstance(case_input, dict):
        return " ".join(str(value) for value in case_input.values())
    return str(case_input)


def expected_terms(expected: Any) -> list[str]:
    if isinstance(expected, dict):
        raw = expected.get("must_include")
        if isinstance(raw, list):
            return [str(item).lower() for item in raw]
        if "answer" in expected:
            return [str(expected["answer"]).lower()]
    if expected is None:
        return []
    return [str(expected).lower()]


def main() -> None:
    load_dotenv()
    suite_id = os.environ.get("AGENTO11Y_SUITE_ID", "dashboard-regression")
    version = os.environ.get("AGENTO11Y_SUITE_VERSION", "latest_published")
    experiment_id = os.environ.get("AGENTO11Y_EXPERIMENT_ID", f"pulled-suite-{suite_id}-{version}")

    verifier = experiments.Evaluator(evaluator_id="example.expected_terms", version="1", kind="deterministic")

    with experiments.experiment_from_suite(
        suite_id,
        version=version,
        experiment_id=experiment_id,
        candidate={"agent_name": "example-agent", "git_sha": os.environ.get("GIT_SHA", "manual")},
        tags=["example", "pulled-suite"],
    ) as exp:
        assert exp.suite is not None
        for case in exp.suite.cases:
            with exp.trial(case) as trial:
                output = run_agent(case.input)
                terms = expected_terms(case.expected)
                passed = all(term in output.lower() for term in terms)
                score = 1.0 if passed else 0.0
                trial.record_io(input=case.input, output=output, agent_name="example-agent")
                trial.score(
                    "final",
                    score,
                    passed=passed,
                    explanation="checked expected terms",
                    evaluator=verifier,
                    metadata={"expected": case.expected},
                )

    report = exp.report()
    row_ids = [row.get("test_case_id", "") for row in report.rows]
    print(f"Experiment '{exp.experiment_id}' finished over {exp.suite.suite_id}@{exp.suite.version}")
    print(f"report rows: {', '.join(row_ids)}")
    print(f"View in Agent Observability: {exp.url}")


if __name__ == "__main__":
    main()
