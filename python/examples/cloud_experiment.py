"""Cloud experiment example: wrap an already-instrumented agent.

This is the canonical end-to-end example. It loads a suite from YAML, opens an
experiment over the one-token ingest path, and for each case opens a trial, runs
an agent, and records the verdict plus artifacts. Everything — scores, the trial
rollup, the agent's conversation, and the artifacts — shows up in Sigil.

Set SIGIL_ENDPOINT (your Grafana Cloud Sigil URL), SIGIL_AUTH_TOKEN (your
ingestion API key), and optionally SIGIL_AUTH_TENANT_ID (your stack id), then run::

    python examples/cloud_experiment.py
"""

from __future__ import annotations

import base64
import os
import tempfile
from dataclasses import dataclass, field
from typing import Any

from sigil_sdk import experiments as sigil

# A 1x1 PNG, stand-in for an "after" screenshot the agent would capture.
_PNG = base64.b64decode(
    "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="
)


@dataclass
class AgentResult:
    answer: str
    score: float
    passed: bool
    explanation: str
    conversation_id: str
    screenshot: str
    details: dict[str, Any] = field(default_factory=dict)


def run_agent(case: sigil.TestCase, experiment: sigil.Experiment) -> AgentResult:
    """An already-instrumented agent: it does the work and emits its own telemetry.

    Here we stand in for a real agent — it "solves" the case, ingests its
    transcript as a generation under a conversation id (so the trial's
    conversation is openable in Sigil), and writes a screenshot to disk.
    """

    answer = f"Completed: {case.input}"
    conversation_id = sigil.stable_id("conv", experiment.experiment_id, case.test_case_id)
    experiment.client.export_generation(
        generation_id=sigil.stable_id("gen", experiment.experiment_id, case.test_case_id),
        conversation_id=conversation_id,
        input_text=case.input,
        output_text=answer,
        model_provider="openai",
        model_name="gpt-5",
        agent_name="dashboarding-agent",
    )
    screenshot = os.path.join(tempfile.gettempdir(), f"{case.test_case_id}-after.png")
    with open(screenshot, "wb") as handle:
        handle.write(_PNG)
    return AgentResult(
        answer=answer,
        score=0.9,
        passed=True,
        explanation="Panels/annotation present and labeled correctly.",
        conversation_id=conversation_id,
        screenshot=screenshot,
        details={"checks": {"panels_present": True, "labels_ok": True}, "score": 0.9},
    )


def main() -> None:
    suite = sigil.TestSuite.from_yaml(os.path.join(os.path.dirname(__file__), "dashboard-regression.yaml"))

    with sigil.experiment(
        name="PR 412 dashboard regression",
        suite=suite,
        candidate={"git_sha": "abc123", "model_name": "gpt-5", "agent_name": "dashboarding-agent"},
    ) as experiment:
        for case in suite.cases:
            with experiment.trial(case) as trial:
                result = run_agent(case, experiment)
                trial.bind_conversation(result.conversation_id)
                trial.score(
                    "final",
                    value=result.score,
                    passed=result.passed,
                    explanation=result.explanation,
                )
                trial.artifact("after", path=result.screenshot, kind="image")
                trial.artifact("grading-details", data=result.details, kind="json")

    report = experiment.report()
    print(f"experiment: {experiment.experiment_id}")
    print(f"open:       {experiment.url}")
    print(f"report:     {report.summary if hasattr(report, 'summary') else report}")


if __name__ == "__main__":
    main()
